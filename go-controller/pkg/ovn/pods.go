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

	podDesc := pod.Namespace + "/" + pod.Name
	logrus.Infof("Deleting pod: %s", podDesc)
	logicalPort := podLogicalPortName(pod)
	out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
		logicalPort)
	if err != nil {
		logrus.Errorf("Error in deleting pod %s logical port "+
			"stdout: %q, stderr: %q, (%v)",
			podDesc, out, stderr, err)
	}

	var podIP net.IP
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err != nil {
		logrus.Debugf("failed to read pod %s annotation when deleting "+
			"logical port; falling back to PodIP %s: %v",
			podDesc, pod.Status.PodIP, err)
		podIP = net.ParseIP(pod.Status.PodIP)
	} else {
		podIP = podAnnotation.IP.IP
	}

	delete(oc.logicalPortCache, logicalPort)

	oc.lspMutex.Lock()
	delete(oc.logicalPortUUIDCache, logicalPort)
	oc.lspMutex.Unlock()

	oc.deletePodFromNamespace(pod.Namespace, podIP, logicalPort)
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
	var subnet string
	if err := wait.PollImmediate(500*time.Millisecond, 30*time.Second, func() (bool, error) {
		var err error
		if subnet, _, err = util.RunOVNNbctl("get", "logical_switch", nodeName, "other-config:subnet"); err != nil {
			return false, nil
		}
		if subnet == "" {
			return false, nil
		}
		return true, nil
	}); err != nil {
		logrus.Errorf("timed out waiting for logical switch %q other-config:subnet: %v", nodeName, err)
		return err
	}

	oc.lsMutex.Lock()
	defer oc.lsMutex.Unlock()
	if !oc.logicalSwitchCache[nodeName] {
		oc.logicalSwitchCache[nodeName] = true
	}
	return nil
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

func (oc *Controller) addLogicalPort(pod *kapi.Pod) error {
	var out, stderr string
	var err error

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		logrus.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	logicalSwitch := pod.Spec.NodeName
	if err = oc.waitForNodeLogicalSwitch(pod.Spec.NodeName); err != nil {
		return err
	}

	portName := podLogicalPortName(pod)
	logrus.Debugf("Creating logical port for %s on switch %s", portName, logicalSwitch)

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
	// If pod already has annotations, just add the lsp with static ip/mac.
	// Else, create the lsp with dynamic addresses.
	if err == nil {
		out, stderr, err = util.RunOVNNbctl(
			"--may-exist", "lsp-add", logicalSwitch, portName,
			"--", "lsp-set-addresses", portName, fmt.Sprintf("%s %s", annotation.MAC, annotation.IP.IP),
			"--", "set", "logical_switch_port", portName, "external-ids:namespace="+pod.Namespace,
			"external-ids:logical_switch="+logicalSwitch, "external-ids:pod=true",
			"--", "--if-exists", "clear", "logical_switch_port", portName, "dynamic_addresses")
		if err != nil {
			return fmt.Errorf("Failed to add logical port to switch "+
				"stdout: %q, stderr: %q (%v)", out, stderr, err)
		}
		// now set the port security for the logical switch port
		out, stderr, err = util.RunOVNNbctl("lsp-set-port-security", portName,
			fmt.Sprintf("%s %s", annotation.MAC, annotation.IP))
		if err != nil {
			return fmt.Errorf("error while setting port security for logical port %s "+
				"stdout: %q, stderr: %q (%v)", portName, out, stderr, err)
		}
		oc.logicalPortCache[portName] = logicalSwitch
		oc.addPodToNamespace(pod.Namespace, annotation.IP.IP, portName)
		return nil
	}

	addressStr := "dynamic"
	networks, err := util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
	if err != nil || (networks != nil && len(networks) != 1) {
		return fmt.Errorf("error while getting custom MAC config for port %q from "+
			"default-network's network-attachment: %v", portName, err)
	} else if networks != nil && networks[0].MacRequest != "" {
		logrus.Debugf("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
		addressStr = networks[0].MacRequest + " dynamic"
	}
	out, stderr, err = util.RunOVNNbctl(
		"--", "--may-exist", "lsp-add", logicalSwitch, portName,
		"--", "lsp-set-addresses", portName, addressStr,
		"--", "set", "logical_switch_port", portName, "external-ids:namespace="+pod.Namespace,
		"external-ids:logical_switch="+logicalSwitch, "external-ids:pod=true")
	if err != nil {
		return fmt.Errorf("Error while creating logical port %s stdout: %q, stderr: %q (%v)",
			portName, out, stderr, err)
	}

	oc.logicalPortCache[portName] = logicalSwitch

	gatewayIP, err := oc.getGatewayFromSwitch(logicalSwitch)
	if err != nil {
		return fmt.Errorf("Error obtaining gateway address for switch %s", logicalSwitch)
	}

	podMac, podIP, err := waitForPodAddresses(portName)
	if err != nil {
		return err
	}

	podCIDR := &net.IPNet{IP: podIP, Mask: gatewayIP.Mask}

	// now set the port security for the logical switch port
	out, stderr, err = util.RunOVNNbctl("lsp-set-port-security", portName,
		fmt.Sprintf("%s %s", podMac, podCIDR))
	if err != nil {
		return fmt.Errorf("error while setting port security for logical port %s "+
			"stdout: %q, stderr: %q (%v)", portName, out, stderr, err)
	}

	marshalledAnnotation, err := util.MarshalPodAnnotation(&util.PodAnnotation{
		IP:  podCIDR,
		MAC: podMac,
		GW:  gatewayIP.IP,
	})
	if err != nil {
		return fmt.Errorf("error creating pod network annotation: %v", err)
	}

	oc.addPodToNamespace(pod.Namespace, podIP, portName)

	logrus.Debugf("Annotation values: ip=%s ; mac=%s ; gw=%s\nAnnotation=%s",
		podCIDR, podMac, gatewayIP, marshalledAnnotation)
	err = oc.kube.SetAnnotationsOnPod(pod, marshalledAnnotation)
	if err != nil {
		return fmt.Errorf("failed to set annotation on pod %s - %v", pod.Name, err)
	}

	// observe the pod creation latency metric.
	recordPodCreated(pod)

	return nil
}
