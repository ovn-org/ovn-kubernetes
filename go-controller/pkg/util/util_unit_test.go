// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
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
