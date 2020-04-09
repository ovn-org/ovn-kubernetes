package util

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
			name string
			in   *L3GatewayConfig
			out  string
		}

		vlanid := uint(1024)

		testcases := []testcase{
			{
				name: "Disabled",
				in: &L3GatewayConfig{
					Mode: config.GatewayModeDisabled,
				},
				out: `{"default":{"mode":""}}`,
			},
			{
				name: "Local",
				in: &L3GatewayConfig{
					Mode:           config.GatewayModeLocal,
					ChassisID:      "SYSTEM-ID",
					InterfaceID:    "INTERFACE-ID",
					MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
					IPAddress:      ovntest.MustParseIPNet("192.168.1.10/24"),
					NextHop:        ovntest.MustParseIP("192.168.1.1"),
					NodePortEnable: true,
				},
				out: `{"default":{"mode":"local","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.10/24","next-hop":"192.168.1.1","node-port-enable":"true"}}`,
			},
			{
				name: "Shared",
				in: &L3GatewayConfig{
					Mode:           config.GatewayModeShared,
					ChassisID:      "SYSTEM-ID",
					InterfaceID:    "INTERFACE-ID",
					MACAddress:     ovntest.MustParseMAC("11:22:33:44:55:66"),
					IPAddress:      ovntest.MustParseIPNet("192.168.1.10/24"),
					NextHop:        ovntest.MustParseIP("192.168.1.1"),
					NodePortEnable: false,
					VLANID:         &vlanid,
				},
				out: `{"default":{"mode":"shared","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.10/24","next-hop":"192.168.1.1","node-port-enable":"false","vlan-id":"1024"}}`,
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

			err := SetL3GatewayConfig(nodeAnnotator, tc.in)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(testNode.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(updatedNode.Annotations[ovnNodeL3GatewayConfig]).To(MatchJSON(tc.out))
			if tc.in.Mode != config.GatewayModeDisabled {
				Expect(updatedNode.Annotations[ovnNodeChassisID]).To(Equal("SYSTEM-ID"))
			}

			l3gc, err := ParseNodeL3GatewayAnnotation(updatedNode)
			Expect(err).NotTo(HaveOccurred())
			Expect(l3gc).To(Equal(tc.in))
		}
	})
})
