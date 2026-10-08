// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

var _ = Describe("Network Segmentation: Localnet", feature.NetworkSegmentation, func() {
	var (
		f           = wrappedTestFramework("network-segmentation-localnet")
		providerCtx infraapi.Context
	)
	f.SkipNamespaceCreation = true

	BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()
	})

	It("using ClusterUserDefinedNetwork CR, pods in different namespaces, should communicate over localnet topology", func(ctx SpecContext) {
		const (
			vlan              = 200
			testPort          = 9000
			subnetIPv4        = "192.168.100.0/24"
			subnetIPv6        = "2001:dbb::/64"
			excludeSubnetIPv4 = "192.168.100.0/29"
			excludeSubnetIPv6 = "2001:dbb::/120"
		)
		ovsBrName := "ovsbr-udn"
		// use unique names to avoid conflicts with tests running in parallel
		nsBlue := uniqueMetaName("blue")
		nsRed := uniqueMetaName("red")
		cudnName := uniqueMetaName("localnet-test")
		physicalNetworkName := uniqueMetaName("localnet1")

		By("setup the localnet underlay")
		c := networkAttachmentConfig{networkAttachmentConfigParams: networkAttachmentConfigParams{networkName: physicalNetworkName, vlanID: vlan}}
		Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
			BridgeName:         ovsBrName,
			LogicalNetworkName: c.networkName,
			VlanID:             c.vlanID,
		})).To(Succeed())

		By("create test namespaces")
		_, err := f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsRed}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.ClientSet.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsBlue}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup test namespaces")
			Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), nsBlue, metav1.DeleteOptions{})).To(Succeed())
			Expect(f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), nsRed, metav1.DeleteOptions{})).To(Succeed())
		})

		By("create CR selecting the test namespaces")
		netConf := networkAttachmentConfigParams{
			name:                cudnName,
			physicalNetworkName: physicalNetworkName,
			vlanID:              vlan,
			cidr:                joinStrings(subnetIPv4, subnetIPv6),
			excludeCIDRs:        []string{excludeSubnetIPv4, excludeSubnetIPv6},
		}

		cudnYAML := newLocalnetCUDNYaml(netConf, nsBlue, nsRed)
		cleanup, err := createManifest("", cudnYAML)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup CUDN CR")
			cleanup()
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", nsBlue))
			Expect(f.ClientSet.CoreV1().Pods(nsBlue).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", nsRed))
			Expect(f.ClientSet.CoreV1().Pods(nsRed).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
			Expect(err).NotTo(HaveOccurred())
		})
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName)).WithTimeout(5*time.Second).WithPolling(time.Second).
			Should(Succeed(), "CUDN CR is not ready")

		By("create test pods")
		serverPodCfg := podConfiguration{
			name:         "test-server",
			namespace:    nsBlue,
			attachments:  []nadapi.NetworkSelectionElement{{Name: cudnName}},
			containerCmd: httpServerContainerCmd(testPort),
		}
		clientPodCfg := podConfiguration{
			name:        "test-client",
			namespace:   nsRed,
			attachments: []nadapi.NetworkSelectionElement{{Name: cudnName}},
		}
		serverPod, err := f.ClientSet.CoreV1().Pods(serverPodCfg.namespace).Create(context.Background(), generatePodSpec(serverPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		clientPod, err := f.ClientSet.CoreV1().Pods(clientPodCfg.namespace).Create(context.Background(), generatePodSpec(clientPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, clientPod.Name, clientPod.Namespace)).To(Succeed())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), f.ClientSet, serverPod.Name, serverPod.Namespace)).To(Succeed())

		By("assert pods interface's MTU is set with default MTU (1500)")
		for _, cfg := range []podConfiguration{serverPodCfg, clientPodCfg} {
			mtuRAW, err := e2ekubectl.RunKubectl(cfg.namespace, "exec", cfg.name, "--", "cat", "/sys/class/net/net1/mtu")
			Expect(err).NotTo(HaveOccurred())
			Expect(mtuRAW).To(ContainSubstring("1500"))
		}

		By("assert pods IPs not in exclude range")
		serverIPs, err := podIPsForAttachment(f.ClientSet, serverPodCfg.namespace, serverPodCfg.name, cudnName)
		Expect(err).NotTo(HaveOccurred())
		clientIPs, err := podIPsForAttachment(f.ClientSet, clientPodCfg.namespace, clientPodCfg.name, cudnName)
		Expect(err).NotTo(HaveOccurred())
		podIPs := append(serverIPs, clientIPs...)
		for _, excludedRange := range netConf.excludeCIDRs {
			for _, podIP := range podIPs {
				Expect(inRange(excludedRange, podIP)).To(
					MatchError(fmt.Errorf("ip [%s] is NOT in range %s", podIP, excludedRange)))
			}
		}

		for _, serverIP := range serverIPs {
			By(fmt.Sprintf("asserting the *client* pod can contact the server pod exposed endpoint [%s:%d]", serverIP, testPort))
			Eventually(func() error {
				return reachServerPodFromClient(f.ClientSet, serverPodCfg, clientPodCfg, serverIP, testPort)
			}).WithTimeout(2 * time.Minute).WithPolling(6 * time.Second).Should(Succeed())
		}
	})

	It("should preserve LSPs for IPAM-less localnet pods after ovnkube-node restart", func(ctx SpecContext) {
		const (
			vlan = 201
		)
		var (
			ovsBrName           = "ovsbr-ipamless"
			cudnName            = uniqueMetaName("ipamless-localnet")
			physicalNetworkName = uniqueMetaName("physnet-ipamless")
		)

		By("setup the localnet underlay")
		c := networkAttachmentConfig{networkAttachmentConfigParams: networkAttachmentConfigParams{networkName: physicalNetworkName, vlanID: vlan}}
		Expect(infraprovider.SetupUnderlay(providerCtx, f, infraapi.Underlay{
			BridgeName:         ovsBrName,
			LogicalNetworkName: c.networkName,
			VlanID:             c.vlanID,
		})).To(Succeed())

		By("create test namespace")
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework": f.BaseName,
		})
		f.Namespace = namespace
		Expect(err).NotTo(HaveOccurred())

		By("create IPAM-less localnet CUDN CR (no subnets)")
		netConf := networkAttachmentConfigParams{
			name:                cudnName,
			physicalNetworkName: physicalNetworkName,
			vlanID:              vlan,
			cidr:                "", // Empty CIDR = IPAM-less
			excludeCIDRs:        []string{},
		}

		cudnYAML := newLocalnetIPAMLessCUDNYaml(netConf, f.Namespace.Name)
		cleanup, err := createManifest("", cudnYAML)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup CUDN CR")
			cleanup()
			By(fmt.Sprintf("delete pods in namespace %q to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
			Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
			Expect(err).NotTo(HaveOccurred())
		})
		Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName)).WithTimeout(5*time.Second).WithPolling(time.Second).
			Should(Succeed(), "CUDN CR is not ready")

		By("create test pod with IPAM-less localnet network")
		podCfg := podConfiguration{
			name:        "test-ipamless-pod",
			namespace:   f.Namespace.Name,
			attachments: []nadapi.NetworkSelectionElement{{Name: cudnName}},
			labels: map[string]string{
				"app": "test-ipamless",
			},
		}
		testPod, err := f.ClientSet.CoreV1().Pods(podCfg.namespace).Create(context.Background(), generatePodSpec(podCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			By("cleanup test pod")
			Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(context.Background(), testPod.Name, metav1.DeleteOptions{})).To(Succeed())
		})

		By("wait for pod to be running and annotated")
		Eventually(func() bool {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
			if err != nil {
				return false
			}
			// Check that pod has network annotation
			_, hasAnnotation := pod.Annotations["k8s.ovn.org/pod-networks"]
			return pod.Status.Phase == corev1.PodRunning && hasAnnotation
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(), "pod should be running and annotated")

		By("get node where pod is running")
		pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		nodeName := pod.Spec.NodeName
		framework.Logf("Pod is running on node: %s", nodeName)

		By("find ovnkube-node pod on the node where test pod is running")
		ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		ovnkubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ovnkubeNodePods.Items).NotTo(BeEmpty(), "should find ovnkube-node pod on node %s", nodeName)
		ovnkubeNodePod := ovnkubeNodePods.Items[0]
		framework.Logf("Found ovnkube-node pod: %s on node %s", ovnkubeNodePod.Name, nodeName)

		By("verify LSP exists in OVN NB database")
		// Expected LSP name format: GetUserDefinedNetworkPrefix(nadName) + composePortName(podNamespace, podName)
		// NAD name for CUDN is: <namespace>/<cudn-name>
		// GetUserDefinedNetworkPrefix replaces hyphens and slashes with dots and adds underscore
		nadName := fmt.Sprintf("%s/%s", f.Namespace.Name, cudnName)
		nadNameWithDots := strings.ReplaceAll(strings.ReplaceAll(nadName, "-", "."), "/", ".")
		expectedLSPName := fmt.Sprintf("%s_%s_%s", nadNameWithDots, f.Namespace.Name, testPod.Name)
		framework.Logf("Expected LSP name: %s", expectedLSPName)
		findLSP := func() (string, error) {
			stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, ovnkubeNodePod.Name, "nb-ovsdb",
				"ovn-nbctl", "get", "logical-switch-port", expectedLSPName, "_uuid")
			if err != nil {
				return "", fmt.Errorf("failed to find LSP %w, stderr: %s", err, stderr)
			}
			return stdout, nil
		}
		expectedLSP, err := findLSP()
		Expect(err).ToNot(HaveOccurred())
		Expect(expectedLSP).ToNot(BeEmpty())

		By("restart ovnkube-node pod on the node where test pod is running")
		err = restartOVNKubeNodePod(f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(), nodeName)
		Expect(err).NotTo(HaveOccurred(), "should successfully restart ovnkube-node pod")

		By("wait for ovnkube-node to complete sync")
		// Give ovnkube-node time to complete sync operation
		time.Sleep(30 * time.Second)

		By("find new ovnkube-node pod after restart")
		ovnkubeNodePods, err = f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ovnkubeNodePods.Items).NotTo(BeEmpty(), "should find ovnkube-node pod on node %s after restart", nodeName)
		ovnkubeNodePod = ovnkubeNodePods.Items[0]
		framework.Logf("Found new ovnkube-node pod after restart: %s on node %s", ovnkubeNodePod.Name, nodeName)

		By("verify LSP still exists after ovnkube-node restart (this will fail due to bug)")
		Consistently(findLSP).WithTimeout(10*time.Second).WithPolling(2*time.Second).
			Should(Equal(expectedLSP), "LSP should not have being recreated after ovnkube-node restart")

		By("verify pod still has network annotation")
		pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), testPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, hasAnnotation := pod.Annotations["k8s.ovn.org/pod-networks"]
		Expect(hasAnnotation).To(BeTrue(), "pod should still have network annotation")
	})
})

func newLocalnetCUDNYaml(params networkAttachmentConfigParams, selectedNamespaces ...string) string {
	selectedNs := strings.Join(selectedNamespaces, ",")
	excludeSubnets := strings.Join(params.excludeCIDRs, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + selectedNs + `]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ` + params.physicalNetworkName + `
      subnets: [` + params.cidr + `]
      excludeSubnets: [` + excludeSubnets + `]
      vlan:
        mode: Access
        access: {id: ` + strconv.Itoa(params.vlanID) + `}
`
}

// newLocalnetIPAMLessCUDNYaml creates a ClusterUserDefinedNetwork YAML for IPAM-less localnet topology
func newLocalnetIPAMLessCUDNYaml(params networkAttachmentConfigParams, selectedNamespaces ...string) string {
	selectedNs := strings.Join(selectedNamespaces, ",")
	// For IPAM-less, we explicitly set ipam.mode to Disabled
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + selectedNs + `]
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: ` + params.physicalNetworkName + `
      ipam:
        mode: Disabled
      vlan:
        mode: Access
        access: {id: ` + strconv.Itoa(params.vlanID) + `}
`
}

// uniqueMetaName generate unique name from given string that complies with metadata object name.
func uniqueMetaName(originalName string) string {
	const randomStringLength = 5
	return fmt.Sprintf("%s-%s", originalName, rand.String(randomStringLength))
}
