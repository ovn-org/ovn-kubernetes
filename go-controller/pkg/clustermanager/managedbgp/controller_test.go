// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managedbgp

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ctesting "k8s.io/client-go/testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var generateNameCount uint32

// addGenerateNameReactor adds a reactor to a fake client that simulates
// the server-side GenerateName behavior.
func addGenerateNameReactor(fake interface {
	PrependReactor(verb, resource string, reaction ctesting.ReactionFunc)
}) {
	fake.PrependReactor("create", "*", func(action ctesting.Action) (handled bool, ret runtime.Object, err error) {
		ret = action.(ctesting.CreateAction).GetObject()
		meta, ok := ret.(metav1.Object)
		if !ok {
			return
		}
		if meta.GetName() == "" && meta.GetGenerateName() != "" {
			meta.SetName(meta.GetGenerateName() + fmt.Sprintf("%d", atomic.AddUint32(&generateNameCount, 1)))
		}
		return
	})
}

func TestManagedBGPController(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Managed BGP Controller Suite")
}

var _ = ginkgo.Describe("Managed BGP Controller", func() {
	var (
		// Save original config
		oldTopology              string
		oldASNumber              uint32
		oldFRRNamespace          string
		oldOVNConfigNamespace    string
		oldEnableMultiNetwork    bool
		oldEnableRouteAdvertise  bool
		oldEnableNetSegmentation bool
		oldTransport             string
		oldNoOverlayRouting      string
	)

	ginkgo.BeforeEach(func() {
		// Save original config
		oldTopology = config.ManagedBGP.Topology
		oldASNumber = config.ManagedBGP.ASNumber
		oldFRRNamespace = config.ManagedBGP.FRRNamespace
		oldOVNConfigNamespace = config.Kubernetes.OVNConfigNamespace
		oldEnableMultiNetwork = config.OVNKubernetesFeature.EnableMultiNetwork
		oldEnableRouteAdvertise = config.OVNKubernetesFeature.EnableRouteAdvertisements
		oldEnableNetSegmentation = config.OVNKubernetesFeature.EnableNetworkSegmentation
		oldTransport = config.Default.Transport
		oldNoOverlayRouting = config.NoOverlay.Routing

		// Set test config
		config.Default.Transport = types.NetworkTransportNoOverlay
		config.NoOverlay.Routing = config.NoOverlayRoutingManaged
		config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
		config.ManagedBGP.ASNumber = 64512
		config.ManagedBGP.FRRNamespace = "frr-k8s-system"
		config.Kubernetes.OVNConfigNamespace = "ovn-kubernetes"
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	})

	ginkgo.AfterEach(func() {
		// Restore original config
		config.ManagedBGP.Topology = oldTopology
		config.ManagedBGP.ASNumber = oldASNumber
		config.ManagedBGP.FRRNamespace = oldFRRNamespace
		config.Kubernetes.OVNConfigNamespace = oldOVNConfigNamespace
		config.OVNKubernetesFeature.EnableMultiNetwork = oldEnableMultiNetwork
		config.OVNKubernetesFeature.EnableRouteAdvertisements = oldEnableRouteAdvertise
		config.OVNKubernetesFeature.EnableNetworkSegmentation = oldEnableNetSegmentation
		config.Default.Transport = oldTransport
		config.NoOverlay.Routing = oldNoOverlayRouting
	})

	ginkgo.Context("Controller initialization", func() {
		ginkgo.It("should create a new controller", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.frrClient).To(gomega.Equal(frrFakeClient))
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.nodeEventHandler).NotTo(gomega.BeNil())
			gomega.Expect(controller.raEventHandler).NotTo(gomega.BeNil())
			gomega.Expect(controller.frrEventHandler).NotTo(gomega.BeNil())
		})
	})

	ginkgo.Context("Full-mesh topology", func() {
		ginkgo.BeforeEach(func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
		})

		ginkgo.It("should create base FRRConfiguration with IPv4 nodes", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Should create one base FRRConfiguration
			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			// Verify base configuration
			baseConfig, err := getBaseFRRConfig(frrFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Labels).To(gomega.HaveKeyWithValue(managedNetworkLabel, ""))
			gomega.Expect(baseConfig.Spec.BGP.Routers).To(gomega.HaveLen(1))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].ASN).To(gomega.Equal(uint32(64512)))
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(2))

			addresses := []string{baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address, baseConfig.Spec.BGP.Routers[0].Neighbors[1].Address}
			gomega.Expect(addresses).To(gomega.ConsistOf("10.0.0.1", "10.0.0.2"))

			// Verify neighbors use address-family-specific sessions.
			for _, neighbor := range baseConfig.Spec.BGP.Routers[0].Neighbors {
				gomega.Expect(neighbor.DualStackAddressFamily).To(gomega.BeFalse())
				gomega.Expect(neighbor.ASN).To(gomega.Equal(uint32(64512)))
			}
		})

		ginkgo.It("should create base FRRConfiguration with IPv6 nodes", func() {
			node1 := createNode("node1", "", "fd00::1")
			node2 := createNode("node2", "", "fd00::2")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := getBaseFRRConfig(frrFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(2))

			addresses := []string{baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address, baseConfig.Spec.BGP.Routers[0].Neighbors[1].Address}
			gomega.Expect(addresses).To(gomega.ConsistOf("fd00::1", "fd00::2"))
		})

		ginkgo.It("should create base FRRConfiguration with dual-stack nodes", func() {
			node1 := createNode("node1", "10.0.0.1", "fd00::1")
			node2 := createNode("node2", "10.0.0.2", "fd00::2")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				list, _ := frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := getBaseFRRConfig(frrFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// 2 nodes × 2 addresses each = 4 neighbors
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors).To(gomega.HaveLen(4))

			addresses := make([]string, 4)
			for i, neighbor := range baseConfig.Spec.BGP.Routers[0].Neighbors {
				addresses[i] = neighbor.Address
			}
			gomega.Expect(addresses).To(gomega.ConsistOf("10.0.0.1", "fd00::1", "10.0.0.2", "fd00::2"))
		})

		ginkgo.It("should update base FRRConfiguration when a node is added", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Wait for initial configuration
			gomega.Eventually(func() int {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil {
					return 0
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(2))

			// Add a third node
			node3 := createNode("node3", "10.0.0.3", "")
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), node3, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to include node3
			gomega.Eventually(func() []string {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil {
					return nil
				}
				addresses := make([]string, len(bc.Spec.BGP.Routers[0].Neighbors))
				for i, n := range bc.Spec.BGP.Routers[0].Neighbors {
					addresses[i] = n.Address
				}
				return addresses
			}, 2*time.Second).Should(gomega.ConsistOf("10.0.0.1", "10.0.0.2", "10.0.0.3"))
		})

		ginkgo.It("should update base FRRConfiguration when a node is deleted", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			node2 := createNode("node2", "10.0.0.2", "")

			fakeClient := fake.NewSimpleClientset(node1, node2)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() int {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil {
					return 0
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(2))

			// Delete node2
			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), "node2", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to exclude node2
			gomega.Eventually(func() int {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil {
					return -1
				}
				return len(bc.Spec.BGP.Routers[0].Neighbors)
			}, 2*time.Second).Should(gomega.Equal(1))

			baseConfig, err := getBaseFRRConfig(frrFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(baseConfig.Spec.BGP.Routers[0].Neighbors[0].Address).To(gomega.Equal("10.0.0.1"))
		})

		ginkgo.It("should update base FRRConfiguration when a node IP changes", func() {
			node1 := createNode("node1", "10.0.0.1", "")

			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() string {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil || len(bc.Spec.BGP.Routers) == 0 || len(bc.Spec.BGP.Routers[0].Neighbors) == 0 {
					return ""
				}
				return bc.Spec.BGP.Routers[0].Neighbors[0].Address
			}, 2*time.Second).Should(gomega.Equal("10.0.0.1"))

			// Update node IP
			node1Updated := createNode("node1", "10.0.0.99", "")
			_, err = fakeClient.CoreV1().Nodes().Update(context.TODO(), node1Updated, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should update to new IP
			gomega.Eventually(func() string {
				bc, err := getBaseFRRConfig(frrFakeClient)
				if err != nil || len(bc.Spec.BGP.Routers) == 0 || len(bc.Spec.BGP.Routers[0].Neighbors) == 0 {
					return ""
				}
				return bc.Spec.BGP.Routers[0].Neighbors[0].Address
			}, 2*time.Second).Should(gomega.Equal("10.0.0.99"))
		})

		ginkgo.It("should repair base FRRConfiguration drift", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			gomega.Eventually(func() error {
				_, err := getBaseFRRConfig(frrFakeClient)
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			// Drift the FRR config externally (both spec and label)
			baseConfig, err := getBaseFRRConfig(frrFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			drifted := baseConfig.DeepCopy()
			drifted.Labels = map[string]string{"drifted": "true"}
			drifted.Spec.BGP.Routers[0].Neighbors = []frrtypes.Neighbor{{
				Address:                "10.0.0.99",
				ASN:                    config.ManagedBGP.ASNumber,
				DualStackAddressFamily: true,
			}}
			_, err = frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), drifted, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// FRR config should be repaired
			gomega.Eventually(func() bool {
				cfg, err := getBaseFRRConfig(frrFakeClient)
				if err != nil {
					return false
				}
				_, hasLabel := cfg.Labels[managedNetworkLabel]
				return hasLabel &&
					len(cfg.Spec.BGP.Routers) == 1 &&
					len(cfg.Spec.BGP.Routers[0].Neighbors) == 1 &&
					cfg.Spec.BGP.Routers[0].Neighbors[0].Address == "10.0.0.1" &&
					!cfg.Spec.BGP.Routers[0].Neighbors[0].DualStackAddressFamily
			}, 2*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should clean up CDN RouteAdvertisement and FRR config when CDN managed mode is disabled", func() {
			// Pre-create the resources that would have been created by a previous managed-mode run
			baseFRRConfig := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedNamePrefix,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						managedNetworkLabel: "",
					},
				},
				Spec: frrtypes.FRRConfigurationSpec{},
			}
			managedRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: managedRAName(types.DefaultNetworkName),
				},
			}

			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset(baseFRRConfig)
			raFakeClient := rafake.NewSimpleClientset(managedRA)
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Switch CDN to unmanaged mode
			config.NoOverlay.Routing = config.NoOverlayRoutingUnmanaged

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// CDN managed RouteAdvertisement should be deleted
			gomega.Eventually(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName(types.DefaultNetworkName), metav1.GetOptions{})
				return err != nil && apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())

			// Base FRRConfiguration should be deleted (no managed networks remain)
			gomega.Eventually(func() bool {
				_, err := getBaseFRRConfig(frrFakeClient)
				return err != nil
			}, 2*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should handle empty cluster (no nodes)", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			waitForBaseNeighbors := func(expected ...string) {
				gomega.Eventually(func() error {
					baseConfig, err := getBaseFRRConfig(frrFakeClient)
					if err != nil {
						return err
					}
					if len(baseConfig.Spec.BGP.Routers) != 1 {
						return fmt.Errorf("expected one BGP router, got %d", len(baseConfig.Spec.BGP.Routers))
					}
					neighbors := baseConfig.Spec.BGP.Routers[0].Neighbors
					if len(neighbors) != len(expected) {
						return fmt.Errorf("expected neighbors %v, got %v", expected, neighbors)
					}
					for i, expectedAddress := range expected {
						if neighbors[i].Address != expectedAddress {
							return fmt.Errorf("expected neighbor %d to be %q, got %q", i, expectedAddress, neighbors[i].Address)
						}
					}
					return nil
				}, 2*time.Second).Should(gomega.Succeed())
			}

			// Should create the base config with no neighbors when no nodes exist.
			waitForBaseNeighbors()

			// Add a node and wait until the controller has reconciled it before
			// deleting it. Otherwise the test can race with the queued add event.
			tempNode := createNode("temp", "10.0.0.99", "")
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), tempNode, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			waitForBaseNeighbors("10.0.0.99")

			err = fakeClient.CoreV1().Nodes().Delete(context.TODO(), "temp", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Should return to no neighbors after the last node is deleted.
			waitForBaseNeighbors()
		})
	})

	ginkgo.Context("Non-full-mesh topology", func() {
		ginkgo.It("should return error from ensureBaseFRRConfiguration when topology is not full-mesh", func() {
			config.ManagedBGP.Topology = ""

			node1 := createNode("node1", "10.0.0.1", "")

			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.ensureBaseFRRConfiguration(context.Background())
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("unsupported managed BGP topology"))
		})
	})

	ginkgo.Context("nodeNeedsUpdate", func() {
		var controller *Controller

		ginkgo.BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should return true when old node is nil", func() {
			newNode := createNode("node1", "10.0.0.1", "")
			gomega.Expect(controller.nodeNeedsUpdate(nil, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when new node is nil", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, nil)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when IPv4 address changes", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			newNode := createNode("node1", "10.0.0.2", "")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return true when IPv6 address changes", func() {
			oldNode := createNode("node1", "", "fd00::1")
			newNode := createNode("node1", "", "fd00::2")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when addresses are the same", func() {
			oldNode := createNode("node1", "10.0.0.1", "fd00::1")
			newNode := createNode("node1", "10.0.0.1", "fd00::1")
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeFalse())
		})

		ginkgo.It("should return false when only labels change", func() {
			oldNode := createNode("node1", "10.0.0.1", "")
			newNode := createNode("node1", "10.0.0.1", "")
			newNode.Labels["foo"] = "bar"
			gomega.Expect(controller.nodeNeedsUpdate(oldNode, newNode)).To(gomega.BeFalse())
		})
	})

	ginkgo.Context("RouteAdvertisements management", func() {
		ginkgo.BeforeEach(func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
			config.Default.Transport = types.NetworkTransportNoOverlay
			config.NoOverlay.Routing = config.NoOverlayRoutingManaged
		})

		ginkgo.It("should create managed RouteAdvertisement on controller start", func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Verify managed RouteAdvertisement was created
			var ra *ratypes.RouteAdvertisements
			gomega.Eventually(func() error {
				var err error
				ra, err = raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName(types.DefaultNetworkName), metav1.GetOptions{})
				if err != nil {
					return err
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())

			gomega.Expect(ra).NotTo(gomega.BeNil())
			gomega.Expect(ra.Name).To(gomega.Equal(managedRAName(types.DefaultNetworkName)))
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels).To(gomega.HaveKeyWithValue(managedNetworkLabel, ""))
			gomega.Expect(ra.Spec.Advertisements).To(gomega.ConsistOf(ratypes.PodNetwork))
			gomega.Expect(ra.Spec.NetworkSelectors).To(gomega.HaveLen(1))
			gomega.Expect(ra.Spec.NetworkSelectors[0].NetworkSelectionType).To(gomega.Equal(apitypes.DefaultNetwork))
		})

		ginkgo.It("should repair managed RouteAdvertisement drift", func() {
			node := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			var ra *ratypes.RouteAdvertisements
			gomega.Eventually(func() error {
				var err error
				ra, err = raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName(types.DefaultNetworkName), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())

			// Drift the RA externally - modify spec (wrong selector)
			drifted := ra.DeepCopy()
			drifted.Spec.FRRConfigurationSelector.MatchLabels = map[string]string{"drifted": "true"}
			_, err = raFakeClient.K8sV1().RouteAdvertisements().Update(context.TODO(), drifted, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() bool {
				repairedRA, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName(types.DefaultNetworkName), metav1.GetOptions{})
				if err != nil || repairedRA == nil {
					return false
				}
				// Should have correct spec
				hasCorrectSpec := repairedRA.Spec.FRRConfigurationSelector.MatchLabels[managedNetworkLabel] == ""
				return hasCorrectSpec
			}, 2*time.Second).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("cudnNeedsUpdate", func() {
		var controller *Controller

		ginkgo.BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should return true when a managed CUDN is added", func() {
			newCUDN := createManagedCUDN("blue", map[string]string{"network": "blue"})
			gomega.Expect(controller.cudnNeedsUpdate(nil, newCUDN)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when an unmanaged CUDN is added", func() {
			newCUDN := createUnmanagedCUDN("green", map[string]string{"network": "green"})
			gomega.Expect(controller.cudnNeedsUpdate(nil, newCUDN)).To(gomega.BeFalse())
		})

		ginkgo.It("should return true when a managed CUDN managed-network label is removed", func() {
			oldCUDN := createManagedCUDN("blue", map[string]string{managedNetworkLabel: ""})
			newCUDN := oldCUDN.DeepCopy()
			delete(newCUDN.Labels, managedNetworkLabel)
			gomega.Expect(controller.cudnNeedsUpdate(oldCUDN, newCUDN)).To(gomega.BeTrue())
		})

		ginkgo.It("should return false when a managed CUDN unrelated labels change", func() {
			oldCUDN := createManagedCUDN("blue", map[string]string{"network": "blue"})
			newCUDN := oldCUDN.DeepCopy()
			newCUDN.Labels["extra"] = "value"
			gomega.Expect(controller.cudnNeedsUpdate(oldCUDN, newCUDN)).To(gomega.BeFalse())
		})

		ginkgo.It("should return false when an unmanaged CUDN labels change", func() {
			oldCUDN := createUnmanagedCUDN("green", map[string]string{"network": "green"})
			newCUDN := oldCUDN.DeepCopy()
			newCUDN.Labels["extra"] = "value"
			gomega.Expect(controller.cudnNeedsUpdate(oldCUDN, newCUDN)).To(gomega.BeFalse())
		})

		ginkgo.It("should return false when new CUDN is nil", func() {
			oldCUDN := createManagedCUDN("blue", map[string]string{"network": "blue"})
			gomega.Expect(controller.cudnNeedsUpdate(oldCUDN, nil)).To(gomega.BeFalse())
		})
	})

	ginkgo.Context("owned resource update filters", func() {
		var controller *Controller

		ginkgo.BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller, err = NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should reconcile external managed RouteAdvertisement add events", func() {
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: managedRAName("cudn"),
				},
			}
			gomega.Expect(controller.raNeedsUpdate(nil, ra)).To(gomega.BeTrue())
		})

		ginkgo.It("should ignore own managed RouteAdvertisement add events", func() {
			now := metav1.Now()
			ra := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: managedRAName("cudn"),
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: fieldManager, Time: &now},
					},
				},
			}
			gomega.Expect(controller.raNeedsUpdate(nil, ra)).To(gomega.BeFalse())
		})

		ginkgo.It("should reconcile managed RouteAdvertisement updates", func() {
			oldRA := &ratypes.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: managedRAName("cudn"),
				},
			}
			newRA := oldRA.DeepCopy()
			newRA.Spec.Advertisements = []ratypes.AdvertisementType{ratypes.EgressIP}
			gomega.Expect(controller.raNeedsUpdate(oldRA, newRA)).To(gomega.BeTrue())
		})

		ginkgo.It("should reconcile external managed FRRConfiguration add events", func() {
			frr := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedNamePrefix,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						managedNetworkLabel: "",
					},
				},
			}
			gomega.Expect(controller.frrNeedsUpdate(nil, frr)).To(gomega.BeTrue())
		})

		ginkgo.It("should ignore own managed FRRConfiguration add events", func() {
			now := metav1.Now()
			frr := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedNamePrefix,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						managedNetworkLabel: "",
					},
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: fieldManager, Time: &now},
					},
				},
			}
			gomega.Expect(controller.frrNeedsUpdate(nil, frr)).To(gomega.BeFalse())
		})

		ginkgo.It("should reconcile managed FRRConfiguration updates", func() {
			oldFRR := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedNamePrefix,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						managedNetworkLabel: "",
					},
				},
			}
			newFRR := oldFRR.DeepCopy()
			newFRR.Spec.NodeSelector.MatchLabels = map[string]string{"role": "worker"}
			gomega.Expect(controller.frrNeedsUpdate(oldFRR, newFRR)).To(gomega.BeTrue())
		})

		ginkgo.It("should reconcile managed FRRConfiguration label drift", func() {
			oldFRR := &frrtypes.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedNamePrefix,
					Namespace: config.ManagedBGP.FRRNamespace,
					Labels: map[string]string{
						managedNetworkLabel: "",
					},
				},
			}
			newFRR := oldFRR.DeepCopy()
			newFRR.Labels = map[string]string{"drifted": "true"}
			gomega.Expect(controller.frrNeedsUpdate(oldFRR, newFRR)).To(gomega.BeTrue())
		})
	})

	ginkgo.Context("CUDN managed routing", func() {
		ginkgo.BeforeEach(func() {
			config.ManagedBGP.Topology = config.ManagedBGPTopologyFullMesh
			// CDN is NOT no-overlay — CUDN-only managed mode
			config.Default.Transport = ""
			config.NoOverlay.Routing = ""
		})

		ginkgo.It("should create RouteAdvertisement for managed CUDN", func() {
			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			// Verify CUDN RouteAdvertisement was created
			var ra *ratypes.RouteAdvertisements
			gomega.Eventually(func() error {
				var err error
				ra, err = raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())

			gomega.Expect(ra).NotTo(gomega.BeNil())
			gomega.Expect(ra.Name).To(gomega.Equal(managedRAName("cudn")))
			gomega.Expect(ra.Spec.Advertisements).To(gomega.ConsistOf(ratypes.PodNetwork))
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels).To(gomega.HaveKeyWithValue(managedNetworkLabel, ""))
			gomega.Expect(ra.Spec.NetworkSelectors).To(gomega.HaveLen(1))
			gomega.Expect(ra.Spec.NetworkSelectors[0].NetworkSelectionType).To(gomega.Equal(apitypes.ClusterUserDefinedNetworks))
			gomega.Expect(ra.Spec.NetworkSelectors[0].ClusterUserDefinedNetworkSelector).NotTo(gomega.BeNil())
			gomega.Expect(ra.Spec.NetworkSelectors[0].ClusterUserDefinedNetworkSelector.NetworkSelector.MatchLabels).To(
				gomega.HaveKeyWithValue(managedNetworkLabel, ""))

			// Verify base FRRConfiguration was also created
			gomega.Eventually(func() error {
				_, err := getBaseFRRConfig(frrFakeClient)
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			// Verify NO CDN RouteAdvertisement was created (CDN is not no-overlay)
			_, err = raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName(types.DefaultNetworkName), metav1.GetOptions{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
		})

		ginkgo.It("should delete RouteAdvertisement when managed CUDN is deleted", func() {
			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			// Wait for RA to be created, then delete the CUDN
			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				// RA exists, now delete the CUDN
				err = udnFakeClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), "blue", metav1.DeleteOptions{})
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			// RA should be deleted (last CUDN deleted, so shared CUDN RA should be removed)
			gomega.Eventually(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				return err != nil && apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should create base FRRConfiguration when a managed CUDN is added after startup", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			udnFakeClient := udnfake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			_, err = udnFakeClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), cudn, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				_, err := getBaseFRRConfig(frrFakeClient)
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should restore RouteAdvertisement if externally deleted while running", func() {
			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			var raName string
			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				raName = ra.Name
				return nil
			}, 2*time.Second).Should(gomega.Succeed())

			err = raFakeClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should restore base FRRConfiguration if externally deleted while running", func() {
			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			var baseFRR *frrtypes.FRRConfiguration
			gomega.Eventually(func() error {
				baseFRR, err = getBaseFRRConfig(frrFakeClient)
				return err
			}, 2*time.Second).Should(gomega.Succeed())

			err = frrFakeClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(context.TODO(), baseFRR.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Controller recreates the base FRRConfiguration
			gomega.Eventually(func() error {
				_, err := getBaseFRRConfig(frrFakeClient)
				return err
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should not create RouteAdvertisement for unmanaged CUDN", func() {
			cudn := createUnmanagedCUDN("green", map[string]string{"network": "green"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			// Should not create a CUDN RA
			gomega.Consistently(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				return err != nil && apierrors.IsNotFound(err)
			}, 1*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should not create RouteAdvertisement for non-no-overlay CUDN", func() {
			// CUDN without transport set (default Geneve)
			cudn := &userdefinednetworkv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "default-transport",
					Labels: map[string]string{"network": "default-transport"},
				},
				Spec: userdefinednetworkv1.ClusterUserDefinedNetworkSpec{
					Network: userdefinednetworkv1.NetworkSpec{
						Topology: userdefinednetworkv1.NetworkTopologyLayer3,
						Layer3: &userdefinednetworkv1.Layer3Config{
							Role: userdefinednetworkv1.NetworkRolePrimary,
						},
					},
				},
			}
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			gomega.Consistently(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				return err != nil && apierrors.IsNotFound(err)
			}, 1*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should restore RouteAdvertisement if externally deleted on restart", func() {
			cudn := createManagedCUDN("blue", map[string]string{"network": "blue"})
			udnFakeClient := udnfake.NewSimpleClientset(cudn)

			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnFakeClient,
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, udnFakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			// Wait for RA to be created
			var raName string
			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				raName = ra.Name
				return nil
			}, 2*time.Second).Should(gomega.Succeed())

			c.Stop()

			// Delete the RA externally
			err = raFakeClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() bool {
				_, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				return err != nil && apierrors.IsNotFound(err)
			}, 2*time.Second).Should(gomega.BeTrue())

			// Wait for the informer cache to process the deletion
			gomega.Eventually(func() error {
				_, err := c.raLister.Get(managedRAName("cudn"))
				return err
			}, 2*time.Second).ShouldNot(gomega.Succeed())

			// ensureManagedConfiguration (triggered on restart) should restore the RA
			err = c.ensureManagedConfiguration(context.Background(), "")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() error {
				ra, err := raFakeClient.K8sV1().RouteAdvertisements().Get(context.TODO(), managedRAName("cudn"), metav1.GetOptions{})
				if err != nil {
					return err
				}
				if ra == nil {
					return fmt.Errorf("RouteAdvertisement not found")
				}
				return nil
			}, 2*time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("should not create RA when no managed CUDNs exist", func() {
			node1 := createNode("node1", "10.0.0.1", "")
			fakeClient := fake.NewSimpleClientset(node1)
			frrFakeClient := frrfake.NewSimpleClientset()
			raFakeClient := rafake.NewSimpleClientset()
			addGenerateNameReactor(frrFakeClient)

			wf, err := factory.NewClusterManagerWatchFactory(&util.OVNClusterManagerClientset{
				KubeClient:                fakeClient,
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: raFakeClient,
				FRRClient:                 frrFakeClient,
				UserDefinedNetworkClient:  udnfake.NewSimpleClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = wf.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			c, err := NewController(wf, frrFakeClient, raFakeClient, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = c.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer c.Stop()

			// No CUDN RAs should be created (no CDN RA either since CDN is not no-overlay)
			gomega.Consistently(func() int {
				list, _ := raFakeClient.K8sV1().RouteAdvertisements().List(context.TODO(), metav1.ListOptions{})
				return len(list.Items)
			}, 1*time.Second, 100*time.Millisecond).Should(gomega.Equal(0))
		})
	})
})

// getBaseFRRConfig returns the managed base FRRConfiguration found by name.
func getBaseFRRConfig(frrClient *frrfake.Clientset) (*frrtypes.FRRConfiguration, error) {
	cfg, err := frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(
		context.TODO(), managedNamePrefix, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// Helper function to create test nodes
func createNode(name, ipv4, ipv6 string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
			Annotations: map[string]string{},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{},
		},
	}

	// Build annotation
	var annotation string
	if ipv4 != "" && ipv6 != "" {
		annotation = `{"ipv4":"` + ipv4 + `/24","ipv6":"` + ipv6 + `/64"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv4},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv6},
		)
	} else if ipv4 != "" {
		annotation = `{"ipv4":"` + ipv4 + `/24"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv4},
		)
	} else if ipv6 != "" {
		annotation = `{"ipv6":"` + ipv6 + `/64"}`
		node.Status.Addresses = append(node.Status.Addresses,
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ipv6},
		)
	}

	if annotation != "" {
		node.Annotations[util.OvnNodeIfAddr] = annotation
	}

	return node
}

// Helper function to create a managed CUDN (NoOverlay + Managed routing)
func createManagedCUDN(name string, labels map[string]string) *userdefinednetworkv1.ClusterUserDefinedNetwork {
	return &userdefinednetworkv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: userdefinednetworkv1.ClusterUserDefinedNetworkSpec{
			Network: userdefinednetworkv1.NetworkSpec{
				Topology: userdefinednetworkv1.NetworkTopologyLayer3,
				Layer3: &userdefinednetworkv1.Layer3Config{
					Role: userdefinednetworkv1.NetworkRolePrimary,
				},
				Transport: userdefinednetworkv1.TransportOptionNoOverlay,
				NoOverlay: &userdefinednetworkv1.NoOverlayConfig{
					OutboundSNAT: userdefinednetworkv1.SNATEnabled,
					Routing:      userdefinednetworkv1.RoutingManaged,
				},
			},
		},
	}
}

// Helper function to create an unmanaged CUDN (NoOverlay + Unmanaged routing)
func createUnmanagedCUDN(name string, labels map[string]string) *userdefinednetworkv1.ClusterUserDefinedNetwork {
	return &userdefinednetworkv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: userdefinednetworkv1.ClusterUserDefinedNetworkSpec{
			Network: userdefinednetworkv1.NetworkSpec{
				Topology: userdefinednetworkv1.NetworkTopologyLayer3,
				Layer3: &userdefinednetworkv1.Layer3Config{
					Role: userdefinednetworkv1.NetworkRolePrimary,
				},
				Transport: userdefinednetworkv1.TransportOptionNoOverlay,
				NoOverlay: &userdefinednetworkv1.NoOverlayConfig{
					OutboundSNAT: userdefinednetworkv1.SNATDisabled,
					Routing:      userdefinednetworkv1.RoutingUnmanaged,
				},
			},
		},
	}
}
