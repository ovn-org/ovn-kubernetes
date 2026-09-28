// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	uplinkStateControllerName = "uplink-state-controller"
)

// uplinkStateController bridges typed UplinkState changes back into the
// existing node/network reconciliation path. It does not discover or publish
// UplinkState; ovnkube-node owns that. This controller watches the published
// state and asks the node reconciler to re-run gateway reconciliation for CUDNs
// that use the changed uplink on the affected node.
type uplinkStateController struct {
	uplinkStateLister uplinklister.UplinkStateLister
	// uplinkByStateKey is owned by the single reconciliation worker. Protect it if Threadiness increases.
	uplinkByStateKey map[string]string

	networkManager     networkmanager.Interface
	nodeReconciler     *nodecontroller.NodeController
	routeImportManager routeimport.Manager
	nodeName           string

	uplinkStateController controllerutil.Controller
}

type gatewaySyncHandler interface {
	MarkGatewaySyncNeeded(nodeName string)
}

func newUplinkStateController(
	uplinkStateInformer uplinkinformer.UplinkStateInformer,
	networkManager networkmanager.Interface,
	nodeReconciler *nodecontroller.NodeController,
	routeImportManager routeimport.Manager,
	nodeName string,
) *uplinkStateController {
	c := &uplinkStateController{
		uplinkStateLister:  uplinkStateInformer.Lister(),
		uplinkByStateKey:   map[string]string{},
		networkManager:     networkManager,
		nodeReconciler:     nodeReconciler,
		routeImportManager: routeImportManager,
		nodeName:           nodeName,
	}

	uplinkStateConfig := &controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    uplinkStateInformer.Informer(),
		Lister:      uplinkStateInformer.Lister().List,
		Reconcile:   c.syncUplinkState,
		ObjNeedsUpdate: func(oldObj, newObj *uplinkv1alpha1.UplinkState) bool {
			return oldObj == nil || newObj == nil ||
				!reflect.DeepEqual(oldObj.Spec, newObj.Spec) ||
				!reflect.DeepEqual(oldObj.Status, newObj.Status)
		},
		Threadiness: 1,
		MaxAttempts: controllerutil.InfiniteAttempts,
	}
	c.uplinkStateController = controllerutil.NewController(
		uplinkStateControllerName,
		uplinkStateConfig,
	)

	return c
}

func (c *uplinkStateController) Start() error {
	klog.Infof("Starting %s", uplinkStateControllerName)
	return controllerutil.Start(c.uplinkStateController)
}

func (c *uplinkStateController) Stop() {
	klog.Infof("Stopping %s", uplinkStateControllerName)
	controllerutil.Stop(
		c.uplinkStateController,
	)
}

func (c *uplinkStateController) syncUplinkState(_ context.Context, key string) error {
	state, err := c.uplinkStateLister.Get(key)
	deleted := apierrors.IsNotFound(err)
	var uplinkName string
	if deleted {
		uplinkName = c.uplinkByStateKey[key]
		if uplinkName == "" {
			return c.reconcileUplinkNetworks("")
		}
	} else {
		if err != nil {
			return err
		}
		var nodeName string
		uplinkName, nodeName = uplinkutil.StateIdentity(state)
		if uplinkName == "" || nodeName == "" {
			return nil
		}
		if nodeName != c.nodeName {
			return nil
		}
		c.uplinkByStateKey[key] = uplinkName
	}
	if err := c.reconcileUplinkNetworks(uplinkName); err != nil {
		return err
	}
	if deleted {
		delete(c.uplinkByStateKey, key)
	}
	return nil
}

func (c *uplinkStateController) reconcileUplinkNetworks(uplinkName string) error {
	networkNames, err := c.uplinkNetworkNames(uplinkName)
	if err != nil {
		return err
	}
	for _, networkName := range networkNames {
		if c.routeImportManager != nil {
			if err := c.routeImportManager.ReconcileNetwork(networkName); err != nil {
				return err
			}
		}
		if !c.networkManager.NodeHasNetwork(c.nodeName, networkName) {
			continue
		}
		c.requestNetworkGatewaySync(c.nodeName, networkName)
	}
	return nil
}

func (c *uplinkStateController) requestNetworkGatewaySync(nodeName, networkName string) {
	c.nodeReconciler.ReconcileNetworkWithPreflight(nodeName, networkName, func(handler nodecontroller.NodeHandler) {
		if gatewayHandler, ok := handler.(gatewaySyncHandler); ok {
			gatewayHandler.MarkGatewaySyncNeeded(nodeName)
		}
	})
}

func (c *uplinkStateController) uplinkNetworkNames(uplinkName string) ([]string, error) {
	networkNames := sets.New[string]()
	err := c.networkManager.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.Uplink() == "" {
			return nil
		}
		if uplinkName != "" && netInfo.Uplink() != uplinkName {
			return nil
		}
		networkNames.Insert(netInfo.GetNetworkName())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return networkNames.UnsortedList(), nil
}
