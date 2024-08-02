package ops

import (
	"golang.org/x/net/context"
	"hash/fnv"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func CreateOrUpdateSampleCollector(nbClient libovsdbclient.Client, collector *nbdb.SampleCollector) error {
	opModel := operationModel{
		Model:          collector,
		OnModelUpdates: onModelUpdatesAllNonDefault(),
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

func DeleteSampleCollectorWithPredicate(nbClient libovsdbclient.Client, p func(collector *nbdb.SampleCollector) bool) error {
	opModel := operationModel{
		Model:          &nbdb.SampleCollector{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}
	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

func FindSampleCollectorWithPredicate(nbClient libovsdbclient.Client, p func(*nbdb.SampleCollector) bool) ([]*nbdb.SampleCollector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	collectors := []*nbdb.SampleCollector{}
	err := nbClient.WhereCache(p).List(ctx, &collectors)
	return collectors, err
}

func CreateOrUpdateSamplingAppsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, samplingApps ...*nbdb.SamplingApp) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(samplingApps))
	for i := range samplingApps {
		// can't use i in the predicate, for loop replaces it in-memory
		samplingApp := samplingApps[i]
		opModel := operationModel{
			Model:          samplingApp,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

func DeleteSamplingAppsWithPredicate(nbClient libovsdbclient.Client, p func(collector *nbdb.SamplingApp) bool) error {
	opModel := operationModel{
		Model:          &nbdb.SamplingApp{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}
	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

func FindSample(nbClient libovsdbclient.Client, sampleMetadata int) (*nbdb.Sample, error) {
	sample := &nbdb.Sample{
		Metadata: sampleMetadata,
	}
	opModel := operationModel{
		Model:       sample,
		ErrNotFound: true,
		BulkOp:      false,
	}
	modelClient := newModelClient(nbClient)
	err := modelClient.Lookup(opModel)
	if err != nil {
		return nil, err
	}
	return sample, err
}

type SampleFeature int

const (
	EgressFirewall SampleFeature = iota
	NetworkPolicy
	AdminNetworkPolicy
	Multicast
	UDNIsolation
)

// SamplingConfig is used to configure sampling for different db objects.
type SamplingConfig struct {
	featureCollectors map[SampleFeature][]string
}

func NewSamplingConfig(featureCollectors map[SampleFeature][]string) *SamplingConfig {
	return &SamplingConfig{
		featureCollectors: featureCollectors,
	}
}

func addSample(c *SamplingConfig, opModels []operationModel, model model.Model) []operationModel {
	if c == nil {
		return opModels
	}
	switch t := model.(type) {
	case *nbdb.ACL:
		return createOrUpdateSampleForACL(opModels, c, t)
	}
	return opModels
}

// createOrUpdateSampleForACL should be called before acl operationModel is appended to opModels.
func createOrUpdateSampleForACL(opModels []operationModel, c *SamplingConfig, acl *nbdb.ACL) []operationModel {
	collectors := c.featureCollectors[getACLSampleFeature(acl)]
	if len(collectors) == 0 {
		return opModels
	}
	sample := &nbdb.Sample{
		Collectors: collectors,
		// 32 bits
		Metadata: int(GetSampleID(acl)),
	}
	opModel := operationModel{
		Model: sample,
		DoAfter: func() {
			acl.SampleEst = &sample.UUID
			acl.SampleNew = &sample.UUID
		},
		OnModelUpdates: []interface{}{&sample.Collectors},
		ErrNotFound:    false,
		BulkOp:         false,
	}
	opModels = append(opModels, opModel)
	return opModels
}

func GetSampleID(dbObj hasExternalIDs) uint32 {
	primaryID := dbObj.GetExternalIDs()[PrimaryIDKey.String()]
	h := fnv.New32a()
	h.Write([]byte(primaryID))
	return h.Sum32()
}

func getACLSampleFeature(acl *nbdb.ACL) SampleFeature {
	switch acl.ExternalIDs[OwnerTypeKey.String()] {
	case AdminNetworkPolicyOwnerType, BaselineAdminNetworkPolicyOwnerType:
		return AdminNetworkPolicy
	case MulticastNamespaceOwnerType, MulticastClusterOwnerType:
		return Multicast
	case NetpolNodeOwnerType, NetworkPolicyOwnerType, NetpolNamespaceOwnerType:
		return NetworkPolicy
	case EgressFirewallOwnerType:
		return EgressFirewall
	case UDNIsolationOwnerType:
		return UDNIsolation
	}
	return -1
}
