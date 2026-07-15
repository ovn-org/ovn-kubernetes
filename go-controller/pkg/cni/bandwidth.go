// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"
	"strconv"
	"strings"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func clearPodBandwidth(ovsClient libovsdbclient.Client, sandboxID string) error {
	if ovsClient != nil {
		return clearPodBandwidthWithOVSClient(ovsClient, sandboxID)
	}

	// interfaces will have the same name as ports
	portList, err := ovsFind("interface", "name", "external-ids:sandbox="+sandboxID)
	if err != nil {
		return err
	}

	return clearPodBandwidthForPorts(portList, sandboxID)
}

func clearPodBandwidthWithOVSClient(ovsClient libovsdbclient.Client, sandboxID string) error {
	ifaces, err := ovsops.FindInterfacesWithPredicate(ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.ExternalIDs["sandbox"] == sandboxID
	})
	if err != nil {
		return err
	}
	portNames := make(map[string]struct{}, len(ifaces))
	for _, iface := range ifaces {
		portNames[iface.Name] = struct{}{}
	}

	var ops []ovsdb.Operation
	ports, err := ovsops.FindOVSPortsWithPredicate(ovsClient, func(port *vswitchd.Port) bool {
		_, ok := portNames[port.Name]
		return ok
	})
	if err != nil {
		return err
	}
	for _, port := range ports {
		update := &vswitchd.Port{UUID: port.UUID}
		portOps, err := ovsClient.Where(update).Update(update, &update.QOS)
		if err != nil {
			return err
		}
		ops = append(ops, portOps...)
	}

	qos := &vswitchd.QoS{}
	qosOps, err := ovsClient.WhereAll(qos, model.Condition{
		Field:    &qos.ExternalIDs,
		Function: ovsdb.ConditionIncludes,
		Value:    map[string]string{"sandbox": sandboxID},
	}).Delete()
	if err != nil {
		return err
	}
	ops = append(ops, qosOps...)

	_, err = ovsops.TransactAndCheck(ovsClient, ops)
	return err
}

func clearPodBandwidthForPorts(portList []string, sandboxID string) error {
	// Clear the QoS for any ports of this sandbox
	for _, port := range portList {
		if err := ovsClear("port", port, "qos"); err != nil {
			return err
		}
	}

	// Now that the QoS is unused remove it
	qosList, err := ovsFind("qos", "_uuid", "external-ids:sandbox="+sandboxID)
	if err != nil {
		return err
	}
	for _, qos := range qosList {
		if err := ovsDestroy("qos", qos); err != nil {
			return err
		}
	}

	return nil
}

func setPodBandwidth(ovsClient libovsdbclient.Client, sandboxID, ifname string, ingressBPS, egressBPS int64) error {
	// Pod ingress is OVS egress, and pod egress is OVS ingress.
	if ovsClient != nil {
		return setPodBandwidthWithOVSClient(ovsClient, sandboxID, ifname, ingressBPS, egressBPS)
	}

	return setPodBandwidthWithExec(sandboxID, ifname, ingressBPS, egressBPS)
}

func setPodBandwidthWithOVSClient(ovsClient libovsdbclient.Client, sandboxID, ifname string, ingressBPS, egressBPS int64) error {
	var ops []ovsdb.Operation

	if ingressBPS > 0 {
		port, err := ovsops.GetOVSPort(ovsClient, ifname)
		if err != nil {
			return fmt.Errorf("failed to get OVS port %s: %w", ifname, err)
		}
		qos := &vswitchd.QoS{
			// The named UUID lets the port update reference this row in the
			// same transaction while OVSDB assigns its persistent UUID.
			UUID:        "podQoS",
			Type:        "linux-htb",
			OtherConfig: map[string]string{"max-rate": strconv.FormatInt(ingressBPS, 10)},
			ExternalIDs: map[string]string{"sandbox": sandboxID},
		}
		qosOps, err := ovsClient.Create(qos)
		if err != nil {
			return fmt.Errorf("failed to build QoS creation operation for port %s: %w", ifname, err)
		}
		ops = append(ops, qosOps...)

		portUpdate := &vswitchd.Port{UUID: port.UUID, QOS: &qos.UUID}
		portOps, err := ovsClient.Where(&vswitchd.Port{UUID: port.UUID}).Update(portUpdate, &portUpdate.QOS)
		if err != nil {
			return fmt.Errorf("failed to build QoS update operation for port %s: %w", ifname, err)
		}
		ops = append(ops, portOps...)
	}

	if egressBPS > 0 {
		iface, err := ovsops.GetOVSInterface(ovsClient, ifname)
		if err != nil {
			return fmt.Errorf("failed to get OVS interface %s: %w", ifname, err)
		}
		egressKBPS := int(egressBPS / 1000)
		ifaceUpdate := &vswitchd.Interface{
			UUID:                 iface.UUID,
			IngressPolicingRate:  egressKBPS,
			IngressPolicingBurst: egressKBPS / 10,
		}
		ifaceOps, err := ovsClient.Where(&vswitchd.Interface{UUID: iface.UUID}).Update(ifaceUpdate,
			&ifaceUpdate.IngressPolicingRate, &ifaceUpdate.IngressPolicingBurst)
		if err != nil {
			return fmt.Errorf("failed to build policing update operation for interface %s: %w", ifname, err)
		}
		ops = append(ops, ifaceOps...)
	}

	_, err := ovsops.TransactAndCheck(ovsClient, ops)
	return err
}

func setPodBandwidthWithExec(sandboxID, ifname string, ingressBPS, egressBPS int64) error {
	// note pod ingress == OVS egress and vice versa

	if ingressBPS > 0 {
		qos, err := ovsCreate("qos", "type=linux-htb", fmt.Sprintf("other-config:max-rate=%d", ingressBPS), "external-ids=sandbox="+sandboxID)
		if err != nil {
			return err
		}
		err = ovsSet("port", ifname, fmt.Sprintf("qos=%s", qos))
		if err != nil {
			return err
		}
	}
	if egressBPS > 0 {
		// ingress_policing_rate is in Kbps
		egressKBPS := egressBPS / 1000
		err := ovsSet("interface", ifname, fmt.Sprintf("ingress_policing_rate=%d", egressKBPS))
		if err != nil {
			return err
		}
		// Set the ingress_policing_burst too per recommendation in ovsdb schema, i.e
		// 10% of the rate
		err = ovsSet("interface", ifname, fmt.Sprintf("ingress_policing_burst=%d", (egressKBPS/10)))
		if err != nil {
			return err
		}
	}

	return nil
}

func getOvsPortBandwidth(ifname string, dir direction) (int64, error) {
	// note pod ingress == OVS egress and vice versa
	// so we ingress_policing_rate is egress and max-rate is ingress from the pod's
	// perspective

	// ingressBPS
	if dir == Ingress {
		return getInterfaceIngressBandwith(ifname)
	}
	// egreessBPS
	return getInterfaceEgressBandwith(ifname)
}

func getInterfaceIngressBandwith(ifname string) (int64, error) {
	qos_id, err := ovsGet("port", ifname, "qos", "")
	if err != nil {
		return 0, fmt.Errorf("failed to get qos for port %s: %w", ifname, err)
	}
	if len(qos_id) == 0 {
		return 0, BandwidthNotFound
	}
	maxRate, err := ovsGet("qos", qos_id, "other_config", "max-rate")
	if err != nil {
		return 0, fmt.Errorf("failed to get max-rate for qos_id %s: %w", qos_id, err)
	}
	if len(maxRate) == 0 {
		return 0, BandwidthNotFound
	}
	maxRate = strings.ReplaceAll(maxRate, "\"", "")
	ingressBPS, err := strconv.ParseInt(maxRate, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse qos max rate for %s: %w", ifname, err)
	}
	return ingressBPS, nil
}

func getInterfaceEgressBandwith(ifname string) (int64, error) {
	// egressBPS
	out, err := ovsGet("interface", ifname, "ingress_policing_rate", "")
	if err != nil {
		return 0, fmt.Errorf("failed to get ingress_policing_rate for interface %s: %w", ifname, err)
	}
	if len(out) == 0 {
		return 0, BandwidthNotFound
	}
	egressValue, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse ingress_policing_rate for interface %s from %q: %w", ifname, out, err)
	}
	if egressValue == 0 { // 0 is the default value so we return not found
		return 0, BandwidthNotFound
	}

	return egressValue * 1000, nil
}
