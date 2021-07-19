package ovn

import (
	"fmt"
	"net"
	"strings"

	goovn "github.com/ebay/go-ovn"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"
)

var (
	nodeName = "test-node"
)

func generateGatewayInitExpectedNB(nodeName string, clusterIPSubnets []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, joinLRPIPs, defLRPIPs []*net.IPNet) []libovsdb.TestData {

	GRName := "GR_" + nodeName
	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + GRName
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + GRName
	externalSwitch := fmt.Sprintf("%s%s", types.ExternalSwitchPrefix, nodeName)
	externalRouterPort := types.GWRouterToExtSwitchPrefix + GRName
	externalSwitchPortToRouter := types.EXTSwitchToGWRouterPrefix + GRName

	testData := []libovsdb.TestData{}

	networks := []string{}
	ovnClusterRouterStaticRoutes := []string{}
	for i, joinLRPIP := range joinLRPIPs {
		networks = append(networks, joinLRPIP.String())
		joinStaticRouteNamedUUID := fmt.Sprintf("join-static-route-ovn-cluster-router-%v-UUID", i)
		ovnClusterRouterStaticRoutes = append(ovnClusterRouterStaticRoutes, joinStaticRouteNamedUUID)
		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:     joinStaticRouteNamedUUID,
			IPPrefix: joinLRPIP.IP.String(),
			Nexthop:  joinLRPIP.IP.String(),
		})
	}
	testData = append(testData, &nbdb.LogicalRouterPort{
		UUID:     gwRouterPort + "-UUID",
		Name:     gwRouterPort,
		MAC:      util.IPAddrToHWAddr(joinLRPIPs[0].IP).String(),
		Networks: networks,
	})
	grStaticRoutes := []string{}
	for i, subnet := range clusterIPSubnets {
		nexthop, _ := util.MatchIPNetFamily(utilnet.IsIPv6CIDR(subnet), defLRPIPs)
		grStaticRouteNamedUUID := fmt.Sprintf("static-subnet-route-%v-UUID", i)
		grStaticRoutes = append(grStaticRoutes, grStaticRouteNamedUUID)
		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:     grStaticRouteNamedUUID,
			IPPrefix: subnet.String(),
			Nexthop:  nexthop.IP.String(),
		})

	}
	for i, hostSubnet := range hostSubnets {
		joinLRPIP, _ := util.MatchIPNetFamily(utilnet.IsIPv6CIDR(hostSubnet), joinLRPIPs)
		ocrStaticRouteNamedUUID := fmt.Sprintf("subnet-static-route-ovn-cluster-router-%v-UUID", i)
		ovnClusterRouterStaticRoutes = append(ovnClusterRouterStaticRoutes, ocrStaticRouteNamedUUID)
		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:     ocrStaticRouteNamedUUID,
			Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			IPPrefix: hostSubnet.String(),
			Nexthop:  joinLRPIP.IP.String(),
		})
	}
	for i, nexthop := range l3GatewayConfig.NextHops {
		var allIPs string
		if utilnet.IsIPv6(nexthop) {
			allIPs = "::/0"
		} else {
			allIPs = "0.0.0.0/0"
		}

		nexthopStaticRouteNamedUUID := fmt.Sprintf("static-nexthop-route-%v-UUID", i)
		grStaticRoutes = append(grStaticRoutes, nexthopStaticRouteNamedUUID)

		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:       nexthopStaticRouteNamedUUID,
			IPPrefix:   allIPs,
			Nexthop:    nexthop.String(),
			OutputPort: &externalRouterPort,
		})
	}
	networks = []string{}
	physicalIPs := []string{}
	for _, ip := range l3GatewayConfig.IPAddresses {
		networks = append(networks, ip.String())
		physicalIPs = append(physicalIPs, ip.IP.String())
	}
	testData = append(testData, &nbdb.LogicalRouterPort{
		UUID: externalRouterPort + "-UUID",
		Name: externalRouterPort,
		MAC:  l3GatewayConfig.MACAddress.String(),
		ExternalIDs: map[string]string{
			"gateway-physical-ip": "yes",
		},
		Networks: networks,
	})
	testData = append(testData, &nbdb.LogicalRouter{
		UUID: GRName + "-UUID",
		Name: GRName,
		Options: map[string]string{
			"lb_force_snat_ip":              "router_ip",
			"snat-ct-zone":                  "0",
			"always_learn_from_arp_request": "false",
			"dynamic_neigh_routers":         "true",
			"chassis":                       l3GatewayConfig.ChassisID,
		},
		ExternalIDs: map[string]string{
			"physical_ip":  physicalIPs[0],
			"physical_ips": strings.Join(physicalIPs, ","),
		},
		Ports:        []string{gwRouterPort + "-UUID", externalRouterPort + "-UUID"},
		StaticRoutes: grStaticRoutes,
	})
	testData = append(testData, &nbdb.LogicalRouter{
		UUID:         types.OVNClusterRouter + "-UUID",
		Name:         types.OVNClusterRouter,
		StaticRoutes: ovnClusterRouterStaticRoutes,
	})
	externalLogicalSwitchPort := &nbdb.LogicalSwitchPort{
		UUID:      l3GatewayConfig.InterfaceID + "-UUID",
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": types.PhysicalNetworkName,
		},
		Name: l3GatewayConfig.InterfaceID,
	}
	if l3GatewayConfig.VLANID != nil {
		intVlanID := int(*l3GatewayConfig.VLANID)
		externalLogicalSwitchPort.TagRequest = &intVlanID
	}
	testData = append(testData, externalLogicalSwitchPort)
	testData = append(testData,
		&nbdb.LogicalSwitchPort{
			UUID:      gwSwitchPort + "-UUID",
			Name:      gwSwitchPort,
			Type:      "router",
			Addresses: []string{"router"},
			Options: map[string]string{
				"router-port": gwRouterPort,
			},
		},
		&nbdb.LogicalSwitchPort{
			UUID: externalSwitchPortToRouter + "-UUID",
			Name: externalSwitchPortToRouter,
			Type: "router",
			Options: map[string]string{
				"router-port": externalRouterPort,
			},
			Addresses: []string{l3GatewayConfig.MACAddress.String()},
		},
		&nbdb.LogicalSwitch{
			UUID:  types.OVNJoinSwitch + "-UUID",
			Name:  types.OVNJoinSwitch,
			Ports: []string{gwSwitchPort + "-UUID"},
		},
		&nbdb.LogicalSwitch{
			UUID: nodeName + "-UUID",
			Name: nodeName,
		},
		&nbdb.LogicalSwitch{
			UUID:  externalSwitch + "-UUID",
			Name:  externalSwitch,
			Ports: []string{l3GatewayConfig.InterfaceID + "-UUID", externalSwitchPortToRouter + "-UUID"},
		})
	return testData
}

var _ = ginkgo.Describe("Gateway Init Operations", func() {
	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		// TODO make contexts here for shared gw mode and local gw mode, right now this only tests shared gw
		config.Gateway.Mode = config.GatewayModeShared
		// Create new LBCache
		ovnlb.TestOnlySetCache(nil)
	})

	ginkgo.It("correctly sorts gateway routers", func() {
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
			Output: `node5      chassis=842fdade-747a-43b8-b40a-d8e8e26379fa lb_force_snat_ip=100.64.0.5
node2 chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.2
node1 chassis=d17ddb5a-050d-42ab-ab50-7c6ce79a8f2e lb_force_snat_ip=100.64.0.1
node4 chassis=912d592c-904c-40cd-9ef1-c2e5b49a33dd lb_force_snat_ip=100.64.0.4`,
		})

		err := util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("ignores malformatted gateway router entires", func() {
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
			Output: `node5      chassis=842fdade-747a-43b8-b40a-d8e8e26379fa lb_force_snat_ip=100.64.0.5
node2 chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=asdfsadf
node1 chassis=d17ddb5a-050d-42ab-ab50-7c6ce79a8f2e lb_force_xxxxxxx=100.64.0.1
node4 chassis=912d592c-904c-40cd-9ef1-c2e5b49a33dd lb_force_snat_ip=100.64.0.4`,
		})

		err := util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("creates an IPv4 gateway in OVN", func() {

		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
				&nbdb.LogicalRouter{
					UUID: types.OVNClusterRouter + "-UUID",
					Name: types.OVNClusterRouter,
				},
				&nbdb.LogicalSwitch{
					UUID: nodeName + "-UUID",
					Name: nodeName,
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
		hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
		joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
		defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
		l3GatewayConfig := &util.L3GatewayConfig{
			Mode:           config.GatewayModeLocal,
			ChassisID:      "SYSTEM-ID",
			InterfaceID:    "INTERFACE-ID",
			MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
			IPAddresses:    ovntest.MustParseIPNets("169.254.33.2/24"),
			NextHops:       ovntest.MustParseIPs("169.254.33.1"),
			NodePortEnable: true,
		}
		sctpSupport := false

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --columns _uuid --format=csv --no-headings find nat external_ip=\"169.254.33.2\" type=snat logical_ip=\"10.128.0.0/14\"",
			"ovn-nbctl --timeout=15 --if-exists lr-nat-del GR_test-node snat 10.128.0.0/14",
			"ovn-nbctl --timeout=15 lr-nat-add GR_test-node snat 169.254.33.2 10.128.0.0/14",
		})

		err = clusterController.gatewayInit(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, sctpSupport, joinLRPIPs, defLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDatabaseState := generateGatewayInitExpectedNB(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs)
		gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())
	})

	ginkgo.It("creates an IPv6 gateway in OVN", func() {

		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
				&nbdb.LogicalRouter{
					UUID: types.OVNClusterRouter + "-UUID",
					Name: types.OVNClusterRouter,
				},
				&nbdb.LogicalSwitch{
					UUID: nodeName + "-UUID",
					Name: nodeName,
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		clusterIPSubnets := ovntest.MustParseIPNets("fd01::/48")
		hostSubnets := ovntest.MustParseIPNets("fd01:0:0:2::/64")
		joinLRPIPs := ovntest.MustParseIPNets("fd98::3/64")
		defLRPIPs := ovntest.MustParseIPNets("fd98::1/64")
		nodeName := "test-node"
		l3GatewayConfig := &util.L3GatewayConfig{
			Mode:           config.GatewayModeLocal,
			ChassisID:      "SYSTEM-ID",
			InterfaceID:    "INTERFACE-ID",
			MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
			IPAddresses:    ovntest.MustParseIPNets("fd99::2/64"),
			NextHops:       ovntest.MustParseIPs("fd99::1"),
			NodePortEnable: true,
		}
		sctpSupport := false

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --columns _uuid --format=csv --no-headings find nat external_ip=\"fd99::2\" type=snat logical_ip=\"fd01::/48\"",
			"ovn-nbctl --timeout=15 --if-exists lr-nat-del GR_test-node snat fd01::/48",
			"ovn-nbctl --timeout=15 lr-nat-add GR_test-node snat fd99::2 fd01::/48",
		})

		err = clusterController.gatewayInit(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, sctpSupport, joinLRPIPs, defLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDatabaseState := generateGatewayInitExpectedNB(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs)
		gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())
	})

	ginkgo.It("creates a dual-stack gateway in OVN", func() {

		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
				&nbdb.LogicalRouter{
					UUID: types.OVNClusterRouter + "-UUID",
					Name: types.OVNClusterRouter,
				},
				&nbdb.LogicalSwitch{
					UUID: nodeName + "-UUID",
					Name: nodeName,
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14", "fd01::/48")
		hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23", "fd01:0:0:2::/64")
		joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16", "fd98::3/64")
		defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16", "fd98::1/64")
		nodeName := "test-node"
		l3GatewayConfig := &util.L3GatewayConfig{
			Mode:           config.GatewayModeLocal,
			ChassisID:      "SYSTEM-ID",
			InterfaceID:    "INTERFACE-ID",
			MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
			IPAddresses:    ovntest.MustParseIPNets("169.254.33.2/24", "fd99::2/64"),
			NextHops:       ovntest.MustParseIPs("169.254.33.1", "fd99::1"),
			NodePortEnable: true,
		}
		sctpSupport := false

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --columns _uuid --format=csv --no-headings find nat external_ip=\"169.254.33.2\" type=snat logical_ip=\"10.128.0.0/14\"",
			"ovn-nbctl --timeout=15 --if-exists lr-nat-del GR_test-node snat 10.128.0.0/14",
			"ovn-nbctl --timeout=15 lr-nat-add GR_test-node snat 169.254.33.2 10.128.0.0/14",
			"ovn-nbctl --timeout=15 --columns _uuid --format=csv --no-headings find nat external_ip=\"fd99::2\" type=snat logical_ip=\"fd01::/48\"",
			"ovn-nbctl --timeout=15 --if-exists lr-nat-del GR_test-node snat fd01::/48",
			"ovn-nbctl --timeout=15 lr-nat-add GR_test-node snat fd99::2 fd01::/48",
		})

		err = clusterController.gatewayInit(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, sctpSupport, joinLRPIPs, defLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDatabaseState := generateGatewayInitExpectedNB(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs)
		gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())
	})

	ginkgo.It("cleans up a single-stack gateway in OVN", func() {
		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		nodeName := "test-node"

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouterPort{
					Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
					Networks: []string{"100.64.0.1/16"},
				},
				&nbdb.LoadBalancer{
					UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "default/kubernetes",
					},
					Vips: map[string]string{
						"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
						"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
					},
				},
				&nbdb.LogicalRouter{
					UUID: "GR_test-node",
					Name: "GR_test-node",
					LoadBalancer: []string{
						"Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					},
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		hostSubnet := ovntest.MustParseIPNet("10.130.0.0/23")
		const (
			nodeRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
		)

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"100.64.0.1\"",
			Output: nodeRouteUUID,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove logical_router ovn_cluster_router static_routes " + nodeRouteUUID,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exist lsp-del jtor-GR_test-node",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exist lr-del GR_test-node",
			"ovn-nbctl --timeout=15 --if-exist ls-del ext_test-node",
			"ovn-nbctl --timeout=15 --if-exist ls-del ext_ext_test-node",
		})

		cleanupPBRandNATRules(fexec, nodeName, []*net.IPNet{hostSubnet})

		err = clusterController.gatewayCleanup(nodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())

		libovsdbOvnNBClient.Close()
	})

	ginkgo.It("cleans up a dual-stack gateway in OVN", func() {
		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		nodeName := "test-node"

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouterPort{
					Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
					Networks: []string{"100.64.0.1/16", "fd98::1/64"},
				},
				&nbdb.LoadBalancer{
					UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": "default/kubernetes",
					},
					Vips: map[string]string{
						"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
						"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
					},
				},
				&nbdb.LogicalRouter{
					UUID: "GR_test-node",
					Name: "GR_test-node",
					LoadBalancer: []string{
						"Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					},
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23", "fd01:0:0:2::/64")
		const (
			v4RouteUUID    string = "0cac12cf-3e0f-4682-b028-5ea2e0001962"
			v6RouteUUID    string = "0cac12cf-4682-3e0f-b028-5ea2e0001962"
			v4mgtRouteUUID string = "0cac12cf-3e0f-4682-b028-5ea2e0001963"
			v6mgtRouteUUID string = "0cac12cf-4682-3e0f-b028-5ea2e0001963"
		)

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"100.64.0.1\"",
			Output: v4RouteUUID,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove logical_router ovn_cluster_router static_routes " + v4RouteUUID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_router_static_route nexthop=\"fd98::1\"",
			Output: v6RouteUUID,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove logical_router ovn_cluster_router static_routes " + v6RouteUUID,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exist lsp-del jtor-GR_test-node",
		})

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exist lr-del GR_test-node",
			"ovn-nbctl --timeout=15 --if-exist ls-del ext_test-node",
			"ovn-nbctl --timeout=15 --if-exist ls-del ext_ext_test-node",
		})

		cleanupPBRandNATRules(fexec, nodeName, hostSubnets)

		err = clusterController.gatewayCleanup(nodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())

		libovsdbOvnNBClient.Close()
	})

	ginkgo.It("removes leftover SNAT entries during init", func() {

		stopChan := make(chan struct{})
		defer close(stopChan)
		kubeFakeClient := fake.NewSimpleClientset()
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressIPClient:       egressIPFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}
		f, err := factory.NewMasterWatchFactory(fakeClient)

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
				&nbdb.LogicalRouter{
					UUID: types.OVNClusterRouter + "-UUID",
					Name: types.OVNClusterRouter,
				},
				&nbdb.LogicalSwitch{
					UUID: nodeName + "-UUID",
					Name: nodeName,
				},
			},
		}
		libovsdbOvnNBClient, libovsdbOvnSBClient, err := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		clusterController := NewOvnController(fakeClient, f, stopChan, addressset.NewFakeAddressSetFactory(),
			ovntest.NewMockOVNClient(goovn.DBNB), ovntest.NewMockOVNClient(goovn.DBSB),
			libovsdbOvnNBClient, libovsdbOvnSBClient,
			record.NewFakeRecorder(0))

		clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
		hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
		joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
		defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
		nodeName := "test-node"
		l3GatewayConfig := &util.L3GatewayConfig{
			Mode:           config.GatewayModeLocal,
			ChassisID:      "SYSTEM-ID",
			InterfaceID:    "INTERFACE-ID",
			MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
			IPAddresses:    ovntest.MustParseIPNets("169.254.33.2/24"),
			NextHops:       ovntest.MustParseIPs("169.254.33.1"),
			NodePortEnable: true,
		}
		sctpSupport := false
		config.Gateway.DisableSNATMultipleGWs = true

		fexec := ovntest.NewFakeExec()
		err = util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = clusterController.gatewayInit(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, sctpSupport, joinLRPIPs, defLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDatabaseState := generateGatewayInitExpectedNB(nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs)
		gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue())

	})
})
