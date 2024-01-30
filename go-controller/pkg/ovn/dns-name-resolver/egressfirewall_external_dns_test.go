package dnsnameresolver

import (
	"net"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("Egress Firewall External DNS Operations", func() {
	var (
		extEgDNS            *ExternalEgressDNS
		_, clusterSubnet, _ = net.ParseCIDR("10.128.0.0/14")
	)

	const (
		dnsName   = "www.example.com."
		namespace = "namespace1"
	)

	ginkgo.BeforeEach(func() {
		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}
		fakeAddressSetFactory := addressset.NewFakeAddressSetFactory(DefaultNetworkControllerName)

		extEgDNS = NewExternalEgressDNS(fakeAddressSetFactory, DefaultNetworkControllerName)
	})

	ginkgo.Context("on Add", func() {
		ginkgo.It("Should add IPv4 addresses", func() {
			config.IPv4Mode = true
			config.IPv6Mode = false

			addresses := []string{"1.1.1.1", "2.2.2.2"}
			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})

		ginkgo.It("Should add IPv6 address", func() {
			config.IPv4Mode = false
			config.IPv6Mode = true

			addresses := []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})

		ginkgo.It("Should support dual stack", func() {
			config.IPv4Mode = true
			config.IPv6Mode = true

			addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})

		ginkgo.It("Should only add supported IPv4 addresses", func() {
			config.IPv4Mode = true
			config.IPv6Mode = false

			ipv4Addresses := []string{"1.1.1.1", "2.2.2.2"}
			ipv6Addresses := []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			addresses := append(ipv4Addresses, ipv6Addresses...)

			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range ipv4Addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})

		ginkgo.It("Should only add supported IPv6 addresses", func() {
			config.IPv4Mode = false
			config.IPv6Mode = true

			ipv4Addresses := []string{"1.1.1.1", "2.2.2.2"}
			ipv6Addresses := []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			addresses := append(ipv4Addresses, ipv6Addresses...)

			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range ipv6Addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})

		ginkgo.It("Should not add IP addresses matching cluster subnet", func() {
			config.IPv4Mode = true
			config.IPv6Mode = false

			addresses := []string{"10.128.0.1"}
			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			gomega.Expect(ipStrings).To(gomega.BeEmpty())
		})

		ginkgo.It("Should not add IP addresses matching cluster subnet, should add other IPs", func() {
			config.IPv4Mode = true
			config.IPv6Mode = false

			clusterSubnetIPs := []string{"10.128.0.1"}
			ipv4Addresses := []string{"1.1.1.1", "2.2.2.2"}
			addresses := append(clusterSubnetIPs, ipv4Addresses...)

			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range ipv4Addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())
		})
	})

	ginkgo.Context("on Delete", func() {
		ginkgo.It("Should delete added addresses", func() {
			config.IPv4Mode = true
			config.IPv6Mode = true

			addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
			gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

			v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
			ipStrings := append(v4, v6...)
			netIPs := []net.IP{}
			for _, addr := range ipStrings {
				netIPs = append(netIPs, net.ParseIP(addr))
			}

			ips := []net.IP{}
			for _, addr := range addresses {
				ips = append(ips, net.ParseIP(addr))
			}

			cmpOpts := []cmp.Option{
				cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
			}
			diff := cmp.Diff(netIPs, ips, cmpOpts...)
			gomega.Expect(diff).To(gomega.BeEmpty())

			deleteResp := extEgDNS.Delete(DeleteRequest{DNSName: dnsName})
			gomega.Expect(deleteResp.Err).NotTo(gomega.HaveOccurred())

			getAddrSetResp = extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
			gomega.Expect(getAddrSetResp.Err).To(gomega.HaveOccurred())
		})
	})

	ginkgo.It("Should not delete added addresses if DNS name is still used in a namespace", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true

		addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
		addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
		gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

		getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
		gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

		v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
		ipStrings := append(v4, v6...)
		netIPs := []net.IP{}
		for _, addr := range ipStrings {
			netIPs = append(netIPs, net.ParseIP(addr))
		}

		ips := []net.IP{}
		for _, addr := range addresses {
			ips = append(ips, net.ParseIP(addr))
		}

		cmpOpts := []cmp.Option{
			cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
		}
		diff := cmp.Diff(netIPs, ips, cmpOpts...)
		gomega.Expect(diff).To(gomega.BeEmpty())

		addNamespaceResp := extEgDNS.AddNamespace(AddNamespaceRequest{Namespace: namespace, DNSName: dnsName})
		gomega.Expect(addNamespaceResp.Err).NotTo(gomega.HaveOccurred())

		deleteResp := extEgDNS.Delete(DeleteRequest{DNSName: dnsName})
		gomega.Expect(deleteResp.Err).To(gomega.HaveOccurred())
	})

	ginkgo.It("Should delete added addresses if DNS name is not used in a namespace", func() {
		config.IPv4Mode = true
		config.IPv6Mode = true

		addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
		addResp := extEgDNS.Add(AddRequest{DNSName: dnsName, Addresses: addresses})
		gomega.Expect(addResp.Err).NotTo(gomega.HaveOccurred())

		getAddrSetResp := extEgDNS.GetAddressSet(GetAddressSetRequest{DNSName: dnsName})
		gomega.Expect(getAddrSetResp.Err).NotTo(gomega.HaveOccurred())

		v4, v6 := getAddrSetResp.DNSAddressSet.GetIPs()
		ipStrings := append(v4, v6...)
		netIPs := []net.IP{}
		for _, addr := range ipStrings {
			netIPs = append(netIPs, net.ParseIP(addr))
		}

		ips := []net.IP{}
		for _, addr := range addresses {
			ips = append(ips, net.ParseIP(addr))
		}

		cmpOpts := []cmp.Option{
			cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
		}
		diff := cmp.Diff(netIPs, ips, cmpOpts...)
		gomega.Expect(diff).To(gomega.BeEmpty())

		addNamespaceResp := extEgDNS.AddNamespace(AddNamespaceRequest{Namespace: namespace, DNSName: dnsName})
		gomega.Expect(addNamespaceResp.Err).NotTo(gomega.HaveOccurred())

		removeNamespaceResp := extEgDNS.RemoveNamespace(RemoveNamespaceRequest{Namespace: namespace})
		gomega.Expect(removeNamespaceResp.Err).NotTo(gomega.HaveOccurred())

		deleteResp := extEgDNS.Delete(DeleteRequest{DNSName: dnsName})
		gomega.Expect(deleteResp.Err).NotTo(gomega.HaveOccurred())
	})
})
