// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"errors"
	"fmt"
	"strconv"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func clearPodBandwidth(ovsClient libovsdbclient.Client, sandboxID string) error {
	ops, err := clearPodBandwidthOps(ovsClient, sandboxID)
	if err != nil {
		return err
	}
	_, err = ovsops.TransactAndCheck(ovsClient, ops)
	return err
}

func clearPodBandwidthOps(ovsClient libovsdbclient.Client, sandboxID string) ([]ovsdb.Operation, error) {
	ifaces, err := ovsops.FindInterfacesWithPredicate(ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.ExternalIDs["sandbox"] == sandboxID
	})
	if err != nil {
		return nil, err
	}
	portNames := make(map[string]struct{}, len(ifaces))
	for _, iface := range ifaces {
		portNames[iface.Name] = struct{}{}
	}

	var ops []ovsdb.Operation
	for name := range portNames {
		port, err := ovsops.GetOVSPort(ovsClient, name)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		update := &vswitchd.Port{UUID: port.UUID}
		portOps, err := ovsClient.Where(update).Update(update, &update.QOS)
		if err != nil {
			return nil, err
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
		return nil, err
	}
	ops = append(ops, qosOps...)

	return ops, nil
}

func setPodBandwidthOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, sandboxID, ifname string, ingressBPS, egressBPS int64) ([]ovsdb.Operation, error) {
	// Pod ingress is OVS egress, and pod egress is OVS ingress.

	if ingressBPS > 0 {
		port, err := ovsops.GetOVSPort(ovsClient, ifname)
		if err != nil {
			return nil, fmt.Errorf("failed to get OVS port %s: %w", ifname, err)
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
			return nil, fmt.Errorf("failed to build QoS creation operation for port %s: %w", ifname, err)
		}
		ops = append(ops, qosOps...)

		portUpdate := &vswitchd.Port{UUID: port.UUID, QOS: &qos.UUID}
		portOps, err := ovsClient.Where(&vswitchd.Port{UUID: port.UUID}).Update(portUpdate, &portUpdate.QOS)
		if err != nil {
			return nil, fmt.Errorf("failed to build QoS update operation for port %s: %w", ifname, err)
		}
		ops = append(ops, portOps...)
	}

	// Zero removes this interface's limit without changing other attachments.
	iface, err := ovsops.GetOVSInterface(ovsClient, ifname)
	if err != nil {
		return nil, fmt.Errorf("failed to get OVS interface %s: %w", ifname, err)
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
		return nil, fmt.Errorf("failed to build policing update operation for interface %s: %w", ifname, err)
	}
	ops = append(ops, ifaceOps...)

	return ops, nil
}
