package sampledecoder

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ovn-org/libovsdb/client"
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

type nbConfig struct {
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

func getLocalNBClient(ctx context.Context) (client.Client, error) {
	config := nbConfig{
		address: "unix:/var/run/ovn/ovnnb_db.sock",
		scheme:  "unix",
	}
	libovsdbOvnNBClient, err1 := NewNBClientWithConfig(ctx, config)
	if err1 == nil {
		return libovsdbOvnNBClient, nil
	}
	config = nbConfig{
		address: "unix:/var/run/ovn-ic/ovnnb_db.sock",
		scheme:  "unix",
	}
	var err2 error
	libovsdbOvnNBClient, err2 = NewNBClientWithConfig(ctx, config)
	if err2 != nil {
		return nil, fmt.Errorf("error creating libovsdb client: [ with /var/run/ovn/ovnnb_db.sock: %w, with /var/run/ovn-ic/ovnnb_db.sock: %w ]", err1, err2)
	}
	return libovsdbOvnNBClient, nil
}

func getLocalOVSDBClient(ctx context.Context) (client.Client, error) {
	config := nbConfig{
		address: "unix:/var/run/openvswitch/db.sock",
		scheme:  "unix",
	}
	return NewOVSDBClientWithConfig(ctx, config)
}

// NewSampleDecoderWithDefaultCollector creates a new SampleDecoder, initializes the OVSDB client and adds the default collector.
// It allows to set the groupID and ownerName for the created default collector.
// If the default collector already exists with a different owner or different groupID an error will be returned.
// Shutdown should be called to clean up the collector from the OVSDB.
func NewSampleDecoderWithDefaultCollector(ctx context.Context, ownerName string, groupID int) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx)
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
func NewSampleDecoder(ctx context.Context) (*SampleDecoder, error) {
	nbClient, err := getLocalNBClient(ctx)
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

func (d *SampleDecoder) DecodeCookieIDs(obsDomainID, obsPointID uint32) (string, error) {
	// Find sample using obsPointID
	sample, err := libovsdbops.FindSample(d.nbClient, int(obsPointID))
	if err != nil || sample == nil {
		return "", fmt.Errorf("find sample failed: %w", err)
	}
	// find db object using observ application ID
	var dbObj interface{}
	switch getObservAppID(obsDomainID) {
	case observability.ACLNewTrafficSamplingID:
		acls, err := libovsdbops.FindACLsWithPredicate(d.nbClient, func(acl *nbdb.ACL) bool {
			return acl.SampleNew != nil && *acl.SampleNew == sample.UUID
		})
		if err != nil {
			return "", fmt.Errorf("find acl for sample failed: %w", err)
		}
		if len(acls) != 1 {
			return "", fmt.Errorf("expected 1 ACL, got %d", len(acls))
		}
		dbObj = acls[0]
	case observability.ACLEstTrafficSamplingID:
		acls, err := libovsdbops.FindACLsWithPredicate(d.nbClient, func(acl *nbdb.ACL) bool {
			return acl.SampleEst != nil && *acl.SampleEst == sample.UUID
		})
		if err != nil {
			return "", fmt.Errorf("find acl for sample failed: %w", err)
		}
		if len(acls) != 1 {
			return "", fmt.Errorf("expected 1 ACL, got %d", len(acls))
		}
		dbObj = acls[0]
	default:
		return "", fmt.Errorf("unknown app ID: %d", getObservAppID(obsDomainID))
	}
	msg := getMessage(dbObj)
	if msg == "" {
		return "", fmt.Errorf("failed to get message for db object %v", dbObj)
	}
	return msg, nil
}

func getMessage(dbObj interface{}) string {
	switch o := dbObj.(type) {
	case *nbdb.ACL:
		action := "Allowed"
		if o.Action == "drop" {
			action = "Dropped"
		}
		actor := o.ExternalIDs[libovsdbops.OwnerTypeKey.String()]
		var msg string
		switch actor {
		case libovsdbops.AdminNetworkPolicyOwnerType:
			msg = fmt.Sprintf("admin network policy %s, direction %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()], o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.BaselineAdminNetworkPolicyOwnerType:
			msg = fmt.Sprintf("baseline admin network policy %s, direction %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()], o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.MulticastNamespaceOwnerType:
			msg = fmt.Sprintf("multicast in namespace %s, direction %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()], o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.MulticastClusterOwnerType:
			msg = fmt.Sprintf("cluster multicast policy, direction %s", o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.NetpolNodeOwnerType:
			msg = "default allow from local node policy, direction ingress"
		case libovsdbops.NetworkPolicyOwnerType:
			msg = fmt.Sprintf("network policy %s, direction %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()], o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.NetpolNamespaceOwnerType:
			msg = fmt.Sprintf("network policies isolation in namespace %s, direction %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()], o.ExternalIDs[libovsdbops.PolicyDirectionKey.String()])
		case libovsdbops.EgressFirewallOwnerType:
			msg = fmt.Sprintf("egress firewall in namespace %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()])
		case libovsdbops.UDNIsolationOwnerType:
			msg = fmt.Sprintf("UDN isolation of type %s", o.ExternalIDs[libovsdbops.ObjectNameKey.String()])
		}
		return fmt.Sprintf("%s by %s", action, msg)
	default:
		return ""
	}
}

func (d *SampleDecoder) DecodeCookieBytes(cookie []byte) (string, error) {
	if uint64(len(cookie)) != CookieSize {
		return "", fmt.Errorf("invalid cookie size: %d", len(cookie))
	}
	c := Cookie{}
	err := binary.Read(bytes.NewReader(cookie), SampleEndian, &c)
	if err != nil {
		return "", err
	}
	return d.DecodeCookieIDs(c.ObsDomainID, c.ObsPointID)
}

func (d *SampleDecoder) DecodeCookie8Bytes(cookie [8]byte) (string, error) {
	c := Cookie{}
	err := binary.Read(bytes.NewReader(cookie[:]), SampleEndian, &c)
	if err != nil {
		return "", err
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
