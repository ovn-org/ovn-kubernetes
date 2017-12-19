package ovn

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

func (oc *Controller) syncPods(pods []interface{}) {
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			logrus.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
		expectedLogicalPorts[logicalPort] = true
	}

	// get the list of logical ports from OVN
	output, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=name", "find", "logical_switch_port", "external_ids:pod=true").Output()
	if err != nil {
		logrus.Errorf("Error in obtaining list of logical ports from OVN: %v", err)
		return
	}
	existingLogicalPorts := strings.Fields(string(output))
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			logrus.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			out, err := exec.Command(OvnNbctl, "--if-exists", "lsp-del",
				existingPort).CombinedOutput()
			if err != nil {
				logrus.Errorf("Error in deleting pod's logical port in ovn - %s (%v)",
					string(out), err)
			}
			oc.deletePodAcls(existingPort)
		}
	}
}

func (oc *Controller) deletePodAcls(logicalPort string) {
	// delete the ACL rules on OVN that corresponding pod has been deleted
	uuids, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external_ids:logical_port=%s", logicalPort)).Output()
	if err != nil {
		logrus.Errorf("Error in getting list of acls")
		return
	}

	if string(uuids) == "" {
		logrus.Debugf("deletePodAcls: returning because find " +
			"returned no ACLs")
		return
	}

	uuidSlice := strings.Fields(string(uuids))
	for _, uuid := range uuidSlice {
		// Get logical switch
		out, err := exec.Command(OvnNbctl, "--data=bare",
			"--no-heading", "--columns=_uuid", "find", "logical_switch",
			fmt.Sprintf("acls{>=}%s", uuid)).Output()
		if err != nil {
			logrus.Errorf("find failed to get the logical_switch of acl"+
				"uuid=%s (%v)", uuid, err)
			continue
		}

		if string(out) == "" {
			continue
		}
		logicalSwitch := strings.TrimSpace(string(out))

		_, err = exec.Command(OvnNbctl, "remove", "logical_switch",
			logicalSwitch, "acls", uuid).Output()
		if err != nil {
			logrus.Errorf("remove failed to delete the allow-from rule %s for"+
				" logical_switch=%s, logical_port=%s (%s)",
				uuid, logicalSwitch, logicalPort, err)
			continue
		}
	}
}

func (oc *Controller) getGatewayFromSwitch(logicalSwitch string) (string, string, error) {
	var gatewayIPMaskStr string
	var ok bool
	if gatewayIPMaskStr, ok = oc.gatewayCache[logicalSwitch]; !ok {
		gatewayIPBytes, err := exec.Command(OvnNbctl, "--if-exists", "get",
			"logical_switch", logicalSwitch,
			"external_ids:gateway_ip").Output()
		if err != nil {
			logrus.Debugf("Gateway IP:  %s, %v", string(gatewayIPBytes), err)
			return "", "", err
		}
		gatewayIPMaskStr = strings.TrimFunc(string(gatewayIPBytes), unicode.IsSpace)
		gatewayIPMaskStr = strings.Trim(gatewayIPMaskStr, `"`)
		oc.gatewayCache[logicalSwitch] = gatewayIPMaskStr
	}
	gatewayIPMask := strings.Split(gatewayIPMaskStr, "/")
	gatewayIP := gatewayIPMask[0]
	mask := gatewayIPMask[1]
	logrus.Debugf("Gateway IP: %s, Mask: %s", gatewayIP, mask)
	return gatewayIP, mask, nil
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	logrus.Debugf("Deleting pod: %s", pod.Name)
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	out, err := exec.Command(OvnNbctl, "--if-exists", "lsp-del",
		logicalPort).CombinedOutput()
	if err != nil {
		logrus.Errorf("Error in deleting pod network switch - %s (%v)",
			string(out), err)
	}

	ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])

	delete(oc.logicalPortCache, logicalPort)

	oc.lspMutex.Lock()
	delete(oc.lspIngressDenyCache, logicalPort)
	delete(oc.lspEgressDenyCache, logicalPort)
	oc.lspMutex.Unlock()

	oc.deleteACLDeny(pod.Namespace, pod.Spec.NodeName, logicalPort, "Ingress")
	oc.deleteACLDeny(pod.Namespace, pod.Spec.NodeName, logicalPort, "Egress")
	oc.deletePodFromNamespaceAddressSet(pod.Namespace, ipAddress)
	return
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) {
	var logicalSwitch string

	// If we don't have the logical switch
	switch oc.podSubnetMode {
	case PodSubnetModeNode:
		logicalSwitch = pod.Spec.NodeName
		for count := 30; count > 0; count-- {
			p, err := oc.Kube.GetPod(pod.Namespace, pod.Name)
			if err != nil {
				logrus.Errorf("Could not get pod %s/%s for obtaining the logical switch it belongs to", pod.Namespace, pod.Name)
				continue
			}
			logicalSwitch = p.Spec.NodeName
			if logicalSwitch != "" {
				break
			}
			time.Sleep(time.Second)
		}
	}

	if logicalSwitch == "" {
		logrus.Errorf("Could not find the logical switch that the pod %s/%s belongs to", pod.Namespace, pod.Name)
		return
	}

	if !oc.logicalSwitchCache[logicalSwitch] {
		oc.logicalSwitchCache[logicalSwitch] = true
		oc.addAllowACLFromNode(logicalSwitch)
	}

	portName := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	logrus.Debugf("Creating logical port for %s on switch %s", portName, logicalSwitch)

	out, err := exec.Command(OvnNbctl, "--wait=sb", "--", "--may-exist", "lsp-add",
		logicalSwitch, portName, "--", "lsp-set-addresses",
		portName, "dynamic", "--", "set",
		"logical_switch_port", portName,
		"external-ids:namespace="+pod.Namespace,
		"external-ids:logical_switch="+logicalSwitch,
		"external-ids:pod=true").CombinedOutput()
	if err != nil {
		logrus.Errorf("Error while creating logical port %s - %v (%s)", portName, err, string(out))
		return
	}

	oc.logicalPortCache[portName] = logicalSwitch

	gatewayIP, mask, err := oc.getGatewayFromSwitch(logicalSwitch)
	if err != nil {
		logrus.Errorf("Error obtaining gateway address for switch %s", logicalSwitch)
		return
	}

	count := 30
	for count > 0 {
		out, err = exec.Command(OvnNbctl, "get", "logical_switch_port", portName, "dynamic_addresses").Output()
		if err == nil {
			break
		}
		logrus.Debugf("Error while obtaining addresses for %s - %v", portName, err)
		time.Sleep(time.Second)
		count--
	}
	if count == 0 {
		logrus.Errorf("Error while obtaining addresses for %s", portName)
		return
	}

	outStr := strings.TrimFunc(string(out), unicode.IsSpace)
	outStr = strings.Trim(outStr, `"`)
	addresses := strings.Split(outStr, " ")
	if len(addresses) != 2 {
		logrus.Errorf("Error while obtaining addresses for %s", portName)
		return
	}

	annotation := fmt.Sprintf(`{\"ip_address\":\"%s/%s\", \"mac_address\":\"%s\", \"gateway_ip\": \"%s\"}`, addresses[1], mask, addresses[0], gatewayIP)
	logrus.Debugf("Annotation values: ip=%s/%s ; mac=%s ; gw=%s\nAnnotation=%s", addresses[1], mask, addresses[0], gatewayIP, annotation)
	err = oc.Kube.SetAnnotationOnPod(pod, "ovn", annotation)
	if err != nil {
		logrus.Errorf("Failed to set annotation on pod %s - %v", pod.Name, err)
	}

	oc.addPodToNamespaceAddressSet(pod.Namespace, addresses[1])

	return
}
