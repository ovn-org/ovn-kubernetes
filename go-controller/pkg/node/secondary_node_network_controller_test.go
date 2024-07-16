package node

import (
	"context"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
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
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.watchFactory.Start()).To(Succeed())

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
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.watchFactory.Start()).To(Succeed())

		controller.NetInfo, err = util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())

		networkID, err := controller.getNetworkID()
		Expect(err).To(HaveOccurred())
		Expect(networkID).To(Equal(util.InvalidNetworkID))
	})
	It("ensure UDNGateway is not invoked when feature gate is OFF", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller := NewSecondaryNodeNetworkController(&cnnci, NetInfo)
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
	It("ensure UDNGateway is invoked for Primary UDNs when feature gate is ON", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller := NewSecondaryNodeNetworkController(&cnnci, NetInfo)
		err = controller.Start(context.Background())
		Expect(err).To(HaveOccurred()) // we don't have the gateway pieces setup so its expected to fail here
		Expect(err.Error()).To(ContainSubstring("no annotation found"))
		Expect(controller.gateway).To(Not(BeNil()))
	})
	It("ensure UDNGateway is not invoked for Primary UDNs when feature gate is ON but network is not Primary", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		factoryMock := factoryMocks.NodeWatchFactory{}
		nodeList := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
		}
		cnnci := CommonNodeNetworkControllerInfo{name: "worker1", watchFactory: &factoryMock}
		factoryMock.On("GetNode", "worker1").Return(nodeList[0], nil)
		factoryMock.On("GetNodes").Return(nodeList, nil)
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		NetInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		controller := NewSecondaryNodeNetworkController(&cnnci, NetInfo)
		err = controller.Start(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(controller.gateway).To(BeNil())
	})
})
