// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	raclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("No-Overlay: Default network is enabled with no-overlay", feature.NoOverlay, func() {
	f := wrappedTestFramework("no-overlay-default-network")
	const (
		tcpdumpPodName = "tcpdump-pod-no-overlay"
		serverPodName  = "server-pod-no-overlay"
		clientPodName  = "client-pod-no-overlay"
	)
	var clientPod, serverPod, tcpdumpPod *corev1.Pod
	var serverService *corev1.Service
	var nodes *corev1.NodeList

	ginkgo.JustBeforeEach(func() {
		var err error
		ginkgo.By("Selecting nodes")
		nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if len(nodes.Items) < 2 {
			ginkgo.Skip("requires at least 2 Nodes")
		}

		ginkgo.By("Creating server pod on first node")
		serverPod = e2epod.NewAgnhostPod(f.Namespace.Name, serverPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: netexecPort}}, "netexec")
		serverPod.Labels = map[string]string{"app": "no-overlay-server"}
		serverPod.Spec.NodeName = nodes.Items[0].Name
		e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

		ginkgo.By("Creating client pod on second node")
		clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, clientPodName, nil, nil, []corev1.ContainerPort{{ContainerPort: netexecPort}}, "netexec")
		clientPod.Spec.NodeName = nodes.Items[1].Name
		e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

		// Wait for pods to be ready and refresh their status
		ginkgo.By("Waiting for server pod to be ready")
		err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, serverPod.Name, f.Namespace.Name, 60*time.Second)
		framework.ExpectNoError(err, "Server pod failed to become ready")

		ginkgo.By("Waiting for client pod to be ready")
		err = e2epod.WaitTimeoutForPodReadyInNamespace(context.TODO(), f.ClientSet, clientPod.Name, f.Namespace.Name, 60*time.Second)
		framework.ExpectNoError(err, "Client pod failed to become ready")

		// Refresh pod status to get IP addresses
		serverPod, err = e2epod.NewPodClient(f).Get(context.TODO(), serverPod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get server pod status")

		clientPod, err = e2epod.NewPodClient(f).Get(context.TODO(), clientPod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get client pod status")

		framework.Logf("Server pod IPs: %v", serverPod.Status.PodIPs)
		framework.Logf("Client pod IPs: %v", clientPod.Status.PodIPs)

		// Verify pods have IP addresses
		gomega.Expect(serverPod.Status.PodIPs).NotTo(gomega.BeEmpty(), "Server pod should have at least one IP address")
		gomega.Expect(clientPod.Status.PodIPs).NotTo(gomega.BeEmpty(), "Client pod should have at least one IP address")

		ginkgo.By("Creating service to select server pod")
		familyPolicy := corev1.IPFamilyPolicyPreferDualStack
		serverService = e2eservice.CreateServiceSpec("no-overlay-server-service", "", false, map[string]string{"app": "no-overlay-server"})
		serverService.Spec.Ports = []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: netexecPort}}
		serverService.Spec.IPFamilyPolicy = &familyPolicy
		serverService, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(context.TODO(), serverService, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Failed to create server service")
		framework.Logf("Created service %s with ClusterIPs %v", serverService.Name, serverService.Spec.ClusterIPs)

		// Create tcpdump pod as host networked pod to capture traffic on the physical interface
		// In no-overlay mode pod IPs are routed directly and appear unencapsulated on the physical NIC.
		ginkgo.By("Creating tcpdump pod")
		tcpdumpPod, err = createPod(f, tcpdumpPodName, nodes.Items[1].Name, f.Namespace.Name,
			[]string{"sh", "-c", "sleep 20000"},
			map[string]string{},
			func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = images.Netshoot()
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"NET_RAW", "NET_ADMIN"},
					},
				}
			})
		framework.ExpectNoError(err, "Failed to create tcpdump pod")
		framework.Logf("tcpdumpPod pod IPs: %v", tcpdumpPod.Status.PodIPs)
	})

	ginkgo.When("connectivity tests", func() {

		ginkgo.It("should maintain pod2pod/pod2service/host2pod/host2service connectivity without overlay before and after ovnkube-node pod restarted", func() {
			// test traffic for pod2pod, host2pod, pod2service, host2service and verify no overlay traffic is captured by tcpdump
			ginkgo.By("Testing pod2pod connectivity without overlay on different node before ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2pod connectivity without overlay on different node before ovnkube-node pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Testing pod2service connectivity without overlay via service IPs before ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, clientPod, tcpdumpPod)

			ginkgo.By("Testing host2service connectivity without overlay via service IPs before ovnkube-node pod restart")
			// here use tcpdumpPod as the client (since it's host networked)
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Getting ovnkube-node pod on worker node")
			ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			ovnkubeNodePodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovnkube-node",
				FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
			})
			framework.ExpectNoError(err, "Failed to list ovnkube-node pods")
			gomega.Expect(ovnkubeNodePodList.Items).NotTo(gomega.BeEmpty(), "Should find ovnkube-node pod")
			ovnkubeNodePod := &ovnkubeNodePodList.Items[0]
			framework.Logf("Found ovnkube-node pod: %s on node %s", ovnkubeNodePod.Name, nodes.Items[0].Name)

			ginkgo.By("Deleting ovnkube-node pod to trigger restart")
			err = f.ClientSet.CoreV1().Pods(ovnNamespace).Delete(context.TODO(), ovnkubeNodePod.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to delete ovnkube-node pod")

			ginkgo.By("Waiting for new ovnkube-node pod to be ready")
			gomega.Eventually(func() bool {
				newOvnkubeNodePodList, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodes.Items[0].Name),
				})
				if err != nil {
					framework.Logf("Failed to list ovnkube-node pods: %v", err)
					return false
				}
				if len(newOvnkubeNodePodList.Items) == 0 {
					framework.Logf("No ovnkube-node pod found yet")
					return false
				}
				newOvnkubeNodePod := &newOvnkubeNodePodList.Items[0]
				// Check if it's a new pod (different UID)
				if newOvnkubeNodePod.UID == ovnkubeNodePod.UID {
					framework.Logf("Still the old pod, waiting for deletion to complete")
					return false
				}
				// Check if all containers are ready
				for _, condition := range newOvnkubeNodePod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						framework.Logf("New ovnkube-node pod %s is ready", newOvnkubeNodePod.Name)
						return true
					}
				}
				framework.Logf("New ovnkube-node pod %s is not ready yet", newOvnkubeNodePod.Name)
				return false
			}, 120*time.Second, 2*time.Second).Should(gomega.BeTrue(), "New ovnkube-node pod should be ready within 120 seconds")

			ginkgo.By("Verifying pod2pod connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2pod connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, tcpdumpPod, tcpdumpPod)

			ginkgo.By("Verifying pod2service connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, clientPod, tcpdumpPod)

			ginkgo.By("Verifying host2service connectivity after ovnkube-node pod restart")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, serverService.Spec.ClusterIPs, tcpdumpPod, tcpdumpPod)

			framework.Logf("Pod2pod and pod2service connectivity maintained after ovnkube-node pod restart - test passed!")
		})
	})

	ginkgo.When("unmanaged routing is enabled without RouteAdvertisements", func() {
		ginkgo.BeforeEach(func() {
			enabled, err := isDefaultNetworkNoOverlayUnmanagedWithoutRouteAdvertisements(f)
			framework.ExpectNoError(err, "Failed to check default network no-overlay unmanaged state")
			if !enabled {
				ginkgo.Skip("Test requires unmanaged no-overlay default network without RouteAdvertisements")
			}
		})

		ginkgo.It("should maintain pod2pod connectivity without RouteAdvertisements", func() {
			// the connectivity check alone would not distinguish this spec from
			// the generic no-overlay one: first prove no BGP-learned routes are
			// involved, so connectivity relies on the underlay routing alone.
			// The node may still hold BGP routes for unrelated prefixes (the
			// RouteAdvertisements feature can be enabled cluster-wide), so only
			// routes covering the pod IPs count.
			ginkgo.By("Verifying no BGP-learned route on the client node covers the server pod")
			var bgpRoutes []kernelRoute
			for _, ipCmd := range []string{"ip", "ip -6"} {
				out, err := e2epodoutput.RunHostCmd(tcpdumpPod.Namespace, tcpdumpPod.Name,
					ipCmd+" --json route show proto bgp")
				framework.ExpectNoError(err, "Failed to list BGP routes on the client node")
				var routes []kernelRoute
				framework.ExpectNoError(json.Unmarshal([]byte(out), &routes),
					"Failed to parse BGP routes %q", out)
				bgpRoutes = append(bgpRoutes, routes...)
			}
			expectNoRouteCoversPodIPs(bgpRoutes, serverPod.Status.PodIPs)

			ginkgo.By("Testing pod2pod connectivity without default-network RouteAdvertisements")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)
		})
	})

	ginkgo.When("managed mode routing is enabled", func() {
		ginkgo.BeforeEach(func() {
			if !isManagedRoutingEnabled() {
				ginkgo.Skip("Test requires managed routing mode to be enabled")
			}
		})

		ginkgo.It("should reconcile RA CR if manually deleted in managed mode", func() {
			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			framework.ExpectNoError(err, "Failed to create RouteAdvertisements client")

			ginkgo.By("Verifying auto-created RA exists before deletion")
			raName := managedRouteAdvertisementName(types.DefaultNetworkName)
			ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get RouteAdvertisement")
			gomega.Expect(ra).NotTo(gomega.BeNil(), "RouteAdvertisement should exist")
			originalUID := ra.UID
			framework.Logf("RouteAdvertisement %s exists before deletion (UID=%s)", raName, originalUID)

			ginkgo.By("Deleting RouteAdvertisement manually")
			err = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), raName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to delete RouteAdvertisement")

			ginkgo.By("Verifying RA is auto-recreated by the managed BGP controller")
			gomega.Eventually(func() bool {
				current, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
				return err == nil &&
					current.DeletionTimestamp == nil &&
					current.UID != originalUID
			}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue(), "Auto-created RA should be recreated within 30 seconds")

			ginkgo.By("Verifying RA is Accepted")
			gomega.Eventually(func() bool {
				ra, err = raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), raName, metav1.GetOptions{})
				if err != nil {
					framework.Logf("Failed to get RouteAdvertisement: %v", err)
					return false
				}
				acceptedCond := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				return acceptedCond != nil && acceptedCond.Status == metav1.ConditionTrue
			}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue(), "RouteAdvertisement should be Accepted within 30 seconds")

			ginkgo.By("Verifying RA has the expected label")
			expectedLabel := "k8s.ovn.org/managed-network"
			gomega.Expect(ra.Labels[expectedLabel]).To(gomega.Equal(types.DefaultNetworkName))

			ginkgo.By("Verifying RA selects managed FRRConfigurations")
			expectedFRRLabel := "k8s.ovn.org/managed-internal-fabric"
			gomega.Expect(ra.Spec.FRRConfigurationSelector.MatchLabels[expectedFRRLabel]).To(gomega.Equal("bgp"))
			ginkgo.By("Verifying it selects the default network")
			gomega.Expect(ra.Spec.NetworkSelectors).To(gomega.ContainElement(apitypes.NetworkSelector{NetworkSelectionType: apitypes.DefaultNetwork}))

			ginkgo.By("Verifying pod2pod connectivity works after RA recreation")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)
		})

		ginkgo.It("should reconcile FRRConfiguration if manually deleted in managed mode", func() {
			frrNamespace := deploymentconfig.Get().FRRK8sNamespace()

			ginkgo.By("Getting FRRConfigurations managed by the system")
			labelSelector := "k8s.ovn.org/managed-internal-fabric=bgp"
			getFRRCmd := []string{"get", "frrconfigurations", "-n", frrNamespace, "-l", labelSelector, "-o", "jsonpath={.items[0].metadata.name}"}
			frrName := e2ekubectl.RunKubectlOrDie(frrNamespace, getFRRCmd...)
			gomega.Expect(frrName).NotTo(gomega.BeEmpty(), "Should find at least one managed FRRConfiguration")
			framework.Logf("Found FRRConfiguration %s in namespace %s", frrName, frrNamespace)

			originalUID := strings.TrimSpace(e2ekubectl.RunKubectlOrDie(frrNamespace,
				"get", "frrconfigurations", frrName, "-n", frrNamespace, "-o", "jsonpath={.metadata.uid}"))
			gomega.Expect(originalUID).NotTo(gomega.BeEmpty(), "FRRConfiguration should have a UID")

			ginkgo.By("Deleting FRRConfiguration manually")
			deleteCmd := []string{"delete", "frrconfigurations", frrName, "-n", frrNamespace}
			e2ekubectl.RunKubectlOrDie(frrNamespace, deleteCmd...)
			framework.Logf("Deleted FRRConfiguration %s (UID was %s)", frrName, originalUID)

			ginkgo.By("Verifying the new FRRConfiguration is recreated (new UID, not terminating)")
			gomega.Eventually(func() bool {
				currentUID, err := e2ekubectl.RunKubectl(frrNamespace,
					"get", "frrconfigurations", frrName, "-n", frrNamespace, "-o", "jsonpath={.metadata.uid}")
				if err != nil {
					return false
				}
				currentUID = strings.TrimSpace(currentUID)
				if currentUID == "" || currentUID == originalUID {
					return false
				}
				dt, err := e2ekubectl.RunKubectl(frrNamespace,
					"get", "frrconfigurations", frrName, "-n", frrNamespace, "-o", "jsonpath={.metadata.deletionTimestamp}")
				if err != nil {
					return false
				}
				return strings.TrimSpace(dt) == ""
			}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue(),
				"FRRConfiguration %s should be recreated with a new UID and no deletion timestamp", frrName)

			ginkgo.By("Verifying pod2pod connectivity works after FRRConfiguration recreation")
			checkConnectivityWithoutOverlay(serverPod.Status.PodIPs, nil, clientPod, tcpdumpPod)
		})
	})
})

// getTcpdumpOnPhysicalIface starts tcpdump on the physical NIC (from deploymentconfig) filtered
// by pod IP, runs curlCmd, and returns the tcpdump output and curl output.
// In no-overlay mode pod IPs are routed directly and appear unencapsulated on the physical NIC.
// enp2s0 Out IP 10.131.0.123.49084 > 10.128.3.208.8080:
// In overlay mode pod IPs are going through genev_sys_6081
// genev_sys_6081 Out IP 10.131.0.23.39410 > 10.129.2.15.8080:
func getTcpdumpOnPhysicalIface(tcpdumpPod *corev1.Pod, clientPod *corev1.Pod, curlCmd string, podIP string) (string, string, error) {
	iface := deploymentconfig.Get().PrimaryInterfaceName()
	ginkgo.By(fmt.Sprintf("start tcpdump on physical interface %s to capture traffic", iface))
	// Save PID then verify process is still alive after a brief delay, so an immediate
	// tcpdump crash causes a non-zero exit code rather than silently succeeding.
	startCmd := fmt.Sprintf("sh -c 'rm -f /tmp/tcpdump.log /tmp/tcpdump.pid; tcpdump -ni %s tcp and host %s -n -s 0 -l > /tmp/tcpdump.log 2>&1 & PID=$!; echo $PID > /tmp/tcpdump.pid; sleep 0.2; kill -0 $PID'", iface, podIP)
	_, tcpdumpErr := e2epodoutput.RunHostCmdWithRetries(
		tcpdumpPod.Namespace,
		tcpdumpPod.Name, startCmd,
		framework.Poll,
		10*time.Second)
	framework.ExpectNoError(tcpdumpErr, "tcpdump failed to start on interface %s", iface)

	ginkgo.By("Generating tcp traffic")
	framework.Logf("Testing connectivity with command %q", curlCmd)
	curlOutput, curlErr := e2epodoutput.RunHostCmdWithRetries(
		clientPod.Namespace,
		clientPod.Name,
		curlCmd,
		framework.Poll,
		10*time.Second)

	// Always stop tcpdump and collect output before returning, so the background
	// process is not left running even when curl failed.
	collectCmd := "sh -c 'sleep 0.5; kill -INT $(cat /tmp/tcpdump.pid) >/dev/null 2>&1 || true; sleep 1; cat /tmp/tcpdump.log'"
	tcpdumpOut, err := e2epodoutput.RunHostCmdWithRetries(tcpdumpPod.Namespace, tcpdumpPod.Name, collectCmd, framework.Poll, 10*time.Second)
	framework.ExpectNoError(err, "Failed to collect tcpdump output")
	framework.Logf("tcpdump output:\n%s", tcpdumpOut)
	framework.Logf("curl output:\n%s", curlOutput)
	return tcpdumpOut, curlOutput, curlErr
}

// checkConnectivityWithoutOverlay verifies no-overlay connectivity from clientPod and asserts
// that traffic is visible unencapsulated on the physical NIC via tcpdump.
//
// serverPodIPs is always the real server pod IPs; it is used both as the curl destination
// (pod-to-pod) and as the tcpdump filter (the actual endpoint seen on the wire).
// When serviceClusterIPs is non-nil the curl destination switches to those VIPs instead
// (pod-to-service), while tcpdump still filters on the server pod IP backing the VIP.
func checkConnectivityWithoutOverlay(serverPodIPs []corev1.PodIP, serviceClusterIPs []string, clientPod, tcpdumpPod *corev1.Pod) {
	// destIPs: what to curl to.
	var destIPs []string
	if len(serviceClusterIPs) > 0 {
		destIPs = serviceClusterIPs
	} else {
		for _, pip := range serverPodIPs {
			destIPs = append(destIPs, pip.IP)
		}
	}

	for _, destIP := range destIPs {
		// tcpdump always filters on the server pod IP.
		// For pod-to-pod this equals destIP; for pod-to-service it is the pod backing the VIP.
		var filterIP string
		for _, pip := range serverPodIPs {
			if utilnet.IsIPv6String(pip.IP) == utilnet.IsIPv6String(destIP) {
				filterIP = pip.IP
				break
			}
		}
		gomega.Expect(filterIP).NotTo(gomega.BeEmpty(), "Could not find server pod IP matching family of %s", destIP)

		ginkgo.By(fmt.Sprintf("curl %s", destIP))
		curlCmd := fmt.Sprintf("curl -s -m 2 %s/clientip", net.JoinHostPort(destIP, fmt.Sprint(netexecPort)))
		// In no-overlay mode pod IPs are routed directly; capturing on the physical NIC with a
		// pod IP filter proves the traffic is unencapsulated (overlay would hide it inside Geneve).
		tcpdumpOut, _, curlErr := getTcpdumpOnPhysicalIface(tcpdumpPod, clientPod, curlCmd, filterIP)
		framework.ExpectNoError(curlErr, "curl to %s failed", destIP)
		gomega.Expect(tcpdumpOut).To(gomega.MatchRegexp(`(?m)^[1-9][0-9]* packets captured`),
			"Should capture unencapsulated pod traffic on the physical interface")
	}
}

// expectNoRouteCoversPodIPs fails the test if any of the routes covers one
// of the given pod IPs. A default route always counts as covering them.
func expectNoRouteCoversPodIPs(routes []kernelRoute, podIPs []corev1.PodIP) {
	ginkgo.GinkgoHelper()
	for _, route := range routes {
		if route.Dst == "default" {
			framework.Failf("found a default route: %+v", route)
		}
		_, cidr, err := net.ParseCIDR(route.Dst)
		if err != nil {
			// plain host route without a mask
			ip := net.ParseIP(route.Dst)
			if ip == nil {
				continue
			}
			cidr = util.GetIPNetFullMaskFromIP(ip)
		}
		for _, podIP := range podIPs {
			gomega.Expect(cidr.Contains(net.ParseIP(podIP.IP))).To(gomega.BeFalse(),
				"route to %s covers pod IP %s", route.Dst, podIP.IP)
		}
	}
}

func isManagedRoutingEnabled() bool {
	ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	args := []string{"get", "configmap", "ovnkube-config", "-o=jsonpath={.data.ovnkube\\.conf}"}
	conf := e2ekubectl.RunKubectlOrDie(ovnKubeNamespace, args...)
	// Simplistic check for routing = managed
	if strings.Contains(conf, "routing = managed") || strings.Contains(conf, "routing=managed") {
		framework.Logf("Managed routing is enabled in ovnkube-config")
		return true
	}
	return false
}

func isDefaultNetworkNoOverlayUnmanagedWithoutRouteAdvertisements(f *framework.Framework) (bool, error) {
	if isManagedRoutingEnabled() {
		return false, nil
	}
	hasDefaultNetworkRA, err := isAnyDefaultNetworkPodNetworkRouteAdvertisementsConfigured(f)
	if err != nil {
		return false, err
	}
	return !hasDefaultNetworkRA, nil
}

func isAnyDefaultNetworkPodNetworkRouteAdvertisementsConfigured(f *framework.Framework) (bool, error) {
	raClient, err := raclientset.NewForConfig(f.ClientConfig())
	if err != nil {
		return false, err
	}
	raList, err := raClient.K8sV1().RouteAdvertisements().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		// The RouteAdvertisements CRD is not installed when the feature is
		// disabled: no RouteAdvertisements are configured then.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	for i := range raList.Items {
		if util.RAAdvertisesDefaultNetwork(&raList.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}

// managedRouteAdvertisementName matches clustermanager/managedbgp.ManagedRouteAdvertisementName
// ("ovnk-managed-" + hex(fnv64a(networkName)))
func managedRouteAdvertisementName(networkName string) string {
	const prefix = "ovnk-managed-"
	h := fnv.New64a()
	_, _ = h.Write([]byte(networkName))
	return fmt.Sprintf("%s%x", prefix, h.Sum64())
}
