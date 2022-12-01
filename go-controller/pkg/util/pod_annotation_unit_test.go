package util

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestMarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc            string
		inpPodAnnot     PodAnnotation
		errAssert       bool   // used when an error string CANNOT be matched or sub-matched
		errMatch        string //used when an error string CAN be matched or sub-matched
		expectedOutput  map[string]string
		existingAnnot   map[string]string
		replaceExisting bool
	}{
		{
			desc:        "error when no fields are set",
			errAssert:   true,
			inpPodAnnot: PodAnnotation{},
		},
		{
			desc: "error when single IP assigned to pod with MAC with gateway and routes not specified",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
			},
			errMatch: "unable to determine any valid MAC",
		},
		{
			desc: "verify no error when single IP, MAC and gateway assigned to pod with no routes specified",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","gateway_ips":["192.168.0.1"],"ip_address":"192.168.0.5/32","gateway_ip":"192.168.0.1"}}`},
		},
		{
			desc: "verify no error when single IP, MAC, gateway and a route assigned to pod",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
				},
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/32","gateway_ip":"192.168.0.1"}}`},
		},
		{
			desc: "verify error when number of gateways greater than one for a single-stack network",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
					net.ParseIP("fd01::1"),
				},
			},
			errMatch: "single-stack network can only have a single gateway",
		},
		{
			desc: "verify multiple gateways when using dual-stack network",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32"), ovntest.MustParseIPNet("2001:0db8:3c4d:0015::1a2f:1a2b/128")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
					net.ParseIP("fd01::1"),
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32","2001:db8:3c4d:15::1a2f:1a2b/128"],"mac_address":"00:00:5e:00:53:af","gateway_ips":["192.168.0.1","fd01::1"]}}`},
		},
		{
			desc: "verify error thrown when destination IP not specified as part of Route",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("0.0.0.0/0"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
			},
			errMatch: "should have valid destination",
		},
		{
			desc: "verify multiple routes",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
					{
						Dest:    ovntest.MustParseIPNet("192.168.2.0/24"),
						NextHop: net.ParseIP("192.168.2.1"),
					},
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"},{"dest":"192.168.2.0/24","nextHop":"192.168.2.1"}],"ip_address":"192.168.0.5/32"}}`},
		},
		{
			desc: "verify no error when next hop not set for route",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:af"),
				Routes: []PodRoute{
					{
						Dest: ovntest.MustParseIPNet("192.168.1.0/24"),
					},
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","routes":[{"dest":"192.168.1.0/24","nextHop":""}],"ip_address":"192.168.0.5/32"}}`},
		},
		{
			desc: "should not replace existing IP and MAC address",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.6/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:ff"),
			},
			errAssert:     true,
			existingAnnot: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","ip_address":"192.168.0.5/32"}}`},
		},
		{
			desc: "should replace existing IP and MAC address when forced",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.6/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:ff"),
			},
			replaceExisting: true,
			existingAnnot:   map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","ip_address":"192.168.0.5/32"}}`},
			expectedOutput:  map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.6/32"],"mac_address":"00:00:5e:00:53:ff","ip_address":"192.168.0.6/32"}}`},
		},
		{
			desc: "should update existing routes when forced",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/32")},
				MAC: ovntest.MustParseMAC("00:00:5e:00:53:ff"),
				Gateways: []net.IP{
					net.ParseIP("192.168.0.2"),
				},
				Routes: []PodRoute{
					{
						Dest: ovntest.MustParseIPNet("192.168.1.0/24"),
					},
				},
			},
			replaceExisting: true,
			existingAnnot:   map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:af","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.100/24","nextHop":""}],"ip_address":"192.168.0.5/32","gateway_ip":"192.168.0.1"}}`},
			expectedOutput:  map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/32"],"mac_address":"00:00:5e:00:53:ff","gateway_ips":["192.168.0.2"],"routes":[{"dest":"192.168.1.0/24","nextHop":""}],"ip_address":"192.168.0.5/32","gateway_ip":"192.168.0.2"}}`},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			var (
				e                  error
				existingAnnot, res map[string]string
			)
			if len(tc.existingAnnot) > 0 {
				existingAnnot = tc.existingAnnot
			} else {
				existingAnnot = make(map[string]string)
			}
			res, e = MarshalPodAnnotation(existingAnnot, &tc.inpPodAnnot, types.DefaultNetworkName, tc.replaceExisting)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != "" {
				assert.Contains(t, e.Error(), tc.errMatch)
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
			desc:        "verify error thrown when neither ip_addresses nor ip_address is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"0a:58:fd:98:00:01"}}`},
			errMatch:    fmt.Errorf("bad annotation data (neither ip_address nor ip_addresses is set)"),
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

func TestGetAllPodIPs(t *testing.T) {
	tests := []struct {
		desc      string
		inpPod    *v1.Pod
		errAssert bool
		errMatch  error
		outExp    []net.IP
	}{
		// TODO: The function body may need to check that pod input is non-nil to avoid panic ?
		/*{
			desc:	"test when pod input is nil",
			inpPod: nil,
			errExp: true,
		},*/
		{
			desc: "test when pod annotation is non-nil",
			inpPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.1/24"],"mac_address":"0a:58:fd:98:00:01"}}`},
				},
			},
			outExp: []net.IP{ovntest.MustParseIP("192.168.0.1")},
		},
		{
			desc:     "test when pod.status.PodIP is empty",
			inpPod:   &v1.Pod{},
			errMatch: ErrNoPodIPFound,
		},
		{
			desc: "test when pod.status.PodIP is non-empty",
			inpPod: &v1.Pod{
				Status: v1.PodStatus{
					PodIP: "192.168.1.15",
				},
			},
			outExp: []net.IP{ovntest.MustParseIP("192.168.1.15")},
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
			outExp: []net.IP{ovntest.MustParseIP("192.168.1.15")},
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
			errMatch: ErrNoPodIPFound,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res1, e := GetAllPodIPs(tc.inpPod)
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
			res2, e := GetPodCIDRsWithFullMask(tc.inpPod)
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
				podIPStr := tc.outExp[0].String()
				mask := GetIPFullMask(podIPStr)
				_, ipnet, _ := net.ParseCIDR(podIPStr + mask)
				assert.Equal(t, []*net.IPNet{ipnet}, res2)
			}
		})
	}
}
