// +build linux

package node

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	kapi "k8s.io/api/core/v1"
	"net"
)

var _ = Describe("Gateway localnet unit tests", func() {

	Context("Tests for number of subnets", func() {
		const (
			v4Nodesubnet1 string = "10.1.1.0/24"
			v6Nodesubnet1 string = "2001:db8:abcd:0012::0/64"
			v4Nodesubnet2 string = "10.2.1.0/24"
			v6Nodesubnet2 string = "2002:db8:abcd:0012::0/64"
		)
		var (
			subnets []*net.IPNet
		)
		BeforeEach(func() {
			subnets = make([]*net.IPNet, 0)
		})

		It("There should be maximum two subnets passed", func() {

			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet2))

			err := evaluateInputNetworks(subnets)
			Expect(err).To(HaveOccurred())
		})

		It("Two subnets of different IP family should pass", func() {

			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))

			err := evaluateInputNetworks(subnets)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Maximum of only one IPv4 subnet", func() {

			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet2))

			err := evaluateInputNetworks(subnets)
			Expect(err).To(HaveOccurred())
		})

		It("Maximum of only one IPv6 subnet", func() {

			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet2))

			err := evaluateInputNetworks(subnets)
			Expect(err).To(HaveOccurred())
		})

	})

	Context("Tests for IPTable helper selection", func() {
		const (
			v4Nodesubnet1 string = "10.1.1.0/24"
			v6Nodesubnet1 string = "2001:db8:abcd:0012::0/64"
		)
		var (
			subnets []*net.IPNet
		)
		BeforeEach(func() {
			subnets = make([]*net.IPNet, 0)
			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
		})

		It("IPv4 cluster IP should result in Ipv4 IPT ", func() {

			ipData, err := constructIPData(subnets)

			Expect(err).NotTo(HaveOccurred())

			service := &kapi.Service{
				Spec: kapi.ServiceSpec{
					ClusterIP: "192.168.1.1",
				},
			}
			localnetdata := getLocalNetDataForService(ipData, service)
			Expect(localnetdata).NotTo(BeNil())
			Expect(localnetdata.ipVersion).To(Equal(4))

		})

		It("IPv6 cluster IP should result in Ipv6 IPT ", func() {

			ipData, err := constructIPData(subnets)

			Expect(err).NotTo(HaveOccurred())

			service := &kapi.Service{
				Spec: kapi.ServiceSpec{
					ClusterIP: "fd99::2",
				},
			}
			localnetdata := getLocalNetDataForService(ipData, service)
			Expect(localnetdata).NotTo(BeNil())
			Expect(localnetdata.ipVersion).To(Equal(6))

		})

	})

})
