package observability

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

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
const DefaultObservabilityCollectorSetID = 42

// this is inferred from nbdb schema, check Sample_Collector.id
const maxCollectorID = 255
const collectorFeaturesExternalID = "sample-features"

// collectorConfig holds the configuration for a collector.
// It is allowed to set different probabilities for every feature.
// collectorSetID is used to set up sampling via OVSDB.
type collectorConfig struct {
	collectorSetID int
	// probability in percent, 0 to 100
	featuresProbability map[libovsdbops.SampleFeature]int
}

type Manager struct {
	nbClient       libovsdbclient.Client
	sampConfig     *libovsdbops.SamplingConfig
	collectorsLock sync.Mutex
	// nbdb Collectors have probability. To allow different probabilities for different features,
	// multiple nbdb Collectors will be created, one per probability.
	// getCollectorKey() => collector.UUID
	dbCollectors map[string]string
	// cleaning up unused collectors may take time and multiple retries, as all referencing samples must be removed first.
	// Therefore, we need to save state between those retries.
	// getCollectorKey() => collector.SetID
	unusedCollectors              map[string]int
	unusedCollectorsRetryInterval time.Duration
	collectorsCleanupRetries      int
	// Only maxCollectorID collectors are allowed, each should have unique ID.
	// this set is tracking already assigned IDs.
	takenCollectorIDs sets.Set[int]
}

func NewManager(nbClient libovsdbclient.Client) *Manager {
	return &Manager{
		nbClient:                      nbClient,
		collectorsLock:                sync.Mutex{},
		dbCollectors:                  make(map[string]string),
		unusedCollectors:              make(map[string]int),
		unusedCollectorsRetryInterval: time.Minute,
		takenCollectorIDs:             sets.New[int](),
	}
}

func (m *Manager) SamplingConfig() *libovsdbops.SamplingConfig {
	return m.sampConfig
}

func (m *Manager) Init() error {
	// this will be read from the kube-api in the future
	currentConfig := &collectorConfig{
		collectorSetID: DefaultObservabilityCollectorSetID,
		featuresProbability: map[libovsdbops.SampleFeature]int{
			libovsdbops.EgressFirewallSample:     100,
			libovsdbops.NetworkPolicySample:      100,
			libovsdbops.AdminNetworkPolicySample: 100,
			libovsdbops.MulticastSample:          100,
			libovsdbops.UDNIsolationSample:       100,
		},
	}

	return m.initWithConfig(currentConfig)
}

func (m *Manager) initWithConfig(config *collectorConfig) error {
	if err := m.setSamplingAppIDs(); err != nil {
		return err
	}
	if err := m.setDbCollectors(); err != nil {
		return err
	}

	featuresConfig, err := m.addCollector(config)
	if err != nil {
		return err
	}
	m.sampConfig = libovsdbops.NewSamplingConfig(featuresConfig)

	// now cleanup stale collectors
	m.deleteStaleCollectorsWithRetry()
	return nil
}

func (m *Manager) setDbCollectors() error {
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	clear(m.dbCollectors)
	collectors, err := libovsdbops.ListSampleCollectors(m.nbClient)
	if err != nil {
		return fmt.Errorf("error getting sample collectors: %w", err)
	}
	for _, collector := range collectors {
		collectorKey := getCollectorKey(collector.SetID, collector.Probability)
		m.dbCollectors[collectorKey] = collector.UUID
		m.takenCollectorIDs.Insert(collector.ID)
		// all collectors are unused, until we update existing configs
		m.unusedCollectors[collectorKey] = collector.ID
	}
	return nil
}

// Stale collectors can't be deleted until all referencing Samples are deleted.
// Samples will be deleted asynchronously by different controllers on their init with the new Manager.
// deleteStaleCollectorsWithRetry will retry, considering deletion should eventually succeed when all controllers
// update their db entries to use the latest observability config.
func (m *Manager) deleteStaleCollectorsWithRetry() {
	if err := m.deleteStaleCollectors(); err != nil {
		m.collectorsCleanupRetries += 1
		// allow retries for 1 hour, hopefully it will be enough for all handler to complete initial sync
		if m.collectorsCleanupRetries > 60 {
			m.collectorsCleanupRetries = 0
			klog.Errorf("Cleanup stale collectors failed after 30 retries: %v", err)
			return
		}
		time.AfterFunc(m.unusedCollectorsRetryInterval, m.deleteStaleCollectorsWithRetry)
		return
	}
	m.collectorsCleanupRetries = 0
	klog.Infof("Cleanup stale collectors succeeded.")
}

func (m *Manager) deleteStaleCollectors() error {
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	var lastErr error
	for collectorKey, collectorSetID := range m.unusedCollectors {
		collectorUUID := m.dbCollectors[collectorKey]
		err := libovsdbops.DeleteSampleCollector(m.nbClient, &nbdb.SampleCollector{
			UUID: collectorUUID,
		})
		if err != nil {
			lastErr = err
			klog.Infof("Error deleting collector with ID=%d: %v", collectorSetID, lastErr)
			continue
		}
		delete(m.unusedCollectors, collectorKey)
		delete(m.dbCollectors, collectorKey)
		delete(m.takenCollectorIDs, collectorSetID)
	}
	return lastErr
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
	m.collectorsLock.Lock()
	defer m.collectorsLock.Unlock()
	sampleFeaturesConfig := make(map[libovsdbops.SampleFeature][]string)
	probabilityConfig := groupByProbability(conf)

	for probability, features := range probabilityConfig {
		collectorKey := getCollectorKey(conf.collectorSetID, probability)
		var collectorUUID string
		var ok bool
		// ensure predictable externalID
		slices.Sort(features)
		collectorFeatures := strings.Join(features, ",")
		if collectorUUID, ok = m.dbCollectors[collectorKey]; !ok {
			collectorID, err := m.getFreeCollectorID()
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collector := &nbdb.SampleCollector{
				ID:          collectorID,
				SetID:       conf.collectorSetID,
				Probability: probability,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			err = libovsdbops.CreateOrUpdateSampleCollector(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			collectorUUID = collector.UUID
			m.dbCollectors[collectorKey] = collectorUUID
			m.takenCollectorIDs.Insert(collectorID)
		} else {
			// update collector's features
			collector := &nbdb.SampleCollector{
				UUID: collectorUUID,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: collectorFeatures,
				},
			}
			err := libovsdbops.UpdateSampleCollectorExternalIDs(m.nbClient, collector)
			if err != nil {
				return sampleFeaturesConfig, err
			}
			// collector is used, remove from unused Collectors
			delete(m.unusedCollectors, collectorKey)
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
