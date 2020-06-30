package util

import (
	"fmt"
	"net"
	"testing"

	kapi "k8s.io/api/core/v1"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
)

func TestGetFamily(t *testing.T) {
	tests := []struct {
		desc   string
		input  net.IP
		outExp int
	}{
		{
			desc:   "valid IPv4 input",
			input:  ovntest.MustParseIP("192.168.12.121"),
			outExp: netlink.FAMILY_V4,
		},
		{
			desc:   "valid IPv6 input",
			input:  ovntest.MustParseIP("fffb::1"),
			outExp: netlink.FAMILY_V6,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := getFamily(tc.input)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func TestLinkSetUp(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		input                    string
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
	}{
		{
			desc:   "fails to look up link",
			input:  "invalidIfaceName",
			errExp: true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"LinkByName", []string{"string"}, []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "fails to set the link",
			input:  "testIfaceName",
			errExp: true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"LinkByName", []string{"string"}, []interface{}{mockLink, nil}},
				{"LinkSetup", []string{"*mocks.Link"}, []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:  "sets up the link successfully",
			input: "testIfaceName",
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"LinkByName", []string{"string"}, []interface{}{mockLink, nil}},
				{"LinkSetup", []string{"*mocks.Link"}, []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := LinkSetUp(tc.input)
			t.Log(res, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.NotNil(t, res)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}

func TestLinkAddrFlush(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		input                    netlink.Link
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:   "fail to list addresses for link",
			input:  mockLink,
			errExp: true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrList", []string{"*mocks.Link", "int"}, []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:   "fail to delete addresses on link",
			input:  mockLink,
			errExp: true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{
					"AddrList",
					[]string{"*mocks.Link", "int"},
					[]interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
							},
						},
						nil,
					},
				},
				{"AddrDel", []string{"*mocks.Link", "*netlink.Addr"}, []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:  "Link address flushed successfully",
			input: mockLink,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{
					"AddrList",
					[]string{"*mocks.Link", "int"},
					[]interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
							},
						},
						nil,
					},
				},
				{"AddrDel", []string{"*mocks.Link", "*netlink.Addr"}, []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			err := LinkAddrFlush(tc.input)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkAddrExist(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputAddrToMatch         *net.IPNet
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:             "AddrList call returns error for given link",
			inputLink:        mockLink,
			inputAddrToMatch: ovntest.MustParseIPNet("192.168.1.15/24"),
			errExp:           true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrList", []string{"*mocks.Link", "int"}, []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:             "Given address is present on the link",
			inputLink:        mockLink,
			inputAddrToMatch: ovntest.MustParseIPNet("192.168.1.15/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{
					"AddrList",
					[]string{"*mocks.Link", "int"},
					[]interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
							},
						},
						nil,
					},
				},
			},
		},
		{
			desc:             "Given address is NOT present on the link",
			inputLink:        mockLink,
			inputAddrToMatch: ovntest.MustParseIPNet("192.168.1.15/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrList", []string{"*mocks.Link", "int"}, []interface{}{nil, nil}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			flag, err := LinkAddrExist(tc.inputLink, tc.inputAddrToMatch)
			t.Log(flag, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkAddrAdd(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputNewAddr             *net.IPNet
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:         "setting <nil> address on link errors out",
			inputLink:    mockLink,
			inputNewAddr: nil,
			errExp:       true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrAdd", []string{"*mocks.Link", "*netlink.Addr"}, []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "test code path where error is returned when attempting to set new address on link",
			inputLink:    mockLink,
			inputNewAddr: ovntest.MustParseIPNet("192.168.1.15/24"),
			errExp:       true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrAdd", []string{"*mocks.Link", "*netlink.Addr"}, []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "setting new address on link succeeds",
			inputLink:    mockLink,
			inputNewAddr: ovntest.MustParseIPNet("192.168.1.15/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"AddrAdd", []string{"*mocks.Link", "*netlink.Addr"}, []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			err := LinkAddrAdd(tc.inputLink, tc.inputNewAddr)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkRoutesDel(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputSubnets             []*net.IPNet
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:      "fails to get routes for link",
			inputLink: mockLink,
			errExp:    true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteList", []string{"*mocks.Link", "int"}, []interface{}{[]netlink.Route{}, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "subnet input is nil and error returned is nil",
			inputLink:    mockLink,
			inputSubnets: nil,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteList", []string{"*mocks.Link", "int"}, []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:         "route delete fails",
			inputLink:    mockLink,
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.1.0/24"),
			errExp:       true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{
					"RouteList",
					[]string{"*mocks.Link", "int"},
					[]interface{}{
						[]netlink.Route{
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					"RouteDel", []string{"*netlink.Route"}, []interface{}{fmt.Errorf("mock error")},
				},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "route delete succeeds",
			inputLink:    mockLink,
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.1.0/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{
					"RouteList",
					[]string{"*mocks.Link", "int"},
					[]interface{}{
						[]netlink.Route{
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					"RouteDel", []string{"*netlink.Route"}, []interface{}{nil},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			err := LinkRoutesDel(tc.inputLink, tc.inputSubnets)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkRoutesAdd(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputGwIP                net.IP
		inputSubnets             []*net.IPNet
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:         "Route add fails",
			inputLink:    mockLink,
			inputGwIP:    ovntest.MustParseIP("192.168.0.1"),
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.0.0/24"),
			errExp:       true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteAdd", []string{"*netlink.Route"}, []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "Route add succeeds",
			inputLink:    mockLink,
			inputGwIP:    ovntest.MustParseIP("192.168.0.1"),
			inputSubnets: ovntest.MustParseIPNets("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteAdd", []string{"*netlink.Route"}, []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc: "LinkRoutesAdd() returns NO error when subnets input list is empty",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			err := LinkRoutesAdd(tc.inputLink, tc.inputGwIP, tc.inputSubnets)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkRouteExists(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputGwIP                net.IP
		inputSubnet              *net.IPNet
		errExp                   bool
		outBoolFlag              bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
	}{
		{
			desc:        "tests code path when RouteListFiltered() returns error",
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			errExp:      true,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteListFiltered", []string{"int", "*netlink.Route", "uint64"}, []interface{}{[]netlink.Route{}, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "tests code path when RouteListFiltered() returns empty routes list",
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteListFiltered", []string{"int", "*netlink.Route", "uint64"}, []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:        "gateway IP input is nil",
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteListFiltered", []string{"int", "*netlink.Route", "uint64"}, []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:        "tests code path where route GW IP DOES NOT MATCH with input GW IP",
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteListFiltered", []string{"int", "*netlink.Route", "uint64"}, []interface{}{[]netlink.Route{
					{Gw: ovntest.MustParseIP("192.168.1.1")},
				}, nil}},
			},
		},
		{
			desc:        "tests code path where route GW IP MATCHES with input GW IP",
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			outBoolFlag: true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"RouteListFiltered", []string{"int", "*netlink.Route", "uint64"}, []interface{}{[]netlink.Route{
					{Gw: ovntest.MustParseIP("192.168.0.1")},
				}, nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			flag, err := LinkRouteExists(tc.inputLink, tc.inputGwIP, tc.inputSubnet)
			t.Log(flag, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			if tc.outBoolFlag {
				assert.True(t, flag)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}

func TestLinkNeighAdd(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputNeigIP              net.IP
		inputMacAddr             net.HardwareAddr
		errExp                   bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		// NOTE: since, we dont validate function arguments in the function body, a nil value passed for neighIP and neighMac is sufficient
		{
			desc:      "test code path where adding neighbor returns an error",
			inputLink: mockLink,
			errExp:    true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighAdd", []string{"*netlink.Neigh"}, []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:      "test code path where adding neighbor returns success",
			inputLink: mockLink,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighAdd", []string{"*netlink.Neigh"}, []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			err := LinkNeighAdd(tc.inputLink, tc.inputNeigIP, tc.inputMacAddr)
			t.Log(err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestLinkNeighExists(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputNeigIP              net.IP
		inputMacAddr             net.HardwareAddr
		errExp                   bool
		outBoolFlag              bool
		onRetArgsNetLinkLibOpers []onCallReturnArgs
		onRetArgsLinkIfaceOpers  []onCallReturnArgs
	}{
		{
			desc:        "test path when NeighList() returns error",
			inputLink:   mockLink,
			errExp:      true,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighList", []string{"int", "int"}, []interface{}{[]netlink.Neigh{}, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "test path when NeighList() returns empty list and no error",
			inputLink:   mockLink,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighList", []string{"int", "int"}, []interface{}{[]netlink.Neigh{}, nil}},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "test path where MAC/IP binding is established",
			inputLink:    mockLink,
			inputNeigIP:  ovntest.MustParseIP("192.169.1.12"),
			inputMacAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
			outBoolFlag:  true,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighList", []string{"int", "int"},
					[]interface{}{
						[]netlink.Neigh{
							{IP: ovntest.MustParseIP("192.169.1.12"), HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"), State: netlink.NUD_PERMANENT},
						},
						nil,
					},
				},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "test path where MAC/IP bindings DOES NOT exist",
			inputLink:    mockLink,
			inputNeigIP:  ovntest.MustParseIP("192.169.1.15"),
			inputMacAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"NeighList", []string{"int", "int"},
					[]interface{}{
						[]netlink.Neigh{
							{IP: ovntest.MustParseIP("192.169.1.12"), HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"), State: netlink.NUD_PERMANENT},
						},
						nil,
					},
				},
			},
			onRetArgsLinkIfaceOpers: []onCallReturnArgs{
				{"Attrs", []string{}, []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.onRetArgsLinkIfaceOpers {
				call := mockLink.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			flag, err := LinkNeighExists(tc.inputLink, tc.inputNeigIP, tc.inputMacAddr)
			t.Log(flag, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			if tc.outBoolFlag {
				assert.True(t, flag)
			} else {
				assert.False(t, flag)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestDeleteConntrack(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	tests := []struct {
		desc                     string
		errExp                   bool
		inputIPStr               string
		inputPort                int32
		inputProtocol            kapi.Protocol
		onRetArgsNetLinkLibOpers []onCallReturnArgs
	}{
		{
			desc:       "Invalid IP address code input",
			inputIPStr: "blah",
			errExp:     true,
		},
		{
			desc:       "Valid IPv4 address input",
			inputIPStr: "192.168.1.14",
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},
		},
		{
			desc:       "Valid IPv6 address input",
			inputIPStr: "fffb::1",
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},
		},
		{
			desc:          "Valid IPv4 address input with UDP protocol",
			inputIPStr:    "192.168.1.14",
			inputProtocol: kapi.ProtocolUDP,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},
		},
		{
			desc:          "Valid IPv4 address input with SCTP protocol",
			inputIPStr:    "192.168.1.14",
			inputProtocol: kapi.ProtocolSCTP,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},
		},
		{
			desc:       "Valid IPv4 address input with valid port input and NO layer 4 protocol input",
			errExp:     true,
			inputIPStr: "192.168.1.14",
			inputPort:  9999,
			/*onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},*/
		},
		{
			desc:          "Valid IPv6 address input with valid port input and valid Layer 4 protocol",
			inputIPStr:    "fffb::1",
			inputProtocol: kapi.ProtocolSCTP,
			inputPort:     9999,
			onRetArgsNetLinkLibOpers: []onCallReturnArgs{
				{"ConntrackDeleteFilter", []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, []interface{}{uint(1), nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.onRetArgsNetLinkLibOpers {
				call := mockNetLinkOps.On(item.onCallMethodName)
				for _, arg := range item.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			err := DeleteConntrack(tc.inputIPStr, tc.inputPort, tc.inputProtocol)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}
