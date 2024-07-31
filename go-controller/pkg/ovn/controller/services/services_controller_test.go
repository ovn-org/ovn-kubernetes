package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	kube_test "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/exp/maps"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"
	utilpointer "k8s.io/utils/pointer"
)

var (
	nodeA = "node-a"
	nodeB = "node-b"

	serviceClusterIP   = "192.168.1.1"
	serviceClusterIPv6 = "fd00::7777:0:0:1" // as output by libovsdb
	servicePort        = int32(80)
	outPort            = int32(3456)
	tcp                = v1.ProtocolTCP

	initialLsGroups = []string{types.ClusterLBGroupName, types.ClusterSwitchLBGroupName}
	initialLrGroups = []string{types.ClusterLBGroupName, types.ClusterRouterLBGroupName}

	UDNNamespace   = "ns-udn"
	UDNNetworkName = "tenant-red"
	UDNNetInfo     = getSampleUDNNetInfo(UDNNamespace)

	clusterLBGroupNameUDN       = UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName)
	clusterSwitchLBGroupNameUDN = UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)
	clusterRouterLBGroupNameUDN = UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)

	initialLsGroupsUDN = []string{clusterLBGroupNameUDN, clusterSwitchLBGroupNameUDN}
	initialLrGroupsUDN = []string{clusterLBGroupNameUDN, clusterRouterLBGroupNameUDN}
)

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
	NADStore           cache.Store
	libovsdbCleanup    *libovsdbtest.Context
}

func newControllerWithDBSetupForNetwork(dbSetup libovsdbtest.TestSetup, netInfo util.NetInfo, testUDN bool, namespaceForNAD string) (*serviceController, error) {
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
	var NADLister nadlister.NetworkAttachmentDefinitionLister
	if testUDN {
		NADLister = factoryMock.NADInformer().Lister()
	}

	controller, err := NewController(client.KubeClient,
		nbClient,
		factoryMock.ServiceCoreInformer(),
		factoryMock.EndpointSliceCoreInformer(),
		factoryMock.NodeCoreInformer(),
		NADLister,
		recorder,
		netInfo,
	)
	if err != nil {
		return nil, err
	}

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		if err = deleteTestNBGlobal(nbClient, "global"); err != nil {
			return nil, err
		}
	}

	if err = controller.initTopLevelCache(); err != nil {
		return nil, err
	}
	controller.useLBGroups = true
	controller.useTemplates = true

	// When testing services on UDN, add a NAD in the same namespace associated to the service
	if testUDN {
		if err = addSampleNAD(client, namespaceForNAD); err != nil {
			return nil, err
		}
	}

	return &serviceController{
		controller,
		factoryMock.ServiceCoreInformer().Informer().GetStore(), //,factoryMock.Core().V1().Services(),
		factoryMock.EndpointSliceInformer().GetStore(),
		factoryMock.NADInformer().Informer().GetStore(),
		cleanup,
	}, nil
}

func (c *serviceController) close() {
	c.libovsdbCleanup.Cleanup()
}

func getSampleUDNNetInfo(namespace string) util.NetInfo {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{
		Topology:   "layer3",
		NADName:    fmt.Sprintf("%s/nad1", namespace),
		MTU:        1400,
		Role:       "primary",
		Subnets:    "192.168.200.0/16",
		NetConf:    cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
		JoinSubnet: "100.66.0.0/16",
	})
	return netInfo
}

func addSampleNAD(client *util.OVNKubeControllerClientset, namespace string) error {
	_, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
		context.TODO(),
		kube_test.GenerateNAD(UDNNetworkName, UDNNetworkName, namespace, types.Layer3Topology, "10.128.0.0/16/24", types.NetworkRolePrimary),
		metav1.CreateOptions{})
	return err
}

// TestSyncServices - an end-to-end test for the services controller.
func TestSyncServices(t *testing.T) {
	initialMaxLength := format.MaxLength
	temporarilyEnableGomegaMaxLengthFormat()
	t.Cleanup(func() {
		restoreGomegaMaxLengthFormat(initialMaxLength)
	})

	const (
		nodeAEndpointIP = "10.128.0.2"
		nodeBEndpointIP = "10.128.1.2"
		nodeAHostIP     = "10.0.0.1"
		nodeBHostIP     = "10.0.0.2"

		nodePort = 8989
	)

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

	nodeAConfig := nodeConfig(nodeA, nodeAHostIP)
	nodeBConfig := nodeConfig(nodeB, nodeBHostIP)

	// Each test structure is filled in with the initial and expect OVN DB in the case the services controller
	// runs in the default cluster network (initialDb, expectedDb) and in case it runs in a UDN (initialDbUDN, expectedDbUDN).
	// In the UDN scenario, the expectation is that the OVN DB still contains objects for the default cluster network,
	// even though they're left empty for simplicity, and all OVN logic takes place in the UDN-specific OVN objects.
	tests := []struct {
		name                    string
		slice                   *discovery.EndpointSlice
		service                 *v1.Service
		initialDb               []libovsdbtest.TestData
		expectedDb              []libovsdbtest.TestData
		initialDbUDN            []libovsdbtest.TestData
		expectedDbUDN           []libovsdbtest.TestData
		gatewayMode             string
		nodeToDelete            string
		dbStateAfterDeleting    []libovsdbtest.TestData
		dbStateAfterDeletingUDN []libovsdbtest.TestData
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
				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			initialDbUDN: []libovsdbtest.TestData{
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetInfo.GetNetworkName()),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),

				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
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
			initialDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork("wrong-switch", []string{}, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork("node-c", []string{}, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
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
				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork("wrong-switch", []string{}, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork("node-c", []string{}, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),

				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
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
						Port:     &outPort,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{nodeAEndpointIP},
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
			initialDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, UDNNetworkName, nodeAEndpointIP, nodeBEndpointIP),

				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),

				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
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
						Port:     &outPort,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{nodeAEndpointIP},
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
			initialDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, UDNNetworkName, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),

				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			nodeToDelete: nodeA,

			dbStateAfterDeleting: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outPort, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
			dbStateAfterDeletingUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						IPAndPort(serviceClusterIP, servicePort): formatEndpoints(outPort, nodeAEndpointIP, nodeBEndpointIP),
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outPort, UDNNetworkName, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroups, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroups, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),

				nodeIPTemplate(nodeAConfig),
				nodeIPTemplate(nodeBConfig),
			},
		},
	}

	for i, tt := range tests {
		for _, testUDN := range []bool{true, false} {
			udnString := ""
			if testUDN {
				udnString = "_UDN"
			}
			t.Run(fmt.Sprintf("%d_%s%s", i, tt.name, udnString), func(t *testing.T) {

				g := gomega.NewGomegaWithT(t)

				var netInfo util.NetInfo

				netInfo = &util.DefaultNetInfo{}
				initialDb := tt.initialDb
				expectedDb := tt.expectedDb
				dbStateAfterDeleting := tt.dbStateAfterDeleting
				if testUDN {
					netInfo = UDNNetInfo
					initialDb = tt.initialDbUDN
					expectedDb = tt.expectedDbUDN
					dbStateAfterDeleting = tt.dbStateAfterDeletingUDN
					nodeAConfig.gatewayRouterName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeAConfig.gatewayRouterName)
					nodeAConfig.switchName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeAConfig.switchName)
					nodeBConfig.gatewayRouterName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeBConfig.gatewayRouterName)
					nodeBConfig.switchName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeBConfig.switchName)

				}

				if tt.gatewayMode != "" {
					globalconfig.Gateway.Mode = globalconfig.GatewayMode(tt.gatewayMode)
				} else {
					globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
				}

				var controller *serviceController
				var err error

				controller, err = newControllerWithDBSetupForNetwork(libovsdbtest.TestSetup{NBData: initialDb}, netInfo, testUDN, ns)
				if err != nil {
					t.Fatalf("Error creating controller: %v", err)
				}
				defer controller.close()

				// Add objects to the Store
				controller.endpointSliceStore.Add(tt.slice)
				controller.serviceStore.Add(tt.service)
				controller.nodeTracker.nodes = map[string]nodeInfo{
					nodeA: *nodeAConfig,
					nodeB: *nodeBConfig,
				}

				if testUDN {
					// Add mirrored slices
					controller.endpointSliceStore.Add(kube_test.MirrorEndpointSlice(tt.slice, UDNNetInfo.GetNetworkName(), true))
				}
				controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

				err = controller.syncService(namespacedServiceName(ns, serviceName))
				if err != nil {
					t.Fatalf("syncServices error: %v", err)
				}

				g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))

				if tt.nodeToDelete != "" {
					controller.nodeTracker.removeNode(tt.nodeToDelete)

					g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())

					g.Expect(controller.nbClient).To(libovsdbtest.HaveData(dbStateAfterDeleting))
				}
			})
		}

	}
}

func Test_ETPCluster_NodePort_Service_WithMultipleIPAddresses(t *testing.T) {

	ns := "testns"
	serviceName := "foo"

	endpoint1v4 := "10.128.0.2"
	endpoint2v4 := "10.128.1.2"
	endpoint1v6 := "fe00::5555:0:0:2"
	endpoint2v6 := "fe00::5555:0:0:3"

	nodeIPv4 := []net.IP{net.ParseIP("10.1.1.1"), net.ParseIP("10.2.2.2"), net.ParseIP("10.3.3.3")}
	nodeIPv6 := []net.IP{net.ParseIP("fd00:0:0:0:1::1"), net.ParseIP("fd00:0:0:0:2::2")}

	nodeAInfo := nodeInfo{
		name:               nodeA,
		l3gatewayAddresses: []net.IP{nodeIPv4[0], nodeIPv6[0]},
		hostAddresses:      append(nodeIPv4, nodeIPv6...),
		gatewayRouterName:  nodeGWRouterName(nodeA),
		switchName:         nodeSwitchName(nodeA),
		chassisID:          nodeA,
		zone:               types.OvnDefaultZone,
	}

	svc := &v1.Service{
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
	}

	endpointSliceV4 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv4",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outPort}},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeAInfo.name, endpoint1v4, endpoint2v4),
	}

	endpointSliceV6 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv6",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outPort}},
		AddressType: discovery.AddressTypeIPv6,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeAInfo.name, endpoint1v6, endpoint2v6),
	}
	initialDbDefault := []libovsdbtest.TestData{
		nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
		nodeLogicalRouter(nodeAInfo.name, initialLrGroups),

		lbGroup(types.ClusterLBGroupName),
		lbGroup(types.ClusterSwitchLBGroupName),
		lbGroup(types.ClusterRouterLBGroupName),
	}
	initialDbUDN := []libovsdbtest.TestData{
		nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
		nodeLogicalSwitchForNetwork(nodeAInfo.name, initialLsGroups, UDNNetInfo),

		nodeLogicalRouter(nodeAInfo.name, initialLrGroups),
		nodeLogicalRouterForNetwork(nodeAInfo.name, initialLrGroups, UDNNetInfo),

		lbGroup(types.ClusterLBGroupName),
		lbGroup(types.ClusterSwitchLBGroupName),
		lbGroup(types.ClusterRouterLBGroupName),
		lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
		lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
		lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
	}

	// expected DB if test is run on default cluster network
	expectedDbDefault := []libovsdbtest.TestData{
		&nbdb.LoadBalancer{
			UUID:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Name:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Options:  servicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				IPAndPort(serviceClusterIP, servicePort):   formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				IPAndPort(serviceClusterIPv6, servicePort): formatEndpoints(outPort, endpoint1v6, endpoint2v6),
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			Name:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			Options:  templateServicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv4_1:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				"^NODEIP_IPv4_2:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				"^NODEIP_IPv4_0:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol),
			Name:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol),
			Options:  templateServicesOptionsV6(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv6_1:30123": formatEndpoints(outPort, endpoint1v6, endpoint2v6),
				"^NODEIP_IPv6_0:30123": formatEndpoints(outPort, endpoint1v6, endpoint2v6),
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
		nodeLogicalRouter(nodeAInfo.name, initialLrGroups),
		lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name)),
		lbGroup(types.ClusterSwitchLBGroupName,
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol)),
		lbGroup(types.ClusterRouterLBGroupName,
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol)),

		&nbdb.ChassisTemplateVar{
			UUID: nodeAInfo.chassisID, Chassis: nodeAInfo.chassisID,
			Variables: map[string]string{
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeIPv4[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeIPv4[1].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeIPv4[2].String(),

				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeIPv6[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeIPv6[1].String(),
			},
		},
	}

	// expected DB if test is run on UDN
	expectedDbUDN := []libovsdbtest.TestData{
		&nbdb.LoadBalancer{
			UUID:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Name:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Options:  servicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				IPAndPort(serviceClusterIP, servicePort):   formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				IPAndPort(serviceClusterIPv6, servicePort): formatEndpoints(outPort, endpoint1v6, endpoint2v6),
			},
			ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(svc.Namespace, svc.Name), UDNNetworkName),
		},
		&nbdb.LoadBalancer{
			UUID:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			Name:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			Options:  templateServicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv4_1:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				"^NODEIP_IPv4_2:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
				"^NODEIP_IPv4_0:30123": formatEndpoints(outPort, endpoint1v4, endpoint2v4),
			},
			ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(svc.Namespace, svc.Name), UDNNetworkName),
		},
		&nbdb.LoadBalancer{
			UUID:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol),
			Name:     nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol),
			Options:  templateServicesOptionsV6(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv6_1:30123": formatEndpoints(outPort, endpoint1v6, endpoint2v6),
				"^NODEIP_IPv6_0:30123": formatEndpoints(outPort, endpoint1v6, endpoint2v6),
			},
			ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(svc.Namespace, svc.Name), UDNNetworkName),
		},

		nodeLogicalSwitchForNetwork(nodeAInfo.name, initialLsGroups, UDNNetInfo),
		nodeLogicalRouterForNetwork(nodeAInfo.name, initialLrGroups, UDNNetInfo),

		nodeLogicalSwitch(nodeAInfo.name, initialLsGroups),
		nodeLogicalRouter(nodeAInfo.name, initialLrGroups),

		lbGroup(types.ClusterLBGroupName),
		lbGroup(types.ClusterSwitchLBGroupName),
		lbGroup(types.ClusterRouterLBGroupName),

		lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name)),
		lbGroupForNetwork(types.ClusterSwitchLBGroupName,
			UDNNetInfo,
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol)),
		lbGroupForNetwork(types.ClusterRouterLBGroupName,
			UDNNetInfo,
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv4Protocol),
			nodeMergedTemplateLoadBalancerName(svc.Namespace, svc.Name, v1.IPv6Protocol)),

		&nbdb.ChassisTemplateVar{
			UUID: nodeAInfo.chassisID, Chassis: nodeAInfo.chassisID,
			Variables: map[string]string{
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeIPv4[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeIPv4[1].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeIPv4[2].String(),

				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeIPv6[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeIPv6[1].String(),
			},
		},
	}

	for _, testUDN := range []bool{false, true} {
		udnString := ""
		if testUDN {
			udnString = "_UDN"
		}
		t.Run(fmt.Sprintf("ETPCluster_NodePort_Service_WithMultipleIPAddresses%s", udnString), func(t *testing.T) {
			// setup gomega parameters
			g := gomega.NewGomegaWithT(t)
			initialMaxLength := format.MaxLength
			temporarilyEnableGomegaMaxLengthFormat()
			t.Cleanup(func() {
				restoreGomegaMaxLengthFormat(initialMaxLength)
			})

			// setup network-dependent parameters
			var netInfo util.NetInfo
			netInfo = &util.DefaultNetInfo{}
			initialDb := initialDbDefault
			expectedDb := expectedDbDefault
			if testUDN {
				netInfo = UDNNetInfo
				initialDb = initialDbUDN
				expectedDb = expectedDbUDN
				nodeAInfo.gatewayRouterName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeAInfo.gatewayRouterName)
				nodeAInfo.switchName = UDNNetInfo.GetNetworkScopedGWRouterName(nodeAInfo.switchName)
			}
			// setup global config
			globalconfig.IPv4Mode = true
			globalconfig.IPv6Mode = true
			_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
			_, cidr6, _ := net.ParseCIDR("fe00:0:0:0:5555::0/64")
			globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 16}, {CIDR: cidr6, HostSubnetLength: 26}} // 64

			// create services controller
			controller, err := newControllerWithDBSetupForNetwork(
				libovsdbtest.TestSetup{NBData: initialDb},
				netInfo,
				testUDN,
				ns)
			if err != nil {
				t.Fatalf("Error creating controller: %v", err)
			}
			defer controller.close()

			// add k8s objects
			controller.endpointSliceStore.Add(endpointSliceV4)
			controller.endpointSliceStore.Add(endpointSliceV6)
			controller.serviceStore.Add(svc)
			controller.nodeTracker.nodes = map[string]nodeInfo{nodeAInfo.name: nodeAInfo}
			if testUDN {
				// Add mirrored slices
				controller.endpointSliceStore.Add(kube_test.MirrorEndpointSlice(endpointSliceV4, UDNNetInfo.GetNetworkName(), true))
				controller.endpointSliceStore.Add(kube_test.MirrorEndpointSlice(endpointSliceV6, UDNNetInfo.GetNetworkName(), true))
			}

			// trigger services controller
			controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

			err = controller.syncService(namespacedServiceName(svc.Namespace, svc.Name))
			g.Expect(err).ToNot(gomega.HaveOccurred())

			// check OVN db
			g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))
		})
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
			lbGroupsForNetwork = append(lbGroupsForNetwork, UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(lbGroup))
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
func getExternalIDsForNetwork(network string) map[string]string {
	role := types.NetworkRoleDefault
	if network != types.DefaultNetworkName {
		role = types.NetworkRolePrimary
	}
	return map[string]string{
		types.NetworkRoleExternalID: role,
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
	return nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, serviceNamespace, outputPort, types.DefaultNetworkName, endpointIPs...)
}

func nodeMergedTemplateLoadBalancerForNetwork(nodePort int32, serviceName string, serviceNamespace string, outputPort int32, networkName string, endpointIPs ...string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Name:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			IPAndPort(refTemplate(nodeTemplateIP.Name), nodePort): formatEndpoints(outputPort, endpointIPs...),
		},
		ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(serviceNamespace, serviceName), networkName),
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
