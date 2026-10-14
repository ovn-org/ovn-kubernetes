// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
})
