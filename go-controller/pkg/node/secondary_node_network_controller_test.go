package node

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("SecondaryNodeNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	It("should return networkID from one of the nodes in the cluster", func() {
		fakeClient := &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			}),
		}
		controller := SecondaryNodeNetworkController{}
		var err error
		controller.watchFactory, err = factory.NewNodeWatchFactory(fakeClient, "worker1")
		Expect(controller.watchFactory.Start()).To(Succeed())

		Expect(err).NotTo(HaveOccurred())
		controller.NetInfo, err = util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		networkID, err := controller.getNetworkID()
		Expect(err).ToNot(HaveOccurred())
		Expect(networkID).To(Equal(3))
	})

	It("should return invalid networkID if network not found", func() {
		fakeClient := &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"othernet": "3"}`,
					},
				},
			}),
		}
		controller := SecondaryNodeNetworkController{}
		var err error
		controller.watchFactory, err = factory.NewNodeWatchFactory(fakeClient, "worker1")
		Expect(controller.watchFactory.Start()).To(Succeed())

		Expect(err).NotTo(HaveOccurred())
		controller.NetInfo, err = util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		networkID, err := controller.getNetworkID()
		Expect(err).To(HaveOccurred())
		Expect(networkID).To(Equal(util.InvalidNetworkID))
	})
})
