// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls_test

import (
	"crypto/tls"

	ovntls "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NewApplyConfigOptions", func() {
	assertApplySuccess := func(minVersion string, ciperSuites []string, curvePreferences []int) *tls.Config {
		applyOpts, err := ovntls.NewApplyConfigOptions(minVersion, ciperSuites, curvePreferences)
		Expect(err).ToNot(HaveOccurred())
		Expect(applyOpts).ToNot(BeNil())

		cfg := &tls.Config{}
		applyOpts(cfg)

		return cfg
	}

	Context("with valid inputs", func() {
		It("should correctly apply the settings", func() {
			cfg := assertApplySuccess("VersionTLS12", []string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
			}, []int{int(tls.CurveP256), int(tls.CurveP384)})
			Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
			Expect(cfg.CipherSuites).To(ConsistOf(tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384))
			Expect(cfg.CurvePreferences).To(ConsistOf(tls.CurveP256, tls.CurveP384))
		})
	})

	Context("with empty cipher suites and curve preferences", func() {
		It("should apply empty lists", func() {
			cfg := assertApplySuccess("VersionTLS13", []string{}, []int{})
			Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS13)))
			Expect(cfg.CipherSuites).To(BeEmpty())
			Expect(cfg.CurvePreferences).To(BeEmpty())
		})
	})

	Context("with nil cipher suites and curve preferences", func() {
		It("should apply empty lists", func() {
			cfg := assertApplySuccess("VersionTLS11", nil, nil)
			Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS11)))
			Expect(cfg.CipherSuites).To(BeEmpty())
			Expect(cfg.CurvePreferences).To(BeEmpty())
		})
	})

	Context("with empty min version", func() {
		It("should apply the default min version", func() {
			cfg := assertApplySuccess("", []string{}, []int{})
			Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
		})
	})

	DescribeTableSubtree("with invalid",
		func(minVersion string, ciperSuites []string, curvePreferences []int) {
			It("should return an error", func() {
				applyOpts, err := ovntls.NewApplyConfigOptions(minVersion, ciperSuites, curvePreferences)
				Expect(err).To(HaveOccurred())
				Expect(applyOpts).To(BeNil())
			})
		},
		Entry("TLS version string", "InvalidTLSVersion", []string{}, []int{}),
		Entry("cipher suite name", "VersionTLS12", []string{
			"TLS_AES_128_GCM_SHA256",
			"InvalidCipherSuite",
			"TLS_AES_256_GCM_SHA384"}, []int{}),
		Entry("curve ID", "VersionTLS12", []string{}, []int{int(tls.CurveP256), 999}),
		Entry("curve ID, out of range", "VersionTLS12", []string{}, []int{1<<32 + 23}),
	)
})
