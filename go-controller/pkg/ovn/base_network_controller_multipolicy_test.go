package ovn

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	netplumbersv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("convertMultiNetPolicyToNetPolicy", func() {
	const policyName = "pol33"

	var nci *CommonNetworkControllerInfo

	BeforeEach(func() {
		nci = &CommonNetworkControllerInfo{nbClient: nil}
	})

	It("translates an IPAM policy with namespace selectors", func() {
		nInfo, err := util.ParseNADInfo(ipamNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		Expect(bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithNamespaceSelector(policyName))).To(
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

	It("translates an IPAM policy with pod selectors", func() {
		nInfo, err := util.ParseNADInfo(ipamNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		Expect(bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithPodSelector(policyName))).To(
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

	It("translates an IPAM policy with `ipBlock` selectors", func() {
		nInfo, err := util.ParseNADInfo(ipamNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		Expect(bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIPBlock())).To(Equal(
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

	It("translates an IPAM-less policy with `ipBlock` selectors", func() {
		nInfo, err := util.ParseNADInfo(ipamlessNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		Expect(bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithIPBlock())).To(
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

	It("*fails* to translate an IPAM-less policy with pod selector peers", func() {
		nInfo, err := util.ParseNADInfo(ipamlessNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		_, err = bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithPodSelector(policyName))
		Expect(err).To(
			MatchError(
				MatchRegexp(fmt.Sprintf("invalid peer .* in multi-network policy %s; IPAM-less networks can only have `ipBlock` peers", policyName))))
	})

	It("translates an IPAM-less policy with namespace selector peers", func() {
		nInfo, err := util.ParseNADInfo(ipamlessNetAttachDef())
		Expect(err).NotTo(HaveOccurred())
		bnc := NewSecondaryLayer2NetworkController(nci, nInfo)
		_, err = bnc.convertMultiNetPolicyToNetPolicy(multiNetPolicyWithNamespaceSelector(policyName))
		Expect(err).To(MatchError(
			MatchRegexp(fmt.Sprintf("invalid peer .* in multi-network policy %s; IPAM-less networks can only have `ipBlock` peers", policyName))))
	})
})

func sameLabelsEverywhere() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{"George": "Costanza"},
	}
}

func ipamNetAttachDef() *netplumbersv1.NetworkAttachmentDefinition {
	return &netplumbersv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flatl2",
			Namespace: "default",
		},
		Spec: netplumbersv1.NetworkAttachmentDefinitionSpec{
			Config: `{
        "cniVersion": "0.4.0",
        "name": "flatl2",
        "netAttachDefName": "default/flatl2",
        "topology": "layer2",
        "type": "ovn-k8s-cni-overlay",
        "subnets": "192.100.200.0/24"
    }`,
		},
	}
}

func ipamlessNetAttachDef() *netplumbersv1.NetworkAttachmentDefinition {
	return &netplumbersv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flatl2",
			Namespace: "default",
		},
		Spec: netplumbersv1.NetworkAttachmentDefinitionSpec{
			Config: `{
        "cniVersion": "0.4.0",
        "name": "flatl2",
        "netAttachDefName": "default/flatl2",
        "topology": "layer2",
        "type": "ovn-k8s-cni-overlay"
    }`,
		},
	}
}
func multiNetPolicyWithIPBlock() *v1beta1.MultiNetworkPolicy {
	return &v1beta1.MultiNetworkPolicy{
		Spec: v1beta1.MultiNetworkPolicySpec{
			Ingress: []v1beta1.MultiNetworkPolicyIngressRule{
				{
					From: []v1beta1.MultiNetworkPolicyPeer{
						{
							IPBlock: &v1beta1.IPBlock{
								CIDR: "10.10.0.0/16",
							},
						},
					},
				},
			},
		},
	}
}

func multiNetPolicyWithPodSelector(policyName string) *v1beta1.MultiNetworkPolicy {
	return &v1beta1.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: v1beta1.MultiNetworkPolicySpec{
			Ingress: []v1beta1.MultiNetworkPolicyIngressRule{
				{
					From: []v1beta1.MultiNetworkPolicyPeer{{PodSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}

func multiNetPolicyWithNamespaceSelector(policyName string) *v1beta1.MultiNetworkPolicy {
	return &v1beta1.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: v1beta1.MultiNetworkPolicySpec{
			Ingress: []v1beta1.MultiNetworkPolicyIngressRule{
				{
					From: []v1beta1.MultiNetworkPolicyPeer{{NamespaceSelector: sameLabelsEverywhere()}},
				},
			},
		},
	}
}
