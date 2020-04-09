package util

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Node annotation tests", func() {
	It("marshals the l3-gateway-config annotation", func() {
		type testcase struct {
			name   string
			setter func(kube.Annotator) error
			out    string

			setChassisID bool
		}

		testcases := []testcase{
			{
				name: "Disabled",
				setter: func(nodeAnnotator kube.Annotator) error {
					return SetDisabledL3GatewayConfig(nodeAnnotator)
				},
				out: `{"default":{"mode":""}}`,
			},
			{
				name: "Local",
				setter: func(nodeAnnotator kube.Annotator) error {
					return SetLocalL3GatewayConfig(nodeAnnotator,
						"INTERFACE-ID",
						ovntest.MustParseMAC("11:22:33:44:55:66"),
						ovntest.MustParseIPNet("192.168.1.10/24"),
						ovntest.MustParseIP("192.168.1.1"),
						true)
				},
				out: `{"default":{"mode":"local","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.10/24","next-hop":"192.168.1.1","node-port-enable":"true"}}`,

				setChassisID: true,
			},
			{
				name: "Shared",
				setter: func(nodeAnnotator kube.Annotator) error {
					return SetSharedL3GatewayConfig(nodeAnnotator,
						"INTERFACE-ID",
						ovntest.MustParseMAC("11:22:33:44:55:66"),
						ovntest.MustParseIPNet("192.168.1.10/24"),
						ovntest.MustParseIP("192.168.1.1"),
						false,
						1024)
				},
				out: `{"default":{"mode":"shared","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.10/24","next-hop":"192.168.1.1","node-port-enable":"false","vlan-id":"1024"}}`,

				setChassisID: true,
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

			fexec := ovntest.NewFakeExec()
			err := SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			if tc.setChassisID {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
					Output: "SYSTEM-ID",
				})
			}

			err = tc.setter(nodeAnnotator)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(testNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedNode.Annotations[ovnNodeL3GatewayConfig]).To(MatchJSON(tc.out))
			if tc.setChassisID {
				Expect(updatedNode.Annotations[ovnNodeChassisID]).To(Equal("SYSTEM-ID"))
			}
		}
	})
})
