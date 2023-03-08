package cni

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	cni_type_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/cni/pkg/types"
	cni_ns_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/containernetworking/plugins/pkg/ns"
	netlink_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	kexec "k8s.io/utils/exec"
	"k8s.io/utils/pointer"
)

func TestRenameLink(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)

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
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
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
	util.SetNetLinkOpMockInst(mockNetLinkOps)

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
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockNetNS.AssertExpectations(t)
		})
	}
}

func TestSetupNetwork(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockLink := new(netlink_mocks.Link)
	mockCNIPlugin := new(mocks.CNIPluginLibOps)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	// below `cniPluginLibOps` is defined in helper_linux.go
	cniPluginLibOps = mockCNIPlugin

	tests := []struct {
		desc                 string
		inpLink              netlink.Link
		inpPodIfaceInfo      *PodInterfaceInfo
		errMatch             error
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
		cniPluginMockHelper  []ovntest.TestifyMockHelper
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName", Flags: net.FlagUp}}},
			},
		},
		{
			desc:    "test skip ip config",
			inpLink: mockLink,
			inpPodIfaceInfo: &PodInterfaceInfo{
				NetName: "default",
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
					SkipIPConfig: pointer.Bool(true),
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
			ovntest.ProcessMockFnList(&mockCNIPlugin.Mock, tc.cniPluginMockHelper)

			err := setupNetwork(tc.inpLink, tc.inpPodIfaceInfo)
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockLink.AssertExpectations(t)
			mockCNIPlugin.AssertExpectations(t)
		})
	}
}

func TestSetupInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockCNIPlugin := new(mocks.CNIPluginLibOps)
	mockNS := new(cni_ns_mocks.NetNS)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	// `cniPluginLibOps` is defined in helper_linux.go
	cniPluginLibOps = mockCNIPlugin

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
		cniPluginMockHelper  []ovntest.TestifyMockHelper
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
			ovntest.ProcessMockFnList(&mockCNIPlugin.Mock, tc.cniPluginMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)

			hostIface, contIface, err := setupInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo)
			t.Log(hostIface, contIface, err)
			if tc.errExp {
				assert.NotNil(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockCNIPlugin.AssertExpectations(t)
			mockNS.AssertExpectations(t)
		})
	}
}

func TestSetupSriovInterface(t *testing.T) {
	mockNetLinkOps := new(util_mocks.NetLinkOps)
	mockCNIPlugin := new(mocks.CNIPluginLibOps)
	mockSriovnetOps := new(util_mocks.SriovnetOps)
	mockNS := new(cni_ns_mocks.NetNS)
	mockLink := new(netlink_mocks.Link)
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below sets the `netLinkOps` in util/net_linux.go to a mock instance for purpose of unit tests execution
	util.SetNetLinkOpMockInst(mockNetLinkOps)
	// `cniPluginLibOps` is defined in helper_linux.go
	cniPluginLibOps = mockCNIPlugin
	// set `sriovnetOps` in util/sriovnet_linux.go to a mock instance for unit tests execution
	util.SetSriovnetOpsInst(mockSriovnetOps)

	res, err := sriovnet.GetUplinkRepresentor("0000:01:00.0")
	t.Log(res, err)
	/* Need the below to test the Do() function that requires root and needs to be figured out
	testOSNameSpace, err := ns.GetCurrentNS()
	if err != nil {
		t.Log(err)
		t.Fatal("failed to get NameSpace for test")
	}*/

	netNsDoForward := &mocks.NetNS{}
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
		cniPluginMockHelper  []ovntest.TestifyMockHelper
		nsMockHelper         []ovntest.TestifyMockHelper
		netLinkOpsMockHelper []ovntest.TestifyMockHelper
		sriovOpsMockHelper   []ovntest.TestifyMockHelper
		linkMockHelper       []ovntest.TestifyMockHelper
		onRetArgsKexecIface  []ovntest.TestifyMockHelper
		onRetArgsCmdList     []ovntest.TestifyMockHelper
		runnerInstance       kexec.Interface
	}{
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
			inpPCIAddrs:         "0000:03:00.1",
			errExp:              true,
			sriovOpsMockHelper:  []ovntest.TestifyMockHelper{},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}}},
			onRetArgsCmdList:    []ovntest.TestifyMockHelper{{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("failed to run 'ovs-vsctl")}}},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			inpPCIAddrs:         "0000:03:00.1",
			errExp:              true,
			sriovOpsMockHelper:  []ovntest.TestifyMockHelper{},
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}}},
			onRetArgsCmdList:    []ovntest.TestifyMockHelper{{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}}},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			inpPCIAddrs:         "0000:03:00.1",
			errExp:              true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}}},
			onRetArgsCmdList:    []ovntest.TestifyMockHelper{{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("failed to run 'ovs-vsctl")}}},
			runnerInstance:      mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			inpPCIAddrs: "0000:03:00.1",
			errExp:      true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, fmt.Errorf("failed to run 'ovs-vsctl")}}},
			runnerInstance:   mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{-1, fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			inpPCIAddrs: "0000:03:00.1",
			errExp:      true,
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}}},
			runnerInstance:   mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"", fmt.Errorf("mock error")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			desc:         "test code path when renaming host VF representor errors out",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			errMatch:    fmt.Errorf("failed to rename"),
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below is mocked for the renameLink() method that internally invokes LinkByName
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
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
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below 4 calls are mocked for the renameLink() method that internally invokes the below 4 calls
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// The below mock call is needed for the LinkByName() invocation right after the renameLink() method
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:         "test code path when LinkSetMTU() fails",
			inpNetNS:     mockNS,
			inpContID:    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a",
			inpIfaceName: "eth0",
			inpPodIfaceInfo: &PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				NetdevName:    "en01",
			},
			inpPCIAddrs: "0000:03:00.1",
			errMatch:    fmt.Errorf("failed to set MTU on"),
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			sriovOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "GetUplinkRepresentor", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"testlinkrepresentor", nil}},
				{OnCallMethodName: "GetVfIndexByPciAddress", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{0, nil}},
				{OnCallMethodName: "GetVfRepresentor", OnCallMethodArgType: []string{"string", "int"}, RetArgList: []interface{}{"VFRepresentor", nil}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				// The below two mock calls are needed for the moveIfToNetns() call that internally invokes them
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetNsFd", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				// The below 4 calls are mocked for the renameLink() method that internally invokes the below 4 calls
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				{OnCallMethodName: "LinkSetDown", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetName", OnCallMethodArgType: []string{"*mocks.Link", "string"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "LinkSetUp", OnCallMethodArgType: []string{"*mocks.Link"}, RetArgList: []interface{}{nil}},
				// The below mock call is needed for the LinkByName() invocation right after the renameLink() method
				{OnCallMethodName: "LinkByName", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockLink, nil}},
				// The below mock call is self-explanatory and is for the LinkSetMTU() method
				{OnCallMethodName: "LinkSetMTU", OnCallMethodArgType: []string{"*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is needed when moveIfToNetns() is called
				{OnCallMethodName: "Fd", OnCallMethodArgType: []string{}, RetArgList: []interface{}{uintptr(123456)}},
				// The below mock call is for the netns.Do() invocation
				{OnCallMethodName: "Do", OnCallMethodArgType: []string{"func(ns.NetNS) error"}, RetArgList: []interface{}{nil}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				// The below mock call is to retrieve the MAC address of host interface right before LinkSetMTU() method
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Name: "testIfaceName"}}},
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
			onRetArgsKexecIface: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "CombinedOutput", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil, nil}},
			},
			runnerInstance: mockKexecIface,
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
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
			},
			cniPluginMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "AddRoute", OnCallMethodArgType: []string{"*net.IPNet", "net.IP", "*mocks.Link", "int"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
			linkMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Attrs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{&netlink.LinkAttrs{Flags: net.FlagUp}}},
			},
			nsMockHelper: []ovntest.TestifyMockHelper{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockNetLinkOps.Mock, tc.netLinkOpsMockHelper)
			ovntest.ProcessMockFnList(&mockCNIPlugin.Mock, tc.cniPluginMockHelper)
			ovntest.ProcessMockFnList(&mockNS.Mock, tc.nsMockHelper)
			ovntest.ProcessMockFnList(&mockSriovnetOps.Mock, tc.sriovOpsMockHelper)
			ovntest.ProcessMockFnList(&mockLink.Mock, tc.linkMockHelper)
			ovntest.ProcessMockFnList(&mockKexecIface.Mock, tc.onRetArgsKexecIface)
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)

			runner = tc.runnerInstance

			ovsExec()

			netNsDoError = nil
			hostIface, contIface, err := setupSriovInterface(tc.inpNetNS, tc.inpContID, tc.inpIfaceName, tc.inpPodIfaceInfo, tc.inpPCIAddrs)
			t.Log(hostIface, contIface, err)
			if err == nil {
				err = netNsDoError
			}
			if tc.errExp {
				assert.NotNil(t, err)
			} else if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, err)
			}
			mockNetLinkOps.AssertExpectations(t)
			mockCNIPlugin.AssertExpectations(t)
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
	util.SetNetLinkOpMockInst(mockNetLinkOps)
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
				CNIConf: &types.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: nil,
					},
				},
			},
		},
		{
			desc: "test code path NewResultFromResult returns error",
			inpPodRequest: PodRequest{
				CNIConf: &types.NetConf{
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
				CNIConf: &types.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{{Name: "eth0"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
		},
		{
			desc: "test code path when DeleteConntrack returns error",
			inpPodRequest: PodRequest{
				CNIConf: &types.NetConf{
					NetConf: cnitypes.NetConf{
						PrevResult: mockTypeResult,
					},
				},
			},
			inpPrevResult: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{{Name: "eth0", Sandbox: "blah"}},
				IPs:        []*current.IPConfig{{Interface: &[]int{0}[0], Address: *ovntest.MustParseIPNet("192.168.1.15/24"), Gateway: ovntest.MustParseIP("192.168.1.1")}},
			},
			netLinkOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ConntrackDeleteFilter", OnCallMethodArgType: []string{"netlink.ConntrackTableType", "netlink.InetFamily", "*netlink.ConntrackFilter"}, RetArgList: []interface{}{uint(1), fmt.Errorf("mock error")}},
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
			tc.inpPodRequest.deletePodConntrack()
			mockTypeResult.AssertExpectations(t)
		})
	}
}
