// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("NodePortAddressConfig", func() {
	ginkgo.It("treats empty configuration as unrestricted", func() {
		cfg, err := ParseNodePortAddresses("")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "empty nodeport-addresses should parse successfully")
		gomega.Expect(cfg.Restricted()).To(gomega.BeFalse(), "empty configuration should not restrict NodePort addresses")

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		gomega.Expect(cfg.FilterHostAddresses(hosts, nil)).To(gomega.Equal(hosts), "unrestricted config should return all host addresses")
	})

	ginkgo.It("filters host addresses by CIDR", func() {
		cfg, err := ParseNodePortAddresses("10.0.0.0/8")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "valid IPv4 CIDR should parse successfully")
		gomega.Expect(cfg.Restricted()).To(gomega.BeTrue(), "CIDR configuration should restrict NodePort addresses")

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		filtered := cfg.FilterHostAddresses(hosts, nil)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{net.ParseIP("10.0.0.5")}), "only addresses in the configured CIDR should remain")
	})

	ginkgo.It("filters host addresses by IPv6 CIDR", func() {
		cfg, err := ParseNodePortAddresses("2001:db8::/32")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "valid IPv6 CIDR should parse successfully")

		hosts := []net.IP{net.ParseIP("2001:db8::5"), net.ParseIP("2001:db9::5")}
		filtered := cfg.FilterHostAddresses(hosts, nil)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{net.ParseIP("2001:db8::5")}), "only IPv6 addresses in the configured CIDR should remain")
	})

	ginkgo.It("uses primary addresses when configured", func() {
		cfg, err := ParseNodePortAddresses("primary")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "primary selector should parse successfully")

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		primary := []net.IP{net.ParseIP("10.0.0.5")}
		filtered := cfg.FilterHostAddresses(hosts, primary)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{net.ParseIP("10.0.0.5")}), "only primary addresses should remain")
	})

	ginkgo.It("returns no addresses when nothing matches", func() {
		cfg, err := ParseNodePortAddresses("172.16.0.0/12")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "valid CIDR should parse successfully")

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5")}
		filtered := cfg.FilterHostAddresses(hosts, nil)
		gomega.Expect(filtered).To(gomega.BeEmpty(), "no host addresses should match an unrelated CIDR")
	})

	ginkgo.It("combines primary and CIDR selectors", func() {
		cfg, err := ParseNodePortAddresses("primary,192.168.0.0/16")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "combined selectors should parse successfully")

		hosts := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.5"), net.ParseIP("172.16.0.5")}
		primary := []net.IP{net.ParseIP("10.0.0.5")}
		filtered := cfg.FilterHostAddresses(hosts, primary)
		gomega.Expect(filtered).To(gomega.Equal([]net.IP{
			net.ParseIP("10.0.0.5"),
			net.ParseIP("192.168.1.5"),
		}), "primary and CIDR selectors should both contribute allowed addresses")
	})

	ginkgo.It("preserves host address order in filtered results", func() {
		cfg, err := ParseNodePortAddresses("10.0.0.0/8,192.168.0.0/16")
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "multiple CIDRs should parse successfully")

		hosts := []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("10.0.0.5")}
		filtered := cfg.FilterHostAddresses(hosts, nil)
		gomega.Expect(filtered).To(gomega.Equal(hosts), "filtered addresses should preserve the original host order")
	})

	ginkgo.It("rejects invalid CIDR values", func() {
		_, err := ParseNodePortAddresses("not-a-cidr")
		gomega.Expect(err).To(gomega.HaveOccurred(), "invalid CIDR values should be rejected")
	})

	ginkgo.It("rejects empty-only configuration", func() {
		_, err := ParseNodePortAddresses(", , ")
		gomega.Expect(err).To(gomega.HaveOccurred(), "comma-only configuration should be rejected")
	})
})
