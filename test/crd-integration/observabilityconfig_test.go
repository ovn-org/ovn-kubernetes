// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
)

var _ = Describe("ObservabilityConfig CRD", func() {
	// observabilityConfig builds a minimal valid ObservabilityConfig with the given
	// features and optional namespaces filter.
	observabilityConfig := func(namespaces []string, features ...observabilityconfigv1alpha1.ObservabilityFeature) *observabilityconfigv1alpha1.ObservabilityConfig {
		featureConfigs := make([]observabilityconfigv1alpha1.FeatureConfig, 0, len(features))
		for _, f := range features {
			featureConfigs = append(featureConfigs, observabilityconfigv1alpha1.FeatureConfig{Feature: f, Probability: 100})
		}
		cfg := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "test-observabilityconfig-"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: 1,
				Features:    featureConfigs,
			},
		}
		if namespaces != nil {
			cfg.Spec.Filter = &observabilityconfigv1alpha1.Filter{Namespaces: namespaces}
		}
		return cfg
	}

	Context("filter.namespaces with cluster-scoped features", func() {
		It("accepts namespaces with only namespaced features", func() {
			cfg := observabilityConfig(
				[]string{"frontend", "backend"},
				observabilityconfigv1alpha1.NetworkPolicy,
				observabilityconfigv1alpha1.EgressFirewall,
			)
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
		})

		It("accepts a cluster-scoped feature when no namespaces filter is set", func() {
			cfg := observabilityConfig(nil, observabilityconfigv1alpha1.AdminNetworkPolicy)
			Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
		})

		It("rejects namespaces combined with a cluster-scoped feature", func() {
			cfg := observabilityConfig(
				[]string{"frontend"},
				observabilityconfigv1alpha1.NetworkPolicy,
				observabilityconfigv1alpha1.AdminNetworkPolicy,
			)
			err := k8sClient.Create(ctx, cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("filter.namespaces can only be used with namespaced features"))
		})
	})
})
