// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
)

var _ = Describe("Network Segmentation: Default network multus annotation", feature.NetworkSegmentation, func() {
	var (
		f = wrappedTestFramework("default-network-annotation")
	)
	f.SkipNamespaceCreation = true

	type testCase struct {
		ips       []string
		mac       string
		lifecycle udnv1.NetworkIPAMLifecycle
	}
	DescribeTable("when added with static IP and MAC to a pod belonging to primary UDN", func(ctx SpecContext, tc testCase) {
		if !isPreConfiguredUdnAddressesEnabled() {
			Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
		}
		tc.ips = filterCIDRs(f.ClientSet, tc.ips...)
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
		f.Namespace = namespace

		// Create the UDN client using the framework's config
		udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

		// Define the UserDefinedNetwork object
		udn := &udnv1.UserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "l2network",
				Namespace: f.Namespace.Name,
			},
			Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{"103.0.0.0/16", "2014:100:200::0/60"}),
					IPAM:    &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled, Lifecycle: tc.lifecycle},
				},
			},
		}

		// Create the resource in the generated namespace
		By("Create a UserDefinedNetwork with Layer2 topology and wait for availability")
		udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(context.TODO(), udn, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should create UserDefinedNetwork")
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udn.Namespace, udn.Name), 5*time.Second, time.Second).Should(Succeed())

		// Create the Pod in the generated namespace
		By("Create a Pod with the default network annotation and wait for readiness")
		ips, err := json.Marshal(tc.ips)
		Expect(err).NotTo(HaveOccurred(), "Should marshal IPs for annotation")

		// Define the Pod object with the specified annotation
		By("Creating the pod with the default network annotation and wait for readiness")
		pod := e2epod.NewAgnhostPod(f.Namespace.Name, "static-ip-mac-pod", nil, nil, nil)
		pod.Annotations = map[string]string{
			"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, tc.mac, string(ips)),
		}
		pod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
		pod = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod)

		netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
			return status.Default
		})
		Expect(err).NotTo(HaveOccurred(), "Should get network status from pod")
		Expect(netStatus).To(HaveLen(1), "Should have one network status for the default network")
		var exposedIPs []string

		// Remove the CIDR from the IPs to expose only the IPs
		for _, ip := range tc.ips {
			exposedIPs = append(exposedIPs, strings.Split(ip, "/")[0])
		}
		Expect(netStatus[0].IPs).To(ConsistOf(exposedIPs), "Should have the IPs specified in the default network annotation")
		Expect(strings.ToLower(netStatus[0].Mac)).To(Equal(strings.ToLower(tc.mac)), "Should have the MAC specified in the default network annotation")

		By("Create second pod with default network annotation requesting the same MAC request")
		pod2 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-mac-conflict", nil, nil, nil)
		pod2.Annotations = map[string]string{"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q}]`, tc.mac)}
		pod2.Spec.Containers[0].Command = []string{"sleep", "infinity"}
		pod2, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.Background(), pod2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Asserting second pod has event attached reflecting MAC conflict error")
		Eventually(func(g Gomega) []corev1.Event {
			events, err := f.ClientSet.CoreV1().Events(pod2.Namespace).SearchWithContext(context.Background(), scheme.Scheme, pod2)
			g.Expect(err).NotTo(HaveOccurred())
			return events.Items
		}).WithTimeout(time.Minute * 1).WithPolling(time.Second * 3).Should(ContainElement(SatisfyAll(
			HaveField("Type", "Warning"),
			HaveField("Reason", "ErrorAllocatingPod"),
			HaveField("Message", ContainSubstring("MAC address already in use")),
		)))

		By("Assert second pod consistently at pending")
		Consistently(func(g Gomega) corev1.PodPhase {
			pod2Updated, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), pod2.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			return pod2Updated.Status.Phase
		}).
			WithTimeout(3 * time.Second).
			WithPolling(time.Second).
			Should(Equal(corev1.PodPending))
	},

		Entry("should create the pod with the specified static IP and MAC address with persistent IPAM", testCase{
			ips:       []string{"103.0.0.3/16", "2014:100:200::3/60"},
			mac:       "02:A1:B2:C3:D4:E5",
			lifecycle: udnv1.IPAMLifecyclePersistent,
		}),
		Entry("should create the pod with the specified static IP and MAC address without persistent IPAM enabled", testCase{
			ips: []string{"103.0.0.3/16", "2014:100:200::3/60"},
			mac: "02:B1:C2:D3:E4:F5",
		}),
	)

	Context("ValidatingAdmissionPolicy protection", func() {
		It("should prevent adding, modifying and removing the default-network annotation on existing pods", func(ctx SpecContext) {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

			// Create a UserDefinedNetwork for the test
			udn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-network",
					Namespace: f.Namespace.Name,
				},
				Spec: udnv1.UserDefinedNetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role: udnv1.NetworkRolePrimary,
						Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
							"103.0.0.0/16",
							"2014:100:200::0/60",
						}),
					},
				},
			}

			By("Creating a UserDefinedNetwork")
			udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(context.TODO(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create UserDefinedNetwork")
			Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, udn.Namespace, udn.Name), 5*time.Second, time.Second).Should(Succeed())

			By("Creating a pod with the default-network annotation")

			nse := []nadapi.NetworkSelectionElement{{
				Name:       "default",
				Namespace:  "ovn-kubernetes",
				IPRequest:  filterCIDRs(f.ClientSet, "103.0.0.3/16", "2014:100:200::3/60"),
				MacRequest: "02:A1:B2:C3:D4:E5",
			}}
			marshalledNSE, err := json.Marshal(nse)
			Expect(err).NotTo(HaveOccurred(), "Should marshal network selection element")

			podWithAnnotation := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-with-annotation", nil, nil, nil)
			podWithAnnotation.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": string(marshalledNSE),
			}
			podWithAnnotation.Spec.Containers[0].Command = []string{"sleep", "infinity"}
			podWithAnnotation = e2epod.NewPodClient(f).CreateSync(context.TODO(), podWithAnnotation)

			By("Creating a pod without the default-network annotation")
			podWithoutAnnotation := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-without-annotation", nil, nil, nil)
			podWithoutAnnotation.Spec.Containers[0].Command = []string{"sleep", "infinity"}
			podWithoutAnnotation = e2epod.NewPodClient(f).CreateSync(context.TODO(), podWithoutAnnotation)

			By("Attempting to add the default-network annotation to the pod without annotation")
			podWithoutAnnotation.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": string(marshalledNSE),
			}

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), podWithoutAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to add default-network annotation to existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))

			By("Attempting to modify the default-network annotation from the pod with annotation")
			updatedPodWithAnnotation := podWithAnnotation.DeepCopy()
			updatedPodWithAnnotation.Annotations["v1.multus-cni.io/default-network"] = `[{}]`

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), updatedPodWithAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to modify default-network annotation from existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))

			By("Attempting to remove the default-network annotation from the pod with annotation")
			delete(podWithAnnotation.Annotations, "v1.multus-cni.io/default-network")

			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.TODO(), podWithAnnotation, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "Should fail to remove default-network annotation from existing pod")
			Expect(err).To(MatchError(ContainSubstring("The 'v1.multus-cni.io/default-network' annotation cannot be changed after the pod was created")))
		})
	})

	Context("Pod connectivity with static IP and MAC", func() {
		const (
			externalContainerName = "cudn-egress-test-helper"
		)
		var (
			providerCtx       infraapi.Context
			externalContainer infraapi.ExternalContainer
		)

		BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "provider primary network must be available")
			externalContainerPort := infraprovider.Get().GetExternalContainerPort()
			externalContainerSpec := infraapi.ExternalContainer{
				Name:    externalContainerName,
				Image:   images.AgnHost(),
				Network: providerPrimaryNetwork,
				CmdArgs: httpServerContainerCmd(uint16(externalContainerPort)),
				ExtPort: externalContainerPort,
			}
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainerSpec)
			framework.ExpectNoError(err, "external container must succeed")
		})

		It("should configure pods with static IP/MAC on CUDN primary network", func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				"cudn-group":              "net1",
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

			// Generate a unique CUDN name based on namespace to avoid conflicts
			cudnName := fmt.Sprintf("cudn-layer2-%s", f.Namespace.Name)

			// Define the ClusterUserDefinedNetwork object
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: cudnName,
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cudn-group": "net1",
						},
					},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
								"103.0.0.0/16",
								"2014:100:200::0/60",
							}),
							DefaultGatewayIPs: filterDualStackIPs(f.ClientSet, []udnv1.IP{
								"103.0.0.2",
								"2014:100:200::2",
							}),
							IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled},
						},
					},
				},
			}

			By("Create a ClusterUserDefinedNetwork with Layer2 topology and wait for availability")
			cudn, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), cudn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create ClusterUserDefinedNetwork")

			// Setup cleanup - DeferCleanup executes in LIFO order (last registered runs first)
			// So this CUDN cleanup will run AFTER the pod cleanup below
			DeferCleanup(func() {
				By("Cleaning up ClusterUserDefinedNetwork")
				// Delete CUDN and wait for it to be fully deleted
				Eventually(func() bool {
					err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudn.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete CUDN %s: %v", cudn.Name, err)
						return false
					}

					// Check if CUDN is fully deleted
					_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudn.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 30*time.Second, time.Second).Should(BeTrue(), "CUDN should be deleted")
			})

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn.Name), 20*time.Second, time.Second).Should(Succeed())

			// Verify Namespace labels
			ns, err := f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ns.Labels).To(HaveKeyWithValue("cudn-group", "net1"))
			Expect(ns.Labels).To(HaveKey(RequiredUDNNamespaceLabel))

			// Define Pods
			pod1IPs := filterCIDRs(f.ClientSet, "103.0.0.10/16", "2014:100:200::10/60")
			pod1MAC := "02:A1:B2:C3:D4:10"
			pod2IPs := filterCIDRs(f.ClientSet, "103.0.0.20/16", "2014:100:200::20/60")
			pod2MAC := "02:A1:B2:C3:D4:20"

			pod1 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-1", nil, nil, nil)
			marshalledPod1IPs, err := json.Marshal(pod1IPs)
			Expect(err).NotTo(HaveOccurred())
			pod1.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod1MAC, string(marshalledPod1IPs)),
			}
			pod1.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			pod2 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-2", nil, nil, nil)
			marshalledPod2IPs, err := json.Marshal(pod2IPs)
			Expect(err).NotTo(HaveOccurred())
			pod2.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod2MAC, string(marshalledPod2IPs)),
			}
			pod2.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			By("Creating the pods and waiting for readiness")
			pod1 = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod1)
			pod2 = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod2)

			// Register cleanup for pods - this will run BEFORE CUDN cleanup due to LIFO order
			DeferCleanup(func() {
				By("Cleaning up all pods in namespace before CUDN deletion")
				// Delete all pods and wait for them to be actually deleted
				Eventually(func() int {
					// Delete all pods in the namespace to prevent blocking CUDN deletion
					err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(
						context.TODO(),
						metav1.DeleteOptions{},
						metav1.ListOptions{},
					)
					if err != nil {
						framework.Logf("Warning: failed to delete pods: %v", err)
						return -1
					}

					// Check how many pods remain
					pods, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						return -1
					}
					return len(pods.Items)
				}, 30*time.Second, time.Second).Should(Equal(0), "All pods should be deleted before CUDN cleanup")
			})

			// Verification of IP/MAC via introspection
			for _, p := range []struct {
				pod *corev1.Pod
				ips []string
				mac string
			}{
				{pod1, pod1IPs, pod1MAC},
				{pod2, pod2IPs, pod2MAC},
			} {
				By(fmt.Sprintf("Verifying IP and MAC for %s", p.pod.Name))
				// Verify via ip addr (checks all interfaces as UDN interface name may vary)
				stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip addr")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				for _, ip := range p.ips {
					addr := strings.Split(ip, "/")[0]
					Expect(stdout).To(ContainSubstring(addr), "IP %s not found in ip addr output", addr)
				}

				// Verify via ip link
				stdout, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip link")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				Expect(strings.ToLower(stdout)).To(ContainSubstring(strings.ToLower(p.mac)), "MAC %s not found in ip link output", p.mac)

				// Verify default gateway is set correctly via the CUDN network
				stdout, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip route")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				// Check that default route uses the CUDN gateway (103.0.0.2 for IPv4)
				if isIPv4Supported(f.ClientSet) {
					Expect(stdout).To(ContainSubstring("default via 103.0.0.2"), "Default gateway not found in ip route output")
				}
			}

			// Connectivity Validation
			pod1StaticIP := strings.Split(pod1IPs[0], "/")[0]
			pod2StaticIP := strings.Split(pod2IPs[0], "/")[0]

			By(fmt.Sprintf("Testing connectivity from pod-1 to pod-2 via IP %s", pod2StaticIP))
			pingCmd := "ping"
			if utilnet.IsIPv6String(pod2StaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod1.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod2StaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed: %s", stderr)

			By(fmt.Sprintf("Testing connectivity from pod-2 to pod-1 via IP %s", pod1StaticIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod1StaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod2.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod1StaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed: %s", stderr)

			// North-South traffic validation: test egress from static IP pods to external container
			By("Validating north-south egress traffic from pods with static IP to external container")
			externalPort := externalContainer.GetPort()

			for _, testPod := range []*corev1.Pod{pod1, pod2} {
				if isIPv4Supported(f.ClientSet) {
					By(fmt.Sprintf("Testing egress from %s to external container IPv4", testPod.Name))
					Eventually(func() error {
						return connectToServer(podConfiguration{namespace: testPod.Namespace, name: testPod.Name}, externalContainer.GetIPv4(), externalPort)
					}, 2*time.Minute, 6*time.Second).Should(Succeed(), "Egress traffic from %s to external IPv4 should succeed", testPod.Name)
				}
				if isIPv6Supported(f.ClientSet) {
					By(fmt.Sprintf("Testing egress from %s to external container IPv6", testPod.Name))
					Eventually(func() error {
						return connectToServer(podConfiguration{namespace: testPod.Namespace, name: testPod.Name}, externalContainer.GetIPv6(), externalPort)
					}, 2*time.Minute, 6*time.Second).Should(Succeed(), "Egress traffic from %s to external IPv6 should succeed", testPod.Name)
				}
			}
		})

		It("should attach pods to both CUDN primary with static IP/MAC and UDN secondary", func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				"cudn-group":              "net1",
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create namespace for test")
			f.Namespace = namespace

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

			// Generate unique names based on namespace
			cudnName := fmt.Sprintf("cudn-layer2-%s", f.Namespace.Name)
			udnName := fmt.Sprintf("sudn-layer2-%s", f.Namespace.Name)

			// Define the ClusterUserDefinedNetwork object (Primary role)
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: cudnName,
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cudn-group": "net1",
						},
					},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
								"103.0.0.0/16",
								"2014:100:200::0/60",
							}),
							DefaultGatewayIPs: filterDualStackIPs(f.ClientSet, []udnv1.IP{
								"103.0.0.2",
								"2014:100:200::2",
							}),
							IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled},
						},
					},
				},
			}

			By("Create a ClusterUserDefinedNetwork with Layer2 primary topology and wait for availability")
			cudn, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), cudn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create ClusterUserDefinedNetwork")

			// Setup cleanup for CUDN - will run AFTER pod and UDN cleanup
			DeferCleanup(func() {
				By("Cleaning up ClusterUserDefinedNetwork")
				// Delete CUDN and wait for it to be fully deleted
				Eventually(func() bool {
					err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudn.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete CUDN %s: %v", cudn.Name, err)
						return false
					}

					// Check if CUDN is fully deleted
					_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudn.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 30*time.Second, time.Second).Should(BeTrue(), "CUDN should be deleted")
			})

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn.Name), 20*time.Second, time.Second).Should(Succeed())

			// Define the UserDefinedNetwork object (Secondary role)
			udn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:      udnName,
					Namespace: f.Namespace.Name,
				},
				Spec: udnv1.UserDefinedNetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role: udnv1.NetworkRoleSecondary,
						Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
							"104.0.0.0/16",
							"2014:100:300::0/60",
						}),
						IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled},
					},
				},
			}

			By("Create a UserDefinedNetwork with Layer2 secondary topology and wait for availability")
			udn, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Create(context.TODO(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create UserDefinedNetwork")

			// Setup cleanup for UDN - will run AFTER pod cleanup but BEFORE CUDN cleanup
			DeferCleanup(func() {
				By("Cleaning up UserDefinedNetwork")
				// Delete UDN and wait for it to be fully deleted
				Eventually(func() bool {
					err := udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Delete(context.TODO(), udn.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete UDN %s: %v", udn.Name, err)
						return false
					}

					// Check if UDN is fully deleted
					_, err = udnClient.K8sV1().UserDefinedNetworks(f.Namespace.Name).Get(context.TODO(), udn.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 30*time.Second, time.Second).Should(BeTrue(), "UDN should be deleted")
			})

			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, f.Namespace.Name, udn.Name), 20*time.Second, time.Second).Should(Succeed())

			// Verify Namespace labels
			ns, err := f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ns.Labels).To(HaveKeyWithValue("cudn-group", "net1"))
			Expect(ns.Labels).To(HaveKey(RequiredUDNNamespaceLabel))

			// Define Pods with static IPs on primary network
			pod1IPs := filterCIDRs(f.ClientSet, "103.0.0.10/16", "2014:100:200::10/60")
			pod1MAC := "02:A1:B2:C3:D4:10"
			pod2IPs := filterCIDRs(f.ClientSet, "103.0.0.20/16", "2014:100:200::20/60")
			pod2MAC := "02:A1:B2:C3:D4:20"

			pod1 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-1", nil, nil, nil)
			marshalledPod1IPs, err := json.Marshal(pod1IPs)
			Expect(err).NotTo(HaveOccurred())
			pod1.Annotations = map[string]string{
				// Static IP/MAC on primary CUDN network
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod1MAC, string(marshalledPod1IPs)),
				// Attach to secondary UDN network (dynamic IP)
				"k8s.v1.cni.cncf.io/networks": udnName,
			}
			pod1.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			pod2 := e2epod.NewAgnhostPod(f.Namespace.Name, "pod-2", nil, nil, nil)
			marshalledPod2IPs, err := json.Marshal(pod2IPs)
			Expect(err).NotTo(HaveOccurred())
			pod2.Annotations = map[string]string{
				// Static IP/MAC on primary CUDN network
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod2MAC, string(marshalledPod2IPs)),
				// Attach to secondary UDN network (dynamic IP)
				"k8s.v1.cni.cncf.io/networks": udnName,
			}
			pod2.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			By("Creating the pods and waiting for readiness")
			pod1 = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod1)
			pod2 = e2epod.NewPodClient(f).CreateSync(context.TODO(), pod2)

			// Register cleanup for pods - will run BEFORE UDN and CUDN cleanup
			DeferCleanup(func() {
				By("Cleaning up all pods in namespace before UDN and CUDN deletion")
				// Delete all pods and wait for them to be actually deleted
				Eventually(func() int {
					// Delete all pods in the namespace to prevent blocking UDN and CUDN deletion
					err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(
						context.TODO(),
						metav1.DeleteOptions{},
						metav1.ListOptions{},
					)
					if err != nil {
						framework.Logf("Warning: failed to delete pods: %v", err)
						return -1
					}

					// Check how many pods remain
					pods, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						return -1
					}
					return len(pods.Items)
				}, 30*time.Second, time.Second).Should(Equal(0), "All pods should be deleted before network cleanup")
			})

			// Get the secondary network IPs from pod status annotations
			pod1Updated, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), pod1.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pod2Updated, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), pod2.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Parse k8s.ovn.org/pod-networks annotation to get secondary network IPs
			pod1Networks := pod1Updated.Annotations["k8s.ovn.org/pod-networks"]
			pod2Networks := pod2Updated.Annotations["k8s.ovn.org/pod-networks"]
			Expect(pod1Networks).NotTo(BeEmpty(), "pod-1 should have pod-networks annotation")
			Expect(pod2Networks).NotTo(BeEmpty(), "pod-2 should have pod-networks annotation")

			var pod1NetworksData, pod2NetworksData map[string]interface{}
			err = json.Unmarshal([]byte(pod1Networks), &pod1NetworksData)
			Expect(err).NotTo(HaveOccurred(), "Should parse pod-1 networks annotation")
			err = json.Unmarshal([]byte(pod2Networks), &pod2NetworksData)
			Expect(err).NotTo(HaveOccurred(), "Should parse pod-2 networks annotation")

			// Extract secondary network IPs
			var pod1SecondaryIP, pod2SecondaryIP string
			for netName, netData := range pod1NetworksData {
				if strings.Contains(netName, udnName) {
					netDataMap, ok := netData.(map[string]interface{})
					if !ok {
						framework.Logf("Warning: unexpected format for network data in pod-1, netName: %s", netName)
						continue
					}
					if ipAddresses, ok := netDataMap["ip_addresses"].([]interface{}); ok && len(ipAddresses) > 0 {
						if ipAddr, ok := ipAddresses[0].(string); ok {
							pod1SecondaryIP = strings.Split(ipAddr, "/")[0]
						}
					}
				}
			}
			for netName, netData := range pod2NetworksData {
				if strings.Contains(netName, udnName) {
					netDataMap, ok := netData.(map[string]interface{})
					if !ok {
						framework.Logf("Warning: unexpected format for network data in pod-2, netName: %s", netName)
						continue
					}
					if ipAddresses, ok := netDataMap["ip_addresses"].([]interface{}); ok && len(ipAddresses) > 0 {
						if ipAddr, ok := ipAddresses[0].(string); ok {
							pod2SecondaryIP = strings.Split(ipAddr, "/")[0]
						}
					}
				}
			}
			Expect(pod1SecondaryIP).NotTo(BeEmpty(), "pod-1 should have secondary network IP")
			Expect(pod2SecondaryIP).NotTo(BeEmpty(), "pod-2 should have secondary network IP")

			// Verification of primary network IP/MAC
			for _, p := range []struct {
				pod *corev1.Pod
				ips []string
				mac string
			}{
				{pod1, pod1IPs, pod1MAC},
				{pod2, pod2IPs, pod2MAC},
			} {
				By(fmt.Sprintf("Verifying primary network IP and MAC for %s", p.pod.Name))
				stdout, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip addr")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				for _, ip := range p.ips {
					addr := strings.Split(ip, "/")[0]
					Expect(stdout).To(ContainSubstring(addr), "IP %s not found in ip addr output", addr)
				}

				stdout, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip link")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				Expect(strings.ToLower(stdout)).To(ContainSubstring(strings.ToLower(p.mac)), "MAC %s not found in ip link output", p.mac)

				// Verify default gateway on primary network
				stdout, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, p.pod.Name, "ip route")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				if isIPv4Supported(f.ClientSet) {
					Expect(stdout).To(ContainSubstring("default via 103.0.0.2"), "Default gateway not found in ip route output")
				}
			}

			// Connectivity Validation on Primary Network (static IPs)
			pod1StaticIP := strings.Split(pod1IPs[0], "/")[0]
			pod2StaticIP := strings.Split(pod2IPs[0], "/")[0]

			By(fmt.Sprintf("Testing primary network connectivity from pod-1 to pod-2 via IP %s", pod2StaticIP))
			pingCmd := "ping"
			if utilnet.IsIPv6String(pod2StaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err := e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod1.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod2StaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed: %s", stderr)

			By(fmt.Sprintf("Testing primary network connectivity from pod-2 to pod-1 via IP %s", pod1StaticIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod1StaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod2.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod1StaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed: %s", stderr)

			// Connectivity Validation on Secondary Network (dynamic IPs)
			By(fmt.Sprintf("Testing secondary network connectivity from pod-1 to pod-2 via IP %s", pod2SecondaryIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod2SecondaryIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod1.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod2SecondaryIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed on secondary network: %s", stderr)

			By(fmt.Sprintf("Testing secondary network connectivity from pod-2 to pod-1 via IP %s", pod1SecondaryIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod1SecondaryIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = e2epod.ExecShellInPodWithFullOutput(context.TODO(), f, pod2.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod1SecondaryIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed on secondary network: %s", stderr)
		})

		It("should configure pods with overlapping static IP/MAC across multiple CUDNs", func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			// Create two namespaces with different CUDN group labels
			namespaceBlue, err := f.CreateNamespace(context.TODO(), f.BaseName+"-blue", map[string]string{
				"e2e-framework":           f.BaseName,
				"cudn-group":              "blue",
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create blue namespace")

			namespaceRed, err := f.CreateNamespace(context.TODO(), f.BaseName+"-red", map[string]string{
				"e2e-framework":           f.BaseName,
				"cudn-group":              "red",
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred(), "Should create red namespace")

			// Ensure both namespaces are cleaned up
			DeferCleanup(func() {
				By("Cleaning up blue namespace")
				// Delete namespace and wait for it to be fully deleted
				Eventually(func() bool {
					err := f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), namespaceBlue.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete blue namespace %s: %v", namespaceBlue.Name, err)
						return false
					}

					// Check if namespace is fully deleted
					_, err = f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), namespaceBlue.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 60*time.Second, time.Second).Should(BeTrue(), "Blue namespace should be deleted")
			})
			DeferCleanup(func() {
				By("Cleaning up red namespace")
				// Delete namespace and wait for it to be fully deleted
				Eventually(func() bool {
					err := f.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), namespaceRed.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete red namespace %s: %v", namespaceRed.Name, err)
						return false
					}

					// Check if namespace is fully deleted
					_, err = f.ClientSet.CoreV1().Namespaces().Get(context.TODO(), namespaceRed.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 60*time.Second, time.Second).Should(BeTrue(), "Red namespace should be deleted")
			})

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred(), "Should create UDN client")

			// Generate unique CUDN names
			cudnBlueName := fmt.Sprintf("cudn-layer2-blue-%s", namespaceBlue.Name)
			cudnRedName := fmt.Sprintf("cudn-layer2-red-%s", namespaceRed.Name)

			// Define CUDN for blue namespace (same subnet will be used for both)
			cudnBlue := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: cudnBlueName,
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cudn-group": "blue",
						},
					},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
								"103.0.0.0/16",
								"2014:100:200::0/60",
							}),
							DefaultGatewayIPs: filterDualStackIPs(f.ClientSet, []udnv1.IP{
								"103.0.0.2",
								"2014:100:200::2",
							}),
							IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled},
						},
					},
				},
			}

			// Define CUDN for red namespace (same subnet as blue - demonstrating isolation)
			cudnRed := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name: cudnRedName,
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							"cudn-group": "red",
						},
					},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role: udnv1.NetworkRolePrimary,
							Subnets: filterDualStackCIDRs(f.ClientSet, []udnv1.CIDR{
								"103.0.0.0/16",
								"2014:100:200::0/60",
							}),
							DefaultGatewayIPs: filterDualStackIPs(f.ClientSet, []udnv1.IP{
								"103.0.0.2",
								"2014:100:200::2",
							}),
							IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMEnabled},
						},
					},
				},
			}

			By("Creating ClusterUserDefinedNetwork for blue namespace")
			cudnBlue, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), cudnBlue, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create blue CUDN")

			DeferCleanup(func() {
				By("Cleaning up blue ClusterUserDefinedNetwork")
				// Delete blue CUDN and wait for it to be fully deleted
				Eventually(func() bool {
					err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnBlue.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete blue CUDN %s: %v", cudnBlue.Name, err)
						return false
					}

					// Check if CUDN is fully deleted
					_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnBlue.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 30*time.Second, time.Second).Should(BeTrue(), "Blue CUDN should be deleted")
			})

			By("Creating ClusterUserDefinedNetwork for red namespace")
			cudnRed, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.TODO(), cudnRed, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create red CUDN")

			DeferCleanup(func() {
				By("Cleaning up red ClusterUserDefinedNetwork")
				// Delete red CUDN and wait for it to be fully deleted
				Eventually(func() bool {
					err := udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnRed.Name, metav1.DeleteOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						framework.Logf("Warning: failed to delete red CUDN %s: %v", cudnRed.Name, err)
						return false
					}

					// Check if CUDN is fully deleted
					_, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnRed.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 30*time.Second, time.Second).Should(BeTrue(), "Red CUDN should be deleted")
			})

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnBlue.Name), 20*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnRed.Name), 20*time.Second, time.Second).Should(Succeed())

			// Use the SAME IP addresses and MACs for pods in both namespaces (demonstrating isolation)
			pod1IPs := filterCIDRs(f.ClientSet, "103.0.0.10/16", "2014:100:200::10/60")
			pod1MAC := "02:A1:B2:C3:D4:10"
			pod2IPs := filterCIDRs(f.ClientSet, "103.0.0.20/16", "2014:100:200::20/60")
			pod2MAC := "02:A1:B2:C3:D4:20"

			// Create pods in blue namespace
			By("Creating pods in blue namespace with static IPs")
			podBlue1 := e2epod.NewAgnhostPod(namespaceBlue.Name, "pod-1", nil, nil, nil)
			marshalledPod1IPs, err := json.Marshal(pod1IPs)
			Expect(err).NotTo(HaveOccurred())
			podBlue1.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod1MAC, string(marshalledPod1IPs)),
			}
			podBlue1.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			podBlue2 := e2epod.NewAgnhostPod(namespaceBlue.Name, "pod-2", nil, nil, nil)
			marshalledPod2IPs, err := json.Marshal(pod2IPs)
			Expect(err).NotTo(HaveOccurred())
			podBlue2.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod2MAC, string(marshalledPod2IPs)),
			}
			podBlue2.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			// Create pods in blue namespace using direct client
			podBlue1, err = f.ClientSet.CoreV1().Pods(namespaceBlue.Name).Create(context.TODO(), podBlue1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create pod-1 in blue namespace")
			podBlue2, err = f.ClientSet.CoreV1().Pods(namespaceBlue.Name).Create(context.TODO(), podBlue2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create pod-2 in blue namespace")

			// Wait for pods to be ready
			err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, podBlue1)
			Expect(err).NotTo(HaveOccurred(), "Pod-1 should be running in blue namespace")
			err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, podBlue2)
			Expect(err).NotTo(HaveOccurred(), "Pod-2 should be running in blue namespace")

			DeferCleanup(func() {
				By("Cleaning up pods in blue namespace")
				// Delete all pods and wait for them to be actually deleted
				Eventually(func() int {
					// Delete all pods in blue namespace
					err := f.ClientSet.CoreV1().Pods(namespaceBlue.Name).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
					if err != nil {
						framework.Logf("Warning: failed to delete pods in blue namespace: %v", err)
						return -1
					}

					// Check how many pods remain
					pods, err := f.ClientSet.CoreV1().Pods(namespaceBlue.Name).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						return -1
					}
					return len(pods.Items)
				}, 30*time.Second, time.Second).Should(Equal(0))
			})

			// Create pods in red namespace with SAME IPs and MACs (demonstrating network isolation)
			By("Creating pods in red namespace with same static IPs as blue namespace")
			podRed1 := e2epod.NewAgnhostPod(namespaceRed.Name, "pod-1", nil, nil, nil)
			podRed1.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod1MAC, string(marshalledPod1IPs)),
			}
			podRed1.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			podRed2 := e2epod.NewAgnhostPod(namespaceRed.Name, "pod-2", nil, nil, nil)
			podRed2.Annotations = map[string]string{
				"v1.multus-cni.io/default-network": fmt.Sprintf(`[{"name":"default", "namespace":"ovn-kubernetes", "mac":%q, "ips": %s}]`, pod2MAC, string(marshalledPod2IPs)),
			}
			podRed2.Spec.Containers[0].Command = []string{"sleep", "infinity"}

			// Create pods in red namespace using direct client
			podRed1, err = f.ClientSet.CoreV1().Pods(namespaceRed.Name).Create(context.TODO(), podRed1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create pod-1 in red namespace")
			podRed2, err = f.ClientSet.CoreV1().Pods(namespaceRed.Name).Create(context.TODO(), podRed2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should create pod-2 in red namespace")

			// Wait for pods to be ready
			err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, podRed1)
			Expect(err).NotTo(HaveOccurred(), "Pod-1 should be running in red namespace")
			err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, podRed2)
			Expect(err).NotTo(HaveOccurred(), "Pod-2 should be running in red namespace")

			DeferCleanup(func() {
				By("Cleaning up pods in red namespace")
				// Delete all pods and wait for them to be actually deleted
				Eventually(func() int {
					// Delete all pods in red namespace
					err := f.ClientSet.CoreV1().Pods(namespaceRed.Name).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
					if err != nil {
						framework.Logf("Warning: failed to delete pods in red namespace: %v", err)
						return -1
					}

					// Check how many pods remain
					pods, err := f.ClientSet.CoreV1().Pods(namespaceRed.Name).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						return -1
					}
					return len(pods.Items)
				}, 30*time.Second, time.Second).Should(Equal(0))
			})

			// Verify IPs and MACs in blue namespace
			pod1BlueStaticIP := strings.Split(pod1IPs[0], "/")[0]
			pod2BlueStaticIP := strings.Split(pod2IPs[0], "/")[0]

			By("Verifying IP and MAC assignments in blue namespace")
			for _, p := range []struct {
				pod *corev1.Pod
				ips []string
				mac string
			}{
				{podBlue1, pod1IPs, pod1MAC},
				{podBlue2, pod2IPs, pod2MAC},
			} {
				stdout, stderr, err := ExecShellInPodWithFullOutput(f, namespaceBlue.Name, p.pod.Name, "ip addr")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				for _, ip := range p.ips {
					addr := strings.Split(ip, "/")[0]
					Expect(stdout).To(ContainSubstring(addr), "IP %s not found in blue namespace", addr)
				}

				stdout, stderr, err = ExecShellInPodWithFullOutput(f, namespaceBlue.Name, p.pod.Name, "ip link")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				Expect(strings.ToLower(stdout)).To(ContainSubstring(strings.ToLower(p.mac)), "MAC %s not found in blue namespace", p.mac)
			}

			// Verify IPs and MACs in red namespace (using same IPs/MACs as blue to demonstrate isolation)
			pod1RedStaticIP := strings.Split(pod1IPs[0], "/")[0]
			pod2RedStaticIP := strings.Split(pod2IPs[0], "/")[0]

			By("Verifying IP and MAC assignments in red namespace")
			for _, p := range []struct {
				pod *corev1.Pod
				ips []string
				mac string
			}{
				{podRed1, pod1IPs, pod1MAC},
				{podRed2, pod2IPs, pod2MAC},
			} {
				stdout, stderr, err := ExecShellInPodWithFullOutput(f, namespaceRed.Name, p.pod.Name, "ip addr")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				for _, ip := range p.ips {
					addr := strings.Split(ip, "/")[0]
					Expect(stdout).To(ContainSubstring(addr), "IP %s not found in red namespace", addr)
				}

				stdout, stderr, err = ExecShellInPodWithFullOutput(f, namespaceRed.Name, p.pod.Name, "ip link")
				Expect(err).NotTo(HaveOccurred(), "stdout: %s, stderr: %s", stdout, stderr)
				Expect(strings.ToLower(stdout)).To(ContainSubstring(strings.ToLower(p.mac)), "MAC %s not found in red namespace", p.mac)
			}

			// Test connectivity within blue namespace
			By(fmt.Sprintf("Testing connectivity within blue namespace: pod-1 (%s) to pod-2 (%s)", pod1BlueStaticIP, pod2BlueStaticIP))
			pingCmd := "ping"
			if utilnet.IsIPv6String(pod2BlueStaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err := ExecShellInPodWithFullOutput(f, namespaceBlue.Name, podBlue1.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod2BlueStaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed in blue namespace: %s", stderr)

			By(fmt.Sprintf("Testing connectivity within blue namespace: pod-2 (%s) to pod-1 (%s)", pod2BlueStaticIP, pod1BlueStaticIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod1BlueStaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = ExecShellInPodWithFullOutput(f, namespaceBlue.Name, podBlue2.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod1BlueStaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed in blue namespace: %s", stderr)

			// Test connectivity within red namespace
			By(fmt.Sprintf("Testing connectivity within red namespace: pod-1 (%s) to pod-2 (%s)", pod1RedStaticIP, pod2RedStaticIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod2RedStaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = ExecShellInPodWithFullOutput(f, namespaceRed.Name, podRed1.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod2RedStaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed in red namespace: %s", stderr)

			By(fmt.Sprintf("Testing connectivity within red namespace: pod-2 (%s) to pod-1 (%s)", pod2RedStaticIP, pod1RedStaticIP))
			pingCmd = "ping"
			if utilnet.IsIPv6String(pod1RedStaticIP) {
				pingCmd = "ping6"
			}
			_, stderr, err = ExecShellInPodWithFullOutput(f, namespaceRed.Name, podRed2.Name, fmt.Sprintf("%s -c 3 -W 2 %s", pingCmd, pod1RedStaticIP))
			Expect(err).NotTo(HaveOccurred(), "Ping failed in red namespace: %s", stderr)
		})
	})
})
