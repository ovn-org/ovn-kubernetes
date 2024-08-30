package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestCreateDefaultRouteToExternal(t *testing.T) {
	nodeName := "ovn-control-plane"
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/14")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr4, 24}}
	expectedStaticRoute := &nbdb.LogicalRouterStaticRoute{
		Nexthop:  "100.64.0.1",
		IPPrefix: "10.128.0.0/14",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "route1-UUID",
	}
	unexpectedStaticRoute := &nbdb.LogicalRouterStaticRoute{
		Nexthop:  "100.66.0.1",
		IPPrefix: "10.128.0.0/14",
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
		UUID:     "route2-UUID",
	}
	initialOVNClusterRouter := &nbdb.LogicalRouter{
		UUID:         types.OVNClusterRouter + "-UUID",
		Name:         types.OVNClusterRouter,
		StaticRoutes: []string{"route2-UUID"},
	}
	expectedOVNClusterRouter := &nbdb.LogicalRouter{
		UUID:         types.OVNClusterRouter + "-UUID",
		Name:         types.OVNClusterRouter,
		StaticRoutes: []string{"route1-UUID"},
	}
	expectedLogicalRouterPort := &nbdb.LogicalRouterPort{
		Name:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName,
		UUID:     types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID",
		Networks: []string{"100.64.0.1/16"},
	}
	GRName := "GR_" + nodeName
	expectedOVNGatewayRouter := &nbdb.LogicalRouter{
		UUID:  GRName + "-UUID",
		Name:  GRName,
		Ports: []string{types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + nodeName + "-UUID"},
	}

	tests := []struct {
		desc         string
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc: "removes stale static route and creates new route with current gateway router logical port IP",
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					unexpectedStaticRoute,
					initialOVNClusterRouter,
					expectedLogicalRouterPort,
					expectedOVNGatewayRouter,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					expectedStaticRoute,
					expectedOVNClusterRouter,
					expectedLogicalRouterPort,
					expectedOVNGatewayRouter,
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

			var e error
			if e = CreateDefaultRouteToExternal(nbClient, types.OVNClusterRouter, GRName); e != nil {
				t.Fatal(fmt.Errorf("failed to create pod to external catch-all reroute for gateway router %s, err: %v", GRName, e))
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
