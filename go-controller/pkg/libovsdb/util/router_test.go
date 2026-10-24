// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestCreateDefaultRouteToExternal(t *testing.T) {

	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to PrepareTestConfig: %v", err)
	}
	nodeName := "ovn-worker"

	clusterSubnetV4 := ovntest.MustParseIPNet("10.128.0.0/16")
	clusterSubnetV6 := ovntest.MustParseIPNet("fe00::/48")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: clusterSubnetV4, HostSubnetLength: 24},
		{CIDR: clusterSubnetV6, HostSubnetLength: 64},
	}

	ovnClusterRouterName := types.OVNClusterRouter
	gwRouterName := types.GWRouterPrefix + nodeName
	gwRouterPortName := types.GWRouterToJoinSwitchPrefix + gwRouterName
	gwRouterIPAddressV4 := "100.64.0.3"
	gwRouterIPAddressV6 := "fd98::3"
	gwRouterIPAddressV4CIDR := fmt.Sprintf("%s/32", gwRouterIPAddressV4)
	gwRouterIPAddressV6CIDR := fmt.Sprintf("%s/128", gwRouterIPAddressV6)
	gatewayIPs := []*net.IPNet{ovntest.MustParseIPNet(gwRouterIPAddressV4CIDR), ovntest.MustParseIPNet(gwRouterIPAddressV6CIDR)}
	gwRouterPort := &nbdb.LogicalRouterPort{
		UUID:     gwRouterPortName + "-uuid",
		Name:     gwRouterPortName,
		Networks: []string{gwRouterIPAddressV4 + "/16", gwRouterIPAddressV6 + "/64"},
	}

	clusterSubnetRouteV4 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV4.String(),
		Nexthop:  gwRouterIPAddressV4,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "cluster-subnet-route-v4-uuid",
	}
	clusterSubnetRouteV6 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV6.String(),
		Nexthop:  gwRouterIPAddressV6,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "cluster-subnet-route-v6-uuid",
	}

	// to test that we won't erase the old cluster subnet route that had a different next hop
	wrongNextHopClusterSubnetRouteV4 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV4.String(),
		Nexthop:  "100.64.0.33",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "old-cluster-subnet-route-v4-uuid",
	}
	wrongNextHopClusterSubnetRouteV6 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV6.String(),
		Nexthop:  "fd98::33",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "old-cluster-subnet-route-v6-uuid",
	}

	// to test that we won't erase the existing route for the node subnet via mp0
	_, nodeSubnetV4, _ := net.ParseCIDR("10.128.0.0/24")
	_, nodeSubnetV6, _ := net.ParseCIDR("fe00::/64")
	mp0IPAddressV4 := "100.244.0.2"
	mp0IPAddressV6 := "fe00::2"

	nodeSubnetRouteV4 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: nodeSubnetV4.String(),
		Nexthop:  mp0IPAddressV4,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "node-subnet-route-v4-uuid",
	}
	nodeSubnetRouteV6 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: nodeSubnetV6.String(),
		Nexthop:  mp0IPAddressV6,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "node-subnet-route-v6-uuid",
	}

	// to test that, after cluster subnet expansion, we replace the old cluster subnet route with the new one
	_, newClusterSubnetV4, _ := net.ParseCIDR("10.128.0.0/15")
	_, newClusterSubnetV6, _ := net.ParseCIDR("fe00::/46")
	newClusterSubnetRouteAfterExpansionV4 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: newClusterSubnetV4.String(),
		Nexthop:  gwRouterIPAddressV4,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "new-cluster-subnet-route-v4-uuid",
	}
	newClusterSubnetRouteAfterExpansionV6 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: newClusterSubnetV6.String(),
		Nexthop:  gwRouterIPAddressV6,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "new-cluster-subnet-route-v6-uuid",
	}

	gatewayRouter := &nbdb.LogicalRouter{
		Name:  gwRouterName,
		UUID:  gwRouterName + "-uuid",
		Ports: []string{gwRouterPort.UUID},
	}

	// to test that we remove route with next hop from old join subnet and add route with next hop from current join subnet
	oldNextHopClusterSubnetRouteV4 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV4.String(),
		Nexthop:  "100.66.0.3",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "old-join-subnet-route-v4-uuid",
	}
	oldNextHopClusterSubnetRouteV6 := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: clusterSubnetV6.String(),
		Nexthop:  "fd96::3",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "old-join-subnet-route-v6-uuid",
	}

	tests := []struct {
		desc          string
		initialNbdb   libovsdbtest.TestSetup
		expectedNbdb  libovsdbtest.TestSetup
		preTestAction func()
	}{
		{
			desc: "Add a cluster subnet route to GW router when no cluster subnet route exists",
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID}, // should not be replaced
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, clusterSubnetRouteV4.UUID, clusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
					clusterSubnetRouteV4,
					clusterSubnetRouteV6,
				},
			},
		},
		{
			desc: "Add a cluster subnet route to GW router when a cluster subnet route already exists", // should replace the existing one
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					wrongNextHopClusterSubnetRouteV4,
					wrongNextHopClusterSubnetRouteV6,
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, wrongNextHopClusterSubnetRouteV4.UUID, wrongNextHopClusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, clusterSubnetRouteV4.UUID, clusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
					clusterSubnetRouteV4,
					clusterSubnetRouteV6,
				},
			},
		},
		{
			desc: "Remove route with next hop from old join subnet and add route with next hop from current join subnet", // should replace the existing one
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					oldNextHopClusterSubnetRouteV4,
					oldNextHopClusterSubnetRouteV6,
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, oldNextHopClusterSubnetRouteV4.UUID, oldNextHopClusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, clusterSubnetRouteV4.UUID, clusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
					clusterSubnetRouteV4,
					clusterSubnetRouteV6,
				},
			},
		},
		{
			desc: "Update the cluster subnet route to GW router after cluster subnet expansion", // should replace the existing one
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					clusterSubnetRouteV4,
					clusterSubnetRouteV6,
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, clusterSubnetRouteV4.UUID, clusterSubnetRouteV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
				},
			},
			preTestAction: func() {
				// Apply the new cluster subnets
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
					{CIDR: newClusterSubnetV4, HostSubnetLength: 24},
					{CIDR: newClusterSubnetV6, HostSubnetLength: 64},
				}
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:         ovnClusterRouterName,
						UUID:         ovnClusterRouterName + "-uuid",
						StaticRoutes: []string{nodeSubnetRouteV4.UUID, nodeSubnetRouteV6.UUID, newClusterSubnetRouteAfterExpansionV4.UUID, newClusterSubnetRouteAfterExpansionV6.UUID},
					},
					gatewayRouter,
					gwRouterPort,
					nodeSubnetRouteV4,
					nodeSubnetRouteV6,
					newClusterSubnetRouteAfterExpansionV4,
					newClusterSubnetRouteAfterExpansionV6,
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tc.initialNbdb, nil)
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" failed to create test harness: %v", tc.desc, err))
			}
			t.Cleanup(cleanup.Cleanup)

			if tc.preTestAction != nil {
				tc.preTestAction()
			}

			if err = CreateDefaultRouteToExternal(nbClient, ovnClusterRouterName, gwRouterName, config.Default.ClusterSubnets, gatewayIPs); err != nil {
				t.Fatal(fmt.Errorf("failed to run CreateDefaultRouteToExternal: %v", err))
			}

			matcher := libovsdbtest.HaveDataIgnoringUUIDs(tc.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tc.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tc.desc, err))
			}
		})
	}
}
