// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"sync"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// UserDefinedNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for user-defined networks
type UserDefinedNodeNetworkController struct {
	BaseNodeNetworkController
	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
	// management port device manager
	mpdm *managementport.MgmtPortDeviceManager
}

// NewUserDefinedNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for the given secondary network. It supports layer3, layer2 and
// localnet topology types.
func NewUserDefinedNodeNetworkController(
	cnnci *CommonNodeNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	vrfManager *vrfmanager.Controller,
	ruleManager *iprulemanager.Controller,
	mpdm *managementport.MgmtPortDeviceManager,
	defaultNetworkGateway Gateway,
	ovsClient client.Client,
	uplinkStateGatewayStatusController *UplinkStateGatewayStatusController,
) (*UserDefinedNodeNetworkController, error) {
	if netInfo.Uplink() != "" && config.Gateway.Mode != config.GatewayModeShared {
		return nil, fmt.Errorf("uplink %q for network %s is supported only in shared gateway mode",
			netInfo.Uplink(), netInfo.GetNetworkName())
	}

	snnc := &UserDefinedNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             util.NewReconcilableNetInfo(netInfo),
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
			networkManager:                  networkManager,
			ovsClient:                       ovsClient,
		},
		mpdm: mpdm,
	}
	if util.IsNetworkSegmentationSupportEnabled() && snnc.IsPrimaryNetwork() {
		node, err := snnc.watchFactory.GetNode(snnc.name)
		if err != nil {
			return nil, fmt.Errorf("error retrieving node %s while creating node network controller for network %s: %v",
				snnc.name, netInfo.GetNetworkName(), err)
		}

		var uplinkStateInformer uplinkinformers.UplinkStateInformer
		if util.IsUplinkEnabled() {
			uplinkStateInformer = snnc.watchFactory.UplinkStateInformer()
		}
		snnc.gateway, err = NewUserDefinedNetworkGateway(snnc.GetNetInfo(), node,
			snnc.watchFactory.NodeCoreInformer(), snnc.Kube, vrfManager, ruleManager, defaultNetworkGateway,
			ovsClient, uplinkStateInformer, uplinkStateGatewayStatusController)
		if err != nil {
			return nil, fmt.Errorf("error creating UDN gateway for network %s: %v", netInfo.GetNetworkName(), err)
		}
	}
	return snnc, nil
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *UserDefinedNodeNetworkController) Start(_ context.Context) error {
	klog.Infof("Starting UDN node network controller for network %s", nc.GetNetworkName())

	if util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() {
		if err := nc.gateway.Start(); err != nil {
			return fmt.Errorf("failed to start node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}
	return nil
}

// Stop gracefully stops the controller
func (nc *UserDefinedNodeNetworkController) Stop() {
	if nc.stopChan == nil {
		klog.Infof("UDN node network controller for network %s is already stopped", nc.GetNetworkName())
		return
	}
	klog.Infof("Stopping UDN node network controller for network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.stopChan = nil
	if nc.gateway != nil {
		nc.gateway.Stop()
	}
	nc.wg.Wait()
}

// Cleanup cleans up node entities for the given user-defined network
func (nc *UserDefinedNodeNetworkController) Cleanup(_ context.Context) error {
	var errors []error
	var err error

	if nc.gateway != nil {
		if err = nc.gateway.Cleanup(); err != nil {
			errors = append(errors, fmt.Errorf("deleting network gateway for network %s failed: %v", nc.GetNetworkName(), err))
		}
	}
	if nc.mpdm != nil && util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() {
		if err = nc.mpdm.ReleaseDeviceIDForNetwork(nc.GetNetworkName()); err != nil {
			errors = append(errors, fmt.Errorf("deleting device ID for network %s failed: %v", nc.GetNetworkName(), err))
		}
	}
	if len(errors) > 0 {
		return kerrors.NewAggregate(errors)
	}
	return nil
}

// HandleNetworkRefChange satisfies the NetworkController interface. UDN node controllers only
// manage local node state, so NAD reference changes for remote nodes are ignored.
func (nc *UserDefinedNodeNetworkController) HandleNetworkRefChange(context.Context, string, bool) {}

func (nc *UserDefinedNodeNetworkController) shouldReconcileNetworkChange(old, new util.NetInfo) bool {
	switch {
	case util.IsPodNetworkAdvertisedAtNode(old, nc.name) != util.IsPodNetworkAdvertisedAtNode(new, nc.name):
		return true
	case util.IsPodNetworkAdvertisedAtNodeDefaultVRF(old, nc.name) != util.IsPodNetworkAdvertisedAtNodeDefaultVRF(new, nc.name):
		return true
	}
	return false
}

// Reconcile function reconciles three entities based on whether UDN network is advertised
// and the gateway mode:
// 1. IP rules
// 2. OpenFlows on br-ex bridge to forward traffic to correct ofports
func (nc *UserDefinedNodeNetworkController) Reconcile(_ context.Context, netInfo util.NetInfo) error {
	reconcilePodNetwork := nc.shouldReconcileNetworkChange(nc.ReconcilableNetInfo, netInfo)
	err := util.ReconcileNetInfo(nc.ReconcilableNetInfo, netInfo)
	if err != nil {
		klog.Errorf("Failed to reconcile network information for network %s: %v", nc.GetNetworkName(), err)
	}

	if reconcilePodNetwork {
		if nc.gateway != nil {
			nc.gateway.Reconcile()
		}
	}

	return nil
}
