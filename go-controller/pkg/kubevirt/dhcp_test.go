package kubevirt

import (
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("Kubevirt", func() {
	type dhcpTest struct {
		cidrs               []string
		hostname            string
		dns                 *corev1.Service
		expectedIPv4Options *nbdb.DHCPOptions
		expectedIPv6Options *nbdb.DHCPOptions
	}
	var (
		svc = func(namespace string, name string, clusterIPs []string) *corev1.Service {
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "kube-system",
					Name:      "kube-dns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: clusterIPs,
				},
			}
		}
		parseCIDR = func(cidr string) *net.IPNet {
			_, parsedCIDR, err := net.ParseCIDR(cidr)
			Expect(err).ToNot(HaveOccurred())
			return parsedCIDR
		}
	)
	DescribeTable("composing dhcp options should success", func(t dhcpTest) {
		svcs := []corev1.Service{}
		if t.dns != nil {
			svcs = append(svcs, *t.dns)
		}
		fakeClient := &util.OVNMasterClientset{
			KubeClient: fake.NewSimpleClientset(&corev1.ServiceList{
				Items: svcs,
			}),
		}
		watcher, err := factory.NewMasterWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(watcher.Start()).To(Succeed())

		cidrs := []*net.IPNet{}
		for _, cidr := range t.cidrs {
			cidrs = append(cidrs, parseCIDR(cidr))
		}
		dhcpConfig, err := ComposeDHCPConfig(watcher, t.hostname, cidrs)
		Expect(err).ToNot(HaveOccurred())
		Expect(dhcpConfig).ToNot(BeNil())
		Expect(dhcpConfig.V4Options).To(Equal(t.expectedIPv4Options))
		Expect(dhcpConfig.V6Options).To(Equal(t.expectedIPv6Options))
	},
		Entry("IPv4 Single stack and k8s dns", dhcpTest{
			cidrs:               []string{"192.168.25.0/24"},
			hostname:            "foo1",
			dns:                 svc("kube-system", "core-dns", []string{"192.167.23.44"}),
			expectedIPv4Options: ComposeDHCPv4Options("192.168.25.0/24", ARPProxyIPv4, "192.167.23.44", "foo1"),
		}),
		Entry("IPv6 Single stack and k8s dns", dhcpTest{
			cidrs:               []string{"2002:0:0:1234::/64"},
			hostname:            "foo1",
			dns:                 svc("kube-system", "core-dns", []string{"2001:1:2:3:4:5:6:7"}),
			expectedIPv6Options: ComposeDHCPv6Options("2002:0:0:1234::/64", ARPProxyIPv6, "2001:1:2:3:4:5:6:7"),
		}),
		Entry("Dual stack and k8s dns", dhcpTest{
			cidrs:               []string{"192.168.25.0/24", "2002:0:0:1234::/64"},
			hostname:            "foo1",
			dns:                 svc("kube-system", "core-dns", []string{"192.167.23.44", "2001:1:2:3:4:5:6:7"}),
			expectedIPv4Options: ComposeDHCPv4Options("192.168.25.0/24", ARPProxyIPv4, "192.167.23.44", "foo1"),
			expectedIPv6Options: ComposeDHCPv6Options("2002:0:0:1234::/64", ARPProxyIPv6, "2001:1:2:3:4:5:6:7"),
		}),
		Entry("Dual stack and k8s dns with ipv4 only", dhcpTest{
			cidrs:               []string{"192.168.25.0/24", "2002:0:0:1234::/64"},
			hostname:            "foo1",
			dns:                 svc("kube-system", "core-dns", []string{"192.167.23.44", ""}),
			expectedIPv4Options: ComposeDHCPv4Options("192.168.25.0/24", ARPProxyIPv4, "192.167.23.44", "foo1"),
			expectedIPv6Options: &nbdb.DHCPOptions{
				Cidr: "2002:0:0:1234::/64",
				Options: map[string]string{
					"server_id": util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv6)).String(),
				},
			},
		}),

		Entry("IPv4 Single stack and openshift dns", dhcpTest{
			cidrs:               []string{"192.168.25.0/24"},
			hostname:            "foo1",
			dns:                 svc("openshift-dns", "dns-default", []string{"192.167.23.44"}),
			expectedIPv4Options: ComposeDHCPv4Options("192.168.25.0/24", ARPProxyIPv4, "192.167.23.44", "foo1"),
		}),
		Entry("IPv6 Single stack and openshift dns", dhcpTest{
			cidrs:               []string{"2002:0:0:1234::/64"},
			hostname:            "foo1",
			dns:                 svc("openshift-dns", "dns-default", []string{"2001:1:2:3:4:5:6:7"}),
			expectedIPv6Options: ComposeDHCPv6Options("2002:0:0:1234::/64", ARPProxyIPv6, "2001:1:2:3:4:5:6:7"),
		}),
		Entry("Dual stack and k8s openshift ", dhcpTest{
			cidrs:               []string{"192.168.25.0/24", "2002:0:0:1234::/64"},
			hostname:            "foo1",
			dns:                 svc("openshift-dns", "dns-default", []string{"192.167.23.44", "2001:1:2:3:4:5:6:7"}),
			expectedIPv4Options: ComposeDHCPv4Options("192.168.25.0/24", ARPProxyIPv4, "192.167.23.44", "foo1"),
			expectedIPv6Options: ComposeDHCPv6Options("2002:0:0:1234::/64", ARPProxyIPv6, "2001:1:2:3:4:5:6:7"),
		}),
	)

	DescribeTable("composing dhcp options should fail", func(t dhcpTest) {
		svcs := []corev1.Service{}
		if t.dns != nil {
			svcs = append(svcs, *t.dns)
		}
		fakeClient := &util.OVNMasterClientset{
			KubeClient: fake.NewSimpleClientset(&corev1.ServiceList{
				Items: svcs,
			}),
		}
		watcher, err := factory.NewMasterWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(watcher.Start()).To(Succeed())

		cidrs := []*net.IPNet{}
		for _, cidr := range t.cidrs {
			cidrs = append(cidrs, parseCIDR(cidr))
		}
		_, err = ComposeDHCPConfig(watcher, t.hostname, cidrs)
		Expect(err).To(HaveOccurred())
	},
		Entry("No cidr should fail", dhcpTest{}),
		Entry("No dns should fail", dhcpTest{cidrs: []string{"192.168.3.0/24"}}),
		Entry("No hostname should fail", dhcpTest{
			hostname: "",
			cidrs:    []string{"192.168.25.0/24"},
			dns:      svc("kube-system", "core-dns", []string{"192.167.23.44"}),
		}),
	)

})
