package services

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilpointer "k8s.io/utils/pointer"
)

var (
	name      string = "foo"
	namespace string = "testns"
)

func TestEnsureStaleLBs(t *testing.T) {
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	if err != nil {
		t.Fatalf("Error creating NB: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	defaultService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
		},
	}

	staleLBs := []LB{
		{
			Name:        "Service_testns/foo_TCP_node_router_node-a",
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
			Routers:     []string{"gr-node-a", "non-existing-router"},
			Switches:    []string{"non-existing-switch"},
			Groups:      []string{"non-existing-group"},
			Protocol:    "TCP",
			Rules: []LBRule{
				{
					Source:  Addr{IP: "1.2.3.4", Port: 80},
					Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
				},
			},
			UUID: "test-UUID",
		},
		{
			Name:        "Service_testns/foo_TCP_node_is_gone",
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
			Routers:     []string{"gr-node-is-gone", "non-existing-router"},
			Switches:    []string{"non-existing-switch"},
			Groups:      []string{"non-existing-group"},
			Protocol:    "TCP",
			Rules: []LBRule{
				{
					Source:  Addr{IP: "4.5.6.7", Port: 80},
					Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
				},
			},
			UUID: "test-UUID2",
		},
	}

	// required lb doesn't have stale router, switch, and port group reference.
	// "gr-node-a" is listed as applied in the cache, no update operation will be generated for it.
	LBs := []LB{
		{
			Name:        "Service_testns/foo_TCP_node_router_node-a",
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
			Routers:     []string{"gr-node-a"},
			Protocol:    "TCP",
			Rules: []LBRule{
				{
					Source:  Addr{IP: "1.2.3.4", Port: 80},
					Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
				},
			},
			UUID: "", // intentionally left empty to make sure EnsureLBs sets it properly
		},
	}
	err = EnsureLBs(nbClient, defaultService, staleLBs, LBs, &util.DefaultNetInfo{})
	if err != nil {
		t.Fatalf("Error EnsureLBs: %v", err)
	}
	if LBs[0].UUID != staleLBs[0].UUID {
		t.Fatalf("EnsureLBs did not set UUID of cached LB as is should")
	}
}

func TestEnsureLBs(t *testing.T) {
	tests := []struct {
		desc    string
		service *v1.Service
		LBs     []LB
		finalLB *nbdb.LoadBalancer
	}{
		{
			desc: "create service with permanent session affinity",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1.ServiceSpec{
					Type:            v1.ServiceTypeClusterIP,
					SessionAffinity: v1.ServiceAffinityClientIP,
					SessionAffinityConfig: &v1.SessionAffinityConfig{
						ClientIP: &v1.ClientIPConfig{
							TimeoutSeconds: utilpointer.Int32(86400),
						},
					},
				},
			},
			LBs: []LB{
				{
					Name:        "Service_foo/testns_TCP_cluster",
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.1.1", Port: 80},
							Targets: []Addr{{IP: "10.0.244.3", Port: 8080}},
						},
					},
					UUID: "test-UUID",
					Opts: LBOpts{
						Reject:          true,
						AffinityTimeOut: 86400,
					},
				},
			},
			finalLB: &nbdb.LoadBalancer{
				UUID:     clusterWideTCPServiceLoadBalancerName(name, namespace),
				Name:     clusterWideTCPServiceLoadBalancerName(name, namespace),
				Options:  servicesOptions(),
				Protocol: &nbdb.LoadBalancerProtocolTCP,
				Vips: map[string]string{
					"192.168.1.1:80": "10.0.244.3:8080",
				},
				ExternalIDs:     loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
				SelectionFields: []string{"ip_src", "ip_dst"}, // permanent session affinity, no learn flows
			},
		},
		{
			desc: "create service with default session affinity timeout",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: v1.ServiceSpec{
					Type:            v1.ServiceTypeClusterIP,
					SessionAffinity: v1.ServiceAffinityClientIP,
					SessionAffinityConfig: &v1.SessionAffinityConfig{
						ClientIP: &v1.ClientIPConfig{
							TimeoutSeconds: utilpointer.Int32(10800),
						},
					},
				},
			},
			LBs: []LB{
				{
					Name:        "Service_foo/testns_TCP_cluster",
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.1.1", Port: 80},
							Targets: []Addr{{IP: "10.0.244.3", Port: 8080}},
						},
					},
					UUID: "test-UUID",
					Opts: LBOpts{
						Reject:          true,
						AffinityTimeOut: 10800,
					},
				},
			},
			finalLB: &nbdb.LoadBalancer{
				UUID:     clusterWideTCPServiceLoadBalancerName(name, namespace),
				Name:     clusterWideTCPServiceLoadBalancerName(name, namespace),
				Options:  servicesOptionsWithAffinityTimeout(), // timeout set in the options
				Protocol: &nbdb.LoadBalancerProtocolTCP,
				Vips: map[string]string{
					"192.168.1.1:80": "10.0.244.3:8080",
				},
				ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(namespace, name)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: "gr-node-a",
					},
				},
			}, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = EnsureLBs(nbClient, tt.service, []LB{}, tt.LBs, &util.DefaultNetInfo{})
			if err != nil {
				t.Fatalf("Error EnsureLBs: %v", err)
			}
			matcher := libovsdbtest.HaveDataIgnoringUUIDs([]libovsdbtest.TestData{
				tt.finalLB,
				&nbdb.LogicalRouter{
					Name:         "gr-node-a",
					LoadBalancer: []string{clusterWideTCPServiceLoadBalancerName(name, namespace)},
				},
			})
			success, err := matcher.Match(nbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}
