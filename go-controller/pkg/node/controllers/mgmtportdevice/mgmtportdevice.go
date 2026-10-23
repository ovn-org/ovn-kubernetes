// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package mgmtportdevice

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ManagementPort is a management port that has to be re-plumbed when the
// device published for it changes.
type ManagementPort interface {
	Reconcile() error
}

// Controller watches a node for changes to the devices published for its
// management ports. On a DPU the management port representor is derived from
// the device the host reserved, and the host can move it while the port is
// plumbed, so the representor has to follow it.
type Controller struct {
	name       string
	nodeName   string
	network    string
	mgmtPort   ManagementPort
	controller controller.Controller
}

// NewController creates a controller keeping the management port of network on
// nodeName aligned with the device published for it.
func NewController(nodeName, network string, nodeInformer coreinformers.NodeInformer, mgmtPort ManagementPort) *Controller {
	c := &Controller{
		name:     fmt.Sprintf("%s-mgmt-port-device-controller", network),
		nodeName: nodeName,
		network:  network,
		mgmtPort: mgmtPort,
	}

	config := &controller.ControllerConfig[corev1.Node]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		MaxAttempts: controller.InfiniteAttempts,
		Informer:    nodeInformer.Informer(),
		Lister:      nodeInformer.Lister().List,
		Reconcile: func(_ context.Context, key string) error {
			return c.reconcile(key)
		},
		ObjNeedsUpdate: c.needsUpdate,
		// Reconcile re-plumbs the ports of one management port and is not safe
		// to run concurrently with itself.
		Threadiness: 1,
	}

	c.controller = controller.NewController[corev1.Node](c.name, config)
	return c
}

// Start begins watching. The owner must call Stop before tearing the
// management port down, otherwise a queued update can re-plumb it afterwards.
func (c *Controller) Start() error {
	if c == nil {
		return nil
	}
	if err := controller.Start(c.controller); err != nil {
		return fmt.Errorf("failed to start %s controller: %w", c.name, err)
	}
	return nil
}

// Stop ends the device watch before the owner tears down the management port.
func (c *Controller) Stop() {
	// c is nil when the owner did not need a device watch, and Stop is called
	// unconditionally on teardown.
	if c == nil {
		return
	}
	controller.Stop(c.controller)
}

func (c *Controller) reconcile(string) error {
	return c.mgmtPort.Reconcile()
}

// needsUpdate selects this node, and on update only a change of the device
// published for this network. The initial add is always taken because a device
// may have moved before this controller started.
func (c *Controller) needsUpdate(oldNode, newNode *corev1.Node) bool {
	if newNode == nil || newNode.Name != c.nodeName {
		return false
	}
	if oldNode == nil {
		return true
	}
	// reject the unrelated node updates before parsing
	if oldNode.Annotations[util.OvnNodeManagementPort] == newNode.Annotations[util.OvnNodeManagementPort] {
		return false
	}
	// the annotation carries every network, so an unrelated network being
	// allocated or released must not wake this one. Parse failures leave a nil
	// map, which reads as nothing published and is reported where consumed.
	oldDevices, _ := util.ParseNodeManagementPortAnnotation(oldNode)
	newDevices, _ := util.ParseNodeManagementPortAnnotation(newNode)
	return !reflect.DeepEqual(oldDevices[c.network], newDevices[c.network])
}
