package ovn

import (
	"fmt"
	"strings"
	"time"

	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbtransaction"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/helpers"
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

	var existingLogicalPorts []string
	ports := oc.ovnNbCache.GetMap("Logical_Switch_Port", "name")
	for _, port := range ports {
		if externalIds, ok := port.(map[string]interface{})["external_ids"]; ok {
			if externalIds != nil {
				if pod, ok := externalIds.(map[string]interface{})["pod"]; ok {
					if pod == "true" {
						existingLogicalPorts = append(existingLogicalPorts, port.(map[string]interface{})["name"].(string))
					}
				}
			}
		}
	}

	switches := oc.ovnNbCache.GetMap("Logical_Switch", "uuid")
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			logrus.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)

			portID := ports[existingPort].(map[string]interface{})["uuid"].(string)

			// find corresponding switch
			var switchID string
			for uuid, sw := range switches {
				if _, ok := sw.(map[string]interface{})["ports"].(map[string]interface{})[portID]; ok {
					switchID = uuid
					break
				}
			}

			var retry = true
			for retry {
				_, _, retry = (*oc.ovnNBDB).Transaction("OVN_Northbound").DeleteReferences(dbtransaction.DeleteReferences{
					Table:           "Logical_Switch",
					WhereId:         switchID,
					ReferenceColumn: "ports",
					DeleteIdsList:   []string{portID},
					Wait:            true,
					Cache:           oc.ovnNbCache,
				}).Commit()
			}

			if !oc.portGroupSupport {
				oc.deletePodAcls(existingPort)
			}
		}
	}
}

func (oc *Controller) deletePodAcls(logicalPort string) {
	acls := oc.ovnNbCache.GetMap("ACL", "uuid")
	var aclUUIDs []string
	for uuid, acl := range acls {
		if externalIds, ok := acl.(map[string]interface{})["external_ids"]; ok {
			if externalIds != nil {
				if aclLogicalPort, ok := externalIds.(map[string]interface{})["logical_port"]; ok {
					if aclLogicalPort == logicalPort {
						aclUUIDs = append(aclUUIDs, uuid)
					}
				}
			}
		}
	}

	if len(aclUUIDs) == 0 {
		logrus.Debugf("deletePodAcls: returning because find " +
			"returned no ACLs")
		return
	}

	for _, uuid := range aclUUIDs {
		// Get logical switch
		switches := oc.ovnNbCache.GetMap("Logical_Switch", "uuid")
		var logicalSwitch string
		for swUUID, sw := range switches {
			if acls, ok := sw.(map[string]interface{})["acls"]; ok {
				if acls != nil {
					for aclID := range acls.(map[string]interface{}) {
						if aclID == uuid {
							logicalSwitch = swUUID
							break
						}
					}
				}
			}

			if logicalSwitch != "" {
				break
			}
		}
		if logicalSwitch == "" {
			logrus.Errorf("failed to get the logical_switch of acl "+
				"uuid=%s", uuid)
			continue
		}

		// delete the ACL rules on OVN that corresponding pod has been deleted
		var retry = true
		for retry {
			_, _, retry = (*oc.ovnNBDB).Transaction("OVN_Northbound").DeleteReferences(dbtransaction.DeleteReferences{
				Table:           "Logical_Switch",
				WhereId:         logicalSwitch,
				ReferenceColumn: "acls",
				DeleteIdsList:   []string{uuid},
				Wait:            true,
				Cache:           oc.ovnNbCache,
			}).Commit()
		}
	}
}

func (oc *Controller) getLogicalPortUUID(logicalPort string) string {
	if oc.logicalPortUUIDCache[logicalPort] != "" {
		return oc.logicalPortUUIDCache[logicalPort]
	}

	uuid := oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", logicalPort)["uuid"]

	if uuid == nil {
		return ""
	}

	oc.logicalPortUUIDCache[logicalPort] = uuid.(string)
	return oc.logicalPortUUIDCache[logicalPort]
}

func (oc *Controller) getGatewayFromSwitch(logicalSwitch string) (string, string, error) {
	var gatewayIPMaskStr string
	var ok bool

	if gatewayIPMaskStr, ok = oc.gatewayCache[logicalSwitch]; !ok {
		ls := oc.ovnNbCache.GetMap("Logical_Switch", "name", logicalSwitch)
		if externalIds, ok := ls["external_ids"]; ok {
			if externalIds != nil {
				if gatewayIPMaskStr, ok = externalIds.(map[string]interface{})["gateway_ip"].(string); ok {
					if gatewayIPMaskStr == "" {
						return "", "", fmt.Errorf("Empty gateway IP in logical switch %s", logicalSwitch)
					}

					oc.gatewayCache[logicalSwitch] = gatewayIPMaskStr
				}
			}
		}
		if gatewayIPMaskStr == "" {
			logrus.Errorf("Failed to get gateway")
			return "", "", fmt.Errorf("No gateway IP")
		}
	}

	gatewayIPMask := strings.Split(gatewayIPMaskStr, "/")
	gatewayIP := gatewayIPMask[0]
	mask := gatewayIPMask[1]
	logrus.Debugf("Gateway IP: %s, Mask: %s", gatewayIP, mask)
	return gatewayIP, mask, nil
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	logrus.Infof("Deleting pod: %s", pod.Name)
	logicalPort := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)

	portID := oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", logicalPort)["uuid"]

	if portID != nil {
		// find corresponding switch
		var switchID string
		var err error
		switches := oc.ovnNbCache.GetMap("Logical_Switch", "uuid")
		for uuid, sw := range switches {
			if _, ok := sw.(map[string]interface{})["ports"].(map[string]interface{})[portID.(string)]; ok {
				switchID = uuid
				break
			}
		}

		retry := true
		for retry {
			_, err, retry = oc.ovnNBDB.Transaction("OVN_Northbound").DeleteReferences(dbtransaction.DeleteReferences{
				Table:           "Logical_Switch",
				WhereId:         switchID,
				ReferenceColumn: "ports",
				DeleteIdsList:   []string{portID.(string)},
				Wait:            true,
				Cache:           oc.ovnNbCache,
			}).Commit()
		}

		if err != nil {
			logrus.Errorf("Error in deleting pod logical port (%v)", err)
		}
	}

	ipAddress := oc.getIPFromOvnAnnotation(pod.Annotations["ovn"])

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
	oc.deletePodFromNamespaceAddressSet(pod.Namespace, ipAddress)
	return
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

	if !oc.logicalSwitchCache[logicalSwitch] {
		oc.logicalSwitchCache[logicalSwitch] = true
		oc.addAllowACLFromNode(logicalSwitch)
	}

	portName := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	logrus.Debugf("Creating logical port for %s on switch %s", portName, logicalSwitch)

	annotation, isStaticIP := pod.Annotations["ovn"]

	// If pod already has annotations, just add the lsp with static ip/mac.
	// Else, create the lsp with dynamic addresses.
	if isStaticIP {
		ipAddress := oc.getIPFromOvnAnnotation(annotation)
		macAddress := oc.getMacFromOvnAnnotation(annotation)

		switchID := oc.ovnNbCache.GetMap("Logical_Switch", "name", logicalSwitch)["uuid"].(string)

		portID := oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", portName)["uuid"]

		if portID != nil {
			retry := true
			for retry {
				txn := oc.ovnNBDB.Transaction("OVN_Northbound")
				txn.Update(dbtransaction.Update{
					Table: "Logical_Switch_Port",
					Where: [][]interface{}{{"_uuid", "==", []string{"uuid", portID.(string)}}},
					Row: map[string]interface{}{
						"addresses":         fmt.Sprintf("%s %s", macAddress, ipAddress),
						"dynamic_addresses": dbtransaction.GetNil(),
					},
				})
				_, err, retry = txn.Commit()
			}
		} else {
			retry := true
			for retry {
				txn := oc.ovnNBDB.Transaction("OVN_Northbound")
				newPortID := txn.Insert(dbtransaction.Insert{
					Table: "Logical_Switch_Port",
					Row: map[string]interface{}{
						"name":      portName,
						"addresses": fmt.Sprintf("%s %s", macAddress, ipAddress),
					},
				})
				txn.InsertReferences(dbtransaction.InsertReferences{
					Table:           "Logical_Switch",
					WhereId:         switchID,
					ReferenceColumn: "ports",
					InsertIdsList:   []string{newPortID},
					Wait:            true,
					Cache:           oc.ovnNbCache,
				})
				_, err, retry = txn.Commit()
			}
		}
		if err != nil {
			logrus.Errorf("Failed to add logical port to switch (%v)", err)
			return
		}
	} else {
		switchID := oc.ovnNbCache.GetMap("Logical_Switch", "name", logicalSwitch)["uuid"].(string)

		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			newPortID := txn.Insert(dbtransaction.Insert{
				Table: "Logical_Switch_Port",
				Row: map[string]interface{}{
					"name":      portName,
					"addresses": "dynamic",
					"external_ids": helpers.MakeOVSDBMap(map[string]interface{}{
						"logical_switch": logicalSwitch,
						"namespace":      pod.Namespace,
						"pod":            "true",
					}),
				},
			})
			txn.InsertReferences(dbtransaction.InsertReferences{
				Table:           "Logical_Switch",
				WhereId:         switchID,
				ReferenceColumn: "ports",
				InsertIdsList:   []string{newPortID},
				Wait:            true,
				Cache:           oc.ovnNbCache,
			})
			_, err, retry = txn.Commit()
		}
		if err != nil {
			logrus.Errorf("Failed to add dynamic logical port to switch (%v)", err)
			return
		}
	}

	oc.logicalPortCache[portName] = logicalSwitch

	gatewayIP, mask, err := oc.getGatewayFromSwitch(logicalSwitch)
	if err != nil {
		logrus.Errorf("Error obtaining gateway address for switch %s", logicalSwitch)
		return
	}

	count := 30
	for count > 0 {
		var addr interface{}
		if isStaticIP {
			addr = oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", portName)["addresses"]
		} else {
			addr = oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", portName)["dynamic_addresses"]
		}
		if addr != nil {
			out = addr.(string)
			break
		}
		time.Sleep(time.Second)
		count--
	}
	if count == 0 {
		logrus.Errorf("Error while obtaining addresses for %s "+
			"stdout: %q, stderr: %q, (%v)", portName, out, stderr, err)
		return
	}

	// static addresses have format ["0a:00:00:00:00:01 192.168.1.3"], while
	// dynamic addresses have format "0a:00:00:00:00:01 192.168.1.3".
	outStr := strings.TrimLeft(out, `[`)
	outStr = strings.TrimRight(outStr, `]`)
	outStr = strings.Trim(outStr, `"`)
	addresses := strings.Split(outStr, " ")
	if len(addresses) != 2 {
		logrus.Errorf("Error while obtaining addresses for %s", portName)
		return
	}

	if !isStaticIP {
		annotation = fmt.Sprintf(`{\"ip_address\":\"%s/%s\", \"mac_address\":\"%s\", \"gateway_ip\": \"%s\"}`, addresses[1], mask, addresses[0], gatewayIP)
		logrus.Debugf("Annotation values: ip=%s/%s ; mac=%s ; gw=%s\nAnnotation=%s", addresses[1], mask, addresses[0], gatewayIP, annotation)
		err = oc.kube.SetAnnotationOnPod(pod, "ovn", annotation)
		if err != nil {
			logrus.Errorf("Failed to set annotation on pod %s - %v", pod.Name, err)
		}
	}
	oc.addPodToNamespaceAddressSet(pod.Namespace, addresses[1])

	return
}

// AddLogicalPortWithIP add logical port with static ip address
// and mac adddress for the pod
func (oc *Controller) AddLogicalPortWithIP(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	portName := fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
	logicalSwitch := pod.Spec.NodeName
	logrus.Debugf("Creating logical port for %s on switch %s", portName,
		logicalSwitch)

	annotation, ok := pod.Annotations["ovn"]
	if !ok {
		logrus.Errorf("Failed to get ovn annotation from pod!")
		return
	}
	ipAddress := oc.getIPFromOvnAnnotation(annotation)
	macAddress := oc.getMacFromOvnAnnotation(annotation)

	switchID := oc.ovnNbCache.GetMap("Logical_Switch", "name", logicalSwitch)["uuid"].(string)

	portID := oc.ovnNbCache.GetMap("Logical_Switch_Port", "name", portName)["uuid"]

	var err error
	if portID != nil {
		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			txn.Update(dbtransaction.Update{
				Table: "Logical_Switch_Port",
				Where: [][]interface{}{{"_uuid", "==", []string{"uuid", portID.(string)}}},
				Row: map[string]interface{}{
					"addresses": fmt.Sprintf("%s %s", macAddress, ipAddress),
				},
			})
			_, err, retry = txn.Commit()
		}
	} else {
		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			newPortID := txn.Insert(dbtransaction.Insert{
				Table: "Logical_Switch_Port",
				Row: map[string]interface{}{
					"name":      portName,
					"addresses": fmt.Sprintf("%s %s", macAddress, ipAddress),
				},
			})
			txn.InsertReferences(dbtransaction.InsertReferences{
				Table:           "Logical_Switch",
				WhereId:         switchID,
				ReferenceColumn: "ports",
				InsertIdsList:   []string{newPortID},
				Wait:            true,
				Cache:           oc.ovnNbCache,
			})
			_, err, retry = txn.Commit()
		}
	}

	if err != nil {
		logrus.Errorf("Failed to add logical port to switch (%v)", err)
		return
	}
}
