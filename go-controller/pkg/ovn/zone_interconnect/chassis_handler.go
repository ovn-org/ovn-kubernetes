package zoneinterconnect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ZoneChassisHandler creates chassis records for the remote zone nodes
// in the OVN Southbound DB. It also creates the encap records.
type ZoneChassisHandler struct {
	sbClient libovsdbclient.Client
}

// NewZoneChassisHandler returns a new ZoneChassisHandler instance
func NewZoneChassisHandler(sbClient libovsdbclient.Client) *ZoneChassisHandler {
	return &ZoneChassisHandler{
		sbClient: sbClient,
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
		return fmt.Errorf("failed to parse node chassis-id for node - %s, error: %w", node.Name, err)
	}

	ch := &sbdb.Chassis{
		Name:     chassisID,
		Hostname: node.Name,
	}

	chassis, err := libovsdbops.GetChassis(zch.sbClient, ch)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			// Nothing to do
			return nil
		}
		return fmt.Errorf("failed to get the chassis record for the remote zone node %s, error: %w", node.Name, err)
	}
	if chassis.OtherConfig != nil && strings.ToLower(chassis.OtherConfig["is-remote"]) == "true" {
		// Its a remote chassis, delete it.
		return libovsdbops.DeleteChassis(zch.sbClient, chassis)
	}

	return nil
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
			if !foundNodes.Has(ch.Hostname) {
				// Its a stale remote chassis, delete it.
				if err = libovsdbops.DeleteChassis(zic.sbClient, ch); err != nil {
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

	nodePrimaryIp, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return fmt.Errorf("failed to parse node %s primary IP %w", node.Name, err)
	}

	chassis := sbdb.Chassis{
		Name:     chassisID,
		Hostname: node.Name,
		OtherConfig: map[string]string{
			"is-remote": strconv.FormatBool(isRemote),
		},
	}

	encap := sbdb.Encap{
		ChassisName: chassisID,
		IP:          nodePrimaryIp,
		Type:        "geneve",
		Options:     map[string]string{"csum": "true"},
	}

	return libovsdbops.CreateOrUpdateChassis(zch.sbClient, &chassis, &encap)
}
