// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressfirewallv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
)

var _ = Describe("EgressFirewall CRD", func() {
	egressFirewall := func(ruleType egressfirewallv1.EgressFirewallRuleType, protocol string) *egressfirewallv1.EgressFirewall {
		return &egressfirewallv1.EgressFirewall{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
			Spec: egressfirewallv1.EgressFirewallSpec{
				Egress: []egressfirewallv1.EgressFirewallRule{{
					Type: ruleType,
					Ports: []egressfirewallv1.EgressFirewallPort{{
						Protocol: protocol,
						Port:     443,
					}},
					To: egressfirewallv1.EgressFirewallDestination{CIDRSelector: "192.0.2.0/24"},
				}},
			},
		}
	}

	It("accepts supported rule and protocol values", func() {
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		DeferCleanup(cancel)

		firewall := egressFirewall(egressfirewallv1.EgressFirewallRuleDeny, "SCTP")
		Expect(k8sClient.Create(testCtx, firewall)).To(Succeed(), "failed to create EgressFirewall with supported rule and protocol values")
		DeferCleanup(func() {
			Expect(k8sClient.Delete(testCtx, firewall)).To(Succeed(), "failed to delete EgressFirewall with supported rule and protocol values")
		})
	})

	It("rejects unsupported rule types and protocols", func() {
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		DeferCleanup(cancel)

		firewall := egressFirewall(egressfirewallv1.EgressFirewallRuleType("AllowInvalid"), "TCPInvalid")
		err := k8sClient.Create(testCtx, firewall)
		Expect(err).To(MatchError(And(
			ContainSubstring(`Unsupported value: "AllowInvalid"`),
			ContainSubstring(`Unsupported value: "TCPInvalid"`),
		)), "expected admission to reject unsupported EgressFirewall rule and protocol values")
	})

	It("ratchets enum validation for existing EgressFirewalls", func() {
		testCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		DeferCleanup(cancel)

		var crd apiextensionsv1.CustomResourceDefinition
		crdKey := client.ObjectKey{Name: "egressfirewalls.k8s.ovn.org"}
		Expect(k8sClient.Get(testCtx, crdKey, &crd)).To(Succeed(), "failed to get the EgressFirewall CRD")
		currentSpec := crd.Spec.DeepCopy()

		restoreValidation := func() error {
			var current apiextensionsv1.CustomResourceDefinition
			if err := k8sClient.Get(testCtx, crdKey, &current); err != nil {
				return err
			}
			current.Spec = *currentSpec
			return k8sClient.Update(testCtx, &current)
		}
		DeferCleanup(restoreValidation)

		rule := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["egress"].Items.Schema
		ruleType := rule.Properties["type"]
		ruleType.Enum = nil
		ruleType.Pattern = "^Allow|Deny$"
		rule.Properties["type"] = ruleType
		protocol := rule.Properties["ports"].Items.Schema.Properties["protocol"]
		protocol.Enum = nil
		protocol.Pattern = "^TCP|UDP|SCTP$"
		rule.Properties["ports"].Items.Schema.Properties["protocol"] = protocol
		Expect(k8sClient.Update(testCtx, &crd)).To(Succeed(), "failed to install legacy EgressFirewall validation")

		firewall := egressFirewall(egressfirewallv1.EgressFirewallRuleType("AllowInvalid"), "TCPInvalid")
		firewall.Spec.Egress = append(firewall.Spec.Egress, egressfirewallv1.EgressFirewallRule{
			Type: egressfirewallv1.EgressFirewallRuleDeny,
			To:   egressfirewallv1.EgressFirewallDestination{CIDRSelector: "198.51.100.0/24"},
		})
		Eventually(func() error {
			return k8sClient.Create(testCtx, firewall)
		}).WithTimeout(5*time.Second).WithPolling(100*time.Millisecond).Should(Succeed(), "legacy validation did not accept the previously allowed values")
		DeferCleanup(func() {
			Expect(k8sClient.Delete(testCtx, firewall)).To(Succeed(), "failed to delete the legacy EgressFirewall")
		})

		Expect(restoreValidation()).To(Succeed(), "failed to reinstall EgressFirewall enum validation")
		Eventually(func(g Gomega) {
			candidate := egressFirewall(egressfirewallv1.EgressFirewallRuleType("AllowInvalid"), "TCPInvalid")
			candidate.Name = "new-invalid"
			err := k8sClient.Create(testCtx, candidate, client.DryRunAll)
			g.Expect(err).To(MatchError(ContainSubstring(`Unsupported value: "AllowInvalid"`)))
		}).WithTimeout(5*time.Second).WithPolling(100*time.Millisecond).Should(Succeed(), "enum validation did not reject a new invalid EgressFirewall")

		firewall.Status.Messages = []string{"zone: EgressFirewall Rules applied"}
		Expect(k8sClient.Status().Update(testCtx, firewall)).To(Succeed(), "validation ratcheting blocked an EgressFirewall status update")

		firewall.Spec.Egress[1].To.CIDRSelector = "203.0.113.0/24"
		err := k8sClient.Update(testCtx, firewall)
		Expect(err).To(MatchError(ContainSubstring(`Unsupported value: "AllowInvalid"`)), "expected an update to another rule to revalidate the atomic rule list")
	})
})
