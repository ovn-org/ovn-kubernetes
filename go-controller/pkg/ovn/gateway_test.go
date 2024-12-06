package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	v1 "k8s.io/api/core/v1"

	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	nodeName = "test-node"
)

func init() {
	// libovsdb matcher might produce a lengthy output that will be cropped by
	// default gomega output limit, set to 0 to unlimit.
	format.MaxLength = 0
}

func generateGatewayInitExpectedNB(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, nodeName string, clusterIPSubnets []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, joinLRPIPs, defLRPIPs []*net.IPNet, skipSnat bool, nodeMgmtPortIP,
	gatewayMTU string) []libovsdbtest.TestData {

	GRName := "GR_" + nodeName
	gwSwitchPort := types.JoinSwitchToGWRouterPrefix + GRName
	gwRouterPort := types.GWRouterToJoinSwitchPrefix + GRName
	externalSwitch := fmt.Sprintf("%s%s", types.ExternalSwitchPrefix, nodeName)
	externalRouterPort := types.GWRouterToExtSwitchPrefix + GRName
	externalSwitchPortToRouter := types.EXTSwitchToGWRouterPrefix + GRName

	networks := []string{}

	for i, joinLRPIP := range joinLRPIPs {
		networks = append(networks, joinLRPIP.String())
		joinStaticRouteNamedUUID := fmt.Sprintf("join-static-route-ovn-cluster-router-%v-UUID", i)
		expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, joinStaticRouteNamedUUID)
		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:     joinStaticRouteNamedUUID,
			IPPrefix: joinLRPIP.IP.String(),
			Nexthop:  joinLRPIP.IP.String(),
		})
	}
	var options map[string]string
	if gatewayMTU != "" {
		options = map[string]string{
			"gateway_mtu": gatewayMTU,
		}
	}
	testData = append(testData, &nbdb.LogicalRouterPort{
		UUID:     gwRouterPort + "-UUID",
		Name:     gwRouterPort,
		MAC:      util.IPAddrToHWAddr(joinLRPIPs[0].IP).String(),
		Networks: networks,
		Options:  options,
	})
	grStaticRoutes := []string{}
	var hasIPv4, hasIPv6 bool
	for i, subnet := range clusterIPSubnets {
		if utilnet.IsIPv6CIDR(subnet) {
			hasIPv6 = true
		} else {
			hasIPv4 = true
		}
		nexthop, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(subnet), defLRPIPs)
		grStaticRouteNamedUUID := fmt.Sprintf("static-subnet-route-%v-UUID", i)
		grStaticRoutes = append(grStaticRoutes, grStaticRouteNamedUUID)
		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:     grStaticRouteNamedUUID,
			IPPrefix: subnet.String(),
			Nexthop:  nexthop.IP.String(),
		})

	}
	if config.Gateway.Mode == config.GatewayModeShared {
		for i, hostSubnet := range hostSubnets {
			joinLRPIP, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(hostSubnet), joinLRPIPs)
			ocrStaticRouteNamedUUID := fmt.Sprintf("subnet-static-route-ovn-cluster-router-%v-UUID", i)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, ocrStaticRouteNamedUUID)
			testData = append(testData, &nbdb.LogicalRouterStaticRoute{
				UUID:     ocrStaticRouteNamedUUID,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  joinLRPIP.IP.String(),
			})
		}
	}
	if config.Gateway.Mode == config.GatewayModeLocal && nodeMgmtPortIP != "" {
		for i, hostSubnet := range hostSubnets {
			ocrStaticRouteNamedUUID := fmt.Sprintf("subnet-static-route-ovn-cluster-router-%v-UUID", i)
			expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, ocrStaticRouteNamedUUID)
			testData = append(testData, &nbdb.LogicalRouterStaticRoute{
				UUID:     ocrStaticRouteNamedUUID,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnet.String(),
				Nexthop:  nodeMgmtPortIP,
			})
		}
	}

	if hasIPv4 {
		staticServiceRouteNamedUUID := "static-service-route-v4-UUID"
		grStaticRoutes = append(grStaticRoutes, staticServiceRouteNamedUUID)

		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:        staticServiceRouteNamedUUID,
			IPPrefix:    config.Gateway.V4MasqueradeSubnet,
			Nexthop:     config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
			OutputPort:  &externalRouterPort,
			ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
		})
		testData = append(testData, &nbdb.StaticMACBinding{
			UUID:               "MAC-binding-UUID",
			IP:                 config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
			LogicalPort:        types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
			MAC:                util.IPAddrToHWAddr(config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP).String(),
			OverrideDynamicMAC: true,
		})
	}
	if hasIPv6 {
		staticServiceRouteNamedUUID := "static-service-route-v6-UUID"
		grStaticRoutes = append(grStaticRoutes, staticServiceRouteNamedUUID)

		testData = append(testData, &nbdb.LogicalRouterStaticRoute{
			UUID:        staticServiceRouteNamedUUID,
			IPPrefix:    config.Gateway.V6MasqueradeSubnet,
			Nexthop:     config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String(),
			OutputPort:  &externalRouterPort,
			ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
		})
		testData = append(testData, &nbdb.StaticMACBinding{
			UUID:               "MAC-binding-2-UUID",
			IP:                 config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String(),
			LogicalPort:        types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
			MAC:                util.IPAddrToHWAddr(config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP).String(),
			OverrideDynamicMAC: true,
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

	natUUIDs := make([]string, 0, len(clusterIPSubnets))
	if !skipSnat {
		for i, subnet := range clusterIPSubnets {
			natUUID := fmt.Sprintf("nat-%d-UUID", i)
			natUUIDs = append(natUUIDs, natUUID)
			physicalIP, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(subnet), l3GatewayConfig.IPAddresses)
			testData = append(testData, &nbdb.NAT{
				UUID:       natUUID,
				ExternalIP: physicalIP.IP.String(),
				LogicalIP:  subnet.String(),
				Options:    map[string]string{"stateless": "false"},
				Type:       nbdb.NATTypeSNAT,
			})
		}
	}

	for i, physicalIP := range l3GatewayConfig.IPAddresses {
		natUUID := fmt.Sprintf("nat-join-%d-UUID", i)
		natUUIDs = append(natUUIDs, natUUID)
		joinLRPIP, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(physicalIP), joinLRPIPs)
		testData = append(testData, &nbdb.NAT{
			UUID:       natUUID,
			ExternalIP: physicalIP.IP.String(),
			LogicalIP:  joinLRPIP.IP.String(),
			Options:    map[string]string{"stateless": "false"},
			Type:       nbdb.NATTypeSNAT,
		})
	}

	testData = append(testData, &nbdb.MeterBand{
		UUID:   "25-pktps-rate-limiter-UUID",
		Action: types.MeterAction,
		Rate:   int(25),
	})
	meters := map[string]string{
		OVNARPRateLimiter:              getMeterNameForProtocol(OVNARPRateLimiter),
		OVNARPResolveRateLimiter:       getMeterNameForProtocol(OVNARPResolveRateLimiter),
		OVNBFDRateLimiter:              getMeterNameForProtocol(OVNBFDRateLimiter),
		OVNControllerEventsRateLimiter: getMeterNameForProtocol(OVNControllerEventsRateLimiter),
		OVNICMPV4ErrorsRateLimiter:     getMeterNameForProtocol(OVNICMPV4ErrorsRateLimiter),
		OVNICMPV6ErrorsRateLimiter:     getMeterNameForProtocol(OVNICMPV6ErrorsRateLimiter),
		OVNRejectRateLimiter:           getMeterNameForProtocol(OVNRejectRateLimiter),
		OVNTCPRSTRateLimiter:           getMeterNameForProtocol(OVNTCPRSTRateLimiter),
		OVNServiceMonitorLimiter:       getMeterNameForProtocol(OVNServiceMonitorLimiter),
	}
	fairness := true
	for _, v := range meters {
		testData = append(testData, &nbdb.Meter{
			UUID:  v + "-UUID",
			Bands: []string{"25-pktps-rate-limiter-UUID"},
			Name:  v,
			Unit:  types.PacketsPerSecond,
			Fair:  &fairness,
		})
	}

	copp := &nbdb.Copp{
		UUID:   "copp-UUID",
		Name:   "ovnkube-default",
		Meters: meters,
	}
	testData = append(testData, copp)

	testData = append(testData, &nbdb.LogicalRouter{
		UUID: GRName + "-UUID",
		Name: GRName,
		Options: map[string]string{
			"lb_force_snat_ip":              "router_ip",
			"snat-ct-zone":                  "0",
			"always_learn_from_arp_request": "false",
			"dynamic_neigh_routers":         "true",
			"chassis":                       l3GatewayConfig.ChassisID,
			"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
		},
		ExternalIDs: map[string]string{
			"physical_ip":  physicalIPs[0],
			"physical_ips": strings.Join(physicalIPs, ","),
		},
		Ports:             []string{gwRouterPort + "-UUID", externalRouterPort + "-UUID"},
		StaticRoutes:      grStaticRoutes,
		Nat:               natUUIDs,
		LoadBalancerGroup: []string{types.ClusterLBGroupName + "-UUID", types.ClusterRouterLBGroupName + "-UUID"},
		Copp:              &copp.UUID,
	})

	testData = append(testData, expectedOVNClusterRouter)

	if len(nodeMgmtPortIP) != 0 {
		nodeACL := getAllowFromNodeExpectedACL(nodeName, nodeMgmtPortIP, nil, DefaultNetworkControllerName)
		testData = append(testData, nodeACL)

		expectedNodeSwitch.ACLs = append(expectedNodeSwitch.ACLs, nodeACL.UUID)
	}

	testData = append(testData, expectedNodeSwitch)

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
				"router-port":               externalRouterPort,
				"nat-addresses":             "router",
				"exclude-lb-vips-from-garp": "true",
			},
			Addresses: []string{l3GatewayConfig.MACAddress.String()},
		},
		&nbdb.LogicalSwitch{
			UUID:  types.OVNJoinSwitch + "-UUID",
			Name:  types.OVNJoinSwitch,
			Ports: []string{gwSwitchPort + "-UUID"},
		},
		&nbdb.LogicalSwitch{
			UUID:  externalSwitch + "-UUID",
			Name:  externalSwitch,
			Ports: []string{l3GatewayConfig.InterfaceID + "-UUID", externalSwitchPortToRouter + "-UUID"},
		},
		&nbdb.LoadBalancerGroup{
			Name: types.ClusterLBGroupName,
			UUID: types.ClusterLBGroupName + "-UUID",
		},
		&nbdb.LoadBalancerGroup{
			Name: types.ClusterSwitchLBGroupName,
			UUID: types.ClusterSwitchLBGroupName + "-UUID",
		},
		&nbdb.LoadBalancerGroup{
			Name: types.ClusterRouterLBGroupName,
			UUID: types.ClusterRouterLBGroupName + "-UUID",
		})
	return testData
}

var _ = ginkgo.Describe("Gateway Init Operations", func() {
	var (
		fakeOvn *FakeOVN
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		fakeOvn = NewFakeOVN(true)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("Gateway Creation Operations Shared Gateway Mode", func() {
		ginkgo.BeforeEach(func() {
			config.Gateway.Mode = config.GatewayModeShared
		})

		ginkgo.It("creates an IPv4 gateway in OVN", func() {
			routeUUID := "route-UUID"
			leftoverMgmtIPRoute := &nbdb.LogicalRouterStaticRoute{
				Nexthop:  "10.130.0.2",
				IPPrefix: "10.130.0.0/23",
				UUID:     routeUUID,
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{routeUUID},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					// tests migration from local to shared
					leftoverMgmtIPRoute,
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{} // the leftover LGW route should have got deleted
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("removes stale MAC and route for old masquerade subnet using auto-detect", func() {
			routeUUID := "route-UUID"
			outputPort := types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName
			leftoverMasqueradeIPRoute := &nbdb.LogicalRouterStaticRoute{
				Nexthop:     "170.254.169.4",
				IPPrefix:    "170.254.169.0/16",
				OutputPort:  &outputPort,
				UUID:        routeUUID,
				ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
			}
			leftoverStaticMAC := &nbdb.StaticMACBinding{
				IP:          "170.254.169.4",
				LogicalPort: types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
			}
			GRName := "GR_" + nodeName
			expectedOVNGatewayRouter := &nbdb.LogicalRouter{
				UUID: GRName + "-UUID",
				Name: GRName,
				Options: map[string]string{
					"lb_force_snat_ip":              "router_ip",
					"snat-ct-zone":                  "0",
					"always_learn_from_arp_request": "false",
					"dynamic_neigh_routers":         "true",
					"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
				},
				StaticRoutes: []string{routeUUID},
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					// tests migration from local to shared
					leftoverMasqueradeIPRoute,
					leftoverStaticMAC,
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedOVNGatewayRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{} // the leftover LGW route should have got deleted
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("removes stale MAC and route for old masquerade subnet using stale annotation", func() {
			routeUUID := "route-UUID"
			outputPort := types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName
			leftoverMasqueradeIPRoute := &nbdb.LogicalRouterStaticRoute{
				Nexthop:     "170.254.169.4",
				IPPrefix:    "170.254.169.0/16",
				OutputPort:  &outputPort,
				UUID:        routeUUID,
				ExternalIDs: map[string]string{util.OvnNodeMasqCIDR: ""},
			}
			leftoverStaticMAC := &nbdb.StaticMACBinding{
				IP:          "170.254.169.4",
				LogicalPort: types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
			}
			GRName := "GR_" + nodeName
			expectedOVNGatewayRouter := &nbdb.LogicalRouter{
				UUID: GRName + "-UUID",
				Name: GRName,
				Options: map[string]string{
					"lb_force_snat_ip":              "router_ip",
					"snat-ct-zone":                  "0",
					"always_learn_from_arp_request": "false",
					"dynamic_neigh_routers":         "true",
					"mac_binding_age_threshold":     types.GRMACBindingAgeThreshold,
				},
				StaticRoutes: []string{routeUUID},
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			a := newObjectMeta(nodeName, "")
			a.Annotations = map[string]string{
				util.OvnNodeMasqCIDR: "{\"ipv4\":\"170.254.169.0/16\",\"ipv6\":\"fd69::/112\"}",
			}
			node1 := v1.Node{ObjectMeta: a}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					// tests migration from local to shared
					leftoverMasqueradeIPRoute,
					leftoverStaticMAC,
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedOVNGatewayRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			},
				&v1.NodeList{Items: []v1.Node{node1}},
			)

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{} // the leftover LGW route should have got deleted
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("creates an IPv4 gateway in OVN without next hops", func() {
			routeUUID := "route-UUID"
			leftoverMgmtIPRoute := &nbdb.LogicalRouterStaticRoute{
				Nexthop:  "10.130.0.2",
				IPPrefix: "10.130.0.0/23",
				UUID:     routeUUID,
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{routeUUID},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					// tests migration from local to shared
					leftoverMgmtIPRoute,
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{} // the leftover LGW route should have got deleted
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("updates options:gateway_mtu for GR LRP", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{}
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""

			// Disable option:gateway_mtu.
			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				false,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

			// Enable option:gateway_mtu.
			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState = generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("updates SNAT when join IP changes", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{}
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				false,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

			ginkgo.By("modifying the node join IP")
			oldJoinLRPIPs := joinLRPIPs
			joinLRPIPs = ovntest.MustParseIPNets("100.64.0.99/16")
			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState = generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			// FIXME(trozet): we do not clean up stale routes after join IP changes
			for i, joinLRPIP := range oldJoinLRPIPs {
				joinStaticRouteNamedUUID := fmt.Sprintf("hack-join-static-route-ovn-cluster-router-%v-UUID", i)
				expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, joinStaticRouteNamedUUID)
				expectedDatabaseState = append(expectedDatabaseState, &nbdb.LogicalRouterStaticRoute{
					UUID:     joinStaticRouteNamedUUID,
					IPPrefix: joinLRPIP.IP.String(),
					Nexthop:  joinLRPIP.IP.String(),
				})
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("creates an IPv6 gateway in OVN", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			config.IPv4Mode = false
			config.IPv6Mode = true
			clusterIPSubnets := ovntest.MustParseIPNets("fd01::/48")
			hostSubnets := ovntest.MustParseIPNets("fd01:0:0:2::/64")
			joinLRPIPs := ovntest.MustParseIPNets("fd98::3/64")
			defLRPIPs := ovntest.MustParseIPNets("fd98::1/64")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("fd99::2/64"),
				NextHops:       ovntest.MustParseIPs("fd99::1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("creates an IPv6 gateway in OVN without next hops", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("fd01::/48")
			hostSubnets := ovntest.MustParseIPNets("fd01:0:0:2::/64")
			joinLRPIPs := ovntest.MustParseIPNets("fd98::3/64")
			defLRPIPs := ovntest.MustParseIPNets("fd98::1/64")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("fd99::2/64"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			config.IPv4Mode = false
			config.IPv6Mode = true

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

		})

		ginkgo.It("creates a dual-stack gateway in OVN", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			config.IPv4Mode = true
			config.IPv6Mode = true
			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14", "fd01::/48")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23", "fd01:0:0:2::/64")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16", "fd98::3/64")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16", "fd98::1/64")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24", "fd99::2/64"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1", "fd99::1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("removes leftover SNAT entries during init", func() {
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := true
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("ensures only a single static route per node for ovn_cluster_router", func() {
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			badRouteName := "wrongRoute-UUID"
			badRoute := &nbdb.LogicalRouterStaticRoute{
				UUID:     badRouteName,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnets[0].String(),
				Nexthop:  "100.64.0.5",
			}
			badRouteName2 := "wrongRoute-UUID-2"
			badRoute2 := &nbdb.LogicalRouterStaticRoute{
				UUID:     badRouteName2,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnets[0].String(),
				Nexthop:  "100.64.0.6",
			}
			badRouteName3 := "wrongRoute-UUID-3"
			badRoute3 := &nbdb.LogicalRouterStaticRoute{
				UUID:     badRouteName3,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnets[0].String(),
				Nexthop:  "100.64.0.7",
			}
			// ignoreRoute has no policy, aka dst-ip. It should be ignored for removal
			ignoreRouteName4 := "ignoreRoute-UUID"
			ignoreRoute4 := &nbdb.LogicalRouterStaticRoute{
				UUID:     ignoreRouteName4,
				IPPrefix: hostSubnets[0].String(),
				Nexthop:  "100.64.0.99",
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{badRouteName, badRouteName2, badRouteName3, ignoreRouteName4},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					badRoute,
					badRoute2,
					badRoute3,
					ignoreRoute4,
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})
			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := true

			// remove bad route from expected data
			expectedOVNClusterRouter.StaticRoutes = []string{ignoreRouteName4}
			mgmtPortIP := ""
			ginkgo.By("Gateway init should have removed bad route")
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			expectedDatabaseState = append(expectedDatabaseState, ignoreRoute4)
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})
	})

	ginkgo.Context("Gateway Create Operations Local Gateway Mode", func() {

		ginkgo.BeforeEach(func() {
			config.Gateway.Mode = config.GatewayModeLocal
			config.IPv6Mode = false
		})

		ginkgo.It("creates a dual-stack gateway in OVN", func() {
			config.IPv6Mode = true
			// covers both IPv4, IPv6 single stack cases since path is the same.
			routeUUID1 := "route1-UUID"
			leftoverJoinRoute1 := &nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				Nexthop:  "100.64.0.3",
				IPPrefix: "10.130.0.0/23",
				UUID:     routeUUID1,
			}
			routeUUID2 := "route2-UUID"
			leftoverJoinRoute2 := &nbdb.LogicalRouterStaticRoute{
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				Nexthop:  "fd98::3",
				IPPrefix: "fd01:0:0:2::/64",
				UUID:     routeUUID2,
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{routeUUID1, routeUUID2},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				Name: types.ClusterLBGroupName,
				UUID: types.ClusterLBGroupName + "-UUID",
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					// tests migration from shared to local
					leftoverJoinRoute1,
					leftoverJoinRoute2,
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})

			config.IPv4Mode = true
			config.IPv6Mode = true
			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14", "fd01::/48")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23", "fd01:0:0:2::/64")
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16", "fd98::3/64")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16", "fd98::1/64")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24", "fd99::2/64"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1", "fd99::1"),
				NodePortEnable: true,
			}
			sctpSupport := false

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{} // the leftover SGW route should have got deleted
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("ensures a leftover route on ovn_cluster_router to join subnet is removed", func() {
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")
			badRouteName := "wrongRoute-UUID"
			badRoute := &nbdb.LogicalRouterStaticRoute{
				UUID:     badRouteName,
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				IPPrefix: hostSubnets[0].String(),
				Nexthop:  "100.64.0.5",
			}
			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID:         types.OVNClusterRouter + "-UUID",
				Name:         types.OVNClusterRouter,
				StaticRoutes: []string{badRouteName},
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					badRoute,
					expectedOVNClusterRouter,
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})
			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			nodeName := "test-node"
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := true

			// remove bad route from expected data
			expectedOVNClusterRouter.StaticRoutes = []string{}
			mgmtPortIP := ""
			ginkgo.By("Gateway init should have removed bad route")
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("ensures a leftover mtu on rtoe port is cleared", func() {
			nodeName := "test-node"
			joinLRPIPs := ovntest.MustParseIPNets("100.64.0.3/16")
			hostSubnets := ovntest.MustParseIPNets("10.130.0.0/23")

			expectedOVNClusterRouter := &nbdb.LogicalRouter{
				UUID: types.OVNClusterRouter + "-UUID",
				Name: types.OVNClusterRouter,
			}
			expectedNodeSwitch := &nbdb.LogicalSwitch{
				UUID: nodeName + "-UUID",
				Name: nodeName,
			}

			expectedGR := &nbdb.LogicalRouter{
				Name:         types.GWRouterPrefix + nodeName,
				UUID:         types.GWRouterPrefix + nodeName + "-UUID",
				LoadBalancer: []string{},
				Ports: []string{
					types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
				},
			}
			expectedClusterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterLBGroupName + "-UUID",
				Name: types.ClusterLBGroupName,
			}
			expectedSwitchLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterSwitchLBGroupName + "-UUID",
				Name: types.ClusterSwitchLBGroupName,
			}
			expectedRouterLBGroup := &nbdb.LoadBalancerGroup{
				UUID: types.ClusterRouterLBGroupName + "-UUID",
				Name: types.ClusterRouterLBGroupName,
			}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
						Networks: []string{"100.64.0.1/16"},
					},
					&nbdb.StaticMACBinding{
						IP:          config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
						LogicalPort: types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
					},
					&nbdb.LogicalRouterPort{
						UUID:    types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
						Name:    types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
						Options: map[string]string{"gateway_mtu": "1400"},
					},
					expectedGR,
					expectedOVNClusterRouter,
					&nbdb.LogicalSwitchPort{
						Name: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
						UUID: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					},
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					&nbdb.LogicalSwitch{
						Name: types.ExternalSwitchPrefix + nodeName,
						UUID: types.ExternalSwitchPrefix + nodeName + "-UUID ",
					},
					expectedNodeSwitch,
					expectedClusterLBGroup,
					expectedSwitchLBGroup,
					expectedRouterLBGroup,
				},
			})
			clusterIPSubnets := ovntest.MustParseIPNets("10.128.0.0/14")
			defLRPIPs := ovntest.MustParseIPNets("100.64.0.1/16")
			l3GatewayConfig := &util.L3GatewayConfig{
				Mode:           config.GatewayModeLocal,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			sctpSupport := false
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).GatewayInit(
				nodeName,
				clusterIPSubnets,
				hostSubnets,
				l3GatewayConfig,
				sctpSupport,
				joinLRPIPs,
				defLRPIPs,
				extractExternalIPs(l3GatewayConfig),
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := true

			mgmtPortIP := ""

			//expectedGR.Ports = []string{}
			ginkgo.By("Gateway init should have cleared stale MTU on rtoe port")
			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"1400")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

	})

	ginkgo.Context("Gateway Cleanup Operations", func() {

		ginkgo.It("cleans up a single-stack gateway in OVN", func() {
			nodeName := "test-node"

			nodeSubnetPriority, _ := strconv.Atoi(types.NodeSubnetPolicyPriority)

			matchstr2 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == nodePhysicalIP /* %s */`, nodeName, nodeName)
			matchstr3 := "ip4.src == source && ip4.dst == nodePhysicalIP"
			matchstr6 := fmt.Sprintf("ip4.src == NO DELETE && ip4.dst == nodePhysicalIP /* %s-no */", nodeName)

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
						Networks: []string{"100.64.0.1/16"},
					},
					&nbdb.LoadBalancer{
						UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						Protocol: &nbdb.LoadBalancerProtocolTCP,
						ExternalIDs: map[string]string{
							types.LoadBalancerKindExternalID:  "Service",
							types.LoadBalancerOwnerExternalID: "default/kubernetes",
						},
						Vips: map[string]string{
							"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
							"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
						},
					},
					&nbdb.StaticMACBinding{
						IP:          config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String(),
						LogicalPort: types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + nodeName,
						UUID:  types.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
						LoadBalancer: []string{
							"Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						},
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match2-UUID",
						Match:    matchstr2,
						Priority: nodeSubnetPriority,
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match3-UUID",
						Match:    matchstr3,
						Priority: nodeSubnetPriority,
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match6-UUID",
						Match:    matchstr6,
						Priority: nodeSubnetPriority,
					},
					&nbdb.LogicalRouterStaticRoute{
						Nexthop: "100.64.0.1",
						UUID:    "static-route-UUID",
					},
					&nbdb.LogicalRouter{
						Name:         types.OVNClusterRouter,
						UUID:         types.OVNClusterRouter + "-UUID",
						Policies:     []string{"match2-UUID", "match3-UUID", "match6-UUID"},
						StaticRoutes: []string{"static-route-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						Name: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
						UUID: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					},
					&nbdb.LogicalSwitch{
						UUID:  types.OVNJoinSwitch + "-UUID",
						Name:  types.OVNJoinSwitch,
						Ports: []string{types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						Name: types.ExternalSwitchPrefix + nodeName,
						UUID: types.ExternalSwitchPrefix + nodeName + "-UUID ",
					},
					&nbdb.LogicalSwitch{
						Name: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeName,
						UUID: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeName + "-UUID",
					},
				},
			})

			err := newGatewayManager(fakeOvn, nodeName).Cleanup()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: map[string]string{
						types.LoadBalancerKindExternalID:  "Service",
						types.LoadBalancerOwnerExternalID: "default/kubernetes",
					},
					Vips: map[string]string{
						"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
						"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
					},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "match3-UUID",
					Match:    matchstr3,
					Priority: nodeSubnetPriority,
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "match6-UUID",
					Match:    matchstr6,
					Priority: nodeSubnetPriority,
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					UUID:     types.OVNClusterRouter + "-UUID",
					Policies: []string{"match3-UUID", "match6-UUID"},
				},
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})

		ginkgo.It("cleans up a dual-stack gateway in OVN", func() {
			nodeName := "test-node"

			nodeSubnetPriority, _ := strconv.Atoi(types.NodeSubnetPolicyPriority)

			matchstr2 := fmt.Sprintf(`inport == "rtos-%s" && ip4.dst == nodePhysicalIP /* %s */`, nodeName, nodeName)
			matchstr3 := "ip4.src == source && ip4.dst == nodePhysicalIP"
			matchstr6 := fmt.Sprintf("ip4.src == NO DELETE && ip4.dst == nodePhysicalIP /* %s-no */", nodeName)

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
						UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
						Networks: []string{"100.64.0.1/16", "fd98::1/64"},
					},
					&nbdb.LoadBalancer{
						UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						Protocol: &nbdb.LoadBalancerProtocolTCP,
						ExternalIDs: map[string]string{
							types.LoadBalancerKindExternalID:  "Service",
							types.LoadBalancerOwnerExternalID: "default/kubernetes",
						},
						Vips: map[string]string{
							"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
							"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
						},
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + nodeName,
						UUID:  types.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
						LoadBalancer: []string{
							"Service_default/kubernetes_TCP_node_router_ovn-control-plane",
						},
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match2-UUID",
						Match:    matchstr2,
						Priority: nodeSubnetPriority,
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match3-UUID",
						Match:    matchstr3,
						Priority: nodeSubnetPriority,
					},
					&nbdb.LogicalRouterPolicy{
						UUID:     "match6-UUID",
						Match:    matchstr6,
						Priority: nodeSubnetPriority,
					},
					// add a stale egressIP reroute policy with nexthop == node's joinIP
					&nbdb.LogicalRouterPolicy{
						Priority: types.EgressIPReroutePriority,
						Match:    "ip4.src == 10.224.0.5",
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.1"},
						ExternalIDs: map[string]string{
							"name": "egresip",
						},
						UUID: "reroute-UUID",
					},
					&nbdb.LogicalRouterStaticRoute{
						Nexthop: "100.64.0.1",
						UUID:    "static-route-1-UUID",
					},
					&nbdb.LogicalRouterStaticRoute{
						Nexthop: "fd98::1",
						UUID:    "static-route-2-UUID",
					},
					&nbdb.LogicalRouter{
						Name:         types.OVNClusterRouter,
						UUID:         types.OVNClusterRouter + "-UUID",
						Policies:     []string{"match2-UUID", "match3-UUID", "match6-UUID", "reroute-UUID"},
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
					&nbdb.LogicalSwitchPort{
						Name: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
						UUID: types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID",
					},
					&nbdb.LogicalSwitch{
						UUID:  types.OVNJoinSwitch + "-UUID",
						Name:  types.OVNJoinSwitch,
						Ports: []string{types.JoinSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
					},
					&nbdb.LogicalSwitch{
						Name: types.ExternalSwitchPrefix + nodeName,
						UUID: types.ExternalSwitchPrefix + nodeName + "-UUID ",
					},
					&nbdb.LogicalSwitch{
						Name: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeName,
						UUID: types.EgressGWSwitchPrefix + types.ExternalSwitchPrefix + nodeName + "-UUID",
					},
				},
			})
			err := newGatewayManager(fakeOvn, nodeName).Cleanup()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					ExternalIDs: map[string]string{
						types.LoadBalancerKindExternalID:  "Service",
						types.LoadBalancerOwnerExternalID: "default/kubernetes",
					},
					Vips: map[string]string{
						"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
						"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
					},
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "match3-UUID",
					Match:    matchstr3,
					Priority: nodeSubnetPriority,
				},
				&nbdb.LogicalRouterPolicy{
					UUID:     "match6-UUID",
					Match:    matchstr6,
					Priority: nodeSubnetPriority,
				},
				&nbdb.LogicalRouter{
					Name:     types.OVNClusterRouter,
					UUID:     types.OVNClusterRouter + "-UUID",
					Policies: []string{"match3-UUID", "match6-UUID"},
				},
				&nbdb.LogicalSwitch{
					UUID: types.OVNJoinSwitch + "-UUID",
					Name: types.OVNJoinSwitch,
				},
			}
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})
	})
})

func newGatewayManager(ovn *FakeOVN, nodeName string) *GatewayManager {
	controller := ovn.controller
	return NewGatewayManager(
		nodeName,
		controller.defaultCOPPUUID,
		controller.kube,
		controller.nbClient,
		controller.GetNetInfo(),
		ovn.watcher,
		WithLoadBalancerGroups(
			controller.routerLoadBalancerGroupUUID,
			controller.clusterLoadBalancerGroupUUID,
			controller.switchLoadBalancerGroupUUID,
		),
	)
}

func extractExternalIPs(l3GatewayConfig *util.L3GatewayConfig) []net.IP {
	externalIPs := make([]net.IP, len(l3GatewayConfig.IPAddresses))
	for i, ip := range l3GatewayConfig.IPAddresses {
		externalIPs[i] = ip.IP
	}
	return externalIPs
}
