// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"time"

	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("BGP: UDN service marks after advertisement updates", feature.RouteAdvertisements, func() {
	f := wrappedTestFramework("ra-service-marks")
	f.SkipNamespaceCreation = true

	ginkgo.It("refreshes unchanged services when network selection changes", func(ctx ginkgo.SpecContext) {
		ictx := infraprovider.Get().NewTestContext()
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, f.ClientSet, 1)
		framework.ExpectNoError(err)
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty())
		nodeName := nodes.Items[0].Name
		suffix := framework.RandomSuffix()
		names := []string{"marks-a-" + suffix, "marks-b-" + suffix}
		const labelKey = "e2e-ra-service-marks"
		var namespaces []*corev1.Namespace
		var routers []frrapi.Router
		for _, name := range names {
			ns, err := f.CreateNamespace(ctx, name, map[string]string{RequiredUDNNamespaceLabel: ""})
			framework.ExpectNoError(err)
			namespaces = append(namespaces, ns)
			if len(namespaces) == 1 {
				f.Namespace = ns
			}
			allocation, err := allocators.AllocateBGP(f, ictx)
			framework.ExpectNoError(err)
			spec := &udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{Role: udnv1.NetworkRolePrimary,
					Subnets: filterL3Subnets(f.ClientSet, []udnv1.Layer3Subnet{
						{CIDR: udnv1.CIDR(allocation.UDNSubnet), HostSubnet: 24},
						{CIDR: udnv1.CIDR(allocation.UDNSubnet6), HostSubnet: 64},
					}),
				},
			}
			framework.ExpectNoError(createUserDefinedNetwork(f, ictx, ns, name, true, spec, map[string]string{labelKey: suffix, labelKey + "-network": name}))
			ginkgo.DeferCleanup(func() error {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				return f.ClientSet.CoreV1().Pods(ns.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
			})
			// Keep both networks active on the observed node with dynamic allocation.
			_, err = createPod(f, "keep-network-active", nodeName, ns.Name, []string{"bash", "-c", "sleep infinity"}, nil)
			framework.ExpectNoError(err)
			// No live peer is needed: this test checks host service reconciliation.
			routers = append(routers, frrapi.Router{ASN: 64512, VRF: name,
				Neighbors: []frrapi.Neighbor{{ASN: 64513, Address: "192.0.2.1"}},
			})
		}

		// No selector means no endpoint churn can accidentally resync this Service.
		service, err := f.ClientSet.CoreV1().Services(namespaces[0].Name).Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "unchanged-nodeport"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(8080)}},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		markPattern := regexp.MustCompile(fmt.Sprintf(`\b(?:tcp|6)\s*\.\s*%d\s*:\s*jump\s+(udn-mark-[[:alnum:]]+)`, service.Spec.Ports[0].NodePort))
		var originalMark string
		expectMark := func(present bool) {
			ginkgo.GinkgoHelper()
			gomega.Eventually(ctx, func(g gomega.Gomega) {
				output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"nft", "-n", "list", "map", "inet", "ovn-kubernetes", "udn-mark-nodeports"})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				match := markPattern.FindStringSubmatch(output)
				if !present {
					g.Expect(match).To(gomega.BeEmpty(), "stale service mark: %s", output)
					return
				}
				g.Expect(match).To(gomega.HaveLen(2), "missing service mark: %s", output)
				if originalMark == "" {
					originalMark = match[1]
				}
				g.Expect(match[1]).To(gomega.Equal(originalMark))
			}, time.Minute, time.Second).Should(gomega.Succeed())
		}
		ginkgo.By("observing the service mark before creating the advertisement")
		expectMark(true)

		frr, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)
		source, err := frr.ApiV1beta1().FRRConfigurations(deploymentconfig.Get().FRRK8sNamespace()).Create(ctx, &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "marks-" + suffix, Labels: map[string]string{labelKey: suffix}},
			Spec:       frrapi.FRRConfigurationSpec{BGP: frrapi.BGPConfig{Routers: routers}},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return frr.ApiV1beta1().FRRConfigurations(source.Namespace).Delete(ctx, source.Name, metav1.DeleteOptions{})
		})
		client, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err)
		ra, err := client.K8sV1().RouteAdvertisements().Create(ctx, &rav1.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: "marks-" + suffix},
			Spec: rav1.RouteAdvertisementsSpec{TargetVRF: "auto", Advertisements: []rav1.AdvertisementType{rav1.PodNetwork},
				FRRConfigurationSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelKey: suffix}},
				NetworkSelectors: apitypes.NetworkSelectors{{NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{NetworkSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelKey: suffix}}}},
				},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return client.K8sV1().RouteAdvertisements().Delete(ctx, ra.Name, metav1.DeleteOptions{})
		})
		waitAccepted := func() {
			ginkgo.GinkgoHelper()
			gomega.Eventually(ctx, func(g gomega.Gomega) {
				current, err := client.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				condition := meta.FindStatusCondition(current.Status.Conditions, "Accepted")
				g.Expect(condition).NotTo(gomega.BeNil())
				g.Expect(condition.ObservedGeneration).To(gomega.Equal(current.Generation))
				g.Expect(condition.Status).To(gomega.Equal(metav1.ConditionTrue))
			}, time.Minute, time.Second).Should(gomega.Succeed())
		}
		waitAccepted()
		expectMark(false)
		for _, selected := range []bool{false, true} {
			ginkgo.By(fmt.Sprintf("updating only networkSelectors, selecting the service network: %t", selected))
			ra, err = client.K8sV1().RouteAdvertisements().Get(ctx, ra.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			selector := &ra.Spec.NetworkSelectors[0].ClusterUserDefinedNetworkSelector.NetworkSelector
			if selected {
				delete(selector.MatchLabels, labelKey+"-network")
			} else {
				selector.MatchLabels[labelKey+"-network"] = names[1]
			}
			ra, err = client.K8sV1().RouteAdvertisements().Update(ctx, ra, metav1.UpdateOptions{})
			framework.ExpectNoError(err)
			waitAccepted()
			expectMark(!selected)
		}
		unchanged, err := f.ClientSet.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(unchanged.UID).To(gomega.Equal(service.UID))
		gomega.Expect(unchanged.ResourceVersion).To(gomega.Equal(service.ResourceVersion))
	})
})
