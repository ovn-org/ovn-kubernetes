// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zone_tracker

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ZoneTracker collects the existing single-node zones. In the supported
// topology each OVN-managed node is its own zone and the zone ID is the node
// name. Nodes configured to manage their own networking do not have an OVN
// zone and are excluded.
type ZoneTracker struct {
	// zonesLock protects zones and onZonesUpdate notifications.
	zonesLock sync.RWMutex
	// zones contains the names of OVN-managed nodes.
	zones sets.Set[string]

	// onZonesUpdate will be called on every zone change
	onZonesUpdate func(newZones sets.Set[string])

	nodeLister     corelisters.NodeLister
	nodeController controller.Controller
}

func NewZoneTracker(nodeInformer coreinformers.NodeInformer, onZonesUpdate func(newZones sets.Set[string])) *ZoneTracker {
	zt := &ZoneTracker{
		zonesLock:     sync.RWMutex{},
		zones:         sets.New[string](),
		onZonesUpdate: onZonesUpdate,
		nodeLister:    nodeInformer.Lister(),
	}

	controllerConfig := &controller.ControllerConfig[corev1.Node]{
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		ObjNeedsUpdate: zt.needsUpdate,
		Reconcile: func(_ context.Context, nodeName string) error {
			return zt.reconcileNode(nodeName)
		},
		Threadiness: 1,
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
	controller.Stop(zt.nodeController)
}

func (zt *ZoneTracker) needsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	return util.NoHostSubnet(oldNode) != util.NoHostSubnet(newNode)
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

func (zt *ZoneTracker) reconcileNode(nodeName string) error {
	node, err := zt.nodeLister.Get(nodeName)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	zt.zonesLock.Lock()
	defer zt.zonesLock.Unlock()

	isZoneNode := node != nil && !util.NoHostSubnet(node)
	isTracked := zt.zones.Has(nodeName)
	if isZoneNode == isTracked {
		return nil
	}

	if isZoneNode {
		zt.zones.Insert(nodeName)
	} else {
		zt.zones.Delete(nodeName)
	}
	zt.notifySubscriber()
	return nil
}

// notifySubscriber must be called with zonesLock
func (zt *ZoneTracker) notifySubscriber() {
	zt.onZonesUpdate(sets.New[string](zt.zones.UnsortedList()...))
}
