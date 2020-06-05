package util

import (
	"fmt"
	"net"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Pod annotation tests", func() {
	It("marshals network info to pod annotations", func() {
		type testcase struct {
			name         string
			in           *PodAnnotation
			out          map[string]string
			unmarshalErr error
		}

		testcases := []testcase{
			{
				name: "Single-stack IPv4",
				in: &PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`,
				},
			},
			{
				name: "No GW",
				in: &PodAnnotation{
					IPs: ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC: ovntest.MustParseMAC("0A:58:FD:98:00:01"),
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`,
				},
			},
			{
				name: "Nil entry in GW",
				in: &PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: []net.IP{nil},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["\u003cnil\u003e"],"ip_address":"192.168.0.5/24","gateway_ip":"\u003cnil\u003e"}}`,
				},
				unmarshalErr: fmt.Errorf(`failed to parse pod gateway "<nil>"`),
			},
			{
				name: "Routes",
				in: &PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.0.1"),
					Routes: []PodRoute{
						{
							Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
							NextHop: net.ParseIP("192.168.1.1"),
						},
					},
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`,
				},
			},
			{
				name: "Single-stack IPv6",
				in: &PodAnnotation{
					IPs:      ovntest.MustParseIPNets("fd01::1234/64"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("fd01::1"),
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["fd01::1"],"ip_address":"fd01::1234/64","gateway_ip":"fd01::1"}}`,
				},
			},
			{
				name: "Dual-stack",
				in: &PodAnnotation{
					IPs:      ovntest.MustParseIPNets("192.168.0.5/24", "fd01::1234/64"),
					MAC:      ovntest.MustParseMAC("0A:58:FD:98:00:01"),
					Gateways: ovntest.MustParseIPs("192.168.1.0", "fd01::1"),
				},
				out: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24","fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.1.0","fd01::1"]}}`,
				},
			},
		}

		for _, tc := range testcases {
			marshalled, err := MarshalPodAnnotation(tc.in)
			Expect(err).NotTo(HaveOccurred(), "test case %q got unexpected marshalling error", tc.name)
			Expect(marshalled).To(Equal(tc.out), "test case %q marshalled to wrong value", tc.name)
			unmarshalled, err := UnmarshalPodAnnotation(marshalled)
			if tc.unmarshalErr == nil {
				Expect(err).NotTo(HaveOccurred(), "test case %q got unexpected unmarshalling error", tc.name)
				Expect(unmarshalled).To(Equal(tc.in), "test case %q unmarshalled to wrong value", tc.name)
			} else {
				Expect(err).To(Equal(tc.unmarshalErr))
			}
		}
	})

	It("return all pod IPs", func() {
		type testcase struct {
			name string
			in   *v1.Pod
			out  []net.IP
			err  error
		}

		testcases := []testcase{
			// Pod IPs are wrong on purpose to verify that the order of precedence is correct:
			// 1. annotations
			// 2. Pod.Status.PodIPs
			// 3. Pod.Status.PodIP
			{
				name: "Single-stack IPv4 with annotations",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`,
						},
					},
					Status: v1.PodStatus{
						PodIP:  "192.168.0.6",
						PodIPs: []v1.PodIP{{"192.168.0.7"}},
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.5")},
			},
			{
				name: "Single-stack IPv4 without annotation and PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP:  "192.168.0.6",
						PodIPs: []v1.PodIP{{"192.168.0.7"}},
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.7")},
			},
			{
				name: "Single-stack IPv4 without annotation and no PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP: "192.168.0.6",
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.6")},
			},
			{
				name: "Single-stack IPv6 with annotations",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["fd01::1"],"ip_address":"fd01::1234/64","gateway_ip":"fd01::1"}}`,
						},
					},
					Status: v1.PodStatus{
						PodIP:  "fd01::1236",
						PodIPs: []v1.PodIP{{"fd01::1237"}},
					},
				},
				out: []net.IP{net.ParseIP("fd01::1234")},
			},
			{
				name: "Single-stack IPv6 without annotation and PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP:  "fd01::1236",
						PodIPs: []v1.PodIP{{"fd01::1237"}},
					},
				},
				out: []net.IP{net.ParseIP("fd01::1237")},
			},
			{
				name: "Single-stack IPv6 without annotation and no PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP: "fd01::1236",
					},
				},
				out: []net.IP{net.ParseIP("fd01::1236")},
			},
			{
				name: "Dual-stack with annotations",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24","fd01::1234/64"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.1.0","fd01::1"]}}`,
						},
					},
					Status: v1.PodStatus{
						PodIP:  "192.168.0.6",
						PodIPs: []v1.PodIP{{"192.168.0.7"}, {"fd01::1237"}},
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.5"), net.ParseIP("fd01::1234")},
			},
			{
				name: "Dual-stack with annotations without annotation and PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP:  "192.168.0.6",
						PodIPs: []v1.PodIP{{"192.168.0.7"}, {"fd01::1237"}},
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.7"), net.ParseIP("fd01::1237")},
			},
			{
				name: "Dual-stack with annotations without annotation and no PodIPs",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
					Status: v1.PodStatus{
						PodIP: "192.168.0.6",
					},
				},
				out: []net.IP{net.ParseIP("192.168.0.6")},
			},
			{
				name: "no annotations, neither PodIPs nor PodIP",
				in: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pod",
					},
				},
				err: fmt.Errorf("no pod IPs found on pod test-pod: could not find OVN pod annotation in map[]"),
			},
		}

		for _, tc := range testcases {
			ips, err := GetAllPodIPs(tc.in)
			if tc.err == nil {
				Expect(err).NotTo(HaveOccurred(), "test case %q got unexpected error getting IPs from Pod", tc.name)
				Expect(ips).To(Equal(tc.out), "test case %q returned wrong IPs", tc.name)
			} else {
				Expect(err).To(Equal(tc.err))
			}
		}
	})

})
