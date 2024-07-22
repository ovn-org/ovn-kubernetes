package openflowmanager

import (
	"reflect"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// openflow manager per bridge
type ofmanager struct {
	bridge string
	// flow cache, use map instead of array for readability when debugging
	flowCache map[string][]string
	flowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan chan struct{}
}

type Controller struct {
	ofmanagers *syncmap.SyncMap[*ofmanager]
	stopChan   <-chan struct{}
}

// NewController creates a new openflow manager
func NewController(stopChan <-chan struct{}) *Controller {
	return &Controller{
		ofmanagers: syncmap.NewSyncMap[*ofmanager](),
		stopChan:   stopChan,
	}
}

// UpdateFlowCacheEntry update flow cache of the given bridge
func (oc *Controller) UpdateFlowCacheEntry(bridgeName, key string, flows []string) {
	_ = oc.ofmanagers.DoWithLock(bridgeName, func(bridge string) error {
		c, found := oc.ofmanagers.LoadOrStore(bridge, &ofmanager{
			bridge:    bridge,
			flowCache: make(map[string][]string),
			flowMutex: sync.Mutex{},
			flowChan:  make(chan struct{}, 1),
		})
		c.flowMutex.Lock()
		defer c.flowMutex.Unlock()
		c.flowCache[key] = flows
		// oc.stopChan is nil in unit testing, so it does not start syncFlow go-routine for unit tests.
		if !found && oc.stopChan != nil {
			c.run(oc.stopChan)
		}
		return nil
	})
}

// DeleteFlowsByKey delete flow of the specific key from the flow cache of the given bridge
func (oc *Controller) DeleteFlowsByKey(bridgeName, key string) {
	_ = oc.ofmanagers.DoWithLock(bridgeName, func(bridge string) error {
		c, found := oc.ofmanagers.Load(bridge)
		if found {
			c.flowMutex.Lock()
			delete(c.flowCache, key)
			c.flowMutex.Unlock()
		}
		return nil
	})
}

// RequestFlowSync request to sync flow config of the specific bridge
func (oc *Controller) RequestFlowSync(bridge string) {
	_ = oc.ofmanagers.DoWithLock(bridge, func(bridgeName string) error {
		c, found := oc.ofmanagers.Load(bridgeName)
		if found {
			select {
			case c.flowChan <- struct{}{}:
				klog.V(5).Infof("OpenFlow sync on bridge %s requested", bridgeName)
			default:
				klog.V(5).Infof("OpenFlow sync on bridge %s already requested", bridgeName)
			}
		}
		return nil
	})
}

func (ofm *ofmanager) syncFlows() {
	ofm.flowMutex.Lock()
	defer ofm.flowMutex.Unlock()

	flows := []string{}
	for _, entry := range ofm.flowCache {
		flows = append(flows, entry...)
	}

	_, stderr, err := util.ReplaceOFFlows(ofm.bridge, flows)
	if err != nil {
		klog.Errorf("Failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, ofm.flowCache)
	}
}

// Run starts OpenFlow Manager which will constantly sync flows for managed OVS bridges
func (ofm *ofmanager) run(stopChan <-chan struct{}) {
	go func() {
		syncPeriod := 15 * time.Second
		timer := time.NewTicker(syncPeriod)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				ofm.syncFlows()
			case <-ofm.flowChan:
				ofm.syncFlows()
				timer.Reset(syncPeriod)
			case <-stopChan:
				return
			}
		}
	}()
}

// CheckFlowEntryMatch checks the flows of the specific key for specifiy bridge is as expected
// Called by unit tests only.
func (oc *Controller) CheckFlowEntryMatch(bridgeName, key string, flows []string) bool {
	var match bool
	_ = oc.ofmanagers.DoWithLock(bridgeName, func(bridge string) error {
		c, found := oc.ofmanagers.Load(bridge)
		if found {
			match = reflect.DeepEqual(c.flowCache[key], flows)
		} else {
			match = (flows == nil)
		}
		return nil
	})
	return match
}

// SyncFlows request the flow manager to sync the flows configuration of the specific bridge
// Called by unit test only
func (oc *Controller) SyncFlows(bridge string) {
	_ = oc.ofmanagers.DoWithLock(bridge, func(bridgeName string) error {
		c, found := oc.ofmanagers.Load(bridgeName)
		if found {
			c.syncFlows()
		}
		return nil
	})
}
