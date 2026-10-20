// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	sharednode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func rangesFromStrings(ranges []string, networkLens []int) ([]config.CIDRNetworkEntry, error) {
	entries := make([]config.CIDRNetworkEntry, 0, len(ranges))
	for i, subnetString := range ranges {
		_, subnet, err := net.ParseCIDR(subnetString)
		if err != nil {
			return nil, fmt.Errorf("error parsing subnet %s", subnetString)
		}
		entries = append(entries, config.CIDRNetworkEntry{
			CIDR:             subnet,
			HostSubnetLength: networkLens[i],
		})
	}
	return entries, nil
}

type existingAllocation struct {
	subnet string
	owner  string
}

func TestController_allocateNodeSubnets(t *testing.T) {
	tests := []struct {
		name          string
		networkRanges []string
		networkLens   []int
		configIPv4    bool
		configIPv6    bool
		existingNets  []*net.IPNet
		alreadyOwned  *existingAllocation
		// to be converted during the test to []*net.IPNet
		wantStr       []string
		allocated     int
		wantErr       bool
		existingNodes []*corev1.Node
	}{
		{
			name:          "new node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       []string{"172.16.0.0/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "new node, IPv4 only cluster, the test node is added when a hybrid overlay node with overlapped node subnet exists",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       []string{"172.16.1.0/24"},
			allocated:     1,
			wantErr:       false,
			existingNodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "ho_node1",
						Annotations: map[string]string{
							"k8s.ovn.org/hybrid-overlay-node-subnet": "172.16.0.0/24",
						},
						Labels: map[string]string{
							"hybrid-overlay-node": "true",
						},
					},
					Spec: corev1.NodeSpec{},
				},
			},
		},
		{
			name:          "new node, IPv4 only cluster, the test node is added when a hybrid overlay node with overlapped and differernt mask length node subnet exists",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       []string{"172.16.2.0/24"},
			allocated:     1,
			wantErr:       false,
			existingNodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "ho_node1",
						Annotations: map[string]string{
							"k8s.ovn.org/hybrid-overlay-node-subnet": "172.16.0.0/23",
						},
						Labels: map[string]string{
							"hybrid-overlay-node": "true",
						},
					},
					Spec: corev1.NodeSpec{},
				},
			},
		},
		{
			name:          "new node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  nil,
			wantStr:       []string{"2001:db2::/64"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("172.16.8.0/24"),
			wantStr:       []string{"172.16.8.0/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "existing annotated node with unexpected IPv4 prefix length",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("172.16.8.0/25"),
			wantStr:       []string{"172.16.0.0/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/32"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("2001:db2:1::/64"),
			wantStr:       []string{"2001:db2:1::/64"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "new node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  nil,
			wantStr:       []string{"172.16.0.0/24", "2000::/24"},
			allocated:     2,
			wantErr:       false,
		},
		{
			name:          "existing annotated node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2000::/24"),
			wantStr:       []string{"172.16.5.0/24", "2000::/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "single IPv4 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLens:   []int{24, 24},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24"),
			wantStr:       []string{"172.16.5.0/24", "2000::/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "single IPv6 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/16"},
			networkLens:   []int{24, 32},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("2000:1::/32"),
			wantStr:       []string{"2000:1::/32", "172.16.0.0/24"},
			allocated:     1,
			wantErr:       false,
		},
		{
			name:          "dual stack cluster to single IPv4",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2000:2::/24"),
			wantStr:       []string{"172.16.5.0/24"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "dual stack cluster to single IPv6",
			networkRanges: []string{"2001:db2:1::/56"},
			networkLens:   []int{64},
			configIPv4:    false,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.5.0/24", "2001:db2:1:2::/64"),
			wantStr:       []string{"2001:db2:1:2::/64"},
			allocated:     0,
			wantErr:       false,
		},
		{
			name:          "new node, OVN wrong configuration: IPv4 only cluster but IPv6 range",
			networkRanges: []string{"2001:db2::/64"},
			networkLens:   []int{112},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  nil,
			wantStr:       nil,
			allocated:     0,
			wantErr:       true,
		},
		{
			name:          "existing annotated node outside cluster CIDR",
			networkRanges: []string{"172.16.0.0/16"},
			networkLens:   []int{24},
			configIPv4:    true,
			configIPv6:    false,
			existingNets:  ovntest.MustParseIPNets("10.1.0.0/24"),
			wantStr:       []string{"172.16.0.0/24"},
			allocated:     1,
		},
		{
			name:          "existing annotated node with too many subnets",
			networkRanges: []string{"172.16.0.0/16", "2001:db2:1::/56"},
			networkLens:   []int{24, 64},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.0.0/24", "172.16.1.0/24", "2001:db2:1:2::/64", "2001:db2:1:3::/64"),
			wantStr:       []string{"172.16.0.0/24", "2001:db2:1:2::/64"},
			allocated:     0,
		},
		{
			name:          "existing annotated node with too many subnets, one of which is already owned",
			networkRanges: []string{"172.16.0.0/16", "2001:db2:1::/56"},
			networkLens:   []int{24, 64},
			configIPv4:    true,
			configIPv6:    true,
			existingNets:  ovntest.MustParseIPNets("172.16.0.0/24", "172.16.1.0/24", "2001:db2:1:2::/64", "2001:db2:1:3::/64"),
			alreadyOwned: &existingAllocation{
				owner:  "another-node",
				subnet: "172.16.1.0/24",
			},
			wantStr:   []string{"172.16.0.0/24", "2001:db2:1:2::/64"},
			allocated: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.HybridOverlay.Enabled = true
			config.Kubernetes.NoHostSubnetNodes, _ = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{"hybrid-overlay-node": "true"},
			})
			config.HybridOverlay.ClusterSubnets = nil

			ranges, err := rangesFromStrings(tt.networkRanges, tt.networkLens)
			if err != nil {
				t.Fatal(err)
			}
			config.Default.ClusterSubnets = ranges

			netInfo, err := util.NewNetInfo(
				&ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			na := &NodeAllocator{
				netInfo:                netInfo,
				clusterSubnetAllocator: NewSubnetAllocator(),
				nodeLister:             newFakeNodeLister([]*corev1.Node{}),
			}

			if err := na.Init(); err != nil {
				t.Fatalf("Failed to initialize node allocator: %v", err)
			}
			nodeInterfaces := make([]interface{}, len(tt.existingNodes))
			for i, node := range tt.existingNodes {
				nodeInterfaces[i] = node
			}
			// Sync existing nodes before allocating subnets
			err = na.Sync(nodeInterfaces)
			if err != nil {
				t.Fatal(err)
			}

			if tt.alreadyOwned != nil {
				err := na.clusterSubnetAllocator.MarkAllocatedNetworks(tt.alreadyOwned.owner, ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
				if err != nil {
					t.Fatal(err)
				}
			}

			// test network allocation works correctly
			got, allocated, err := na.allocateNodeSubnets(na.clusterSubnetAllocator, "testnode", tt.existingNets, tt.configIPv4, tt.configIPv6)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Controller.addNode() error = %v, wantErr %v", err, tt.wantErr)
			}

			var want []*net.IPNet
			for _, netStr := range tt.wantStr {
				_, ipnet, err := net.ParseCIDR(netStr)
				if err != nil {
					t.Fatalf("Error parsing subnet %s", netStr)
				}
				want = append(want, ipnet)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Controller.allocateNodeSubnets() = %v, want %v", got, want)
			}

			if len(allocated) != tt.allocated {
				t.Fatalf("Expected %d subnets allocated, received %d", tt.allocated, len(allocated))
			}

			// Ensure an already owned subnet isn't touched
			if tt.alreadyOwned != nil {
				err = na.clusterSubnetAllocator.MarkAllocatedNetworks("blahblah", ovntest.MustParseIPNets(tt.alreadyOwned.subnet)...)
				if err == nil {
					t.Fatal("Expected subnet to already be allocated by a different node")
				}
			}
		})
	}
}

func TestController_allocateNodeSubnets_ReleaseOnError(t *testing.T) {
	ranges, err := rangesFromStrings([]string{"172.16.0.0/16", "2000::/127"}, []int{24, 127})
	if err != nil {
		t.Fatal(err)
	}
	config.Default.ClusterSubnets = ranges

	netInfo, err := util.NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	na := &NodeAllocator{
		netInfo:                netInfo,
		clusterSubnetAllocator: NewSubnetAllocator(),
		nodeLister:             newFakeNodeLister([]*corev1.Node{}),
	}

	if err := na.Init(); err != nil {
		t.Fatalf("Failed to initialize node allocator: %v", err)
	}

	// Mark all v6 subnets already allocated to force an error in AllocateNodeSubnets()
	if err := na.clusterSubnetAllocator.MarkAllocatedNetworks("blah", ovntest.MustParseIPNet("2000::/127")); err != nil {
		t.Fatalf("MarkAllocatedNetworks() expected no error but got: %v", err)
	}

	// test network allocation works correctly
	v4usedBefore, v6usedBefore := na.clusterSubnetAllocator.Usage()
	got, allocated, err := na.allocateNodeSubnets(na.clusterSubnetAllocator, "testNode", nil, true, true)
	if err == nil {
		t.Fatalf("allocateNodeSubnets() expected error but got success")
	}
	if got != nil {
		t.Fatalf("allocateNodeSubnets() expected no existing host subnets, got %v", got)
	}
	if allocated != nil {
		t.Fatalf("allocateNodeSubnets() expected no allocated subnets, got %v", allocated)
	}

	v4usedAfter, v6usedAfter := na.clusterSubnetAllocator.Usage()
	if v4usedAfter != v4usedBefore {
		t.Fatalf("Expected %d v4 allocated subnets, but got %d", v4usedBefore, v4usedAfter)
	}
	if v6usedAfter != v6usedBefore {
		t.Fatalf("Expected %d v6 allocated subnets, but got %d", v6usedBefore, v6usedAfter)
	}
}

func newFakeNodeLister(nodes []*corev1.Node) listersv1.NodeLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, node := range nodes {
		_ = indexer.Add(node)
	}
	return listersv1.NewNodeLister(indexer)
}

func TestController_CleanupStaleAnnotation(t *testing.T) {
	// create a node with an annotation that shouldn't be changed and one that should be cleaned up.
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{"leave-me": "value", util.OVNNodeGRLRPAddrs: "remove-me"},
		},
	}
	fakeClient := fake.NewClientset(newNode)
	kube := &kube.Kube{
		KClient: fakeClient,
	}

	netInfo, err := util.NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	na := &NodeAllocator{
		nodeLister: newFakeNodeLister([]*corev1.Node{newNode}),
		kube:       kube,
		netInfo:    netInfo,
	}
	na.CleanupStaleAnnotation()
	nodes, err := fakeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 {
		t.Fatalf("Expected 1 node, got %d", len(nodes.Items))
	}
	// check that unrelated annotation is not changed, and stale one is cleaned up
	if !reflect.DeepEqual(nodes.Items[0].Annotations, map[string]string{"leave-me": "value"}) {
		t.Fatalf("Expected annotation %s to be cleaned up, got %v", util.OVNNodeGRLRPAddrs, nodes.Items[0].Annotations)
	}
}

func TestNodeAllocatorNeedsNodeAllocationForIPModeChange(t *testing.T) {
	origClusterSubnets := config.Default.ClusterSubnets
	t.Cleanup(func() {
		config.Default.ClusterSubnets = origClusterSubnets
	})

	tests := []struct {
		name             string
		clusterSubnets   []string
		clusterSubnetLen []int
		nodeSubnets      []string
		want             bool
	}{
		{
			name:             "single stack annotation requires reconcile after dual stack conversion",
			clusterSubnets:   []string{"10.244.0.0/16", "fd00:10:244::/48"},
			clusterSubnetLen: []int{24, 64},
			nodeSubnets:      []string{"10.244.2.0/24"},
			want:             true,
		},
		{
			name:             "dual stack annotation requires reconcile after single stack conversion",
			clusterSubnets:   []string{"10.244.0.0/16"},
			clusterSubnetLen: []int{24},
			nodeSubnets:      []string{"10.244.2.0/24", "fd00:10:244:2::/64"},
			want:             true,
		},
		{
			name:             "dual stack annotation satisfies dual stack cluster",
			clusterSubnets:   []string{"10.244.0.0/16", "fd00:10:244::/48"},
			clusterSubnetLen: []int{24, 64},
			nodeSubnets:      []string{"10.244.2.0/24", "fd00:10:244:2::/64"},
			want:             false,
		},
		{
			name:             "single stack annotation satisfies single stack cluster",
			clusterSubnets:   []string{"10.244.0.0/16"},
			clusterSubnetLen: []int{24},
			nodeSubnets:      []string{"10.244.2.0/24"},
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("PrepareTestConfig() failed: %v", err)
			}

			ranges, err := rangesFromStrings(tt.clusterSubnets, tt.clusterSubnetLen)
			if err != nil {
				t.Fatal(err)
			}
			config.Default.ClusterSubnets = ranges
			config.IPv4Mode = false
			config.IPv6Mode = false
			for _, subnet := range ranges {
				if subnet.CIDR.IP.To4() != nil {
					config.IPv4Mode = true
				} else {
					config.IPv6Mode = true
				}
			}

			netInfo, err := util.NewNetInfo(
				&ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			annotations, err := util.UpdateNodeHostSubnetAnnotation(map[string]string{}, ovntest.MustParseIPNets(tt.nodeSubnets...), types.DefaultNetworkName)
			if err != nil {
				t.Fatal(err)
			}
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: annotations,
				},
			}

			na := &NodeAllocator{netInfo: netInfo}
			state := sharednode.NewNodeAnnotationCache().UpdateNodeAnnotationState(node, true)
			if got := na.NeedsNodeAllocationWithState(node, state); got != tt.want {
				t.Fatalf("NeedsNodeAllocationWithState() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNodeAllocator_CleanupNodeWithoutNodeObject verifies that CleanupNode correctly releases
// allocator state when the node object is unavailable (true delete case).
func TestNodeAllocator_CleanupNodeWithoutNodeObject(t *testing.T) {
	origHybridEnabled := config.HybridOverlay.Enabled
	origHybridSubnets := config.HybridOverlay.ClusterSubnets
	origClusterSubnets := config.Default.ClusterSubnets
	origNoHostSubnetNodes := config.Kubernetes.NoHostSubnetNodes
	t.Cleanup(func() {
		config.HybridOverlay.Enabled = origHybridEnabled
		config.HybridOverlay.ClusterSubnets = origHybridSubnets
		config.Default.ClusterSubnets = origClusterSubnets
		config.Kubernetes.NoHostSubnetNodes = origNoHostSubnetNodes
	})

	config.HybridOverlay.Enabled = true
	config.HybridOverlay.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.0.0.0/16"), HostSubnetLength: 24},
	}

	ranges, err := rangesFromStrings([]string{"172.16.0.0/16"}, []int{24})
	if err != nil {
		t.Fatal(err)
	}
	config.Default.ClusterSubnets = ranges

	netInfo, err := util.NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	na := &NodeAllocator{
		netInfo:                netInfo,
		clusterSubnetAllocator: NewSubnetAllocator(),
		nodeLister:             newFakeNodeLister([]*corev1.Node{}),
	}
	if na.hasHybridOverlayAllocation() {
		na.hybridOverlaySubnetAllocator = NewSubnetAllocator()
	}

	if !na.hasHybridOverlayAllocation() {
		t.Fatal("Hybrid overlay allocation should be enabled given the test configuration")
	}

	if err := na.Init(); err != nil {
		t.Fatalf("Failed to initialize node allocator: %v", err)
	}

	nodeName := "node-delete-test"
	if !na.hasNodeSubnetAllocation() {
		t.Fatal("Node subnet allocation should be enabled")
	}

	allocated, _, err := na.allocateNodeSubnets(na.clusterSubnetAllocator, nodeName, nil, true, false)
	if err != nil {
		t.Fatalf("Failed to allocate subnet: %v", err)
	}
	if len(allocated) == 0 {
		t.Fatal("No subnet allocated")
	}

	v4used, _ := na.clusterSubnetAllocator.Usage()
	if v4used != 1 {
		t.Fatalf("Expected 1 allocated subnet, got %d", v4used)
	}

	if na.hasHybridOverlayAllocation() {
		if _, _, err := na.allocateNodeSubnets(na.hybridOverlaySubnetAllocator, nodeName, nil, true, false); err != nil {
			t.Fatalf("Failed to allocate hybrid overlay subnet: %v", err)
		}
		hoUsed, _ := na.hybridOverlaySubnetAllocator.Usage()
		if hoUsed != 1 {
			t.Fatalf("Expected 1 allocated hybrid overlay subnet, got %d", hoUsed)
		}
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}

	if err := na.CleanupNode(node.Name, nil); err != nil {
		t.Fatalf("CleanupNode failed: %v", err)
	}

	v4usedAfter, _ := na.clusterSubnetAllocator.Usage()
	if v4usedAfter != 0 {
		t.Errorf("Subnet leak detected! Expected 0 allocated subnets, got %d", v4usedAfter)
	}

	if na.hasHybridOverlayAllocation() {
		hoUsedAfter, _ := na.hybridOverlaySubnetAllocator.Usage()
		if hoUsedAfter != 0 {
			t.Errorf("Hybrid overlay subnet leak detected! Expected 0 allocated subnets, got %d", hoUsedAfter)
		}
	}
}

func TestController_CleanupNodeRemovesUDNAnnotations(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn1",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		Subnets:  "10.1.0.0/16",
	}
	netInfo, err := util.NewNetInfo(netConf)
	if err != nil {
		t.Fatal(err)
	}
	networkName := netInfo.GetNetworkName()

	nodeSubnet := ovntest.MustParseIPNet("10.1.0.0/24")
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}
	node.Annotations, err = util.UpdateNodeHostSubnetAnnotation(node.Annotations, []*net.IPNet{nodeSubnet}, networkName)
	if err != nil {
		t.Fatal(err)
	}
	node.Annotations, err = util.UpdateNetworkIDAnnotation(node.Annotations, networkName, 7)
	if err != nil {
		t.Fatal(err)
	}

	fakeClient := fake.NewClientset(node)
	kube := &kube.Kube{KClient: fakeClient}

	na := &NodeAllocator{
		nodeLister:             newFakeNodeLister([]*corev1.Node{node}),
		kube:                   kube,
		netInfo:                netInfo,
		clusterSubnetAllocator: NewSubnetAllocator(),
	}
	_, subnetCIDR, err := net.ParseCIDR("10.1.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if err := na.clusterSubnetAllocator.AddNetworkRange(subnetCIDR, 24); err != nil {
		t.Fatal(err)
	}
	if err := na.clusterSubnetAllocator.MarkAllocatedNetworks(node.Name, nodeSubnet); err != nil {
		t.Fatal(err)
	}

	if err := na.CleanupNode(node.Name, node); err != nil {
		t.Fatalf("CleanupNode failed: %v", err)
	}

	updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if util.HasNodeHostSubnetAnnotation(updatedNode, networkName) {
		t.Fatalf("expected host subnet annotation to be removed for network %s", networkName)
	}
	if _, err := util.ParseNetworkIDAnnotation(updatedNode, networkName); !util.IsAnnotationNotSetError(err) {
		t.Fatalf("expected network ID annotation to be removed for network %s, got: %v", networkName, err)
	}
	v4used, v6used := na.clusterSubnetAllocator.Usage()
	if v4used != 0 || v6used != 0 {
		t.Fatalf("expected subnet allocation to be released, got v4=%d v6=%d", v4used, v6used)
	}
}

func TestController_CleanupNodeReleasesTunnelIDs(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableInterconnect = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn2",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer2Topology,
		Role:     types.NetworkRolePrimary,
	}
	netInfo, err := util.NewNetInfo(netConf)
	if err != nil {
		t.Fatal(err)
	}
	networkName := netInfo.GetNetworkName()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node1",
			Annotations: map[string]string{},
		},
	}
	node.Annotations, err = util.UpdateUDNLayer2NodeGRLRPTunnelIDs(node.Annotations, networkName, 42)
	if err != nil {
		t.Fatal(err)
	}

	fakeClient := fake.NewClientset(node)
	kube := &kube.Kube{KClient: fakeClient}

	na := &NodeAllocator{
		nodeLister:             newFakeNodeLister([]*corev1.Node{node}),
		kube:                   kube,
		netInfo:                netInfo,
		clusterSubnetAllocator: NewSubnetAllocator(),
		idAllocator:            id.NewIDAllocator("tunnel-ids", 1024),
	}
	if err := na.idAllocator.ReserveID(networkName+"_"+node.Name, 42); err != nil {
		t.Fatal(err)
	}

	if err := na.CleanupNode(node.Name, node); err != nil {
		t.Fatalf("CleanupNode failed: %v", err)
	}

	updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(updatedNode, networkName); !util.IsAnnotationNotSetError(err) {
		t.Fatalf("expected tunnel ID annotation to be removed for network %s, got: %v", networkName, err)
	}
	if gotID := na.idAllocator.GetID(networkName + "_" + node.Name); gotID != types.InvalidID {
		t.Fatalf("expected tunnel ID to be released, got %d", gotID)
	}
}
