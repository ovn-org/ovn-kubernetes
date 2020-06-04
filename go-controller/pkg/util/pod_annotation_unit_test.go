package util

import (
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/stretchr/testify/assert"
	"net"
	"reflect"
	"testing"
)
import "fmt"

func TestMarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc           string
		inpPodAnnot    PodAnnotation
		errExpected    bool
		expectedOutput map[string]string
	}{
		{
			desc:           "PodAnnotation instance with no fields set",
			inpPodAnnot:    PodAnnotation{},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
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
			desc:        "verify error thrown when number of gateways greater than one for a single-stack network",
			errExpected: true,
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.1.0"),
					net.ParseIP("fd01::1"),
				},
			},
		},
		{
			desc:        "verify error thrown when destination IP not specified as part of Route",
			errExpected: true,
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
			desc: "next hop not set for route ",
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
			res, e := MarshalPodAnnotation(&tc.inpPodAnnot)
			t.Log(res, e)
			if tc.errExpected {
				assert.Error(t, e)
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
		errExpected bool
	}{
		{
			desc:        "verify `OVN pod annotation not found` error thrown",
			inpAnnotMap: nil,
			errExpected: true,
		},
		{
			desc:        "verify json unmarshal error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"}}`}, //removed a quote to force json unmarshal error
			errExpected: true,
		},
		{
			desc:        "verify MAC error parse error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when neither ip_addresses nor ip_address is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"0a:58:fd:98:00:01"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when ip_address and ip_addresses are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.11/24"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when failed to parse pod IP",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0./24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0./24"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when gateway_ip and gateway_ips are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0.1"], "gateway_ip":"192.168.1.1","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when failed to parse gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0."], "gateway_ip":"192.168.0.","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when failed to parse pod route destination",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1./24"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when default Route not specified as gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"0.0.0.0/0"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown when failed to parse pod route next hop",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1."}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errExpected: true,
		},
		{
			desc:        "verify error thrown where pod route has next hop of different family",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"fd01::1234/64","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errExpected: true,
		},
		{
			desc:        "verify successful unmarshal of pod annotation",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := UnmarshalPodAnnotation(tc.inpAnnotMap)
			t.Log(res, e)
			if tc.errExpected {
				assert.Error(t, e)
			} else {
				t.Log(res)
				assert.NotNil(t, res)
			}
		})
	}
}
