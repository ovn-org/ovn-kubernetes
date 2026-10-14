// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestNADNeedsUpdate_NotifiesReconcilersOnNoopUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	c := &nadController{name: "test-nad-controller"}
	c.RegisterNADReconciler(r)

	oldNAD := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nad", ResourceVersion: "1"},
		Spec:       nettypes.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion":"0.3.1"}`},
	}
	newNAD := oldNAD.DeepCopy()
	newNAD.ResourceVersion = "2"

	needsUpdate := c.nadNeedsUpdate(oldNAD, newNAD)
	g.Expect(needsUpdate).To(gomega.BeFalse())

	g.Eventually(keyCh, time.Second).Should(gomega.Receive(gomega.Equal("ns/nad")))
}

func TestNADNeedsUpdate_DoesNotNotifyReconcilersOnRelevantUpdate(t *testing.T) {
	g := gomega.NewWithT(t)

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	c := &nadController{name: "test-nad-controller"}
	c.RegisterNADReconciler(r)

	oldNAD := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nad", ResourceVersion: "1"},
		Spec:       nettypes.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion":"0.3.1"}`},
	}
	newNAD := oldNAD.DeepCopy()
	newNAD.ResourceVersion = "2"
	newNAD.Spec.Config = `{"cniVersion":"0.3.1","name":"changed"}`

	needsUpdate := c.nadNeedsUpdate(oldNAD, newNAD)
	g.Expect(needsUpdate).To(gomega.BeTrue())

	g.Consistently(keyCh, 200*time.Millisecond).ShouldNot(gomega.Receive())
}

func TestSyncNAD_NotifiesReconcilers(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})

	keyCh := make(chan string, 1)
	r := controller.NewReconciler("test-nad-reconciler-sync", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile: func(key string) error {
			keyCh <- key
			return nil
		},
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	t.Cleanup(func() { controller.Stop(r) })

	nc := &networkController{
		networks:           map[string]util.MutableNetInfo{},
		networkControllers: map[string]*networkControllerState{},
	}
	netIDAlloc := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		networkController:  nc,
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkIDAllocator: netIDAlloc,
	}
	c.RegisterNADReconciler(r)

	nadNS := "ns"
	nadName := "nad"
	nadKey := nadNS + "/" + nadName
	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := ovntesting.BuildNAD(nadName, nadNS, networkAPrimary)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// The NAD has no network ID annotation so syncNAD will not ensure the network,
	// but it should still notify all reconcilers.
	g.Expect(c.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Eventually(keyCh, time.Second).Should(gomega.Receive(gomega.Equal(nadKey)))
}

type testNetworkController struct {
	util.ReconcilableNetInfo
	tcm             *testControllerManager
	handleRefChange func(node string, active bool)
}

type testNetworkRefReconciler struct {
	updates chan struct {
		node        string
		networkName string
	}
}

func (tr *testNetworkRefReconciler) Reconcile(node, networkName string) {
	if tr == nil || tr.updates == nil {
		return
	}
	tr.updates <- struct {
		node        string
		networkName string
	}{
		node:        node,
		networkName: networkName,
	}
}

func (tnc *testNetworkController) Start(context.Context) error {
	tnc.tcm.Lock()
	delay := tnc.tcm.startDelay
	tnc.tcm.Unlock()
	if delay > 0 {
		// Simulate a slow UDN Start so async syncAll can return before Start finishes.
		time.Sleep(delay)
	}
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("starting network: %s\n", testNetworkKey(tnc))
	tnc.tcm.started = append(tnc.tcm.started, testNetworkKey(tnc))
	if tnc.tcm.raiseErrorWhenStartingController != nil {
		return tnc.tcm.raiseErrorWhenStartingController
	}
	return nil
}

func (tnc *testNetworkController) Stop() {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("stopping network: %s\n", testNetworkKey(tnc))
	tnc.tcm.stopped = append(tnc.tcm.stopped, testNetworkKey(tnc))
}

func (tnc *testNetworkController) Cleanup() error {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("cleaning up network: %s\n", testNetworkKey(tnc))
	tnc.tcm.cleaned = append(tnc.tcm.cleaned, testNetworkKey(tnc))
	return tnc.tcm.raiseErrorWhenCleaningController
}

func (tnc *testNetworkController) Reconcile(netInfo util.NetInfo) error {
	return util.ReconcileNetInfo(tnc.ReconcilableNetInfo, netInfo)
}

func (tnc *testNetworkController) HandleNetworkRefChange(node string, active bool) {
	if tnc.handleRefChange != nil {
		tnc.handleRefChange(node, active)
	}
}

// GomegaString is used to avoid printing embedded mutexes which can cause a
// race
func (tnc *testNetworkController) GomegaString() string {
	return format.Object(tnc.GetNetworkName(), 1)
}

func TestSyncNAD_ForceDeleteKeepsCacheForExistingNAD(t *testing.T) {
	g := gomega.NewWithT(t)

	key := "ns/nad"
	ns := "ns"
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nad",
			Namespace: ns,
		},
	}

	netIDAlloc := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		nads:               map[string]string{key: "netA"},
		primaryNADs:        map[string]string{ns: key},
		markedForRemoval:   map[string]time.Time{key: time.Now().Add(-time.Minute)},
		networkIDAllocator: netIDAlloc,
		networkController: &networkController{
			networks:           map[string]util.MutableNetInfo{},
			networkControllers: map[string]*networkControllerState{},
		},
	}

	g.Expect(c.syncNAD(key, nad)).To(gomega.Succeed())
	g.Expect(c.nads).To(gomega.HaveKeyWithValue(key, "netA"))
	g.Expect(c.primaryNADs).To(gomega.HaveKeyWithValue(ns, key))
	g.Expect(c.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func TestSyncNAD_ForceDeleteRemovesCacheOnActualDelete(t *testing.T) {
	g := gomega.NewWithT(t)

	key := "ns/nad"
	ns := "ns"

	netIDAlloc := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(netIDAlloc.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	c := &nadController{
		name:               "test-nad-controller",
		nads:               map[string]string{key: "netA"},
		primaryNADs:        map[string]string{ns: key},
		markedForRemoval:   map[string]time.Time{key: time.Now().Add(-time.Minute)},
		networkIDAllocator: netIDAlloc,
		networkController: &networkController{
			networks:           map[string]util.MutableNetInfo{},
			networkControllers: map[string]*networkControllerState{},
		},
	}

	g.Expect(c.syncNAD(key, nil)).To(gomega.Succeed())
	g.Expect(c.nads).ToNot(gomega.HaveKey(key))
	g.Expect(c.primaryNADs).ToNot(gomega.HaveKey(ns))
	g.Expect(c.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func ptrTo[T any](v T) *T { return &v }

func testNetworkKey(nInfo util.NetInfo) string {
	return nInfo.GetNetworkName() + " " + nInfo.TopologyType()
}

func networkFromTestNetworkKey(key string) string {
	return key[:strings.LastIndex(key, " ")]
}

type fakeNADNamespaceLister struct {
	nads map[string]*nettypes.NetworkAttachmentDefinition
}

func (f *fakeNADNamespaceLister) List(_ labels.Selector) ([]*nettypes.NetworkAttachmentDefinition, error) {
	result := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range f.nads {
		result = append(result, nad)
	}
	return result, nil
}

func (f *fakeNADNamespaceLister) Get(name string) (*nettypes.NetworkAttachmentDefinition, error) {
	if nad, ok := f.nads[name]; ok {
		return nad, nil
	}
	return nil, apierrors.NewNotFound(nettypes.Resource("networkattachmentdefinition"), name)
}

type fakeNADLister struct {
	nads map[string]*nettypes.NetworkAttachmentDefinition
}

func (f *fakeNADLister) List(_ labels.Selector) ([]*nettypes.NetworkAttachmentDefinition, error) {
	result := []*nettypes.NetworkAttachmentDefinition{}
	for _, nad := range f.nads {
		result = append(result, nad)
	}
	return result, nil
}

func (f *fakeNADLister) NetworkAttachmentDefinitions(_ string) nadlisters.NetworkAttachmentDefinitionNamespaceLister {
	return &fakeNADNamespaceLister{nads: f.nads}
}

type fakeCNCLister struct {
	cncs map[string]*networkconnectv1.ClusterNetworkConnect
}

func (f *fakeCNCLister) List(_ labels.Selector) ([]*networkconnectv1.ClusterNetworkConnect, error) {
	result := []*networkconnectv1.ClusterNetworkConnect{}
	for _, cnc := range f.cncs {
		result = append(result, cnc)
	}
	return result, nil
}

func (f *fakeCNCLister) Get(name string) (*networkconnectv1.ClusterNetworkConnect, error) {
	if cnc, ok := f.cncs[name]; ok {
		return cnc, nil
	}
	return nil, apierrors.NewNotFound(networkconnectv1.Resource("clusternetworkconnect"), name)
}

type testControllerManager struct {
	sync.Mutex

	defaultNetwork *testNetworkController
	controllers    map[string]NetworkController

	started []string
	stopped []string
	cleaned []string

	raiseErrorWhenCreatingController error
	raiseErrorWhenStartingController error
	raiseErrorWhenCleaningController error
	// startDelay is applied inside testNetworkController.Start for async start tests.
	startDelay time.Duration

	valid []util.NetInfo
}

func (tcm *testControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	tcm.Lock()
	defer tcm.Unlock()
	if tcm.raiseErrorWhenCreatingController != nil {
		return nil, tcm.raiseErrorWhenCreatingController
	}
	t := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
	}
	tcm.controllers[testNetworkKey(netInfo)] = t
	return t, nil
}

func (tcm *testControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	tcm.valid = validNetworks
	return nil
}

func (tcm *testControllerManager) GetDefaultNetworkController() ReconcilableNetworkController {
	return tcm.defaultNetwork
}

func (tcm *testControllerManager) Reconcile(string, util.NetInfo, util.NetInfo) error {
	return nil
}

func (tcm *testControllerManager) Filter(*nettypes.NetworkAttachmentDefinition) (bool, error) {
	return false, nil
}

type fakeNamespaceLister struct{}

func (f *fakeNamespaceLister) List(labels.Selector) (ret []*corev1.Namespace, err error) {
	return nil, nil
}

// Get retrieves the Namespace from the index for a given name.
// Objects returned here must be treated as read-only.
func (f *fakeNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
		},
	}, nil
}

func TestNADController(t *testing.T) {
	t.Run("filter respects node trackers", func(t *testing.T) {
		if err := config.PrepareTestConfig(); err != nil {
			t.Fatalf("prepare test config: %v", err)
		}
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		pt := &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
		}
		pt.nodeNADToPodCache["node1"] = map[string]map[string]struct{}{
			"ns1/nad1": {"pod": {}},
		}

		cm := &nadController{
			filterNADsOnNode: "node1",
			podTracker:       pt,
			nads: map[string]string{
				"ns1/nad1": "network1",
				"ns2/nad2": "network2",
			},
			nadsByNetwork: map[string]sets.Set[string]{
				"network1": sets.New[string]("ns1/nad1"),
				"network2": sets.New[string]("ns2/nad2"),
			},
			dynamicFilterNADs: map[string]bool{
				"ns1/nad1": true,
				"ns2/nad2": true,
			},
			cncConnectedNetworks: map[string]sets.Set[string]{},
		}

		tests := []struct {
			name     string
			nad      *nettypes.NetworkAttachmentDefinition
			expected bool
		}{
			{
				name: "no ownerRef",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "nad1"},
				},
				expected: false,
			},
			{
				name: "unrelated ownerRef",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1", Name: "nad1",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "Deployment",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: false,
			},
			{
				name: "UDN ownerRef but unused on node -> filtered",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns2", Name: "nad2",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "UserDefinedNetwork",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: true,
			},
			{
				name: "UDN ownerRef and used on node -> not filtered",
				nad: &nettypes.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1", Name: "nad1",
						OwnerReferences: []metav1.OwnerReference{{
							Kind:       "UserDefinedNetwork",
							Controller: ptrTo(true),
						}},
					},
				},
				expected: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := cm.filter(tt.nad)
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if got != tt.expected {
					t.Fatalf("expected filter=%v got %v", tt.expected, got)
				}
			})
		}
	})

	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets:       "10.1.130.0/24",
		TransitSubnet: config.ClusterManager.V4TransitSubnet,
		Role:          types.NetworkRolePrimary,
		MTU:           1400,
	}
	networkAIncompatible := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	networkASecondary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRoleSecondary,
		MTU:     1400,
	}

	networkBSecondary := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkBSecondary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	networkDefault := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	type args struct {
		nad     string
		network *ovncnitypes.NetConf
		wantErr bool
	}
	type expected struct {
		network *ovncnitypes.NetConf
		nads    []string
	}
	tests := []struct {
		name     string
		args     []args
		expected []expected
	}{
		{
			name: "NAD on default network is tracked with default controller",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkDefault,
				},
			},
			expected: []expected{
				{
					network: networkDefault,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "two NADs added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "Two Primary NADs added for same namespace",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAPrimary,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two Primary NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test2/nad_2",
					network: networkAPrimary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test2/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad: "test/nad_2",
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "NAD added then updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to same network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkBSecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "NAD added then incompatible NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
				},
			},
			expected: []expected{
				{
					network: networkAIncompatible,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "non ovn-k NAD added",
			args: []args{
				{
					nad: "test/nad_1",
					network: &ovncnitypes.NetConf{
						NetConf: cnitypes.NetConf{
							Name: "test",
							Type: "sriov",
						},
					},
					wantErr: false,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadController := &nadController{
				nads:                map[string]string{},
				primaryNADs:         map[string]string{},
				networkController:   newNetworkController("", "", tcm, nil, false),
				networkIDAllocator:  id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
				tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
				nadClient:           fakeClient.NetworkAttchDefClient,
				nodeLister:          wf.NodeCoreInformer().Lister(),
				namespaceLister:     &fakeNamespaceLister{},
			}
			err = nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			netController := nadController.networkController

			// Drive reconciliation only for networks touched by the NAD operation
			// to avoid assertions against transient async queue states.
			syncTouchedNetworks := func(nadKey, prevNetwork string) {
				networkNames := sets.New[string]()
				if prevNetwork != "" {
					networkNames.Insert(prevNetwork)
				}
				if currNetwork := nadController.nads[nadKey]; currNetwork != "" {
					networkNames.Insert(currNetwork)
				}
				for _, network := range networkNames.UnsortedList() {
					g.Expect(netController.syncNetwork(network)).To(gomega.Succeed())
				}
			}

			for _, args := range tt.args {
				namespace, name, err := cache.SplitMetaNamespaceKey(args.nad)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				var nad *nettypes.NetworkAttachmentDefinition
				if args.network != nil {
					args.network.NADName = args.nad
					nad, err = ovntesting.BuildNAD(name, namespace, args.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())
					_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
					g.Expect(err).To(gomega.Or(gomega.Not(gomega.HaveOccurred()), gomega.MatchError(apierrors.IsAlreadyExists, "AlreadyExists")))
				}

				prevNetwork := nadController.nads[args.nad]
				err = nadController.syncNAD(args.nad, nad)
				if args.wantErr {
					g.Expect(err).To(gomega.HaveOccurred())
				} else {
					g.Expect(err).NotTo(gomega.HaveOccurred())
				}
				syncTouchedNetworks(args.nad, prevNetwork)
			}

			meetsExpectations := func(g gomega.Gomega) {
				// test that the manager has all the desired networks
				g.Expect(netController.getAllNetworks()).To(gomega.HaveLen(len(tt.expected)))

				var expectRunning []string
				for _, expected := range tt.expected {
					netInfo, err := util.NewNetInfo(expected.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())

					name := netInfo.GetNetworkName()
					testNetworkKey := testNetworkKey(netInfo)
					func() {
						netController.Lock()
						defer netController.Unlock()
						tcm.Lock()
						defer tcm.Unlock()

						// test that the desired networks have the expected
						// config and NADs, including the default network which
						// could have had NAD/Advertisement changes as well
						g.Expect(netController.networks).To(gomega.HaveKey(name))
						g.Expect(util.AreNetworksCompatible(netController.networks[name], netInfo)).To(gomega.BeTrue(),
							fmt.Sprintf("matching network config for network %s", name))
						nadKeys := nadController.GetNADKeysForNetwork(name)
						g.Expect(nadKeys).To(gomega.ConsistOf(expected.nads),
							fmt.Sprintf("matching NADs for network %s", name))
						id, err := nadController.networkIDAllocator.AllocateID(name)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						g.Expect(netController.networks[name].GetNetworkID()).To(gomega.Equal(id))
						if netInfo.TopologyType() == types.Layer2Topology && netInfo.IsPrimaryNetwork() {
							tunnelKeys, err := nadController.tunnelKeysAllocator.AllocateKeys(name, id, 2)
							g.Expect(err).ToNot(gomega.HaveOccurred())
							g.Expect(netController.networks[name].GetTunnelKeys()).To(gomega.Equal(tunnelKeys))
						}
						// test that the actual controllers have the expected config and NADs
						if !netInfo.IsDefault() {
							g.Expect(tcm.controllers).To(gomega.HaveKey(testNetworkKey))
							g.Expect(util.AreNetworksCompatible(tcm.controllers[testNetworkKey], netInfo)).To(gomega.BeTrue(),
								fmt.Sprintf("matching network config for network %s", name))
							g.Expect(tcm.controllers[testNetworkKey].GetNetworkID()).To(gomega.Equal(id))
							expectRunning = append(expectRunning, testNetworkKey)
						}
					}()
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						key := expected.nads[0]
						namespace, _, err := cache.SplitMetaNamespaceKey(key)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						netInfoFound, err := nadController.GetActiveNetworkForNamespace(namespace)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						g.Expect(util.AreNetworksCompatible(netInfoFound, netInfo)).To(gomega.BeTrue())
						nadKeys := nadController.GetNADKeysForNetwork(netInfoFound.GetNetworkName())
						g.Expect(nadKeys).To(gomega.ConsistOf(expected.nads))
					}
				}
				tcm.Lock()
				defer tcm.Unlock()
				expectStopped := sets.New(tcm.started...).Difference(sets.New(expectRunning...)).UnsortedList()
				// test that the controllers are started, stopped and cleaned up as expected
				g.Expect(tcm.started).To(gomega.ContainElements(expectRunning), "started network controllers")
				g.Expect(tcm.stopped).To(gomega.ConsistOf(expectStopped), "stopped network controllers")
				g.Expect(tcm.cleaned).To(gomega.ConsistOf(expectStopped), "cleaned up network controllers")

				// if we reallocate all stopped networks, they should get a higher id than base if the previous id was released
				base, err := nadController.networkIDAllocator.AllocateID("test")
				g.Expect(err).ToNot(gomega.HaveOccurred())
				for _, stopped := range expectStopped {
					network := networkFromTestNetworkKey(stopped)
					if nadController.GetNetwork(network) != nil {
						// this network is still running under different config
						continue
					}
					id, err := nadController.networkIDAllocator.AllocateID(network)
					g.Expect(err).ToNot(gomega.HaveOccurred())
					g.Expect(id).To(gomega.BeNumerically(">", base), "unexpected network ID for network %s", network)
				}
			}

			meetsExpectations(g)
		})
	}
}

func TestNetworkGracePeriodCleanup(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	// Enable segmentation and grace period
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = 2 * time.Second // short grace period for test
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	fakeCtrl := &controller.FakeController{}
	nadKey := util.GetNADName("test", "nad1")
	stopCh := make(chan struct{})
	defer close(stopCh)
	nadController := &nadController{
		nads:                map[string]string{},
		primaryNADs:         map[string]string{},
		networkController:   newNetworkController("", "", tcm, nil, false),
		networkIDAllocator:  id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
		nadClient:           fakeClient.NetworkAttchDefClient,
		namespaceLister:     &fakeNamespaceLister{},
		markedForRemoval:    map[string]time.Time{},
		controller:          fakeCtrl,
		filterNADsOnNode:    "node1",
		stopChan:            stopCh,
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{
				"node1": {
					nadKey: {"test/pod-a": {}},
				},
			},
		},
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(nadController.networkController.Start()).To(gomega.Succeed())
	defer nadController.networkController.Stop()
	// --- Step 1: Add a NAD ---
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
	}
	netConf.NADName = nadKey
	nad, err := buildNADWithAnnotations("nad1", "test", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}
	// Create the NAD in the fake client so syncNAD can find it
	_, err = fakeClient.NetworkAttchDefClient.
		K8sCniCncfIoV1().
		NetworkAttachmentDefinitions(nad.Namespace).
		Create(context.Background(), nad, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = nadController.syncNAD(nadKey, nad)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// Should have been started
	g.Eventually(func() []string {
		tcm.Lock()
		defer tcm.Unlock()
		return append([]string(nil), tcm.started...)
	}).WithTimeout(1*time.Second).Should(gomega.ContainElement(testNetworkKey(netInfo)),
		"network should be started before we check grace period")
	fakeCtrl.Lock()
	numberOfReconciles := len(fakeCtrl.Reconciles)
	fakeCtrl.Unlock()
	// --- Step 2: Mark as inactive ---
	// This triggers the grace-period timer, not immediate deletion.
	nadController.podTracker.nodeNADToPodCache = map[string]map[string]map[string]struct{}{}
	err = nadController.syncNAD(nadKey, nad)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	// --- Step 3: Verify that within the grace period, cleanup has NOT happened ---
	g.Consistently(func() []string {
		tcm.Lock()
		defer tcm.Unlock()
		return append([]string(nil), tcm.cleaned...)
	}).WithTimeout(1*time.Second).Should(gomega.BeEmpty(),
		"cleanup should not happen before grace period ends")
	// --- Step 4: Verify a *second* reconcile only AFTER grace period expires ---
	g.Eventually(func() int {
		fakeCtrl.Lock()
		defer fakeCtrl.Unlock()
		return len(fakeCtrl.Reconciles)
	}).WithTimeout(5 * time.Second).Should(gomega.Equal(numberOfReconciles + 1))
}

func TestFilteredNADDeleteReleasesNetworkID(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", tcm, nil, false),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		filterNADsOnNode:   "node1",
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey := util.GetNADName("ns1", "nad1")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "filtered-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	controller := true
	nad.OwnerReferences = []metav1.OwnerReference{
		{
			Kind:       "UserDefinedNetwork",
			Name:       "udn1",
			Controller: &controller,
		},
	}

	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Dynamic UDN is on and node is filtered, expect ID to be reserved anyway
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netInfo.GetNetworkName())).To(gomega.Equal(2))

	// Simulate a delete on filtered network and makes sure it still releases the ID
	g.Expect(nadController.syncNAD(nadKey, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netInfo.GetNetworkName())).To(gomega.Equal(types.InvalidID))
}

func TestFilteredAndActiveNADDeleteRetainsIDUntilNoRefs(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	pt := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
	}
	pt.nodeNADToPodCache["node1"] = map[string]map[string]struct{}{
		"ns1/nad1": {"pod": {}},
	}

	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", tcm, nil, false),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		filterNADsOnNode:   "node1",
		podTracker:         pt,
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey1 := util.GetNADName("ns1", "nad1")
	nadKey2 := util.GetNADName("ns2", "nad2")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "shared-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey1,
	}
	netConf2 := *netConf
	netConf2.NADName = nadKey2

	nad1, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad2, err := buildNADWithAnnotations("nad2", "ns2", &netConf2, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad1.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn1",
		Controller: ptrTo(true),
	}}
	nad2.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn2",
		Controller: ptrTo(true),
	}}

	// Active NAD should render, filtered NAD should not, but both should reserve ID.
	g.Expect(nadController.syncNAD(nadKey1, nad1)).To(gomega.Succeed())
	g.Expect(nadController.syncNAD(nadKey2, nad2)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Delete active NAD; filtered NAD still references the network, so ID stays reserved.
	g.Expect(nadController.syncNAD(nadKey1, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Delete filtered NAD; now no refs remain, so ID is released.
	g.Expect(nadController.syncNAD(nadKey2, nil)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(types.InvalidID))
}

func TestNodeHasNetworkIgnoresDynamicFilteringForBareNADs(t *testing.T) {
	tests := []struct {
		name             string
		ownerKind        string
		expectNodeHasNet bool
	}{
		{
			name:             "bare NAD is treated as present on all nodes",
			expectNodeHasNet: true,
		},
		{
			name:             "owner managed NAD stays activity gated",
			ownerKind:        "UserDefinedNetwork",
			expectNodeHasNet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nadController := &nadController{
				nads:               map[string]string{},
				nadsByNetwork:      map[string]sets.Set[string]{},
				dynamicFilterNADs:  map[string]bool{},
				primaryNADs:        map[string]string{},
				networkController:  newNetworkController("", "", tcm, nil, false),
				networkIDAllocator: id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
				filterNADsOnNode:   "node1",
			}
			g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

			nadKey := util.GetNADName("ns1", "nad1")
			netConf := &ovncnitypes.NetConf{
				Topology: types.Layer2Topology,
				NetConf: cnitypes.NetConf{
					Name: "test-net",
					Type: "ovn-k8s-cni-overlay",
				},
				Subnets: "10.1.130.0/24",
				Role:    types.NetworkRolePrimary,
				MTU:     1400,
				NADName: nadKey,
			}
			nad, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
				types.OvnNetworkIDAnnotation: "2",
			})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if tt.ownerKind != "" {
				nad.OwnerReferences = []metav1.OwnerReference{{
					Kind:       tt.ownerKind,
					Name:       "owner1",
					Controller: ptrTo(true),
				}}
			}

			g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
			g.Expect(nadController.NodeHasNetwork("node1", netConf.Name)).To(gomega.Equal(tt.expectNodeHasNet))
			g.Expect(nadController.NodeHasNetwork("node2", netConf.Name)).To(gomega.Equal(tt.expectNodeHasNet))
		})
	}
}

func TestDynamicDeleteDoesNotReleaseNetworkID(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nadController := &nadController{
		nads:               map[string]string{},
		primaryNADs:        map[string]string{},
		networkController:  newNetworkController("", "", tcm, nil, false),
		networkIDAllocator: id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
	}
	g.Expect(nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nadKey := util.GetNADName("ns1", "nad1")
	netConf := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "dyn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}

	nad, err := buildNADWithAnnotations("nad1", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Initial sync reserves the ID.
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))

	// Simulate inactive transition via expired grace period.
	nadController.markedForRemoval = map[string]time.Time{nadKey: time.Now().Add(-time.Minute)}
	g.Expect(nadController.syncNAD(nadKey, nad)).To(gomega.Succeed())
	g.Expect(nadController.networkIDAllocator.GetID(netConf.Name)).To(gomega.Equal(2))
}

func TestSyncAll(t *testing.T) {
	const nodeNetworkID = 1337
	type mode string

	const (
		modeClusterManager mode = "clusterManager"
		modeNode           mode = "node"
	)
	network_A := &ovncnitypes.NetConf{
		Topology: types.Layer3Topology,
		NetConf: cnitypes.NetConf{
			Name: "network_A",
			Type: "ovn-k8s-cni-overlay",
		},
		Role: types.NetworkRolePrimary,
		MTU:  1400,
	}
	network_A_Copy := *network_A
	network_B := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "network_B",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	type TestNAD struct {
		name      string
		netconf   *ovncnitypes.NetConf
		networkID string
	}
	tests := []struct {
		name                 string
		testNADs             []TestNAD
		syncAllInjectedError error
		expectNADIgnored     bool
		extraNADsIgnored     bool
		node                 *corev1.Node
		mode                 mode
		expectGeneratedID    bool
		expectInheritID      bool // for Node with conflicting ID
	}{
		{
			name: "multiple networks referenced by multiple nads",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
				{
					name:    "test/nad2",
					netconf: network_B,
				},
				{
					name:    "test2/nad3",
					netconf: &network_A_Copy,
				},
			},
			syncAllInjectedError: ErrNetworkControllerTopologyNotManaged,
		},
		{
			name:              "nad with negative network ID gets a replacement",
			testNADs:          []TestNAD{{name: "test/nad1", netconf: network_A, networkID: "-2"}},
			mode:              modeClusterManager,
			expectGeneratedID: true,
		},
		{
			name:              "nad with upper boundary network ID gets a replacement",
			testNADs:          []TestNAD{{name: "test/nad1", netconf: network_A, networkID: strconv.Itoa(types.MaxNetworks)}},
			mode:              modeClusterManager,
			expectGeneratedID: true,
		},
		{
			name:              "nad with huge network ID gets a replacement",
			testNADs:          []TestNAD{{name: "test/nad1", netconf: network_A, networkID: strconv.Itoa(math.MaxInt)}},
			mode:              modeClusterManager,
			expectGeneratedID: true,
		},
		{
			name: "nad already annotated with network ID",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
			},
		},
		{
			name: "nad and node with no network ID should not fail to sync in node mode and should be ignored",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: map[string]string{
						// no "OVNNodeNetworkIds" annotation or it's empty
					},
				},
			},
			mode:             modeNode,
			expectNADIgnored: true,
		},
		{
			name: "nad and node with no network ID should not fail to sync in cluster-manager mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: map[string]string{
						// no "OVNNodeNetworkIds" annotation or it's empty
					},
				},
			},
			mode:              modeClusterManager,
			expectGeneratedID: true,
		},
		{
			name: "nad without network ID + node with network ID should not fail to sync in cluster manager mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode: modeClusterManager,
		},
		{
			name: "nad with network ID + node with network ID should preserve NAD ID on sync in cluster manager mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode: modeClusterManager,
		},
		{
			name: "nad without network ID + node with network ID should sync in node mode",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"%s": "%d"}`, network_A.NetConf.Name, nodeNetworkID),
					},
				},
			},
			mode: modeNode,
		},
		{
			name: "two NADs same network but conflicting IDs on node -> second should not sync in node mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
				{
					name:    "test2/nad2",
					netconf: network_A,
					// no ID on this NAD, but node has conflicting ID
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode:             modeNode,
			extraNADsIgnored: true,
		},
		{
			name: "two NADs same network but conflicting IDs on node -> second should be inherit NAD ID in cm mode",
			testNADs: []TestNAD{
				{
					name:      "test/nad1",
					netconf:   network_A,
					networkID: "1",
				},
				{
					name:    "test2/nad2",
					netconf: network_A,
					// no ID on this NAD, but node has conflicting ID
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Annotations: map[string]string{
						util.OvnNetworkIDs: fmt.Sprintf(`{"network_A": "%d"}`, nodeNetworkID),
					},
				},
			},
			mode:            modeClusterManager,
			expectInheritID: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
			}
			if tt.syncAllInjectedError != nil {
				tcm.raiseErrorWhenCreatingController = tt.syncAllInjectedError
			}

			// default to cluster manager
			if len(tt.mode) == 0 {
				tt.mode = modeClusterManager
			}

			var controller Controller
			if tt.mode == modeNode {
				controller, err = NewForNode("test", tcm, wf)
			} else {
				controller, err = NewForCluster(
					tcm,
					wf,
					fakeClient,
					nil,
					id.NewTunnelKeyAllocator("TunnelKeys"),
				)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())

			expectedNetworks := map[string]util.NetInfo{}
			expectedPrimaryNetworks := map[string]util.NetInfo{}
			for _, namespace := range []string{"test", "test2"} {
				_, err = fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(),
					&corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name:   namespace,
							Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
						},
					}, metav1.CreateOptions{},
				)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())
			for i, testNAD := range tt.testNADs {
				if i > 0 && tt.extraNADsIgnored {
					break
				}
				namespace, name, err := cache.SplitMetaNamespaceKey(testNAD.name)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				testNAD.netconf.NADName = testNAD.name
				nadAnnotations := map[string]string{
					types.OvnNetworkNameAnnotation: testNAD.netconf.Name,
				}
				if len(testNAD.networkID) > 0 {
					nadAnnotations[types.OvnNetworkIDAnnotation] = testNAD.networkID
				}
				nad, err := buildNADWithAnnotations(name, namespace, testNAD.netconf, nadAnnotations)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
					context.Background(),
					nad,
					metav1.CreateOptions{},
				)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				netInfo := expectedNetworks[testNAD.netconf.Name]
				if netInfo == nil && !tt.expectNADIgnored {
					netInfo, err = util.NewNetInfo(testNAD.netconf)
					mutableNetInfo := util.NewMutableNetInfo(netInfo)
					if tt.expectGeneratedID {
						mutableNetInfo.SetNetworkID(1)
						netInfo = mutableNetInfo
					} else if testNAD.networkID != "" {
						id, err := strconv.Atoi(testNAD.networkID)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						mutableNetInfo.SetNetworkID(id)
						netInfo = mutableNetInfo
					} else if tt.node != nil {
						mutableNetInfo.SetNetworkID(nodeNetworkID)
						netInfo = mutableNetInfo
					}
					g.Expect(err).ToNot(gomega.HaveOccurred())
					expectedNetworks[testNAD.netconf.Name] = netInfo
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						expectedPrimaryNetworks[netInfo.GetNetworkName()] = netInfo
					}
				}
			}

			if tt.node != nil {
				_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), tt.node, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Exercise initial synchronization without starting workers: annotation
			// updates can otherwise reconcile NADs while the network controller
			// takes the cleanup snapshot asserted below.
			nadController := controller.(*nadController)
			defer controller.Stop()
			g.Expect(nadController.syncAll()).To(gomega.Succeed())
			g.Expect(nadController.networkController.syncAll()).To(gomega.Succeed())

			if tt.expectGeneratedID {
				for _, testNAD := range tt.testNADs {
					namespace, name, _ := strings.Cut(testNAD.name, "/")
					nad, err := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(namespace).Get(context.Background(), name, metav1.GetOptions{})
					g.Expect(err).ToNot(gomega.HaveOccurred())
					g.Expect(nad.Annotations[types.OvnNetworkIDAnnotation]).To(gomega.Equal("1"))
				}
			}

			actualNetworks := map[string]util.NetInfo{}
			for _, network := range tcm.valid {
				actualNetworks[network.GetNetworkName()] = network
			}

			g.Expect(actualNetworks).To(gomega.HaveLen(len(expectedNetworks)))
			for name, network := range expectedNetworks {
				g.Expect(actualNetworks).To(gomega.HaveKey(name))
				g.Expect(util.AreNetworksCompatible(actualNetworks[name], network)).To(
					gomega.BeTrue(),
					"network compatibility failed\nactual=%#v\nexpected=%#v",
					actualNetworks[name],
					network,
				)
				if network.GetNetworkID() != types.InvalidID {
					g.Expect(actualNetworks[name].GetNetworkID()).To(gomega.Equal(network.GetNetworkID()))
				}
			}
			if tt.expectInheritID {
				// Only one network should exist
				g.Expect(actualNetworks).To(gomega.HaveLen(1), "network should still be created")

				info := actualNetworks["network_A"]
				g.Expect(info).ToNot(gomega.BeNil())

				// Expect ID = 1 (from first NAD), not from node
				g.Expect(info.GetNetworkID()).To(gomega.Equal(1))

				// Both NADs should now be part of the same network
				nadKeys := controller.Interface().GetNADKeysForNetwork(info.GetNetworkName())
				g.Expect(nadKeys).To(gomega.HaveLen(2))

				// NAD2 should now have the inherited ID
				nad2, _ := fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().
					NetworkAttachmentDefinitions("test2").Get(context.Background(), "nad2", metav1.GetOptions{})
				g.Expect(nad2).ToNot(gomega.BeNil())
				g.Expect(nad2.Annotations[types.OvnNetworkIDAnnotation]).To(gomega.Equal("1"))

				// Skip further primary network checks for this scenario
				return
			}
			if !tt.expectNADIgnored {
				actualPrimaryNetwork, err := controller.Interface().GetActiveNetworkForNamespace("test")
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(expectedPrimaryNetworks).To(gomega.HaveKey(actualPrimaryNetwork.GetNetworkName()))
				expectedPrimaryNetwork := expectedPrimaryNetworks[actualPrimaryNetwork.GetNetworkName()]
				g.Expect(util.AreNetworksCompatible(expectedPrimaryNetwork, actualPrimaryNetwork)).To(gomega.BeTrue())
			}
		})
	}
}

func TestResourceCleanup(t *testing.T) {
	g := gomega.NewWithT(t)
	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	fakeClient := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadController := &nadController{
		nads:                map[string]string{},
		primaryNADs:         map[string]string{},
		networkController:   newNetworkController("", "", tcm, nil, false),
		networkIDAllocator:  id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
		nadClient:           fakeClient.NetworkAttchDefClient,
		namespaceLister:     &fakeNamespaceLister{},
		nodeLister:          wf.NodeCoreInformer().Lister(),
	}
	err = nadController.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(nadController.networkController.Start()).To(gomega.Succeed())
	defer nadController.networkController.Stop()

	nadNs := "test"
	nadName := "nad_1"
	nadKey := nadNs + "/" + nadName
	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
		NADName: nadKey,
	}
	nad, err := ovntesting.BuildNAD(nadName, nadNs, networkAPrimary)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// make annotation update fail (nad doesn't exist), make sure networkID and tunnel keys are released
	err = nadController.syncNAD(nadKey, nad)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("failed to annotate network ID and/or tunnel keys"))
	// we know the allocated network ID was 1 and tunnelKeys were [16711684, 16715779] (first available IDs after Default network)
	// try to reserve these exact IDs for a different network to make sure they were released
	err = nadController.networkIDAllocator.ReserveID("networkB", 1)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = nadController.tunnelKeysAllocator.ReserveKeys("networkB", []int{16711684, 16715779})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

func buildNADWithAnnotations(name, namespace string, network *ovncnitypes.NetConf, annotations map[string]string) (*nettypes.NetworkAttachmentDefinition, error) {
	nad, err := ovntesting.BuildNAD(name, namespace, network)
	if err != nil {
		return nil, err
	}
	nad.Annotations = annotations
	return nad, nil
}

func buildNAD(name, namespace string, network *ovncnitypes.NetConf) (*nettypes.NetworkAttachmentDefinition, error) {
	config, err := json.Marshal(network)
	if err != nil {
		return nil, err
	}
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: string(config),
		},
	}
	return nad, nil
}

func TestNodeHasNetworkIncludesCNCConnectivity(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

	podTracker := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{
			"node1": {
				"ns1/nad-a": {"pod-a": {}},
			},
		},
	}

	nc := &nadController{
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string]("ns1/nad-a"),
			"net-b": sets.New[string]("ns1/nad-b"),
		},
		dynamicFilterNADs: map[string]bool{
			"ns1/nad-a": true,
			"ns1/nad-b": true,
		},
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-b": sets.New[string]("net-a"),
		},
		podTracker: podTracker,
	}

	g.Expect(nc.NodeHasNetwork("node1", "net-a")).To(gomega.BeTrue())
	g.Expect(nc.NodeHasNetwork("node1", "net-b")).To(gomega.BeTrue())
	g.Expect(nc.NodeHasNetwork("node1", "net-c")).To(gomega.BeFalse())
}

func TestSyncCNCIncludesClusterIPServiceConnectivity(t *testing.T) {
	g := gomega.NewWithT(t)

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-b", 2)).To(gomega.Succeed())

	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-services",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`,
			},
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{
				networkconnectv1.ServiceNetwork,
			},
		},
	}

	nc := &nadController{
		networkIDAllocator: networkIDAllocator,
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](),
			"net-b": sets.New[string](),
		},
		cncConnectedNetworks: map[string]sets.Set[string]{},
		cncLister: &fakeCNCLister{
			cncs: map[string]*networkconnectv1.ClusterNetworkConnect{
				cnc.Name: cnc,
			},
		},
	}

	g.Expect(nc.syncAllCNCs()).To(gomega.Succeed())
	g.Expect(nc.cncConnectedNetworks["net-a"].Has("net-b")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks["net-b"].Has("net-a")).To(gomega.BeTrue())
}

func TestSyncCNCSyncsOnlyRequestedCNC(t *testing.T) {
	g := gomega.NewWithT(t)

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	for networkName, networkID := range map[string]int{
		"net-a": 1,
		"net-b": 2,
		"net-c": 3,
		"net-d": 4,
	} {
		g.Expect(networkIDAllocator.ReserveID(networkName, networkID)).To(gomega.Succeed())
	}

	cncAB := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-ab",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`,
			},
		},
	}
	cncCD := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-cd",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_3":{"ipv4":"192.168.2.0/24"},"layer3_4":{"ipv4":"192.168.3.0/24"}}`,
			},
		},
	}

	nc := &nadController{
		networkIDAllocator: networkIDAllocator,
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](),
			"net-b": sets.New[string](),
			"net-c": sets.New[string](),
			"net-d": sets.New[string](),
		},
		cncSelectedNetworks:  map[string]sets.Set[string]{},
		cncConnectedNetworks: map[string]sets.Set[string]{},
		cncLister: &fakeCNCLister{
			cncs: map[string]*networkconnectv1.ClusterNetworkConnect{
				cncAB.Name: cncAB,
				cncCD.Name: cncCD,
			},
		},
	}

	g.Expect(nc.syncCNC(cncAB.Name)).To(gomega.Succeed())

	g.Expect(nc.cncConnectedNetworks["net-a"].Has("net-b")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks["net-b"].Has("net-a")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks).ToNot(gomega.HaveKey("net-c"))
	g.Expect(nc.cncConnectedNetworks).ToNot(gomega.HaveKey("net-d"))
}

func TestSyncCNCIndexesUnresolvedNetworkIDs(t *testing.T) {
	g := gomega.NewWithT(t)

	reconcileCh := make(chan string, 1)
	cncReconciler := controller.NewController(
		"test-unresolved-cnc-index-reconciler",
		&controller.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				reconcileCh <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(cncReconciler)).To(gomega.Succeed())
	defer controller.Stop(cncReconciler)

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-waiting",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_7":{"ipv4":"192.168.0.0/24"}}`,
			},
		},
	}
	nc := &nadController{
		cncController:      cncReconciler,
		networkIDAllocator: networkIDAllocator,
		nadsByNetwork:      map[string]sets.Set[string]{},
		cncLister: &fakeCNCLister{
			cncs: map[string]*networkconnectv1.ClusterNetworkConnect{
				cnc.Name: cnc,
			},
		},
	}

	g.Expect(nc.syncCNC(cnc.Name)).To(gomega.Succeed())
	g.Expect(nc.cncConnectedNetworks).To(gomega.BeEmpty())
	g.Expect(nc.cncsByNetworkID[7].Has(cnc.Name)).To(gomega.BeTrue())

	nc.reconcileCNCsForNetworkIDs(7)

	g.Eventually(reconcileCh, time.Second).Should(gomega.Receive(gomega.Equal(cnc.Name)))
}

func TestSyncCNCDeletePreservesOtherConnectivity(t *testing.T) {
	g := gomega.NewWithT(t)

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	for networkName, networkID := range map[string]int{
		"net-a": 1,
		"net-b": 2,
		"net-c": 3,
	} {
		g.Expect(networkIDAllocator.ReserveID(networkName, networkID)).To(gomega.Succeed())
	}

	cncAB := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-ab",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`,
			},
		},
	}
	cncBC := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-bc",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_2":{"ipv4":"192.168.2.0/24"},"layer3_3":{"ipv4":"192.168.3.0/24"}}`,
			},
		},
	}
	cncLister := &fakeCNCLister{
		cncs: map[string]*networkconnectv1.ClusterNetworkConnect{
			cncAB.Name: cncAB,
			cncBC.Name: cncBC,
		},
	}

	nc := &nadController{
		networkIDAllocator: networkIDAllocator,
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](),
			"net-b": sets.New[string](),
			"net-c": sets.New[string](),
		},
		cncConnectedNetworks: map[string]sets.Set[string]{},
		cncLister:            cncLister,
	}

	g.Expect(nc.syncAllCNCs()).To(gomega.Succeed())
	g.Expect(nc.cncConnectedNetworks["net-a"].Has("net-b")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks["net-b"].Has("net-c")).To(gomega.BeTrue())

	delete(cncLister.cncs, cncAB.Name)
	g.Expect(nc.syncCNC(cncAB.Name)).To(gomega.Succeed())

	g.Expect(nc.cncConnectedNetworks["net-b"].Has("net-c")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks["net-c"].Has("net-b")).To(gomega.BeTrue())
	g.Expect(nc.cncConnectedNetworks["net-a"].Has("net-b")).To(gomega.BeFalse())
	g.Expect(nc.cncsByNetworkID).ToNot(gomega.HaveKey(1))
	g.Expect(nc.cncsByNetworkID[2].Has(cncAB.Name)).To(gomega.BeFalse())
	g.Expect(nc.cncsByNetworkID[2].Has(cncBC.Name)).To(gomega.BeTrue())
}

func TestReconcileCNCsForNetworkIDsTargetsMatchingCNCs(t *testing.T) {
	g := gomega.NewWithT(t)

	reconcileCh := make(chan string, 4)
	cncReconciler := controller.NewController(
		"test-targeted-cnc-reconciler",
		&controller.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				reconcileCh <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(cncReconciler)).To(gomega.Succeed())
	defer controller.Stop(cncReconciler)

	nc := &nadController{
		cncController: cncReconciler,
		cncsByNetworkID: map[int]sets.Set[string]{
			7: sets.New[string]("cnc-match"),
			9: sets.New[string]("cnc-other"),
		},
	}

	nc.reconcileCNCsForNetworkIDs(7)

	g.Eventually(reconcileCh, time.Second).Should(gomega.Receive(gomega.Equal("cnc-match")))
	g.Consistently(reconcileCh, 200*time.Millisecond).ShouldNot(gomega.Receive())
}

func TestOnNetworkRefChangeReconcilesCNCConnectedNetworks(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConfA := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nadA, err := buildNAD("nad-a", "ns1", netConfA)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	reconcileCh := make(chan string, 8)
	nadSyncController := controller.NewController(
		"test-nad-sync-controller",
		&controller.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				reconcileCh <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(nadSyncController)).To(gomega.Succeed())
	defer controller.Stop(nadSyncController)

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 7)).To(gomega.Succeed())

	nc := &nadController{
		controller:         nadSyncController,
		filterNADsOnNode:   "node1",
		networkIDAllocator: networkIDAllocator,
		nadLister: &fakeNADLister{
			nads: map[string]*nettypes.NetworkAttachmentDefinition{
				nadA.Name: nadA,
			},
		},
		networkController: &networkController{
			networks:           map[string]util.MutableNetInfo{},
			networkControllers: map[string]*networkControllerState{},
		},
		nads: map[string]string{
			"ns1/nad-a": "net-a",
			"ns1/nad-b": "net-b",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string]("ns1/nad-a"),
			"net-b": sets.New[string]("ns1/nad-b"),
		},
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-a": sets.New[string]("net-b"),
		},
	}

	nc.OnNetworkRefChange("node1", "ns1/nad-a", true)

	received := sets.New[string]()
	g.Eventually(func() []string {
		for {
			select {
			case key := <-reconcileCh:
				received.Insert(key)
			default:
				return received.UnsortedList()
			}
		}
	}, time.Second).Should(gomega.ConsistOf("ns1/nad-a", "ns1/nad-b"))
}

func TestSyncNADNotFilteredWhenCNCConnectedNetworkActive(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	podTracker := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{
			"node1": {
				"ns1/nad-a": {"pod-a": {}},
			},
		},
	}

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())

	nc := &nadController{
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		nads:               map[string]string{"ns1/nad-a": "net-a"},
		nadsByNetwork:      map[string]sets.Set[string]{"net-a": sets.New[string]("ns1/nad-a")},
		dynamicFilterNADs:  map[string]bool{"ns1/nad-a": true},
		primaryNADs:        map[string]string{},
		networkIDAllocator: networkIDAllocator,
		filterNADsOnNode:   "node1",
		podTracker:         podTracker,
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-b": sets.New[string]("net-a"),
		},
	}

	netConfB := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-b",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRoleSecondary,
		NADName:  "ns1/nad-b",
	}
	nadB, err := buildNADWithAnnotations("nad-b", "ns1", netConfB, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadB.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-b",
		Controller: ptrTo(true),
	}}

	nadKey := util.GetNADName(nadB.Namespace, nadB.Name)
	g.Expect(nc.syncNAD(nadKey, nadB)).To(gomega.Succeed())

	// net-b must be rendered (not filtered) because net-a is active on this
	// node and net-b is CNC-connected to net-a.
	g.Expect(nc.networkController.getNetwork("net-b")).ToNot(gomega.BeNil())
}

func TestSyncNADClearsStaleRemovalMarkWhenDynamicNetworkActive(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	key := "ns1/nad-a"
	podTracker := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{
			"node1": {
				key: {"ns1/pod-a": {}},
			},
		},
	}

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	nc := &nadController{
		networkController:    newNetworkController("test-nad", "", nil, nil, false),
		nads:                 map[string]string{},
		nadsByNetwork:        map[string]sets.Set[string]{},
		dynamicFilterNADs:    map[string]bool{},
		primaryNADs:          map[string]string{},
		networkIDAllocator:   networkIDAllocator,
		filterNADsOnNode:     "node1",
		podTracker:           podTracker,
		markedForRemoval:     map[string]time.Time{key: time.Now().Add(time.Hour)},
		cncConnectedNetworks: map[string]sets.Set[string]{},
	}

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  key,
	}
	nad, err := buildNADWithAnnotations("nad-a", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	g.Expect(nc.syncNAD(key, nad)).To(gomega.Succeed())

	g.Expect(nc.networkController.getNetwork("net-a")).ToNot(gomega.BeNil())
	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func TestSyncNADClearsExpiredRemovalMarkWhenDynamicNetworkActive(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	key := "ns1/nad-a"
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  key,
	}
	nad, err := buildNADWithAnnotations("nad-a", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	nc := &nadController{
		controller:         &controller.FakeController{},
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		nads:               map[string]string{key: "net-a"},
		nadsByNetwork:      map[string]sets.Set[string]{"net-a": sets.New[string](key)},
		dynamicFilterNADs:  map[string]bool{key: true},
		primaryNADs:        map[string]string{"ns1": key},
		networkIDAllocator: networkIDAllocator,
		filterNADsOnNode:   "node1",
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{
				"node1": {
					key: {"ns1/pod-a": {}},
				},
			},
		},
		markedForRemoval:       map[string]time.Time{key: time.Now().Add(-time.Minute)},
		dynamicallyRemovedNADs: sets.New[string](),
		cncConnectedNetworks:   map[string]sets.Set[string]{},
	}

	g.Expect(nc.syncNAD(key, nad)).To(gomega.Succeed())

	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(key))
	g.Expect(nc.dynamicallyRemovedNADs.Has(key)).To(gomega.BeFalse())
	g.Expect(nc.networkController.getNetwork("net-a")).ToNot(gomega.BeNil())
}

func TestSyncNADDoesNotRescheduleAlreadyDynamicallyRemovedNAD(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = time.Hour

	key := "ns1/nad-a"
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  key,
	}
	nad, err := buildNADWithAnnotations("nad-a", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	networkController := newNetworkController("test-nad", "", nil, nil, false)
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(key)
	networkController.setNetwork("net-a", mutableNetInfo)

	nc := &nadController{
		controller:             &controller.FakeController{},
		networkController:      networkController,
		nads:                   map[string]string{key: "net-a"},
		nadsByNetwork:          map[string]sets.Set[string]{"net-a": sets.New[string](key)},
		dynamicFilterNADs:      map[string]bool{key: true},
		primaryNADs:            map[string]string{"ns1": key},
		networkIDAllocator:     networkIDAllocator,
		filterNADsOnNode:       "node1",
		podTracker:             &PodTrackerController{nodeNADToPodCache: map[string]map[string]map[string]struct{}{}},
		markedForRemoval:       map[string]time.Time{key: time.Now().Add(-time.Minute)},
		dynamicallyRemovedNADs: sets.New[string](),
		cncConnectedNetworks:   map[string]sets.Set[string]{},
	}

	g.Expect(nc.syncNAD(key, nad)).To(gomega.Succeed())
	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(key))
	g.Expect(nc.dynamicallyRemovedNADs.Has(key)).To(gomega.BeTrue())
	g.Expect(nc.networkController.getNetwork("net-a")).To(gomega.BeNil())

	g.Expect(nc.syncNAD(key, nad)).To(gomega.Succeed())
	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(key))
	g.Expect(nc.dynamicallyRemovedNADs.Has(key)).To(gomega.BeTrue())
}

// newDynamicRemovalTestController returns a node NAD controller rendering
// network net-a through two primary NADs in different namespaces, with nad-a
// past its inactivity grace period.
func newDynamicRemovalTestController(g *gomega.WithT) (*nadController, string, string, *nettypes.NetworkAttachmentDefinition) {
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = time.Hour

	keyA := "ns1/nad-a"
	keyB := "ns2/nad-a"
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  keyA,
	}
	nadA, err := buildNADWithAnnotations("nad-a", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "ClusterUserDefinedNetwork",
		Name:       "cudn-a",
		Controller: ptrTo(true),
	}}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	networkController := newNetworkController("test-nad", "", nil, nil, false)
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(keyA, keyB)
	networkController.setNetwork("net-a", mutableNetInfo)

	nc := &nadController{
		controller:             &controller.FakeController{},
		networkController:      networkController,
		nads:                   map[string]string{keyA: "net-a", keyB: "net-a"},
		nadsByNetwork:          map[string]sets.Set[string]{"net-a": sets.New[string](keyA, keyB)},
		dynamicFilterNADs:      map[string]bool{keyA: true, keyB: true},
		primaryNADs:            map[string]string{"ns1": keyA, "ns2": keyB},
		networkIDAllocator:     networkIDAllocator,
		filterNADsOnNode:       "node1",
		podTracker:             &PodTrackerController{nodeNADToPodCache: map[string]map[string]map[string]struct{}{}},
		markedForRemoval:       map[string]time.Time{keyA: time.Now().Add(-time.Minute)},
		dynamicallyRemovedNADs: sets.New[string](),
		cncConnectedNetworks:   map[string]sets.Set[string]{},
	}
	return nc, keyA, keyB, nadA
}

func TestSyncNADDynamicRemovalDeletesNetworkWhenOtherNADsAlreadyRemoved(t *testing.T) {
	g := gomega.NewWithT(t)
	nc, keyA, keyB, nadA := newDynamicRemovalTestController(g)
	nc.dynamicallyRemovedNADs.Insert(keyB)

	g.Expect(nc.syncNAD(keyA, nadA)).To(gomega.Succeed())
	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(keyA))
	g.Expect(nc.dynamicallyRemovedNADs.Has(keyA)).To(gomega.BeTrue())
	g.Expect(nc.dynamicallyRemovedNADs.Has(keyB)).To(gomega.BeTrue())
	// Both NADs remain cached to mirror the informer.
	g.Expect(nc.nads).To(gomega.HaveKey(keyA))
	g.Expect(nc.nads).To(gomega.HaveKey(keyB))
	g.Expect(nc.nadsByNetwork["net-a"].UnsortedList()).To(gomega.ConsistOf(keyA, keyB))
	g.Expect(nc.networkController.getNetwork("net-a")).To(gomega.BeNil())
}

func TestSyncNADDynamicRemovalKeepsNetworkWhileOtherNADInGracePeriod(t *testing.T) {
	g := gomega.NewWithT(t)
	nc, keyA, keyB, nadA := newDynamicRemovalTestController(g)
	nc.markedForRemoval[keyB] = time.Now().Add(time.Hour)

	g.Expect(nc.syncNAD(keyA, nadA)).To(gomega.Succeed())
	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(keyA))
	g.Expect(nc.markedForRemoval).To(gomega.HaveKey(keyB))
	g.Expect(nc.dynamicallyRemovedNADs.Has(keyA)).To(gomega.BeTrue())
	network := nc.networkController.getNetwork("net-a")
	g.Expect(network).ToNot(gomega.BeNil())
	g.Expect(network.GetNADNamespaces()).To(gomega.ConsistOf("ns2"))
}

func TestSyncNADDeleteKeepsNetworkIDWhileOtherNADDynamicallyRemoved(t *testing.T) {
	g := gomega.NewWithT(t)
	nc, keyA, keyB, _ := newDynamicRemovalTestController(g)
	delete(nc.markedForRemoval, keyA)
	nc.dynamicallyRemovedNADs.Insert(keyB)

	// nad-a is deleted from the API while nad-b, still in the informer, is
	// only removed locally: the network goes away but keeps its ID for nad-b.
	g.Expect(nc.syncNAD(keyA, nil)).To(gomega.Succeed())
	g.Expect(nc.nads).NotTo(gomega.HaveKey(keyA))
	g.Expect(nc.nadsByNetwork["net-a"].UnsortedList()).To(gomega.ConsistOf(keyB))
	g.Expect(nc.networkController.getNetwork("net-a")).To(gomega.BeNil())
	g.Expect(nc.networkIDAllocator.GetID("net-a")).To(gomega.Equal(1))
}

func TestOnNetworkRefChangeNotifiesNetworkRefReconcilers(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
	}
	nad, err := buildNAD("primary", "ns1", netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn",
		Controller: ptrTo(true),
	}}

	nadSyncController := controller.NewController(
		"test-nad-sync-controller-for-network-ref",
		&controller.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(string) error {
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(nadSyncController)).To(gomega.Succeed())
	defer controller.Stop(nadSyncController)

	nc := &nadController{
		controller:            nadSyncController,
		filterNADsOnNode:      "node1",
		networkIDAllocator:    id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		nadLister:             &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{nad.Name: nad}},
		networkRefReconcilers: map[uint64]networkRefReconcilerRegistration{},
	}
	g.Expect(nc.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	refReconciler := &testNetworkRefReconciler{
		updates: make(chan struct {
			node        string
			networkName string
		}, 1),
	}
	id := nc.RegisterNetworkRefReconciler(refReconciler)
	g.Expect(id).ToNot(gomega.BeZero())

	nc.OnNetworkRefChange("node1", util.GetNADName(nad.Namespace, nad.Name), true)

	g.Eventually(refReconciler.updates, time.Second).Should(gomega.Receive(gomega.Equal(struct {
		node        string
		networkName string
	}{
		node:        "node1",
		networkName: "udn-net",
	})))
}

func TestOnNetworkRefChangeNotifiesConnectedNetworkRefReconcilers(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConfA := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nadA, err := buildNAD("nad-a", "ns1", netConfA)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	nadSyncController := controller.NewController(
		"test-nad-sync-controller-for-connected-network-ref",
		&controller.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(string) error {
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(nadSyncController)).To(gomega.Succeed())
	defer controller.Stop(nadSyncController)

	nc := &nadController{
		controller:            nadSyncController,
		filterNADsOnNode:      "local-node",
		networkController:     newNetworkController("", "", nil, nil, false),
		networkIDAllocator:    id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
		nadLister:             &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{nadA.Name: nadA}},
		networkRefReconcilers: map[uint64]networkRefReconcilerRegistration{},
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-a": sets.New[string]("net-b"),
			"net-b": sets.New[string]("net-a"),
		},
	}
	g.Expect(nc.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	refReconciler := &testNetworkRefReconciler{
		updates: make(chan struct {
			node        string
			networkName string
		}, 2),
	}
	id := nc.RegisterNetworkRefReconciler(refReconciler)
	g.Expect(id).ToNot(gomega.BeZero())

	nc.OnNetworkRefChange("remote-node", util.GetNADName(nadA.Namespace, nadA.Name), true)

	received := sets.New[string]()
	g.Eventually(func() []string {
		for {
			select {
			case update := <-refReconciler.updates:
				g.Expect(update.node).To(gomega.Equal("remote-node"))
				received.Insert(update.networkName)
			default:
				return received.UnsortedList()
			}
		}
	}, time.Second).Should(gomega.ConsistOf("net-a", "net-b"))
}

func TestOnNetworkRefChangeRendersCNCConnectedNetworksWhenNodeBecomesActive(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConfA := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nadA, err := buildNADWithAnnotations("nad-a", "ns1", netConfA, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	netConfB := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-b",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRoleSecondary,
		NADName:  "ns1/nad-b",
	}
	nadB, err := buildNADWithAnnotations("nad-b", "ns1", netConfB, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadB.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-b",
		Controller: ptrTo(true),
	}}

	podTracker := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
	}

	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

	var nc *nadController
	nadSyncController := controller.NewController(
		"test-nad-sync-controller",
		&controller.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				namespace, name, err := cache.SplitMetaNamespaceKey(key)
				if err != nil {
					return err
				}
				nad, err := nc.nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
				if apierrors.IsNotFound(err) {
					nad = nil
					err = nil
				}
				if err != nil {
					return err
				}
				return nc.syncNAD(key, nad)
			},
			Threadiness: 1,
		},
	)
	g.Expect(controller.Start(nadSyncController)).To(gomega.Succeed())
	defer controller.Stop(nadSyncController)

	nc = &nadController{
		controller:         nadSyncController,
		filterNADsOnNode:   "node1",
		podTracker:         podTracker,
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		networkIDAllocator: networkIDAllocator,
		nads:               map[string]string{},
		nadsByNetwork:      map[string]sets.Set[string]{},
		primaryNADs:        map[string]string{},
		nadLister:          &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{"nad-a": nadA, "nad-b": nadB}},
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-a": sets.New[string]("net-b"),
			"net-b": sets.New[string]("net-a"),
		},
	}

	keyA := util.GetNADName(nadA.Namespace, nadA.Name)
	keyB := util.GetNADName(nadB.Namespace, nadB.Name)

	// Initially no local refs: both networks stay filtered.
	g.Expect(nc.syncNAD(keyA, nadA)).To(gomega.Succeed())
	g.Expect(nc.syncNAD(keyB, nadB)).To(gomega.Succeed())
	g.Expect(nc.networkController.getNetwork("net-a")).To(gomega.BeNil())
	g.Expect(nc.networkController.getNetwork("net-b")).To(gomega.BeNil())

	// A local pod on net-a should trigger reconciliation of both A and
	// CNC-connected B, and both should render.
	podTracker.cacheMutex.Lock()
	podTracker.nodeNADToPodCache["node1"] = map[string]map[string]struct{}{
		keyA: {"ns1/pod-a": {}},
	}
	podTracker.cacheMutex.Unlock()

	nc.OnNetworkRefChange("node1", keyA, true)

	g.Eventually(func() bool { return nc.networkController.getNetwork("net-a") != nil }, time.Second).Should(gomega.BeTrue())
	g.Eventually(func() bool { return nc.networkController.getNetwork("net-b") != nil }, time.Second).Should(gomega.BeTrue())
}

func TestOnNetworkRefChangeClearsCNCConnectedRemovalMarks(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConfA := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nadA, err := buildNADWithAnnotations("nad-a", "ns1", netConfA, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	netConfB := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-b",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRoleSecondary,
		NADName:  "ns1/nad-b",
	}
	nadB, err := buildNADWithAnnotations("nad-b", "ns1", netConfB, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadB.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-b",
		Controller: ptrTo(true),
	}}

	keyA := util.GetNADName(nadA.Namespace, nadA.Name)
	keyB := util.GetNADName(nadB.Namespace, nadB.Name)
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	podTracker := &PodTrackerController{
		nodeNADToPodCache: map[string]map[string]map[string]struct{}{
			"node1": {
				keyA: {"ns1/pod-a": {}},
			},
		},
	}
	nc := &nadController{
		controller:         &controller.FakeController{},
		filterNADsOnNode:   "node1",
		podTracker:         podTracker,
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		networkIDAllocator: networkIDAllocator,
		primaryNADs:        map[string]string{},
		nadLister: &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{
			nadA.Name: nadA,
			nadB.Name: nadB,
		}},
		nads: map[string]string{
			keyA: "net-a",
			keyB: "net-b",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](keyA),
			"net-b": sets.New[string](keyB),
		},
		dynamicFilterNADs: map[string]bool{
			keyA: true,
			keyB: true,
		},
		markedForRemoval: map[string]time.Time{
			keyB: time.Now().Add(time.Hour),
		},
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-a": sets.New[string]("net-b"),
			"net-b": sets.New[string]("net-a"),
		},
	}

	nc.OnNetworkRefChange("node1", keyA, true)
	g.Expect(nc.syncNAD(keyB, nadB)).To(gomega.Succeed())

	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(keyB))
}

func TestOnNetworkRefChangeMarksCNCConnectedNetworksInactive(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = time.Hour

	netConfA := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nadA, err := buildNADWithAnnotations("nad-a", "ns1", netConfA, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadA.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	netConfB := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-b",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRoleSecondary,
		NADName:  "ns1/nad-b",
	}
	nadB, err := buildNADWithAnnotations("nad-b", "ns1", netConfB, map[string]string{
		types.OvnNetworkIDAnnotation: "2",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadB.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-b",
		Controller: ptrTo(true),
	}}

	keyA := util.GetNADName(nadA.Namespace, nadA.Name)
	keyB := util.GetNADName(nadB.Namespace, nadB.Name)
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	stopCh := make(chan struct{})
	defer close(stopCh)
	nc := &nadController{
		controller:       &controller.FakeController{},
		filterNADsOnNode: "node1",
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
		},
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		networkIDAllocator: networkIDAllocator,
		primaryNADs:        map[string]string{},
		nadLister: &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{
			nadA.Name: nadA,
			nadB.Name: nadB,
		}},
		nads: map[string]string{
			keyA: "net-a",
			keyB: "net-b",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](keyA),
			"net-b": sets.New[string](keyB),
		},
		dynamicFilterNADs: map[string]bool{
			keyA: true,
			keyB: true,
		},
		markedForRemoval: map[string]time.Time{},
		stopChan:         stopCh,
		cncConnectedNetworks: map[string]sets.Set[string]{
			"net-a": sets.New[string]("net-b"),
			"net-b": sets.New[string]("net-a"),
		},
	}

	nc.OnNetworkRefChange("node1", keyA, false)
	g.Expect(nc.syncNAD(keyA, nadA)).To(gomega.Succeed())
	g.Expect(nc.syncNAD(keyB, nadB)).To(gomega.Succeed())

	g.Expect(nc.markedForRemoval).To(gomega.HaveKey(keyA))
	g.Expect(nc.markedForRemoval).To(gomega.HaveKey(keyB))
}

func TestOnNetworkRefChangeKeepsNADActiveWhenAnotherTrackerStillReferencesIt(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = time.Hour

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net-a",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/nad-a",
	}
	nad, err := buildNADWithAnnotations("nad-a", "ns1", netConf, map[string]string{
		types.OvnNetworkIDAnnotation: "1",
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nad.OwnerReferences = []metav1.OwnerReference{{
		Kind:       "UserDefinedNetwork",
		Name:       "udn-a",
		Controller: ptrTo(true),
	}}

	key := util.GetNADName(nad.Namespace, nad.Name)
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	stopCh := make(chan struct{})
	defer close(stopCh)
	nc := &nadController{
		controller:       &controller.FakeController{},
		filterNADsOnNode: "node1",
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{
				"node1": {
					key: {"ns1/pod-a": {}},
				},
			},
		},
		// The egress tracker has already removed its local ref before calling
		// OnNetworkRefChange(active=false); the pod tracker still keeps this
		// NAD active on the node.
		egressIPTracker: &EgressIPTrackerController{
			cache: map[string]map[string]struct{}{},
		},
		networkController:  newNetworkController("test-nad", "", nil, nil, false),
		networkIDAllocator: networkIDAllocator,
		primaryNADs:        map[string]string{},
		nadLister: &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{
			nad.Name: nad,
		}},
		nads: map[string]string{
			key: "net-a",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](key),
		},
		dynamicFilterNADs: map[string]bool{
			key: true,
		},
		markedForRemoval: map[string]time.Time{
			key: time.Now().Add(time.Hour),
		},
		stopChan:             stopCh,
		cncConnectedNetworks: map[string]sets.Set[string]{},
	}

	nc.OnNetworkRefChange("node1", key, false)
	g.Expect(nc.syncNAD(key, nad)).To(gomega.Succeed())

	g.Expect(nc.markedForRemoval).ToNot(gomega.HaveKey(key))
}

func TestReconcileNetworkActivityRequeuesKnownNADs(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.UDNDeletionGracePeriod = time.Hour

	keyA := "ns1/nad-a"
	keyB := "ns1/nad-b"
	stopCh := make(chan struct{})
	defer close(stopCh)
	nc := &nadController{
		controller:       &controller.FakeController{},
		filterNADsOnNode: "node1",
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{
				"node1": {
					keyA: {"ns1/pod-a": {}},
				},
			},
		},
		nads: map[string]string{
			keyA: "net-a",
			keyB: "net-b",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](keyA),
			"net-b": sets.New[string](keyB),
		},
		dynamicFilterNADs: map[string]bool{
			keyA: true,
			keyB: true,
		},
		markedForRemoval:     map[string]time.Time{},
		stopChan:             stopCh,
		cncConnectedNetworks: map[string]sets.Set[string]{},
	}

	nc.reconcileNetworkActivity([]string{"net-a", "net-b"})

	fakeController := nc.controller.(*controller.FakeController)
	fakeController.Lock()
	reconciles := append([]string(nil), fakeController.Reconciles...)
	fakeController.Unlock()
	g.Expect(reconciles).To(gomega.ContainElements("Reconcile:"+keyA, "Reconcile:"+keyB))
	g.Expect(nc.markedForRemoval).To(gomega.BeEmpty())
}

func TestSyncCNCRequeuesAffectedNetworkNADs(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	keyA := "ns1/nad-a"
	keyB := "ns1/nad-b"
	networkIDAllocator := id.NewIDAllocator("NetworkIDs", types.MaxNetworks)
	g.Expect(networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-a", 1)).To(gomega.Succeed())
	g.Expect(networkIDAllocator.ReserveID("net-b", 2)).To(gomega.Succeed())

	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cnc-ab",
			Annotations: map[string]string{
				"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`,
			},
		},
	}
	fakeController := &controller.FakeController{}
	nc := &nadController{
		controller:         fakeController,
		filterNADsOnNode:   "node1",
		networkIDAllocator: networkIDAllocator,
		podTracker: &PodTrackerController{
			nodeNADToPodCache: map[string]map[string]map[string]struct{}{
				"node1": {
					keyA: {"ns1/pod-a": {}},
				},
			},
		},
		nads: map[string]string{
			keyA: "net-a",
			keyB: "net-b",
		},
		nadsByNetwork: map[string]sets.Set[string]{
			"net-a": sets.New[string](keyA),
			"net-b": sets.New[string](keyB),
		},
		dynamicFilterNADs: map[string]bool{
			keyA: true,
			keyB: true,
		},
		markedForRemoval:     map[string]time.Time{keyB: time.Now().Add(time.Hour)},
		cncConnectedNetworks: map[string]sets.Set[string]{},
		cncLister: &fakeCNCLister{
			cncs: map[string]*networkconnectv1.ClusterNetworkConnect{
				cnc.Name: cnc,
			},
		},
	}

	g.Expect(nc.syncAllCNCs()).To(gomega.Succeed())

	fakeController.Lock()
	reconciles := append([]string(nil), fakeController.Reconciles...)
	fakeController.Unlock()
	g.Expect(reconciles).To(gomega.ContainElements("Reconcile:"+keyA, "Reconcile:"+keyB))
}

func TestOnNetworkRefChangeNotifiesNetworkController(t *testing.T) {
	tests := []struct {
		name                 string
		notifyActive         bool
		nodeHasNetworkActive bool
	}{
		{
			name:                 "active notification with no refs",
			notifyActive:         true,
			nodeHasNetworkActive: false,
		},
		{
			name:                 "inactive notification with refs",
			notifyActive:         false,
			nodeHasNetworkActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

			// Build fake NAD with UDN owner reference and overlay topology.
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "udn-net",
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
				NADName:  "ns1/primary",
			}
			nad, err := ovntesting.BuildNAD("primary", "ns1", netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nad.OwnerReferences = []metav1.OwnerReference{{
				Kind:       "UserDefinedNetwork",
				Name:       "udn",
				Controller: ptrTo(true),
			}}

			nadLister := &fakeNADLister{
				nads: map[string]*nettypes.NetworkAttachmentDefinition{
					"primary": nad,
				},
			}
			nodeName := "node1"

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}

			nc := &nadController{
				nads:                map[string]string{},
				primaryNADs:         map[string]string{},
				networkController:   newNetworkController("", "", tcm, nil, false),
				networkIDAllocator:  id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
				tunnelKeysAllocator: id.NewTunnelKeyAllocator("TunnelKeys"),
				nadLister:           nadLister,
			}
			err = nc.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			nadNetwork, err := util.ParseNADInfo(nad)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			networkName := nadNetwork.GetNetworkName()
			mutableNetInfo := util.NewMutableNetInfo(nadNetwork)
			mutableNetInfo.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
			nc.networkController.setNetwork(networkName, mutableNetInfo)
			nc.networkController.nodeHasNetwork = func(_, _ string) bool { return tt.nodeHasNetworkActive }
			var gotNode string
			var gotActive bool
			var callCount int
			testController := &testNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(nadNetwork),
				tcm:                 tcm,
				handleRefChange: func(node string, active bool) {
					gotNode = node
					gotActive = active
					callCount++
				},
			}
			nc.networkController.networkControllers[networkName] = &networkControllerState{
				controller: testController,
			}

			// Trigger network ref change.
			nc.OnNetworkRefChange(nodeName, util.GetNADName(nad.Namespace, nad.Name), tt.notifyActive)
			err = nc.networkController.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(callCount).To(gomega.Equal(1))
			g.Expect(gotNode).To(gomega.Equal(nodeName))
			g.Expect(gotActive).To(gomega.Equal(tt.nodeHasNetworkActive))
		})
	}
}

func TestOnNetworkRefChangeNotifiesConnectedRemoteNetworkControllers(t *testing.T) {
	tests := []struct {
		name           string
		notifyActive   bool
		directRef      bool
		expectedActive bool
	}{
		{
			name:           "connected networks become active",
			notifyActive:   true,
			directRef:      true,
			expectedActive: true,
		},
		{
			name:           "connected networks become inactive",
			notifyActive:   false,
			directRef:      false,
			expectedActive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true

			keyA := "ns1/nad-a"
			keyB := "ns1/nad-b"
			netConfA := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "net-a",
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
				NADName:  keyA,
			}
			nadA, err := buildNAD("nad-a", "ns1", netConfA)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadA.OwnerReferences = []metav1.OwnerReference{{
				Kind:       "UserDefinedNetwork",
				Name:       "udn-a",
				Controller: ptrTo(true),
			}}
			netConfB := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "net-b",
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRoleSecondary,
				NADName:  keyB,
			}
			nadB, err := buildNAD("nad-b", "ns1", netConfB)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadB.OwnerReferences = []metav1.OwnerReference{{
				Kind:       "UserDefinedNetwork",
				Name:       "udn-b",
				Controller: ptrTo(true),
			}}

			podTracker := &PodTrackerController{
				nodeNADToPodCache: map[string]map[string]map[string]struct{}{},
			}
			if tt.directRef {
				podTracker.nodeNADToPodCache["remote-node"] = map[string]map[string]struct{}{
					keyA: {"ns1/pod-a": {}},
				}
			}

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			networkController := newNetworkController("", "", tcm, nil, false)
			nc := &nadController{
				filterNADsOnNode:   "local-node",
				podTracker:         podTracker,
				nads:               map[string]string{keyA: "net-a", keyB: "net-b"},
				nadsByNetwork:      map[string]sets.Set[string]{"net-a": sets.New[string](keyA), "net-b": sets.New[string](keyB)},
				dynamicFilterNADs:  map[string]bool{keyA: true, keyB: true},
				primaryNADs:        map[string]string{},
				networkController:  networkController,
				networkIDAllocator: id.NewIDAllocator("NetworkIDs", types.MaxNetworks),
				nadLister:          &fakeNADLister{nads: map[string]*nettypes.NetworkAttachmentDefinition{nadA.Name: nadA, nadB.Name: nadB}},
				cncConnectedNetworks: map[string]sets.Set[string]{
					"net-a": sets.New[string]("net-b"),
					"net-b": sets.New[string]("net-a"),
				},
			}
			nc.networkController.nodeHasNetwork = nc.NodeHasNetwork
			g.Expect(nc.networkIDAllocator.ReserveID(types.DefaultNetworkName, types.DefaultNetworkID)).To(gomega.Succeed())

			nadNetworkA, err := util.ParseNADInfo(nadA)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			mutableNetInfoA := util.NewMutableNetInfo(nadNetworkA)
			mutableNetInfoA.SetNADs(keyA)
			nc.networkController.setNetwork("net-a", mutableNetInfoA)

			nadNetworkB, err := util.ParseNADInfo(nadB)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			mutableNetInfoB := util.NewMutableNetInfo(nadNetworkB)
			mutableNetInfoB.SetNADs(keyB)
			nc.networkController.setNetwork("net-b", mutableNetInfoB)

			callCount := map[string]int{}
			gotNode := map[string]string{}
			gotActive := map[string]bool{}
			for _, networkName := range []string{"net-a", "net-b"} {
				networkName := networkName
				nadNetwork := nadNetworkA
				if networkName == "net-b" {
					nadNetwork = nadNetworkB
				}
				nc.networkController.networkControllers[networkName] = &networkControllerState{
					controller: &testNetworkController{
						ReconcilableNetInfo: util.NewReconcilableNetInfo(nadNetwork),
						tcm:                 tcm,
						handleRefChange: func(node string, active bool) {
							callCount[networkName]++
							gotNode[networkName] = node
							gotActive[networkName] = active
						},
					},
				}
			}

			nc.OnNetworkRefChange("remote-node", keyA, tt.notifyActive)
			g.Expect(nc.networkController.syncNetwork("net-a")).To(gomega.Succeed())
			g.Expect(nc.networkController.syncNetwork("net-b")).To(gomega.Succeed())

			for _, networkName := range []string{"net-a", "net-b"} {
				g.Expect(callCount[networkName]).To(gomega.Equal(1), "network %s", networkName)
				g.Expect(gotNode[networkName]).To(gomega.Equal("remote-node"), "network %s", networkName)
				g.Expect(gotActive[networkName]).To(gomega.Equal(tt.expectedActive), "network %s", networkName)
			}
		})
	}
}

type notFoundNamespaceLister struct {
	notFound sets.Set[string]
}

func (l *notFoundNamespaceLister) List(labels.Selector) ([]*corev1.Namespace, error) {
	return nil, nil
}

func (l *notFoundNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	if l.notFound != nil && l.notFound.Has(name) {
		return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), name)
	}
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
		},
	}, nil
}

func TestGetActiveNetworkForNamespace_ReturnsNotFoundWhenNamespaceAbsent(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	c := &nadController{
		namespaceLister: &notFoundNamespaceLister{
			notFound: sets.New("missing-ns"),
		},
	}

	_, err := c.GetActiveNetworkForNamespace("missing-ns")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}
