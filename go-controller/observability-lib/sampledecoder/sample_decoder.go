// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package sampledecoder

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/model"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/ovsdb"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type SampleDecoder struct {
	nbClient    client.Client
	ovsdbClient client.Client
	collectorID int
}

type Cookie struct {
	ObsDomainID uint32
	ObsPointID  uint32
}

const CookieSize = 8

var SampleEndian = getEndian()

func getEndian() binary.ByteOrder {
	// Use network bite order
	return binary.BigEndian
}

// getLocalNBClient only supports connecting to nbdb via unix socket.
// address is the path to the unix socket, e.g. "/var/run/ovn/ovnnb_db.sock"
func getLocalNBClient(ctx context.Context, address string) (client.Client, error) {
	libovsdbOvnNBClient, err := newNBClient(ctx, "unix:"+address)
	if err != nil {
		return nil, fmt.Errorf("error creating libovsdb client: %w", err)
	}
	return libovsdbOvnNBClient, nil
}

func getLocalOVSDBClient(ctx context.Context) (client.Client, error) {
	return newOVSDBClient(ctx, "unix:/var/run/openvswitch/db.sock")
}

// NewSampleDecoderWithCollector creates a new SampleDecoder, initializes the OVSDB client and adds the collector with the provided collectorID.
// It allows to set the groupID and ownerName for the created collector.
// If a collector with the same collectorID already exists on br-int and belongs to the same owner with the same
// groupID, it is reused (idempotent, e.g. after the owning process restarted without cleaning up).
// If it exists with a different owner or a different groupID, an error is returned.
// Shutdown should be called to clean up the collector and close the database clients.
func NewSampleDecoderWithCollector(ctx context.Context, nbdbSocketPath string, ownerName string, collectorID, groupID int) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx, nbdbSocketPath)
	if err != nil {
		return nil, err
	}
	ovsdbClient, err := getLocalOVSDBClient(ctx)
	if err != nil {
		nbClient.Close()
		return nil, err
	}
	decoder := &SampleDecoder{
		nbClient:    nbClient,
		ovsdbClient: ovsdbClient,
	}
	if err := decoder.AddCollector(collectorID, groupID, ownerName); err != nil {
		decoder.Shutdown()
		return nil, err
	}
	decoder.collectorID = collectorID
	return decoder, nil
}

// NewSampleDecoder creates a new SampleDecoder and initializes the OVSDB client.
// Shutdown should be called when the decoder is no longer needed.
func NewSampleDecoder(ctx context.Context, nbdbSocketPath string) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx, nbdbSocketPath)
	if err != nil {
		return nil, err
	}
	return &SampleDecoder{
		nbClient: nbClient,
	}, nil
}

// Shutdown removes the collector this decoder added (if any) and closes its database clients.
func (d *SampleDecoder) Shutdown() {
	if d.ovsdbClient != nil {
		if d.collectorID != 0 {
			if err := d.DeleteCollector(d.collectorID); err != nil {
				fmt.Printf("Error deleting collector with ID=%d: %v", d.collectorID, err)
			}
		}
		d.ovsdbClient.Close()
		d.ovsdbClient = nil
	}
	if d.nbClient != nil {
		d.nbClient.Close()
		d.nbClient = nil
	}
}

func getObservAppID(obsDomainID uint32) uint8 {
	return uint8(obsDomainID >> 24)
}

// findACLBySample relies on the client index based on sample_new and sample_est column.
func findACLBySample(nbClient client.Client, acl *nbdb.ACL) ([]*nbdb.ACL, error) {
	found := []*nbdb.ACL{}
	err := nbClient.Where(acl).List(context.Background(), &found)
	return found, err
}

func (d *SampleDecoder) DecodeCookieIDs(obsDomainID, obsPointID uint32) (model.NetworkEvent, error) {
	// Find sample using obsPointID
	sample, err := libovsdbops.FindSample(d.nbClient, int(obsPointID))
	if err != nil || sample == nil {
		return nil, fmt.Errorf("find sample failed: %w", err)
	}
	// find db object using observ application ID
	// Since ACL is indexed both by sample_new and sample_est, when searching by one of them,
	// we need to make sure the other one will not match.
	// nil is a valid index value, therefore we have to use non-existing UUID.
	wrongUUID := "wrongUUID"
	var dbObj interface{}
	switch getObservAppID(obsDomainID) {
	case observability.ACLNewTrafficSamplingID:
		acls, err := findACLBySample(d.nbClient, &nbdb.ACL{SampleNew: &sample.UUID, SampleEst: &wrongUUID})
		if err != nil {
			return nil, fmt.Errorf("find acl for sample failed: %w", err)
		}
		if len(acls) != 1 {
			return nil, fmt.Errorf("expected 1 ACL, got %d", len(acls))
		}
		dbObj = acls[0]
	case observability.ACLEstTrafficSamplingID:
		acls, err := findACLBySample(d.nbClient, &nbdb.ACL{SampleNew: &wrongUUID, SampleEst: &sample.UUID})
		if err != nil {
			return nil, fmt.Errorf("find acl for sample failed: %w", err)
		}
		if len(acls) != 1 {
			return nil, fmt.Errorf("expected 1 ACL, got %d", len(acls))
		}
		dbObj = acls[0]
	default:
		return nil, fmt.Errorf("unknown app ID: %d", getObservAppID(obsDomainID))
	}
	var event model.NetworkEvent
	switch o := dbObj.(type) {
	case *nbdb.ACL:
		event, err = newACLEvent(o)
		if err != nil {
			return nil, fmt.Errorf("failed to build ACL network event: %w", err)
		}
	}
	if event == nil {
		return nil, fmt.Errorf("failed to build network event for db object %v", dbObj)
	}
	return event, nil
}

func newACLEvent(o *nbdb.ACL) (*model.ACLEvent, error) {
	actor := o.ExternalIDs[libovsdbops.OwnerTypeKey.String()]
	event := model.ACLEvent{
		Action: o.Action,
		Actor:  actor,
	}
	switch actor {
	case libovsdbops.NetworkPolicyOwnerType:
		objName := o.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		nsname := strings.SplitN(objName, ":", 2)
		if len(nsname) == 2 {
			event.Namespace = nsname[0]
			event.Name = nsname[1]
		} else {
			return nil, fmt.Errorf("expected format namespace:name for Object Name, but found: %s", objName)
		}
		event.Direction = o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()]
	case libovsdbops.AdminNetworkPolicyOwnerType, libovsdbops.BaselineAdminNetworkPolicyOwnerType:
		event.Name = o.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		event.Direction = o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()]
	case libovsdbops.MulticastNamespaceOwnerType, libovsdbops.NetpolNamespaceOwnerType:
		event.Namespace = o.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		event.Direction = o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()]
	case libovsdbops.MulticastClusterOwnerType:
		event.Direction = o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()]
	case libovsdbops.EgressFirewallOwnerType:
		event.Namespace = o.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		event.Direction = "Egress"
	case libovsdbops.UDNIsolationOwnerType:
		event.Name = o.ExternalIDs[libovsdbops.ObjectNameKey.String()]
	case libovsdbops.NetpolNodeOwnerType:
		event.Direction = "Ingress"
	}
	return &event, nil
}

func (d *SampleDecoder) DecodeCookieBytes(cookie []byte) (model.NetworkEvent, error) {
	if uint64(len(cookie)) != CookieSize {
		return nil, fmt.Errorf("invalid cookie size: %d", len(cookie))
	}
	c := Cookie{}
	err := binary.Read(bytes.NewReader(cookie), SampleEndian, &c)
	if err != nil {
		return nil, err
	}
	return d.DecodeCookieIDs(c.ObsDomainID, c.ObsPointID)
}

func (d *SampleDecoder) DecodeCookie8Bytes(cookie [8]byte) (model.NetworkEvent, error) {
	c := Cookie{}
	err := binary.Read(bytes.NewReader(cookie[:]), SampleEndian, &c)
	if err != nil {
		return nil, err
	}
	return d.DecodeCookieIDs(c.ObsDomainID, c.ObsPointID)
}

func getGroupID(groupID *int) string {
	if groupID == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *groupID)
}

// validateCollectorReuse makes sure that an existing collector may be reused by a caller identified by ownerName/groupID.
// Reuse is only allowed when the existing collector has the same owner and the same group.
// A different owner or a different group is reported as a conflict.
func validateCollectorReuse(existing *ovsdb.FlowSampleCollectorSet, groupID int, ownerName string) error {
	if existing.ExternalIDs["owner"] != ownerName ||
		existing.LocalGroupID == nil || *existing.LocalGroupID != groupID {
		return fmt.Errorf("requested collector with id=%v is already in use "+
			"(owner=%q, local_group_id=%v)", existing.ID, existing.ExternalIDs["owner"], getGroupID(existing.LocalGroupID))
	}
	return nil
}

// getCollectorOnBrInt retrieves an existing collector set up on br-int, if any. It returns the collector, the br-int UUID, and an error, if any.
func getCollectorOnBrInt(ovsdbClient client.Client, collectorID int) (*ovsdb.FlowSampleCollectorSet, string, error) {
	// Retrieve br-int UUID
	bridges := []*ovsdb.Bridge{}
	err := ovsdbClient.WhereCache(func(item *ovsdb.Bridge) bool {
		return item.Name == "br-int"
	}).List(context.Background(), &bridges)
	if err != nil {
		return nil, "", fmt.Errorf("failed finding br-int: %w", err)
	}
	if len(bridges) != 1 {
		return nil, "", fmt.Errorf("expected exactly 1 br-int bridge, found %d", len(bridges))
	}
	brIntUUID := bridges[0].UUID

	// Retrieve collectors matching collectorID (any bridge)
	collectors := []*ovsdb.FlowSampleCollectorSet{}
	err = ovsdbClient.WhereCache(func(item *ovsdb.FlowSampleCollectorSet) bool {
		return item.ID == collectorID
	}).List(context.Background(), &collectors)
	if err != nil {
		return nil, "", fmt.Errorf("failed finding existing collector: %w", err)
	}

	// Return collector with bridge matching br-int
	for _, c := range collectors {
		if c.Bridge == brIntUUID {
			return c, brIntUUID, nil
		}
	}
	return nil, brIntUUID, nil
}

// AddCollector ensures a Flow_Sample_Collector_Set with the given collectorID exists on br-int,
// creating it if necessary or reusing an existing one owned by the same owner with the same group.
func (d *SampleDecoder) AddCollector(collectorID, groupID int, ownerName string) error {
	// Match the ObservabilityConfig CRD bounds (set_id is an OVS uint32) so both sides agree on
	// the valid range. collectorID 0 is additionally reserved: Shutdown treats a zero collectorID
	// as "no collector to clean up", so a collector created with ID 0 would leak. int64 comparison
	// keeps this correct on 32-bit platforms where int cannot hold math.MaxUint32.
	if collectorID < 1 || int64(collectorID) > math.MaxUint32 {
		return fmt.Errorf("collector ID must be between 1 and %d, got %d", int64(math.MaxUint32), collectorID)
	}
	if d.ovsdbClient == nil {
		return fmt.Errorf("OVSDB client is not initialized")
	}

	// Find existing collector with the same ID on br-int.
	existing, brIntUUID, err := getCollectorOnBrInt(d.ovsdbClient, collectorID)
	if err != nil {
		return err
	} else if existing != nil {
		// A collector with this ID already exists on br-int. If it belongs to the same owner
		// with the same group, treat this as an idempotent re-registration (e.g. the owning
		// process restarted without cleaning up) and reuse it instead of creating a
		// duplicate row. Otherwise it is a genuine conflict with a different consumer,
		// or the same owner requesting a different group, which we reject.
		return validateCollectorReuse(existing, groupID, ownerName)
	}

	ops, err := d.ovsdbClient.Create(&ovsdb.FlowSampleCollectorSet{
		ID:           collectorID,
		Bridge:       brIntUUID,
		LocalGroupID: &groupID,
		ExternalIDs:  map[string]string{"owner": ownerName},
	})
	if err != nil {
		return fmt.Errorf("failed creating collector: %w", err)
	}
	_, err = d.ovsdbClient.Transact(context.Background(), ops...)
	return err
}

func (d *SampleDecoder) DeleteCollector(collectorID int) error {
	existing, _, err := getCollectorOnBrInt(d.ovsdbClient, collectorID)
	if err != nil {
		return err
	} else if existing == nil {
		// Nothing to delete
		return nil
	}

	ops, err := d.ovsdbClient.Where(existing).Delete()
	if err != nil {
		return fmt.Errorf("failed deleting collector: %w", err)
	}
	_, err = d.ovsdbClient.Transact(context.Background(), ops...)
	return err
}

func networkNameToUDNNamespacedName(networkName string) string {
	namespace, name := util.ParseNetworkName(networkName)
	if name == "" {
		return ""
	}
	namespacedName := name
	if namespace != "" {
		namespacedName = namespace + "/" + name
	}
	return namespacedName
}

// GetInterfaceUDNs returns a map of all pod interface names to their corresponding (C)UDN namespaced names.
// default network or NAD that is not created by (C)UDN is represented by an empty string.
// UDN namespace+name are joined by "/", CUDN will just have a name.
func (d *SampleDecoder) GetInterfaceUDNs() (map[string]string, error) {
	res := map[string]string{}
	ifaces := []*ovsdb.Interface{}
	err := d.ovsdbClient.List(context.Background(), &ifaces)
	if err != nil {
		return nil, fmt.Errorf("failed listing interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.ExternalIDs["iface-id-ver"] == "" || iface.ExternalIDs["iface-id"] == "" {
			// not a pod interface
			continue
		}
		if iface.ExternalIDs["k8s.ovn.org/network"] == "" {
			res[iface.Name] = ""
			continue
		}
		res[iface.Name] = networkNameToUDNNamespacedName(iface.ExternalIDs["k8s.ovn.org/network"])
	}
	return res, nil
}
