// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

// Temporary nodes let us exercise controller failures without damaging the
// subnet annotations used by running workloads. They do not run FRR daemons:
// this verifies API reconciliation, not BGP convergence or datapath behavior.
var _ = ginkgo.Describe("BGP partial node configuration failure", feature.RouteAdvertisements, ginkgo.Serial, func() {
	f := wrappedTestFramework("ra-partial-failure")
	ginkgo.It("preserves failed pod-network configurations and recovers without blocking healthy nodes", func(ctx context.Context) {
		if IsGatewayModeLocal(f.ClientSet) {
			e2eskipper.Skipf("DPU-host annotation failure injection requires shared gateway mode")
		}
		raf, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)
		frr, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)
		const testLabel = "e2e.ovn.org/ra-partial-failure"
		selector := metav1.LabelSelector{MatchLabels: map[string]string{testLabel: f.Namespace.Name}}
		nodeNames := []string{f.Namespace.Name + "-healthy", f.Namespace.Name + "-failing"}
		for _, name := range nodeNames {
			_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
					testLabel: f.Namespace.Name, corev1.LabelHostname: name,
				}},
				Spec: corev1.NodeSpec{Unschedulable: true},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "create temporary node %s", name)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				framework.ExpectNoError(f.ClientSet.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}))
			})
		}
		// Let cluster-manager allocate real subnets so this test does not
		// introduce duplicate or out-of-range allocations.
		for _, name := range nodeNames {
			gomega.Eventually(func() error {
				node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				subnets, err := util.ParseNodeHostSubnetsAnnotation(node)
				if err != nil {
					return err
				}
				if len(subnets[types.DefaultNetworkName]) == 0 {
					return fmt.Errorf("node %s has no default subnet", name)
				}
				return nil
			}, 60*time.Second, time.Second).Should(gomega.Succeed(), "allocate temporary node subnet")
		}
		_, err = frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Create(ctx, &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "peers", Labels: selector.MatchLabels},
			Spec: frrapi.FRRConfigurationSpec{NodeSelector: selector, BGP: frrapi.BGPConfig{Routers: []frrapi.Router{{
				ASN: 64512, Neighbors: []frrapi.Neighbor{{ASN: 64513, Address: "192.0.2.1"}},
			}}}},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create isolated peering template")
		ginkgo.DeferCleanup(func(ctx context.Context) {
			framework.ExpectNoError(frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Delete(ctx, "peers", metav1.DeleteOptions{}))
		})
		ra, err := raf.K8sV1().RouteAdvertisements().Create(ctx, &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: f.Namespace.Name},
			Spec: rav1.RouteAdvertisementsSpec{
				NetworkSelectors: apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.DefaultNetwork}},
				NodeSelector:     selector, FRRConfigurationSelector: selector,
				Advertisements: []rav1.AdvertisementType{rav1.PodNetwork},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create isolated advertisement")
		ginkgo.DeferCleanup(func(ctx context.Context) {
			framework.ExpectNoError(raf.K8sV1().RouteAdvertisements().Delete(ctx, ra.Name, metav1.DeleteOptions{}))
		})
		waitForState := func(degraded bool) map[string]frrapi.FRRConfiguration {
			var byNode map[string]frrapi.FRRConfiguration
			gomega.Eventually(func(g gomega.Gomega) {
				current, err := raf.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				accepted := meta.FindStatusCondition(current.Status.Conditions, "Accepted")
				g.Expect(accepted).NotTo(gomega.BeNil())
				g.Expect(accepted.ObservedGeneration).To(gomega.Equal(current.Generation))
				g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionTrue))
				condition := meta.FindStatusCondition(current.Status.Conditions, "Degraded")
				g.Expect(condition).NotTo(gomega.BeNil())
				g.Expect(condition.ObservedGeneration).To(gomega.Equal(current.Generation))
				g.Expect(condition.Status == metav1.ConditionTrue).To(gomega.Equal(degraded))
				configs, err := frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).List(ctx, metav1.ListOptions{
					LabelSelector: types.OvnRouteAdvertisementsKey + "=" + ra.Name,
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(configs.Items).To(gomega.HaveLen(2))
				byNode = map[string]frrapi.FRRConfiguration{}
				for _, cfg := range configs.Items {
					byNode[cfg.Labels[types.OvnRouteAdvertisementsNodeKey]] = cfg
				}
				for i, name := range nodeNames {
					g.Expect(byNode).To(gomega.HaveKey(name))
					if degraded && i == 1 {
						g.Expect(byNode[name].Annotations).NotTo(gomega.HaveKey(types.OvnRouteAdvertisementsReadyKey))
					} else {
						g.Expect(byNode[name].Annotations[types.OvnRouteAdvertisementsReadyKey]).To(gomega.Equal(util.RouteAdvertisementsReadyToken(current, 1)))
					}
				}
			}, 60*time.Second, time.Second).Should(gomega.Succeed())
			return byNode
		}
		original := waitForState(false)
		node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeNames[1], metav1.GetOptions{})
		framework.ExpectNoError(err)
		var originalValue any
		if value, exists := node.Annotations[util.OVNNodePrimaryDPUHostAddr]; exists {
			originalValue = value
		}
		patchAnnotation := func(ctx context.Context, value any) {
			patch, err := json.Marshal(map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{util.OVNNodePrimaryDPUHostAddr: value}},
			})
			framework.ExpectNoError(err)
			_, err = f.ClientSet.CoreV1().Nodes().Patch(ctx, node.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
			framework.ExpectNoError(err, "patch DPU-host address annotation")
		}
		// A nil merge-patch value removes an annotation that was absent before
		// the test. Register cleanup before injecting the failure.
		restore := func(ctx context.Context) { patchAnnotation(ctx, originalValue) }
		ginkgo.DeferCleanup(restore)
		// Unlike node-subnets, the allocator does not repair this annotation.
		// Shared-gateway RA generation will keep failing until we restore it.
		patchAnnotation(ctx, "invalid")
		degraded := waitForState(true)
		for _, name := range nodeNames {
			gomega.Expect(degraded[name].UID).To(gomega.Equal(original[name].UID), "configuration should not be recreated")
			gomega.Expect(degraded[name].Spec).To(gomega.Equal(original[name].Spec), "retain existing node configuration")
		}
		// Force new work on the healthy node while the other node still fails.
		// Merely observing its original configuration would not detect a
		// controller that aborted generation for every node.
		framework.ExpectNoError(frr.ApiV1beta1().FRRConfigurations(f.Namespace.Name).Delete(
			ctx, degraded[nodeNames[0]].Name, metav1.DeleteOptions{}))
		regenerated := waitForState(true)
		gomega.Expect(regenerated[nodeNames[0]].UID).NotTo(gomega.Equal(original[nodeNames[0]].UID))
		gomega.Expect(regenerated[nodeNames[1]].UID).To(gomega.Equal(original[nodeNames[1]].UID))
		restore(ctx)
		recovered := waitForState(false)
		gomega.Expect(recovered[nodeNames[1]].UID).To(gomega.Equal(original[nodeNames[1]].UID))
	})
})
