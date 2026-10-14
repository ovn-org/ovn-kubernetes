// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	kexec "k8s.io/utils/exec"
	"sigs.k8s.io/knftables"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	cni_type_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/cni/pkg/types"
	cni_ns_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/plugins/pkg/ns"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestRenameLink(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

	tests := []struct {
		desc                 string
		inpCurrName          string
		inpNewName           string
		errExp               bool
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:        "test code path when LinkByName() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetDown() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetName() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test code path when LinkSetUp() errors out",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			errExp:      true,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:        "test success code path",
			inpCurrName: "testCurrName",
			inpNewName:  "testNewName",
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			err := renameLink(tc.inpCurrName, tc.inpNewName)
			t.Log(err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
		})
	}
}

func TestMoveIfToNetns(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNetNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

	tests := []struct {
		desc                 string
		inpIfaceName         string
		inpNetNs             ns.NetNS
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		netNsOpsMockHelper   []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when LinkByName() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     nil,
			errMatch:     fmt.Errorf("failed to lookup device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:         "test code path when LinkSetNsFd() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			errMatch:     fmt.Errorf("failed to move device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test success path",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNetNS.Mock, tc.netNsOpsMockHelper)

			err := moveIfToNetns(tc.inpIfaceName, tc.inpNetNs)
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNetNS.AssertExpectations(t)
		})
	}
}

func TestSafeMoveIfToNetns(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNetNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

	tests := []struct {
		desc                 string
		inpIfaceName         string
		inpNetNs             ns.NetNS
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		netNsOpsMockHelper   []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when LinkSetNsFd() returns error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			errMatch:     fmt.Errorf("failed to move device"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test code path when LinkSetNsFd() returns 'file exists' error",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// error when moving netdevice to namespace
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("file exists")}},
				// rename the netdevice before moving
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// move netdevice to namespace
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
		{
			desc:         "test success path",
			inpIfaceName: "testIfaceName",
			inpNetNs:     mockNetNS,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			netNsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNetNS.Mock, tc.netNsOpsMockHelper)

			_, err := safeMoveIfToNetns(tc.inpIfaceName, tc.inpNetNs, "containerID")
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNetNS.AssertExpectations(t)
		})
	}
}

func TestSetupNetwork(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

	tests := []struct {
		desc                 string
		inpLink              netlink.Link
		inpPodIfaceInfo      *PodInterfaceInfo
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
	}{
		{
			desc:    "test code path when AddrAdd returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
				},
			},
			errMatch: fmt.Errorf("failed to add IP addr"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
		{
			desc:    "test code path when AddRoute for gateway returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
				},
			},
			errMatch: fmt.Errorf("failed to add gateway route"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test code path when AddRoute for pod returns error",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			errMatch: fmt.Errorf("failed to add pod route"),
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test success path",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}, CallTimes: 3},
			},
		},
		{
			desc:    "test container link already set up",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Flags: net.FlagUp}}, CallTimes: 3},
			},
		},
		{
			desc:    "test skip ip config",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				SkipIPConfig: true,
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)

			err := setupNetwork(tc.inpLink, tc.inpPodIfaceInfo)
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSetupInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockNS := new(cni_ns_mocks.NetNS)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

	/* Need the below to test the Do() function that requires root and needs to be figured out
	testOSNameSpace, err := ns.GetCurrentNS()
	if err != nil {
		t.Log(err)
		t.Fatal("failed to get NameSpace for test")
	}*/

	tests := []struct {
		desc                 string
		inpNetNS             ns.NetNS
		inpContID            string
		inpIfaceName         string
		inpPodIfaceInfo      *PodInterfaceInfo
		errExp               bool
		errMatch             error
		nsMockHelper         []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:         "test code path when Do() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
			},
			errExp: true,
			nsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		/* TODO: Running the below requires root, need to figure this out
		// `sudo -E /usr/local/go/bin/go test -v -run TestSetupInterface` would be the command, but mocking SetupVeth() mock is a challenge
		{
			desc:         "test code path when SetupVeth() returns error",
			inpNetNS:     testOSNameSpace,
			inpContID:    "test",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
			},
			errExp: true,
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{"SetupVeth",[]string{"string", "string", "int", "*ns.NetNS"}, []interface{}{nil, nil, fmt.Errorf("mock error")}},
			},
		},*/
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)

			hostIface, contIface, err := setupInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo)
			t.Log(hostIface, contIface, err)
			if tc.errExp {
				require.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNS.AssertExpectations(t)
		})
	}
}

func TestSetupSriovInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)
	mockNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	origSriovnetOps := util.GetSriovnetOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	// set `sriovnetOps` in util/sriovnet_linux.go to a mock instance for unit tests execution
	util.SetSriovnetOpsInst(mockSriovnetOps)
	t.Cleanup(func() {
		util.SetNetLinkOpMockInst(origNetLinkOps)
		util.SetSriovnetOpsInst(origSriovnetOps)
	})

	res, err := sriovnet.GetUplinkRepresentor("0000:01:00.0")
	t.Log(res, err)
	/* Need the below to test the Do() function that requires root and needs to be figured out
	testOSNameSpace, err := ns.GetCurrentNS()
	if err != nil {
		t.Log(err)
		t.Fatal("failed to get NameSpace for test")
	}*/

	netNsDoForward := &cni_ns_mocks.NetNS{}
	netNsDoForward.On("Fd", mock.Anything).Return(uintptr(0))
	var netNsDoError error
	netNsDoForward.On("Do", mock.AnythingOfType("func(ns.NetNS) error")).Run(func(args mock.Arguments) {
		do := args.Get(0).(func(ns.NetNS) error)
		netNsDoError = do(nil)
	}).Return(nil)

	tests := []struct {
		desc                 string
		inpNetNS             ns.NetNS
		inpContID            string
		inpIfaceName         string
		inpPodIfaceInfo      *PodInterfaceInfo
		inpPCIAddrs          string
		errExp               bool
		errMatch             error
		nsMockHelper         []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		sriovOpsMockHelper   []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
		onRetArgsKexecIface  []ovntest.TestifyMockHelper
		onRetArgsCmdList     []ovntest.TestifyMockHelper
		runnerInstance       kexec.Interface
	}{
		{
			desc:         "test code path when LinkSetAlias() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:         "test code path when moveIfToNetns() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:         "test code path when Do() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			runnerInstance: mockKexecIface,
		},
		{
			desc:         "test code path when GetUplinkRepresentor() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when GetVfIndexByPciAddress() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{-1, fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when GetVfRepresentor() returns error",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs:    "0000:03:00.1",
			errExp:         true,
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when retrieving LinkByName() for host interface errors out",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			errExp:      true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for SetVFHardwreAddress()
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetVfHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "int", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				// The below mock call is for the LinkByName() of the host representor
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test success code path for non-DPU host mode",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for SetVFHardwreAddress()
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetVfHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "int", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				// The below mock call is for the LinkByName() of the host representor
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is to retrieve the MAC address of the host representor
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "VFRepresentor", HardwareAddr: ovntest.MustParseMAC("0A:58:FD:98:00:01")}}},
			},
		},
		{
			desc:         "test code path when working in DPUHost mode",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             false,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			runnerInstance:     mockKexecIface,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when container LinkByName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetDown() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetUp() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container second LinkByName() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetHardwareAddr() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container LinkSetMTU() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container second LinkSetUp() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{},
			nsMockHelper:   []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork AddrAdd() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork Gateways AddRoute() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}, CallTimes: 3},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
		{
			desc:         "test code path when container setupNetwork Routes AddRoute() returns error",
			inpNetNS:     netNsDoForward,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []util.PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
				MTU:           1500,
				IsDPUHostMode: true,
				NetdevName:    "en01",
			},
			inpPCIAddrs:        "0000:03:00.1",
			errExp:             true,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetAlias", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetHardwareAddr", OnCallMethodArgType: []string{"*mocks.Link", "net.HardwareAddr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddrAdd", OnCallMethodArgType: []string{"*mocks.Link", "*netlink.Addr"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "RouteAdd", OnCallMethodArgType: []string{"*netlink.Route"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}, CallTimes: 3},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			netNsDoError = nil
			hostIface, contIface, err := setupSriovInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo, tc.inpPCIAddrs, false)
			t.Log(hostIface, contIface, err)
			if err == nil {
				err = netNsDoError
			}
			if tc.errExp {
				require.Error(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNS.AssertExpectations(t)
			mockSriovnetOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestPodRequest_deletePodConntrack(t *testing.T) {
	mockTypeResult := new(cni_type_mocks.Result)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	origNetLinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })
	tests := []struct {
		desc                 string
		inpPodRequest        PodRequest
		inpPrevResult        *current.Result
		resultMockHelper     []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc: "test code path when CNIConf.PrevResult == nil",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: nil,
					},
				},
			},
		},
		{
			desc: "test code path NewResultFromResult returns error",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			resultMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Version", OnCallMethodArgType: []string{}, RetArgList: []interface{}{"0.0.0"}},
			},
		},
		{
			desc: "test code path when ip.Interface != nil and path when Sandbox is empty value",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.1.0",
				Interfaces: []*current.Interface{{Name: "eth0"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
		},
		{
			desc: "test code path when DeleteConntrack returns error",
			inpPodRequest: PodRequest{
				CNIConf: &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.1.0",
				Interfaces: []*current.Interface{{Name: "eth0", Sandbox: "blah"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilters", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), fmt.Errorf("mock error")}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockTypeResult.Mock, tc.resultMockHelper)
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)

			if tc.inpPrevResult != nil {
				res, err := json.Marshal(tc.inpPrevResult)
				if err != nil {
					t.Log(err)
					t.Fatal("json marshal error, test input invalid for inpPrevResult")
				} else {
					tc.inpPodRequest.CNIConf.PrevResult, err = current.NewResult(res)
					if err != nil {
						t.Fatal("NewResult failed", err)
					}
				}
			}
			tc.inpPodRequest.deletePodConntrack(nil, nil)
			mockTypeResult.AssertExpectations(t)
		})
	}
}

func TestPodRequest_migrationPreservedIPs(t *testing.T) {
	const (
		ns      = "kv-live-migration-84"
		vmName  = "vm1"
		srcName = "virt-launcher-vm1-source"
		tgtName = "virt-launcher-vm1-target"
		udnNAD  = ns + "/net1"

		// These primary UDN pods have different default-network
		// infrastructure IPs. Default-network VMs can instead share IPs.
		srcDefaultIPv4 = "10.244.1.4"
		tgtDefaultIPv4 = "10.244.2.20"

		// The (dual-stack) UDN IPs are common to source and target pods:
		// they are preserved across live migration via the IPAMClaim.
		commonUDNIPv4 = "10.100.200.5"
		commonUDNIPv6 = "fd10:0:2b:f000::5"
	)

	var (
		migrationTargetReadyAnnotation = map[string]string{
			kubevirtv1.MigrationTargetReadyTimestamp: "2026-06-29T06:14:48Z",
		}
	)

	// podNetworksAnnotation renders the OVN pod-networks annotation wire
	// format for the given NAD key -> IPs (CIDR) mapping.
	podNetworksAnnotation := func(networks map[string][]string) string {
		podNetworks := map[string]map[string][]string{}
		for nad, ips := range networks {
			podNetworks[nad] = map[string][]string{"ip_addresses": ips}
		}
		raw, err := json.Marshal(podNetworks)
		require.NoError(t, err)
		return string(raw)
	}

	srcNetworks := map[string][]string{
		"default": {srcDefaultIPv4 + "/24"},
		udnNAD:    {commonUDNIPv4 + "/24", commonUDNIPv6 + "/64"},
	}

	tgtNetworks := map[string][]string{
		"default": {tgtDefaultIPv4 + "/24"},
		udnNAD:    {commonUDNIPv4 + "/24", commonUDNIPv6 + "/64"},
	}

	tgtNetworksIPv6Only := map[string][]string{
		"default": {tgtDefaultIPv4 + "/24"},
		udnNAD:    {commonUDNIPv6 + "/64"},
	}

	// virtPod builds a virt-launcher pod for vm1 with the given annotations,
	// pod phase (defaulting to Running) and OVN pod-networks annotation (NAD key -> IPs in CIDR
	// format). The target pod gets a later creation timestamp than the
	// source, since DiscoverLiveMigrationStatus relies on creation time
	// ordering to tell them apart.
	virtPod := func(name string, anns map[string]string, phase corev1.PodPhase, networks map[string][]string) *corev1.Pod {
		if anns == nil {
			anns = map[string]string{}
		}
		if phase == "" {
			phase = corev1.PodRunning
		}
		anns[kubevirtv1.DomainAnnotation] = vmName
		anns[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation] = ""
		if networks != nil {
			anns[ovntypes.OvnPodAnnotationName] = podNetworksAnnotation(networks)
		}
		creation := metav1.NewTime(time.Date(2026, 6, 29, 6, 0, 0, 0, time.UTC))
		if name == tgtName {
			creation = metav1.NewTime(creation.Add(time.Minute))
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         ns,
				UID:               ktypes.UID(name),
				CreationTimestamp: creation,
				Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: vmName},
				Annotations:       anns,
			},
		}
		pod.Status.Phase = phase
		return pod
	}

	nonVMPod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: ktypes.UID(name)}}
	}

	newLister := func(pods ...*corev1.Pod) corev1listers.PodLister {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		for _, p := range pods {
			require.NoError(t, indexer.Add(p))
		}
		return corev1listers.NewPodLister(indexer)
	}

	tests := []struct {
		desc     string
		podName  string
		lister   corev1listers.PodLister
		expected sets.Set[string]
	}{
		{
			desc:     "nil lister (unprivileged shim) -> nothing preserved, flush all",
			lister:   nil,
			expected: sets.New[string](),
		},
		{
			desc:     "pod not in lister -> nothing preserved, flush all",
			lister:   newLister(virtPod(tgtName, migrationTargetReadyAnnotation, "", tgtNetworks)),
			expected: sets.New[string](),
		},
		{
			desc:     "single VM pod, no migration -> nothing preserved, flush all",
			lister:   newLister(virtPod(srcName, nil, "", srcNetworks)),
			expected: sets.New[string](),
		},
		{
			desc:     "non-VM pod -> nothing preserved, flush all",
			lister:   newLister(nonVMPod(srcName)),
			expected: sets.New[string](),
		},
		{
			desc:   "migration in progress -> all target addresses protected",
			lister: newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, nil, "", tgtNetworks)),
			// The helper returns every address owned by the surviving pod.
			// deletePodConntrack compares these with the departing pod
			// addresses, so its different infrastructure IP is still flushed.
			expected: sets.New(tgtDefaultIPv4, commonUDNIPv4, commonUDNIPv6),
		},
		{
			desc:     "migration target ready -> shared UDN IPs preserved (dual-stack)",
			lister:   newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, migrationTargetReadyAnnotation, "", tgtNetworks)),
			expected: sets.New(tgtDefaultIPv4, commonUDNIPv4, commonUDNIPv6),
		},
		{
			desc:     "migration in progress, IPv6-only UDN -> IPv6 preserved",
			lister:   newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, nil, "", tgtNetworksIPv6Only)),
			expected: sets.New(tgtDefaultIPv4, commonUDNIPv6),
		},
		{
			desc:     "migration in progress, target without OVN annotation -> nothing preserved, flush all",
			lister:   newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, nil, "", nil)),
			expected: sets.New[string](),
		},
		{
			desc:     "migration failed (target completed, source living) -> nothing preserved, flush all",
			lister:   newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, nil, corev1.PodSucceeded, tgtNetworks)),
			expected: sets.New[string](),
		},
		{
			desc:    "deleting the failed migration target -> shared UDN IPs preserved",
			podName: tgtName,
			lister:  newLister(virtPod(srcName, nil, "", srcNetworks), virtPod(tgtName, nil, corev1.PodSucceeded, tgtNetworks)),
			// The source still owns these addresses after target failure.
			expected: sets.New(srcDefaultIPv4, commonUDNIPv4, commonUDNIPv6),
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			podName := tc.podName
			if podName == "" {
				podName = srcName
			}
			var pod *corev1.Pod
			if tc.lister != nil {
				pod, _ = tc.lister.Pods(ns).Get(podName)
			}
			pr := &PodRequest{
				PodNamespace: ns,
				PodName:      podName,
				nadKey:       ovntypes.DefaultNetworkName,
			}
			require.Equal(t, tc.expected, pr.migrationPreservedIPs(tc.lister, pod))
		})
	}
}

// TestPodRequest_deletePodConntrackLiveMigration asserts that deletePodConntrack
// suppresses the conntrack flush exactly for the IPs preserved on the live
// migration target pod, while still flushing the released ones.
func TestPodRequest_deletePodConntrackLiveMigration(t *testing.T) {
	const (
		ns      = "kv-live-migration-84"
		vmName  = "vm1"
		srcName = "virt-launcher-vm1-source"
		tgtName = "virt-launcher-vm1-target"
		udnNAD  = ns + "/net1"
	)
	// The target pod gets a later creation timestamp than the source, since
	// DiscoverLiveMigrationStatus relies on creation time ordering to tell
	// them apart.
	virtPod := func(name string, networks string) *corev1.Pod {
		creation := metav1.NewTime(time.Date(2026, 6, 29, 6, 0, 0, 0, time.UTC))
		if name == tgtName {
			creation = metav1.NewTime(creation.Add(time.Minute))
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         ns,
				UID:               ktypes.UID(name),
				CreationTimestamp: creation,
				Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: vmName},
				Annotations: map[string]string{
					kubevirtv1.DomainAnnotation:                             vmName,
					kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					ovntypes.OvnPodAnnotationName:                           networks,
				},
			},
		}
	}
	newLister := func(pods ...*corev1.Pod) corev1listers.PodLister {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		for _, p := range pods {
			require.NoError(t, indexer.Add(p))
		}
		return corev1listers.NewPodLister(indexer)
	}
	// The pod being torn down owns the released default-network IPv4 address
	// plus the dual-stack UDN addresses preserved on the migration target.
	newPodRequest := func(t *testing.T) *PodRequest {
		t.Helper()
		prevResult := &current.Result{
			CNIVersion: "1.1.0",
			Interfaces: []*current.Interface{{Name: "eth0", Sandbox: "blah"}, {Name: "ovn-udn1", Sandbox: "blah"}},
			IPs: []*current.IPConfig{
				{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("10.244.1.4/24")},
				{Interface: &[]int{1}[0], Address: *ovntest.MustParseIPNet("10.100.200.5/24")},
				{Interface: &[]int{1}[0], Address: *ovntest.MustParseIPNet("fd10:0:2b:f000::5/64")},
			},
		}
		raw, err := json.Marshal(prevResult)
		require.NoError(t, err)
		parsedResult, err := current.NewResult(raw)
		require.NoError(t, err)
		return &PodRequest{
			PodNamespace: ns,
			PodName:      srcName,
			nadKey:       ovntypes.DefaultNetworkName,
			CNIConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					PrevResult: parsedResult,
				},
			},
		}
	}
	srcNetworks := `{"default":{"ip_addresses":["10.244.1.4/24"]},"` + udnNAD + `":{"ip_addresses":["10.100.200.5/24","fd10:0:2b:f000::5/64"]}}`
	tgtNetworks := `{"default":{"ip_addresses":["10.244.2.20/24"]},"` + udnNAD + `":{"ip_addresses":["10.100.200.5/24","fd10:0:2b:f000::5/64"]}}`

	t.Run("default network migration preserves dual-stack conntrack across nodes", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			deleteTarget bool
			sourcePhase  corev1.PodPhase
			targetPhase  corev1.PodPhase
			flush        bool
		}{
			{name: "source cleanup during migration"},
			{name: "completed source cleanup", sourcePhase: corev1.PodSucceeded},
			{name: "failed target cleanup", deleteTarget: true, targetPhase: corev1.PodFailed},
			{name: "VM deletion flushes released addresses", sourcePhase: corev1.PodSucceeded, targetPhase: corev1.PodFailed, flush: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				const networks = `{"default":{"ip_addresses":["10.244.1.4/24","fd00:10:244:1::4/64"]}}`
				source := virtPod(srcName, networks)
				target := virtPod(tgtName, networks)
				source.Spec.NodeName = "source-node"
				target.Spec.NodeName = "target-node"
				source.Status.Phase = tc.sourcePhase
				target.Status.Phase = tc.targetPhase
				target.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = time.Now().Format(time.RFC3339Nano)
				deletedPod := source
				if tc.deleteTarget {
					deletedPod = target
				}

				pr := newPodRequest(t)
				pr.PodName = deletedPod.Name
				pr.CNIConf.PrevResult = &current.Result{
					CNIVersion: "1.1.0",
					Interfaces: []*current.Interface{{Name: "eth0", Sandbox: "sandbox"}},
					IPs: []*current.IPConfig{
						{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("10.244.1.4/24")},
						{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("fd00:10:244:1::4/64")},
					},
				}
				mockNetLinkOps := new(util_mocks.NetLinkOps)
				util.SetNetLinkOpMockInst(mockNetLinkOps)
				t.Cleanup(util.ResetNetLinkOpMockInst)
				expectedFlushes := 0
				if tc.flush {
					expectedFlushes = 2
					mockNetLinkOps.On("ConntrackDeleteFilters", netlink.ConntrackTableType(netlink.ConntrackTable), netlink.InetFamily(netlink.FAMILY_V4), mock.Anything).
						Return(uint(1), nil).Once()
					mockNetLinkOps.On("ConntrackDeleteFilters", netlink.ConntrackTableType(netlink.ConntrackTable), netlink.InetFamily(netlink.FAMILY_V6), mock.Anything).
						Return(uint(1), nil).Once()
				}

				pr.deletePodConntrack(newLister(source, target), deletedPod)

				mockNetLinkOps.AssertExpectations(t)
				mockNetLinkOps.AssertNumberOfCalls(t, "ConntrackDeleteFilters", expectedFlushes)
			})
		}
	})

	for _, tc := range []struct {
		name                 string
		withBridgeAnnotation bool
	}{
		{name: "with legacy bridge migration annotation", withBridgeAnnotation: true},
		{name: "without legacy bridge migration annotation (primary UDN l2bridge)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := virtPod(srcName, srcNetworks)
			target := virtPod(tgtName, tgtNetworks)
			if !tc.withBridgeAnnotation {
				delete(source.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
				delete(target.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
			}

			t.Run("live migration in progress: flushes only the released IP, keeps the preserved dual-stack UDN IPs", func(t *testing.T) {
				mockNetLinkOps := new(util_mocks.NetLinkOps)
				util.SetNetLinkOpMockInst(mockNetLinkOps)
				t.Cleanup(util.ResetNetLinkOpMockInst)
				// Only the released default-network IPv4 address is flushed; no
				// IPv6 flush at all since the only IPv6 address is preserved.
				mockNetLinkOps.On("ConntrackDeleteFilters", netlink.ConntrackTableType(netlink.ConntrackTable), netlink.InetFamily(netlink.FAMILY_V4), mock.Anything).
					Return(uint(1), nil).Once()

				lister := newLister(source, target)
				newPodRequest(t).deletePodConntrack(lister, source)

				mockNetLinkOps.AssertExpectations(t)
				mockNetLinkOps.AssertNumberOfCalls(t, "ConntrackDeleteFilters", 1)
			})

			t.Run("no other living VM pod: flushes all the IPs", func(t *testing.T) {
				mockNetLinkOps := new(util_mocks.NetLinkOps)
				util.SetNetLinkOpMockInst(mockNetLinkOps)
				t.Cleanup(util.ResetNetLinkOpMockInst)
				mockNetLinkOps.On("ConntrackDeleteFilters", netlink.ConntrackTableType(netlink.ConntrackTable), netlink.InetFamily(netlink.FAMILY_V4), mock.Anything).
					Return(uint(1), nil).Twice()
				mockNetLinkOps.On("ConntrackDeleteFilters", netlink.ConntrackTableType(netlink.ConntrackTable), netlink.InetFamily(netlink.FAMILY_V6), mock.Anything).
					Return(uint(1), nil).Once()

				lister := newLister(source)
				newPodRequest(t).deletePodConntrack(lister, source)

				mockNetLinkOps.AssertExpectations(t)
				mockNetLinkOps.AssertNumberOfCalls(t, "ConntrackDeleteFilters", 3)
			})
		})
	}
}

func TestConfigureOVS(t *testing.T) {
	mockLink := new(netlink_mocks.Link)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)

	origNetLinkOps := util.GetNetLinkOps()
	origSriovnetOps := util.GetSriovnetOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	util.SetSriovnetOpsInst(mockSriovnetOps)
	t.Cleanup(func() {
		util.SetNetLinkOpMockInst(origNetLinkOps)
		util.SetSriovnetOpsInst(origSriovnetOps)
	})

	vfPciAddress := "0000:c5:03.1"
	fakeIP := "192.168.1.1/24"
	ip, ipnet, _ := net.ParseCIDR(fakeIP)
	ipnet.IP = ip
	sandboxID := "deadbeef"
	ovnPfEncapIpMapping := "enp1s0f0:10.0.0.1,enp193s0f0:10.0.0.193,enp197s0f0:10.0.0.197"

	tests := []struct {
		desc                  string
		podNs                 string
		podName               string
		vfRep                 string
		ifInfo                *PodInterfaceInfo
		ovnPfEncapIpMapping   string
		errMatch              error
		pfEncapIp             string
		execMock              *ovntest.FakeExec
		sriovnetOpsMockHelper []ovntest.TestifyMockHelper
		netLinkOpsMockHelper  []ovntest.TestifyMockHelper
	}{
		{
			desc:    "VF representor has matching external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "VF representor has no matching external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp999s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp999s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp999s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "empty external_ids:ovn-pf-encap-ip-mapping",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping:   "", // no external_ids:ovn-pf-encap-ip-mapping config
			errMatch:              nil,
			pfEncapIp:             "", // ovs port added without encap-ip
			execMock:              ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "ignore get SR-IOV uplink representor failure",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            nil,
			pfEncapIp:           "", // ovs port added without encap-ip
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "GetUplinkRepresentor",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{"", fmt.Errorf("failed to lookup")},
				},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:    "LinkByName fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to find interface"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
		},
		{
			desc:    "LinkSetMTU fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to set MTU on"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:    "LinkSetUp fails for VF representor after add-port",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   "enp1s0f0_1",
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				IsDPUHostMode: false,
				NetName:       ovntypes.DefaultNetworkName,
				NetdevName:    "enp1s0f0v1",
				PodUID:        "xyz",
			},
			ovnPfEncapIpMapping: ovnPfEncapIpMapping,
			errMatch:            fmt.Errorf("failed to set link UP on"),
			pfEncapIp:           "10.0.0.1",
			execMock:            ovntest.NewFakeExec(),
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"enp1s0f0", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovnetOpsMockHelper)

			err := util.SetExec(tc.execMock)
			require.NoError(t, err)
			err = SetExec(tc.execMock)
			require.NoError(t, err)

			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id=ns-foo_pod-bar"),
			})

			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSFindCmd("30", "Interface", "external_ids", "name="+tc.vfRep),
				Output: "",
			})

			// getPfEncapIP()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: tc.ovnPfEncapIpMapping,
			})

			// ovs-vsctl add port to br-int
			ovsAddPortCmd := fmt.Sprintf(
				"ovs-vsctl --timeout=30 --may-exist "+
					"add-port br-int %s other_config:transient=true "+
					"-- set interface %s external_ids:attached_mac=%s "+
					"external_ids:iface-id=%s external_ids:iface-id-ver=%s "+
					"external_ids:sandbox=%s ",
				tc.vfRep, tc.vfRep, "", genIfaceID(tc.podNs, tc.podName), tc.ifInfo.PodUID, sandboxID)
			if tc.pfEncapIp != "" {
				ovsAddPortCmd += fmt.Sprintf("external_ids:encap-ip=%s ", tc.pfEncapIp)
			}
			ovsAddPortCmd += fmt.Sprintf("external_ids:ip_addresses=%s external_ids:vf-netdev-name=%s "+
				"-- --if-exists remove interface %s external_ids %s -- --if-exists remove interface %s external_ids %s",
				fakeIP, tc.ifInfo.NetdevName, tc.vfRep, ovntypes.NetworkExternalID, tc.vfRep, ovntypes.NADExternalID)
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: ovsAddPortCmd,
				Err: nil,
			})

			// clearPodBandwidth()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "interface", "name",
					fmt.Sprintf("external-ids:sandbox=%s", sandboxID)),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "qos", "_uuid",
					fmt.Sprintf("external-ids:sandbox=%s", sandboxID)),
			})

			// waitForPodInterface()
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Interface", tc.vfRep, "external-ids", "iface-id") + " " + "external-ids:ovn-installed",
				Output: genIfaceID(tc.podNs, tc.podName) + "\n" + "true",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Interface", tc.vfRep, "external-ids", "iface-id"),
				Output: genIfaceID(tc.podNs, tc.podName),
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=8,dl_src="),
				Output: "non-empty-output",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=0,in_port=1"),
				Output: "non-empty-output",
			})
			tc.execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOfctlDumpFlowsCmd("table=48,ip,ip_dst=" + strings.Split(fakeIP, "/")[0]),
				Output: "non-empty-output",
			})

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			var pod corev1.Pod
			pod.UID = "xyz"
			podNamespaceLister := v1mocks.PodNamespaceLister{}
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
			var podLister v1mocks.PodLister
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
			fakeClient := fake.NewSimpleClientset(&corev1.PodList{Items: []corev1.Pod{pod}})
			clientset := NewClientSet(fakeClient, &podLister)
			err = ConfigureOVS(ctx, nil, tc.podNs, tc.podName, "", tc.vfRep,
				tc.ifInfo, sandboxID, vfPciAddress, false, clientset)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}

			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestConfigureOVS_getPfEncapIpWithError(t *testing.T) {
	mockLink := new(netlink_mocks.Link)
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)

	origNetLinkOps := util.GetNetLinkOps()
	origSriovnetOps := util.GetSriovnetOps()
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	util.SetSriovnetOpsInst(mockSriovnetOps)
	t.Cleanup(func() {
		util.SetNetLinkOpMockInst(origNetLinkOps)
		util.SetSriovnetOpsInst(origSriovnetOps)
	})

	vfPciAddress := "0000:c5:03.1"
	fakeIP := "192.168.1.1/24"
	ip, ipnet, _ := net.ParseCIDR(fakeIP)
	ipnet.IP = ip
	sandboxID := "deadbeef"
	vfRep := "enp1s0f0_1"

	tests := []struct {
		desc                  string
		podNs                 string
		podName               string
		vfRep                 string
		ifInfo                *PodInterfaceInfo
		ovnPfEncapIpMapping   string
		errMatch              error
		pfEncapIp             string
		execMock              *ovntest.FakeExec
		execMockCommands      []*ovntest.ExpectedCmd
		sriovnetOpsMockHelper []ovntest.TestifyMockHelper
		netLinkOpsMockHelper  []ovntest.TestifyMockHelper
	}{
		{
			desc:    "ovs get external_ids:ovn-pf-encap-ip-mapping failed",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   vfRep,
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				NetName:    ovntypes.DefaultNetworkName,
				NetdevName: "enp1s0f0v1",
				PodUID:     "xyz",
			},
			errMatch:  fmt.Errorf("failed to get ovn-pf-encap-ip-mapping"),
			pfEncapIp: "",
			execMockCommands: []*ovntest.ExpectedCmd{
				{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id=ns-foo_pod-bar"),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "external_ids", "name="+vfRep),
				},
				{
					Cmd: genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Err: fmt.Errorf("ovs-vsctl: any error ..."),
				},
			},
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper:  []ovntest.TestifyMockHelper{},
		},
		{
			desc:    "bad external_ids:ovn-pf-encap-ip-mapping config",
			podNs:   "ns-foo",
			podName: "pod-bar",
			vfRep:   vfRep,
			ifInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{
					IPs: []*net.IPNet{ipnet},
				},
				NetName:    ovntypes.DefaultNetworkName,
				NetdevName: "enp1s0f0v1",
				PodUID:     "xyz",
			},
			errMatch:  fmt.Errorf("bad ovn-pf-encap-ip-mapping config"),
			pfEncapIp: "",
			execMockCommands: []*ovntest.ExpectedCmd{
				{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id=ns-foo_pod-bar"),
				},
				{
					Cmd: genOVSFindCmd("30", "Interface", "external_ids", "name="+vfRep),
				},
				{
					Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Output: "10.0.0.1,10.0.0.193,10.0.0.197",
				},
			},
			sriovnetOpsMockHelper: []ovntest.TestifyMockHelper{},
			netLinkOpsMockHelper:  []ovntest.TestifyMockHelper{},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			execMock := ovntest.NewFakeExec()
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovnetOpsMockHelper)

			err := util.SetExec(execMock)
			require.NoError(t, err)
			err = SetExec(execMock)
			require.NoError(t, err)

			execMock.AddFakeCmds(tc.execMockCommands)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			var pod corev1.Pod
			podNamespaceLister := v1mocks.PodNamespaceLister{}
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)

			var podLister v1mocks.PodLister
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
			err = ConfigureOVS(ctx, nil, tc.podNs, tc.podName, "", tc.vfRep,
				tc.ifInfo, sandboxID, vfPciAddress, false, nil)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}

			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
		})
	}
}

func TestSetupIngressFilter(t *testing.T) {
	nft := knftables.NewFake(knftables.NetDevFamily, "ingress_filter")

	gwLLA := "fe80::1"
	err := setupIngressFilterWithTableAndLifetimeMatcher(nft, "test-iface-1", gwLLA, "mark")
	require.NoError(t, err)
	output := nft.Dump()

	expectedDump := `add table netdev ingress_filter
add chain netdev ingress_filter input { type filter hook ingress device "test-iface-1" priority 0 ; }
add rule netdev ingress_filter input icmpv6 type nd-router-advert ip6 saddr != fe80::1 mark != 0 drop`

	assert.Equal(t, expectedDump, strings.TrimRight(output, "\n"), "The nftables dump output does not match the expected output")
}

// TODO(leih): Below functions are copied from pkg/node/base_node_network_controller_dpu_test.go.
// Move them to a common place to elimate duplications.
func genOVSFindCmd(timeout, table, column, condition string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=%s --no-heading --format=csv --data=bare --columns=%s find %s %s",
		timeout, column, table, condition)
}

func genOVSGetCmd(table, record, column, key string, timeout ...int) string {
	_timeout := 30
	if len(timeout) > 0 {
		_timeout = timeout[0]
	}
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=%d --if-exists get %s %s %s", _timeout, table, record, column)
}

func genIfaceID(podNamespace, podName string) string {
	return fmt.Sprintf("%s_%s", podNamespace, podName)
}

func genOfctlDumpFlowsCmd(queryStr string) string {
	return fmt.Sprintf("ovs-ofctl --timeout=10 --no-stats --strict dump-flows br-int %s", queryStr)
}

func TestReturnSimulatedNetdevsToHost(t *testing.T) {
	linkWith := func(name, alias string) *netlink_mocks.Link {
		link := new(netlink_mocks.Link)
		link.On("Attrs").Return(&netlink.LinkAttrs{Name: name, Alias: alias, Index: 4})
		return link
	}

	tests := []struct {
		desc string
		// links present in the pod netns, keyed by name -> alias
		links        [][2]string
		linkListErr  error
		netlinkFails string // netlink op that returns an error, if any
		expRenames   map[string]string
		expMoved     []string
		errExp       bool
	}{
		{
			desc:  "no simulated netdevs is a no-op",
			links: [][2]string{{"lo", ""}, {"eth0", ""}},
		},
		{
			desc:       "primary UDN netdev restored from the link alias",
			links:      [][2]string{{"lo", ""}, {primaryUDNIfName, "eth0-5"}},
			expRenames: map[string]string{primaryUDNIfName: "eth0-5"},
			expMoved:   []string{primaryUDNIfName},
		},
		{
			desc:     "netdev left under its simulated name is moved without a rename",
			links:    [][2]string{{"eth0-5", ""}},
			expMoved: []string{"eth0-5"},
		},
		{
			desc:  "primary UDN netdev without a recoverable name is left in place",
			links: [][2]string{{primaryUDNIfName, ""}},
		},
		{
			desc:  "foreign alias is not a rename target",
			links: [][2]string{{primaryUDNIfName, "some ifAlias set by an admin"}},
		},
		{
			desc:        "link list failure is an error",
			linkListErr: fmt.Errorf("mock error"),
			errExp:      true,
		},
		{
			desc:         "rename failure is an error",
			links:        [][2]string{{primaryUDNIfName, "eth0-5"}},
			netlinkFails: "LinkSetName",
			expRenames:   map[string]string{primaryUDNIfName: "eth0-5"},
			errExp:       true,
		},
		{
			desc:         "move failure is an error",
			links:        [][2]string{{primaryUDNIfName, "eth0-5"}},
			netlinkFails: "LinkSetNsFd",
			expRenames:   map[string]string{primaryUDNIfName: "eth0-5"},
			errExp:       true,
		},
		{
			desc:         "link set down failure is an error",
			links:        [][2]string{{primaryUDNIfName, "eth0-5"}},
			netlinkFails: "LinkSetDown",
			errExp:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			origNetLinkOps := util.GetNetLinkOps()
			mockNetLinkOps := new(util_mocks.NetLinkOps)
			util.SetNetLinkOpMockInst(mockNetLinkOps)
			t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

			var links []netlink.Link
			mockLinks := map[string]*netlink_mocks.Link{}
			for _, nameAlias := range tc.links {
				link := linkWith(nameAlias[0], nameAlias[1])
				mockLinks[nameAlias[0]] = link
				links = append(links, link)
			}
			mockNetLinkOps.On("LinkList").Return(links, tc.linkListErr)
			for _, op := range []string{"LinkSetDown", "LinkSetName", "LinkSetNsFd"} {
				var ret error
				if tc.netlinkFails == op {
					ret = fmt.Errorf("mock %s error", op)
				}
				mockNetLinkOps.On(op, mock.Anything, mock.Anything).Return(ret).Maybe()
				if op == "LinkSetDown" {
					mockNetLinkOps.On(op, mock.Anything).Return(ret).Maybe()
				}
			}

			netNS := new(cni_ns_mocks.NetNS)
			netNS.On("Do", mock.AnythingOfType("func(ns.NetNS) error")).Return(
				func(do func(ns.NetNS) error) error { return do(nil) },
			)
			hostNS := new(cni_ns_mocks.NetNS)
			hostNS.On("Fd").Return(uintptr(7)).Maybe()

			err := returnSimulatedNetdevsToHost(netNS, hostNS)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			for name, newName := range tc.expRenames {
				mockNetLinkOps.AssertCalled(t, "LinkSetName", mockLinks[name], newName)
			}
			for _, name := range tc.expMoved {
				if tc.netlinkFails == "" {
					mockNetLinkOps.AssertCalled(t, "LinkSetNsFd", mockLinks[name], 7)
				}
			}
			if len(tc.expRenames) == 0 {
				mockNetLinkOps.AssertNotCalled(t, "LinkSetName", mock.Anything, mock.Anything)
			}
			if len(tc.expMoved) == 0 && tc.netlinkFails != "LinkSetNsFd" {
				mockNetLinkOps.AssertNotCalled(t, "LinkSetNsFd", mock.Anything, mock.Anything)
			}
		})
	}

	// The table above drives every netlink operation to a single behavior, so
	// it cannot express per-device outcomes: cover the partial-failure
	// contract separately, one device failing must not stop the return of the
	// others and the error must be reported.
	t.Run("a failing netdev does not stop the return of the others", func(t *testing.T) {
		origNetLinkOps := util.GetNetLinkOps()
		mockNetLinkOps := new(util_mocks.NetLinkOps)
		util.SetNetLinkOpMockInst(mockNetLinkOps)
		t.Cleanup(func() { util.SetNetLinkOpMockInst(origNetLinkOps) })

		failing := linkWith(primaryUDNIfName, "eth0-5")
		succeeding := linkWith("eth0-7", "")
		mockNetLinkOps.On("LinkList").Return([]netlink.Link{failing, succeeding}, nil)
		mockNetLinkOps.On("LinkSetDown", failing).Return(nil)
		mockNetLinkOps.On("LinkSetName", failing, "eth0-5").Return(fmt.Errorf("mock rename error"))
		mockNetLinkOps.On("LinkSetDown", succeeding).Return(nil)
		mockNetLinkOps.On("LinkSetNsFd", succeeding, 7).Return(nil)

		netNS := new(cni_ns_mocks.NetNS)
		netNS.On("Do", mock.AnythingOfType("func(ns.NetNS) error")).Return(
			func(do func(ns.NetNS) error) error { return do(nil) },
		)
		hostNS := new(cni_ns_mocks.NetNS)
		hostNS.On("Fd").Return(uintptr(7))

		err := returnSimulatedNetdevsToHost(netNS, hostNS)
		require.ErrorContains(t, err, "mock rename error")
		mockNetLinkOps.AssertCalled(t, "LinkSetNsFd", succeeding, 7)
		mockNetLinkOps.AssertNotCalled(t, "LinkSetNsFd", failing, mock.Anything)
	})
}
