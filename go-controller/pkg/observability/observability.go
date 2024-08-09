package observability

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// OVN observ app IDs. Make sure to always add new apps in the end.
const (
	DropSamplingID = iota + 1
	ACLNewTrafficSamplingID
	ACLEstTrafficSamplingID
)

// temporary const, until we have dynamic config
const DefaultObservabilityCollectorSetID = 28

// this is inferred from nbdb schema, check Sample_Collector.id
const maxCollectorID = 255

// collectorConfig holds the configuration for a collector.
// It is allowed to set different probabilities for every feature.
// collectorSetID is used to set up sampling via OVSDB.
type collectorConfig struct {
	collectorSetID int
	// probability in percent, 0 to 100
	featuresProbability map[libovsdbops.SampleFeature]int
}

type Manager struct {
	nbClient   libovsdbclient.Client
	sampConfig *libovsdbops.SamplingConfig
	// nbdb Collectors have probability. To allow different probabilities for different features,
	// multiple nbdb Collectors will be created, one per probability.
	// getCollectorKey() => collector.UUID
	dbCollectors map[string]string
	// Only maxCollectorID collectors are allowed, each should have unique ID.
	// this set is tracking already assigned IDs.
	takenCollectorIDs sets.Set[int]
}

func NewManager(nbClient libovsdbclient.Client) *Manager {
	return &Manager{
		nbClient:          nbClient,
		dbCollectors:      make(map[string]string),
		takenCollectorIDs: sets.New[int](),
	}
}

func (m *Manager) SamplingConfig() *libovsdbops.SamplingConfig {
	return m.sampConfig
}

func (m *Manager) Init() error {
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	if err := m.setDbCollectors(); err != nil {
		return err
	}
	featuresConfig, err := m.addCollector(&collectorConfig{
		collectorSetID: DefaultObservabilityCollectorSetID,
		featuresProbability: map[libovsdbops.SampleFeature]int{
			libovsdbops.EgressFirewall:     100,
			libovsdbops.NetworkPolicy:      100,
			libovsdbops.AdminNetworkPolicy: 100,
			libovsdbops.Multicast:          100,
			libovsdbops.UDNIsolation:       100,
		},
	})
	if err != nil {
		return err
	}
	m.sampConfig = libovsdbops.NewSamplingConfig(featuresConfig)
	return nil
}

func (m *Manager) setDbCollectors() error {
	clear(m.dbCollectors)
	collectors, err := libovsdbops.FindSampleCollectorWithPredicate(m.nbClient, func(collector *nbdb.SampleCollector) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error getting sample collectors: %w", err)
	}
	for _, collector := range collectors {
		m.dbCollectors[getCollectorKey(collector.SetID, collector.Probability)] = collector.UUID
		m.takenCollectorIDs.Insert(collector.SetID)
	}
	return nil
}

// Cleanup must be called when observability is no longer needed.
// It will return an error if some samples still exist in the db.
// This is expected, and Cleanup may be retried on the next restart.
func Cleanup(nbClient libovsdbclient.Client) error {
	// Do the opposite of init
	err := libovsdbops.DeleteSamplingAppsWithPredicate(nbClient, func(app *nbdb.SamplingApp) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sampling apps: %w", err)
	}

	err = libovsdbops.DeleteSampleCollectorWithPredicate(nbClient, func(collector *nbdb.SampleCollector) bool {
		return true
	})
	if err != nil {
		return fmt.Errorf("error deleting sample collectors: %w", err)
	}
	return nil
}

func (m *Manager) setSamplingAppIDs() error {
	var ops []libovsdb.Operation
	var err error
	for _, appConfig := range []struct {
		id      int
		appType nbdb.SamplingAppType
	}{
		{
			id:      DropSamplingID,
			appType: nbdb.SamplingAppTypeDrop,
		},
		{
			id:      ACLNewTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLNew,
		},
		{
			id:      ACLEstTrafficSamplingID,
			appType: nbdb.SamplingAppTypeACLEst,
		},
	} {
		samplingApp := &nbdb.SamplingApp{
			ID:   appConfig.id,
			Type: appConfig.appType,
		}
		ops, err = libovsdbops.CreateOrUpdateSamplingAppsOps(m.nbClient, ops, samplingApp)
		if err != nil {
			return fmt.Errorf("error creating or updating sampling app %s: %w", appConfig.appType, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(m.nbClient, ops)
	return err
}

func groupByProbability(c *collectorConfig) map[int][]libovsdbops.SampleFeature {
	probabilities := make(map[int][]libovsdbops.SampleFeature)
	for feature, percentProbability := range c.featuresProbability {
		probability := percentToProbability(percentProbability)
		probabilities[probability] = append(probabilities[probability], feature)
	}
	return probabilities
}

func getCollectorKey(collectorID int, probability int) string {
	return fmt.Sprintf("%d-%d", collectorID, probability)
}

func (m *Manager) getFreeCollectorID() (int, error) {
	for i := 1; i <= maxCollectorID; i++ {
		if !m.takenCollectorIDs.Has(i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free collector IDs")
}

func (m *Manager) addCollector(conf *collectorConfig) (map[libovsdbops.SampleFeature][]string, error) {
	sampleFeaturesConfig := make(map[libovsdbops.SampleFeature][]string)
	probabilityConfig := groupByProbability(conf)

	for probability, features := range probabilityConfig {
		collectorKey := getCollectorKey(conf.collectorSetID, probability)
		var collectorUUID string
		var ok bool
		if collectorUUID, ok = m.dbCollectors[collectorKey]; !ok {
			collectorID, err := m.getFreeCollectorID()
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collector := &nbdb.SampleCollector{
				ID:          collectorID,
				SetID:       conf.collectorSetID,
				Probability: probability,
			}
			err = libovsdbops.CreateOrUpdateSampleCollector(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collectorUUID = collector.UUID
			m.dbCollectors[collectorKey] = collectorUUID
			m.takenCollectorIDs.Insert(collectorID)
		}
		for _, feature := range features {
			sampleFeaturesConfig[feature] = append(sampleFeaturesConfig[feature], collectorUUID)
		}
	}
	return sampleFeaturesConfig, nil
}

func percentToProbability(percent int) int {
	return 65535 * percent / 100
}
