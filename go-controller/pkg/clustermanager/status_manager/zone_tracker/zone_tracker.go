package zone_tracker

import (
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// UnknownZone is used to distinguish nodes that don't have zone label.
	// If a node doesn't get zone label for 30 Seconds, UnknownZone will be removed from the zones set.
	// This label is needed to allow nodes some time to get/restore their zone label without all their
	// status messages being removed.
	UnknownZone        = "unknown"
	unknownZoneTimeout = 30 * time.Second
)

// ZoneTracker collects information about existing zones based on nodes annotations.
// Subscribe method may be used to subscribe to zones updates.
type ZoneTracker struct {
	// zonesLock is used to protect internal containers nodeToZone, zones, and unknownZoneNodes.
	// Also subscriber info inited and onZonesUpdate
	zonesLock sync.RWMutex
	// nodeToZone stores node to zone mapping
	nodeToZone map[string]string
	// zones is a set of zones from nodeToZone
	zones sets.Set[string]
	// unknownZoneNodes is used to track nodes which have an UnknownZone, and a timestamp when they were created.
	// If these nodes don't change a zone within an unknownZoneTimeout, we don't expect them to change zone anymore,
	// and we don't want subscribers to be confused about UnknownZone because of that node, so we remove UnknownZone from
	// the zones set, but leave it in the unknownZoneNodes.
	// That is useful to avoid triggering any updates if the node stays in the UnknownZone
	unknownZoneNodes   map[string]time.Time
	unknownZoneTimeout time.Duration

	// onZonesUpdate will be called on every zone change
	onZonesUpdate func(newZones sets.Set[string])

	// stopChan is used internally to shut down UnknownZone trackers when Stop() is called
	stopChan       chan struct{}
	nodeLister     corelisters.NodeLister
	nodeController controller.Controller
}

func NewZoneTracker(nodeInformer coreinformers.NodeInformer, onZonesUpdate func(newZones sets.Set[string])) *ZoneTracker {
	zt := &ZoneTracker{
		zonesLock:          sync.RWMutex{},
		nodeToZone:         map[string]string{},
		zones:              sets.New[string](),
		unknownZoneNodes:   map[string]time.Time{},
		unknownZoneTimeout: unknownZoneTimeout,
		onZonesUpdate:      onZonesUpdate,

		stopChan:   make(chan struct{}),
		nodeLister: nodeInformer.Lister(),
	}

	controllerConfig := &controller.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		ObjNeedsUpdate: zt.needsUpdate,
		Reconcile:      zt.reconcileNode,
		Threadiness:    1,
	}
	zt.nodeController = controller.NewController[corev1.Node]("zone_tracker", controllerConfig)
	return zt
}

func (zt *ZoneTracker) Start() error {
	if err := controller.StartWithInitialSync(zt.initialSync, zt.nodeController); err != nil {
		return fmt.Errorf("failed to start zone tracker: %w", err)
	}
	return nil
}

func (zt *ZoneTracker) Stop() {
	close(zt.stopChan)
	controller.Stop(zt.nodeController)
}

func (zt *ZoneTracker) needsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	return util.NodeZoneAnnotationChanged(oldNode, newNode)
}

func (zt *ZoneTracker) initialSync() error {
	nodes, err := zt.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list objects: %w", err)
	}
	for _, node := range nodes {
		if err := zt.reconcileNode(node.Name); err != nil {
			return err
		}
	}
	zt.zonesLock.Lock()
	defer zt.zonesLock.Unlock()
	zt.notifySubscriber()
	return nil
}

func (zt *ZoneTracker) checkUnknownNodeTimeout(nodeName string) {
	zt.zonesLock.Lock()
	defer zt.zonesLock.Unlock()
	timestamp, ok := zt.unknownZoneNodes[nodeName]
	if !ok {
		return
	}
	// if node still doesn't have a zone after a timeout, delete
	// allow some extra time to cover potential timer inaccuracy
	skewDelta := 100 * time.Millisecond
	if zt.nodeToZone[nodeName] == UnknownZone && time.Since(timestamp) >= zt.unknownZoneTimeout-skewDelta {
		klog.Warningf("Node %s has UnknownZone for more than %v", nodeName, zt.unknownZoneTimeout)
		if zt.deleteNodeZoneNoLock(nodeName, true) {
			zt.notifySubscriber()
		}
	}
}

func (zt *ZoneTracker) reconcileNode(nodeName string) error {
	node, err := zt.nodeLister.Get(nodeName)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if node == nil {
		// node was deleted
		zt.deleteNode(nodeName)
		return nil
	}

	zoneName, ok := node.Annotations[util.OvnNodeZoneName]
	if !ok {
		zoneName = UnknownZone
	}
	zt.zonesLock.Lock()
	defer zt.zonesLock.Unlock()

	if zt.nodeToZone[nodeName] == zoneName {
		return nil
	}

	if _, ok := zt.unknownZoneNodes[nodeName]; zoneName == UnknownZone && ok {
		return nil
	}

	zoneDeleted := zt.deleteNodeZoneNoLock(nodeName, false)
	// notify subscriber if the zone was deleted or added.
	// zone is added if it wasn't in the zones set before
	notifySubscriber := zoneDeleted || !zt.zones.Has(zoneName)

	zt.nodeToZone[nodeName] = zoneName
	zt.zones.Insert(zoneName)

	if zoneName == UnknownZone {
		zt.unknownZoneNodes[nodeName] = time.Now()
		// wait for unknownZoneTimeout to allow node to get a zone label.
		// if that doesn't happen, consider a node is not a part of any zone and remove
		go func() {
			select {
			case <-zt.stopChan:
				return
			case <-time.After(zt.unknownZoneTimeout):
				zt.checkUnknownNodeTimeout(nodeName)
			}
		}()
	}

	if notifySubscriber {
		zt.notifySubscriber()
	}
	return nil
}

func (zt *ZoneTracker) deleteNode(nodeName string) {
	zt.zonesLock.Lock()
	defer zt.zonesLock.Unlock()

	if zt.deleteNodeZoneNoLock(nodeName, false) {
		zt.notifySubscriber()
	}
}

// deleteNodeZoneNoLock deletes a node and its zone if there are no nodes referencing it without acquiring a lock.
// return true if a zone was deleted, must be called with zonesLock
func (zt *ZoneTracker) deleteNodeZoneNoLock(nodeName string, leaveUnknownZoneTimestamp bool) bool {
	if !leaveUnknownZoneTimestamp {
		// in case node is in the UnknownZone, delete
		delete(zt.unknownZoneNodes, nodeName)
	}

	oldZone, ok := zt.nodeToZone[nodeName]
	if !ok {
		// node doesn't exist in the map
		return false
	}
	delete(zt.nodeToZone, nodeName)
	deleteZone := true
	for _, existingZone := range zt.nodeToZone {
		if existingZone == oldZone {
			// at least one node in the same zone exists, don't delete
			deleteZone = false
			break
		}
	}

	if !deleteZone {
		return false
	}

	zt.zones.Delete(oldZone)
	return true
}

// notifySubscriber must be called with zonesLock
func (zt *ZoneTracker) notifySubscriber() {
	zt.onZonesUpdate(sets.New[string](zt.zones.UnsortedList()...))
}
