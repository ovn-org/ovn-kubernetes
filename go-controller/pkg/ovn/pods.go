package ovn

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/Sirupsen/logrus"
	kapi "k8s.io/client-go/pkg/api/v1"
)

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
	out, err := exec.Command(OvnNbctl, "--if-exists", "lsp-del",
		fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)).CombinedOutput()
	if err != nil {
		logrus.Errorf("Error in deleting pod network switch - %s (%v)",
			string(out), err)
	}
	return
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) {

	count := 30
	logicalSwitch := pod.Spec.NodeName
	for count > 0 {
		if logicalSwitch != "" {
			break
		}
		if count != 30 {
			time.Sleep(1 * time.Second)
		}
		count--
		p, err := oc.Kube.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			logrus.Errorf("Could not get pod %s/%s for obtaining the logical switch it belongs to", pod.Namespace, pod.Name)
			continue
		}
		logicalSwitch = p.Spec.NodeName
	}
	if logicalSwitch == "" {
		logrus.Errorf("Could not find the logical switch that the pod %s/%s belongs to", pod.Namespace, pod.Name)
		return
	}

	portName := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	logrus.Debugf("Creating logical port for %s on switch %s", portName, logicalSwitch)

	out, err := exec.Command(OvnNbctl, "--wait=sb", "--", "--may-exist", "lsp-add",
		logicalSwitch, portName, "--", "lsp-set-addresses",
		portName, "dynamic", "--", "set",
		"logical_switch_port", portName,
		"external-ids:namespace="+pod.Namespace,
		"external-ids:pod=true").CombinedOutput()
	if err != nil {
		logrus.Errorf("Error while creating logical port %s - %v (%s)", portName, err, string(out))
		return
	}

	gatewayIP, mask, err := oc.getGatewayFromSwitch(logicalSwitch)
	if err != nil {
		logrus.Errorf("Error obtaining gateway address for switch %s", logicalSwitch)
		return
	}

	count = 30
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
	return
}
