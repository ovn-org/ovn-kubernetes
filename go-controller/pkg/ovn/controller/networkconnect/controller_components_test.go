// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"context"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Test for cncNeedsUpdate function
func TestCNCNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *networkconnectv1.ClusterNetworkConnect
		newObj   *networkconnectv1.ClusterNetworkConnect
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &networkconnectv1.ClusterNetworkConnect{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &networkconnectv1.ClusterNetworkConnect{},
			newObj:   nil,
			expected: true,
		},
		{
			name: "no changes",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			expected: false,
		},
		{
			name: "connectivity changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork, networkconnectv1.ServiceNetwork},
				},
			},
			expected: true,
		},
		{
			name: "irrelevant annotations changed - should not trigger update",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"key": "value1"},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"key": "value2"},
				},
			},
			expected: false,
		},
		{
			name: "subnet annotation changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24"},"layer3_2":{"ipv4":"192.168.1.0/24"}}`},
				},
			},
			expected: true,
		},
		{
			name: "tunnel key annotation changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/connect-router-tunnel-key": "12345"},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/connect-router-tunnel-key": "67890"},
				},
			},
			expected: true,
		},
		{
			name: "relevant annotations unchanged",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/network-connect-subnet":    `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`,
						"k8s.ovn.org/connect-router-tunnel-key": "12345",
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/network-connect-subnet":    `{"layer3_1":{"ipv4":"192.168.0.0/24"}}`,
						"k8s.ovn.org/connect-router-tunnel-key": "12345",
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors changed - CUDN selector added",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors changed - PUDN selector label changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &crdtypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"env": "dev"},
								},
							},
						},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &crdtypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"env": "prod"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "network selectors unchanged",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []crdtypes.NetworkSelector{
						{
							NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"app": "test"},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cncNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test for nodeNeedsUpdate function
func TestNodeNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *corev1.Node
		newObj   *corev1.Node
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &corev1.Node{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &corev1.Node{},
			newObj:   nil,
			expected: true,
		},
		{
			name: "no changes",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{},
				},
			},
			expected: false,
		},
		{
			name: "legacy zone annotation change is ignored",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{util.OvnNodeZoneName: "zone1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{util.OvnNodeZoneName: "zone2"},
				},
			},
			expected: false,
		},
		{
			name: "node subnet annotation changed",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"10.244.1.0/24"}`},
				},
			},
			expected: true,
		},
		{
			name: "node ID annotation changed",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "2"},
				},
			},
			expected: false,
		},
		{
			name: "node ID annotation update during add time",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"k8s.ovn.org/node-id": "2"},
				},
			},
			expected: true,
		},
		{
			name: "irrelevant annotation changed - should not trigger update",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"some-other-annotation": "value1"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: map[string]string{"some-other-annotation": "value2"},
				},
			},
			expected: false,
		},
		{
			name: "relevant annotations unchanged",
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
						"k8s.ovn.org/node-id":      "1",
					},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
						"k8s.ovn.org/node-id":      "1",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := nodeNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestController_reconcileNode tests that reconcileNode requeues all CNCs
func TestController_reconcileNode(t *testing.T) {
	g := gomega.NewWithT(t)
	setupTestConfig(true, true)

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	// Create test CNCs
	cnc1 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
	}
	cnc2 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc2"},
	}
	_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc1, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc2, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create test node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"k8s.ovn.org/node-subnets": `{"default":"10.244.0.0/24"}`,
			},
		},
	}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(
		context.Background(), node, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Wait for informer caches to sync
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ClusterNetworkConnectInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
	)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	// Track reconciled CNCs
	reconciledCNCs := sets.New[string]()
	reconciledMutex := sync.Mutex{}

	// Create controller with listers from watch factory
	c := &Controller{
		cncLister:  wf.ClusterNetworkConnectInformer().Lister(),
		nodeLister: wf.NodeCoreInformer().Lister(),
	}

	// Create CNC controller with custom reconcile function that tracks calls
	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      wf.ClusterNetworkConnectInformer().Lister().List,
		Reconcile: func(_ context.Context, key string) error {
			reconciledMutex.Lock()
			defer reconciledMutex.Unlock()
			reconciledCNCs.Insert(key)
			return nil
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController("test-cnc-controller", cncCfg)

	err = controllerutil.Start(c.cncController)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controllerutil.Stop(c.cncController)

	// Wait for initial sync, then clear recorded reconciliations
	g.Eventually(func() int {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.Len()
	}).Should(gomega.BeNumerically(">=", 2))
	reconciledMutex.Lock()
	reconciledCNCs = sets.New[string]()
	reconciledMutex.Unlock()

	// Call reconcileNode
	err = c.reconcileNode("node1")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify all CNCs were requeued
	g.Eventually(func() []string {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.UnsortedList()
	}).Should(gomega.ConsistOf("cnc1", "cnc2"))
}

// TestController_syncNAD tests that syncNAD requeues CNCs matching the NAD network ID.
func TestController_syncNAD(t *testing.T) {
	g := gomega.NewWithT(t)
	networkID := 7
	nadKey := "ns1/nad1"
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "net1",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: ovntypes.Layer3Topology,
		Role:     ovntypes.NetworkRolePrimary,
		NADName:  nadKey,
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(nadKey)
	mutableNetInfo.SetNetworkID(networkID)

	cncController := &controllerutil.FakeController{}
	c := &Controller{
		networkManager: &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{
				nadKey: mutableNetInfo,
			},
		},
		cncController: cncController,
	}
	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc1", sets.New(networkID))
	c.updateCNCNetworkIDsLocked("cnc2", sets.New(99))
	c.updateCNCNetworkIDsLocked("cnc3", sets.New(networkID, 99))
	c.Unlock()

	err = c.syncNAD(nadKey)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(cncController.Reconciles).To(gomega.ConsistOf("Reconcile:cnc1", "Reconcile:cnc3"))
}

// TestController_syncNetworkRef tests that syncNetworkRef requeues CNCs matching the network ID.
func TestController_syncNetworkRef(t *testing.T) {
	g := gomega.NewWithT(t)
	networkID := 11
	networkName := "blue-net"
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: networkName,
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: ovntypes.Layer3Topology,
		Role:     ovntypes.NetworkRoleSecondary,
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadNetwork := util.NewMutableNetInfo(netInfo)
	nadNetwork.SetNetworkID(networkID)

	cncController := &controllerutil.FakeController{}
	c := &Controller{
		cncController:  cncController,
		networkManager: &networkmanager.FakeNetworkManager{NADNetworks: map[string]util.NetInfo{"ns1/nad1": nadNetwork}},
	}
	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc1", sets.New(networkID))
	c.updateCNCNetworkIDsLocked("cnc2", sets.New(99))
	c.updateCNCNetworkIDsLocked("cnc3", sets.New(networkID, 99))
	c.Unlock()

	err = c.syncNetworkRef(networkRefKey("node1", networkName))
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(cncController.Reconciles).To(gomega.ConsistOf("Reconcile:cnc1", "Reconcile:cnc3"))

	err = c.syncNetworkRef(networkRefKey("node1", "missing-net"))
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(cncController.Reconciles).To(gomega.ConsistOf("Reconcile:cnc1", "Reconcile:cnc3"))
}

type filteredNADNetworkManager struct {
	networkmanager.FakeNetworkManager
	nadNetworkNames map[string]string
}

func (fnm *filteredNADNetworkManager) GetNetInfoForNADKey(string) util.NetInfo {
	return nil
}

func (fnm *filteredNADNetworkManager) GetNetworkNameForNADKey(nadKey string) string {
	return fnm.nadNetworkNames[nadKey]
}

func TestController_syncNADRequeuesCNCForFilteredNAD(t *testing.T) {
	g := gomega.NewWithT(t)
	networkID := 11
	networkName := "blue-net"
	nadKey := "ns1/nad1"
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: networkName,
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: ovntypes.Layer3Topology,
		Role:     ovntypes.NetworkRoleSecondary,
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	nadNetwork := util.NewMutableNetInfo(netInfo)
	nadNetwork.SetNetworkID(networkID)

	cncController := &controllerutil.FakeController{}
	c := &Controller{
		cncController: cncController,
		networkManager: &filteredNADNetworkManager{
			FakeNetworkManager: networkmanager.FakeNetworkManager{
				NADNetworks: map[string]util.NetInfo{nadKey: nadNetwork},
			},
			nadNetworkNames: map[string]string{nadKey: networkName},
		},
	}
	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc1", sets.New(networkID))
	c.updateCNCNetworkIDsLocked("cnc2", sets.New(99))
	c.Unlock()

	err = c.syncNAD(nadKey)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(cncController.Reconciles).To(gomega.ConsistOf("Reconcile:cnc1"))
}

func TestController_UpdateCNCNetworkIDsLocked(t *testing.T) {
	g := gomega.NewWithT(t)

	c := &Controller{}
	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc1", sets.New(7, 8))
	c.updateCNCNetworkIDsLocked("cnc2", sets.New(8))
	c.Unlock()
	g.Expect(c.cncNetworkIDs["cnc1"]).To(gomega.Equal(sets.New(7, 8)))
	g.Expect(c.cncsByNetworkID[7]).To(gomega.Equal(sets.New("cnc1")))
	g.Expect(c.cncsByNetworkID[8]).To(gomega.Equal(sets.New("cnc1", "cnc2")))

	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc1", sets.New(9))
	c.Unlock()
	g.Expect(c.cncNetworkIDs["cnc1"]).To(gomega.Equal(sets.New(9)))
	g.Expect(c.cncsByNetworkID).ToNot(gomega.HaveKey(7))
	g.Expect(c.cncsByNetworkID[8]).To(gomega.Equal(sets.New("cnc2")))
	g.Expect(c.cncsByNetworkID[9]).To(gomega.Equal(sets.New("cnc1")))

	c.Lock()
	c.updateCNCNetworkIDsLocked("cnc2", nil)
	c.Unlock()
	g.Expect(c.cncNetworkIDs).ToNot(gomega.HaveKey("cnc2"))
	g.Expect(c.cncsByNetworkID).ToNot(gomega.HaveKey(8))
}

// Test for serviceNeedsUpdate function
func TestServiceNeedsUpdate(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   *corev1.Service
		newObj   *corev1.Service
		expected bool
	}{
		{
			name:     "create event (oldObj nil)",
			oldObj:   nil,
			newObj:   &corev1.Service{},
			expected: true,
		},
		{
			name:     "delete event (newObj nil)",
			oldObj:   &corev1.Service{},
			newObj:   nil,
			expected: false,
		},
		{
			name:     "both nil",
			oldObj:   nil,
			newObj:   nil,
			expected: false,
		},
		{
			name: "update event - should not trigger",
			oldObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
			},
			newObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
					Labels:    map[string]string{"app": "test"},
				},
			},
			expected: false,
		},
		{
			name: "irrelevant spec change (ClusterIP) - should not trigger",
			oldObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
				},
			},
			newObj: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc1",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.2",
				},
			},
			expected: false,
		},
		{
			name: "new protocol added - should trigger",
			oldObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: corev1.ProtocolTCP},
					},
				},
			},
			newObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: corev1.ProtocolTCP},
						{Port: 53, Protocol: corev1.ProtocolUDP},
					},
				},
			},
			expected: true,
		},
		{
			name: "protocol removed - should trigger",
			oldObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: corev1.ProtocolTCP},
						{Port: 53, Protocol: corev1.ProtocolUDP},
					},
				},
			},
			newObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expected: true,
		},
		{
			name: "same protocols different ports - should not trigger",
			oldObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 80, Protocol: corev1.ProtocolTCP},
					},
				},
			},
			newObj: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.1",
					Ports: []corev1.ServicePort{
						{Port: 8080, Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := serviceNeedsUpdate(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test for mustProcessCNCForService function
func TestMustProcessCNCForService(t *testing.T) {
	tests := []struct {
		name            string
		cncCache        map[string]*networkConnectState
		cnc             *networkconnectv1.ClusterNetworkConnect
		svc             *corev1.Service
		networkOwnerKey string
		expected        bool
	}{
		{
			name:     "CNC without ServiceNetwork - should return false",
			cncCache: map[string]*networkConnectState{},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name:     "CNC with ServiceNetwork but not in cache - should return false",
			cncCache: map[string]*networkConnectState{},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "CNC in cache but network not connected - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer3_2", "layer3_3"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "CNC in cache and network is connected - should return true",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer3_1", "layer3_2"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        true,
		},
		{
			name: "CNC with multiple connectivity types including ServiceNetwork - should return true",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New("layer2_5"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{
						networkconnectv1.PodNetwork,
						networkconnectv1.ServiceNetwork,
					},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "ns1"},
			},
			networkOwnerKey: "layer2_5",
			expected:        true,
		},
		{
			name: "Different CNC in cache - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc2": {
					name:              "cnc2",
					tunnelID:          67890,
					connectedNetworks: sets.New("layer3_1"),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
		{
			name: "Empty connected networks set - should return false",
			cncCache: map[string]*networkConnectState{
				"cnc1": {
					name:              "cnc1",
					tunnelID:          12345,
					connectedNetworks: sets.New[string](),
				},
			},
			cnc: &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			svc: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			},
			networkOwnerKey: "layer3_1",
			expected:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{
				cncCache: tt.cncCache,
			}
			result := c.mustProcessCNCForService(tt.svc, tt.cnc, tt.networkOwnerKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestController_reconcileService tests that reconcileService requeues matching CNCs
func TestController_reconcileService(t *testing.T) {
	g := gomega.NewWithT(t)
	setupTestConfig(true, true)

	// Create NetInfo for the UDN network (layer3, ID=1)
	netInfo, err := createNetInfo(testNetwork{
		name:         "udn1",
		id:           1,
		topologyType: ovntypes.Layer3Topology,
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	// Create test CNCs - cnc1 has ServiceNetwork and connects layer3_1
	// cnc2 has ServiceNetwork but connects different network
	// cnc3 has only PodNetwork
	cnc1 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
		},
	}
	cnc2 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc2"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
		},
	}
	cnc3 := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: "cnc3"},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
		},
	}
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc1, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc2, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
		context.Background(), cnc3, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Create test service in namespace "ns1"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc1",
			Namespace: "ns1",
		},
	}
	_, err = fakeClientset.KubeClient.CoreV1().Services("ns1").Create(
		context.Background(), svc, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	// Wait for informer caches to sync
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ClusterNetworkConnectInformer().Informer().HasSynced,
		wf.ServiceCoreInformer().Informer().HasSynced,
	)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	// Track reconciled CNCs
	reconciledCNCs := sets.New[string]()
	reconciledMutex := sync.Mutex{}

	// Create FakeNetworkManager - namespace "ns1" uses UDN network
	fakeNetworkManager := &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			"ns1": netInfo, // ns1 uses the UDN network (layer3_1)
		},
	}

	// Create controller with listers and network manager
	c := &Controller{
		cncLister:      wf.ClusterNetworkConnectInformer().Lister(),
		serviceLister:  wf.ServiceCoreInformer().Lister(),
		networkManager: fakeNetworkManager,
		cncCache: map[string]*networkConnectState{
			"cnc1": {
				name:              "cnc1",
				tunnelID:          12345,
				connectedNetworks: sets.New("layer3_1", "layer3_2"), // connects layer3_1
			},
			"cnc2": {
				name:              "cnc2",
				tunnelID:          67890,
				connectedNetworks: sets.New("layer3_3", "layer3_4"), // doesn't connect layer3_1
			},
			"cnc3": {
				name:              "cnc3",
				tunnelID:          11111,
				connectedNetworks: sets.New("layer3_1"), // connects layer3_1 but no ServiceNetwork
			},
		},
	}

	// Create CNC controller with custom reconcile function that tracks calls
	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    wf.ClusterNetworkConnectInformer().Informer(),
		Lister:      wf.ClusterNetworkConnectInformer().Lister().List,
		Reconcile: func(_ context.Context, key string) error {
			reconciledMutex.Lock()
			defer reconciledMutex.Unlock()
			reconciledCNCs.Insert(key)
			return nil
		},
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController("test-cnc-controller", cncCfg)

	err = controllerutil.Start(c.cncController)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controllerutil.Stop(c.cncController)

	// Wait for initial sync, then clear recorded reconciliations
	g.Eventually(func() int {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.Len()
	}).Should(gomega.BeNumerically(">=", 3))
	reconciledMutex.Lock()
	reconciledCNCs = sets.New[string]()
	reconciledMutex.Unlock()

	// Call reconcileService
	err = c.reconcileService("ns1/svc1")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Verify only cnc1 was requeued (has ServiceNetwork AND connects layer3_1)
	// cnc2 doesn't connect layer3_1, cnc3 has only PodNetwork
	g.Eventually(func() []string {
		reconciledMutex.Lock()
		defer reconciledMutex.Unlock()
		return reconciledCNCs.UnsortedList()
	}).Should(gomega.ConsistOf("cnc1"))
}

func TestController_reconcileService_ReturnsErrorOnNamespaceNotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	setupTestConfig(true, true)

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "ns1"},
	}
	_, err := fakeClientset.KubeClient.CoreV1().Services("ns1").Create(
		context.Background(), svc, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	g.Expect(cache.WaitForCacheSync(
		syncCtx.Done(),
		wf.ServiceCoreInformer().Informer().HasSynced,
	)).To(gomega.BeTrue())

	fakeNetworkManager := &networkmanager.FakeNetworkManager{
		NotFoundNamespaces: sets.New[string]("ns1"),
	}

	c := &Controller{
		cncLister:      wf.ClusterNetworkConnectInformer().Lister(),
		serviceLister:  wf.ServiceCoreInformer().Lister(),
		networkManager: fakeNetworkManager,
		cncCache:       map[string]*networkConnectState{},
	}

	err = c.reconcileService("ns1/svc1")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}
