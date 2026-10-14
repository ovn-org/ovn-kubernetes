// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	adminpolicybasedroutev1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
)

var _ = Describe("AdminPolicyBasedExternalRoute CRD", func() {
	routeWithStaticHops := func(ips ...string) *adminpolicybasedroutev1.AdminPolicyBasedExternalRoute {
		staticHops := make([]*adminpolicybasedroutev1.StaticHop, len(ips))
		for i, ip := range ips {
			staticHops[i] = &adminpolicybasedroutev1.StaticHop{IP: ip}
		}
		return &adminpolicybasedroutev1.AdminPolicyBasedExternalRoute{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "test-static-hop-"},
			Spec: adminpolicybasedroutev1.AdminPolicyBasedExternalRouteSpec{
				From: adminpolicybasedroutev1.ExternalNetworkSource{
					NamespaceSelector: metav1.LabelSelector{},
				},
				NextHops: adminpolicybasedroutev1.ExternalNextHops{
					StaticHops: staticHops,
				},
			},
		}
	}

	It("accepts IPv4 and IPv6 static hops", func() {
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		DeferCleanup(cancel)

		route := routeWithStaticHops("192.0.2.10", "2001:db8::10")
		Expect(k8sClient.Create(testCtx, route)).To(Succeed(), "failed to create route with IPv4 and IPv6 static hops")
		DeferCleanup(func() {
			Expect(k8sClient.Delete(testCtx, route)).To(Succeed(), "failed to delete route with IPv4 and IPv6 static hops")
		})
	})

	It("rejects trailing text after an IPv6 address", func() {
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		DeferCleanup(cancel)

		err := k8sClient.Create(testCtx, routeWithStaticHops("2001:db8::10invalid"))
		Expect(err).To(MatchError(ContainSubstring("must be a valid IP address")), "expected admission to reject trailing text after an IPv6 address")
	})
})
