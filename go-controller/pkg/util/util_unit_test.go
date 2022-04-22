package util

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"

	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestGetLegacyK8sMgmtIntfName(t *testing.T) {
	tests := []struct {
		desc        string
		inpNodeName string
		expRetStr   string
	}{
		{
			desc:        "node name less than 11 characters",
			inpNodeName: "lesseleven",
			expRetStr:   "k8s-lesseleven",
		},
		{
			desc:        "node name more than 11 characters",
			inpNodeName: "morethaneleven",
			expRetStr:   "k8s-morethanele",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ret := GetLegacyK8sMgmtIntfName(tc.inpNodeName)
			if tc.expRetStr != ret {
				t.Fail()
			}
		})
	}
}

func TestGetNodeChassisID(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		errExpected             bool
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "ovs-vsctl command returns error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID along with error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID with NO error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns valid chassisID",
			errExpected:             false,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("4e98c281-f12b-4601-ab5a-a3d759fcb493")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			ret, e := GetNodeChassisID()
			if tc.errExpected {
				assert.Error(t, e)
			} else {
				assert.Greater(t, len(ret), 0)
			}
			mockExecRunner.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestUpdateIPsSlice(t *testing.T) {
	var tests = []struct {
		name              string
		s, oldIPs, newIPs []string
		want              []string
	}{
		{
			"Tests no matching IPs to remove",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"1.1.1.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
		},
		{
			"Tests some matching IPs to replace",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"10.0.0.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "9.9.9.9", "127.0.0.2"},
		},
		{
			"Tests matching IPv6 to replace",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"fed9::5", "ab13::1e15", "fe99::1"},
		},
		{
			"Tests match but nothing to replace with",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9"},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
		{
			"Tests with no newIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
		{
			"Tests with no newIPs or oldIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans := UpdateIPsSlice(tt.s, tt.oldIPs, tt.newIPs)
			if !reflect.DeepEqual(ans, tt.want) {
				t.Errorf("got %v, want %v", ans, tt.want)
			}
		})
	}
}

func TestFilterIPsSlice(t *testing.T) {

	var tests = []struct {
		s, cidrs []string
		keep     bool
		want     []string
	}{
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24"},
			keep:  true,
			want:  []string{"1.0.0.1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24"},
			keep:  false,
			want:  []string{"2.0.0.1", "2001::1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"2001::/64"},
			keep:  true,
			want:  []string{"2001::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"2001::/64"},
			keep:  false,
			want:  []string{"1.0.0.1", "2.0.0.1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "2001::/64", "3.0.0.0/24"},
			keep:  false,
			want:  []string{"2.0.0.1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "2001::/64", "3.0.0.0/24"},
			keep:  true,
			want:  []string{"1.0.0.1", "2001::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "0.0.0.0/0"},
			keep:  true,
			want:  []string{"1.0.0.1", "2.0.0.1"},
		},
	}

	for i, tc := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			cidrs := []net.IPNet{}
			for _, cidr := range tc.cidrs {
				_, n, err := net.ParseCIDR(cidr)
				if err != nil {
					t.Fatal(err)
				}
				cidrs = append(cidrs, *n)
			}

			actual := FilterIPsSlice(tc.s, cidrs, tc.keep)
			assert.Equal(t, tc.want, actual)
		})
	}
}

func TestUpdateNodeSwitchExcludeIPs(t *testing.T) {
	nodeName := "ovn-control-plane"

	fakeManagementPort := &nbdb.LogicalSwitchPort{
		Name: types.K8sPrefix + nodeName,
		UUID: types.K8sPrefix + nodeName + "-uuid",
	}

	fakeHoPort := &nbdb.LogicalSwitchPort{
		Name: types.HybridOverlayPrefix + nodeName,
		UUID: types.HybridOverlayPrefix + nodeName + "-uuid",
	}

	tests := []struct {
		desc                    string
		inpSubnetStr            string
		setCfgHybridOvlyEnabled bool
		initialNbdb             libovsdbtest.TestSetup
		expectedNbdb            libovsdbtest.TestSetup
	}{
		{
			desc: "IPv6 CIDR, excludes ips empty",
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name:        nodeName,
						UUID:        nodeName + "-uuid",
						Ports:       []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{"ipv6_prefix": "ipv6_prefix"},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name:        nodeName,
						UUID:        nodeName + "-uuid",
						Ports:       []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{"ipv6_prefix": "ipv6_prefix"},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			inpSubnetStr: "fd04:3e42:4a4e:3381::/64",
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, haveHybridOverlayPort=true and haveManagementPort=true, excludes ips empty",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{
							"subnet": "subnet",
						},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, haveHybridOverlayPort=false and haveManagementPort=false, excludes HO and MP ips",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveHybridOverlayPort=false and haveManagementPort=true, excludes HO ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeManagementPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.3",
						},
					},
					fakeManagementPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveHybridOverlayPort=true and haveManagementPort=false, excludes MP ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
					fakeHoPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=false, haveManagementPort=true, excludes ips empty",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
					fakeManagementPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet": "subnet",
						},
					},
					fakeManagementPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=false, haveManagementPort=false, excludes MP ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
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

			_, ipnet, err := net.ParseCIDR(tc.inpSubnetStr)
			if err != nil {
				t.Fail()
			}
			var e error
			if tc.setCfgHybridOvlyEnabled {
				config.HybridOverlay.Enabled = true
				if e = UpdateNodeSwitchExcludeIPs(nbClient, nodeName, ipnet); e != nil {
					t.Fatal(fmt.Errorf("failed to update NodeSwitchExcludeIPs with Hybrid Overlay enabled err: %v", e))
				}
				config.HybridOverlay.Enabled = false
			} else {
				if e = UpdateNodeSwitchExcludeIPs(nbClient, nodeName, ipnet); e != nil {
					t.Fatal(fmt.Errorf("failed to update NodeSwitchExcludeIPs with Hybrid Overlay disabled err: %v", e))
				}

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
