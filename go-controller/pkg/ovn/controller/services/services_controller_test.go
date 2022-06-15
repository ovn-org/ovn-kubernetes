package services

import (
	"fmt"
	"net"
	"testing"

	"github.com/onsi/gomega"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	utilpointer "k8s.io/utils/pointer"
)

var alwaysReady = func() bool { return true }
var FakeGRs = "GR_1 GR_2"

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
	libovsdbCleanup    *libovsdbtest.Cleanup
}

func newController() (*serviceController, error) {
	return newControllerWithDBSetup(libovsdbtest.TestSetup{})
}

func newControllerWithDBSetup(dbSetup libovsdbtest.TestSetup) (*serviceController, error) {
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	if err != nil {
		return nil, err
	}
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	controller := NewController(client,
		nbClient,
		informerFactory.Core().V1().Services(),
		informerFactory.Discovery().V1().EndpointSlices(),
		informerFactory.Core().V1().Nodes(),
	)
	controller.servicesSynced = alwaysReady
	controller.endpointSlicesSynced = alwaysReady
	return &serviceController{
		controller,
		informerFactory.Core().V1().Services().Informer().GetStore(),
		informerFactory.Discovery().V1().EndpointSlices().Informer().GetStore(),
		cleanup,
	}, nil
}

func (c *serviceController) close() {
	c.libovsdbCleanup.Cleanup()
}

// TestSyncServices - an end-to-end test for the services controller.
func TestSyncServices(t *testing.T) {
	ns := "testns"
	serviceName := "foo"

	oldGateway := globalconfig.Gateway.Mode
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	globalconfig.IPv4Mode = true
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
		globalconfig.IPv4Mode = false
		globalconfig.Gateway.Mode = oldGateway
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	outport := int32(3456)
	tcp := v1.ProtocolTCP

	const (
		nodeA = "node-a"
		nodeB = "node-b"
	)
	defaultNodes := map[string]nodeInfo{
		nodeA: {
			name:              nodeA,
			nodeIPs:           []string{"10.0.0.1"},
			gatewayRouterName: "gr-node-a",
			switchName:        "switch-node-a",
		},
		nodeB: {
			name:              nodeB,
			nodeIPs:           []string{"10.0.0.2"},
			gatewayRouterName: "gr-node-b",
			switchName:        "switch-node-b",
		},
	}

	tests := []struct {
		name        string
		slice       *discovery.EndpointSlice
		service     *v1.Service
		initialDb   []libovsdbtest.TestData
		expectedDb  []libovsdbtest.TestData
		gatewayMode string
	}{

		{
			name: "create service from Single Stack Service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				nodeLogicalSwitch(nodeA),
				nodeLogicalSwitch(nodeB),
				nodeLogicalRouter(nodeA),
				nodeLogicalRouter(nodeB),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				nodeLogicalSwitch(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
			},
		},
		{
			name: "update service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				nodeLogicalSwitch(nodeA),
				nodeLogicalSwitch(nodeB),
				nodeLogicalSwitch("wrong-switch", "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter("node-c", "Service_testns/foo_TCP_cluster"),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				nodeLogicalSwitch(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch("wrong-switch"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter("node-c"),
			},
		},
		{
			name: "remove service from legacy load balancers",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				&nbdb.LoadBalancer{
					UUID:     "TCP_lb_gateway_router",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: map[string]string{
						"TCP_lb_gateway_router": "",
					},
				},
				nodeLogicalSwitch(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				&nbdb.LoadBalancer{
					UUID:     "TCP_lb_gateway_router",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: map[string]string{
						"TCP_lb_gateway_router": "",
					},
				},
				nodeLogicalSwitch(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
			},
		},
		{
			name: "transition to endpoints, create nodeport",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{
					{
						Protocol: &tcp,
						Port:     &outport,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.BoolPtr(true),
						},
						Addresses: []string{"10.128.0.2", "10.128.1.2"},
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
						NodePort:   8989,
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				nodeLogicalSwitch(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalSwitch(nodeB, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeA, "Service_testns/foo_TCP_cluster"),
				nodeLogicalRouter(nodeB, "Service_testns/foo_TCP_cluster"),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_cluster",
					Name: "Service_testns/foo_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_node_router+switch_node-a",
					Name: "Service_testns/foo_TCP_node_router+switch_node-a",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"10.0.0.1:8989": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				&nbdb.LoadBalancer{
					UUID: "Service_testns/foo_TCP_node_router+switch_node-b",
					Name: "Service_testns/foo_TCP_node_router+switch_node-b",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"10.0.0.2:8989": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "testns/foo",
					},
				},
				nodeLogicalSwitch(nodeA,
					"Service_testns/foo_TCP_cluster",
					"Service_testns/foo_TCP_node_router+switch_node-a"),
				nodeLogicalSwitch(nodeB,
					"Service_testns/foo_TCP_cluster",
					"Service_testns/foo_TCP_node_router+switch_node-b"),
				nodeLogicalRouter(nodeA,
					"Service_testns/foo_TCP_cluster",
					"Service_testns/foo_TCP_node_router+switch_node-a"),
				nodeLogicalRouter(nodeB,
					"Service_testns/foo_TCP_cluster",
					"Service_testns/foo_TCP_node_router+switch_node-b"),
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			if tt.gatewayMode != "" {
				globalconfig.Gateway.Mode = globalconfig.GatewayMode(tt.gatewayMode)
			} else {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			}

			ovnlb.TestOnlySetCache(nil)
			controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: tt.initialDb})
			if err != nil {
				t.Fatalf("Error creating controller: %v", err)
			}
			defer controller.close()
			// Add objects to the Store
			controller.endpointSliceStore.Add(tt.slice)
			controller.serviceStore.Add(tt.service)

			controller.nodeTracker.nodes = defaultNodes

			err = controller.syncService(ns + "/" + serviceName)
			if err != nil {
				t.Errorf("syncServices error: %v", err)
			}

			g.Eventually(controller.nbClient).Should(libovsdbtest.HaveData(tt.expectedDb))
		})
	}
}

func nodeLogicalSwitch(nodeName string, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	ls := &nbdb.LogicalSwitch{
		UUID: nodeSwitchName(nodeName),
		Name: nodeSwitchName(nodeName),
	}
	if len(namespacedServiceNames) > 0 {
		ls.LoadBalancer = namespacedServiceNames
	}
	return ls
}

func nodeLogicalRouter(nodeName string, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	ls := &nbdb.LogicalRouter{
		UUID: nodeGWRouterName(nodeName),
		Name: nodeGWRouterName(nodeName),
	}
	if len(namespacedServiceNames) > 0 {
		ls.LoadBalancer = namespacedServiceNames
	}
	return ls
}

func nodeSwitchName(nodeName string) string {
	return fmt.Sprintf("switch-%s", nodeName)
}

func nodeGWRouterName(nodeName string) string {
	return fmt.Sprintf("gr-%s", nodeName)
}
