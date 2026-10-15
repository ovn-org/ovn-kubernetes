// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	nodeName = "test-node"
)

func init() {
	// libovsdb matcher might produce a lengthy output that will be cropped by
	// default gomega output limit, set to 0 to unlimit.
	format.MaxLength = 0
}

func generateAdvertisedUDNIsolationExpectedNB(testData []libovsdbtest.TestData, networkName string, networkID int, clusterIPSubnets []*net.IPNet, nodeSwitch *nbdb.LogicalSwitch, addrSet addressset.AddressSet) []libovsdbtest.TestData {
	var passMatches []string
	for _, subnet := range clusterIPSubnets {
		ipPrefix := "ip4"
		if utilnet.IsIPv6CIDR(subnet) {
			ipPrefix = "ip6"
		}
		passMatches = append(passMatches, fmt.Sprintf("((%s.src == %s) && (%s.dst == %s))", ipPrefix, subnet, ipPrefix, subnet))

	}
	passACL := libovsdbutil.BuildACLWithDefaultTier(
		GetAdvertisedNetworkSubnetsPassACLdbIDs(types.DefaultNetworkControllerName, networkName, networkID),
		types.AdvertisedNetworkPassPriority,
		strings.Join(passMatches, " || "),
		nbdb.ACLActionPass,
		nil,
		libovsdbutil.LportEgressAfterLB)
	passACL.Tier = types.PrimaryACLTier
	passACL.UUID = "advertised-udn-isolation-pass-acl-UUID"
	dropACL := BuildAdvertisedNetworkSubnetsDropACL(addrSet)
	dropACL.UUID = "advertised-udn-isolation-drop-acl-UUID"
	nodeSwitch.ACLs = append(nodeSwitch.ACLs, passACL.UUID)
	pg := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, []*nbdb.ACL{dropACL})
	pg.UUID = "advertised-udn-isolation-drop-pg-UUID"
	pg.Ports = []string{types.SwitchToRouterPrefix + nodeSwitch.Name + "-UUID"}
	testData = append(testData, passACL, dropACL, pg)

	var cidrs []string
	for _, subnet := range clusterIPSubnets {
		cidrs = append(cidrs, subnet.String())
	}
	v4set, v6set := addressset.GetTestDbAddrSets(GetAdvertisedNetworkSubnetsAddressSetDBIDs(), cidrs)
	if config.IPv4Mode {
		testData = append(testData, v4set)
	}
	if config.IPv6Mode {
		testData = append(testData, v6set)
	}

	return testData
}

func generateGatewayInitExpectedNB(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, nodeName string, clusterIPSubnets []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, joinLRPIPs, defLRPIPs []*net.IPNet, skipSnat bool, nodeMgmtPortIP,
	gatewayMTU string, nodeIPsForClusterNodeIPAS ...string) []libovsdbtest.TestData {
	return generateGatewayInitExpectedNBWithPodNetworkAdvertised(testData, expectedOVNClusterRouter, expectedNodeSwitch,
		nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, nodeMgmtPortIP,
		gatewayMTU, false, nodeIPsForClusterNodeIPAS...) // Default to no pod network advertised
}

func generateGatewayInitExpectedNBWithPodNetworkAdvertised(testData []libovsdbtest.TestData, expectedOVNClusterRouter *nbdb.LogicalRouter,
	expectedNodeSwitch *nbdb.LogicalSwitch, nodeName string, clusterIPSubnets []*net.IPNet, hostSubnets []*net.IPNet,
	l3GatewayConfig *util.L3GatewayConfig, joinLRPIPs, defLRPIPs []*net.IPNet, skipSnat bool, nodeMgmtPortIP,
	gatewayMTU string, isPodNetworkAdvertised bool, nodeIPsForClusterNodeIPAS ...string) []libovsdbtest.TestData {

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
			libovsdbops.GatewayMTU: gatewayMTU,
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
	var egressNodeIPsASv4, egressNodeIPsASv6 *nbdb.AddressSet
	if config.OVNKubernetesFeature.EnableEgressIP {
		nodeIPs := physicalIPs
		if len(nodeIPsForClusterNodeIPAS) > 0 {
			nodeIPs = nodeIPsForClusterNodeIPAS
		}
		egressNodeIPsASv4, egressNodeIPsASv6 = buildEgressIPNodeAddressSets(nodeIPs)
		if config.IPv4Mode {
			testData = append(testData, egressNodeIPsASv4)
		}
		if config.IPv6Mode {
			testData = append(testData, egressNodeIPsASv6)
		}
	}

	natUUIDs := make([]string, 0, len(clusterIPSubnets))
	if !skipSnat {
		for i, subnet := range clusterIPSubnets {
			natUUID := fmt.Sprintf("nat-%d-UUID", i)
			natUUIDs = append(natUUIDs, natUUID)
			physicalIP, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(subnet), l3GatewayConfig.IPAddresses)
			nat := nbdb.NAT{
				UUID:       natUUID,
				ExternalIP: physicalIP.IP.String(),
				LogicalIP:  subnet.String(),
				Options:    map[string]string{"stateless": "false"},
				Type:       nbdb.NATTypeSNAT,
			}
			if config.Gateway.Mode != config.GatewayModeDisabled {
				nat.ExternalPortRange = config.DefaultEphemeralPortRange
			}
			if isPodNetworkAdvertised {
				// IPv6 pod network
				if utilnet.IsIPv6CIDR(subnet) {
					if egressNodeIPsASv6 != nil {
						nat.Match = fmt.Sprintf("ip6.dst == $%s", egressNodeIPsASv6.Name)
					}
				} else {
					// IPv4 pod network
					if egressNodeIPsASv4 != nil {
						nat.Match = fmt.Sprintf("ip4.dst == $%s", egressNodeIPsASv4.Name)
					}
				}
			}
			testData = append(testData, &nat)
		}
	}

	for i, physicalIP := range l3GatewayConfig.IPAddresses {
		natUUID := fmt.Sprintf("nat-join-%d-UUID", i)
		natUUIDs = append(natUUIDs, natUUID)
		joinLRPIP, _ := util.MatchFirstIPNetFamily(utilnet.IsIPv6CIDR(physicalIP), joinLRPIPs)
		nat := nbdb.NAT{
			UUID:       natUUID,
			ExternalIP: physicalIP.IP.String(),
			LogicalIP:  joinLRPIP.IP.String(),
			Options:    map[string]string{"stateless": "false"},
			Type:       nbdb.NATTypeSNAT,
		}
		if config.Gateway.Mode != config.GatewayModeDisabled {
			nat.ExternalPortRange = config.DefaultEphemeralPortRange
		}
		testData = append(testData, &nat)
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

	dynamicNeighRouters := "false"

	testData = append(testData, &nbdb.LogicalRouter{
		UUID: GRName + "-UUID",
		Name: GRName,
		Options: map[string]string{
			"lb_force_snat_ip":              "router_ip",
			"snat-ct-zone":                  "0",
			"always_learn_from_arp_request": "false",
			"dynamic_neigh_routers":         dynamicNeighRouters,
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
		nodeACL := getAllowFromNodeExpectedACL(nodeName, nodeMgmtPortIP, nil, types.DefaultNetworkControllerName)
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
				libovsdbops.RouterPort: gwRouterPort,
			},
		},
		&nbdb.LogicalSwitchPort{
			UUID: externalSwitchPortToRouter + "-UUID",
			Name: externalSwitchPortToRouter,
			Type: "router",
			Options: map[string]string{
				libovsdbops.RouterPort:      externalRouterPort,
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
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		fakeOvn = NewFakeOVN(true, nodeName)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("Gateway Creation Operations Shared Gateway Mode", func() {
		ginkgo.BeforeEach(func() {
			config.Gateway.Mode = config.GatewayModeShared
			config.Gateway.EphemeralPortRange = config.DefaultEphemeralPortRange
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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

		ginkgo.It("replaces a stale localnet port on the external switch when the interface ID changes", func() {
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
			// The external switch already carries the localnet port of the
			// previous bridge; gatewayInit with a new interface ID must
			// replace it rather than add a second localnet port.
			staleLocalnetPort := &nbdb.LogicalSwitchPort{
				UUID:      "OLD-INTERFACE-ID-UUID",
				Name:      "OLD-INTERFACE-ID",
				Type:      "localnet",
				Addresses: []string{"unknown"},
				Options: map[string]string{
					"network_name": types.PhysicalNetworkName,
				},
			}
			// The switch-to-router port already exists with stale content, so
			// the transaction carries a delete, an update and an insert.
			staleSwitchToRouterPort := &nbdb.LogicalSwitchPort{
				UUID:      types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName + "-UUID",
				Name:      types.EXTSwitchToGWRouterPrefix + types.GWRouterPrefix + nodeName,
				Type:      "router",
				Addresses: []string{"00:11:22:33:44:55"},
				Options: map[string]string{
					libovsdbops.RouterPort: types.GWRouterToExtSwitchPrefix + types.GWRouterPrefix + nodeName,
				},
			}
			externalSwitch := &nbdb.LogicalSwitch{
				UUID:  types.ExternalSwitchPrefix + nodeName + "-UUID",
				Name:  types.ExternalSwitchPrefix + nodeName,
				Ports: []string{staleLocalnetPort.UUID, staleSwitchToRouterPort.UUID},
			}
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: types.OVNJoinSwitch + "-UUID",
						Name: types.OVNJoinSwitch,
					},
					staleLocalnetPort,
					staleSwitchToRouterPort,
					externalSwitch,
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
				Mode:           config.GatewayModeShared,
				ChassisID:      "SYSTEM-ID",
				BridgeID:       "BRIDGE-ID",
				InterfaceID:    "INTERFACE-ID",
				MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses:    ovntest.MustParseIPNets("169.255.33.2/24"),
				NextHops:       ovntest.MustParseIPs("169.255.33.1"),
				NodePortEnable: true,
			}
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
				true,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
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

			dynamicNeighRouters := "false"

			expectedOVNGatewayRouter := &nbdb.LogicalRouter{
				UUID: GRName + "-UUID",
				Name: GRName,
				Options: map[string]string{
					"lb_force_snat_ip":              "router_ip",
					"snat-ct-zone":                  "0",
					"always_learn_from_arp_request": "false",
					"dynamic_neigh_routers":         dynamicNeighRouters,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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

			dynamicNeighRouters := "false"

			expectedOVNGatewayRouter := &nbdb.LogicalRouter{
				UUID: GRName + "-UUID",
				Name: GRName,
				Options: map[string]string{
					"lb_force_snat_ip":              "router_ip",
					"snat-ct-zone":                  "0",
					"always_learn_from_arp_request": "false",
					"dynamic_neigh_routers":         dynamicNeighRouters,
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
			node1 := corev1.Node{ObjectMeta: a}
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
				&corev1.NodeList{Items: []corev1.Node{node1}},
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}
			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{}
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""

			// Disable option:gateway_mtu.
			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
				false,
			)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedDatabaseState := generateGatewayInitExpectedNB(testData, expectedOVNClusterRouter, expectedNodeSwitch,
				nodeName, clusterIPSubnets, hostSubnets, l3GatewayConfig, joinLRPIPs, defLRPIPs, skipSnat, mgmtPortIP,
				"")
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

			// Enable option:gateway_mtu.
			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testData := []libovsdbtest.TestData{}
			skipSnat := false
			expectedOVNClusterRouter.StaticRoutes = []string{}
			// We don't set up the Allow from mgmt port ACL here
			mgmtPortIP := ""

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig.gwRouterJoinCIDRs = joinLRPIPs
			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			config.IPv4Mode = false
			config.IPv6Mode = true

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			config.Gateway.EphemeralPortRange = config.DefaultEphemeralPortRange
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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
						Options: map[string]string{libovsdbops.GatewayMTU: "1400"},
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
			gwConfig := &GatewayConfig{
				annoConfig:                 l3GatewayConfig,
				hostSubnets:                hostSubnets,
				clusterSubnets:             clusterIPSubnets,
				gwRouterJoinCIDRs:          joinLRPIPs,
				hostAddrs:                  nil,
				externalIPs:                extractExternalIPs(l3GatewayConfig),
				ovnClusterLRPToJoinIfAddrs: defLRPIPs,
			}
			config.Gateway.DisableSNATMultipleGWs = true

			var err error
			fakeOvn.controller.defaultCOPPUUID, err = EnsureDefaultCOPP(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedOVNClusterRouter.StaticRoutes = []string{}
			err = newGatewayManager(fakeOvn, nodeName).gatewayInit(
				nodeName,
				gwConfig,
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

var _ = ginkgo.Describe("Gateway Router static routes to the distributed router", func() {
	const (
		drNextHop      = "100.65.0.1"
		clusterSubnet  = "10.193.0.0/16"
		nodeHostSubnet = "10.193.0.0/24"
	)

	var (
		fakeOvn      *FakeOVN
		gwRouterName = types.GWRouterPrefix + nodeName
	)

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.Gateway.Mode = config.GatewayModeShared
		config.Gateway.EphemeralPortRange = config.DefaultEphemeralPortRange
		fakeOvn = NewFakeOVN(true, nodeName)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	// drRoutePrefixes returns the IP prefixes of the gateway router's static
	// routes whose nexthop is the distributed router (ovn_cluster_router) join
	// switch port.
	drRoutePrefixes := func() []string {
		routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(
			fakeOvn.nbClient,
			&nbdb.LogicalRouter{Name: gwRouterName},
			func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.Nexthop == drNextHop
			})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		prefixes := make([]string, 0, len(routes))
		for _, route := range routes {
			prefixes = append(prefixes, route.IPPrefix)
		}
		return prefixes
	}

	// runUpdate seeds a gateway router (plus any pre-existing static routes) and
	// invokes updateGWRouterStaticRoutes for the default network.
	runUpdate := func(gwConfig *GatewayConfig, seedRoutes ...*nbdb.LogicalRouterStaticRoute) {
		gwRouter := &nbdb.LogicalRouter{
			UUID: gwRouterName + "-UUID",
			Name: gwRouterName,
		}
		nbData := []libovsdbtest.TestData{gwRouter}
		for _, route := range seedRoutes {
			nbData = append(nbData, route)
			gwRouter.StaticRoutes = append(gwRouter.StaticRoutes, route.UUID)
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: nbData}, &corev1.NodeList{
			Items: []corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: nodeName}},
			},
		})

		gw := newGatewayManager(fakeOvn, nodeName)
		externalRouterPort := types.GWRouterToExtSwitchPrefix + gwRouterName
		gomega.Expect(gw.updateGWRouterStaticRoutes(gwConfig, externalRouterPort, gwRouter)).To(gomega.Succeed())
	}

	newGWConfig := func() *GatewayConfig {
		return &GatewayConfig{
			annoConfig:                 &util.L3GatewayConfig{},
			hostSubnets:                ovntest.MustParseIPNets(nodeHostSubnet),
			clusterSubnets:             ovntest.MustParseIPNets(clusterSubnet),
			ovnClusterLRPToJoinIfAddrs: ovntest.MustParseIPNets(drNextHop + "/16"),
		}
	}

	ginkgo.It("routes the whole cluster subnet via the distributed router in overlay mode", func() {
		// default (empty) transport == overlay
		runUpdate(newGWConfig())
		gomega.Expect(drRoutePrefixes()).To(gomega.ConsistOf(clusterSubnet))
	})

	ginkgo.It("routes only the node host subnet via the distributed router in no-overlay mode", func() {
		config.Default.Transport = types.NetworkTransportNoOverlay
		runUpdate(newGWConfig())
		gomega.Expect(drRoutePrefixes()).To(gomega.ConsistOf(nodeHostSubnet))
	})

	ginkgo.It("removes a stale cluster subnet route via the distributed router in no-overlay mode", func() {
		config.Default.Transport = types.NetworkTransportNoOverlay
		staleRoute := &nbdb.LogicalRouterStaticRoute{
			UUID:     "stale-cluster-subnet-route-UUID",
			IPPrefix: clusterSubnet,
			Nexthop:  drNextHop,
		}
		runUpdate(newGWConfig(), staleRoute)
		// the stale /16 must be gone, leaving only the node's /24
		gomega.Expect(drRoutePrefixes()).To(gomega.ConsistOf(nodeHostSubnet))
	})

	ginkgo.It("removes a stale node host subnet route via the distributed router in overlay mode", func() {
		// default (empty) transport == overlay; simulate an upgrade from
		// no-overlay where the GR was left with only the node's /24 route.
		staleRoute := &nbdb.LogicalRouterStaticRoute{
			UUID:     "stale-host-subnet-route-UUID",
			IPPrefix: nodeHostSubnet,
			Nexthop:  drNextHop,
		}
		runUpdate(newGWConfig(), staleRoute)
		// the stale /24 must be gone, leaving only the cluster /16
		gomega.Expect(drRoutePrefixes()).To(gomega.ConsistOf(clusterSubnet))
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
		nodecontroller.NewNodeAnnotationCache(),
		ovn.addressSetManager,
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

var _ = ginkgo.Describe("No-overlay cluster SNAT cleanup", func() {
	var fakeOvn *FakeOVN

	buildMarkedClusterSNAT := func(externalIP net.IP, clusterSubnet *net.IPNet, exemptedExtIPs, uuid string) *nbdb.NAT {
		extIDs := getNoOverlayClusterSubnetSNATDbIDs(types.DefaultNetworkControllerName, types.DefaultNetworkName,
			nodeName, clusterSubnet).GetExternalIDs()
		nat := libovsdbops.BuildSNATWithExemptedExtIPs(&externalIP, clusterSubnet, "", extIDs, "", exemptedExtIPs)
		nat.UUID = uuid
		return nat
	}

	buildUnmarkedClusterSNAT := func(externalIP net.IP, clusterSubnet *net.IPNet, exemptedExtIPs, uuid string) *nbdb.NAT {
		nat := libovsdbops.BuildSNATWithExemptedExtIPs(&externalIP, clusterSubnet, "", nil, "", exemptedExtIPs)
		nat.UUID = uuid
		return nat
	}

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.Gateway.Mode = config.GatewayModeShared
		config.Gateway.EphemeralPortRange = config.DefaultEphemeralPortRange
		fakeOvn = NewFakeOVN(false, nodeName)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.It("removes unmarked cluster subnet SNATs with missing or stale exemptions", func() {
		externalIPv4 := net.ParseIP("172.18.0.5")
		externalIPv6 := net.ParseIP("fd00::5")
		clusterSubnetV4 := ovntest.MustParseIPNets("10.244.0.0/16")[0]
		clusterSubnetV6 := ovntest.MustParseIPNets("fd01::/48")[0]
		joinSubnet := ovntest.MustParseIPNets("100.64.0.2/32")[0]

		plainSNATV4 := buildUnmarkedClusterSNAT(externalIPv4, clusterSubnetV4, "", "plain-snat-v4-UUID")
		staleSNATV4 := buildUnmarkedClusterSNAT(externalIPv4, clusterSubnetV4, "stale-as-v4-UUID", "stale-snat-v4-UUID")
		desiredSNATV4 := buildMarkedClusterSNAT(externalIPv4, clusterSubnetV4, "desired-as-v4-UUID", "desired-snat-v4-UUID")
		plainSNATV6 := buildUnmarkedClusterSNAT(externalIPv6, clusterSubnetV6, "", "plain-snat-v6-UUID")
		staleSNATV6 := buildUnmarkedClusterSNAT(externalIPv6, clusterSubnetV6, "stale-as-v6-UUID", "stale-snat-v6-UUID")
		desiredSNATV6 := buildMarkedClusterSNAT(externalIPv6, clusterSubnetV6, "desired-as-v6-UUID", "desired-snat-v6-UUID")
		joinSNAT := libovsdbops.BuildSNAT(&externalIPv4, joinSubnet, "", nil)
		joinSNAT.UUID = "join-snat-UUID"
		desiredAddressSetV4 := &nbdb.AddressSet{
			UUID: "desired-as-v4-UUID",
			Name: "desired-as-v4",
		}
		desiredAddressSetV6 := &nbdb.AddressSet{
			UUID: "desired-as-v6-UUID",
			Name: "desired-as-v6",
		}
		staleAddressSetV4 := &nbdb.AddressSet{
			UUID: "stale-as-v4-UUID",
			Name: "stale-as-v4",
		}
		staleAddressSetV6 := &nbdb.AddressSet{
			UUID: "stale-as-v6-UUID",
			Name: "stale-as-v6",
		}

		gwRouter := &nbdb.LogicalRouter{
			UUID: "GR_" + nodeName + "-UUID",
			Name: "GR_" + nodeName,
			Nat: []string{
				plainSNATV4.UUID, desiredSNATV4.UUID, staleSNATV4.UUID,
				plainSNATV6.UUID, desiredSNATV6.UUID, staleSNATV6.UUID,
				joinSNAT.UUID,
			},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				gwRouter,
				plainSNATV4, desiredSNATV4, staleSNATV4,
				plainSNATV6, desiredSNATV6, staleSNATV6,
				joinSNAT,
				desiredAddressSetV4, desiredAddressSetV6,
				staleAddressSetV4, staleAddressSetV6,
			},
		})
		asV4UUID := resolveDesiredClusterSNAT(fakeOvn, desiredSNATV4, desiredAddressSetV4.Name)
		asV6UUID := resolveDesiredClusterSNAT(fakeOvn, desiredSNATV6, desiredAddressSetV6.Name)

		gw := newGatewayManager(fakeOvn, nodeName)
		err := transactClusterSNATSync(fakeOvn, gw, gwRouter, types.DefaultNetworkControllerName, []*nbdb.NAT{desiredSNATV4, desiredSNATV6})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		routerNATs, err := libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouter)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		remainingJoinSNATs := []*nbdb.NAT{}
		for _, nat := range routerNATs {
			if nat.LogicalIP == "100.64.0.2" {
				remainingJoinSNATs = append(remainingJoinSNATs, nat)
			}
		}
		expectRouterClusterSNATWithExemption(routerNATs, clusterSubnetV4.String(), asV4UUID)
		expectRouterClusterSNATWithExemption(routerNATs, clusterSubnetV6.String(), asV6UUID)
		gomega.Expect(remainingJoinSNATs).To(gomega.HaveLen(1))

		clusterSNATsV4, err := libovsdbops.FindNATsWithPredicate(fakeOvn.nbClient, func(nat *nbdb.NAT) bool {
			return nat.Type == nbdb.NATTypeSNAT &&
				nat.ExternalIP == externalIPv4.String() &&
				nat.LogicalIP == clusterSubnetV4.String()
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(clusterSNATsV4).To(gomega.HaveLen(1))
		clusterSNATsV6, err := libovsdbops.FindNATsWithPredicate(fakeOvn.nbClient, func(nat *nbdb.NAT) bool {
			return nat.Type == nbdb.NATTypeSNAT &&
				nat.ExternalIP == externalIPv6.String() &&
				nat.LogicalIP == clusterSubnetV6.String()
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(clusterSNATsV6).To(gomega.HaveLen(1))
	})

	ginkgo.It("removes marked cluster subnet SNATs that are no longer desired", func() {
		externalIP := net.ParseIP("172.18.0.5")
		clusterSubnet := ovntest.MustParseIPNets("10.244.0.0/16")[0]
		oldClusterSubnet := ovntest.MustParseIPNets("10.128.0.0/16")[0]

		desiredSNAT := buildMarkedClusterSNAT(externalIP, clusterSubnet, "desired-as-UUID", "desired-snat-UUID")
		undesiredSNAT := buildMarkedClusterSNAT(externalIP, oldClusterSubnet, "desired-as-UUID", "undesired-snat-UUID")
		desiredAddressSet := &nbdb.AddressSet{
			UUID: "desired-as-UUID",
			Name: "desired-as",
		}

		gwRouter := &nbdb.LogicalRouter{
			UUID: "GR_" + nodeName + "-UUID",
			Name: "GR_" + nodeName,
			Nat:  []string{desiredSNAT.UUID, undesiredSNAT.UUID},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{gwRouter, desiredSNAT, undesiredSNAT, desiredAddressSet},
		})
		resolveDesiredClusterSNAT(fakeOvn, desiredSNAT, desiredAddressSet.Name)

		gw := newGatewayManager(fakeOvn, nodeName)
		err := transactClusterSNATSync(fakeOvn, gw, gwRouter, types.DefaultNetworkControllerName, []*nbdb.NAT{desiredSNAT})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		routerNATs, err := libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouter)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(routerNATs).To(gomega.HaveLen(1))
		gomega.Expect(routerNATs[0].UUID).To(gomega.Equal(desiredSNAT.UUID))
		gomega.Expect(routerNATs[0].LogicalIP).To(gomega.Equal(clusterSubnet.String()))
	})

	ginkgo.It("only cleans up the given gateway router", func() {
		externalIP := net.ParseIP("172.18.0.5")
		clusterSubnet := ovntest.MustParseIPNets("10.244.0.0/16")[0]
		desiredAddressSet := &nbdb.AddressSet{
			UUID: "desired-as-UUID",
			Name: "desired-as",
		}
		testData := []libovsdbtest.TestData{desiredAddressSet}
		gwRouters := []*nbdb.LogicalRouter{}
		desiredSNATs := map[string]*nbdb.NAT{}

		for _, gwNodeName := range []string{"test-node-a", "test-node-b"} {
			plainSNAT := buildUnmarkedClusterSNAT(externalIP, clusterSubnet, "", "plain-snat-"+gwNodeName+"-UUID")
			desiredSNAT := buildMarkedClusterSNAT(externalIP, clusterSubnet, desiredAddressSet.UUID, "desired-snat-"+gwNodeName+"-UUID")
			gwRouter := &nbdb.LogicalRouter{
				UUID: "GR_" + gwNodeName + "-UUID",
				Name: "GR_" + gwNodeName,
				Nat:  []string{plainSNAT.UUID, desiredSNAT.UUID},
			}
			gwRouters = append(gwRouters, gwRouter)
			desiredSNATs[gwRouter.Name] = desiredSNAT
			testData = append(testData, gwRouter, plainSNAT, desiredSNAT)
		}

		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: testData})
		// Both routers share the same exemption address set.
		resolvedAS, err := libovsdbops.GetAddressSet(fakeOvn.nbClient, &nbdb.AddressSet{Name: desiredAddressSet.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, gwRouter := range gwRouters {
			resolveDesiredClusterSNAT(fakeOvn, desiredSNATs[gwRouter.Name], desiredAddressSet.Name)
		}

		gw := newGatewayManager(fakeOvn, nodeName)
		err = transactClusterSNATSync(fakeOvn, gw, gwRouters[0], types.DefaultNetworkControllerName, []*nbdb.NAT{desiredSNATs[gwRouters[0].Name]})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// the first router is cleaned up, the second one is untouched
		routerNATs, err := libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouters[0])
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(routerNATs).To(gomega.HaveLen(1))
		gomega.Expect(routerNATs[0].UUID).To(gomega.Equal(desiredSNATs[gwRouters[0].Name].UUID))
		routerNATs, err = libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouters[1])
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(routerNATs).To(gomega.HaveLen(2))

		err = transactClusterSNATSync(fakeOvn, gw, gwRouters[1], types.DefaultNetworkControllerName, []*nbdb.NAT{desiredSNATs[gwRouters[1].Name]})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, gwRouter := range gwRouters {
			routerNATs, err := libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouter)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(routerNATs).To(gomega.HaveLen(1))
			gomega.Expect(routerNATs[0].ExemptedExtIPs).NotTo(gomega.BeNil())
			gomega.Expect(*routerNATs[0].ExemptedExtIPs).To(gomega.Equal(resolvedAS.UUID))
		}
	})

	ginkgo.It("replaces an unmarked cluster subnet SNAT and keeps the marked one on a second sync", func() {
		externalIP := net.ParseIP("172.18.0.5")
		clusterSubnet := ovntest.MustParseIPNets("10.244.0.0/16")[0]

		plainSNAT := buildUnmarkedClusterSNAT(externalIP, clusterSubnet, "", "plain-snat-UUID")
		desiredAddressSet := &nbdb.AddressSet{
			UUID: "desired-as-UUID",
			Name: "desired-as",
		}
		gwRouter := &nbdb.LogicalRouter{
			UUID: "GR_" + nodeName + "-UUID",
			Name: "GR_" + nodeName,
			Nat:  []string{plainSNAT.UUID},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{gwRouter, plainSNAT, desiredAddressSet},
		})

		gw := newGatewayManager(fakeOvn, nodeName)
		syncOnce := func() string {
			// updateGWRouterNAT builds the desired SNATs from scratch on every sync
			desiredSNAT := buildMarkedClusterSNAT(externalIP, clusterSubnet, "", "")
			asUUID := resolveDesiredClusterSNAT(fakeOvn, desiredSNAT, desiredAddressSet.Name)
			err := transactClusterSNATSync(fakeOvn, gw, gwRouter, types.DefaultNetworkControllerName, []*nbdb.NAT{desiredSNAT})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return asUUID
		}

		// on upgrade, the plain SNAT is replaced by a marked, exempted one
		asUUID := syncOnce()
		routerNATs, err := libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouter)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(routerNATs).To(gomega.HaveLen(1))
		gomega.Expect(routerNATs[0].ExternalIDs).To(gomega.HaveKey(libovsdbops.PrimaryIDKey.String()))
		expectRouterClusterSNATWithExemption(routerNATs, clusterSubnet.String(), asUUID)
		markedUUID := routerNATs[0].UUID

		// a steady-state second sync leaves the row untouched
		syncOnce()
		routerNATs, err = libovsdbops.GetRouterNATs(fakeOvn.nbClient, gwRouter)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(routerNATs).To(gomega.HaveLen(1))
		gomega.Expect(routerNATs[0].UUID).To(gomega.Equal(markedUUID))
		expectRouterClusterSNATWithExemption(routerNATs, clusterSubnet.String(), asUUID)
	})
})

// resolveDesiredClusterSNAT updates a placeholder desired SNAT to reference the
// real UUID of the exemption address set in the test DB, and returns that UUID
// so callers can assert on it.
func resolveDesiredClusterSNAT(fakeOvn *FakeOVN, desiredSNAT *nbdb.NAT, addressSetName string) string {
	as, err := libovsdbops.GetAddressSet(fakeOvn.nbClient, &nbdb.AddressSet{Name: addressSetName})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	desiredSNAT.ExemptedExtIPs = &as.UUID
	return as.UUID
}

// transactClusterSNATSync builds and commits the cluster-subnet SNAT ops for a
// single gateway router the same way updateGWRouterNAT does: create or update
// the desired SNATs and delete the stale rows, all in one transaction.
func transactClusterSNATSync(fakeOvn *FakeOVN, gw *GatewayManager, gwRouter *nbdb.LogicalRouter, controllerName string, desiredNATs []*nbdb.NAT) error {
	ops, err := libovsdbops.CreateOrUpdateNATsOps(fakeOvn.nbClient, nil, gwRouter, desiredNATs...)
	if err != nil {
		return err
	}
	ops, err = gw.deleteStaleNoOverlayClusterSNATsOps(ops, gwRouter, controllerName, desiredNATs)
	if err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(fakeOvn.nbClient, ops)
	return err
}

var _ = ginkgo.Describe("GetNetworkScopedClusterSubnetSNATMatch", func() {
	var (
		fakeOvn           *FakeOVN
		addressSetFactory addressset.AddressSetFactory
		netInfo           *testNetInfo
		nodeName          = "test-node"
	)

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		fakeOvn = NewFakeOVN(false, nodeName)
		fakeOvn.start()
		netInfo = &testNetInfo{
			NetInfo: fakeOvn.controller.GetNetInfo(),
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("when network is not advertised", func() {
		ginkgo.It("returns empty match for Layer3 topology", func() {
			netInfo.topology = types.Layer3Topology
			match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, false, utilnet.IPv4, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(match).To(gomega.BeEmpty())
		})

		ginkgo.It("returns empty match for Layer2 topology with transit router", func() {
			originalValue := config.Layer2UsesTransitRouter
			defer func() { config.Layer2UsesTransitRouter = originalValue }()
			config.Layer2UsesTransitRouter = true
			netInfo.topology = types.Layer2Topology
			match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, false, utilnet.IPv4, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(match).To(gomega.BeEmpty())
		})

		ginkgo.It("returns outport match for Layer2 topology without transit router", func() {
			originalValue := config.Layer2UsesTransitRouter
			defer func() { config.Layer2UsesTransitRouter = originalValue }()
			config.Layer2UsesTransitRouter = false
			netInfo.topology = types.Layer2Topology
			match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, false, utilnet.IPv4, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedMatch := fmt.Sprintf("outport == %q", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName))
			gomega.Expect(match).To(gomega.Equal(expectedMatch))
		})
	})

	ginkgo.Context("when network is advertised", func() {
		ginkgo.Context("when outbound SNAT is enabled", func() {
			ginkgo.BeforeEach(func() {
				netInfo.transport = types.NetworkTransportNoOverlay
				netInfo.outboundSNAT = types.NoOverlaySNATEnabled
			})

			ginkgo.Context("in shared gateway mode", func() {
				ginkgo.BeforeEach(func() {
					config.Gateway.Mode = config.GatewayModeShared
				})

				ginkgo.It("returns empty match for Layer3 topology", func() {
					netInfo.topology = types.Layer3Topology
					match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, true, utilnet.IPv4, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(match).To(gomega.BeEmpty())
				})
			})

			ginkgo.Context("in local gateway mode", func() {
				ginkgo.BeforeEach(func() {
					config.Gateway.Mode = config.GatewayModeLocal
					addressSetFactory = addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, config.IPv4Mode, config.IPv6Mode)
					dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
					as, err := addressSetFactory.EnsureAddressSet(dbIDs)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = as.AddAddresses([]string{"10.0.0.1"})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("returns destination match for Layer3 topology", func() {
					netInfo.topology = types.Layer3Topology
					dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
					match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, true, utilnet.IPv4, dbIDs)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					as, err := addressSetFactory.GetAddressSet(dbIDs)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					v4Hash, _ := as.GetASHashNames()
					expectedMatch := fmt.Sprintf("ip4.dst == $%s", v4Hash)
					gomega.Expect(match).To(gomega.Equal(expectedMatch))
				})
			})
		})

		ginkgo.Context("when outbound SNAT is disabled", func() {
			ginkgo.BeforeEach(func() {
				addressSetFactory = addressset.NewOvnAddressSetFactory(fakeOvn.nbClient, config.IPv4Mode, config.IPv6Mode)
				dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
				as, err := addressSetFactory.EnsureAddressSet(dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = as.AddAddresses([]string{"10.0.0.1"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("returns destination match for Layer3 topology", func() {
				netInfo.topology = types.Layer3Topology
				dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
				match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, true, utilnet.IPv4, dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				as, err := addressSetFactory.GetAddressSet(dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				v4Hash, _ := as.GetASHashNames()
				expectedMatch := fmt.Sprintf("ip4.dst == $%s", v4Hash)
				gomega.Expect(match).To(gomega.Equal(expectedMatch))
			})

			ginkgo.It("returns destination match for Layer2 topology with transit router", func() {
				originalValue := config.Layer2UsesTransitRouter
				defer func() { config.Layer2UsesTransitRouter = originalValue }()
				config.Layer2UsesTransitRouter = true
				netInfo.topology = types.Layer2Topology
				dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
				match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, true, utilnet.IPv4, dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				as, err := addressSetFactory.GetAddressSet(dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				v4Hash, _ := as.GetASHashNames()
				expectedMatch := fmt.Sprintf("ip4.dst == $%s", v4Hash)
				gomega.Expect(match).To(gomega.Equal(expectedMatch))
			})

			ginkgo.It("returns outport and destination match for Layer2 topology without transit router", func() {
				originalValue := config.Layer2UsesTransitRouter
				defer func() { config.Layer2UsesTransitRouter = originalValue }()
				config.Layer2UsesTransitRouter = false
				netInfo.topology = types.Layer2Topology
				dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
				match, err := GetNetworkScopedClusterSubnetSNATMatch(fakeOvn.nbClient, netInfo, nodeName, true, utilnet.IPv4, dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				as, err := addressSetFactory.GetAddressSet(dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				v4Hash, _ := as.GetASHashNames()
				expectedMatch := fmt.Sprintf("outport == %q && ip4.dst == $%s", types.GWRouterToExtSwitchPrefix+netInfo.GetNetworkScopedGWRouterName(nodeName), v4Hash)
				gomega.Expect(match).To(gomega.Equal(expectedMatch))
			})
		})
	})
})

var _ = ginkgo.Describe("AddPodSNATOps", func() {
	var (
		fakeOvn    *FakeOVN
		controller *DefaultNetworkController
		nodeName   = "test-node"
		nodeIP     = "192.168.1.10"
		nodeIPv6   = "fd00::1234"
	)

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		fakeOvn = NewFakeOVN(false, nodeName)

		// Create the gateway router in the fake database
		gwRouterName := "GR_" + nodeName
		gwRouter := &nbdb.LogicalRouter{
			UUID: gwRouterName + "-UUID",
			Name: gwRouterName,
		}

		// Create a node with L3 gateway configuration
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s/24\", \"ipv6\": \"%s/64\"}", nodeIP, nodeIPv6),
					"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet),
					util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s/24\", \"%s/64\"]", nodeIP, nodeIPv6),
					"k8s.ovn.org/node-chassis-id":     "test-chassis",
					util.OvnNodeL3GatewayConfig:       fmt.Sprintf(`{"default":{"mode":"shared","mac-address":"7e:57:f8:f0:3c:49", "ip-addresses": ["%s/24", "%s/64"], "next-hops": ["192.168.1.1", "fd00::1"]}}`, nodeIP, nodeIPv6),
				},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		// Initialize fake DB and watcher with the gateway router and node
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				gwRouter,
			},
		}, node)
		controller = fakeOvn.controller

		// Create address set for node IPs (required for SNAT match)
		dbIDs := getClusterNodeIPsAddrSetDbIDsForTest()
		nodeIPAddrSet, asErr := controller.addressSetFactory.EnsureAddressSet(dbIDs)
		gomega.Expect(asErr).NotTo(gomega.HaveOccurred())
		asErr = nodeIPAddrSet.AddAddresses([]string{nodeIP, nodeIPv6})
		gomega.Expect(asErr).NotTo(gomega.HaveOccurred())
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.It("creates SNAT operations for IPv4 pod IPs", func() {
		podIPs := ovntest.MustParseIPNets("10.128.1.5/24")

		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Should have 2 ops: 1 insert NAT + 1 mutate LogicalRouter
		gomega.Expect(ops).To(gomega.HaveLen(2))
	})

	ginkgo.It("creates SNAT operations for IPv6 pod IPs", func() {
		// Set up IPv6-only mode
		originalIPv4Mode := config.IPv4Mode
		config.IPv4Mode = false
		originalIPv6Mode := config.IPv6Mode
		config.IPv6Mode = true
		defer func() {
			config.IPv4Mode = originalIPv4Mode
			config.IPv6Mode = originalIPv6Mode
		}()

		podIPs := ovntest.MustParseIPNets("fd00::5/64")

		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Should have 2 ops: 1 insert NAT + 1 mutate LogicalRouter
		gomega.Expect(ops).To(gomega.HaveLen(2))
	})

	ginkgo.It("creates SNAT operations for dual-stack pod IPs", func() {
		// Set up dual-stack mode
		originalIPv4Mode := config.IPv4Mode
		originalIPv6Mode := config.IPv6Mode
		config.IPv4Mode = true
		config.IPv6Mode = true
		defer func() {
			config.IPv4Mode = originalIPv4Mode
			config.IPv6Mode = originalIPv6Mode
		}()

		podIPs := ovntest.MustParseIPNets("10.128.1.5/24", "fd00::5/64")

		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Should have 4 ops: 2 for IPv4 (insert NAT + mutate LR) + 2 for IPv6 (insert NAT + mutate LR)
		gomega.Expect(ops).To(gomega.HaveLen(4))
	})

	ginkgo.It("creates SNAT operations with exempted external IPs for no-overlay outboundSNAT enabled mode", func() {
		// Set up no-overlay mode with outbound SNAT enabled
		// We need to set the global config because DefaultNetInfo.Transport() reads from config.Default.Transport
		originalTransport := config.Default.Transport
		defer func() { config.Default.Transport = originalTransport }()
		config.Default.Transport = types.NetworkTransportNoOverlay

		originalOutboundSNAT := config.NoOverlay.OutboundSNAT
		defer func() { config.NoOverlay.OutboundSNAT = originalOutboundSNAT }()
		config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled

		originalGatewayMode := config.Gateway.Mode
		defer func() { config.Gateway.Mode = originalGatewayMode }()
		config.Gateway.Mode = config.GatewayModeShared

		// Configure the network as advertised on this node
		controller.GetNetInfo().(util.MutableNetInfo).SetPodNetworkAdvertisedVRFs(map[string][]string{
			nodeName: {"default"},
		})
		// Create the no-overlay SNAT exemption address set in the fake database
		dbIDs := libovsdbops.NewDbObjectIDs(
			libovsdbops.AddressSetNoOverlaySNATExemption,
			controller.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: "no-overlay-snat-exemption",
				libovsdbops.NetworkKey:    controller.GetNetworkName(),
			},
		)
		as, err := controller.addressSetFactory.EnsureAddressSet(dbIDs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(as).NotTo(gomega.BeNil())

		podIPs := ovntest.MustParseIPNets("10.128.1.5/24")

		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Should have 2 ops: 1 insert NAT + 1 mutate LogicalRouter
		gomega.Expect(ops).To(gomega.HaveLen(2))

		// Get the address set UUID to verify it's referenced correctly
		v4UUID, _ := as.GetASUUID()
		gomega.Expect(v4UUID).NotTo(gomega.BeEmpty(), "Address set should have a UUID")

		// Find the NAT insert operation and verify it has exempted external IPs
		foundNATOp := false
		for _, op := range ops {
			if op.Op == "insert" && op.Table == "NAT" {
				foundNATOp = true
				gomega.Expect(op.Row).ToNot(gomega.HaveKey("match"))

				// The NAT row should have exempted_ext_ips set
				gomega.Expect(op.Row).To(gomega.HaveKey("exempted_ext_ips"))
				exemptedExtIPs := op.Row["exempted_ext_ips"]
				gomega.Expect(exemptedExtIPs).NotTo(gomega.BeNil(), "exempted_ext_ips should be set for no-overlay mode")
				break
			}
		}
		gomega.Expect(foundNATOp).To(gomega.BeTrue(), "Should have NAT insert operation")
	})

	ginkgo.It("returns an error when the no-overlay exemption address set is missing", func() {
		originalTransport := config.Default.Transport
		defer func() { config.Default.Transport = originalTransport }()
		config.Default.Transport = types.NetworkTransportNoOverlay

		originalOutboundSNAT := config.NoOverlay.OutboundSNAT
		defer func() { config.NoOverlay.OutboundSNAT = originalOutboundSNAT }()
		config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled

		originalGatewayMode := config.Gateway.Mode
		defer func() { config.Gateway.Mode = originalGatewayMode }()
		config.Gateway.Mode = config.GatewayModeShared

		ops, err := controller.AddPodSNATOps(nodeName, ovntest.MustParseIPNets("10.128.1.5/24"))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no-overlay SNAT exemption address set")))
		gomega.Expect(ops).To(gomega.BeNil())
	})

	ginkgo.It("returns error when node is not found", func() {
		podIPs := ovntest.MustParseIPNets("10.128.1.5/24")

		ops, err := controller.AddPodSNATOps("non-existent-node", podIPs)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(ops).To(gomega.BeNil())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("failed to get node"))
	})

	ginkgo.It("handles empty pod IPs list", func() {
		var podIPs []*net.IPNet

		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ops).To(gomega.BeEmpty())
	})

	ginkgo.It("creates SNAT operations for advertised network", func() {
		// The address set for node IPs is already created in BeforeEach
		podIPs := ovntest.MustParseIPNets("10.128.1.5/24")
		// Configure the network as advertised on this node
		controller.GetNetInfo().(util.MutableNetInfo).SetPodNetworkAdvertisedVRFs(map[string][]string{
			nodeName: {"default"},
		})
		ops, err := controller.AddPodSNATOps(nodeName, podIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// Should have 2 ops: 1 insert NAT + 1 mutate LogicalRouter
		gomega.Expect(ops).To(gomega.HaveLen(2))

		foundNATOp := false
		for _, op := range ops {
			if op.Op == "insert" && op.Table == "NAT" {
				foundNATOp = true
				gomega.Expect(op.Row).ToNot(gomega.HaveKey("exempted_ext_ips"))

				// The NAT row should have match set
				gomega.Expect(op.Row).To(gomega.HaveKey("match"))
				exemptedExtIPs := op.Row["match"]
				gomega.Expect(exemptedExtIPs).NotTo(gomega.BeNil(), "match should be set for no-overlay mode")
				break
			}
		}
		gomega.Expect(foundNATOp).To(gomega.BeTrue(), "Should have NAT insert operation")

	})
})

var _ = ginkgo.Describe("cleanupStalePodSNATs", func() {
	var (
		fakeOvn *FakeOVN
	)
	const testNode = "test-node"

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		fakeOvn = NewFakeOVN(true, testNode)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.It("returns error when router does not exist", func() {
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{})

		gw := newGatewayManager(fakeOvn, testNode)
		nodeIPs := ovntest.MustParseIPNets("169.255.33.2/24")
		gwLRPIPs := ovntest.MustParseIPs("100.64.0.3")

		err := gw.cleanupStalePodSNATs(testNode, nodeIPs, gwLRPIPs)
		gomega.Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(gomega.BeTrue())
	})

	ginkgo.It("deletes stale pod SNAT", func() {
		gwRouterName := "GR_" + testNode
		staleNAT := &nbdb.NAT{
			UUID:       "stale-nat-UUID",
			Type:       nbdb.NATTypeSNAT,
			ExternalIP: "169.255.33.2",
			LogicalIP:  "10.128.0.5",
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: gwRouterName + "-UUID",
					Name: gwRouterName,
					Nat:  []string{staleNAT.UUID},
				},
				staleNAT,
			},
		})

		gw := newGatewayManager(fakeOvn, testNode)
		nodeIPs := ovntest.MustParseIPNets("169.255.33.2/24")
		gwLRPIPs := ovntest.MustParseIPs("100.64.0.3")

		err := gw.cleanupStalePodSNATs(testNode, nodeIPs, gwLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDB := []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				UUID: gwRouterName + "-UUID",
				Name: gwRouterName,
				Nat:  []string{},
			},
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDB))
	})

	ginkgo.It("preserves SNAT for gateway LRP IP", func() {
		gwRouterName := "GR_" + testNode
		gwLRPNAT := &nbdb.NAT{
			UUID:       "gwlrp-nat-UUID",
			Type:       nbdb.NATTypeSNAT,
			ExternalIP: "169.255.33.2",
			LogicalIP:  "100.64.0.3",
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: gwRouterName + "-UUID",
					Name: gwRouterName,
					Nat:  []string{gwLRPNAT.UUID},
				},
				gwLRPNAT,
			},
		})

		gw := newGatewayManager(fakeOvn, testNode)
		nodeIPs := ovntest.MustParseIPNets("169.255.33.2/24")
		gwLRPIPs := ovntest.MustParseIPs("100.64.0.3")

		err := gw.cleanupStalePodSNATs(testNode, nodeIPs, gwLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDB := []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				UUID: gwRouterName + "-UUID",
				Name: gwRouterName,
				Nat:  []string{gwLRPNAT.UUID},
			},
			gwLRPNAT,
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDB))
	})

	ginkgo.It("preserves SNAT for active pod when DisableSNATMultipleGWs is true", func() {
		gwRouterName := "GR_" + testNode
		podNAT := &nbdb.NAT{
			UUID:       "pod-nat-UUID",
			Type:       nbdb.NATTypeSNAT,
			ExternalIP: "169.255.33.2",
			LogicalIP:  "10.128.0.5",
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`,
				},
			},
			Spec: corev1.PodSpec{
				NodeName: testNode,
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.128.0.5",
			},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: gwRouterName + "-UUID",
					Name: gwRouterName,
					Nat:  []string{podNAT.UUID},
				},
				podNAT,
			},
		},
			&corev1.PodList{Items: []corev1.Pod{*pod}},
		)

		config.Gateway.DisableSNATMultipleGWs = true
		gw := newGatewayManager(fakeOvn, testNode)
		nodeIPs := ovntest.MustParseIPNets("169.255.33.2/24")
		gwLRPIPs := ovntest.MustParseIPs("100.64.0.3")

		err := gw.cleanupStalePodSNATs(testNode, nodeIPs, gwLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDB := []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				UUID: gwRouterName + "-UUID",
				Name: gwRouterName,
				Nat:  []string{podNAT.UUID},
			},
			podNAT,
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDB))
	})

	ginkgo.DescribeTable("cleans up completed pod SNATs without local IPAM ownership", func(layer2 bool, replacementNode string) {
		config.Gateway.DisableSNATMultipleGWs = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		var netInfo util.NetInfo = &util.DefaultNetInfo{}
		nadKey := types.DefaultNetworkName
		podCIDR := "10.128.0.5/24"
		if layer2 {
			var err error
			netInfo, err = util.ParseNADInfo(ovntest.GenerateNAD("bluenet", "rednad", "default",
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nadKey = "default/rednad"
			podCIDR = "100.128.0.5/24"
		}
		ips := ovntest.MustParseIPNets(podCIDR)
		completed := ovntest.NewPod("default", "completed", testNode, ips[0].IP.String())
		completed.UID = "completed"
		completed.Status.Phase = corev1.PodSucceeded
		var err error
		completed.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{
			IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP), Role: types.NetworkRolePrimary,
		}, nadKey)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pods := &corev1.PodList{Items: []corev1.Pod{*completed}}
		if replacementNode != "" {
			replacement := completed.DeepCopy()
			replacement.Name, replacement.UID = "replacement", "replacement"
			replacement.Spec.NodeName = replacementNode
			replacement.Status.Phase = corev1.PodRunning
			pods.Items = append(pods.Items, *replacement)
		}
		nat := &nbdb.NAT{
			UUID: "pod-nat-UUID", Type: nbdb.NATTypeSNAT,
			ExternalIP: "169.255.33.2", LogicalIP: ips[0].IP.String(),
		}
		router := &nbdb.LogicalRouter{
			UUID: "gateway-UUID", Name: netInfo.GetNetworkScopedGWRouterName(testNode), Nat: []string{nat.UUID},
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{router, nat},
		}, pods)
		gomega.Expect(fakeOvn.controller.lsManager.GetSwitchSubnets(testNode)).To(gomega.BeEmpty())
		gw := newGatewayManager(fakeOvn, testNode)
		gw.netInfo, gw.gwRouterName = netInfo, router.Name
		gw.getNetworkNameForNADKey = func(key string) string {
			if key == nadKey {
				return netInfo.GetNetworkName()
			}
			return ""
		}
		gomega.Expect(gw.cleanupStalePodSNATs(testNode, ovntest.MustParseIPNets("169.255.33.2/24"), nil)).To(gomega.Succeed())
		expectedRouter := *router
		expected := []libovsdbtest.TestData{&expectedRouter}
		if replacementNode != "" && replacementNode != testNode {
			expectedRouter.Nat = []string{}
		} else {
			expected = append(expected, nat)
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expected))
	},
		ginkgo.Entry("default network before pod sync", false, ""),
		ginkgo.Entry("default network with local IP reuse", false, testNode),
		ginkgo.Entry("default network with remote IP reuse", false, "other-node"),
		ginkgo.Entry("Layer2 network", true, ""),
		ginkgo.Entry("Layer2 network with local IP reuse", true, testNode),
		ginkgo.Entry("Layer2 network with remote IP reuse", true, "other-node"),
	)

	ginkgo.It("deletes pod SNAT when DisableSNATMultipleGWs is false", func() {
		gwRouterName := "GR_" + testNode
		podNAT := &nbdb.NAT{
			UUID:       "pod-nat-UUID",
			Type:       nbdb.NATTypeSNAT,
			ExternalIP: "169.255.33.2",
			LogicalIP:  "10.128.0.5",
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: gwRouterName + "-UUID",
					Name: gwRouterName,
					Nat:  []string{podNAT.UUID},
				},
				podNAT,
			},
		},
			&corev1.PodList{Items: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["10.128.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`,
						},
					},
					Spec: corev1.PodSpec{
						NodeName: testNode,
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						PodIP: "10.128.0.5",
					},
				},
			}},
		)

		config.Gateway.DisableSNATMultipleGWs = false
		gw := newGatewayManager(fakeOvn, testNode)
		nodeIPs := ovntest.MustParseIPNets("169.255.33.2/24")
		gwLRPIPs := ovntest.MustParseIPs("100.64.0.3")

		err := gw.cleanupStalePodSNATs(testNode, nodeIPs, gwLRPIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectedDB := []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				UUID: gwRouterName + "-UUID",
				Name: gwRouterName,
				Nat:  []string{},
			},
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDB))
	})
})
