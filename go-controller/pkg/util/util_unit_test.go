// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	v1 "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
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
	RunCmdExecRunner = mockExecRunner
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
				require.Error(t, e)
			} else {
				assert.NotEmpty(t, ret)
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
	assert.Len(t, id, 10)
	matchesPattern, _ := regexp.MatchString("([a-zA-Z0-9-]*)", id)
	assert.True(t, matchesPattern)
}

func TestGetNetworkScopedK8sMgmtHostIntfName(t *testing.T) {
	intfName := GetNetworkScopedK8sMgmtHostIntfName(1245678)
	assert.Equal(t, "ovn-k8s-mp12456", intfName)
}

func TestFindServicePortForEndpointSlicePort(t *testing.T) {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	tests := []struct {
		name                      string
		service                   *corev1.Service
		endpointslicePortName     string
		endpointslicePortProtocol corev1.Protocol
		wantPort                  *corev1.ServicePort
		wantErr                   bool
	}{
		{
			name: "Match named port with TCP protocol",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
						{Name: "https", Protocol: tcp, Port: 443, TargetPort: intstr.FromInt(8443)},
					},
				},
			},
			endpointslicePortName:     "http",
			endpointslicePortProtocol: tcp,
			wantPort:                  &corev1.ServicePort{Name: "http", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
			wantErr:                   false,
		},
		{
			name: "Match unnamed port (empty string)",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
					},
				},
			},
			endpointslicePortName:     "",
			endpointslicePortProtocol: tcp,
			wantPort:                  &corev1.ServicePort{Name: "", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
			wantErr:                   false,
		},
		{
			name: "Protocol mismatch",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "dns", Protocol: tcp, Port: 53, TargetPort: intstr.FromInt(5353)},
					},
				},
			},
			endpointslicePortName:     "dns",
			endpointslicePortProtocol: udp,
			wantPort:                  nil,
			wantErr:                   true,
		},
		{
			name: "Port name not found",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
					},
				},
			},
			endpointslicePortName:     "https",
			endpointslicePortProtocol: tcp,
			wantPort:                  nil,
			wantErr:                   true,
		},
		{
			name: "Multiple ports, match second one",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Protocol: tcp, Port: 80, TargetPort: intstr.FromInt(8080)},
						{Name: "grpc", Protocol: tcp, Port: 9090, TargetPort: intstr.FromInt(9091)},
						{Name: "metrics", Protocol: tcp, Port: 8080, TargetPort: intstr.FromInt(8081)},
					},
				},
			},
			endpointslicePortName:     "grpc",
			endpointslicePortProtocol: tcp,
			wantPort:                  &corev1.ServicePort{Name: "grpc", Protocol: tcp, Port: 9090, TargetPort: intstr.FromInt(9091)},
			wantErr:                   false,
		},
		{
			name: "Named target port (not numeric)",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-ns",
					Name:      "test-svc",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "web", Protocol: tcp, Port: 80, TargetPort: intstr.FromString("http")},
					},
				},
			},
			endpointslicePortName:     "web",
			endpointslicePortProtocol: tcp,
			wantPort:                  &corev1.ServicePort{Name: "web", Protocol: tcp, Port: 80, TargetPort: intstr.FromString("http")},
			wantErr:                   false,
		},
		{
			name:                      "Nil service input",
			service:                   nil,
			endpointslicePortName:     "web",
			endpointslicePortProtocol: tcp,
			wantPort:                  nil,
			wantErr:                   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FindServicePortForEndpointSlicePort(tt.service, tt.endpointslicePortName, tt.endpointslicePortProtocol)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantPort, got)
			}
		})
	}
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
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-network",
						},
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
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
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "wrong-network",
						},
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
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
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-network",
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
			got, err := ServiceFromEndpointSlice(tt.args.eps, tt.args.netInfo.GetNetworkName())
			if !tt.wantErr(t, err, fmt.Sprintf("ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)) {
				return
			}
			assert.Equalf(t, tt.want, got, "ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)
		})
	}
}

// fakeEndpointSliceLister implements discoverylisters.EndpointSliceLister for testing
type fakeEndpointSliceLister struct {
	indexer      cache.Indexer
	errorOnLabel string
}

func (f *fakeEndpointSliceLister) List(_ labels.Selector) ([]*discovery.EndpointSlice, error) {
	// Not used by GetServiceEndpointSlices - namespace-scoped List() is used instead
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeEndpointSliceLister) EndpointSlices(namespace string) v1.EndpointSliceNamespaceLister {
	return &fakeEndpointSliceNamespaceLister{
		indexer:      f.indexer,
		namespace:    namespace,
		errorOnLabel: f.errorOnLabel,
	}
}

type fakeEndpointSliceNamespaceLister struct {
	indexer      cache.Indexer
	namespace    string
	errorOnLabel string
}

func (f *fakeEndpointSliceNamespaceLister) List(selector labels.Selector) ([]*discovery.EndpointSlice, error) {
	if f.errorOnLabel != "" && selector.String() != "" {
		if strings.Contains(selector.String(), f.errorOnLabel) {
			return nil, fmt.Errorf("injected error for testing")
		}
	}

	var result []*discovery.EndpointSlice
	for _, obj := range f.indexer.List() {
		eps := obj.(*discovery.EndpointSlice)
		if eps.Namespace != f.namespace {
			continue
		}
		if selector.Matches(labels.Set(eps.Labels)) {
			result = append(result, eps)
		}
	}
	return result, nil
}

func (f *fakeEndpointSliceNamespaceLister) Get(_ string) (*discovery.EndpointSlice, error) {
	// Not used by GetServiceEndpointSlices - only List() is called
	return nil, fmt.Errorf("not implemented")
}

func TestGetServiceEndpointSlices(t *testing.T) {
	tests := []struct {
		name               string
		namespace          string
		serviceName        string
		network            string
		existingSlices     []*discovery.EndpointSlice
		expectedSliceCount int
		expectError        bool
		errorOnLabel       string
		description        string
	}{
		// Scenario 1: Default network returns combined default + UDN slices
		{
			name:        "default network with both default and UDN slices",
			namespace:   "test-ns",
			serviceName: "test-service",
			network:     types.DefaultNetworkName,
			existingSlices: []*discovery.EndpointSlice{
				// Default network endpoint slice
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-abc",
						Namespace: "test-ns",
						Labels: map[string]string{
							discovery.LabelServiceName: "test-service",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.0.5"},
						},
					},
				},
				// UDN endpoint slice (mirrored from Primary CUDN with open-default-ports)
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-udn-xyz",
						Namespace: "test-ns",
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-cudn",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.0.5"},
						},
					},
				},
				// UDN endpoint slice for a different network (also returned for default network query)
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-udn-other",
						Namespace: "test-ns",
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "secondary-cudn",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.5"},
						},
					},
				},
			},
			expectedSliceCount: 3,
			expectError:        false,
			description:        "Should return combined default + all UDN endpoint slices for default network service",
		},
		// Scenario 2: Default network when no UDN slices exist
		{
			name:        "default network when no UDN slices exist",
			namespace:   "test-ns",
			serviceName: "test-service",
			network:     types.DefaultNetworkName,
			existingSlices: []*discovery.EndpointSlice{
				// Only default network endpoint slice exists
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-abc",
						Namespace: "test-ns",
						Labels: map[string]string{
							discovery.LabelServiceName: "test-service",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.0.5"},
						},
					},
				},
				// NO UDN slices exist (UDN lookup returns empty)
			},
			expectedSliceCount: 1,
			expectError:        false,
			description:        "Should return only default slices when no UDN slices exist",
		},
		// Scenario 3: Non-default network behavior unchanged
		{
			name:        "UDN network returns only UDN slices for that network",
			namespace:   "test-ns",
			serviceName: "test-service",
			network:     "primary-cudn",
			existingSlices: []*discovery.EndpointSlice{
				// Default network slice (should be IGNORED for UDN network query)
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-abc",
						Namespace: "test-ns",
						Labels: map[string]string{
							discovery.LabelServiceName: "test-service",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.244.0.5"},
						},
					},
				},
				// UDN slice for primary-cudn network
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-udn-xyz",
						Namespace: "test-ns",
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-cudn",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.0.5"},
						},
					},
				},
				// UDN slice for different network (should be filtered out)
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-service-other-udn",
						Namespace: "test-ns",
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "other-network",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.5"},
						},
					},
				},
			},
			expectedSliceCount: 1,
			expectError:        false,
			description:        "Non-default network query should only return UDN slices for that specific network",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, eps := range tt.existingSlices {
				err := indexer.Add(eps)
				require.NoError(t, err)
			}

			lister := &fakeEndpointSliceLister{
				indexer:      indexer,
				errorOnLabel: tt.errorOnLabel,
			}

			result, err := GetServiceEndpointSlices(tt.namespace, tt.serviceName, tt.network, lister)

			if tt.expectError {
				require.Error(t, err, tt.description)
			} else {
				require.NoError(t, err, tt.description)
				assert.Len(t, result, tt.expectedSliceCount, tt.description)

				if tt.name == "default network with both default and UDN slices" {
					require.Len(t, result, 3, "Should return 3 distinct slices")

					var defaultSliceCount, udnSliceCount int
					sliceNames := make(map[string]bool)
					for _, slice := range result {
						assert.False(t, sliceNames[slice.Name], "Slice names should be unique: %s", slice.Name)
						sliceNames[slice.Name] = true

						if _, hasDefaultLabel := slice.Labels[discovery.LabelServiceName]; hasDefaultLabel {
							defaultSliceCount++
						}
						if _, hasUDNLabel := slice.Labels[types.LabelUserDefinedServiceName]; hasUDNLabel {
							udnSliceCount++
						}
					}
					assert.Equal(t, 1, defaultSliceCount, "Should have exactly 1 default slice")
					assert.Equal(t, 2, udnSliceCount, "Should have exactly 2 UDN slices")

					allIPs := []string{}
					for _, slice := range result {
						for _, ep := range slice.Endpoints {
							allIPs = append(allIPs, ep.Addresses...)
						}
					}
					assert.Contains(t, allIPs, "10.244.0.5", "Should include default network IP")
					assert.Contains(t, allIPs, "192.168.0.5", "Should include primary-cudn UDN IP")
					assert.Contains(t, allIPs, "192.168.1.5", "Should include secondary-cudn UDN IP")
				}

				if tt.name == "default network when no UDN slices exist" {
					require.Len(t, result, 1, "Should return exactly one slice")
					returnedSlice := result[0]
					assert.Equal(t, "test-service", returnedSlice.Labels[discovery.LabelServiceName],
						"Returned slice should have default network service label")
					_, hasUDNLabel := returnedSlice.Labels[types.LabelUserDefinedServiceName]
					assert.False(t, hasUDNLabel, "Returned slice should NOT have UDN label")
				}

				if tt.name == "UDN network returns only UDN slices for that network" {
					require.Len(t, result, 1, "Should return exactly one slice")
					returnedSlice := result[0]
					assert.Equal(t, "primary-cudn", returnedSlice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation],
						"Returned slice should be for the requested network")

					require.Len(t, returnedSlice.Endpoints, 1, "Should have exactly one endpoint")
					assert.Contains(t, returnedSlice.Endpoints[0].Addresses, "192.168.0.5",
						"Should include primary-cudn network IP")

					allIPs := []string{}
					for _, ep := range returnedSlice.Endpoints {
						allIPs = append(allIPs, ep.Addresses...)
					}
					assert.NotContains(t, allIPs, "10.244.0.5", "Should NOT include default network IP")
					assert.NotContains(t, allIPs, "192.168.1.5", "Should NOT include other-network UDN IP")
				}
			}
		})
	}
}

// TestGetServiceEndpointSlicesErrors tests error handling in GetServiceEndpointSlices
func TestGetServiceEndpointSlicesErrors(t *testing.T) {
	errorTests := []struct {
		name         string
		namespace    string
		serviceName  string
		network      string
		errorOnLabel string
		description  string
	}{
		{
			name:         "error fetching default slices for default network",
			namespace:    "test-ns",
			serviceName:  "test-service",
			network:      types.DefaultNetworkName,
			errorOnLabel: discovery.LabelServiceName,
			description:  "Should return error when default slice lookup fails",
		},
		{
			name:         "error fetching UDN slices for non-default network",
			namespace:    "test-ns",
			serviceName:  "test-service",
			network:      "primary-cudn",
			errorOnLabel: types.LabelUserDefinedServiceName,
			description:  "Should return error when UDN slice lookup fails for non-default network",
		},
		{
			name:         "error fetching UDN slices for default network",
			namespace:    "test-ns",
			serviceName:  "test-service",
			network:      types.DefaultNetworkName,
			errorOnLabel: types.LabelUserDefinedServiceName,
			description:  "Should return error when UDN slice lookup fails for default network",
		},
	}

	for _, tt := range errorTests {
		t.Run(tt.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			lister := &fakeEndpointSliceLister{
				indexer:      indexer,
				errorOnLabel: tt.errorOnLabel,
			}

			result, err := GetServiceEndpointSlices(tt.namespace, tt.serviceName, tt.network, lister)

			require.Error(t, err, tt.description)
			assert.Nil(t, result, "Result should be nil when error occurs")
		})
	}
}
