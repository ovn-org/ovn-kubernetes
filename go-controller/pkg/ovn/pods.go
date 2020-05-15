package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

// Builds the logical switch port name for a given pod.
func podLogicalPortName(pod *kapi.Pod) string {
	return pod.Namespace + "_" + pod.Name
}

func (oc *Controller) syncPods(pods []interface{}) {
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		_, err := util.UnmarshalPodAnnotation(pod.Annotations)
		if podScheduled(pod) && podWantsNetwork(pod) && err == nil {
			logicalPort := podLogicalPortName(pod)
			expectedLogicalPorts[logicalPort] = true
		}
	}

	// get the list of logical ports from OVN
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "logical_switch_port", "external_ids:pod=true")
	if err != nil {
		klog.Errorf("Error in obtaining list of logical ports, "+
			"stderr: %q, err: %v",
			stderr, err)
		return
	}
	existingLogicalPorts := strings.Fields(output)
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			klog.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
				existingPort)
			if err != nil {
				klog.Errorf("Error in deleting pod's logical port "+
					"stdout: %q, stderr: %q err: %v",
					out, stderr, err)
			}
		}
	}
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)

	logicalPort := podLogicalPortName(pod)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// Remove the port from the default deny multicast policy
	if oc.multicastSupport {
		if err := podDeleteDefaultDenyMulticastPolicy(portInfo); err != nil {
			klog.Errorf(err.Error())
		}
	}

	if err := oc.deletePodFromNamespace(pod.Namespace, portInfo); err != nil {
		klog.Errorf(err.Error())
	}

	out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del", logicalPort)
	if err != nil {
		klog.Errorf("Error in deleting pod %s logical port "+
			"stdout: %q, stderr: %q, (%v)",
			podDesc, out, stderr, err)
	}

	oc.logicalPortCache.remove(logicalPort)
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) (*net.IPNet, error) {
	// Wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	var subnets []*net.IPNet
	if err := wait.PollImmediate(10*time.Millisecond, 30*time.Second, func() (bool, error) {
		oc.lsMutex.Lock()
		defer oc.lsMutex.Unlock()
		var ok bool
		subnets, ok = oc.logicalSwitchCache[nodeName]
		return ok, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for logical switch %q subnet: %v", nodeName, err)
	}
	// FIXME DUAL-STACK
	return subnets[0], nil
}

func getPodAddresses(portName string) (net.HardwareAddr, net.IP, bool, error) {
	podMac, podIP, err := util.GetPortAddresses(portName)
	if err != nil {
		return nil, nil, false, err
	}
	if podMac == nil || podIP == nil {
		// wait longer
		return nil, nil, false, nil
	}
	return podMac, podIP, true, nil
}

func waitForPodAddresses(portName string) (net.HardwareAddr, net.IP, error) {
	var (
		podMac net.HardwareAddr
		podIP  net.IP
		done   bool
		err    error
	)

	// First try to get the pod addresses quickly then fall back to polling every second.
	err = wait.PollImmediate(50*time.Millisecond, 300*time.Millisecond, func() (bool, error) {
		podMac, podIP, done, err = getPodAddresses(portName)
		return done, err
	})
	if err == wait.ErrWaitTimeout {
		err = wait.PollImmediate(time.Second, 30*time.Second, func() (bool, error) {
			podMac, podIP, done, err = getPodAddresses(portName)
			return done, err
		})
	}

	if err != nil || podMac == nil || podIP == nil {
		return nil, nil, fmt.Errorf("Error while obtaining addresses for %s: %v", portName, err)
	}

	return podMac, podIP, nil
}

func getRoutesGatewayIP(pod *kapi.Pod, subnet *net.IPNet, hybridOverlayExternalGW net.IP) ([]util.PodRoute, net.IP, error) {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetPodNetSelAnnotation(pod, util.NetworkAttachmentAnnotation)
	if err != nil {
		return nil, nil, fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRoute := false
	for _, network := range networks {
		if len(network.GatewayRequest) != 0 && network.GatewayRequest[0] != nil {
			otherDefaultRoute = true
			break
		}
	}
	gatewayIPnet := util.GetNodeGatewayIfAddr(subnet)
	var gatewayIP net.IP
	routes := make([]util.PodRoute, 0)
	if otherDefaultRoute || len(hybridOverlayExternalGW) > 0 {
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			var route util.PodRoute
			route.Dest = clusterSubnet.CIDR
			route.NextHop = gatewayIPnet.IP
			routes = append(routes, route)
		}
		for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
			var route util.PodRoute
			route.Dest = serviceSubnet
			route.NextHop = gatewayIPnet.IP
			routes = append(routes, route)
		}
		if len(hybridOverlayExternalGW) > 0 {
			gatewayIP = util.GetNodeHybridOverlayIfAddr(subnet).IP
		}
	} else {
		gatewayIP = gatewayIPnet.IP
	}

	if gatewayIP != nil && len(config.HybridOverlay.ClusterSubnets) > 0 {
		// Add a route for each hybrid overlay subnet via the hybrid
		// overlay port on the pod's logical switch.
		second := util.NextIP(gatewayIP)
		thirdIP := util.NextIP(second)
		for _, subnet := range config.HybridOverlay.ClusterSubnets {
			routes = append(routes, util.PodRoute{
				Dest:    subnet.CIDR,
				NextHop: thirdIP,
			})
		}
	}
	return routes, gatewayIP, nil
}

func (oc *Controller) getHybridOverlayExternalGwAnnotation(ns string) (net.IP, error) {
	nsInfo, err := oc.waitForNamespaceLocked(ns)
	if err != nil {
		return nil, err
	}
	defer nsInfo.Unlock()
	return nsInfo.hybridOverlayExternalGW, nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) error {
	var out, stderr string
	var err error

	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	if val, ok := oc.logicalSwitchCache[pod.Spec.NodeName]; ok && val == nil {
		return nil
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	logicalSwitch := pod.Spec.NodeName
	nodeSubnet, err := oc.waitForNodeLogicalSwitch(pod.Spec.NodeName)
	if err != nil {
		return err
	}

	portName := podLogicalPortName(pod)
	klog.V(5).Infof("Creating logical port for %s on switch %s", portName, logicalSwitch)

	var podMac net.HardwareAddr
	var podCIDR *net.IPNet
	var args []string

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err == nil {
		podMac = annotation.MAC
		// DUAL-STACK FIXME: handle multiple IPs
		podCIDR = annotation.IPs[0]

		// Check if the pod's logical switch port already exists. If it
		// does don't re-add the port to OVN as this will change its
		// UUID and and the port cache, address sets, and port groups
		// will still have the old UUID.
		out, _, err = util.RunOVNNbctl("--if-exists", "get", "logical_switch_port", portName, "_uuid")
		if err != nil || !strings.Contains(out, "-") {
			// Pod's logical switch port does not yet exist
			args = []string{"lsp-add", logicalSwitch, portName}
		}

		// If the pod already has annotations use the existing static
		// IP/MAC from the annotation.
		args = append(args,
			"--", "lsp-set-addresses", portName, fmt.Sprintf("%s %s", podMac, podCIDR.IP),
			"--", "--if-exists", "clear", "logical_switch_port", portName, "dynamic_addresses",
		)
	} else {
		addresses := "dynamic"
		networks, err := util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
		if err != nil || (networks != nil && len(networks) != 1) {
			return fmt.Errorf("error while getting custom MAC config for port %q from "+
				"default-network's network-attachment: %v", portName, err)
		} else if networks != nil && networks[0].MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
			addresses = networks[0].MacRequest + " dynamic"
		}

		// If it has no annotations, let OVN assign it IP and MAC addresses
		args = []string{
			"--may-exist", "lsp-add", logicalSwitch, portName,
			"--", "lsp-set-addresses", portName, addresses,
		}
	}
	args = append(args, "--", "set", "logical_switch_port", portName, "external-ids:namespace="+pod.Namespace, "external-ids:pod=true")

	out, stderr, err = util.RunOVNNbctl(args...)
	if err != nil {
		return fmt.Errorf("Error while creating logical port %s stdout: %q, stderr: %q (%v)",
			portName, out, stderr, err)
	}

	// If the pod has not already been assigned addresses, read them now
	if podMac == nil || podCIDR == nil {
		var podIP net.IP
		podMac, podIP, err = waitForPodAddresses(portName)
		if err != nil {
			return err
		}
		podCIDR = &net.IPNet{IP: podIP, Mask: nodeSubnet.Mask}
	}

	// UUID must be retrieved separately from the lsp-add transaction since
	// (as of OVN 2.12) a bogus UUID is returned if they are part of the same
	// transaction.
	// FIXME: move to the lsp-add transaction once https://bugzilla.redhat.com/show_bug.cgi?id=1806788
	// is resolved.
	var uuid string
	uuid, _, err = util.RunOVNNbctl("get", "logical_switch_port", portName, "_uuid")
	if err != nil {
		return fmt.Errorf("error while getting UUID for logical port %s "+
			"stdout: %q, stderr: %q (%v)", portName, uuid, stderr, err)
	}
	if !strings.Contains(uuid, "-") {
		return fmt.Errorf("invalid logical port %s uuid %q", portName, uuid)
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, uuid, podMac, podCIDR.IP)

	// Set the port security for the logical switch port
	addresses := fmt.Sprintf("%s %s", podMac, podCIDR.IP)
	out, stderr, err = util.RunOVNNbctl("lsp-set-port-security", portName, addresses)
	if err != nil {
		return fmt.Errorf("error while setting port security for logical port %s "+
			"stdout: %q, stderr: %q (%v)", portName, out, stderr, err)
	}

	// Enforce the default deny multicast policy
	if oc.multicastSupport {
		if err := podAddDefaultDenyMulticastPolicy(portInfo); err != nil {
			return err
		}
	}

	if err := oc.addPodToNamespace(pod.Namespace, portInfo); err != nil {
		return err
	}

	if annotation == nil {
		hybridOverlayExternalGW := net.IP{}
		if config.HybridOverlay.Enabled {
			hybridOverlayExternalGW, err = oc.getHybridOverlayExternalGwAnnotation(pod.Namespace)
			if err != nil {
				return err
			}
		}
		routes, gwIP, err := getRoutesGatewayIP(pod, nodeSubnet, hybridOverlayExternalGW)
		if err != nil {
			return err
		}

		var gwIPs []net.IP
		if gwIP != nil {
			gwIPs = []net.IP{gwIP}
		}

		marshalledAnnotation, err := util.MarshalPodAnnotation(&util.PodAnnotation{
			IPs:      []*net.IPNet{podCIDR},
			MAC:      podMac,
			Gateways: gwIPs,
			Routes:   routes,
		})
		if err != nil {
			return fmt.Errorf("error creating pod network annotation: %v", err)
		}

		klog.V(5).Infof("Annotation values: ip=%s ; mac=%s ; gw=%s\nAnnotation=%s",
			podCIDR, podMac, gwIPs, marshalledAnnotation)
		if err = oc.kube.SetAnnotationsOnPod(pod, marshalledAnnotation); err != nil {
			return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
		}

		// observe the pod creation latency metric.
		metrics.RecordPodCreated(pod)
	}

	return nil
}
