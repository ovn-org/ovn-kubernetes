package util

import (
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("subnet annotation tests", func() {
	It("marshals and unmarshals the node-subnets and node-join-subnets annotations", func() {
		type testcase struct {
			name    string
			hsIn    []*net.IPNet
			joinIn  []*net.IPNet
			hsOut   string
			joinOut string
		}

		testcases := []testcase{
			{
				name:    "IPv4",
				hsIn:    []*net.IPNet{ovntest.MustParseIPNet("10.130.0.0/23")},
				hsOut:   `{"default":"10.130.0.0/23"}`,
				joinIn:  []*net.IPNet{ovntest.MustParseIPNet("100.64.0.0/29")},
				joinOut: `{"default":"100.64.0.0/29"}`,
			},
			{
				name:    "IPv6",
				hsIn:    []*net.IPNet{ovntest.MustParseIPNet("fd02:0:0:2::/64")},
				hsOut:   `{"default":"fd02:0:0:2::/64"}`,
				joinIn:  []*net.IPNet{ovntest.MustParseIPNet("fd98::/64")},
				joinOut: `{"default":"fd98::/64"}`,
			},
			{
				name: "Dual Stack",
				hsIn: []*net.IPNet{
					ovntest.MustParseIPNet("10.130.0.0/23"),
					ovntest.MustParseIPNet("fd02:0:0:2::/64"),
				},
				hsOut: `{"default":["10.130.0.0/23","fd02:0:0:2::/64"]}`,
				joinIn: []*net.IPNet{
					ovntest.MustParseIPNet("100.64.0.0/29"),
					ovntest.MustParseIPNet("fd98::/64"),
				},
				joinOut: `{"default":["100.64.0.0/29","fd98::/64"]}`,
			},
		}

		for _, tc := range testcases {
			testNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
			}}

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{testNode},
			})
			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &testNode)

			err := SetNodeHostSubnetAnnotation(nodeAnnotator, tc.hsIn)
			Expect(err).NotTo(HaveOccurred())
			err = SetNodeJoinSubnetAnnotation(nodeAnnotator, tc.joinIn)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(testNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedNode.Annotations[ovnNodeSubnets]).To(MatchJSON(tc.hsOut))
			Expect(updatedNode.Annotations[ovnNodeJoinSubnets]).To(MatchJSON(tc.joinOut))

			subnet, err := ParseNodeHostSubnetAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnet).To(Equal(tc.hsIn))

			subnet, err = ParseNodeJoinSubnetAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(subnet).To(Equal(tc.joinIn))
		}
	})
})
