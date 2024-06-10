package ovn

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta2"
)

var _ = Describe("convertMultiNetPolicyToNetPolicy", func() {
	const policyName = "pol33"

	It("translates an IPAM policy with ingress namespace selectors", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressNamespaceSelector(policyName), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: policyName},
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{
							{
								From:  []netv1.NetworkPolicyPeer{{NamespaceSelector: sameLabelsEverywhere()}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						Egress:      []netv1.NetworkPolicyEgressRule{},
						PolicyTypes: []netv1.PolicyType{},
					},
				}))
	})

	It("translates an IPAM policy with egress namespace selectors", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressNamespaceSelector(policyName), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: policyName},
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{},
						Egress: []netv1.NetworkPolicyEgressRule{
							{
								To:    []netv1.NetworkPolicyPeer{{NamespaceSelector: sameLabelsEverywhere()}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						PolicyTypes: []netv1.PolicyType{},
					},
				}))
	})

	It("translates an IPAM policy with ingress pod selectors", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressPodSelector(policyName), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: policyName},
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{
							{
								From:  []netv1.NetworkPolicyPeer{{PodSelector: sameLabelsEverywhere()}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						Egress:      []netv1.NetworkPolicyEgressRule{},
						PolicyTypes: []netv1.PolicyType{},
					},
				}))
	})

	It("translates an IPAM policy with egress pod selectors", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressPodSelector(policyName), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: policyName},
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{},
						Egress: []netv1.NetworkPolicyEgressRule{
							{
								To:    []netv1.NetworkPolicyPeer{{PodSelector: sameLabelsEverywhere()}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						PolicyTypes: []netv1.PolicyType{},
					},
				}))
	})

	It("translates an IPAM policy with ingress `ipBlock` rule", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressIPBlock(), allowPeerSelectors)).To(Equal(
			&netv1.NetworkPolicy{
				Spec: netv1.NetworkPolicySpec{
					Ingress: []netv1.NetworkPolicyIngressRule{
						{
							From:  []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "10.10.0.0/16"}}},
							Ports: []netv1.NetworkPolicyPort{},
						},
					},
					Egress:      []netv1.NetworkPolicyEgressRule{},
					PolicyTypes: []netv1.PolicyType{},
				},
			},
		))
	})

	It("translates an IPAM policy with egress `ipBlock` rule", func() {
		allowPeerSelectors := true
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressIPBlock(), allowPeerSelectors)).To(Equal(
			&netv1.NetworkPolicy{
				Spec: netv1.NetworkPolicySpec{
					Ingress: []netv1.NetworkPolicyIngressRule{},
					Egress: []netv1.NetworkPolicyEgressRule{
						{
							To:    []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "10.10.0.0/16"}}},
							Ports: []netv1.NetworkPolicyPort{},
						},
					},
					PolicyTypes: []netv1.PolicyType{},
				},
			},
		))
	})

	It("translates an IPAM-less policy with ingress `ipBlock` rule", func() {
		allowPeerSelectors := false
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressIPBlock(), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{
							{
								From:  []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "10.10.0.0/16"}}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						Egress:      []netv1.NetworkPolicyEgressRule{},
						PolicyTypes: []netv1.PolicyType{},
					},
				},
			))
	})

	It("translates an IPAM-less policy with egress `ipBlock` rule", func() {
		allowPeerSelectors := false
		Expect(convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressIPBlock(), allowPeerSelectors)).To(
			Equal(
				&netv1.NetworkPolicy{
					Spec: netv1.NetworkPolicySpec{
						Ingress: []netv1.NetworkPolicyIngressRule{},
						Egress: []netv1.NetworkPolicyEgressRule{
							{
								To:    []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: "10.10.0.0/16"}}},
								Ports: []netv1.NetworkPolicyPort{},
							},
						},
						PolicyTypes: []netv1.PolicyType{},
					},
				},
			))
	})

	It("*fails* to translate an IPAM-less policy with ingress pod selector peers", func() {
		allowPeerSelectors := false
		_, err := convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressPodSelector(policyName), allowPeerSelectors)
		Expect(err).To(HaveOccurred())
	})

	It("*fails* to translate an IPAM-less policy with egress pod selector peers", func() {
		allowPeerSelectors := false
		_, err := convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressPodSelector(policyName), allowPeerSelectors)
		Expect(err).To(HaveOccurred())
	})

	It("*fails* translates an IPAM-less policy with ingress namespace selector peers", func() {
		allowPeerSelectors := false
		_, err := convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIngressNamespaceSelector(policyName), allowPeerSelectors)
		Expect(err).To(HaveOccurred())
	})

	It("*fails* translates an IPAM-less policy with egress namespace selector peers", func() {
		allowPeerSelectors := false
		_, err := convertMultiNetPolicyToNetPolicy(multiNetPolicyWithEgressNamespaceSelector(policyName), allowPeerSelectors)
		Expect(err).To(HaveOccurred())
	})
})

func sameLabelsEverywhere() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{"George": "Costanza"},
	}
}

func multiNetPolicyWithIngressIPBlock() *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		Spec: mnpapi.MultiNetworkPolicySpec{
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &mnpapi.IPBlock{
								CIDR: "10.10.0.0/16",
							},
						},
					},
				},
			},
		},
	}
}

func multiNetPolicyWithEgressIPBlock() *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		Spec: mnpapi.MultiNetworkPolicySpec{
			Egress: []mnpapi.MultiNetworkPolicyEgressRule{
				{
					To: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &mnpapi.IPBlock{
								CIDR: "10.10.0.0/16",
							},
						},
					},
				},
			},
		},
	}
}

func multiNetPolicyWithIngressPodSelector(policyName string) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: mnpapi.MultiNetworkPolicySpec{
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					From: []mnpapi.MultiNetworkPolicyPeer{{PodSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}

func multiNetPolicyWithEgressPodSelector(policyName string) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: mnpapi.MultiNetworkPolicySpec{
			Egress: []mnpapi.MultiNetworkPolicyEgressRule{
				{
					To: []mnpapi.MultiNetworkPolicyPeer{{PodSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}

func multiNetPolicyWithIngressNamespaceSelector(policyName string) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: mnpapi.MultiNetworkPolicySpec{
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					From: []mnpapi.MultiNetworkPolicyPeer{{NamespaceSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}

func multiNetPolicyWithEgressNamespaceSelector(policyName string) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: mnpapi.MultiNetworkPolicySpec{
			Egress: []mnpapi.MultiNetworkPolicyEgressRule{
				{
					To: []mnpapi.MultiNetworkPolicyPeer{{NamespaceSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}
