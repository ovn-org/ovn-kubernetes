package util

import (
	"fmt"
	"net"
	"testing"

	kapi "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
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

func TestLinkByName(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		input                    string
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to look up link",
			input:  "invalidIfaceName",
			errExp: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "sets up the link successfully",
			input:  "testIfaceName",
			errExp: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			res, err := LinkByName(tc.input)
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

func TestLinkSetUp(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		input                    string
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to look up link",
			input:  "invalidIfaceName",
			errExp: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "fails to set the link",
			input:  "testIfaceName",
			errExp: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:   "sets up the link successfully",
			input:  "testIfaceName",
			errExp: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fail to list addresses for link",
			input:  mockLink,
			errExp: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:   "fail to delete addresses on link",
			input:  mockLink,
			errExp: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "AddrList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
							},
						},
						nil,
					},
				},
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:  "Link address flushed successfully",
			input: mockLink,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "AddrList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
							},
						},
						nil,
					},
				},
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:  "IPv6 link-local address is not flushed",
			input: mockLink,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "AddrList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Addr{
							{
								IPNet: ovntest.MustParseIPNet("fe80::1234/64"),
							},
						},
						nil,
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)
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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:             "AddrList call returns error for given link",
			inputLink:        mockLink,
			inputAddrToMatch: ovntest.MustParseIPNet("192.168.1.15/24"),
			errExp:           true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:             "Given address is present on the link",
			inputLink:        mockLink,
			inputAddrToMatch: ovntest.MustParseIPNet("192.168.1.15/24"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "AddrList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
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
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, nil}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)
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

func TestSyncAddresses(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps
	existingIPNet := netlink.Addr{
		IPNet: ovntest.MustParseIPNet("192.168.1.15/24"),
	}
	undesiredExistingIPNet := netlink.Addr{
		IPNet: ovntest.MustParseIPNet("123.123.123.15/24"),
	}
	undesiredExistingIPNet2 := netlink.Addr{
		IPNet: ovntest.MustParseIPNet("123.123.124.15/24"),
	}
	linkLocalAddr := netlink.Addr{
		IPNet: ovntest.MustParseIPNet("fe80::210:5aff:feaa:20a2/64"),
	}

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputNewAddrs            []*net.IPNet
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:                     "specifying multiple address families fails",
			inputLink:                mockLink,
			inputNewAddrs:            []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24"), ovntest.MustParseIPNet("6b35:d6d1:5789:1b33:8ad4:866c:78c1:a085/128")},
			errExp:                   true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{},
			onRetArgsLinkIfaceOpers:  []ovntest.TestifyMockHelper{},
		},
		{
			desc:          "link address list failure causes error",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24")},
			errExp:        true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:          "new non-existent address should be added",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24")},
			errExp:        false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{}, nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:          "address that already exists should not be added",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24")},
			errExp:        false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{existingIPNet}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:          "address should be added while undesired address should be removed",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24")},
			errExp:        false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{undesiredExistingIPNet}, nil}},
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:          "multiple addresses should be added while multiple undesired addresses should be removed",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.15/24"), ovntest.MustParseIPNet("192.168.1.16/24")},
			errExp:        false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{undesiredExistingIPNet, undesiredExistingIPNet2}, nil}},
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrDel", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:          "IPv6 LLA addresses should not be touched",
			inputLink:     mockLink,
			inputNewAddrs: []*net.IPNet{ovntest.MustParseIPNet("6b35:d6d1:5789:1b33:8ad4:866c:78c1:a085/128")},
			errExp:        false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Addr{linkLocalAddr}, nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)
			err := SyncAddresses(tc.inputLink, tc.inputNewAddrs)
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

func TestLinkAddrAdd(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputNewAddr             *net.IPNet
		inputFlags               int
		errExp                   bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:         "setting <nil> address on link errors out",
			inputLink:    mockLink,
			inputNewAddr: nil,
			inputFlags:   0,
			errExp:       true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "test code path where error is returned when attempting to set new address on link",
			inputLink:    mockLink,
			inputNewAddr: ovntest.MustParseIPNet("192.168.1.15/24"),
			inputFlags:   0,
			errExp:       true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "setting new address on link succeeds",
			inputLink:    mockLink,
			inputNewAddr: ovntest.MustParseIPNet("192.168.1.15/24"),
			inputFlags:   2,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)
			err := LinkAddrAdd(tc.inputLink, tc.inputNewAddr, tc.inputFlags, 0, 0)
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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:      "fails to get routes for link",
			inputLink: mockLink,
			errExp:    true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{}, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "subnet input is nil and error returned is nil",
			inputLink:    mockLink,
			inputSubnets: nil,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteList", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
			},
		},
		{
			desc:         "route delete fails",
			inputLink:    mockLink,
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.1.0/24"),
			errExp:       true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RouteList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Route{
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")},
				},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:         "route delete succeeds",
			inputLink:    mockLink,
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.1.0/24"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RouteList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Route{
							{Dst: nil},
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:         "default route delete succeeds",
			inputLink:    mockLink,
			inputSubnets: []*net.IPNet{nil},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RouteList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Route{
							{Dst: getAnyNetwork(false)},
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil},
				},
			},
		},
		{
			desc:         "delete all routes for a link",
			inputLink:    mockLink,
			inputSubnets: nil,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "RouteList",
					OnCallMethodArgType: []string{"*mocks.Link", "int"},
					RetArgList: []interface{}{
						[]netlink.Route{
							{Dst: ovntest.MustParseIPNet("192.168.1.0/24")},
						},
						nil,
					},
				},
				{
					OnCallMethodName: "RouteDel", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)
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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:         "Route add fails",
			inputLink:    mockLink,
			inputGwIP:    ovntest.MustParseIP("192.168.0.1"),
			inputSubnets: ovntest.MustParseIPNets("10.18.20.0/24", "192.168.0.0/24"),
			errExp:       true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "Route add succeeds",
			inputLink:    mockLink,
			inputGwIP:    ovntest.MustParseIP("192.168.0.1"),
			inputSubnets: ovntest.MustParseIPNets("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc: "LinkRoutesAdd() returns NO error when subnets input list is empty",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			err := LinkRoutesAdd(tc.inputLink, tc.inputGwIP, tc.inputSubnets, 0, nil)
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
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		inputLink                netlink.Link
		inputGwIP                net.IP
		inputSubnet              *net.IPNet
		errExp                   bool
		outBoolFlag              bool
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:        "tests code path when RouteListFiltered() returns error",
			inputLink:   mockLink,
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			errExp:      true,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "tests code path when RouteListFiltered() returns empty routes list",
			inputLink:   mockLink,
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "gateway IP input is nil",
			inputLink:   mockLink,
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "tests code path where route GW IP DOES NOT MATCH with input GW IP",
			inputLink:   mockLink,
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{Gw: ovntest.MustParseIP("192.168.1.1")},
				}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "tests code path where route GW IP MATCHES with input GW IP",
			inputLink:   mockLink,
			inputGwIP:   ovntest.MustParseIP("192.168.0.1"),
			inputSubnet: ovntest.MustParseIPNet("192.168.0.0/24"),
			outBoolFlag: true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{Gw: ovntest.MustParseIP("192.168.0.1")},
				}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			route, err := LinkRouteGetByDstAndGw(tc.inputLink, tc.inputGwIP, tc.inputSubnet)
			t.Log(route, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			if tc.outBoolFlag {
				assert.True(t, route != nil)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		// NOTE: since, we dont validate function arguments in the function body, a nil value passed for neighIP and neighMac is sufficient
		{
			desc:      "test code path where adding neighbor returns an error",
			inputLink: mockLink,
			errExp:    true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighAdd", OnCallMethodArgType: []string{"*netlink.Neigh"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:      "test code path where adding neighbor returns success",
			inputLink: mockLink,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighAdd", OnCallMethodArgType: []string{"*netlink.Neigh"}, RetArgList: []interface{}{nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

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
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:        "test path when NeighList() returns error",
			inputLink:   mockLink,
			errExp:      true,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighList", OnCallMethodArgType: []string{"int", "int"}, RetArgList: []interface{}{[]netlink.Neigh{}, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:        "test path when NeighList() returns empty list and no error",
			inputLink:   mockLink,
			outBoolFlag: false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighList", OnCallMethodArgType: []string{"int", "int"}, RetArgList: []interface{}{[]netlink.Neigh{}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "test path where MAC/IP binding is established",
			inputLink:    mockLink,
			inputNeigIP:  ovntest.MustParseIP("192.169.1.12"),
			inputMacAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
			outBoolFlag:  true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighList", OnCallMethodArgType: []string{"int", "int"},
					RetArgList: []interface{}{
						[]netlink.Neigh{
							{IP: ovntest.MustParseIP("192.169.1.12"), HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"), State: netlink.NUD_PERMANENT},
						},
						nil,
					},
				},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:         "test path where MAC/IP bindings DOES NOT exist",
			inputLink:    mockLink,
			inputNeigIP:  ovntest.MustParseIP("192.169.1.15"),
			inputMacAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NeighList", OnCallMethodArgType: []string{"int", "int"},
					RetArgList: []interface{}{
						[]netlink.Neigh{
							{IP: ovntest.MustParseIP("192.169.1.12"), HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01"), State: netlink.NUD_PERMANENT},
						},
						nil,
					},
				},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

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
		labels                   [][]byte
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
	}{
		{
			desc:       "Invalid IP address code input",
			inputIPStr: "blah",
			errExp:     true,
		},
		{
			desc:       "Valid IPv4 address input",
			inputIPStr: "192.168.1.14",
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
			},
		},
		{
			desc:       "Valid IPv6 address input",
			inputIPStr: "fffb::1",
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
			},
		},
		{
			desc:          "Valid IPv4 address input with UDP protocol",
			inputIPStr:    "192.168.1.14",
			inputProtocol: kapi.ProtocolUDP,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
			},
		},
		{
			desc:          "Valid IPv4 address input with SCTP protocol",
			inputIPStr:    "192.168.1.14",
			inputProtocol: kapi.ProtocolSCTP,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
			},
		},
		{
			desc:          "Valid IPv4 address input with TCP protocol",
			inputIPStr:    "192.168.1.14",
			inputProtocol: kapi.ProtocolTCP,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
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
			desc:          "Valid IPv6 address input with valid port input and valid Layer 4 protocol and labels",
			inputIPStr:    "fffb::1",
			inputProtocol: kapi.ProtocolSCTP,
			inputPort:     9999,
			labels:        [][]byte{{3, 4, 61, 141, 207, 170}, {0x2}},
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)

			err := DeleteConntrack(tc.inputIPStr, tc.inputPort, tc.inputProtocol, netlink.ConntrackReplyAnyIP, tc.labels)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)

			err := DeleteConntrack(tc.inputIPStr, tc.inputPort, tc.inputProtocol, netlink.ConntrackOrigDstIP, tc.labels)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}

func TestGetIPv6OnSubnet(t *testing.T) {
	mockNetLinkOps := new(mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below is defined in net_linux.go
	netLinkOps = mockNetLinkOps

	tests := []struct {
		desc                     string
		errExp                   bool
		inputIface               string
		inputIP                  *net.IPNet
		expectedIP               *net.IPNet
		onRetArgsNetLinkLibOpers []ovntest.TestifyMockHelper
		onRetArgsLinkIfaceOpers  []ovntest.TestifyMockHelper
	}{
		{
			desc:       "tests code path when LinkByName() returns error",
			inputIface: "testIfaceName",
			inputIP:    ovntest.MustParseIPNet("2620:52:0:11c::20/128"),
			expectedIP: nil,
			errExp:     true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:       "tests code path when RouteListFiltered() returns error",
			inputIface: "testIfaceName",
			inputIP:    ovntest.MustParseIPNet("2620:52:0:11c::20/128"),
			expectedIP: nil,
			errExp:     true,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:                     "provided address does not have 128 prefix",
			inputIface:               "testIfaceName",
			inputIP:                  ovntest.MustParseIPNet("2620:52:0:11c::20/64"),
			expectedIP:               ovntest.MustParseIPNet("2620:52:0:11c::20/64"),
			errExp:                   false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:                     "provided address is not ipv6",
			inputIface:               "testIfaceName",
			inputIP:                  ovntest.MustParseIPNet("192.169.1.12/32"),
			expectedIP:               ovntest.MustParseIPNet("192.169.1.12/32"),
			errExp:                   false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{},
		},
		{
			desc:       "returns address on broadest subnet found",
			inputIface: "testIfaceName",
			inputIP:    ovntest.MustParseIPNet("2620:52:0:11c::20/128"),
			expectedIP: ovntest.MustParseIPNet("2620:52:0:11c::20/32"),
			errExp:     false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{Dst: ovntest.MustParseIPNet("2620:52:0:11c::/64")},
					{Dst: ovntest.MustParseIPNet("2620:52::/32")},
				}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
		{
			desc:       "returns address on broadest subnet found irrespective of order",
			inputIface: "testIfaceName",
			inputIP:    ovntest.MustParseIPNet("2620:52:0:11c::20/128"),
			expectedIP: ovntest.MustParseIPNet("2620:52:0:11c::20/32"),
			errExp:     false,
			onRetArgsNetLinkLibOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "RouteListFiltered", OnCallMethodArgType: []string{"int", "*netlink.Route", "uint64"}, RetArgList: []interface{}{[]netlink.Route{
					{Dst: ovntest.MustParseIPNet("2620:52::/32")},
					{Dst: ovntest.MustParseIPNet("2620:52:0:11c::/64")},
				}, nil}},
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Index: 1}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.onRetArgsNetLinkLibOpers)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			ip, err := GetIPv6OnSubnet(tc.inputIface, tc.inputIP)
			t.Log(ip, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			if tc.expectedIP != nil {
				assert.Equal(t, ip, tc.expectedIP)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestGetMTUOfInterfaceWithAddress(t *testing.T) {
	var mockNetLinkOps *mocks.NetLinkOps

	existingInterfaceIndex := 3
	nonExistingInterfaceIndex := 4
	existingInterfaceMTU := 1500
	existingInterfaceName := "vtep0"
	exisitngMockLink := new(netlink_mocks.Link)

	tests := []struct {
		desc                    string
		interfaceAddress        net.IP
		wantMTU                 int
		wantError               bool
		netlinkOpsCallbackFunc  func()
		onRetArgsLinkIfaceOpers []ovntest.TestifyMockHelper
	}{
		{
			desc:             "Should return error on AddrList failure",
			interfaceAddress: ovntest.MustParseIP("10.1.0.40"),
			wantMTU:          0,
			wantError:        true,
			netlinkOpsCallbackFunc: func() {
				mockNetLinkOps.On("AddrList", nil, netlink.FAMILY_V4).Return([]netlink.Addr{}, fmt.Errorf("mock error"))
			},
		},
		{
			desc:             "Should return error on no address on the system",
			interfaceAddress: ovntest.MustParseIP("10.1.0.40"),
			wantMTU:          0,
			wantError:        true,
			netlinkOpsCallbackFunc: func() {
				mockNetLinkOps.On("AddrList", nil, netlink.FAMILY_V4).Return(nil, nil)
			},
		},
		{
			desc:             "Should return error if the address is found but index is not found ",
			interfaceAddress: ovntest.MustParseIP("10.1.0.40"),
			wantMTU:          0,
			wantError:        true,
			netlinkOpsCallbackFunc: func() {
				mockNetLinkOps.On("AddrList", nil, netlink.FAMILY_V4).Return([]netlink.Addr{
					{IPNet: ovntest.MustParseIPNet("10.1.0.40/32"), LinkIndex: nonExistingInterfaceIndex},
				}, nil)
				mockNetLinkOps.On("LinkByIndex", nonExistingInterfaceIndex).Return(nil, fmt.Errorf("mock error"))
			},
		},
		{
			desc:             "Should return MTU if the address and link index is found ",
			interfaceAddress: ovntest.MustParseIP("10.1.0.40"),
			wantMTU:          existingInterfaceMTU,
			wantError:        false,
			netlinkOpsCallbackFunc: func() {
				mockNetLinkOps.On("AddrList", nil, netlink.FAMILY_V4).Return([]netlink.Addr{
					{IPNet: ovntest.MustParseIPNet("10.1.0.40/32"), LinkIndex: existingInterfaceIndex},
				}, nil)
				mockNetLinkOps.On("LinkByIndex", existingInterfaceIndex).Return(exisitngMockLink, nil)
			},
			onRetArgsLinkIfaceOpers: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: existingInterfaceName}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{MTU: existingInterfaceMTU}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			mockNetLinkOps = new(mocks.NetLinkOps)
			SetNetLinkOpMockInst(mockNetLinkOps)
			if tc.netlinkOpsCallbackFunc != nil {
				tc.netlinkOpsCallbackFunc()
			}
			ovntest.ProcessMockFnList(&exisitngMockLink.Mock, tc.onRetArgsLinkIfaceOpers)

			_, mtu, err := GetIFNameAndMTUForAddress(tc.interfaceAddress)
			assert.Equal(t, err != nil, tc.wantError)
			assert.Equal(t, mtu, tc.wantMTU)
			mockNetLinkOps.AssertExpectations(t)
			exisitngMockLink.AssertExpectations(t)
		})
	}
}

func TestIsAddressReservedForInternalUse(t *testing.T) {
	tests := []struct {
		desc   string
		input  net.IP
		outExp bool
	}{
		{
			desc:   "non-reserved IPv4 address",
			input:  ovntest.MustParseIP("1.1.1.1"),
			outExp: false,
		},
		{
			desc:   "non-reserved IPv6 address",
			input:  ovntest.MustParseIP("abcd::1"),
			outExp: false,
		},
		{
			desc:   "reserved IPv4 address",
			input:  config.Gateway.MasqueradeIPs.V4HostMasqueradeIP,
			outExp: true,
		},
		{
			desc:   "reserved IPv6 address",
			input:  config.Gateway.MasqueradeIPs.V6HostMasqueradeIP,
			outExp: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := IsAddressReservedForInternalUse(tc.input)
			t.Log(res)
			assert.Equal(t, res, tc.outExp)
		})
	}
}

func getAnyNetwork(v6 bool) *net.IPNet {
	var ipNet *net.IPNet
	if v6 {
		_, ipNet, _ = net.ParseCIDR("::/0")
	} else {
		_, ipNet, _ = net.ParseCIDR("0.0.0.0/0")
	}
	return ipNet
}
