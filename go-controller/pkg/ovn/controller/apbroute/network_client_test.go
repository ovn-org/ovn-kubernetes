// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package apbroute

import (
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("northBoundClient", func() {
	It("skips deleting pod SNAT for a remote node without looking up the Node", func() {
		nb := &northBoundClient{nodeName: "local-node"}

		Expect(nb.deletePodSNAT("remote-node", "GR_remote-node", nil, nil)).To(Succeed())
	})

	// The predicate below mirrors createOrUpdateBFDStaticRoute's route-lookup predicate.
	DescribeTable("compares route Policy by value in the lookup predicate",
		func(policy1, policy2 *nbdb.LogicalRouterStaticRoutePolicy, expected bool) {
			route1 := &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policy1,
			}
			route2 := &nbdb.LogicalRouterStaticRoute{
				IPPrefix:   "10.128.2.25/32",
				Nexthop:    "10.74.237.104",
				OutputPort: stringPtr("rtoe-GR_test-worker"),
				Policy:     policy2,
			}

			p := func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == route2.IPPrefix &&
					item.Nexthop == route2.Nexthop &&
					item.OutputPort != nil &&
					*item.OutputPort == *route2.OutputPort &&
					libovsdbops.PolicyEqualPredicate(item.Policy, route2.Policy)
			}

			Expect(p(route1)).To(Equal(expected))
		},
		// Each Entry allocates its own Policy pointers via policyPtr so the predicate
		// is exercised on value equality, not on reused pointers matching trivially.
		Entry("same policy values but different pointers",
			policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP), policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP), true),
		Entry("both policies are nil",
			(*nbdb.LogicalRouterStaticRoutePolicy)(nil), (*nbdb.LogicalRouterStaticRoutePolicy)(nil), true),
		Entry("nil policy equals DstIP policy",
			(*nbdb.LogicalRouterStaticRoutePolicy)(nil), policyPtr(nbdb.LogicalRouterStaticRoutePolicyDstIP), true),
		Entry("different policy values",
			policyPtr(nbdb.LogicalRouterStaticRoutePolicySrcIP), policyPtr(nbdb.LogicalRouterStaticRoutePolicyDstIP), false),
	)
})

func stringPtr(s string) *string {
	return &s
}

func policyPtr(p nbdb.LogicalRouterStaticRoutePolicy) *nbdb.LogicalRouterStaticRoutePolicy {
	return &p
}
