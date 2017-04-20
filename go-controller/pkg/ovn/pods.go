package ovn

import (
	"fmt"
	"github.com/golang/glog"
	"os/exec"
	"strings"
	"time"
	"unicode"

	kapi "k8s.io/client-go/pkg/api/v1"
)

func (oc *OvnController) getGatewayFromSwitch(logical_switch string) (string, string, error) {
	var gateway_ip_mask_str string
	var ok bool
	if gateway_ip_mask_str, ok = oc.gatewayCache[logical_switch]; !ok {
		gateway_ip_bytes, err := exec.Command(OVN_NBCTL, "--if-exists", "get",
			"logical_switch", logical_switch,
			"external_ids:gateway_ip").Output()
		if err != nil {
			glog.V(4).Infof("Gateway IP:  %s, %v", string(gateway_ip_bytes), err)
			return "", "", err
		}
		gateway_ip_mask_str = strings.TrimFunc(string(gateway_ip_bytes), unicode.IsSpace)
		gateway_ip_mask_str = strings.Trim(gateway_ip_mask_str, `"`)
		oc.gatewayCache[logical_switch] = gateway_ip_mask_str
	}
	gateway_ip_mask := strings.Split(gateway_ip_mask_str, "/")
	gateway_ip := gateway_ip_mask[0]
	mask := gateway_ip_mask[1]
	glog.V(4).Infof("Gateway IP: %s, Mask: %s", gateway_ip, mask)
	return gateway_ip, mask, nil
}

func (oc *OvnController) deleteLogicalPort(pod *kapi.Pod) {
	glog.V(4).Infof("Deleting pod: %s", pod.Name)
	out, err := exec.Command(OVN_NBCTL, "lsp-del", fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)).CombinedOutput()
	if err != nil {
		glog.Errorf("Error in deleting pod network switch - %v(%v)", out, err)
	}
	return
}

func (oc *OvnController) addLogicalPort(pod *kapi.Pod) {

	count := 30
	logical_switch := pod.Spec.NodeName
	for count > 0 {
		if logical_switch != "" {
			break
		}
		if count != 30 {
			time.Sleep(1 * time.Second)
		}
		count--
		p, err := oc.Kube.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			glog.Errorf("Could not get pod %s/%s for obtaining the logical switch it belongs to", pod.Namespace, pod.Name)
			continue
		}
		logical_switch = p.Spec.NodeName
	}
	if logical_switch == "" {
		glog.Errorf("Could not find the logical switch that the pod %s/%s belongs to", pod.Namespace, pod.Name)
		return
	}

	portName := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	glog.V(4).Infof("Creating logical port for %s on switch %s", portName, logical_switch)

	out, err := exec.Command(OVN_NBCTL, "--wait=sb", "--", "--may-exist", "lsp-add",
		logical_switch, portName, "--", "lsp-set-addresses",
		portName, "dynamic", "--", "set",
		"logical_switch_port", portName,
		"external-ids:namespace="+pod.Namespace,
		"external-ids:pod=true").CombinedOutput()
	if err != nil {
		glog.Errorf("Error while creating logical port %s - %v (%s)", portName, err, string(out))
		return
	}

	gateway_ip, mask, err := oc.getGatewayFromSwitch(logical_switch)
	if err != nil {
		glog.Errorf("Error obtaining gateway address for switch %s", logical_switch)
		return
	}

	count = 30
	for count > 0 {
		out, err = exec.Command(OVN_NBCTL, "get", "logical_switch_port", portName, "dynamic_addresses").Output()
		if err == nil {
			break
		}
		glog.V(4).Infof("Error while obtaining addresses for %s - %v", portName, err)
		time.Sleep(time.Second)
		count--;
	}
	if count == 0 {
		glog.Errorf("Error while obtaining addresses for %s", portName)
		return
	}

	outStr := strings.TrimFunc(string(out), unicode.IsSpace)
	outStr = strings.Trim(outStr, `"`)
	addresses := strings.Split(outStr, " ")
	if len(addresses) != 2 {
		glog.Errorf("Error while obtaining addresses for %s", portName)
		return
	}

	annotation := fmt.Sprintf(`{\"ip_address\":\"%s/%s\", \"mac_address\":\"%s\", \"gateway_ip\": \"%s\"}`, addresses[1], mask, addresses[0], gateway_ip)
	glog.V(4).Infof("Annotation values: ip=%s/%s ; mac=%s ; gw=%s\nAnnotation=%s", addresses[1], mask, addresses[0], gateway_ip, annotation)
	err = oc.Kube.SetAnnotationOnPod(pod, "ovn", annotation)
	if err != nil {
		glog.Errorf("Failed to set annotation on pod %s - %v", pod.Name, err)
	}
	return
}
