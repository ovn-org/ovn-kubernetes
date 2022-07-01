package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
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
	initialMaxLength := format.MaxLength
	temporarilyEnableGomegaMaxLengthFormat()
	t.Cleanup(func() {
		restoreGomegaMaxLengthFormat(initialMaxLength)
	})

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
		nodeA           = "node-a"
		nodeB           = "node-b"
		nodeAEndpointIP = "10.128.0.2"
		nodeBEndpointIP = "10.128.1.2"
		nodeAHostIP     = "10.0.0.1"
		nodeBHostIP     = "10.0.0.2"
	)
	firstNode := nodeConfig(nodeA, nodeAHostIP)
	secondNode := nodeConfig(nodeB, nodeBHostIP)
	defaultNodes := map[string]nodeInfo{
		nodeA: *firstNode,
		nodeB: *secondNode,
	}

	const nodePort = 8989

	tests := []struct {
		name                 string
		slice                *discovery.EndpointSlice
		service              *v1.Service
		initialDb            []libovsdbtest.TestData
		expectedDb           []libovsdbtest.TestData
		gatewayMode          string
		nodeToDelete         *nodeInfo
		dbStateAfterDeleting []libovsdbtest.TestData
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
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
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
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA),
				nodeLogicalSwitch(nodeB),
				nodeLogicalSwitch("wrong-switch", loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter("node-c", loadBalancerClusterWideTCPServiceName(ns, serviceName)),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch("wrong-switch"),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
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
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				&nbdb.LoadBalancer{
					UUID:     "TCP_lb_gateway_router",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: tcpGatewayRouterExternalIDs(),
				},
				nodeLogicalSwitch(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				&nbdb.LoadBalancer{
					UUID:        "TCP_lb_gateway_router",
					Protocol:    &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: tcpGatewayRouterExternalIDs(),
				},
				nodeLogicalSwitch(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
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
						NodePort:   nodePort,
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeRouterLoadBalancer(firstNode, nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeRouterLoadBalancer(secondNode, nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeA, ns, serviceName)),
				nodeLogicalSwitch(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
				nodeLogicalRouter(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeA, ns, serviceName)),
				nodeLogicalRouter(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
			},
		},
		{
			name: "deleting a node should not leave stale load balancers",
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
						NodePort:   nodePort,
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeRouterLoadBalancer(firstNode, nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeRouterLoadBalancer(secondNode, nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeA, ns, serviceName)),
				nodeLogicalSwitch(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
				nodeLogicalRouter(nodeA,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeA, ns, serviceName)),
				nodeLogicalRouter(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
			},
			nodeToDelete: nodeConfig(nodeA, nodeAHostIP),
			dbStateAfterDeleting: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: serviceExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeRouterLoadBalancer(secondNode, nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA),
				nodeLogicalSwitch(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
				nodeLogicalRouter(nodeA),
				nodeLogicalRouter(nodeB,
					loadBalancerClusterWideTCPServiceName(ns, serviceName),
					nodeSwitchRouterLoadBalancerName(nodeB, ns, serviceName)),
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

			g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.expectedDb))

			if tt.nodeToDelete != nil {
				controller.nodeTracker.removeNode(tt.nodeToDelete.name)

				// we need to extract the deleted load balancer UUID, because
				// of a test library limitation: it does not delete the weak
				// references to the load balancers on the logical switches /
				// logical routers.
				nodeLoadBalancerUUID, err := extractLoadBalancerRealUUID(
					controller.nbClient,
					nodeSwitchRouterLoadBalancerName(nodeA, ns, serviceName))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(nodeLoadBalancerUUID).NotTo(gomega.BeEmpty())

				g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())

				// here, we need to patch the original expected data with the
				// real load balancer UUID, on the logical switch / GW router
				// of the deleted node, since the test library will be unable
				// to correlate the name of the load balancer with its UUID.
				// This happens because the load balancer was deleted.
				// Patching up the load balancer UUIDs allows us to use the
				// `HaveData` matcher below, thus increasing the test
				// accurateness.
				expectedData := patchLogicalRouterAndSwitchLoadBalancerUUIDs(
					nodeLoadBalancerUUID, tt.dbStateAfterDeleting, nodeSwitchName(nodeA), nodeGWRouterName(nodeA))

				g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedData))
			}
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

func loadBalancerClusterWideTCPServiceName(ns string, serviceName string) string {
	return fmt.Sprintf("Service_%s_TCP_cluster", namespacedServiceName(ns, serviceName))
}

func namespacedServiceName(ns string, name string) string {
	return fmt.Sprintf("%s/%s", ns, name)
}

func nodeSwitchRouterLoadBalancerName(nodeName string, serviceNamespace string, serviceName string) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_router+switch_%s",
		serviceNamespace,
		serviceName,
		nodeName)
}

func servicesOptions() map[string]string {
	return map[string]string{
		"event":     "false",
		"reject":    "true",
		"skip_snat": "false",
	}
}

func tcpGatewayRouterExternalIDs() map[string]string {
	return map[string]string{
		"TCP_lb_gateway_router": "",
	}
}

func serviceExternalIDs(namespacedServiceName string) map[string]string {
	return map[string]string{
		"k8s.ovn.org/kind":  "Service",
		"k8s.ovn.org/owner": namespacedServiceName,
	}
}

func nodeRouterLoadBalancer(node *nodeInfo, nodePort int32, serviceName string, serviceNamespace string, outputPort int32, endpointIPs ...string) *nbdb.LoadBalancer {
	return &nbdb.LoadBalancer{
		UUID:     nodeSwitchRouterLoadBalancerName(node.name, serviceNamespace, serviceName),
		Name:     nodeSwitchRouterLoadBalancerName(node.name, serviceNamespace, serviceName),
		Options:  servicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			endpoint(node.nodeIPs[0], nodePort): computeEndpoints(outputPort, endpointIPs...),
		},
		ExternalIDs: serviceExternalIDs(namespacedServiceName(serviceNamespace, serviceName)),
	}
}

func computeEndpoints(outputPort int32, ips ...string) string {
	var endpoints []string
	for _, ip := range ips {
		endpoints = append(endpoints, endpoint(ip, outputPort))
	}
	return strings.Join(endpoints, ",")
}

func endpoint(ip string, port int32) string {
	return fmt.Sprintf("%s:%d", ip, port)
}

func nodeConfig(nodeName string, nodeIP string) *nodeInfo {
	return &nodeInfo{
		name:              nodeName,
		nodeIPs:           []string{nodeIP},
		gatewayRouterName: nodeGWRouterName(nodeName),
		switchName:        nodeSwitchName(nodeName),
	}
}

func temporarilyEnableGomegaMaxLengthFormat() {
	format.MaxLength = 0
}

func restoreGomegaMaxLengthFormat(originalLength int) {
	format.MaxLength = originalLength
}

func extractLoadBalancerRealUUID(nbClient libovsdbclient.Client, lbName string) (string, error) {
	var lbs []nbdb.LoadBalancer
	predicate := func(lb *nbdb.LoadBalancer) bool {
		return lb.Name == lbName
	}
	if err := nbClient.WhereCache(predicate).List(context.TODO(), &lbs); err != nil {
		return "", errors.Wrapf(err, "failed to find load balancer %s", lbName)
	}

	return lbs[0].UUID, nil
}

func patchLogicalRouterAndSwitchLoadBalancerUUIDs(lbUUID string, testData []libovsdbtest.TestData, logicalEntityNames ...string) []libovsdbtest.TestData {
	entitiesToPatchUUIDs := sets.NewString(logicalEntityNames...)
	for _, ovnNBEntity := range testData {
		if logicalRouter, isLogicalRouter := ovnNBEntity.(*nbdb.LogicalRouter); isLogicalRouter {
			if entitiesToPatchUUIDs.Has(logicalRouter.Name) {
				logicalRouter.LoadBalancer = []string{lbUUID}
			}
		} else if logicalSwitch, isLogicalSwitch := ovnNBEntity.(*nbdb.LogicalSwitch); isLogicalSwitch {
			if entitiesToPatchUUIDs.Has(logicalSwitch.Name) {
				logicalSwitch.LoadBalancer = []string{lbUUID}
			}
		} else {
			continue
		}
	}
	return testData
}
