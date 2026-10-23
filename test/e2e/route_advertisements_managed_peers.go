// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	raclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

// Temporary unschedulable nodes exercise managed template regeneration without
// changing workload-node addresses. This verifies API state, not live peering.
var _ = ginkgo.Describe("BGP: managed self-peer filtering", feature.RouteAdvertisements, ginkgo.Serial, func() {
	f := wrappedTestFramework("managed-self-peers")
	ginkgo.It("filters InternalIPs and reconciles address changes without a primary address annotation", func(ctx ginkgo.SpecContext) {
		frr, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create FRR client")
		raf, err := raclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create RA client")
		namespace := deploymentconfig.Get().FRRK8sNamespace()
		raName := types.ManagedBGPConfigurationName + "-default"
		_, err = raf.K8sV1().RouteAdvertisements().Get(ctx, raName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ginkgo.Skip("requires managed BGP for the default network")
		}
		framework.ExpectNoError(err, "get managed default-network advertisement")
		_, err = frr.ApiV1beta1().FRRConfigurations(namespace).Get(ctx, types.ManagedBGPConfigurationName, metav1.GetOptions{})
		framework.ExpectNoError(err, "get managed full-mesh template")
		name := "self-peer-" + framework.RandomSuffix()
		_, err = f.ClientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create temporary node")
		var testAddresses []string
		ginkgo.DeferCleanup(func(ctx context.Context) {
			err := f.ClientSet.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				framework.ExpectNoError(err, "delete temporary node")
			}
			// Node deletion and both configuration reconciliations are asynchronous.
			// Wait for all managed advertisements before allowing the next spec.
			gomega.Eventually(ctx, func(g gomega.Gomega) {
				source, err := frr.ApiV1beta1().FRRConfigurations(namespace).Get(ctx, types.ManagedBGPConfigurationName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "read full-mesh template during cleanup")
				configs, err := frr.ApiV1beta1().FRRConfigurations(namespace).List(ctx, metav1.ListOptions{LabelSelector: types.OvnRouteAdvertisementsKey})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "list generated configurations during cleanup")
				configs.Items = append(configs.Items, *source)
				for _, cfg := range configs.Items {
					if cfg.Name != source.Name {
						key := strings.Split(cfg.Annotations[types.OvnRouteAdvertisementsKey], "/")
						if len(key) != 3 || key[1] != source.Name {
							continue
						}
						g.Expect(key[2]).NotTo(gomega.Equal(name), "temporary node configuration deleted")
					}
					for _, router := range cfg.Spec.BGP.Routers {
						for _, peer := range router.Neighbors {
							g.Expect(testAddresses).NotTo(gomega.ContainElement(peer.Address), "configuration %s drops temporary node IPs", cfg.Name)
						}
					}
				}
			}, time.Minute, time.Second).Should(gomega.Succeed())
		})
		ipv4Supported, ipv6Supported := getSupportedIPFamilies(f.ClientSet)
		var previous []string
		for _, addresses := range [][]string{{"192.0.2.123", "2001:db8::123"}, {"192.0.2.124", "2001:db8::124"}} {
			addresses = slices.DeleteFunc(addresses, func(address string) bool {
				if utilnet.IsIPv6String(address) {
					return !ipv6Supported
				}
				return !ipv4Supported
			})
			testAddresses = append(testAddresses, addresses...)
			ginkgo.By(fmt.Sprintf("setting temporary node InternalIPs to %v", addresses))
			framework.ExpectNoError(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				node.Status.Addresses = nil
				for _, ip := range addresses {
					node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
				}
				_, err = f.ClientSet.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
				return err
			}), "update temporary node InternalIPs")
			gomega.Eventually(ctx, func(g gomega.Gomega) {
				node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "get temporary node")
				g.Expect(node.Annotations).NotTo(gomega.HaveKey(util.OvnNodeIfAddr), "no kubelet primary address annotation")
				subnets, err := util.ParseNodeHostSubnetsAnnotation(node)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "wait for real subnet allocation")
				g.Expect(subnets[types.DefaultNetworkName]).NotTo(gomega.BeEmpty(), "default subnet allocated")
				source, err := frr.ApiV1beta1().FRRConfigurations(namespace).Get(ctx, types.ManagedBGPConfigurationName, metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "read updated full-mesh template")
				g.Expect(source.Spec.BGP.Routers).To(gomega.HaveLen(1))
				var peers, expected []string
				for _, peer := range source.Spec.BGP.Routers[0].Neighbors {
					peers = append(peers, peer.Address)
					if slices.Contains(addresses, peer.Address) {
						continue
					}
					if slices.ContainsFunc(subnets[types.DefaultNetworkName], func(subnet *net.IPNet) bool {
						return utilnet.IsIPv6CIDR(subnet) == utilnet.IsIPv6String(peer.Address)
					}) {
						expected = append(expected, peer.Address)
					}
				}
				for _, ip := range addresses {
					g.Expect(peers).To(gomega.ContainElement(ip), "template includes current node IP")
				}
				for _, ip := range previous {
					g.Expect(peers).NotTo(gomega.ContainElement(ip), "template removes old node IP")
				}
				g.Expect(expected).NotTo(gomega.BeEmpty(), "retain real cluster peers")
				configs, err := frr.ApiV1beta1().FRRConfigurations(namespace).List(ctx, metav1.ListOptions{LabelSelector: types.OvnRouteAdvertisementsKey + "=" + raName})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "list generated configurations")
				found, foundOther := false, false
				for _, cfg := range configs.Items {
					key := cfg.Annotations[types.OvnRouteAdvertisementsKey]
					if !strings.HasPrefix(key, raName+"/"+source.Name+"/") {
						continue
					}
					if key != raName+"/"+source.Name+"/"+name {
						// Another node must replace the old peer address too. The
						// temporary node's own filtered peer list can stay unchanged.
						foundOther = true
						g.Expect(cfg.Spec.BGP.Routers).To(gomega.HaveLen(1))
						router := cfg.Spec.BGP.Routers[0]
						var actual []string
						for _, peer := range router.Neighbors {
							actual = append(actual, peer.Address)
						}
						for _, ip := range addresses {
							if slices.ContainsFunc(router.Prefixes, func(prefix string) bool {
								return utilnet.IsIPv6CIDRString(prefix) == utilnet.IsIPv6String(ip)
							}) {
								g.Expect(actual).To(gomega.ContainElement(ip), "other node learns updated peer")
							}
						}
						for _, ip := range previous {
							g.Expect(actual).NotTo(gomega.ContainElement(ip), "other node drops stale peer")
						}
						continue
					}
					found = true
					g.Expect(cfg.Spec.BGP.Routers).To(gomega.HaveLen(1))
					var actual []string
					for _, peer := range cfg.Spec.BGP.Routers[0].Neighbors {
						actual = append(actual, peer.Address)
					}
					g.Expect(actual).To(gomega.ConsistOf(expected), "exclude self while retaining other same-family peers")
				}
				g.Expect(found).To(gomega.BeTrue(), "temporary node configuration generated")
				g.Expect(foundOther).To(gomega.BeTrue(), "existing node configuration updated")
			}, time.Minute, time.Second).Should(gomega.Succeed())
			previous = addresses
		}
	})
})
