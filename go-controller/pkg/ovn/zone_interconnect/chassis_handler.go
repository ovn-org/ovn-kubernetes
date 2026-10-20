// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zoneinterconnect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ZoneChassisHandler creates chassis records for the remote zone nodes
// in the OVN Southbound DB. It also creates the encap records.
type ZoneChassisHandler struct {
	sbClient          libovsdbclient.Client
	getNodes          func() ([]*corev1.Node, error)
	localNodeName     string
	getLocalChassisID func() (string, error)
}

// NewZoneChassisHandler returns a new ZoneChassisHandler instance
func NewZoneChassisHandler(sbClient libovsdbclient.Client, localNodeName string, getNodes func() ([]*corev1.Node, error)) *ZoneChassisHandler {
	return &ZoneChassisHandler{
		sbClient:          sbClient,
		getNodes:          getNodes,
		localNodeName:     localNodeName,
		getLocalChassisID: util.GetNodeChassisID,
	}
}

// AddLocalZoneNode marks the chassis entry for the node in the SB DB to a local chassis
func (zch *ZoneChassisHandler) AddLocalZoneNode(node *corev1.Node) error {
	if err := zch.createOrUpdateNodeChassis(node, false); err != nil {
		return fmt.Errorf("failed to update chassis to local for local node %s, error: %w", node.Name, err)
	}

	return nil
}

// AddRemoteZoneNode creates the remote chassis for the remote zone node in the SB DB or marks
// the entry as remote if it was local chassis earlier.
func (zch *ZoneChassisHandler) AddRemoteZoneNode(node *corev1.Node) error {
	if err := zch.createOrUpdateNodeChassis(node, true); err != nil {
		return fmt.Errorf("failed to create or update chassis to remote for remote node %s, error: %w", node.Name, err)
	}

	return nil
}

// DeleteRemoteZoneNode deletes the remote chassis (if it exists) for the node.
func (zch *ZoneChassisHandler) DeleteRemoteZoneNode(node *corev1.Node) error {
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		klog.Infof("Failed to parse node chassis-id for node %s, will remove chassis by node ownership: %v", node.Name, err)
	}
	p := func(chassis *sbdb.Chassis) bool {
		return (err != nil || chassis.Name == chassisID) &&
			libovsdbops.ChassisOwnedByNode(chassis, node.Name) &&
			strings.EqualFold(chassis.OtherConfig["is-remote"], "true")
	}
	return libovsdbops.DeleteChassisWithPredicate(zch.sbClient, p)
}

// SyncNodes cleans up the remote chassis records in the OVN Southbound db
// for the stale nodes
func (zic *ZoneChassisHandler) SyncNodes(kNodes []interface{}) error {
	chassis, err := libovsdbops.ListChassis(zic.sbClient)

	if err != nil {
		return fmt.Errorf("failed to get the list of chassis from OVN Southbound db : %w", err)
	}

	foundNodes := sets.New[string]()
	for _, tmp := range kNodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}
		foundNodes.Insert(node.Name)
	}

	for _, ch := range chassis {
		if ch.OtherConfig != nil && strings.ToLower(ch.OtherConfig["is-remote"]) == "true" {
			owner := ch.ExternalIDs[libovsdbops.ChassisNodeName]
			if owner == "" {
				owner = ch.Hostname
			}
			if !foundNodes.Has(owner) {
				// Its a stale remote chassis, delete it.
				if err = libovsdbops.DeleteChassisWithPredicate(zic.sbClient, func(current *sbdb.Chassis) bool {
					return current.UUID == ch.UUID && libovsdbops.ChassisOwnedByNode(current, owner) &&
						strings.EqualFold(current.OtherConfig["is-remote"], "true")
				}); err != nil {
					return fmt.Errorf("failed to delete remote stale chassis for node %s : %w", ch.Hostname, err)
				}
			}
		}
	}

	return nil
}

// createOrUpdateNodeChassis creates or updates the node chassis to local or remote.
func (zch *ZoneChassisHandler) createOrUpdateNodeChassis(node *corev1.Node, isRemote bool) error {
	// Get the chassis id.
	chassisID, err := util.ParseNodeChassisIDAnnotation(node)
	if err != nil {
		parsedErr := err
		if isRemote {
			parsedErr = ovntypes.NewSuppressedError(err)
		}
		return fmt.Errorf("failed to parse node chassis-id for node - %s, error: %w",
			node.Name, parsedErr)
	}

	nodes, err := zch.getNodes()
	if err != nil {
		return fmt.Errorf("failed to check chassis ID uniqueness: %w", err)
	}
	if err := util.ValidateNodeChassisID(node.Name, chassisID, nodes); err != nil {
		return err
	}

	// Get the encap IPs.
	encapIPs, err := util.ParseNodeEncapIPsAnnotation(node)
	if err != nil {
		return fmt.Errorf("failed to parse node-encap-ips for node - %s, error: %w",
			node.Name, err)
	}

	encaps := make([]*sbdb.Encap, 0, len(encapIPs))
	encapOptions := map[string]string{}
	encapOptions["csum"] = "true"
	// set the geneve port if using something else than default
	if config.Default.EncapPort != config.DefaultEncapPort {
		encapOptions["dst_port"] = strconv.FormatUint(uint64(config.Default.EncapPort), 10)
	}

	for _, ovnEncapIP := range encapIPs {
		encap := sbdb.Encap{
			ChassisName: chassisID,
			IP:          strings.TrimSpace(ovnEncapIP),
			Type:        "geneve",
			Options:     encapOptions,
		}
		encaps = append(encaps, &encap)
	}

	chassis := sbdb.Chassis{
		Name:        chassisID,
		ExternalIDs: map[string]string{libovsdbops.ChassisNodeName: node.Name},
		OtherConfig: map[string]string{
			"is-remote": strconv.FormatBool(isRemote),
		},
	}
	if isRemote {
		// For debugging purposes we add KAPI node name as the chassis hostname.
		// Legacy remote chassis also use this name when adopting explicit ownership.
		// There is no need to set it for the local node, as ovn-controller will
		// set it automatically from the OVS external_id:hostname field.
		chassis.Hostname = node.Name
	}

	if !isRemote {
		existing, err := libovsdbops.GetChassis(zch.sbClient, &sbdb.Chassis{Name: chassisID})
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			return err
		}
		if err == nil && !libovsdbops.ChassisOwnedByNode(existing, node.Name) &&
			existing.ExternalIDs[libovsdbops.ChassisNodeName] == "" &&
			!strings.EqualFold(existing.OtherConfig["is-remote"], "true") {
			if node.Name != zch.localNodeName {
				return fmt.Errorf("cannot adopt local chassis for node %q: controller belongs to %q", node.Name, zch.localNodeName)
			}
			localChassisID, err := zch.getLocalChassisID()
			if err != nil {
				return fmt.Errorf("failed to verify local OVS chassis identity: %w", err)
			}
			return libovsdbops.CreateOrUpdateLocalChassis(zch.sbClient, &chassis, zch.localNodeName, localChassisID, encaps...)
		}
	}
	return libovsdbops.CreateOrUpdateChassis(zch.sbClient, &chassis, encaps...)
}
