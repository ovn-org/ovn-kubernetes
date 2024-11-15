package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"golang.org/x/exp/maps"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"
	utilpointer "k8s.io/utils/pointer"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	kubetest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	nodeA = "node-a"
	nodeB = "node-b"

	tcp = v1.ProtocolTCP
	udp = v1.ProtocolUDP
)

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
	libovsdbCleanup    *libovsdbtest.Context
}

func newControllerWithDBSetupForNetwork(dbSetup libovsdbtest.TestSetup, netInfo util.NetInfo, nadNamespace string) (*serviceController, error) {
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)

	if err != nil {
		return nil, err
	}

	config.OVNKubernetesFeature.EnableInterconnect = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	client := util.GetOVNClientset().GetOVNKubeControllerClientset()

	factoryMock, err := factory.NewOVNKubeControllerWatchFactory(client)
	if err != nil {
		return nil, err
	}

	if err = factoryMock.Start(); err != nil {
		return nil, err
	}
	recorder := record.NewFakeRecorder(10)

	nbZoneFailed := false
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls libovsdbutil.GetNBZone().
	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		if err = createTestNBGlobal(nbClient, "global"); err != nil {
			return nil, err
		}
	}

	controller, err := NewController(client.KubeClient,
		nbClient,
		factoryMock.ServiceCoreInformer(),
		factoryMock.EndpointSliceCoreInformer(),
		factoryMock.NodeCoreInformer(),
		networkmanager.Default().Interface(),
		recorder,
		netInfo,
	)
	if err != nil {
		return nil, err
	}

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		if err = deleteTestNBGlobal(nbClient); err != nil {
			return nil, err
		}
	}

	if err = controller.initTopLevelCache(); err != nil {
		return nil, err
	}
	controller.useLBGroups = true
	controller.useTemplates = true

	// When testing services on UDN, add a NAD in the same namespace associated to the service
	if !netInfo.IsDefault() {
		if err = addSampleNAD(client, nadNamespace, netInfo); err != nil {
			return nil, err
		}
	}

	return &serviceController{
		controller,
		factoryMock.ServiceCoreInformer().Informer().GetStore(),
		factoryMock.EndpointSliceInformer().GetStore(),
		cleanup,
	}, nil
}

func (c *serviceController) close() {
	c.libovsdbCleanup.Cleanup()
}

func getSampleUDNNetInfo(namespace string, topology string) (util.NetInfo, error) {
	// requires that config.IPv4Mode = true
	subnets := "192.168.200.0/16"
	if topology == types.Layer3Topology {
		subnets += "/24"
	}
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		Topology:   topology,
		NADName:    fmt.Sprintf("%s/nad1", namespace),
		MTU:        1400,
		Role:       "primary",
		Subnets:    subnets,
		NetConf:    cnitypes.NetConf{Name: fmt.Sprintf("net_%s", topology), Type: "ovn-k8s-cni-overlay"},
		JoinSubnet: "100.66.0.0/16",
	})
	return netInfo, err
}

func addSampleNAD(client *util.OVNKubeControllerClientset, namespace string, netInfo util.NetInfo) error {
	_, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
		context.TODO(),
		kubetest.GenerateNAD(netInfo.GetNetworkName(), netInfo.GetNetworkName(), namespace, netInfo.TopologyType(), netInfo.Subnets()[0].String(), types.NetworkRolePrimary),
		metav1.CreateOptions{})
	return err
}

// TestSyncServices - an end-to-end test for the services controller.
func TestSyncServices(t *testing.T) {
	// setup gomega parameters
	initialMaxLength := format.MaxLength
	temporarilyEnableGomegaMaxLengthFormat()
	t.Cleanup(func() {
		restoreGomegaMaxLengthFormat(initialMaxLength)
	})

	// define test constants
	const (
		nodeAEndpoint    = "10.128.0.2"
		nodeAEndpoint2   = "10.128.0.3"
		nodeAEndpointV6  = "fe00::5555:0:0:2"
		nodeAEndpoint2V6 = "fe00::5555:0:0:3"

		nodeBEndpointIP = "10.128.1.2"

		nodeAHostAddress = "10.0.0.1"
		nodeBHostAddress = "10.0.0.2"

		nodePort = 8989

		// the IPs below are only used in one test
		nodeAHostAddress2   = "10.2.2.2"
		nodeAHostAddress3   = "10.3.3.3"
		nodeAHostAddressV6  = "fd00::1:0:0:1"
		nodeAHostAddress2V6 = "fd00::2:0:0:2"
	)

	var (
		ns          = "testns"
		serviceName = "foo"

		serviceClusterIP   = "192.168.1.1"
		serviceClusterIPv6 = "fd00::7777:0:0:1"
		servicePort        = int32(80)
		outPort            = int32(3456)

		initialLsGroups = []string{types.ClusterLBGroupName, types.ClusterSwitchLBGroupName}
		initialLrGroups = []string{types.ClusterLBGroupName, types.ClusterRouterLBGroupName}
	)
	// setup global config
	oldGateway := config.Gateway.Mode
	oldClusterSubnet := config.Default.ClusterSubnets
	config.Kubernetes.OVNEmptyLbEvents = true
	config.IPv4Mode = true
	config.IPv6Mode = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
		config.IPv4Mode = false
		config.IPv6Mode = false
		config.Gateway.Mode = oldGateway
		config.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00:0:0:0:5555::0/64")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 26}, {CIDR: cidr6, HostSubnetLength: 26}}
	l3UDN, err := getSampleUDNNetInfo(ns, "layer3")
	if err != nil {
		t.Fatalf("Error creating UDNNetInfo: %v", err)
	}
	l2UDN, err := getSampleUDNNetInfo(ns, "layer2")
	if err != nil {
		t.Fatalf("Error creating UDNNetInfo: %v", err)
	}
	// define node configs
	nodeAInfo := getNodeInfo(nodeA, []string{nodeAHostAddress}, nil)
	nodeBInfo := getNodeInfo(nodeB, []string{nodeBHostAddress}, nil)

	nodeAMultiAddressesV4 := []string{nodeAHostAddress, nodeAHostAddress2, nodeAHostAddress3}
	nodeAMultiAddressesV6 := []string{nodeAHostAddressV6, nodeAHostAddress2V6}

	nodeAInfoMultiIP := getNodeInfo(nodeA, nodeAMultiAddressesV4, nodeAMultiAddressesV6)

	type networkData struct {
		netInfo              util.NetInfo
		initialDb            []libovsdbtest.TestData
		expectedDb           []libovsdbtest.TestData
		dbStateAfterDeleting []libovsdbtest.TestData
	}
	tests := []struct {
		name         string
		nodeAInfo    *nodeInfo
		nodeBInfo    *nodeInfo
		enableIPv6   bool
		slices       []discovery.EndpointSlice
		service      *v1.Service
		networks     []networkData
		gatewayMode  string
		nodeToDelete string
	}{

		{
			name:      "create service from Single Stack Service without endpoints",
			nodeAInfo: nodeAInfo,
			nodeBInfo: nodeBInfo,
			slices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ab23",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports:       []discovery.EndpointPort{},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints:   []discovery.Endpoint{},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  serviceClusterIP,
					ClusterIPs: []string{serviceClusterIP},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       servicePort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt32(outPort),
					}},
				},
			},
			networks: []networkData{
				{
					netInfo: &util.DefaultNetInfo{},
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
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l3UDN,
					initialDb: []libovsdbtest.TestData{
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l2UDN,
					initialDb: []libovsdbtest.TestData{
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
			},
		},
		{
			name:      "update service without endpoints",
			nodeAInfo: nodeAInfo,
			nodeBInfo: nodeBInfo,
			slices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ab23",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports:       []discovery.EndpointPort{},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints:   []discovery.Endpoint{},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  serviceClusterIP,
					ClusterIPs: []string{serviceClusterIP},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       servicePort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt32(outPort),
					}},
				},
			},
			networks: []networkData{
				{
					netInfo: &util.DefaultNetInfo{},
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
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
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
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
						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l3UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork("wrong-switch", []string{}, l3UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						nodeLogicalRouterForNetwork("node-c", []string{}, l3UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork("wrong-switch", []string{}, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork("node-c", []string{}, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l2UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						nodeLogicalRouterForNetwork("node-c", []string{}, l2UDN, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork("node-c", []string{}, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
			},
		},
		{
			name:      "transition to endpoints, create nodeport",
			nodeAInfo: nodeAInfo,
			nodeBInfo: nodeBInfo,
			slices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ab1",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports: []discovery.EndpointPort{
						{
							Protocol: &tcp,
							Port:     &outPort,
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Conditions: discovery.EndpointConditions{
								Ready: utilpointer.Bool(true),
							},
							Addresses: []string{nodeAEndpoint},
							NodeName:  &nodeA,
						},
						{
							Conditions: discovery.EndpointConditions{
								Ready: utilpointer.Bool(true),
							},
							Addresses: []string{nodeBEndpointIP},
							NodeName:  &nodeB,
						},
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  serviceClusterIP,
					ClusterIPs: []string{serviceClusterIP},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       servicePort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt32(outPort),
						NodePort:   nodePort,
					}},
				},
			},
			networks: []networkData{
				{
					netInfo: &util.DefaultNetInfo{},
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
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
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l3UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l3UDN, nodeAEndpoint, nodeBEndpointIP),

						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l2UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l2UDN, nodeAEndpoint, nodeBEndpointIP),

						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
			},
		},
		{
			name:      "deleting a node should not leave stale load balancers",
			nodeAInfo: nodeAInfo,
			nodeBInfo: nodeBInfo,
			slices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ab1",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports: []discovery.EndpointPort{
						{
							Protocol: &tcp,
							Port:     &outPort,
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Conditions: discovery.EndpointConditions{
								Ready: utilpointer.Bool(true),
							},
							Addresses: []string{nodeAEndpoint},
							NodeName:  &nodeA,
						},
						{
							Conditions: discovery.EndpointConditions{
								Ready: utilpointer.Bool(true),
							},
							Addresses: []string{nodeBEndpointIP},
							NodeName:  &nodeB,
						},
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  serviceClusterIP,
					ClusterIPs: []string{serviceClusterIP},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       servicePort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt32(outPort),
						NodePort:   nodePort,
					}},
				},
			},
			networks: []networkData{
				{
					netInfo: &util.DefaultNetInfo{},
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
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
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
					dbStateAfterDeleting: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l3UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l3UDN, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
					dbStateAfterDeleting: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l3UDN, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),
						nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
				{
					netInfo: l2UDN,
					initialDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"192.168.0.1:6443": "",
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l2UDN, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
					dbStateAfterDeleting: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpoint, nodeBEndpointIP),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, l2UDN, nodeAEndpoint, nodeBEndpointIP),
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitch(nodeB, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouter(nodeB, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeB, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN, nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN)),

						nodeIPTemplate(nodeAInfo),
						nodeIPTemplate(nodeBInfo),
					},
				},
			},

			nodeToDelete: nodeA,
		},
		{
			// Test for multiple IP support in Template LBs (https://github.com/ovn-org/ovn-kubernetes/pull/3557)
			name:       "NodePort service, multiple IP addresses, ETP=cluster",
			enableIPv6: true,
			nodeAInfo:  nodeAInfoMultiIP,
			nodeBInfo:  nil,
			slices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ipv4",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outPort}},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints:   kubetest.MakeReadyEndpointList(nodeA, nodeAEndpoint, nodeAEndpoint2),
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName + "ipv6",
						Namespace: ns,
						Labels:    map[string]string{discovery.LabelServiceName: serviceName},
					},
					Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outPort}},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints:   kubetest.MakeReadyEndpointList(nodeA, nodeAEndpointV6, nodeAEndpoint2V6),
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeNodePort,
					ClusterIP:             serviceClusterIP,
					ClusterIPs:            []string{serviceClusterIP, serviceClusterIPv6},
					IPFamilies:            []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
					Selector:              map[string]string{"foo": "bar"},
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
					Ports: []v1.ServicePort{{
						Port:       servicePort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt32(outPort),
						NodePort:   30123,
					}},
				},
			},
			networks: []networkData{
				{
					netInfo: &util.DefaultNetInfo{},
					initialDb: []libovsdbtest.TestData{
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),

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
								IPAndPort(serviceClusterIP, servicePort):   formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								IPAndPort(serviceClusterIPv6, servicePort): formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol),
							Name:     nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol),
							Options:  templateServicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv4_1:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_2:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_0:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv6Protocol),
							Name:     nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv6Protocol),
							Options:  templateServicesOptionsV6(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv6_1:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
								"^NODEIP_IPv6_0:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
						},
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalRouter(nodeA, initialLrGroups),
						lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
						lbGroup(types.ClusterSwitchLBGroupName,
							nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol),
							nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv6Protocol)),
						lbGroup(types.ClusterRouterLBGroupName,
							nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol),
							nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv6Protocol)),

						&nbdb.ChassisTemplateVar{
							UUID: nodeA, Chassis: nodeA,
							Variables: map[string]string{
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeAMultiAddressesV4[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeAMultiAddressesV4[1],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeAMultiAddressesV4[2],

								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeAMultiAddressesV6[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeAMultiAddressesV6[1],
							},
						},
					},
				},
				{
					netInfo: l3UDN,
					initialDb: []libovsdbtest.TestData{
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, l3UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l3UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l3UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l3UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort):   formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								IPAndPort(serviceClusterIPv6, servicePort): formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN),
							Name:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN),
							Options:  templateServicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv4_1:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_2:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_0:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l3UDN),
							Name:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l3UDN),
							Options:  templateServicesOptionsV6(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv6_1:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
								"^NODEIP_IPv6_0:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l3UDN.GetNetworkName()),
						},

						nodeLogicalSwitchForNetwork(nodeAInfo.name, initialLsGroups, l3UDN),
						nodeLogicalRouterForNetwork(nodeAInfo.name, initialLrGroups, l3UDN),

						nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
						nodeLogicalRouter(nodeAInfo.name, initialLrGroups),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),

						lbGroupForNetwork(types.ClusterLBGroupName, l3UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l3UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName,
							l3UDN,
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN),
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l3UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName,
							l3UDN,
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l3UDN),
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l3UDN)),

						&nbdb.ChassisTemplateVar{
							UUID: nodeAInfo.chassisID, Chassis: nodeAInfo.chassisID,
							Variables: map[string]string{
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeAMultiAddressesV4[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeAMultiAddressesV4[1],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeAMultiAddressesV4[2],

								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeAMultiAddressesV6[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeAMultiAddressesV6[1],
							},
						},
					},
				},
				{
					netInfo: l2UDN,
					initialDb: []libovsdbtest.TestData{
						nodeLogicalSwitch(nodeA, initialLsGroups),
						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),

						nodeLogicalRouter(nodeA, initialLrGroups),
						nodeLogicalRouterForNetwork(nodeA, initialLrGroups, l2UDN),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),
						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName, l2UDN),
						lbGroupForNetwork(types.ClusterRouterLBGroupName, l2UDN),
					},
					expectedDb: []libovsdbtest.TestData{
						&nbdb.LoadBalancer{
							UUID:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Name:     clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN),
							Options:  servicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								IPAndPort(serviceClusterIP, servicePort):   formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								IPAndPort(serviceClusterIPv6, servicePort): formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN),
							Name:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN),
							Options:  templateServicesOptions(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv4_1:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_2:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
								"^NODEIP_IPv4_0:30123": formatEndpoints(outPort, nodeAEndpoint, nodeAEndpoint2),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},
						&nbdb.LoadBalancer{
							UUID:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l2UDN),
							Name:     nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l2UDN),
							Options:  templateServicesOptionsV6(),
							Protocol: &nbdb.LoadBalancerProtocolTCP,
							Vips: map[string]string{
								"^NODEIP_IPv6_1:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
								"^NODEIP_IPv6_0:30123": formatEndpoints(outPort, nodeAEndpointV6, nodeAEndpoint2V6),
							},
							ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), l2UDN.GetNetworkName()),
						},

						nodeLogicalSwitchForNetwork("", initialLsGroups, l2UDN),
						nodeLogicalRouterForNetwork(nodeAInfo.name, initialLrGroups, l2UDN),

						nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
						nodeLogicalRouter(nodeAInfo.name, initialLrGroups),

						lbGroup(types.ClusterLBGroupName),
						lbGroup(types.ClusterSwitchLBGroupName),
						lbGroup(types.ClusterRouterLBGroupName),

						lbGroupForNetwork(types.ClusterLBGroupName, l2UDN, clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, l2UDN)),
						lbGroupForNetwork(types.ClusterSwitchLBGroupName,
							l2UDN,
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN),
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l2UDN)),
						lbGroupForNetwork(types.ClusterRouterLBGroupName,
							l2UDN,
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv4Protocol, l2UDN),
							nodeMergedTemplateLoadBalancerNameForNetwork(ns, serviceName, v1.IPv6Protocol, l2UDN)),

						&nbdb.ChassisTemplateVar{
							UUID: nodeAInfo.chassisID, Chassis: nodeAInfo.chassisID,
							Variables: map[string]string{
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeAMultiAddressesV4[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeAMultiAddressesV4[1],
								makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeAMultiAddressesV4[2],

								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeAMultiAddressesV6[0],
								makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeAMultiAddressesV6[1],
							},
						},
					},
				},
			},
		},
	}

	for i, tt := range tests {
		for _, network := range tt.networks {
			t.Run(fmt.Sprintf("%d_%s_%s", i, tt.name, network.netInfo.GetNetworkName()), func(t *testing.T) {

				g := gomega.NewGomegaWithT(t)

				// Setup test-dependent parameters (default network vs UDN)
				netInfo := network.netInfo
				initialDb := network.initialDb
				expectedDb := network.expectedDb
				dbStateAfterDeleting := network.dbStateAfterDeleting
				if !netInfo.IsDefault() {
					nodeAInfo.gatewayRouterName = netInfo.GetNetworkScopedGWRouterName(nodeAInfo.gatewayRouterName)
					nodeAInfo.switchName = netInfo.GetNetworkScopedSwitchName(nodeAInfo.switchName)
					nodeBInfo.gatewayRouterName = netInfo.GetNetworkScopedGWRouterName(nodeBInfo.gatewayRouterName)
					nodeBInfo.switchName = netInfo.GetNetworkScopedSwitchName(nodeBInfo.switchName)

				}

				if tt.gatewayMode != "" {
					config.Gateway.Mode = config.GatewayMode(tt.gatewayMode)
				} else {
					config.Gateway.Mode = config.GatewayModeShared
				}

				if tt.enableIPv6 {
					config.IPv6Mode = true
					defer func() { config.IPv6Mode = false }()
				}

				// Create services controller
				var controller *serviceController
				var err error

				controller, err = newControllerWithDBSetupForNetwork(libovsdbtest.TestSetup{NBData: initialDb}, netInfo, ns)
				if err != nil {
					t.Fatalf("Error creating controller: %v", err)
				}
				defer controller.close()

				// Add k8s objects
				for _, slice := range tt.slices {
					controller.endpointSliceStore.Add(&slice)
				}
				controller.serviceStore.Add(tt.service)

				// Setup node tracker
				controller.nodeTracker.nodes = map[string]nodeInfo{}
				if tt.nodeAInfo != nil {
					controller.nodeTracker.nodes[nodeA] = *tt.nodeAInfo
				}
				if tt.nodeBInfo != nil {
					controller.nodeTracker.nodes[nodeB] = *tt.nodeBInfo
				}

				// Add mirrored endpoint slices when the controller runs on a UDN
				if !netInfo.IsDefault() {
					for _, slice := range tt.slices {
						controller.endpointSliceStore.Add(kubetest.MirrorEndpointSlice(&slice, netInfo.GetNetworkName(), true))
					}
				}

				// Trigger services controller
				controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

				err = controller.syncService(namespacedServiceName(ns, serviceName))
				if err != nil {
					t.Fatalf("syncServices error: %v", err)
				}

				// Check OVN DB
				g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))

				// If the test requires a node to be deleted, remove it from the node tracker,
				// sync the service controller and check the OVN DB
				if tt.nodeToDelete != "" {
					controller.nodeTracker.removeNode(tt.nodeToDelete)

					g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())

					g.Expect(controller.nbClient).To(libovsdbtest.HaveData(dbStateAfterDeleting))
				}
			})
		}

	}
}

func nodeLogicalSwitch(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	return nodeLogicalSwitchForNetwork(nodeName, lbGroups, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func nodeLogicalSwitchForNetwork(nodeName string, lbGroups []string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	var externalIDs map[string]string
	lbGroupsForNetwork := lbGroups

	switchName := nodeSwitchNameForNetwork(nodeName, netInfo)

	if netInfo.IsPrimaryNetwork() {
		for _, lbGroup := range lbGroups {
			lbGroupsForNetwork = append(lbGroupsForNetwork, netInfo.GetNetworkScopedLoadBalancerGroupName(lbGroup))
		}
		externalIDs = getExternalIDsForNetwork(netInfo.GetNetworkName())
	}
	ls := &nbdb.LogicalSwitch{
		UUID:              switchName,
		Name:              switchName,
		LoadBalancerGroup: lbGroupsForNetwork,
		ExternalIDs:       externalIDs,
	}

	if len(namespacedServiceNames) > 0 {
		ls.LoadBalancer = namespacedServiceNames
	}
	return ls
}

func nodeLogicalRouter(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	return nodeLogicalRouterForNetwork(nodeName, lbGroups, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func nodeLogicalRouterForNetwork(nodeName string, lbGroups []string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	var externalIDs map[string]string
	lbGroupsForNetwork := lbGroups

	routerName := nodeGWRouterNameForNetwork(nodeName, netInfo)

	if netInfo.IsPrimaryNetwork() {
		for _, lbGroup := range lbGroups {
			lbGroupsForNetwork = append(lbGroupsForNetwork, netInfo.GetNetworkScopedLoadBalancerGroupName(lbGroup))
		}
		externalIDs = getExternalIDsForNetwork(netInfo.GetNetworkName())
	}

	lr := &nbdb.LogicalRouter{
		UUID:              routerName,
		Name:              routerName,
		LoadBalancerGroup: lbGroups,
		ExternalIDs:       externalIDs,
	}

	if len(namespacedServiceNames) > 0 {
		lr.LoadBalancer = namespacedServiceNames
	}

	return lr
}

func nodeSwitchName(nodeName string) string {
	return nodeSwitchNameForNetwork(nodeName, &util.DefaultNetInfo{})
}

func nodeSwitchNameForNetwork(nodeName string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedSwitchName(fmt.Sprintf("switch-%s", nodeName))
}

func nodeGWRouterName(nodeName string) string {
	return nodeGWRouterNameForNetwork(nodeName, &util.DefaultNetInfo{})
}

func nodeGWRouterNameForNetwork(nodeName string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedGWRouterName(fmt.Sprintf("gr-%s", nodeName))
}

func lbGroup(name string, namespacedServiceNames ...string) *nbdb.LoadBalancerGroup {
	return lbGroupForNetwork(name, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func lbGroupForNetwork(name string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LoadBalancerGroup {
	LBGroupName := netInfo.GetNetworkScopedLoadBalancerGroupName(name)
	lbg := &nbdb.LoadBalancerGroup{
		UUID: LBGroupName,
		Name: LBGroupName,
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

func clusterWideTCPServiceLoadBalancerName(ns string, serviceName string) string {
	return clusterWideTCPServiceLoadBalancerNameForNetwork(ns, serviceName, &util.DefaultNetInfo{})
}

func clusterWideTCPServiceLoadBalancerNameForNetwork(ns string, serviceName string, netInfo util.NetInfo) string {
	baseName := fmt.Sprintf("Service_%s_TCP_cluster", namespacedServiceName(ns, serviceName))
	return netInfo.GetNetworkScopedLoadBalancerName(baseName)
}

func nodeMergedTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return nodeMergedTemplateLoadBalancerNameForNetwork(serviceNamespace, serviceName, addressFamily, &util.DefaultNetInfo{})
}

func nodeMergedTemplateLoadBalancerNameForNetwork(serviceNamespace string, serviceName string, addressFamily v1.IPFamily, netInfo util.NetInfo) string {
	baseName := fmt.Sprintf(
		"Service_%s/%s_TCP_node_switch_template_%s_merged",
		serviceNamespace,
		serviceName,
		addressFamily)
	return netInfo.GetNetworkScopedLoadBalancerName(baseName)
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

func getExternalIDsForNetwork(network string) map[string]string {
	if network == types.DefaultNetworkName {
		return map[string]string{}
	}

	return map[string]string{
		types.NetworkRoleExternalID: types.NetworkRolePrimary,
		types.NetworkExternalID:     network,
	}
}

func loadBalancerExternalIDs(namespacedServiceName string) map[string]string {
	return loadBalancerExternalIDsForNetwork(namespacedServiceName, types.DefaultNetworkName)
}

func loadBalancerExternalIDsForNetwork(namespacedServiceName string, network string) map[string]string {
	externalIDs := map[string]string{
		types.LoadBalancerKindExternalID:  "Service",
		types.LoadBalancerOwnerExternalID: namespacedServiceName,
	}
	maps.Copy(externalIDs, getExternalIDsForNetwork(network))
	return externalIDs

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
	return nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, serviceNamespace, outputPort, &util.DefaultNetInfo{}, endpointIPs...)
}

func nodeMergedTemplateLoadBalancerForNetwork(nodePort int32, serviceName string, serviceNamespace string, outputPort int32, netInfo util.NetInfo, endpointIPs ...string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeMergedTemplateLoadBalancerNameForNetwork(serviceNamespace, serviceName, v1.IPv4Protocol, netInfo),
		Name:     nodeMergedTemplateLoadBalancerNameForNetwork(serviceNamespace, serviceName, v1.IPv4Protocol, netInfo),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			IPAndPort(refTemplate(nodeTemplateIP.Name), nodePort): formatEndpoints(outputPort, endpointIPs...),
		},
		ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(serviceNamespace, serviceName), netInfo.GetNetworkName()),
	}
}

func refTemplate(template string) string {
	return "^" + template
}

func formatEndpoints(outputPort int32, ips ...string) string {
	var endpoints []string
	for _, ip := range ips {
		endpoints = append(endpoints, IPAndPort(ip, outputPort))
	}
	return strings.Join(endpoints, ",")
}

func IPAndPort(ip string, port int32) string {
	ipStr := ip
	if utilnet.IsIPv6String(ip) {
		ipStr = "[" + ip + "]"
	}

	return fmt.Sprintf("%s:%d", ipStr, port)
}

func getNodeInfo(nodeName string, nodeIPsV4 []string, nodeIPsV6 []string) *nodeInfo {
	var gwAddresses []net.IP
	ips := []net.IP{}

	if len(nodeIPsV4) > 0 {
		gwAddresses = append(gwAddresses, net.ParseIP(nodeIPsV4[0]))
		for _, ip := range nodeIPsV4 {
			ips = append(ips, net.ParseIP(ip))
		}
	}
	if len(nodeIPsV6) > 0 {
		gwAddresses = append(gwAddresses, net.ParseIP(nodeIPsV6[0]))
		for _, ip := range nodeIPsV6 {
			ips = append(ips, net.ParseIP(ip))
		}
	}

	return &nodeInfo{
		name:               nodeName,
		l3gatewayAddresses: gwAddresses,
		hostAddresses:      ips,
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

func deleteTestNBGlobal(nbClient libovsdbclient.Client) error {
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
