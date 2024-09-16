package ovn

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("BaseSecondaryNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
	})
	It("should return networkID from one of the nodes node", func() {
		fakeOVN := NewFakeOVN(false)
		fakeOVN.start(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker1",
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
				},
			},
		})
		Expect(fakeOVN.NewSecondaryNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.secondaryControllers["bluenet"]
		Expect(ok).To(BeTrue())

		networkID, err := controller.bnc.getNetworkID()
		Expect(err).ToNot(HaveOccurred())
		Expect(networkID).To(Equal(3))
	})
	It("should return invalid networkID if network is not found", func() {
		fakeOVN := NewFakeOVN(false)
		fakeOVN.start(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker1",
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": `{"other": "3"}`,
				},
			},
		})
		Expect(fakeOVN.NewSecondaryNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.secondaryControllers["bluenet"]
		Expect(ok).To(BeTrue())

		networkID, err := controller.bnc.getNetworkID()
		Expect(err).To(HaveOccurred())
		Expect(networkID).To(Equal(util.InvalidNetworkID))
	})

})
