package node

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/vishvananda/netlink"
)

func TestFilterRoutesByIfIndex(t *testing.T) {
	tcs := []struct {
		routesUnfiltered []netlink.Route
		gwIfIdx          int
		routesFiltered   []netlink.Route
	}{
		// tc0 - baseline.
		{
			routesUnfiltered: []netlink.Route{},
			gwIfIdx:          0,
			routesFiltered:   []netlink.Route{},
		},
		// tc1 - do not filter.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
			gwIfIdx: 0,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
		},
		// tc2 - filter by link index.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 1,
					Gw:        []byte{3, 3, 3, 3},
				},
			},
		},
		// tc3 - filter by MultiPath index.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
		},
		// tc4 - filter by MultiPath index - MultiPath with multiple interfaces.
		{
			routesUnfiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
				{
					LinkIndex: 2,
					Gw:        []byte{2, 2, 2, 2},
				},
				{
					LinkIndex: 0,
					MultiPath: []*netlink.NexthopInfo{
						{
							LinkIndex: 1,
							Hops:      0,
							Gw:        []byte{3, 3, 3, 3},
						},
						{
							LinkIndex: 2,
							Hops:      0,
							Gw:        []byte{4, 4, 4, 4},
						},
					},
				},
			},
			gwIfIdx: 1,
			routesFiltered: []netlink.Route{
				{
					LinkIndex: 1,
					Gw:        []byte{1, 1, 1, 1},
				},
			},
		},
	}

	for i, tc := range tcs {
		routesFiltered := filterRoutesByIfIndex(tc.routesUnfiltered, tc.gwIfIdx)
		if !reflect.DeepEqual(tc.routesFiltered, routesFiltered) {
			t.Fatalf("TestFilterRoutesByIfIndex(%d): Filtering '%v' by index '%d' should have yielded '%v' but got '%v'",
				i, tc.routesUnfiltered, tc.gwIfIdx, tc.routesFiltered, routesFiltered)
		}
	}
}

func TestGetDefaultGatewayInterfaceByFamily(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	defer util.ResetNetLinkOpMockInst()

	defaultIf := "testInterface"
	customIf := "customTestInterface"
	defaultGWIP := ovntest.MustParseIP("1.1.1.1")
	customGWIP := ovntest.MustParseIP("fd99::1")

	tests := []struct {
		desc                 string
		ipFamily             int
		gwIface              string
		expIntfName          string
		expGatewayIP         net.IP
		expErr               bool
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
	}{
		{
			desc:         "no default routes returns empty values",
			ipFamily:     netlink.FAMILY_V4,
			expGatewayIP: net.IP{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:         "first default route is used when no gw is specified",
			gwIface:      "",
			ipFamily:     netlink.FAMILY_V4,
			expIntfName:  defaultIf,
			expGatewayIP: defaultGWIP,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{

				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 2,
						Gw:        defaultGWIP,
					},
					{
						LinkIndex: 9,
						Gw:        ovntest.MustParseIP("3.3.3.3"),
					},
				}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
			},
		},
		{
			desc:         "only routes from the provided GW are considered",
			gwIface:      customIf,
			ipFamily:     netlink.FAMILY_V6,
			expIntfName:  customIf,
			expGatewayIP: customGWIP,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIP,
					},
					{
						LinkIndex: 2,
						Gw:        customGWIP,
					},
				}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 2}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Index: 2}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: customIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: customIf}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)
			intfName, gwIP, err := getDefaultGatewayInterfaceByFamily(tc.ipFamily, tc.gwIface)
			if intfName != tc.expIntfName {
				t.Fatalf("TestGetDefaultGatewayInterfaceByFamily(%d): Default gateway interface should be '%v' but got '%v'",
					i, tc.expIntfName, intfName)
			}
			if !reflect.DeepEqual(tc.expGatewayIP, gwIP) {
				t.Fatalf("TestGetDefaultGatewayInterfaceByFamily(%d): Default gateway IP should be '%v' but got '%v'",
					i, tc.expGatewayIP, gwIP)
			}

			t.Log(err)
			if tc.expErr {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestGetDefaultGatewayInterfaceDetails(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	defer util.ResetNetLinkOpMockInst()

	defaultIf := "testInterface"
	defaultGWIPv4 := ovntest.MustParseIP("1.1.1.1")
	defaultGWIPv6 := ovntest.MustParseIP("fd99::1")

	tests := []struct {
		desc                 string
		ipV4Mode             bool
		ipV6Mode             bool
		gwIface              string
		expIntfName          string
		expGatewayIPs        []net.IP
		expErr               bool
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
	}{
		{
			desc:     "no default routes returns empty values",
			ipV4Mode: true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:          "only ipv4 GW set in dual-stack returns valid interface and one gw",
			ipV4Mode:      true,
			ipV6Mode:      true,
			expGatewayIPs: []net.IP{defaultGWIPv4},
			expIntfName:   defaultIf,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIPv4,
					},
				}, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
			},
		},
		{
			desc:          "only ipv6 GW set in dual-stack returns valid interface and one gw",
			ipV4Mode:      true,
			ipV6Mode:      true,
			expGatewayIPs: []net.IP{defaultGWIPv6},
			expIntfName:   defaultIf,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIPv6,
					},
				}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
			},
		},
		{
			desc:     "in dual-stack the function fails if the default GWs are on different interfaces",
			ipV4Mode: true,
			ipV6Mode: true,
			expErr:   true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIPv4,
					},
				}, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 2,
						Gw:        defaultGWIPv6,
					},
				}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "invalidInterface"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "invalidInterface"}}},
			},
		},
		{
			desc:          "in dual-stack the function returns both GW ips",
			ipV4Mode:      true,
			ipV6Mode:      true,
			expGatewayIPs: []net.IP{defaultGWIPv4, defaultGWIPv6},
			expIntfName:   defaultIf,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIPv4,
					},
				}, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{
						LinkIndex: 1,
						Gw:        defaultGWIPv6,
					},
				}, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkByIndex", OnCallMethodArgType: []string{"int"}, RetArgList: []interface{}{mockLink, nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: defaultIf}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)
			intfName, gwIPs, err := getDefaultGatewayInterfaceDetails(tc.gwIface, tc.ipV4Mode, tc.ipV6Mode)
			if intfName != tc.expIntfName {
				t.Fatalf("TestGetDefaultGatewayInterfaceDetails(%d): Default gateway interface should be '%v' but got '%v'",
					i, tc.expIntfName, intfName)
			}
			if !reflect.DeepEqual(tc.expGatewayIPs, gwIPs) {
				t.Fatalf("TestGetDefaultGatewayInterfaceDetails(%d): Default gateway IPs should be '%v' but got '%v'",
					i, tc.expGatewayIPs, gwIPs)
			}

			t.Log(err)
			if tc.expErr {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}
