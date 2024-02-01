package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	kube_test "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilpointer "k8s.io/utils/pointer"
)

var alwaysReady = func() bool { return true }
var FakeGRs = "GR_1 GR_2"
var initialLsGroups []string = []string{types.ClusterLBGroupName, types.ClusterSwitchLBGroupName}
var initialLrGroups []string = []string{types.ClusterLBGroupName, types.ClusterRouterLBGroupName}

var outport int32 = int32(3456)
var tcp v1.Protocol = v1.ProtocolTCP

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
	libovsdbCleanup    *libovsdbtest.Context
}

func newController() (*serviceController, error) {
	return newControllerWithDBSetup(libovsdbtest.TestSetup{})
}

func newControllerWithDBSetup(dbSetup libovsdbtest.TestSetup) (*serviceController, error) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	if err != nil {
		return nil, err
	}
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	recorder := record.NewFakeRecorder(10)

	nbZoneFailed := false
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls libovsdbutil.GetNBZone().
	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	controller, err := NewController(client,
		nbClient,
		informerFactory.Core().V1().Services(),
		informerFactory.Discovery().V1().EndpointSlices(),
		informerFactory.Core().V1().Nodes(),
		recorder,
	)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	controller.initTopLevelCache()
	controller.useLBGroups = true
	controller.useTemplates = true

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
	var (
		nodeA = "node-a"
		nodeB = "node-b"
	)
	const (
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
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
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
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
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
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitch("wrong-switch", []string{}, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter("node-c", []string{}, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
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
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitch("wrong-switch", []string{}),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouter("node-c", []string{}),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
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
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{"10.128.0.2", "10.128.1.2"},
						NodeName:  &nodeA,
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
				nodeLogicalSwitch(nodeA, initialLsGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, initialLsGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
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
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
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
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{"10.128.0.2", "10.128.1.2"},
						NodeName:  &nodeA,
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
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
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
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
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
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
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

			controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: tt.initialDb})
			if err != nil {
				t.Fatalf("Error creating controller: %v", err)
			}
			defer controller.close()
			// Add objects to the Store
			controller.endpointSliceStore.Add(tt.slice)
			controller.serviceStore.Add(tt.service)

			controller.nodeTracker.nodes = defaultNodes
			controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

			err = controller.syncService(ns + "/" + serviceName)
			if err != nil {
				t.Fatalf("syncServices error: %v", err)
			}

			g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.expectedDb))

			if tt.nodeToDelete != nil {
				controller.nodeTracker.removeNode(tt.nodeToDelete.name)
				g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())
				g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.dbStateAfterDeleting))
			}
		})
	}
}

func Test_ETPCluster_NodePort_Service_WithMultipleIPAddresses(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	globalconfig.IPv4Mode = true
	globalconfig.IPv6Mode = true
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00:0:0:0:5555::0/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 16}, {CIDR: cidr6, HostSubnetLength: 64}}

	nodeName := "node-a"
	nodeIPv4 := []net.IP{net.ParseIP("10.1.1.1"), net.ParseIP("10.2.2.2"), net.ParseIP("10.3.3.3")}
	nodeIPv6 := []net.IP{net.ParseIP("fd00:0:0:0:1::1"), net.ParseIP("fd00:0:0:0:2::2")}

	nodeA := nodeInfo{
		name:               nodeName,
		l3gatewayAddresses: []net.IP{nodeIPv4[0], nodeIPv6[0]},
		hostAddresses:      append(nodeIPv4, nodeIPv6...),
		gatewayRouterName:  nodeGWRouterName(nodeName),
		switchName:         nodeSwitchName(nodeName),
		chassisID:          nodeName,
		zone:               types.OvnDefaultZone,
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-foo", Namespace: "namespace1"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeNodePort,
			ClusterIP:             "192.168.1.1",
			ClusterIPs:            []string{"192.168.1.1", "fd00:0:0:0:7777::1"},
			IPFamilies:            []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			Selector:              map[string]string{"foo": "bar"},
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			Ports: []v1.ServicePort{{
				Port:       80,
				Protocol:   v1.ProtocolTCP,
				TargetPort: intstr.FromInt(3456),
				NodePort:   30123,
			}},
		},
	}

	endPointSliceV4 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv4",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outport}},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeName, "10.128.0.2", "10.128.1.2"),
	}

	endPointSliceV6 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv6",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outport}},
		AddressType: discovery.AddressTypeIPv6,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeName, "fe00:0:0:0:5555::2", "fe00:0:0:0:5555::3"),
	}

	controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		nodeLogicalSwitch(nodeA.name, initialLsGroups),
		nodeLogicalRouter(nodeA.name, initialLrGroups),

		lbGroup(types.ClusterLBGroupName),
		lbGroup(types.ClusterSwitchLBGroupName),
		lbGroup(types.ClusterRouterLBGroupName),
	}})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controller.close()

	controller.endpointSliceStore.Add(endPointSliceV4)
	controller.endpointSliceStore.Add(endPointSliceV6)
	controller.serviceStore.Add(svc)
	controller.nodeTracker.nodes = map[string]nodeInfo{nodeA.name: nodeA}

	controller.RequestFullSync(controller.nodeTracker.getZoneNodes())
	err = controller.syncService(svc.Namespace + "/" + svc.Name)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	expectedDb := []libovsdbtest.TestData{
		&nbdb.LoadBalancer{
			UUID:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Name:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Options:  servicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"192.168.1.1:80":        "10.128.0.2:3456,10.128.1.2:3456",
				"[fd00::7777:0:0:1]:80": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
			},
			ExternalIDs: serviceExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			Name:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			Options:  templateServicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv4_1:30123": "10.128.0.2:3456,10.128.1.2:3456",
				"^NODEIP_IPv4_2:30123": "10.128.0.2:3456,10.128.1.2:3456",
				"^NODEIP_IPv4_0:30123": "10.128.0.2:3456,10.128.1.2:3456",
			},
			ExternalIDs: serviceExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged",
			Name:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged",
			Options:  templateServicesOptionsV6(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv6_1:30123": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
				"^NODEIP_IPv6_0:30123": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
			},
			ExternalIDs: serviceExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		nodeLogicalSwitch(nodeA.name, initialLsGroups),
		nodeLogicalRouter(nodeA.name, initialLrGroups),
		lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name)),
		lbGroup(types.ClusterSwitchLBGroupName,
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged"),
		lbGroup(types.ClusterRouterLBGroupName,
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged"),

		&nbdb.ChassisTemplateVar{
			UUID: nodeA.chassisID, Chassis: nodeA.chassisID,
			Variables: map[string]string{
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeIPv4[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeIPv4[1].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeIPv4[2].String(),

				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeIPv6[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeIPv6[1].String(),
			},
		},
	}

	g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))

}

func nodeLogicalSwitch(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	ls := &nbdb.LogicalSwitch{
		UUID:              nodeSwitchName(nodeName),
		Name:              nodeSwitchName(nodeName),
		LoadBalancerGroup: lbGroups,
	}
	if len(namespacedServiceNames) > 0 {
		ls.LoadBalancer = namespacedServiceNames
	}
	return ls
}

func nodeLogicalRouter(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	lr := &nbdb.LogicalRouter{
		UUID:              nodeGWRouterName(nodeName),
		Name:              nodeGWRouterName(nodeName),
		LoadBalancerGroup: lbGroups,
	}
	if len(namespacedServiceNames) > 0 {
		lr.LoadBalancer = namespacedServiceNames
	}
	return lr
}

func nodeSwitchName(nodeName string) string {
	return fmt.Sprintf("switch-%s", nodeName)
}

func nodeGWRouterName(nodeName string) string {
	return fmt.Sprintf("gr-%s", nodeName)
}

func lbGroup(name string, namespacedServiceNames ...string) *nbdb.LoadBalancerGroup {
	lbg := &nbdb.LoadBalancerGroup{
		UUID: name,
		Name: name,
	}
	if len(namespacedServiceNames) > 0 {
		lbg.LoadBalancer = namespacedServiceNames
	}
	return lbg
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

func nodeSwitchTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_switch_template_%s",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func nodeRouterTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_router_template_%s",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func nodeMergedTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_switch_template_%s_merged",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func servicesOptions() map[string]string {
	return map[string]string{
		"event":              "false",
		"reject":             "true",
		"skip_snat":          "false",
		"neighbor_responder": "none",
		"hairpin_snat_ip":    "169.254.169.5 fd69::5",
	}
}

func servicesOptionsWithAffinityTimeout() map[string]string {
	options := servicesOptions()
	options["affinity_timeout"] = "10800"
	return options
}

func templateServicesOptions() map[string]string {
	// Template LBs need "options:template=true" and "options:address-family" set.
	opts := servicesOptions()
	opts["template"] = "true"
	opts["address-family"] = "ipv4"
	return opts
}

func templateServicesOptionsV6() map[string]string {
	// Template LBs need "options:template=true" and "options:address-family" set.
	opts := servicesOptions()
	opts["template"] = "true"
	opts["address-family"] = "ipv6"
	return opts
}

func tcpGatewayRouterExternalIDs() map[string]string {
	return map[string]string{
		"TCP_lb_gateway_router": "",
	}
}

func serviceExternalIDs(namespacedServiceName string) map[string]string {
	return map[string]string{
		types.LoadBalancerKindExternalID:  "Service",
		types.LoadBalancerOwnerExternalID: namespacedServiceName,
	}
}

func nodeSwitchTemplateLoadBalancer(nodePort int32, serviceName string, serviceNamespace string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeSwitchTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Name:     nodeSwitchTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			endpoint(refTemplate(nodeTemplateIP.Name), nodePort): refTemplate(makeTarget(serviceName, serviceNamespace, "TCP", nodePort, "node_switch_template", v1.IPv4Protocol)),
		},
		ExternalIDs: serviceExternalIDs(namespacedServiceName(serviceNamespace, serviceName)),
	}
}

func nodeRouterTemplateLoadBalancer(nodePort int32, serviceName string, serviceNamespace string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeRouterTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Name:     nodeRouterTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			endpoint(refTemplate(nodeTemplateIP.Name), nodePort): refTemplate(makeTarget(serviceName, serviceNamespace, "TCP", nodePort, "node_router_template", v1.IPv4Protocol)),
		},
		ExternalIDs: serviceExternalIDs(namespacedServiceName(serviceNamespace, serviceName)),
	}
}

func nodeIPTemplate(node *nodeInfo) *nbdb.ChassisTemplateVar {
	return &nbdb.ChassisTemplateVar{
		UUID:    node.chassisID,
		Chassis: node.chassisID,
		Variables: map[string]string{
			makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": node.hostAddresses[0].String(),
		},
	}
}

func nodeMergedTemplateLoadBalancer(nodePort int32, serviceName string, serviceNamespace string, outputPort int32, endpointIPs ...string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Name:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			endpoint(refTemplate(nodeTemplateIP.Name), nodePort): computeEndpoints(outputPort, endpointIPs...),
		},
		ExternalIDs: serviceExternalIDs(namespacedServiceName(serviceNamespace, serviceName)),
	}
}

func refTemplate(template string) string {
	return "^" + template
}

func makeTarget(serviceName, serviceNamespace string, proto v1.Protocol, outputPort int32, scope string, addressFamily v1.IPFamily) string {
	return makeTemplateName(
		fmt.Sprintf("Service_%s/%s_%s_%d_%s_%s",
			serviceNamespace, serviceName,
			proto, outputPort, scope, addressFamily))
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
		name:               nodeName,
		l3gatewayAddresses: []net.IP{net.ParseIP(nodeIP)},
		hostAddresses:      []net.IP{net.ParseIP(nodeIP)},
		gatewayRouterName:  nodeGWRouterName(nodeName),
		switchName:         nodeSwitchName(nodeName),
		chassisID:          nodeName,
		zone:               types.OvnDefaultZone,
	}
}

func temporarilyEnableGomegaMaxLengthFormat() {
	format.MaxLength = 0
}

func restoreGomegaMaxLengthFormat(originalLength int) {
	format.MaxLength = originalLength
}

func createTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	nbGlobal := &nbdb.NBGlobal{Name: zone}
	ops, err := nbClient.Create(nbGlobal)
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func deleteTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	p := func(nbGlobal *nbdb.NBGlobal) bool {
		return true
	}

	ops, err := nbClient.WhereCache(p).Delete()
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func readyEndpointsWithAddresses(addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready: utilpointer.Bool(true),
		},
		Addresses: addresses,
	}
}
