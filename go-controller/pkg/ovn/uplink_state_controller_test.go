// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"testing"

	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklister "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

type uplinkStateNetworkManagerStub struct {
	networkmanager.Interface
	networks map[string]util.NetInfo
}

func (m *uplinkStateNetworkManagerStub) DoWithLock(f func(util.NetInfo) error) error {
	for _, network := range m.networks {
		if err := f(network); err != nil {
			return err
		}
	}
	return nil
}

func (m *uplinkStateNetworkManagerStub) NodeHasNetwork(_, _ string) bool {
	return false
}

type routeImportRecorder struct {
	routeimport.Manager
	reconciledNetworks []string
}

func (r *routeImportRecorder) ReconcileNetwork(networkName string) error {
	r.reconciledNetworks = append(r.reconciledNetworks, networkName)
	return nil
}

func TestReconcileUplinkNetworksReconcilesRouteImport(t *testing.T) {
	g := gomega.NewWithT(t)
	netInfo := multinetworkmocks.NewNetInfo(t)
	netInfo.On("GetNetworkName").Return("blue")
	netInfo.On("Uplink").Return("uplink-a")

	networkManager := &uplinkStateNetworkManagerStub{
		networks: map[string]util.NetInfo{"blue": netInfo},
	}
	routeImportManager := &routeImportRecorder{}
	controller := &uplinkStateController{
		networkManager:     networkManager,
		routeImportManager: routeImportManager,
		nodeName:           "node-a",
	}

	g.Expect(controller.reconcileUplinkNetworks("")).To(gomega.Succeed())
	g.Expect(routeImportManager.reconciledNetworks).To(gomega.ConsistOf("blue"))
}

func TestSyncUplinkStateDeleteUsesCachedUplink(t *testing.T) {
	g := gomega.NewWithT(t)
	blue := multinetworkmocks.NewNetInfo(t)
	blue.On("GetNetworkName").Return("blue")
	blue.On("Uplink").Return("uplink-a")
	red := multinetworkmocks.NewNetInfo(t)
	red.On("Uplink").Return("uplink-b")

	state := &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{Name: "state-a"},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: "uplink-a",
			NodeName:   "node-a",
		},
	}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(indexer.Add(state)).To(gomega.Succeed())

	networkManager := &uplinkStateNetworkManagerStub{
		networks: map[string]util.NetInfo{
			"blue": blue,
			"red":  red,
		},
	}
	routeImportManager := &routeImportRecorder{}
	controller := &uplinkStateController{
		uplinkStateLister:  uplinklister.NewUplinkStateLister(indexer),
		uplinkByStateKey:   map[string]string{},
		networkManager:     networkManager,
		routeImportManager: routeImportManager,
		nodeName:           "node-a",
	}

	g.Expect(controller.syncUplinkState(context.Background(), state.Name)).To(gomega.Succeed())
	g.Expect(routeImportManager.reconciledNetworks).To(gomega.ConsistOf("blue"))
	routeImportManager.reconciledNetworks = nil

	g.Expect(indexer.Delete(state)).To(gomega.Succeed())
	g.Expect(controller.syncUplinkState(context.Background(), state.Name)).To(gomega.Succeed())
	g.Expect(routeImportManager.reconciledNetworks).To(gomega.ConsistOf("blue"))
	g.Expect(controller.uplinkByStateKey).To(gomega.BeEmpty())
}
