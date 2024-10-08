package util

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestMarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc           string
		inpPodAnnot    PodAnnotation
		errAssert      bool  // used when an error string CANNOT be matched or sub-matched
		errMatch       error //used when an error string CAN be matched or sub-matched
		expectedOutput map[string]string
	}{
		{
			desc:           "PodAnnotation instance with no fields set",
			inpPodAnnot:    PodAnnotation{},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
		},
		{
			desc:           "PodAnnotation instance when role is set to primary",
			inpPodAnnot:    PodAnnotation{Role: types.NetworkRolePrimary},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","role":"primary"}}`},
		},
		{
			desc: "single IP assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"","ip_address":"192.168.0.5/24"}}`},
		},
		{
			desc: "multiple IPs assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{
					ovntest.MustParseIPNet("192.168.0.5/24"),
					ovntest.MustParseIPNet("fd01::1234/64"),
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24","fd01::1234/64"],"mac_address":""}}`},
		},
		{
			desc: "test code path when podInfo.Gateways count is equal to ONE",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
				},
				Role: types.NetworkRoleSecondary,
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"","gateway_ips":["192.168.0.1"],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1","role":"secondary"}}`},
		},
		{
			desc:     "verify error thrown when number of gateways greater than one for a single-stack network",
			errMatch: fmt.Errorf("bad podNetwork data: single-stack network can only have a single gateway"),
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.1.0"),
					net.ParseIP("fd01::1"),
				},
			},
		},
		{
			desc:      "verify error thrown when destination IP not specified as part of Route",
			errAssert: true,
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("0.0.0.0/0"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
			},
		},
		{
			desc: "test code path when destination IP is specified as part of Route",
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
				Role: types.NetworkRoleInfrastructure,
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"role":"infrastructure-locked"}}`},
		},
		{
			desc: "next hop not set for route",
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest: ovntest.MustParseIPNet("192.168.1.0/24"),
					},
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","routes":[{"dest":"192.168.1.0/24","nextHop":""}]}}`},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			var e error
			res := map[string]string{}
			res, e = MarshalPodAnnotation(res, &tc.inpPodAnnot, types.DefaultNetworkName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				assert.True(t, reflect.DeepEqual(res, tc.expectedOutput))
			}
		})
	}
}

func TestUnmarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc        string
		inpAnnotMap map[string]string
		errAssert   bool
		errMatch    error
	}{
		{
			desc:        "verify `OVN pod annotation not found` error thrown",
			inpAnnotMap: nil,
			errMatch:    fmt.Errorf("could not find OVN pod annotation in"),
		},
		{
			desc:        "verify json unmarshal error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"}}`}, //removed a quote to force json unmarshal error
			errMatch:    fmt.Errorf("failed to unmarshal ovn pod annotation"),
		},
		{
			desc:        "verify MAC error parse error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
			errMatch:    fmt.Errorf("failed to parse pod MAC"),
		},
		{
			desc:        "test path when ip_addresses is empty and ip_address is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"0a:58:fd:98:00:01", "ip_address":"192.168.0.11/24"}}`},
		},
		{
			desc:        "verify error thrown when ip_address and ip_addresses are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.11/24"}}`},
			errMatch:    fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)"),
		},
		{
			desc:        "verify error thrown when failed to parse pod IP",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0./24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0./24"}}`},
			errMatch:    fmt.Errorf("failed to parse pod IP"),
		},
		{
			desc:        "verify error thrown when gateway_ip and gateway_ips are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0.1"], "gateway_ip":"192.168.1.1","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errMatch:    fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)"),
		},
		{
			desc:        "test path when gateway_ips list is empty but gateway_ip is present",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":[], "gateway_ip":"192.168.0.1","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
		},
		{
			desc:        "verify error thrown when failed to parse pod gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0."], "gateway_ip":"192.168.0.","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errMatch:    fmt.Errorf("failed to parse pod gateway"),
		},
		{
			desc:        "verify error thrown when failed to parse pod route destination",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1./24"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errMatch:    fmt.Errorf("failed to parse pod route dest"),
		},
		{
			desc:        "verify error thrown when default Route not specified as gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"0.0.0.0/0"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errAssert:   true,
		},
		{
			desc:        "verify error thrown when failed to parse pod route next hop",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1."}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errMatch:    fmt.Errorf("failed to parse pod route next hop"),
		},
		{
			desc:        "verify error thrown where pod route has next hop of different family",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"fd01::1234/64","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errAssert:   true,
		},
		{
			desc:        "verify successful unmarshal of pod annotation",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
		},
		{
			desc:        "verify successful unmarshal of pod annotation when role field is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1","role":"primary"}}`},
		},
		{
			desc:        "verify successful unmarshal of pod annotation when *only* the MAC address is present",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"mac_address":"0a:58:fd:98:00:01"}}`},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := UnmarshalPodAnnotation(tc.inpAnnotMap, types.DefaultNetworkName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				t.Log(res)
				assert.NotNil(t, res)
			}
		})
	}
}

func TestGetPodIPsOfNetwork(t *testing.T) {
	const (
		secondaryNetworkIPAddr = "200.200.200.200"
		namespace              = "ns1"
		secondaryNetworkName   = "bluetenant"
	)
	tests := []struct {
		desc        string
		inpPod      *v1.Pod
		networkInfo NetInfo
		errAssert   bool
		errMatch    error
		outExp      []net.IP
	}{
		// TODO: The function body may need to check that pod input is non-nil to avoid panic ?
		/*{
			desc:	"test when pod input is nil",
			inpPod: nil,
			errExp: true,
		},*/
		{
			desc: "test when pod annotation is non-nil for the default cluster network",
			inpPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.1/24"],"mac_address":"0a:58:fd:98:00:01"}}`},
				},
			},
			networkInfo: &DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.0.1")},
		},
		{
			desc:        "test when pod.status.PodIP is empty",
			inpPod:      &v1.Pod{},
			networkInfo: &DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod.status.PodIP is non-empty",
			inpPod: &v1.Pod{
				Status: v1.PodStatus{
					PodIP: "192.168.1.15",
				},
			},
			networkInfo: &DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.1.15")},
		},
		{
			desc: "test when pod.status.PodIPs is non-empty",
			inpPod: &v1.Pod{
				Status: v1.PodStatus{
					PodIPs: []v1.PodIP{
						{"192.168.1.15"},
					},
				},
			},
			networkInfo: &DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.1.15")},
		},
		{
			desc: "test path when an entry in pod.status.PodIPs is malformed",
			inpPod: &v1.Pod{
				Status: v1.PodStatus{
					PodIPs: []v1.PodIP{
						{"192.168.1."},
					},
				},
			},
			networkInfo: &DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod annotation is non-nil for a secondary network",
			inpPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name": %q, "namespace": %q}]`, secondaryNetworkName, namespace),
						"k8s.ovn.org/pod-networks":    fmt.Sprintf(`{%q:{"ip_addresses":["%s/24"],"mac_address":"0a:58:fd:98:00:01"}}`, GetNADName(namespace, secondaryNetworkName), secondaryNetworkIPAddr),
					},
				},
			},
			networkInfo: newDummyNetInfo(namespace, secondaryNetworkName),
			outExp:      []net.IP{ovntest.MustParseIP(secondaryNetworkIPAddr)},
		},
		{
			desc: "test when pod annotation is non-nil for a secondary network",
			inpPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name": %q, "namespace": %q}]`, secondaryNetworkName, namespace),
						"k8s.ovn.org/pod-networks":    "{}",
					},
				},
			},
			networkInfo: newDummyNetInfo(namespace, secondaryNetworkName),
			outExp:      []net.IP{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res1, e := GetPodIPsOfNetwork(tc.inpPod, tc.networkInfo)
			t.Log(res1, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				if errors.Is(tc.errMatch, ErrNoPodIPFound) {
					assert.ErrorIs(t, e, ErrNoPodIPFound)
				} else {
					assert.Contains(t, e.Error(), tc.errMatch.Error())
				}
			} else {
				assert.Equal(t, tc.outExp, res1)
			}
			if len(tc.outExp) > 0 {
				res2, e := GetPodCIDRsWithFullMask(tc.inpPod, tc.networkInfo)
				t.Log(res2, e)
				if tc.errAssert {
					assert.Error(t, e)
				} else if tc.errMatch != nil {
					if errors.Is(tc.errMatch, ErrNoPodIPFound) {
						assert.ErrorIs(t, e, ErrNoPodIPFound)
					} else {
						assert.Contains(t, e.Error(), tc.errMatch.Error())
					}
				} else {
					expectedIP := tc.outExp[0]
					ipNet := net.IPNet{
						IP:   expectedIP,
						Mask: GetIPFullMask(expectedIP),
					}
					assert.Equal(t, []*net.IPNet{&ipNet}, res2)
				}
			}
		})
	}
}

func newDummyNetInfo(namespace, networkName string) NetInfo {
	netInfo, _ := newLayer2NetConfInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: networkName},
	})
	netInfo.AddNADs(GetNADName(namespace, networkName))
	return netInfo
}

func TestUnmarshalUDNOpenPortsAnnotation(t *testing.T) {
	intRef := func(i int) *int {
		return &i
	}

	tests := []struct {
		desc      string
		input     string
		errSubstr string
		result    []*OpenPort
	}{
		{
			desc:      "protocol without port",
			input:     `- protocol: tcp`,
			errSubstr: "port is required",
		},
		{
			desc:      "port without protocol",
			input:     `- port: 80`,
			errSubstr: "invalid protocol",
		},
		{
			desc:      "invalid protocol",
			input:     `- protocol: foo`,
			errSubstr: "invalid protocol",
		},
		{
			desc: "icmp with port",
			input: `- protocol: icmp
  port: 80`,
			errSubstr: "invalid port 80 for icmp protocol, should be empty",
		},
		{
			desc:  "valid icmp",
			input: `- protocol: icmp`,
			result: []*OpenPort{
				{
					Protocol: "icmp",
				},
			},
		},
		{
			desc: "invalid port",
			input: `- protocol: tcp
  port: 100000`,
			errSubstr: "invalid port",
		},
		{
			desc: "valid tcp",
			input: `- protocol: tcp
  port: 80`,
			result: []*OpenPort{
				{
					Protocol: "tcp",
					Port:     intRef(80),
				},
			},
		},
		{
			desc: "valid multiple protocols",
			input: `- protocol: tcp
  port: 1
- protocol: udp
  port: 2
- protocol: sctp
  port: 3
- protocol: icmp`,
			result: []*OpenPort{
				{
					Protocol: "tcp",
					Port:     intRef(1),
				},
				{
					Protocol: "udp",
					Port:     intRef(2),
				},
				{
					Protocol: "sctp",
					Port:     intRef(3),
				},
				{
					Protocol: "icmp",
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			res, err := UnmarshalUDNOpenPortsAnnotation(map[string]string{
				UDNOpenPortsAnnotationName: tc.input,
			})
			if tc.errSubstr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.result, res)
			}
		})
	}
}
