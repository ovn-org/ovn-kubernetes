package kubevirt

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

var _ = Describe("Kubevirt", func() {
	type dhcpTest struct {
		cidrs                             []string
		controllerName, namespace, vmName string
		opts                              []DHCPConfigsOpt
		expectedDHCPConfigs               dhcpConfigs
		expectedError                     string
	}
	var (
		key = func(namespace, name string) ktypes.NamespacedName {
			return ktypes.NamespacedName{Namespace: namespace, Name: name}
		}
		parseCIDR = func(cidr string) *net.IPNet {
			_, parsedCIDR, err := net.ParseCIDR(cidr)
			Expect(err).ToNot(HaveOccurred())
			return parsedCIDR
		}
	)
	DescribeTable("composing dhcp options should success", func(t dhcpTest) {
		cidrs := []*net.IPNet{}
		for _, cidr := range t.cidrs {
			cidrs = append(cidrs, parseCIDR(cidr))
		}
		obtaineddhcpConfigs, err := composeDHCPConfigs(t.controllerName, ktypes.NamespacedName{Namespace: t.namespace, Name: t.vmName}, cidrs, t.opts...)
		Expect(err).ToNot(HaveOccurred())
		Expect(obtaineddhcpConfigs.V4).To(Equal(t.expectedDHCPConfigs.V4))
		Expect(obtaineddhcpConfigs.V6).To(Equal(t.expectedDHCPConfigs.V6))
	},
		Entry("IPv4 Single stack and dns", dhcpTest{
			cidrs:          []string{"192.168.25.0/24"},
			controllerName: "defaultController",
			namespace:      "namespace1",
			vmName:         "foo1",
			opts: []DHCPConfigsOpt{
				WithIPv4DNSServer("192.167.23.44"),
				WithIPv4Router("192.168.25.1"),
				WithIPv4MTU(1500),
			},
			expectedDHCPConfigs: dhcpConfigs{
				V4: &nbdb.DHCPOptions{
					Cidr: "192.168.25.0/24",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "192.168.25.0/24",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:192.168.25.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"lease_time": "3500",
						"server_id":  ARPProxyIPv4,
						"server_mac": ARPProxyMAC,
						"hostname":   `"foo1"`,
						"dns_server": "192.167.23.44",
						"router":     "192.168.25.1",
						"mtu":        "1500",
					},
				},
			},
		}),
		Entry("IPv6 Single stack and dns", dhcpTest{
			cidrs:          []string{"2002:0:0:1234::/64"},
			controllerName: "defaultController",
			namespace:      "namespace1",
			vmName:         "foo1",
			opts:           []DHCPConfigsOpt{WithIPv6DNSServer("2001:1:2:3:4:5:6:7")},
			expectedDHCPConfigs: dhcpConfigs{
				V6: &nbdb.DHCPOptions{
					Cidr: "2002:0:0:1234::/64",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "2002.0.0.1234../64",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:2002.0.0.1234../64",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "0a:58:6d:6d:c1:50",
						"fqdn":       `"foo1"`,
						"dns_server": "2001:1:2:3:4:5:6:7",
					},
				},
			},
		}),
		Entry("Dual stack and dns", dhcpTest{
			cidrs:          []string{"192.168.25.0/24", "2002:0:0:1234::/64"},
			controllerName: "defaultController",
			namespace:      "namespace1",
			vmName:         "foo1",
			opts: []DHCPConfigsOpt{
				WithIPv4DNSServer("192.167.23.44"),
				WithIPv6DNSServer("2001:1:2:3:4:5:6:7"),
			},
			expectedDHCPConfigs: dhcpConfigs{
				V4: &nbdb.DHCPOptions{
					Cidr: "192.168.25.0/24",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "192.168.25.0/24",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:192.168.25.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"lease_time": "3500",
						"server_id":  ARPProxyIPv4,
						"server_mac": ARPProxyMAC,
						"hostname":   `"foo1"`,
						"dns_server": "192.167.23.44",
					},
				},
				V6: &nbdb.DHCPOptions{
					Cidr: "2002:0:0:1234::/64",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "2002.0.0.1234../64",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:2002.0.0.1234../64",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id":  "0a:58:6d:6d:c1:50",
						"fqdn":       `"foo1"`,
						"dns_server": "2001:1:2:3:4:5:6:7",
					},
				},
			},
		}),

		Entry("Dual stack with single IPv4 dns server", dhcpTest{
			cidrs:          []string{"192.168.25.0/24", "2002:0:0:1234::/64"},
			controllerName: "defaultController",
			namespace:      "namespace1",
			vmName:         "foo1",
			opts: []DHCPConfigsOpt{
				WithIPv4DNSServer("192.167.23.44"),
				WithIPv6DNSServer(""),
			},
			expectedDHCPConfigs: dhcpConfigs{
				V4: &nbdb.DHCPOptions{
					Cidr: "192.168.25.0/24",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "192.168.25.0/24",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:192.168.25.0/24",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"lease_time": "3500",
						"server_id":  ARPProxyIPv4,
						"server_mac": ARPProxyMAC,
						"hostname":   `"foo1"`,
						"dns_server": "192.167.23.44",
					},
				},
				V6: &nbdb.DHCPOptions{
					Cidr: "2002:0:0:1234::/64",
					ExternalIDs: map[string]string{
						"k8s.ovn.org/owner-controller": "defaultController",
						"k8s.ovn.org/owner-type":       "VirtualMachine",
						"k8s.ovn.org/name":             "namespace1/foo1",
						"k8s.ovn.org/cidr":             "2002.0.0.1234../64",
						"k8s.ovn.org/id":               "defaultController:VirtualMachine:namespace1/foo1:2002.0.0.1234../64",
						"k8s.ovn.org/zone":             "local",
					},
					Options: map[string]string{
						"server_id": "0a:58:6d:6d:c1:50",
						"fqdn":      `"foo1"`,
					},
				},
			},
		}),
	)

	DescribeTable("composing dhcp options should fail", func(t dhcpTest) {
		cidrs := []*net.IPNet{}
		for _, cidr := range t.cidrs {
			cidrs = append(cidrs, parseCIDR(cidr))
		}
		_, err := composeDHCPConfigs(t.controllerName, key(t.namespace, t.vmName), cidrs)
		Expect(err).To(MatchError(t.expectedError))
	},
		Entry("No cidr should fail", dhcpTest{
			expectedError: "missing podIPs to compose dhcp options",
		}),
		Entry("No hostname should fail", dhcpTest{
			vmName:        "",
			cidrs:         []string{"192.168.25.0/24"},
			expectedError: "missing vmName to compose dhcp options",
		}),
	)

})
