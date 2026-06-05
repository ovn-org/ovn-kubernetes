// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	utilsnet "k8s.io/utils/net"
)

func TestUtilSuite(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "node ip alloc suite")
}

func TestAllocateNext(t *testing.T) {
	tests := []struct {
		desc   string
		input  *net.IPNet
		output []net.IP
	}{
		{
			desc:   "increments IPv4 address",
			input:  mustParseCIDRIncIP("192.168.1.5/16"), // mask /24 would fail
			output: []net.IP{net.ParseIP("192.168.1.6"), net.ParseIP("192.168.1.7"), net.ParseIP("192.168.1.8")},
		},
		{
			desc:   "increments IPv6 address",
			input:  mustParseCIDRIncIP("fc00:f853:ccd:e793::6/64"),
			output: []net.IP{net.ParseIP("fc00:f853:ccd:e793::7"), net.ParseIP("fc00:f853:ccd:e793::8"), net.ParseIP("fc00:f853:ccd:e793::9")},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			nodeIPAlloc := newIPAllocator(tc.input, 0, 255)
			for _, expectedIP := range tc.output {
				allocatedIP, err := nodeIPAlloc.allocateNextIP()
				if err != nil {
					t.Errorf("failed to allocated next IP: %v", err)
				}
				if !allocatedIP.Equal(expectedIP) {
					t.Errorf("Expected IP %q, but got %q", expectedIP.String(), allocatedIP.String())
				}
			}
		})
	}
}

// mustParseCIDRIncIP parses the IP and CIDR. It adds the IP to the returned IPNet.
func mustParseCIDRIncIP(cidr string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse CIDR %q: %v", cidr, err))
	}
	ipNet.IP = ip
	return ipNet
}

type network struct {
	ip   string
	mask string
}

type node struct {
	v4 network
	v6 network
}

func TestIPAlloc(t *testing.T) {
	type expectedAlloc struct {
		subnet     string // subnet to pass for this specific allocation
		expectedIP string
	}
	tests := []struct {
		desc                   string
		existingPrimaryNodeIPs []node
		allocations            []expectedAlloc
	}{
		{
			desc:                   "IPv4",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "192.168.1.1", mask: "16"}}, {v4: network{ip: "192.168.1.2", mask: "16"}}},
			allocations: []expectedAlloc{
				{subnet: "192.168.1.1/16", expectedIP: "192.168.1.200"},
				{subnet: "192.168.1.1/16", expectedIP: "192.168.1.201"},
			},
		},
		{
			desc:                   "IPv6",
			existingPrimaryNodeIPs: []node{{v6: network{ip: "fc00:f853:ccd:e793::5", mask: "64"}}, {v6: network{ip: "fc00:f853:ccd:e793::6", mask: "64"}}},
			allocations: []expectedAlloc{
				{subnet: "fc00:f853:ccd:e793::5/64", expectedIP: "fc00:f853:ccd:e793::c8"},
				{subnet: "fc00:f853:ccd:e793::5/64", expectedIP: "fc00:f853:ccd:e793::c9"},
			},
		},
		{
			desc:                   "IPv4 /24 subnet: no regression from per-subnet refactor",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "192.168.1.1", mask: "24"}}, {v4: network{ip: "192.168.1.2", mask: "24"}}},
			allocations: []expectedAlloc{
				{subnet: "192.168.1.1/24", expectedIP: "192.168.1.200"},
				{subnet: "192.168.1.2/24", expectedIP: "192.168.1.201"},
			},
		},
		{
			desc:                   "IPv4 /16 Kind-like: multiple nodes same /24, shared allocator",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "172.18.0.2", mask: "16"}}, {v4: network{ip: "172.18.0.3", mask: "16"}}, {v4: network{ip: "172.18.0.4", mask: "16"}}},
			allocations: []expectedAlloc{
				{subnet: "172.18.0.2/16", expectedIP: "172.18.0.200"},
				{subnet: "172.18.0.3/16", expectedIP: "172.18.0.201"},
				{subnet: "172.18.0.2/16", expectedIP: "172.18.0.202"},
			},
		},
		{
			desc:                   "IPv4 multi-subnet (Azure-like): each subnet gets its own allocator",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "10.0.0.4", mask: "17"}}, {v4: network{ip: "10.0.128.5", mask: "17"}}},
			allocations: []expectedAlloc{
				{subnet: "10.0.0.4/17", expectedIP: "10.0.0.200"},
				{subnet: "10.0.128.5/17", expectedIP: "10.0.128.200"},
				{subnet: "10.0.0.4/17", expectedIP: "10.0.0.201"},
				{subnet: "10.0.128.5/17", expectedIP: "10.0.128.201"},
			},
		},
		{
			desc:                   "IPv4 /32 host annotations (GCP-like): effective provider subnet is /24",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "10.0.0.4", mask: "32"}}, {v4: network{ip: "10.0.128.5", mask: "32"}}},
			allocations: []expectedAlloc{
				{subnet: "10.0.0.4/24", expectedIP: "10.0.0.200"},
				{subnet: "10.0.128.5/24", expectedIP: "10.0.128.200"},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cs := fake.NewSimpleClientset(getNodesWithIPs(tc.existingPrimaryNodeIPs))
			pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			for j, alloc := range tc.allocations {
				expectedIP := net.ParseIP(alloc.expectedIP)
				var nextIP net.IP
				var err error
				if utilsnet.IsIPv6(expectedIP) {
					nextIP, err = pipa.allocateNextV6(alloc.subnet)
				} else {
					nextIP, err = pipa.allocateNextV4(alloc.subnet)
				}
				if err != nil || nextIP == nil {
					t.Errorf("allocation %d: failed to allocate next IPv4 or IPv6 address. err %v", j, err)
					return
				}
				if !nextIP.Equal(expectedIP) {
					t.Errorf("allocation %d: expected IP %q, but found %q", j, expectedIP, nextIP)
				}
			}
		})
	}

}

// TestIPAllocExhaustion verifies that allocating beyond the reserved range
// (.254 for IPv4, ::ff for IPv6) returns an error instead of returning an
// out-of-range IP like .255 or ::100.
func TestIPAllocExhaustion(t *testing.T) {
	tests := []struct {
		desc           string
		nodes          []node
		subnet         string
		allocCount     int    // number of IPs to allocate to reach the last valid IP
		expectedLastIP string // the last valid IP that should be returned
		isIPv6         bool
	}{
		{
			desc:           "IPv4: allocation after .254 fails",
			nodes:          []node{{v4: network{ip: "192.168.1.1", mask: "16"}}, {v4: network{ip: "192.168.1.2", mask: "16"}}},
			subnet:         "192.168.1.1/16",
			allocCount:     55, // .200-.254 = 55 total
			expectedLastIP: "192.168.1.254",
		},
		{
			desc:           "IPv6: allocation after ::ff fails",
			nodes:          []node{{v6: network{ip: "fc00:f853:ccd:e793::5", mask: "64"}}, {v6: network{ip: "fc00:f853:ccd:e793::6", mask: "64"}}},
			subnet:         "fc00:f853:ccd:e793::5/64",
			allocCount:     56, // ::c8-::ff = 56 total
			expectedLastIP: "fc00:f853:ccd:e793::ff",
			isIPv6:         true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cs := fake.NewSimpleClientset(getNodesWithIPs(tc.nodes))
			pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes(), nil)
			if err != nil {
				t.Fatalf("unexpected init error: %v", err)
			}
			// Allocate all IPs in the reserved range
			var lastIP net.IP
			for i := 0; i < tc.allocCount; i++ {
				if tc.isIPv6 {
					lastIP, err = pipa.allocateNextV6(tc.subnet)
				} else {
					lastIP, err = pipa.allocateNextV4(tc.subnet)
				}
				if err != nil {
					t.Fatalf("unexpected error on allocation %d: %v", i, err)
				}
			}
			expectedIP := net.ParseIP(tc.expectedLastIP)
			if !lastIP.Equal(expectedIP) {
				t.Fatalf("expected last valid IP %s, got %s", expectedIP, lastIP)
			}
			// Next allocation must fail with exhaustion error
			if tc.isIPv6 {
				_, err = pipa.allocateNextV6(tc.subnet)
			} else {
				_, err = pipa.allocateNextV4(tc.subnet)
			}
			if err == nil {
				t.Error("expected reserved range exhaustion error, got nil")
			}
			if err != nil && !strings.Contains(err.Error(), "exhausted") {
				t.Errorf("expected exhaustion error, got: %v", err)
			}
		})
	}
}

func getNodesWithIPs(nodesSpec []node) runtime.Object {
	nodeObjs := make([]corev1.Node, 0, len(nodesSpec))
	getIPMaskFn := func(ip, mask string) string {
		if ip == "" || mask == "" {
			return ""
		}
		return fmt.Sprintf("%s/%s", ip, mask)
	}

	getArrayForHostCIDRs := func(n node) string {
		cidrs := []string{}
		if cidr := getIPMaskFn(n.v4.ip, n.v4.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		if cidr := getIPMaskFn(n.v6.ip, n.v6.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		return fmt.Sprintf("[%s]", strings.Join(cidrs, ","))
	}

	for i, node := range nodesSpec {
		nodePrimaryIfAddrValue := fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
			getIPMaskFn(node.v4.ip, node.v4.mask), getIPMaskFn(node.v6.ip, node.v6.mask))
		node1Annotations := map[string]string{
			"k8s.ovn.org/node-primary-ifaddr": nodePrimaryIfAddrValue,
			util.OVNNodeHostCIDRs:             getArrayForHostCIDRs(node),
		}
		nodeObjs = append(nodeObjs, getNodeObj(fmt.Sprintf("node%d", i), node1Annotations, map[string]string{}))
	}
	nl := &corev1.NodeList{Items: nodeObjs}
	return nl
}

func getNodeObj(nodeName string, annotations, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
			Labels:      labels,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}
