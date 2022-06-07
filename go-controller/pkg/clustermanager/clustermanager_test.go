package clustermanager

import (
	"net"
	"reflect"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func TestController_allocateNodeSubnets(t *testing.T) {
	tests := []struct {
		name          string
		networkRanges []string
		networkLen    int
		configIPv4    bool
		configIPv6    bool
		node          *kapi.Node
		// to be converted during the test to []*net.IPNet
		wantStr   []string
		allocated int
		wantErr   bool
	}{
		{
			name:          "new node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"172.16.0.0/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "new node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"2001:db2::/64"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "existing annotated node, IPv4 only cluster",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "172.16.8.0/24"}`,
					},
				},
			},
			wantStr:   []string{"172.16.8.0/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "existing annotated node, IPv6 only cluster",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "2001:db2:1:2:3:4::/64"}`,
					}},
			},
			wantStr:   []string{"2001:db2:1:2:3:4::/64"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "new node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   []string{"172.16.0.0/24", "2000::/24"},
			allocated: 2,
			wantErr:   false,
		},
		{
			name:          "annotated node, dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2000:2::/24"]}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24", "2000:2::/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "single IPv4 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "172.16.5.0/24"}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24", "2000::/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "single IPv6 to dual stack cluster",
			networkRanges: []string{"172.16.0.0/16", "2000:1::/12"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": "2000:1::/24"}`,
					},
				},
			},
			wantStr:   []string{"2000::/24", "172.16.0.0/24"},
			allocated: 1,
			wantErr:   false,
		},
		{
			name:          "dual stack cluster to single IPv4",
			networkRanges: []string{"172.16.0.0/16"},
			networkLen:    24,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2000:2::/24"]}`,
					},
				},
			},
			wantStr:   []string{"172.16.5.0/24"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "dual stack cluster to single IPv6",
			networkRanges: []string{"2001:db2::/56"},
			networkLen:    64,
			configIPv4:    false,
			configIPv6:    true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testnode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": `{"default": ["172.16.5.0/24","2001:db2:1:2:3:4::/64"]}`,
					},
				},
			},
			wantStr:   []string{"2001:db2:1:2:3:4::/64"},
			allocated: 0,
			wantErr:   false,
		},
		{
			name:          "new node, OVN wrong configuration: IPv4 only cluster but IPv6 range",
			networkRanges: []string{"2001:db2::/64"},
			networkLen:    112,
			configIPv4:    true,
			configIPv6:    false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "testnode",
					Annotations: map[string]string{},
				},
			},
			wantStr:   nil,
			allocated: 0,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// create cluster config
			config.IPv4Mode = tt.configIPv4
			config.IPv6Mode = tt.configIPv6
			// create fake OVN controller
			stopChan := make(chan struct{})
			defer close(stopChan)
			kubeFakeClient := fake.NewSimpleClientset()
			egressFirewallFakeClient := &egressfirewallfake.Clientset{}
			egressIPFakeClient := &egressipfake.Clientset{}
			egressQoSFakeClient := &egressqosfake.Clientset{}
			fakeClient := &util.OVNClientset{
				KubeClient:           kubeFakeClient,
				EgressIPClient:       egressIPFakeClient,
				EgressFirewallClient: egressFirewallFakeClient,
				EgressQoSClient:      egressQoSFakeClient,
			}
			f, err := factory.NewClusterManagerWatchFactory(fakeClient)
			if err != nil {
				t.Fatalf("Error creating master watch factory: %v", err)
			}
			if err := f.Start(); err != nil {
				t.Fatalf("Error starting master watch factory: %v", err)
			}

			clusterManager := NewClusterManager(fakeClient, f, stopChan,
				record.NewFakeRecorder(0))

			// configure the cluster allocators
			for _, subnetString := range tt.networkRanges {
				_, subnet, err := net.ParseCIDR(subnetString)
				if err != nil {
					t.Fatalf("Error parsing subnet %s", subnetString)
				}
				clusterManager.clusterSubnetAllocator.AddNetworkRange(subnet, tt.networkLen)
			}
			// test network allocation works correctly
			got, allocated, err := clusterManager.allocateNodeSubnets(tt.node)
			if (err != nil) != tt.wantErr {
				t.Errorf("Controller.addNode() error = %v, wantErr %v", err, tt.wantErr)
				return
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
				t.Errorf("clusterManager.allocateNodeSubnets() = %v, want %v", got, want)
			}

			if len(allocated) != tt.allocated {
				t.Errorf("Expected %d subnets allocated, received %d", tt.allocated, len(allocated))
			}
		})
	}
}
