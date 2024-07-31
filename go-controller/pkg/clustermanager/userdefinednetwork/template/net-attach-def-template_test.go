package template

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

var _ = Describe("NetAttachDefTemplate", func() {
	const udnTypeName = "UserDefinedNetwork"

	var udnApiVersion = udnv1.SchemeGroupVersion.String()

	DescribeTable("should fail given",
		func(spec *udnv1.UserDefinedNetworkSpec) {
			udn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Namespace: "mynamespace", Name: "test-net", UID: "1"},
				Spec:       *spec,
			}
			_, err := RenderNetAttachDefManifest(udn)
			Expect(err).To(HaveOccurred())
		},
		Entry("invalid subnets", &udnv1.UserDefinedNetworkSpec{Subnets: []string{"abc"}}),
		Entry("invalid exclude subnets", &udnv1.UserDefinedNetworkSpec{ExcludeSubnets: []string{"abc"}}),
		Entry("invalid join subnets", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary, JoinSubnets: []string{"abc"}}),
		Entry("topology=localnet & role=primary", &udnv1.UserDefinedNetworkSpec{
			Topology: udnv1.NetworkTopologyLocalnet, Role: udnv1.NetworkRolePrimary}),
		Entry("ipamLifecycle=persistent & topology=Layer3", &udnv1.UserDefinedNetworkSpec{
			IPAMLifecycle: udnv1.IPAMLifecyclePersistent, Topology: udnv1.NetworkTopologyLayer3}),
		Entry("invalid join subnets", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"abc"}}),
		Entry("invalid dual-stack join subnets, invalid IPv4 CIDR", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"!", "fd50::0/125"}}),
		Entry("invalid dual-stack join subnets, invalid IPv6 CIDR", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"10.10.0.0/24", "!"}}),
		Entry("invalid dual-stack join subnets, multiple valid IPv4 CIDRs", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"10.10.0.0/24", "10.20.0.0/24", "10.30.0.0/24"}}),
		Entry("invalid dual-stack join subnets, multiple valid IPv6 CIDRs", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"fd40::0/125", "fd10::0/125", "fd50::0/125"}}),
		Entry("invalid dual-stack join subnets, multiple valid IPv4 & IPv6 CIDRs", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"fd40::0/125", "10.10.0.0/24", "fd50::0/125", "10.20.0.0/24"}}),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv4", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"100.64.10.0/24"}}),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv6", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"fd98::4/127"}}),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, dual-stack", &udnv1.UserDefinedNetworkSpec{Role: udnv1.NetworkRolePrimary,
			JoinSubnets: []string{"100.64.10.0/24", "fd98::4/127"}}),
	)

	It("should return nil given no NAD", func() {
		_, err := RenderNetAttachDefManifest(nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should create net attach from spec", func() {
		udn := &udnv1.UserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{Namespace: "mynamespace", Name: "test-net", UID: "1"},
			Spec: udnv1.UserDefinedNetworkSpec{
				Topology:       udnv1.NetworkTopologyLayer2,
				Role:           udnv1.NetworkRolePrimary,
				Subnets:        []string{"192.168.100.0/24", "2001:DBB::/64"},
				ExcludeSubnets: []string{"192.168.100.0/26"},
				JoinSubnets:    []string{"100.61.0.0/24", "fd90::/64"},
				MTU:            1500,
				IPAMLifecycle:  udnv1.IPAMLifecyclePersistent,
			},
		}
		expectedNAD := &netv1.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-net",
				Labels: map[string]string{
					"k8s.ovn.org/user-defined-network": "",
				},
				Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         udnApiVersion,
						Kind:               udnTypeName,
						Name:               "test-net",
						UID:                "1",
						BlockOwnerDeletion: pointer.Bool(true),
						Controller:         pointer.Bool(true),
					},
				},
			},
			Spec: netv1.NetworkAttachmentDefinitionSpec{
				Config: `{
						"cniVersion": "1.0.0",
						"name": "mynamespace.test-net",
						"type": "ovn-k8s-cni-overlay",
						"netAttachDefName": "mynamespace/test-net",
						"topology": "layer2",
						"role": "primary",
						"mtu": 1500,
        				"subnets": "192.168.100.0/24/0,2001:dbb::/64/0",
						"excludeSubnets": "192.168.100.0/26",
						"joinSubnets": "100.61.0.0/24,fd90::/64",
 						"allowPersistentIPs": true
					}`,
			},
		}

		nad, err := RenderNetAttachDefManifest(udn)
		Expect(err).NotTo(HaveOccurred())
		Expect(nad.TypeMeta).To(Equal(expectedNAD.TypeMeta))
		Expect(nad.ObjectMeta).To(Equal(expectedNAD.ObjectMeta))
		Expect(nad.Spec.Config).To(MatchJSON(expectedNAD.Spec.Config))
	})

	It("when network role is primary, no join-subnet specified, should set default join subnet", func() {
		udn := &udnv1.UserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{Namespace: "mynamespace", Name: "test-net", UID: "1"},
			Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Role:     udnv1.NetworkRolePrimary,
				Subnets:  []string{"192.168.100.0/24"},
			},
		}

		nad, err := RenderNetAttachDefManifest(udn)
		Expect(err).NotTo(HaveOccurred())

		expectedCNIConf := `{
			"cniVersion":"1.0.0",
			"type":"ovn-k8s-cni-overlay",
			"name":"mynamespace.test-net",
			"netAttachDefName":"mynamespace/test-net",
			"topology":"layer2",
            "role":"primary",
			"subnets":"192.168.100.0/24/0",
			"joinSubnets":"100.65.0.0/16,fd99::/64"
		}`
		Expect(nad.Spec.Config).To(MatchJSON(expectedCNIConf))
	})
})
