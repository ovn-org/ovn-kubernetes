package util

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	podnetworkapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestGetOVNNetwork(t *testing.T) {
	tests := []struct {
		desc           string
		inpPodAnnot    SinglePodNetwork
		errAssert      bool  // used when an error string CANNOT be matched or sub-matched
		errMatch       error //used when an error string CAN be matched or sub-matched
		expectedOutput *podnetworkapi.OVNNetwork
	}{
		{
			desc:           "SinglePodNetwork instance with no fields set",
			inpPodAnnot:    SinglePodNetwork{},
			expectedOutput: &podnetworkapi.OVNNetwork{},
		},
		{
			desc: "single IP assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: SinglePodNetwork{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
			},
			expectedOutput: &podnetworkapi.OVNNetwork{
				IPs: []string{"192.168.0.5/24"},
			},
		},
		{
			desc: "multiple IPs assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: SinglePodNetwork{
				IPs: []*net.IPNet{
					ovntest.MustParseIPNet("192.168.0.5/24"),
					ovntest.MustParseIPNet("fd01::1234/64"),
				},
			},
			expectedOutput: &podnetworkapi.OVNNetwork{
				IPs: []string{"192.168.0.5/24", "fd01::1234/64"},
			},
		},
		{
			desc: "test code path when podInfo.Gateways count is equal to ONE",
			inpPodAnnot: SinglePodNetwork{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
				},
			},
			expectedOutput: &podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				Gateways: []string{"192.168.0.1"},
			},
		},
		{
			desc:     "verify error thrown when number of gateways greater than one for a single-stack network",
			errMatch: fmt.Errorf("bad podNetwork data: single-stack network can only have a single gateway"),
			inpPodAnnot: SinglePodNetwork{
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
			inpPodAnnot: SinglePodNetwork{
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
			inpPodAnnot: SinglePodNetwork{
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
			},
			expectedOutput: &podnetworkapi.OVNNetwork{
				Routes: []podnetworkapi.PodRoute{{
					Dest:    "192.168.1.0/24",
					NextHop: "192.168.1.1",
				}},
			},
		},
		{
			desc: "next hop not set for route",
			inpPodAnnot: SinglePodNetwork{
				Routes: []PodRoute{
					{
						Dest: ovntest.MustParseIPNet("192.168.1.0/24"),
					},
				},
			},
			expectedOutput: &podnetworkapi.OVNNetwork{
				Routes: []podnetworkapi.PodRoute{{
					Dest: "192.168.1.0/24",
				}},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := GetOVNNetwork(&tc.inpPodAnnot)
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

func getPodNetwork(ovnnet podnetworkapi.OVNNetwork) *podnetworkapi.PodNetwork {
	return &podnetworkapi.PodNetwork{
		// don't init ObjectMeta, since it is not used in TestGetAllPodIPs
		Spec: podnetworkapi.PodNetworkSpec{
			Networks: map[string]podnetworkapi.OVNNetwork{
				types.DefaultNetworkName: ovnnet,
			},
		},
	}
}

func TestParsePodNetwork(t *testing.T) {
	tests := []struct {
		desc       string
		podNetwork *podnetworkapi.PodNetwork
		errAssert  bool
		errMatch   error
	}{
		{
			desc:       "verify nil podNetwork error thrown",
			podNetwork: nil,
			errMatch:   fmt.Errorf("podNetwork is nil"),
		},
		{
			desc: "verify MAC error parse error",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs: nil,
				MAC: "",
			}),
			errMatch: fmt.Errorf("failed to parse pod MAC"),
		},
		{
			desc: "verify error thrown when neither ip_addresses nor ip_address is set",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs: nil,
				MAC: "0a:58:fd:98:00:01",
			}),
			errMatch: fmt.Errorf("bad podNetwork %s data ip_addresses are not set", types.DefaultNetworkName),
		},
		{
			desc: "verify error thrown when failed to parse pod IP",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs: []string{"192.168.0./24"},
				MAC: "0a:58:fd:98:00:01",
			}),
			errMatch: fmt.Errorf("failed to parse pod IP"),
		},
		{
			desc: "test path when gateway_ips list is empty but gateway_ip is present",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				Gateways: []string{},
				MAC:      "0a:58:fd:98:00:01",
			}),
		},
		{
			desc: "verify error thrown when failed to parse pod gateway",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				Gateways: []string{"192.168.0."},
				MAC:      "0a:58:fd:98:00:01",
			}),
			errMatch: fmt.Errorf("failed to parse pod gateway"),
		},
		{
			desc: "verify error thrown when failed to parse pod route destination",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				MAC:      "0a:58:fd:98:00:01",
				Gateways: []string{"192.168.0.1"},
				Routes: []podnetworkapi.PodRoute{
					{Dest: "192.168.1./24"},
				},
			}),
			errMatch: fmt.Errorf("failed to parse pod route dest"),
		},
		{
			desc: "verify error thrown when default Route not specified as gateway",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				MAC:      "0a:58:fd:98:00:01",
				Gateways: []string{"192.168.0.1"},
				Routes: []podnetworkapi.PodRoute{
					{Dest: "0.0.0.0/0"},
				},
			}),
			errAssert: true,
		},
		{
			desc: "verify error thrown when failed to parse pod route next hop",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				MAC:      "0a:58:fd:98:00:01",
				Gateways: []string{"192.168.0.1"},
				Routes: []podnetworkapi.PodRoute{
					{Dest: "192.168.1.0/24", NextHop: "192.168.1."},
				},
			}),
			errMatch: fmt.Errorf("failed to parse pod route next hop"),
		},
		{
			desc: "verify error thrown where pod route has next hop of different family",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				MAC:      "0a:58:fd:98:00:01",
				Gateways: []string{"192.168.0.1"},
				Routes: []podnetworkapi.PodRoute{
					{Dest: "fd01::1234/64", NextHop: "192.168.1.1"},
				},
			}),
			errAssert: true,
		},
		{
			desc: "verify successful unmarshal of pod annotation",
			podNetwork: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs:      []string{"192.168.0.5/24"},
				MAC:      "0a:58:fd:98:00:01",
				Gateways: []string{"192.168.0.1"},
				Routes: []podnetworkapi.PodRoute{
					{Dest: "192.168.1.0/24", NextHop: "192.168.1.1"},
				},
			}),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := ParsePodNetwork(tc.podNetwork, types.DefaultNetworkName)
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
		inpPodNet *podnetworkapi.PodNetwork
		errAssert bool
		errMatch  error
		outExp    []net.IP
	}{
		{
			desc:      "test when podNetwork is nil",
			inpPod:    &v1.Pod{},
			inpPodNet: nil,
			errMatch:  ErrNoPodIPFound,
		},
		{
			desc:   "test when pod annotation is non-nil",
			inpPod: &v1.Pod{},
			inpPodNet: getPodNetwork(podnetworkapi.OVNNetwork{
				IPs: []string{"192.168.0.1/24"},
				MAC: "0a:58:fd:98:00:01",
			}),
			outExp: []net.IP{ovntest.MustParseIP("192.168.0.1")},
		},
		{
			desc:      "test when pod.status.PodIP is empty",
			inpPod:    &v1.Pod{},
			inpPodNet: &podnetworkapi.PodNetwork{},
			errMatch:  ErrNoPodIPFound,
		},
		{
			desc: "test when pod.status.PodIP is non-empty",
			inpPod: &v1.Pod{
				Status: v1.PodStatus{
					PodIP: "192.168.1.15",
				},
			},
			inpPodNet: nil,
			outExp:    []net.IP{ovntest.MustParseIP("192.168.1.15")},
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
			inpPodNet: nil,
			outExp:    []net.IP{ovntest.MustParseIP("192.168.1.15")},
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
			res1, e := GetPodIPsOfNetwork(tc.inpPodNet, tc.inpPod, nil)
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
			res2, e := GetPodCIDRsWithFullMask(tc.inpPodNet, tc.inpPod)
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
