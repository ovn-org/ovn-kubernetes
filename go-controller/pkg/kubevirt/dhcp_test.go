package kubevirt

import (
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

var _ = Describe("Kubevirt", func() {
	type dhcpTest struct {
		cidrs                             []*net.IPNet
		controllerName, namespace, vmName string
		expectedDHCPConfigs               []nbdb.DHCPOptions
		expectedError                     string
	}

	const (
		controllerName = "cont1"
		namespace      = "ns1"
		v4CIDR         = "10.10.10.0/24"
		v6CIDR         = "fd10:1234::/64"
		vmName         = "vm1"
	)

	var (
		ipv4CIDR = net.IPNet{
			IP:   net.ParseIP("10.10.10.0"),
			Mask: net.CIDRMask(24, 32),
		}
		ipv6CIDR = net.IPNet{
			IP:   net.ParseIP("fd10:1234::"),
			Mask: net.CIDRMask(64, 128),
		}
	)

	DescribeTable("mandatory DHCP options", func(test dhcpTest) {
		Expect(*newDHCPOptionsForVM(test.controllerName, ktypes.NamespacedName{
			Namespace: test.namespace,
			Name:      test.vmName,
		}, test.cidrs[0])).To(Equal(test.expectedDHCPConfigs[0]))
	},
		Entry("for an IPv4 CIDR", dhcpTest{
			cidrs:          []*net.IPNet{&ipv4CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{{
				Cidr: v4CIDR,
				ExternalIDs: map[string]string{
					"k8s.ovn.org/owner-controller": "cont1",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "ns1/vm1",
					"k8s.ovn.org/cidr":             "10.10.10.0/24",
					"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:10.10.10.0/24",
					"k8s.ovn.org/zone":             "local",
				},
				Options: map[string]string{
					"server_id":  "169.254.1.1",
					"server_mac": "0a:58:a9:fe:01:01",
					"lease_time": "3500",
				},
			}},
		}),
		Entry("for an IPv6 CIDR", dhcpTest{
			cidrs:          []*net.IPNet{&ipv6CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{{
				Cidr: v6CIDR,
				ExternalIDs: map[string]string{
					"k8s.ovn.org/owner-controller": "cont1",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "ns1/vm1",
					"k8s.ovn.org/cidr":             "fd10.1234../64",
					"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:fd10.1234../64",
					"k8s.ovn.org/zone":             "local",
				},
				Options: map[string]string{
					"server_id":  "fe80::1",
					"server_mac": "0a:58:a9:fe:01:01",
					"lease_time": "3500",
				},
			}},
		}),
	)

	DescribeTable("DHCP configs with options", func(test dhcpTest, opts ...Option) {
		Expect(
			*newDHCPOptionsForVM(
				test.controllerName,
				ktypes.NamespacedName{
					Namespace: test.namespace,
					Name:      test.vmName,
				},
				test.cidrs[0],
				opts...,
			)).To(Equal(test.expectedDHCPConfigs[0]))
	},
		Entry("with DNS option", dhcpTest{
			cidrs:          []*net.IPNet{&ipv4CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{
				{
					Cidr: v4CIDR,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "cont1",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "ns1/vm1",
						"k8s.ovn.org/cidr":             "10.10.10.0/24",
						"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:10.10.10.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "169.254.1.1",
						"server_mac": "0a:58:a9:fe:01:01",
						"lease_time": "3500",
						"dns_server": "10.10.10.10",
					},
				},
			},
		}, WithDNSServer("10.10.10.10")),
		Entry("with hostname option", dhcpTest{
			cidrs:          []*net.IPNet{&ipv4CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{
				{
					Cidr: v4CIDR,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "cont1",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "ns1/vm1",
						"k8s.ovn.org/cidr":             "10.10.10.0/24",
						"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:10.10.10.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "169.254.1.1",
						"server_mac": "0a:58:a9:fe:01:01",
						"lease_time": "3500",
						"hostname":   "\"host123\"",
					},
				},
			},
		}, WithHostname("host123")),
		Entry("with MTU option", dhcpTest{
			cidrs:          []*net.IPNet{&ipv4CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{
				{
					Cidr: v4CIDR,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "cont1",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "ns1/vm1",
						"k8s.ovn.org/cidr":             "10.10.10.0/24",
						"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:10.10.10.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "169.254.1.1",
						"server_mac": "0a:58:a9:fe:01:01",
						"lease_time": "3500",
						"mtu":        "9000",
					},
				},
			},
		}, WithMTU(9000)),
		Entry("with gateway option", dhcpTest{
			cidrs:          []*net.IPNet{&ipv4CIDR},
			controllerName: controllerName,
			namespace:      namespace,
			vmName:         vmName,
			expectedDHCPConfigs: []nbdb.DHCPOptions{
				{
					Cidr: v4CIDR,
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "cont1",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "ns1/vm1",
						"k8s.ovn.org/cidr":             "10.10.10.0/24",
						"k8s.ovn.org/id":               "cont1:VirtualMachine:ns1/vm1:10.10.10.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "169.254.1.1",
						"server_mac": "0a:58:a9:fe:01:01",
						"lease_time": "3500",
						"router":     "123.123.123.123",
					},
				},
			},
		}, WithGateway("123.123.123.123")),
	)
})
