package userdefinednetwork

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
)

var _ = Describe("NetAttachDefNotInUse", func() {
	DescribeTable("should succeed",
		func(nad *netv1.NetworkAttachmentDefinition, pods []*corev1.Pod) {
			Expect(NetAttachDefNotInUse(nad, pods)).To(Succeed())
		},
		Entry("pods has no OVN annotation",
			&netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", Namespace: "blue"},
			},
			[]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "blue"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "blue"},
				},
			},
		),
		Entry("no pod is connected",
			&netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", Namespace: "blue"},
			},
			[]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "blue",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"role":"primary", "mac_address":"0a:58:0a:f4:02:03"}}`,
						}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "blue"},
				},
			},
		),
	)

	DescribeTable("should fail",
		func(nad *netv1.NetworkAttachmentDefinition, pods []*corev1.Pod) {
			Expect(NetAttachDefNotInUse(nad, pods)).ToNot(Succeed())
		},
		Entry("1 pod is connected",
			&netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", Namespace: "blue"},
			},
			[]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "blue",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"role":"infrastructure-locked", "mac_address":"0a:58:0a:f4:02:03"},` +
								`"blue/test-net":{"role": "primary","mac_address":"0a:58:0a:f4:02:01"}}`,
						}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "bar"},
				},
			},
		),
		Entry("1 pod has invalid annotation",
			&netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", Namespace: "blue"},
			},
			[]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "blue",
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `INVALID`,
						}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "bar"},
				},
			},
		),
	)
})

var _ = Describe("PrimaryNetAttachDefNotExist", func() {
	It("should succeed given no primary UDN NAD", func() {
		nads := []*netv1.NetworkAttachmentDefinition{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net1", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "ovn-k8s-cni-overlay","role": "secondary"}`},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net2", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "ovn-k8s-cni-overlay","role": "secondary"}`},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net3", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "fake-ovn-cni","role": "primary"}`},
			},
		}
		Expect(PrimaryNetAttachDefNotExist(nads)).To(Succeed())
	})
	It("should fail given primary UDN NAD", func() {
		nads := []*netv1.NetworkAttachmentDefinition{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net1", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "ovn-k8s-cni-overlay","role": "primary"}`},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net2", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "ovn-k8s-cni-overlay","role": "secondary"}`},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net3", Namespace: "blue"},
				Spec:       netv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion": "1.0.0","type": "fake-ovn-cni","role": "primary"}`},
			},
		}
		Expect(PrimaryNetAttachDefNotExist(nads)).ToNot(Succeed())
	})
})
