// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NOTE: This file tests the elements of the networkconnect controller
// in a modular fashion. It is not a comprehensive test of the full controller.
// It focuses on testing the individual functions of the controller.

// makeCUDNOwnerRef creates an owner reference for a ClusterUserDefinedNetwork
func makeCUDNOwnerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: userdefinednetworkv1.SchemeGroupVersion.String(),
		Kind:       "ClusterUserDefinedNetwork",
		Name:       name,
		Controller: func() *bool { b := true; return &b }(),
	}
}

// makeUDNOwnerRef creates an owner reference for a UserDefinedNetwork
func makeUDNOwnerRef(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: userdefinednetworkv1.SchemeGroupVersion.String(),
		Kind:       "UserDefinedNetwork",
		Name:       name,
		Controller: func() *bool { b := true; return &b }(),
	}
}

// testCNC is a helper to build ClusterNetworkConnect objects for testing
type testCNC struct {
	Name             string
	NetworkSelectors []apitypes.NetworkSelector
	ConnectSubnets   []networkconnectv1.ConnectSubnet
	Connectivity     []networkconnectv1.ConnectivityType
}

func (tc testCNC) ClusterNetworkConnect() *networkconnectv1.ClusterNetworkConnect {
	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name: tc.Name,
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			NetworkSelectors: tc.NetworkSelectors,
			ConnectSubnets:   tc.ConnectSubnets,
			Connectivity:     tc.Connectivity,
		},
	}
	if len(tc.ConnectSubnets) == 0 {
		cnc.Spec.ConnectSubnets = []networkconnectv1.ConnectSubnet{
			{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			{CIDR: "fd00:10:244::/112", NetworkPrefix: 120}, // matches ipv4 /24: 32-24=8, 128-8=120
		}
	}
	if len(tc.Connectivity) == 0 {
		cnc.Spec.Connectivity = []networkconnectv1.ConnectivityType{
			networkconnectv1.PodNetwork,
		}
	}
	return cnc
}

// testNAD is a helper to build NetworkAttachmentDefinition objects for testing
type testNAD struct {
	Name        string
	Namespace   string
	Network     string
	Labels      map[string]string
	Annotations map[string]string
	// IsCUDN indicates if this NAD is owned by a ClusterUserDefinedNetwork
	IsCUDN bool
	// IsUDN indicates if this NAD is owned by a UserDefinedNetwork
	IsUDN bool
	// IsPrimary indicates if this is a primary network
	IsPrimary bool
	// Topology is the network topology (layer3 or layer2)
	Topology string
	// Subnet is the subnet CIDR for the network
	Subnet string
	// NetworkID is the OVN network ID annotation value
	NetworkID string
}

func (tn testNAD) NAD() *nadv1.NetworkAttachmentDefinition {
	if tn.Annotations == nil {
		tn.Annotations = map[string]string{}
	}
	tn.Annotations[types.OvnNetworkNameAnnotation] = tn.Network
	if tn.NetworkID != "" {
		tn.Annotations[types.OvnNetworkIDAnnotation] = tn.NetworkID
	}

	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tn.Name,
			Namespace:   tn.Namespace,
			Labels:      tn.Labels,
			Annotations: tn.Annotations,
		},
	}

	// Set owner reference for CUDN
	if tn.IsCUDN {
		ownerRef := makeCUDNOwnerRef(tn.Network)
		nad.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ownerRef}
	}

	// Set owner reference for UDN
	if tn.IsUDN {
		ownerRef := makeUDNOwnerRef(tn.Name)
		nad.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ownerRef}
	}

	// Build NAD spec config
	topology := tn.Topology
	if topology == "" {
		topology = types.Layer3Topology
	}
	role := ""
	if tn.IsPrimary {
		role = ", \"role\": \"primary\""
	}
	subnet := ""
	if tn.Subnet != "" {
		subnet = fmt.Sprintf(", \"subnets\": \"%s\"", tn.Subnet)
	}

	nad.Spec.Config = fmt.Sprintf(
		"{\"cniVersion\": \"0.4.0\", \"name\": \"%s\", \"type\": \"%s\", \"topology\": \"%s\", \"netAttachDefName\": \"%s/%s\"%s%s}",
		tn.Network,
		config.CNI.Plugin,
		topology,
		tn.Namespace,
		tn.Name,
		role,
		subnet,
	)

	return nad
}

// testNamespace is a helper to build Namespace objects for testing
type testNamespace struct {
	Name   string
	Labels map[string]string
	// RequiresUDN indicates this namespace requires a UDN but doesn't have one yet.
	// This simulates the error condition where GetActiveNetworkForNamespace returns an error.
	RequiresUDN bool
}

func (tn testNamespace) Namespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   tn.Name,
			Labels: tn.Labels,
		},
	}
}

func TestController_reconcileClusterNetworkConnect(t *testing.T) {
	tests := []struct {
		// name is the test case name
		name string
		// cnc is the ClusterNetworkConnect object to create and reconcile
		cnc *testCNC
		// nads is the list of NetworkAttachmentDefinitions to create.
		// NADs with IsUDN=true and IsPrimary=true will auto-populate FakeNetworkManager.PrimaryNetworks.
		nads []*testNAD
		// namespaces is the list of Namespaces to create (used for Primary UDN selector tests).
		// Namespaces with RequiresUDN=true but no matching UDN NAD will trigger GetActiveNetworkForNamespace error.
		namespaces []*testNamespace
		// reconcile is the CNC name to reconcile
		reconcile string
		// wantErr indicates if reconciliation should return an error
		wantErr bool
		// expectSelectedNADs is the list of NAD keys expected to be selected
		expectSelectedNADs []string
		// expectSelectedNetworks is the list of network names expected to be selected
		expectSelectedNetworks []string
		// expectTunnelIDAllocated indicates if a tunnel ID should be allocated
		expectTunnelIDAllocated bool
		// expectSubnetsAllocated indicates if subnets should be allocated
		expectSubnetsAllocated bool
		// expectCacheEntryExists indicates if a cache entry should exist after reconciliation
		expectCacheEntryExists bool
		// expectCacheEntryDeleted indicates if the cache entry should be deleted (for CNC deletion tests)
		expectCacheEntryDeleted bool
		// notFoundNamespaces makes FakeNetworkManager return NotFound for these namespaces.
		notFoundNamespaces []string
	}{
		// Primary CUDN owned NAD selection tests
		{
			name: "creates cache entry and allocates tunnel ID and subnets for new CNC with CUDN selector",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-red",
					Namespace: "red",
					Network:   util.GenerateCUDNNetworkName("red"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.0.0.0/16",
					Labels:    map[string]string{"selected": "true"},
					NetworkID: "1",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"red/cudn-red"},
			expectSelectedNetworks:  []string{"layer3_1"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		{
			name: "selects multiple CUDNs matching label selector",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "test"},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-blue",
					Namespace: "blue",
					Network:   util.GenerateCUDNNetworkName("blue"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.1.0.0/16",
					Labels:    map[string]string{"env": "test"},
					NetworkID: "2",
				},
				{
					Name:      "cudn-green",
					Namespace: "green",
					Network:   util.GenerateCUDNNetworkName("green"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer2Topology,
					Subnet:    "10.2.0.0/16",
					Labels:    map[string]string{"env": "test"},
					NetworkID: "3",
				},
				{
					Name:      "cudn-yellow",
					Namespace: "yellow",
					Network:   util.GenerateCUDNNetworkName("yellow"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.3.0.0/16",
					Labels:    map[string]string{"env": "prod"}, // not selected
					NetworkID: "4",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"blue/cudn-blue", "green/cudn-green"},
			expectSelectedNetworks:  []string{"layer3_2", "layer2_3"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		{
			name: "ignores non-CUDN NADs even if labels match",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "regular-nad",
					Namespace: "test",
					Network:   "regular-network",
					IsCUDN:    false, // not a CUDN
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Labels:    map[string]string{"selected": "true"},
					NetworkID: "5",
				},
			},
			reconcile:              "test-cnc",
			expectSelectedNADs:     []string{},
			expectSelectedNetworks: []string{},
			expectCacheEntryExists: true,
		},
		{
			name: "ignores secondary network NADs",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "secondary-cudn",
					Namespace: "test",
					Network:   util.GenerateCUDNNetworkName("secondary"),
					IsCUDN:    true,
					IsPrimary: false, // secondary, not primary
					Topology:  types.Layer3Topology,
					Labels:    map[string]string{"selected": "true"},
					NetworkID: "6",
				},
			},
			reconcile:              "test-cnc",
			expectSelectedNADs:     []string{},
			expectSelectedNetworks: []string{},
			expectCacheEntryExists: true,
		},
		{
			name:                    "deletes cache entry when CNC is deleted",
			cnc:                     nil, // CNC doesn't exist
			reconcile:               "deleted-cnc",
			expectCacheEntryDeleted: true,
		},
		{
			name: "handles layer2 topology networks",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-layer2",
					Namespace: "layer2ns",
					Network:   util.GenerateCUDNNetworkName("layer2net"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer2Topology,
					Subnet:    "10.5.0.0/16",
					Labels:    map[string]string{"selected": "true"},
					NetworkID: "7",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"layer2ns/cudn-layer2"},
			expectSelectedNetworks:  []string{"layer2_7"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		// Primary UDN selector tests
		{
			name: "selects primary UDN by namespace selector",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"udn": "enabled"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "ns1", Labels: map[string]string{"udn": "enabled"}},
				{Name: "ns2", Labels: map[string]string{"udn": "disabled"}},
			},
			nads: []*testNAD{
				{
					Name:      "primary-udn",
					Namespace: "ns1",
					Network:   "ns1-primary-udn",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.10.0.0/16",
					NetworkID: "10",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"ns1/primary-udn"},
			expectSelectedNetworks:  []string{"layer3_10"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		{
			name: "selects multiple primary UDNs from multiple namespaces",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "frontend-a", Labels: map[string]string{"tier": "frontend"}},
				{Name: "frontend-b", Labels: map[string]string{"tier": "frontend"}},
				{Name: "backend", Labels: map[string]string{"tier": "backend"}},
			},
			nads: []*testNAD{
				{
					Name:      "udn-a",
					Namespace: "frontend-a",
					Network:   "frontend-a-udn",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.20.0.0/16",
					NetworkID: "20",
				},
				{
					Name:      "udn-b",
					Namespace: "frontend-b",
					Network:   "frontend-b-udn",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.21.0.0/16",
					NetworkID: "21",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"frontend-a/udn-a", "frontend-b/udn-b"},
			expectSelectedNetworks:  []string{"layer3_20", "layer3_21"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		{
			name: "skips namespace with default network (no primary UDN)",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "ns-with-udn", Labels: map[string]string{"selected": "true"}},
				{Name: "ns-default", Labels: map[string]string{"selected": "true"}}, // no primary UDN configured
			},
			nads: []*testNAD{
				{
					Name:      "primary-udn",
					Namespace: "ns-with-udn",
					Network:   "ns-with-udn-network",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.30.0.0/16",
					NetworkID: "30",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"ns-with-udn/primary-udn"},
			expectSelectedNetworks:  []string{"layer3_30"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		// Combo selector tests
		{
			name: "selects both CUDN and primary UDN with combo selector",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"type": "cudn"},
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"type": "udn"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "cudn-ns", Labels: map[string]string{"type": "cudn"}},
				{Name: "udn-ns", Labels: map[string]string{"type": "udn"}},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-nad",
					Namespace: "cudn-ns",
					Network:   util.GenerateCUDNNetworkName("my-cudn"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.40.0.0/16",
					Labels:    map[string]string{"type": "cudn"},
					NetworkID: "40",
				},
				{
					Name:      "udn-nad",
					Namespace: "udn-ns",
					Network:   "udn-ns-network",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.41.0.0/16",
					NetworkID: "41",
				},
			},
			reconcile:               "test-cnc",
			expectSelectedNADs:      []string{"cudn-ns/cudn-nad", "udn-ns/udn-nad"},
			expectSelectedNetworks:  []string{"layer3_40", "layer3_41"},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  true,
			expectCacheEntryExists:  true,
		},
		// Graceful handling tests - these used to error but now skip gracefully
		{
			name: "skips namespace when UDN was deleted (InvalidPrimaryNetworkError) and continues reconciliation",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"requires-udn": "true"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "pending-ns", Labels: map[string]string{"requires-udn": "true"}, RequiresUDN: true},
			},
			nads:      []*testNAD{},
			reconcile: "test-cnc",
			// No error - we gracefully skip namespaces with deleted UDNs so subnet release can proceed
			wantErr:                 false,
			expectSelectedNADs:      []string{},
			expectSelectedNetworks:  []string{},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  false, // no subnets allocated since no networks matched
			expectCacheEntryExists:  true,
		},
		{
			name: "skips namespace when namespace is absent from informer cache (NotFound) and continues reconciliation",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"requires-udn": "true"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "pending-ns", Labels: map[string]string{"requires-udn": "true"}},
			},
			notFoundNamespaces:      []string{"pending-ns"},
			nads:                    []*testNAD{},
			reconcile:               "test-cnc",
			wantErr:                 false,
			expectSelectedNADs:      []string{},
			expectSelectedNetworks:  []string{},
			expectTunnelIDAllocated: true,
			expectSubnetsAllocated:  false,
			expectCacheEntryExists:  true,
		},
		// Error condition tests
		{
			name: "errors when more than 1 primary NAD is found for namespace",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"multi-nad": "true"},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "multi-nad-ns", Labels: map[string]string{"multi-nad": "true"}},
			},
			nads: []*testNAD{
				{
					Name:      "primary-udn-1",
					Namespace: "multi-nad-ns",
					Network:   "multi-nad-ns-network",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.50.0.0/16",
					NetworkID: "50",
				},
				{
					Name:      "primary-udn-2",
					Namespace: "multi-nad-ns",
					Network:   "multi-nad-ns-network-2",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Subnet:    "10.51.0.0/16",
					NetworkID: "51",
				},
			},
			// Multiple NADs with IsUDN=true for same namespace - auto-triggers error
			reconcile: "test-cnc",
			wantErr:   true,
		},
		{
			name: "errors on unsupported network selection type",
			cnc: &testCNC{
				Name: "test-cnc",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: "UnsupportedType",
					},
				},
			},
			reconcile: "test-cnc",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

			// Create test CNC
			if tt.cnc != nil {
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), tt.cnc.ClusterNetworkConnect(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, nad := range tt.nads {
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(
					context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, ns := range tt.namespaces {
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns.Namespace(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Wait for informer caches to sync
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer syncCancel()
			synced := cache.WaitForCacheSync(
				syncCtx.Done(),
				wf.NADInformer().Informer().HasSynced,
				wf.ClusterNetworkConnectInformer().Informer().HasSynced,
				wf.NamespaceInformer().Informer().HasSynced,
			)
			g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

			// Create fake network manager and auto-configure from nads and namespaces
			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
				NADNetworks:     make(map[string]util.NetInfo),
			}

			// Auto-populate PrimaryNetworks from NADs with IsUDN=true and IsPrimary=true
			// Group NADs by namespace for the FakeNetworkManager
			nadsByNamespace := make(map[string][]*testNAD)
			for _, nad := range tt.nads {
				if nad.IsUDN && nad.IsPrimary {
					nadsByNamespace[nad.Namespace] = append(nadsByNamespace[nad.Namespace], nad)
				}
			}
			for namespace, nads := range nadsByNamespace {
				// Use the first NAD to create the NetInfo
				firstNAD := nads[0]
				nadKey := fmt.Sprintf("%s/%s", firstNAD.Namespace, firstNAD.Name)
				nad, err := wf.NADInformer().Lister().NetworkAttachmentDefinitions(namespace).Get(firstNAD.Name)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should exist", nadKey)
				netInfo, err := util.ParseNADInfo(nad)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should be parseable", nadKey)
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				// Add all NAD keys to the NetInfo
				for _, n := range nads {
					mutableNetInfo.AddNADs(fmt.Sprintf("%s/%s", n.Namespace, n.Name))
				}
				fakeNM.Lock()
				fakeNM.PrimaryNetworks[namespace] = mutableNetInfo
				fakeNM.Unlock()
			}
			for _, nad := range tt.nads {
				nadKey := fmt.Sprintf("%s/%s", nad.Namespace, nad.Name)
				nadObj, err := wf.NADInformer().Lister().NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should exist", nadKey)
				netInfo, err := util.ParseNADInfo(nadObj)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "ParseNADInfo for %s failed", nadKey)
				fakeNM.NADNetworks[nadKey] = netInfo
			}

			// Auto-configure UDN namespaces from namespaces with RequiresUDN=true
			for _, ns := range tt.namespaces {
				if ns.RequiresUDN {
					if fakeNM.UDNNamespaces == nil {
						fakeNM.UDNNamespaces = sets.New[string]()
					}
					fakeNM.UDNNamespaces.Insert(ns.Name)
				}
			}
			if len(tt.notFoundNamespaces) > 0 {
				fakeNM.NotFoundNamespaces = sets.New(tt.notFoundNamespaces...)
			}

			tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")

			c := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

			// Pre-populate cache for deletion test
			if tt.expectCacheEntryDeleted {
				c.cncCache[tt.reconcile] = &clusterNetworkConnectState{
					name:             tt.reconcile,
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
					tunnelID:         12345,
				}
			}

			// Run reconciliation
			err = c.reconcileClusterNetworkConnect(context.Background(), tt.reconcile)
			if tt.wantErr {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Verify cache state
			if tt.expectCacheEntryDeleted {
				_, exists := c.cncCache[tt.reconcile]
				g.Expect(exists).To(gomega.BeFalse(), "cache entry should be deleted")
				return
			}

			if tt.expectCacheEntryExists {
				cncState, exists := c.cncCache[tt.reconcile]
				g.Expect(exists).To(gomega.BeTrue(), "cache entry should exist")

				// Verify selected NADs
				g.Expect(cncState.selectedNADs.UnsortedList()).To(gomega.ConsistOf(tt.expectSelectedNADs))

				// Verify selected networks
				g.Expect(cncState.selectedNetworks.UnsortedList()).To(gomega.ConsistOf(tt.expectSelectedNetworks))

				// Fetch the updated CNC for annotation verification
				updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
					context.Background(), tt.reconcile, metav1.GetOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Verify tunnel ID allocation and annotation
				if tt.expectTunnelIDAllocated && len(tt.expectSelectedNADs) > 0 {
					g.Expect(cncState.tunnelID).ToNot(gomega.BeZero(), "tunnel ID should be allocated")
					tunnelKeyAnnotation, ok := updatedCNC.Annotations[util.OvnConnectRouterTunnelKeyAnnotation]
					g.Expect(ok).To(gomega.BeTrue(), "tunnel key annotation should exist")
					g.Expect(tunnelKeyAnnotation).To(gomega.Equal(fmt.Sprintf("%d", cncState.tunnelID)),
						"tunnel key annotation should match cache")
				}

				// Verify subnet allocation annotation
				if tt.expectSubnetsAllocated && len(tt.expectSelectedNetworks) > 0 {
					subnetAnnotation, ok := updatedCNC.Annotations["k8s.ovn.org/network-connect-subnet"]
					g.Expect(ok).To(gomega.BeTrue(), "subnet annotation should exist")
					var subnetsMap map[string]util.NetworkConnectSubnetAnnotation
					err = json.Unmarshal([]byte(subnetAnnotation), &subnetsMap)
					g.Expect(err).ToNot(gomega.HaveOccurred(), "subnet annotation should be valid JSON")
					// Verify that the number of subnet entries matches the expected selected networks
					// The annotation uses owner keys (e.g., "layer3_1", "layer2_2") not network names
					g.Expect(subnetsMap).To(gomega.HaveLen(len(tt.expectSelectedNetworks)),
						"number of subnet entries should match expected networks")
					// Verify each entry has both IPv4 and IPv6 subnets (since both modes are enabled)
					for owner, subnetEntry := range subnetsMap {
						g.Expect(subnetEntry.IPv4).ToNot(gomega.BeEmpty(),
							"subnet entry %s should have IPv4 address", owner)
						g.Expect(subnetEntry.IPv6).ToNot(gomega.BeEmpty(),
							"subnet entry %s should have IPv6 address", owner)
					}
				}
			}
		})
	}
}

func TestCNCNeedsUpdate(t *testing.T) {
	tests := []struct {
		name       string
		oldObj     *networkconnectv1.ClusterNetworkConnect
		newObj     *networkconnectv1.ClusterNetworkConnect
		wantUpdate bool
	}{
		{
			name:       "CNC is being created",
			oldObj:     nil,
			newObj:     &networkconnectv1.ClusterNetworkConnect{},
			wantUpdate: true,
		},
		{
			name:       "CNC is being deleted",
			oldObj:     &networkconnectv1.ClusterNetworkConnect{},
			newObj:     nil,
			wantUpdate: true,
		},
		{
			name: "NetworkSelectors changed",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.ClusterUserDefinedNetworks},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks},
					},
				},
			},
			wantUpdate: true,
		},
		{
			name: "NetworkSelectors unchanged",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.ClusterUserDefinedNetworks},
					},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.ClusterUserDefinedNetworks},
					},
				},
			},
			wantUpdate: false,
		},
		{
			name: "Connectivity changed (should not trigger update)",
			oldObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
				},
			},
			newObj: &networkconnectv1.ClusterNetworkConnect{
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.ServiceNetwork},
				},
			},
			wantUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			result := cncNeedsUpdate(tt.oldObj, tt.newObj)
			g.Expect(result).To(gomega.Equal(tt.wantUpdate))
		})
	}
}

func TestController_reconcileNAD(t *testing.T) {
	tests := []struct {
		name                string
		cncs                []*testCNC
		nads                []*testNAD
		prePopulateCache    map[string]*clusterNetworkConnectState
		reconcileNAD        string
		expectCNCReconciled []string
		expectNoReconcile   bool
	}{
		{
			name: "NAD creation triggers CNC reconciliation if it matches CNC selector",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"selected": "true"},
								},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-test",
					Namespace: "test",
					Network:   util.GenerateCUDNNetworkName("test"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Labels:    map[string]string{"selected": "true"},
					NetworkID: "1",
				},
			},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNAD:        "test/cudn-test",
			expectCNCReconciled: []string{"cnc1"},
		},
		{
			name: "NAD matching only one of two CNCs triggers reconciliation for only that CNC",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"selected": "true"},
								},
							},
						},
					},
				},
				{
					Name: "cnc2",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"other": "label"},
								},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-test",
					Namespace: "test",
					Network:   util.GenerateCUDNNetworkName("test"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Labels:    map[string]string{"selected": "true"}, // matches cnc1, not cnc2
					NetworkID: "1",
				},
			},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
				"cnc2": {
					name:             "cnc2",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNAD:        "test/cudn-test",
			expectCNCReconciled: []string{"cnc1"},
		},
		{
			name: "NAD not matching (and previously not matching) any CNC selector does not trigger reconciliation",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"selected": "true"},
								},
							},
						},
					},
				},
			},
			nads: []*testNAD{
				{
					Name:      "cudn-test",
					Namespace: "test",
					Network:   util.GenerateCUDNNetworkName("test"),
					IsCUDN:    true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					Labels:    map[string]string{"selected": "false"}, // doesn't match
					NetworkID: "1",
				},
			},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNAD:      "test/cudn-test",
			expectNoReconcile: true,
		},
		{
			name: "NAD deletion triggers CNC reconciliation if it was previously selected",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"selected": "true"},
								},
							},
						},
					},
				},
			},
			nads: []*testNAD{}, // NAD is deleted
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New("test/cudn-test"), // was selected before
					selectedNetworks: sets.New("layer3_1"),
				},
			},
			reconcileNAD:        "test/cudn-test",
			expectCNCReconciled: []string{"cnc1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.IPv4Mode = true
			config.IPv6Mode = false
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

			// Create test CNCs
			for _, cnc := range tt.cncs {
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc.ClusterNetworkConnect(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// Create test NADs
			for _, nad := range tt.nads {
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(
					context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Wait for informer caches to sync
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer syncCancel()
			synced := cache.WaitForCacheSync(
				syncCtx.Done(),
				wf.NADInformer().Informer().HasSynced,
				wf.ClusterNetworkConnectInformer().Informer().HasSynced,
			)
			g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
				NADNetworks:     make(map[string]util.NetInfo),
			}

			for _, nad := range tt.nads {
				nadKey := fmt.Sprintf("%s/%s", nad.Namespace, nad.Name)
				nadObj, err := wf.NADInformer().Lister().NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should exist", nadKey)
				netInfo, err := util.ParseNADInfo(nadObj)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "ParseNADInfo for %s failed", nadKey)
				fakeNM.NADNetworks[nadKey] = netInfo
			}

			tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
			c := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

			// Pre-populate cache
			for name, state := range tt.prePopulateCache {
				c.cncCache[name] = state
			}

			// Track reconciled CNCs using a set to handle duplicate reconciles
			reconciledCNCs := sets.New[string]()
			reconciledMutex := sync.Mutex{}

			// Replace the CNC controller with a mock that tracks reconciliations
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
			c.cncController = controllerutil.NewController(
				"test-cnc-controller",
				cncCfg,
			)

			err = controllerutil.Start(c.cncController)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer controllerutil.Stop(c.cncController)

			// Wait for initial controller sync to reconcile all CNCs,
			// then clear the recorded reconciliations.
			// Post this its correct to check if reconcileNAD added it
			// back or didn't i.e if we called reconcileClusterNetworkConnect
			// or not.
			g.Eventually(func() int {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				return reconciledCNCs.Len()
			}).Should(gomega.BeNumerically(">=", len(tt.cncs)))
			reconciledMutex.Lock()
			reconciledCNCs = sets.New[string]()
			reconciledMutex.Unlock()

			// Run NAD reconciliation
			err = c.reconcileNAD(context.Background(), tt.reconcileNAD)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Allow time for async reconciliation
			if tt.expectNoReconcile {
				g.Consistently(func() []string {
					reconciledMutex.Lock()
					defer reconciledMutex.Unlock()
					return reconciledCNCs.UnsortedList()
				}).Should(gomega.BeEmpty())
			} else {
				g.Eventually(func() []string {
					reconciledMutex.Lock()
					defer reconciledMutex.Unlock()
					return reconciledCNCs.UnsortedList()
				}).Should(gomega.ConsistOf(tt.expectCNCReconciled))
			}
		})
	}
}

func TestMustProcessCNCForNAD(t *testing.T) {
	tests := []struct {
		// name is the test case name
		name string
		// cnc is the ClusterNetworkConnect to test against
		cnc *testCNC
		// nad is the NAD being processed that may or may not match the CNC selector
		nad *testNAD
		// namespaces is the list of namespaces to create (needed for PrimaryUserDefinedNetworks selector tests)
		namespaces []testNamespace
		// cncCacheState is the pre-existing CNC cache state (nil means CNC not in cache)
		cncCacheState *clusterNetworkConnectState
		// mustProcessCNC is the expected result of mustProcessCNCForNAD
		mustProcessCNC bool
		// notFoundNamespaces makes FakeNetworkManager return NotFound for these namespaces.
		notFoundNamespaces []string
	}{
		{
			name: "Primary CUDN owned NAD starts matching but CNC cache doesn't exist (CNC not created yet) - should NOT process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "cudn-test",
				Namespace: "test",
				Network:   util.GenerateCUDNNetworkName("test"),
				IsCUDN:    true,
				IsPrimary: true,
				Labels:    map[string]string{"selected": "true"},
			},
			cncCacheState:  nil, // CNC not in cache yet which means cncCreate has not happened yet
			mustProcessCNC: false,
		},
		{
			name: "Primary CUDN owned NAD starts matching - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "cudn-test",
				Namespace: "test",
				Network:   util.GenerateCUDNNetworkName("test"),
				IsCUDN:    true,
				IsPrimary: true,
				Labels:    map[string]string{"selected": "true"},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New[string](), // empty - NAD not selected before
				selectedNetworks: sets.New[string](),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary CUDN owned NAD stops matching - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "cudn-test",
				Namespace: "test",
				Network:   util.GenerateCUDNNetworkName("test"),
				IsCUDN:    true,
				IsPrimary: true,
				Labels:    map[string]string{"selected": "false"}, // no longer matches
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New("test/cudn-test"), // was selected before
				selectedNetworks: sets.New("layer3_1"),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary CUDN owned NAD still matches - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "cudn-test",
				Namespace: "test",
				Network:   util.GenerateCUDNNetworkName("test"),
				IsCUDN:    true,
				IsPrimary: true,
				Labels:    map[string]string{"selected": "true"},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New("test/cudn-test"), // already selected
				selectedNetworks: sets.New("layer3_1"),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary CUDN owned NAD still doesn't match - should NOT process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "cudn-test",
				Namespace: "test",
				Network:   util.GenerateCUDNNetworkName("test"),
				IsCUDN:    true,
				IsPrimary: true,
				Labels:    map[string]string{"selected": "false"},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New[string](), // not selected before either
				selectedNetworks: sets.New[string](),
			},
			mustProcessCNC: false,
		},
		// PrimaryUserDefinedNetworks selector tests
		{
			name: "Primary UDN owned NAD starts matching but CNC cache doesn't exist - should NOT process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "true"}},
			},
			cncCacheState:  nil, // CNC not in cache yet
			mustProcessCNC: false,
		},
		{
			name: "Primary UDN owned NAD starts matching - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "true"}},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New[string](), // empty - NAD not selected before
				selectedNetworks: sets.New[string](),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary UDN owned NAD stops matching (namespace labels changed) - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "false"}}, // namespace no longer matches
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New("test/udn-test"), // was selected before
				selectedNetworks: sets.New("layer3_1"),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary UDN owned NAD still matches - should process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "true"}},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New("test/udn-test"), // already selected
				selectedNetworks: sets.New("layer3_1"),
			},
			mustProcessCNC: true,
		},
		{
			name: "Primary UDN owned NAD still doesn't match - should NOT process CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "false"}}, // namespace doesn't match
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New[string](), // not selected before either
				selectedNetworks: sets.New[string](),
			},
			mustProcessCNC: false,
		},
		{
			name: "Primary UDN NAD cannot match when namespace active network lookup returns NotFound",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			nad: &testNAD{
				Name:      "udn-test",
				Namespace: "test",
				Network:   util.GenerateUDNNetworkName("test", "udn-test"),
				IsUDN:     true,
				IsPrimary: true,
			},
			namespaces: []testNamespace{
				{Name: "test", Labels: map[string]string{"selected": "true"}},
			},
			cncCacheState: &clusterNetworkConnectState{
				name:             "cnc1",
				selectedNADs:     sets.New[string](),
				selectedNetworks: sets.New[string](),
			},
			notFoundNamespaces: []string{"test"},
			mustProcessCNC:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = true
			config.IPv6Mode = false
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()

			// Create namespaces if provided (for PrimaryUserDefinedNetworks tests)
			for _, ns := range tt.namespaces {
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(),
					ns.Namespace(),
					metav1.CreateOptions{},
				)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
				NADNetworks:     make(map[string]util.NetInfo),
			}

			// Auto-configure primary network from NAD when IsUDN && IsPrimary
			if tt.nad != nil && tt.nad.IsUDN && tt.nad.IsPrimary {
				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
					NetConf:  cnitypes.NetConf{Name: tt.nad.Network},
					Topology: types.Layer3Topology,
					Role:     types.NetworkRolePrimary,
				})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.SetNADs(tt.nad.Namespace + "/" + tt.nad.Name)
				fakeNM.Lock()
				fakeNM.PrimaryNetworks[tt.nad.Namespace] = mutableNetInfo
				fakeNM.Unlock()
			}
			if tt.nad != nil {
				nadKey := fmt.Sprintf("%s/%s", tt.nad.Namespace, tt.nad.Name)
				nadObj := tt.nad.NAD()
				netInfo, err := util.ParseNADInfo(nadObj)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "ParseNADInfo for %s failed", nadKey)
				fakeNM.NADNetworks[nadKey] = netInfo
			}
			if len(tt.notFoundNamespaces) > 0 {
				fakeNM.NotFoundNamespaces = sets.New(tt.notFoundNamespaces...)
			}

			tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
			c := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

			// Pre-populate cache if provided
			if tt.cncCacheState != nil {
				c.cncCache[tt.cnc.Name] = tt.cncCacheState
			}

			var nad *nadv1.NetworkAttachmentDefinition
			if tt.nad != nil {
				nad = tt.nad.NAD()
			}

			cnc := tt.cnc.ClusterNetworkConnect()
			nadKey := ""
			if tt.nad != nil {
				nadKey = tt.nad.Namespace + "/" + tt.nad.Name
			}

			result := c.mustProcessCNCForNAD(nad, cnc, nadKey)
			g.Expect(result).To(gomega.Equal(tt.mustProcessCNC))
		})
	}
}

func TestNamespaceNeedsUpdate(t *testing.T) {
	tests := []struct {
		name       string
		oldObj     *corev1.Namespace
		newObj     *corev1.Namespace
		wantUpdate bool
	}{
		{
			name:       "namespace is being created (oldObj nil)",
			oldObj:     nil,
			newObj:     &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}},
			wantUpdate: false,
		},
		{
			name:       "namespace is being deleted (newObj nil)",
			oldObj:     &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}},
			newObj:     nil,
			wantUpdate: false,
		},
		{
			name: "namespace without UDN label - labels changed",
			oldObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{"old": "label"},
				},
			},
			newObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{"new": "label"},
				},
			},
			wantUpdate: false, // no UDN label, so we don't care
		},
		{
			name: "namespace with UDN label - labels changed",
			oldObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
				},
			},
			newObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{types.RequiredUDNNamespaceLabel: "", "new": "label"},
				},
			},
			wantUpdate: true,
		},
		{
			name: "namespace with UDN label - labels unchanged",
			oldObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{types.RequiredUDNNamespaceLabel: "", "same": "label"},
				},
			},
			newObj: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test",
					Labels: map[string]string{types.RequiredUDNNamespaceLabel: "", "same": "label"},
				},
			},
			wantUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			result := namespaceNeedsUpdate(tt.oldObj, tt.newObj)
			g.Expect(result).To(gomega.Equal(tt.wantUpdate))
		})
	}
}

func TestMustProcessCNCForNamespace(t *testing.T) {
	tests := []struct {
		name           string
		cnc            *testCNC
		namespace      *testNamespace
		primaryNAD     string
		cncCache       map[string]*clusterNetworkConnectState
		mustProcessCNC bool
	}{
		{
			name: "CNC cache doesn't exist - should NOT process",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespace:      &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "true"}},
			primaryNAD:     "test-ns/primary-udn",
			cncCache:       nil,
			mustProcessCNC: false,
		},
		{
			name: "namespace starts matching CNC - should process",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "true"}},
			primaryNAD: "test-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: true,
		},
		{
			name: "namespace stops matching CNC - should process",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "false"}},
			primaryNAD: "test-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New("test-ns/primary-udn"), // was previously selected
					selectedNetworks: sets.New("layer3_1"),
				},
			},
			mustProcessCNC: true,
		},
		{
			name: "namespace continues to match CNC - should NOT process",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "true"}},
			primaryNAD: "test-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New("test-ns/primary-udn"),
					selectedNetworks: sets.New("layer3_1"),
				},
			},
			mustProcessCNC: false, // state unchanged
		},
		{
			name: "namespace continues to NOT match CNC - should NOT process",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"selected": "true"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "false"}},
			primaryNAD: "test-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: false, // state unchanged
		},
		{
			name: "CNC with CUDN selector ignores namespace changes",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
						ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
							NetworkSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"type": "cudn"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "test-ns", Labels: map[string]string{"selected": "true"}},
			primaryNAD: "test-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: false, // CUDN selector doesn't care about namespace labels
		},
		// Multiple CNC tests
		{
			name: "multiple CNCs in cache - only checks the specific CNC",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "frontend-ns", Labels: map[string]string{"tier": "frontend"}},
			primaryNAD: "frontend-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](), // not selected before
					selectedNetworks: sets.New[string](),
				},
				"cnc2": {
					name:             "cnc2",
					selectedNADs:     sets.New("frontend-ns/primary-udn"), // cnc2 already selected this NAD
					selectedNetworks: sets.New("layer3_1"),
				},
			},
			mustProcessCNC: true, // cnc1 state changed (started matching)
		},
		{
			name: "multiple CNCs - CNC not in cache while others exist",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "frontend-ns", Labels: map[string]string{"tier": "frontend"}},
			primaryNAD: "frontend-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				// cnc1 not in cache, but cnc2 is
				"cnc2": {
					name:             "cnc2",
					selectedNADs:     sets.New("frontend-ns/primary-udn"),
					selectedNetworks: sets.New("layer3_1"),
				},
			},
			mustProcessCNC: false, // cnc1 not in cache, so don't process
		},
		// Multiple selectors tests - verify OR semantics across selectors
		{
			name: "multiple PUDN selectors - namespace matches first selector only (OR semantics)",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"}, // namespace matches THIS one
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "backend"}, // namespace does NOT match this one
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "frontend-ns", Labels: map[string]string{"tier": "frontend"}},
			primaryNAD: "frontend-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](), // not selected before
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: true, // should process because namespace matches FIRST selector (OR semantics)
		},
		{
			name: "multiple PUDN selectors - namespace matches second selector only (OR semantics)",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"}, // namespace does NOT match this one
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "backend"}, // namespace matches THIS one
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "backend-ns", Labels: map[string]string{"tier": "backend"}},
			primaryNAD: "backend-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](), // not selected before
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: true, // should process because namespace matches SECOND selector (OR semantics)
		},
		{
			name: "multiple PUDN selectors - namespace matches neither (no state change)",
			cnc: &testCNC{
				Name: "cnc1",
				NetworkSelectors: []apitypes.NetworkSelector{
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "frontend"},
							},
						},
					},
					{
						NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
						PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
							NamespaceSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "backend"},
							},
						},
					},
				},
			},
			namespace:  &testNamespace{Name: "other-ns", Labels: map[string]string{"tier": "database"}},
			primaryNAD: "other-ns/primary-udn",
			cncCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](), // not selected before
					selectedNetworks: sets.New[string](),
				},
			},
			mustProcessCNC: false, // no state change (was not selected, still not selected)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			c := &Controller{
				cncCache: make(map[string]*clusterNetworkConnectState),
			}

			// Populate cache with all entries
			for name, state := range tt.cncCache {
				c.cncCache[name] = state
			}

			cnc := tt.cnc.ClusterNetworkConnect()
			namespace := tt.namespace.Namespace()

			result := c.mustProcessCNCForNamespace(cnc, namespace, tt.primaryNAD)
			g.Expect(result).To(gomega.Equal(tt.mustProcessCNC))
		})
	}
}

func TestController_reconcileNamespace(t *testing.T) {
	tests := []struct {
		name                string
		cncs                []*testCNC
		nads                []*testNAD
		namespaces          []*testNamespace
		prePopulateCache    map[string]*clusterNetworkConnectState
		reconcileNamespace  string
		expectCNCReconciled []string
		expectNoReconcile   bool
	}{
		{
			name: "namespace label change triggers CNC reconciliation for matching Primary UDN selector",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"tier": "frontend"},
								},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "frontend-ns", Labels: map[string]string{"tier": "frontend"}},
			},
			nads: []*testNAD{
				{
					Name:      "primary-udn",
					Namespace: "frontend-ns",
					Network:   "frontend-ns-network",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					NetworkID: "1",
				},
			},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](), // not selected before
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNamespace:  "frontend-ns",
			expectCNCReconciled: []string{"cnc1"},
		},
		{
			name: "namespace with default network does not trigger CNC reconciliation",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"tier": "frontend"},
								},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "default-ns", Labels: map[string]string{"tier": "frontend"}},
			},
			nads: []*testNAD{}, // no UDN NADs
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNamespace: "default-ns",
			expectNoReconcile:  true,
		},
		{
			name: "namespace matching one of two CNCs triggers only that CNC",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"tier": "frontend"},
								},
							},
						},
					},
				},
				{
					Name: "cnc2",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"tier": "backend"},
								},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{
				{Name: "frontend-ns", Labels: map[string]string{"tier": "frontend"}},
			},
			nads: []*testNAD{
				{
					Name:      "primary-udn",
					Namespace: "frontend-ns",
					Network:   "frontend-ns-network",
					IsUDN:     true,
					IsPrimary: true,
					Topology:  types.Layer3Topology,
					NetworkID: "1",
				},
			},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
				"cnc2": {
					name:             "cnc2",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNamespace:  "frontend-ns",
			expectCNCReconciled: []string{"cnc1"}, // only cnc1 matches
		},
		{
			name: "deleted namespace does not panic and does not trigger CNC reconciliation",
			cncs: []*testCNC{
				{
					Name: "cnc1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{
							NetworkSelectionType: apitypes.PrimaryUserDefinedNetworks,
							PrimaryUserDefinedNetworkSelector: &apitypes.PrimaryUserDefinedNetworkSelector{
								NamespaceSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"tier": "frontend"},
								},
							},
						},
					},
				},
			},
			namespaces: []*testNamespace{}, // namespace does not exist (deleted)
			nads:       []*testNAD{},
			prePopulateCache: map[string]*clusterNetworkConnectState{
				"cnc1": {
					name:             "cnc1",
					selectedNADs:     sets.New[string](),
					selectedNetworks: sets.New[string](),
				},
			},
			reconcileNamespace: "deleted-namespace", // namespace that doesn't exist
			expectNoReconcile:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

			// Create test CNCs
			for _, cnc := range tt.cncs {
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc.ClusterNetworkConnect(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// Create test NADs
			for _, nad := range tt.nads {
				_, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(
					context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// Create test namespaces
			for _, ns := range tt.namespaces {
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(
					context.Background(), ns.Namespace(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// Wait for informer caches to sync
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer syncCancel()
			synced := cache.WaitForCacheSync(
				syncCtx.Done(),
				wf.NADInformer().Informer().HasSynced,
				wf.ClusterNetworkConnectInformer().Informer().HasSynced,
				wf.NamespaceInformer().Informer().HasSynced,
			)
			g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

			// Create fake network manager and auto-configure from nads
			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
				NADNetworks:     make(map[string]util.NetInfo),
			}

			// Auto-populate PrimaryNetworks from NADs with IsUDN=true and IsPrimary=true
			nadsByNamespace := make(map[string][]*testNAD)
			for _, nad := range tt.nads {
				if nad.IsUDN && nad.IsPrimary {
					nadsByNamespace[nad.Namespace] = append(nadsByNamespace[nad.Namespace], nad)
				}
			}
			for namespace, nads := range nadsByNamespace {
				firstNAD := nads[0]
				nadKey := fmt.Sprintf("%s/%s", firstNAD.Namespace, firstNAD.Name)
				nad, err := wf.NADInformer().Lister().NetworkAttachmentDefinitions(namespace).Get(firstNAD.Name)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should exist", nadKey)
				netInfo, err := util.ParseNADInfo(nad)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should be parseable", nadKey)
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				for _, n := range nads {
					mutableNetInfo.AddNADs(fmt.Sprintf("%s/%s", n.Namespace, n.Name))
				}
				fakeNM.Lock()
				fakeNM.PrimaryNetworks[namespace] = mutableNetInfo
				fakeNM.Unlock()
			}
			for _, nad := range tt.nads {
				nadKey := fmt.Sprintf("%s/%s", nad.Namespace, nad.Name)
				nadObj, err := wf.NADInformer().Lister().NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "NAD %s should exist", nadKey)
				netInfo, err := util.ParseNADInfo(nadObj)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "ParseNADInfo for %s failed", nadKey)
				fakeNM.NADNetworks[nadKey] = netInfo
			}

			tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
			c := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

			// Pre-populate cache
			for name, state := range tt.prePopulateCache {
				c.cncCache[name] = state
			}

			// Track reconciled CNCs using a set to handle duplicate reconciles
			reconciledCNCs := sets.New[string]()
			reconciledMutex := sync.Mutex{}

			// Replace the CNC controller with a mock that tracks reconciliations
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
			c.cncController = controllerutil.NewController(
				"test-cnc-controller",
				cncCfg,
			)

			err = controllerutil.Start(c.cncController)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer controllerutil.Stop(c.cncController)

			// Wait for initial controller sync to reconcile all CNCs,
			// then clear the recorded reconciliations.
			g.Eventually(func() int {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				return reconciledCNCs.Len()
			}).Should(gomega.BeNumerically(">=", len(tt.cncs)))
			reconciledMutex.Lock()
			reconciledCNCs = sets.New[string]()
			reconciledMutex.Unlock()

			// Run namespace reconciliation
			err = c.reconcileNamespace(context.Background(), tt.reconcileNamespace)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Allow time for async reconciliation
			if tt.expectNoReconcile {
				g.Consistently(func() []string {
					reconciledMutex.Lock()
					defer reconciledMutex.Unlock()
					return reconciledCNCs.UnsortedList()
				}).Should(gomega.BeEmpty())
			} else {
				g.Eventually(func() []string {
					reconciledMutex.Lock()
					defer reconciledMutex.Unlock()
					return reconciledCNCs.UnsortedList()
				}).Should(gomega.ConsistOf(tt.expectCNCReconciled))
			}
		})
	}
}

// expectedCNCCacheState represents the expected state of a CNC cache entry after initialSync
type expectedCNCCacheState struct {
	tunnelID         int
	selectedNetworks []string
}

// expectedSubnetAllocation represents an expected subnet allocation for verification
type expectedSubnetAllocation struct {
	owner    string
	topology string // types.Layer3Topology or types.Layer2Topology
	ipv4     string // expected IPv4 subnet CIDR
	ipv6     string // expected IPv6 subnet CIDR (optional)
}

// TestController_initialSync tests that initialSync correctly restores allocator state from CNC annotations.
// It verifies that tunnel keys and subnet allocations are restored, and that re-allocating the same owner
// returns the exact same subnet (idempotency).
func TestController_initialSync(t *testing.T) {
	tests := []struct {
		name string
		// existingCNCs are CNC objects that exist before initialSync (with annotations set)
		existingCNCs []*networkconnectv1.ClusterNetworkConnect
		// expectCacheEntries maps CNC name to expected cache state after initialSync
		expectCacheEntries map[string]expectedCNCCacheState
		// verifyAllocations verifies that re-allocating the same owner returns exact same subnets
		// Maps CNC name to list of expected allocations to verify
		verifyAllocations map[string][]expectedSubnetAllocation
	}{
		{
			name: "restores single CNC with layer3 subnets",
			existingCNCs: []*networkconnectv1.ClusterNetworkConnect{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc1",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "5",
							// IPv6 /120 blocks within fd00:10:244::/112 range (256 /120 blocks available)
							"k8s.ovn.org/network-connect-subnet": `{"layer3_1":{"ipv4":"192.168.0.0/24","ipv6":"fd00:10:244::/120"},"layer3_2":{"ipv4":"192.168.1.0/24","ipv6":"fd00:10:244::100/120"}}`,
						},
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
							{CIDR: "fd00:10:244::/112", NetworkPrefix: 120},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
			},
			expectCacheEntries: map[string]expectedCNCCacheState{
				"cnc1": {tunnelID: 5, selectedNetworks: []string{"layer3_1", "layer3_2"}},
			},
			verifyAllocations: map[string][]expectedSubnetAllocation{
				"cnc1": {
					{owner: "layer3_1", topology: types.Layer3Topology, ipv4: "192.168.0.0/24", ipv6: "fd00:10:244::/120"},
					{owner: "layer3_2", topology: types.Layer3Topology, ipv4: "192.168.1.0/24", ipv6: "fd00:10:244::100/120"},
				},
			},
		},
		{
			name: "restores multiple CNCs with different subnets",
			existingCNCs: []*networkconnectv1.ClusterNetworkConnect{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc1",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "1",
							// IPv6 /120 block within fd00:10:244::/112 range
							"k8s.ovn.org/network-connect-subnet": `{"layer3_10":{"ipv4":"192.168.0.0/24","ipv6":"fd00:10:244::/120"}}`,
						},
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
							{CIDR: "fd00:10:244::/112", NetworkPrefix: 120},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc2",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "2",
							"k8s.ovn.org/network-connect-subnet":     `{"layer3_20":{"ipv4":"10.100.0.0/24"},"layer3_21":{"ipv4":"10.100.1.0/24"}}`,
						},
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "10.100.0.0/16", NetworkPrefix: 24},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
			},
			expectCacheEntries: map[string]expectedCNCCacheState{
				"cnc1": {tunnelID: 1, selectedNetworks: []string{"layer3_10"}},
				"cnc2": {tunnelID: 2, selectedNetworks: []string{"layer3_20", "layer3_21"}},
			},
			verifyAllocations: map[string][]expectedSubnetAllocation{
				"cnc1": {
					{owner: "layer3_10", topology: types.Layer3Topology, ipv4: "192.168.0.0/24", ipv6: "fd00:10:244::/120"},
				},
				"cnc2": {
					{owner: "layer3_20", topology: types.Layer3Topology, ipv4: "10.100.0.0/24"},
					{owner: "layer3_21", topology: types.Layer3Topology, ipv4: "10.100.1.0/24"},
				},
			},
		},
		{
			name: "restores CNC with layer2 subnets and pool blocks",
			existingCNCs: []*networkconnectv1.ClusterNetworkConnect{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc-layer2",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "10",
							"k8s.ovn.org/network-connect-subnet":     `{"layer2_100":{"ipv4":"192.168.0.0/31","ipv6":"fd00:10:244::/127"},"layer2_101":{"ipv4":"192.168.0.2/31","ipv6":"fd00:10:244::2/127"}}`,
						},
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
							{CIDR: "fd00:10:244::/112", NetworkPrefix: 120},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
			},
			expectCacheEntries: map[string]expectedCNCCacheState{
				"cnc-layer2": {tunnelID: 10, selectedNetworks: []string{"layer2_100", "layer2_101"}},
			},
			verifyAllocations: map[string][]expectedSubnetAllocation{
				"cnc-layer2": {
					{owner: "layer2_100", topology: types.Layer2Topology, ipv4: "192.168.0.0/31", ipv6: "fd00:10:244::/127"},
					{owner: "layer2_101", topology: types.Layer2Topology, ipv4: "192.168.0.2/31", ipv6: "fd00:10:244::2/127"},
				},
			},
		},
		{
			name: "restores CNC with mixed layer3 and layer2 subnets",
			existingCNCs: []*networkconnectv1.ClusterNetworkConnect{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc-mixed",
						Annotations: map[string]string{
							util.OvnConnectRouterTunnelKeyAnnotation: "7",
							// Layer3 gets /120 block, Layer2 gets /127 subnet within a /120 block
							// IPv6 addresses must be within fd00:10:244::/112 range
							"k8s.ovn.org/network-connect-subnet": `{"layer3_5":{"ipv4":"192.168.0.0/24","ipv6":"fd00:10:244::/120"},"layer2_6":{"ipv4":"192.168.1.0/31","ipv6":"fd00:10:244::100/127"}}`,
						},
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
							{CIDR: "fd00:10:244::/112", NetworkPrefix: 120},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
			},
			expectCacheEntries: map[string]expectedCNCCacheState{
				"cnc-mixed": {tunnelID: 7, selectedNetworks: []string{"layer3_5", "layer2_6"}},
			},
			verifyAllocations: map[string][]expectedSubnetAllocation{
				"cnc-mixed": {
					{owner: "layer3_5", topology: types.Layer3Topology, ipv4: "192.168.0.0/24", ipv6: "fd00:10:244::/120"},
					{owner: "layer2_6", topology: types.Layer2Topology, ipv4: "192.168.1.0/31", ipv6: "fd00:10:244::100/127"},
				},
			},
		},
		{
			name: "handles CNC with empty annotations gracefully",
			existingCNCs: []*networkconnectv1.ClusterNetworkConnect{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cnc-empty",
					},
					Spec: networkconnectv1.ClusterNetworkConnectSpec{
						ConnectSubnets: []networkconnectv1.ConnectSubnet{
							{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
						},
						Connectivity: []networkconnectv1.ConnectivityType{networkconnectv1.PodNetwork},
					},
				},
			},
			expectCacheEntries: map[string]expectedCNCCacheState{
				"cnc-empty": {tunnelID: 0, selectedNetworks: []string{}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			ovntest.AddNetworkConnectApplyReactor(fakeClientset.NetworkConnectClient.(*networkconnectfake.Clientset))

			// Create existing CNCs (simulating state from before restart)
			for _, cnc := range tt.existingCNCs {
				_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
					context.Background(), cnc, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
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
			)
			g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
			}

			tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")
			c := NewController(wf, fakeClientset, fakeNM.Interface(), tunnelKeysAllocator)

			// Run initialSync
			err = c.initialSync()
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// Verify cache entries
			for cncName, expected := range tt.expectCacheEntries {
				cncState, exists := c.cncCache[cncName]
				g.Expect(exists).To(gomega.BeTrue(), "cache entry for %s should exist", cncName)

				// Verify tunnel ID
				g.Expect(cncState.tunnelID).To(gomega.Equal(expected.tunnelID),
					"tunnel ID for %s should match", cncName)

				// Verify selected networks
				g.Expect(cncState.selectedNetworks.UnsortedList()).To(gomega.ConsistOf(expected.selectedNetworks),
					"selected networks for %s should match", cncName)

				// Verify allocator was populated (if there are subnets)
				if len(expected.selectedNetworks) > 0 && cncState.allocator != nil {
					// The allocator should have the ranges configured
					// We verify this indirectly by checking that it exists and was set up
					g.Expect(cncState.allocator).ToNot(gomega.BeNil(),
						"allocator for %s should be initialized", cncName)
				}
			}

			// Verify that re-allocating the same owner returns exact same subnets (idempotency)
			for cncName, allocations := range tt.verifyAllocations {
				cncState := c.cncCache[cncName]
				g.Expect(cncState).ToNot(gomega.BeNil(), "cache entry for %s should exist", cncName)
				g.Expect(cncState.allocator).ToNot(gomega.BeNil(), "allocator for %s should exist", cncName)

				for _, expected := range allocations {
					var allocatedSubnets []*net.IPNet
					var err error

					if expected.topology == types.Layer3Topology {
						allocatedSubnets, err = cncState.allocator.AllocateLayer3Subnet(expected.owner)
					} else {
						allocatedSubnets, err = cncState.allocator.AllocateLayer2Subnet(expected.owner)
					}
					g.Expect(err).ToNot(gomega.HaveOccurred(),
						"re-allocation for owner %s should succeed", expected.owner)
					g.Expect(allocatedSubnets).ToNot(gomega.BeEmpty(),
						"re-allocation for owner %s should return subnets", expected.owner)

					// Build map of allocated subnets by type for comparison
					allocatedByType := make(map[string]string) // "ipv4" or "ipv6" -> CIDR
					for _, subnet := range allocatedSubnets {
						if subnet.IP.To4() != nil {
							allocatedByType["ipv4"] = subnet.String()
						} else {
							allocatedByType["ipv6"] = subnet.String()
						}
					}

					// Verify exact match of subnet values
					if expected.ipv4 != "" {
						g.Expect(allocatedByType["ipv4"]).To(gomega.Equal(expected.ipv4),
							"owner %s: IPv4 subnet should match exactly", expected.owner)
					}
					if expected.ipv6 != "" {
						g.Expect(allocatedByType["ipv6"]).To(gomega.Equal(expected.ipv6),
							"owner %s: IPv6 subnet should match exactly", expected.owner)
					}
				}
			}
		})
	}
}
