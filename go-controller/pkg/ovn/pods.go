package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
			logrus.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		logicalPort := podLogicalPortName(pod)
		expectedLogicalPorts[logicalPort] = true
	}

	// get the list of logical ports from OVN
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "logical_switch_port", "external_ids:pod=true")
	if err != nil {
		logrus.Errorf("Error in obtaining list of logical ports, "+
			"stderr: %q, err: %v",
			stderr, err)
		return
	}
	existingLogicalPorts := strings.Fields(output)
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			logrus.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
				existingPort)
			if err != nil {
				logrus.Errorf("Error in deleting pod's logical port "+
					"stdout: %q, stderr: %q err: %v",
					out, stderr, err)
			}
			if !oc.portGroupSupport {
				oc.deletePodAcls(existingPort)
			}
		}
	}
}

func (oc *Controller) deletePodAcls(logicalPort string) {
	// delete the ACL rules on OVN that corresponding pod has been deleted
	uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external_ids:logical_port=%s", logicalPort))
	if err != nil {
		logrus.Errorf("Error in getting list of acls "+
			"stdout: %q, stderr: %q, error: %v", uuids, stderr, err)
		return
	}

	if uuids == "" {
		logrus.Debugf("deletePodAcls: returning because find " +
			"returned no ACLs")
		return
	}

	uuidSlice := strings.Fields(uuids)
	for _, uuid := range uuidSlice {
		// Get logical switch
		out, stderr, err := util.RunOVNNbctl("--data=bare",
			"--no-heading", "--columns=_uuid", "find", "logical_switch",
			fmt.Sprintf("acls{>=}%s", uuid))
		if err != nil {
			logrus.Errorf("find failed to get the logical_switch of acl "+
				"uuid=%s, stderr: %q, (%v)", uuid, stderr, err)
			continue
		}

		if out == "" {
			continue
		}
		logicalSwitch := out

		_, stderr, err = util.RunOVNNbctl("--if-exists", "remove",
			"logical_switch", logicalSwitch, "acls", uuid)
		if err != nil {
			logrus.Errorf("failed to delete the allow-from rule %s for"+
				" logical_switch=%s, logical_port=%s, stderr: %q, (%v)",
				uuid, logicalSwitch, logicalPort, stderr, err)
			continue
		}
	}
}

func (oc *Controller) getLogicalPortUUID(logicalPort string) string {
	if oc.logicalPortUUIDCache[logicalPort] != "" {
		return oc.logicalPortUUIDCache[logicalPort]
	}

	out, stderr, err := util.RunOVNNbctl("--if-exists", "get",
		"logical_switch_port", logicalPort, "_uuid")
	if err != nil {
		logrus.Errorf("Error while getting uuid for logical_switch_port "+
			"%s, stderr: %q, err: %v", logicalPort, stderr, err)
		return ""
	}

	if out == "" {
		return out
	}

	oc.logicalPortUUIDCache[logicalPort] = out
	return oc.logicalPortUUIDCache[logicalPort]
}

func (oc *Controller) getGatewayFromSwitch(logicalSwitch string) (*net.IPNet, error) {
	var gatewayIPMaskStr, stderr string
	var ok bool
	var err error

	oc.lsMutex.Lock()
	defer oc.lsMutex.Unlock()
	if gatewayIPMaskStr, ok = oc.gatewayCache[logicalSwitch]; !ok {
		gatewayIPMaskStr, stderr, err = util.RunOVNNbctl("--if-exists",
			"get", "logical_switch", logicalSwitch,
			"external_ids:gateway_ip")
		if err != nil {
			logrus.Errorf("Failed to get gateway IP:  %s, stderr: %q, %v",
				gatewayIPMaskStr, stderr, err)
			return nil, err
		}
		if gatewayIPMaskStr == "" {
			return nil, fmt.Errorf("Empty gateway IP in logical switch %s",
				logicalSwitch)
		}
		oc.gatewayCache[logicalSwitch] = gatewayIPMaskStr
	}
	ip, ipnet, err := net.ParseCIDR(gatewayIPMaskStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse gateway IP %q: %v", gatewayIPMaskStr, err)
	}
	ipnet.IP = ip
	return ipnet, nil
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	logrus.Infof("Deleting pod: %s", pod.Name)
	logicalPort := podLogicalPortName(pod)
	out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
		logicalPort)
	if err != nil {
		logrus.Errorf("Error in deleting pod logical port "+
			"stdout: %q, stderr: %q, (%v)",
			out, stderr, err)
	}

	var podIP net.IP
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations["ovn"])
	if err != nil {
		logrus.Errorf("Error in deleting pod logical port; failed "+
			"to read pod annotation: %v", err)
	} else {
		podIP = podAnnotation.IP.IP
	}

	delete(oc.logicalPortCache, logicalPort)

	oc.lspMutex.Lock()
	delete(oc.lspIngressDenyCache, logicalPort)
	delete(oc.lspEgressDenyCache, logicalPort)
	delete(oc.logicalPortUUIDCache, logicalPort)
	oc.lspMutex.Unlock()

	if !oc.portGroupSupport {
		oc.deleteACLDenyOld(pod.Namespace, pod.Spec.NodeName, logicalPort,
			"Ingress")
		oc.deleteACLDenyOld(pod.Namespace, pod.Spec.NodeName, logicalPort,
			"Egress")
	}
	oc.deletePodFromNamespace(pod.Namespace, podIP, logicalPort)
	return
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) error {
	oc.lsMutex.Lock()
	ok := oc.logicalSwitchCache[nodeName]
	oc.lsMutex.Unlock()
	// Fast return if we already have the node switch in our cache
	if ok {
		return nil
	}

	// Otherwise wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created very soon after startup so we should
	// only be waiting here once per node at most.
	if err := wait.PollImmediate(500*time.Millisecond, 30*time.Second, func() (bool, error) {
		if _, _, err := util.RunOVNNbctl("get", "logical_switch", nodeName, "other-config"); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		logrus.Errorf("timed out waiting for node %q logical switch: %v", nodeName, err)
		return err
	}

	oc.lsMutex.Lock()
	defer oc.lsMutex.Unlock()
	if !oc.logicalSwitchCache[nodeName] {
		if err := oc.addAllowACLFromNode(nodeName); err != nil {
			return err
		}
		oc.logicalSwitchCache[nodeName] = true
	}
	return nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) {
	var out, stderr string
	var err error
	if pod.Spec.HostNetwork {
		return
	}

	logicalSwitch := pod.Spec.NodeName
	if logicalSwitch == "" {
		logrus.Errorf("Failed to find the logical switch for pod %s/%s",
			pod.Namespace, pod.Name)
		return
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		logrus.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	if err = oc.waitForNodeLogicalSwitch(pod.Spec.NodeName); err != nil {
		return
	}

	portName := podLogicalPortName(pod)
	logrus.Debugf("Creating logical port for %s on switch %s", portName, logicalSwitch)

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations["ovn"])

	// If pod already has annotations, just add the lsp with static ip/mac.
	// Else, create the lsp with dynamic addresses.
	if err == nil {
		out, stderr, err = util.RunOVNNbctl("--may-exist", "lsp-add",
			logicalSwitch, portName, "--", "lsp-set-addresses", portName,
			fmt.Sprintf("%s %s", annotation.MAC, annotation.IP.IP), "--", "set",
			"logical_switch_port", portName,
			"external-ids:namespace="+pod.Namespace,
			"external-ids:logical_switch="+logicalSwitch,
			"external-ids:pod=true", "--", "--if-exists",
			"clear", "logical_switch_port", portName, "dynamic_addresses")
		if err != nil {
			logrus.Errorf("Failed to add logical port to switch "+
				"stdout: %q, stderr: %q (%v)",
				out, stderr, err)
			return
		}
	} else {
		out, stderr, err = util.RunOVNNbctl("--wait=sb", "--",
			"--may-exist", "lsp-add", logicalSwitch, portName,
			"--", "lsp-set-addresses",
			portName, "dynamic", "--", "set",
			"logical_switch_port", portName,
			"external-ids:namespace="+pod.Namespace,
			"external-ids:logical_switch="+logicalSwitch,
			"external-ids:pod=true")
		if err != nil {
			logrus.Errorf("Error while creating logical port %s "+
				"stdout: %q, stderr: %q (%v)",
				portName, out, stderr, err)
			return
		}

	}

	oc.logicalPortCache[portName] = logicalSwitch

	gatewayIP, err := oc.getGatewayFromSwitch(logicalSwitch)
	if err != nil {
		logrus.Errorf("Error obtaining gateway address for switch %s", logicalSwitch)
		return
	}

	var podMac net.HardwareAddr
	var podIP net.IP
	count := 30
	for count > 0 {
		podMac, podIP, err = util.GetPortAddresses(portName)
		if err == nil && podMac != nil && podIP != nil {
			break
		}
		if err != nil {
			logrus.Errorf("Error while obtaining addresses for %s - %v", portName,
				err)
			return
		}
		time.Sleep(time.Second)
		count--
	}
	if count == 0 {
		logrus.Errorf("Error while obtaining addresses for %s "+
			"stdout: %q, stderr: %q, (%v)", portName, out, stderr, err)
		return
	}

	podCIDR := &net.IPNet{IP: podIP, Mask: gatewayIP.Mask}

	// now set the port security for the logical switch port
	out, stderr, err = util.RunOVNNbctl("lsp-set-port-security", portName,
		fmt.Sprintf("%s %s", podMac, podCIDR))
	if err != nil {
		logrus.Errorf("error while setting port security for logical port %s "+
			"stdout: %q, stderr: %q (%v)", portName, out, stderr, err)
		return
	}

	routes := []util.PodRoute{}
	if gatewayIP != nil && len(oc.hybridOverlayClusterSubnets) > 0 {
		// Get the 3rd address in the node's subnet; the first is taken
		// by the k8s-cluster-router port, the second by the management port
		second := util.NextIP(gatewayIP.IP)
		thirdIP := util.NextIP(second)
		for _, subnet := range oc.hybridOverlayClusterSubnets {
			routes = append(routes, util.PodRoute{
				Dest:    subnet.CIDR,
				NextHop: thirdIP,
			})
		}
	}

	marshalledAnnotation, err := util.MarshalPodAnnotation(&util.PodAnnotation{
		IP:     podCIDR,
		MAC:    podMac,
		GW:     gatewayIP.IP,
		Routes: routes,
	})
	if err != nil {
		logrus.Errorf("error creating pod network annotation: %v", err)
		return
	}

	logrus.Debugf("Annotation values: ip=%s ; mac=%s ; gw=%s\nAnnotation=%s",
		podCIDR, podMac, gatewayIP, annotation)
	err = oc.kube.SetAnnotationOnPod(pod, "ovn", marshalledAnnotation)
	if err != nil {
		logrus.Errorf("Failed to set annotation on pod %s - %v", pod.Name, err)
	}
	oc.addPodToNamespace(pod.Namespace, podIP, portName)

	return
}
