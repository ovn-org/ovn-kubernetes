package util

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
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
		changed           bool
	}{
		{
			"Tests no matching IPs to remove",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"1.1.1.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			false,
		},
		{
			"Tests some matching IPs to replace",
			[]string{"192.168.1.1", "10.0.0.1", "127.0.0.2"},
			[]string{"10.0.0.1"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"192.168.1.1", "9.9.9.9", "127.0.0.2"},
			true,
		},
		{
			"Tests matching IPv6 to replace",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9", "fe99::1"},
			[]string{"fed9::5", "ab13::1e15", "fe99::1"},
			true,
		},
		{
			"Tests match but nothing to replace with",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{"9.9.9.9"},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			false,
		},
		{
			"Tests with no newIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{"3dfd::99ac"},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			false,
		},
		{
			"Tests with no newIPs or oldIPs",
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			[]string{},
			[]string{},
			[]string{"fed9::5", "ab13::1e15", "3dfd::99ac"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans, changed := UpdateIPsSlice(tt.s, tt.oldIPs, tt.newIPs)
			if !reflect.DeepEqual(ans, tt.want) {
				t.Errorf("got %v, want %v", ans, tt.want)
			}

			if tt.changed != changed {
				t.Errorf("got changed %t, want %t", changed, tt.changed)
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

func TestGenerateId(t *testing.T) {
	id := GenerateId(10)
	assert.Equal(t, 10, len(id))
	matchesPattern, _ := regexp.MatchString("([a-zA-Z0-9-]*)", id)
	assert.True(t, matchesPattern)
}

func TestGetNetworkScopedK8sMgmtHostIntfName(t *testing.T) {
	intfName := GetNetworkScopedK8sMgmtHostIntfName(1245678)
	assert.Equal(t, "ovn-k8s-mp12456", intfName)
}

func TestServiceFromEndpointSlice(t *testing.T) {
	config.IPv4Mode = true
	type args struct {
		eps     *discovery.EndpointSlice
		netInfo NetInfo
	}
	netInfo, _ := NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "primary-network"},
			Topology: types.Layer3Topology,
			Subnets:  "10.1.130.0/16/24",
			Role:     types.NetworkRolePrimary,
		})
	defaultNetInfo, _ := NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		})
	var tests = []struct {
		name    string
		args    args
		want    *k8stypes.NamespacedName
		wantErr assert.ErrorAssertionFunc
	}{
		{
			name: "Primary network with matching label",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Labels: map[string]string{
							types.LabelUserDefinedEndpointSliceNetwork: "primary-network",
							types.LabelUserDefinedServiceName:          "test-service",
						},
					},
				},
				netInfo: netInfo,
			},
			want: &k8stypes.NamespacedName{
				Namespace: "test-namespace",
				Name:      "test-service",
			},
			wantErr: assert.NoError,
		},
		{
			name: "Wrong primary network with matching label",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Labels: map[string]string{
							types.LabelUserDefinedEndpointSliceNetwork: "wrong-network",
							types.LabelUserDefinedServiceName:          "test-service",
						},
					},
				},
				netInfo: netInfo,
			},
			want:    nil,
			wantErr: assert.Error,
		},
		{
			name: "Primary network with no service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Labels: map[string]string{
							types.LabelUserDefinedEndpointSliceNetwork: "primary-network",
						},
					},
				},
				netInfo: netInfo,
			},
			want:    nil,
			wantErr: assert.NoError,
		},
		{
			name: "default network with a service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Labels: map[string]string{
							discovery.LabelServiceName: "test-service",
						},
					},
				},
				netInfo: defaultNetInfo,
			},
			want:    &k8stypes.NamespacedName{Namespace: "test-namespace", Name: "test-service"},
			wantErr: assert.NoError,
		},
		{
			name: "default network with no service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
					},
				},
				netInfo: defaultNetInfo,
			},
			want:    nil,
			wantErr: assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ServiceFromEndpointSlice(tt.args.eps, tt.args.netInfo)
			if !tt.wantErr(t, err, fmt.Sprintf("ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)) {
				return
			}
			assert.Equalf(t, tt.want, got, "ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)
		})
	}
}
