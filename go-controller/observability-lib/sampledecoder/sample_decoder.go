package sampledecoder

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/observability-lib/model"
	"github.com/ovn-org/ovn-kubernetes/go-controller/observability-lib/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
)

type SampleDecoder struct {
	nbClient          client.Client
	ovsdbClient       client.Client
	cleanupCollectors []int
}

type dbConfig struct {
	address string
	scheme  string
}

type Cookie struct {
	ObsDomainID uint32
	ObsPointID  uint32
}

const CookieSize = 8
const bridgeName = "br-int"

var SampleEndian = getEndian()

func getEndian() binary.ByteOrder {
	// Use network bite order
	return binary.BigEndian
}

// getLocalNBClient only supports connecting to nbdb via unix socket.
// address is the path to the unix socket, e.g. "/var/run/ovn/ovnnb_db.sock"
func getLocalNBClient(ctx context.Context, address string) (client.Client, error) {
	config := dbConfig{
		address: "unix:" + address,
		scheme:  "unix",
	}
	libovsdbOvnNBClient, err := NewNBClientWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("error creating libovsdb client: %w ", err)
	}
	return libovsdbOvnNBClient, nil
}

func getLocalOVSDBClient(ctx context.Context) (client.Client, error) {
	config := dbConfig{
		address: "unix:/var/run/openvswitch/db.sock",
		scheme:  "unix",
	}
	return NewOVSDBClientWithConfig(ctx, config)
}

// NewSampleDecoderWithDefaultCollector creates a new SampleDecoder, initializes the OVSDB client and adds the default collector.
// It allows to set the groupID and ownerName for the created default collector.
// If the default collector already exists with a different owner or different groupID an error will be returned.
// Shutdown should be called to clean up the collector from the OVSDB.
func NewSampleDecoderWithDefaultCollector(ctx context.Context, nbdbSocketPath string, ownerName string, groupID int) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx, nbdbSocketPath)
	if err != nil {
		return nil, err
	}
	ovsdbClient, err := getLocalOVSDBClient(ctx)
	if err != nil {
		return nil, err
	}
	decoder := &SampleDecoder{
		nbClient:    nbClient,
		ovsdbClient: ovsdbClient,
	}
	err = decoder.AddCollector(observability.DefaultObservabilityCollectorSetID, groupID, ownerName)
	if err != nil {
		return nil, err
	}
	decoder.cleanupCollectors = append(decoder.cleanupCollectors, observability.DefaultObservabilityCollectorSetID)
	return decoder, nil
}

// NewSampleDecoder creates a new SampleDecoder and initializes the OVSDB client.
func NewSampleDecoder(ctx context.Context, nbdbSocketPath string) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx, nbdbSocketPath)
	if err != nil {
		return nil, err
	}
	return &SampleDecoder{
		nbClient: nbClient,
	}, nil
}

func (d *SampleDecoder) Shutdown() {
	for _, collectorID := range d.cleanupCollectors {
		err := d.DeleteCollector(collectorID)
		if err != nil {
			fmt.Printf("Error deleting collector with ID=%d: %v", collectorID, err)
		}
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

func (d *SampleDecoder) AddCollector(collectorID, groupID int, ownerName string) error {
	if d.ovsdbClient == nil {
		return fmt.Errorf("OVSDB client is not initialized")
	}
	// find existing collector with the same ID
	collectors := []*ovsdb.FlowSampleCollectorSet{}
	err := d.ovsdbClient.WhereCache(func(item *ovsdb.FlowSampleCollectorSet) bool {
		return item.ID == collectorID
	}).List(context.Background(), &collectors)
	if err != nil {
		return fmt.Errorf("failed finding existing collector: %w", err)
	}
	if len(collectors) > 0 && (collectors[0].ExternalIDs["owner"] != ownerName ||
		collectors[0].LocalGroupID == nil || *collectors[0].LocalGroupID != groupID) {
		return fmt.Errorf("requested collector with id=%v already exists "+
			"with the external_ids=%+v, local_group_id=%v", collectorID, collectors[0].ExternalIDs["owner"], getGroupID(collectors[0].LocalGroupID))
	}

	// find br-int UUID to attach collector
	bridges := []*ovsdb.Bridge{}
	err = d.ovsdbClient.WhereCache(func(item *ovsdb.Bridge) bool {
		return item.Name == bridgeName
	}).List(context.Background(), &bridges)
	if err != nil || len(bridges) != 1 {
		return fmt.Errorf("failed finding br-int: %w", err)
	}

	ops, err := d.ovsdbClient.Create(&ovsdb.FlowSampleCollectorSet{
		ID:           collectorID,
		Bridge:       bridges[0].UUID,
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
	collectors := []*ovsdb.FlowSampleCollectorSet{}
	err := d.ovsdbClient.WhereCache(func(item *ovsdb.FlowSampleCollectorSet) bool {
		return item.ID == collectorID
	}).List(context.Background(), &collectors)
	if err != nil {
		return fmt.Errorf("failed finding exisiting collector: %w", err)
	}
	if len(collectors) != 1 {
		return fmt.Errorf("expected only 1 collector with given id")
	}

	ops, err := d.ovsdbClient.Where(collectors[0]).Delete()
	if err != nil {
		return fmt.Errorf("failed creating collector: %w", err)
	}
	res, err := d.ovsdbClient.Transact(context.Background(), ops...)
	fmt.Println("res: ", res)
	return err
}
