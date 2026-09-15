// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

// findOVNObservNBDBPod finds a running OVN DB pod with the nb-ovsdb container.
func findOVNObservNBDBPod(cs clientset.Interface, ovnNamespace string) (*v1.Pod, error) {
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "ovn-db-pod=true",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list OVN DB pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != v1.PodRunning {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "nb-ovsdb" {
				return pod, nil
			}
		}
	}
	return nil, fmt.Errorf("no running OVN DB pod with nb-ovsdb container found in namespace %s", ovnNamespace)
}

// runObservNBCTL runs an ovn-nbctl command in the nb-ovsdb container.
func runObservNBCTL(f *framework.Framework, cs clientset.Interface, args ...string) (string, error) {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	dbPod, err := findOVNObservNBDBPod(cs, ovnNamespace)
	if err != nil {
		return "", err
	}
	cmd := append([]string{"ovn-nbctl"}, args...)
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, dbPod.Name, "nb-ovsdb", cmd...)
	if err != nil {
		return stdout, fmt.Errorf("failed running ovn-nbctl %v on %s/%s: %w, stderr: %s", args, ovnNamespace, dbPod.Name, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

var _ = Describe("OVN Observability NBDB state", feature.Observability, func() {
	fr := wrappedTestFramework("observability")

	BeforeEach(func() {
		if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.ObservabilityConfig) {
			Skip("OVN Observability is not enabled")
		}
	})

	Context("Sampling infrastructure", func() {
		It("should have SamplingApp entries for drop, acl-new and acl-est", func() {
			output, err := runObservNBCTL(fr, fr.ClientSet,
				"--data=bare", "--no-heading", "--columns=type", "list", "Sampling_App")
			Expect(err).NotTo(HaveOccurred())

			types := strings.Split(output, "\n")
			Expect(types).To(ContainElement("drop"))
			Expect(types).To(ContainElement("acl-new"))
			Expect(types).To(ContainElement("acl-est"))
		})

		It("should have a SampleCollector with expected probability and set_id", func() {
			output, err := runObservNBCTL(fr, fr.ClientSet,
				"--data=bare", "--no-heading", "--columns=probability,set_id",
				"list", "Sample_Collector")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(BeEmpty(), "expected at least one Sample_Collector")

			// Default config: 100% probability = 65535, set_id = 42
			// Output format: two separate lines (probability, then set_id)
			Expect(output).To(ContainSubstring("65535"))
			Expect(output).To(ContainSubstring("42"))
		})

		It("should have SampleCollector with expected feature external_ids", func() {
			output, err := runObservNBCTL(fr, fr.ClientSet,
				"--data=bare", "--no-heading", "--columns=external_ids",
				"list", "Sample_Collector")
			Expect(err).NotTo(HaveOccurred())

			// All features should be listed in the sample-features external_id
			for _, feature := range []string{"NetworkPolicy", "EgressFirewall", "AdminNetworkPolicy", "Multicast", "UDNIsolation"} {
				Expect(output).To(ContainSubstring(feature),
					"expected Sample_Collector external_ids to include %s", feature)
			}
		})
	})

	Context("NetworkPolicy ACL sampling", func() {
		var nsName string

		BeforeEach(func() {
			nsName = fr.Namespace.Name
		})

		It("should attach Sample references to ACLs when a NetworkPolicy is created", func() {
			By("creating a deny-all network policy")
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "observ-deny-all"},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress, knet.PolicyTypeEgress},
					Ingress:     []knet.NetworkPolicyIngressRule{},
					Egress:      []knet.NetworkPolicyEgressRule{},
				},
			}
			_, err := fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Create(context.TODO(), policy, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("creating a pod so that the network policy ACLs are programmed")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			pod := newAgnhostPod(nsName, "observ-pod", cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())

			By("verifying ACLs with NetpolNamespace owner type have sample_new set")
			// A deny-all NetworkPolicy creates ACLs with owner-type "NetpolNamespace"
			// for the namespace-level default deny rules.
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "NetpolNamespace")
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected NetworkPolicy ACLs to have sample_new references")

			By("verifying Sample objects exist in NBDB")
			Eventually(func() (bool, error) {
				return hasSampleObjects(fr, fr.ClientSet)
			}, 15*time.Second, 2*time.Second).Should(BeTrue(),
				"expected Sample objects to exist in NBDB")
		})

		It("should clean up Sample objects when a NetworkPolicy is deleted", func() {
			By("creating a deny-all network policy")
			policyName := "observ-cleanup-test"
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress, knet.PolicyTypeEgress},
					Ingress:     []knet.NetworkPolicyIngressRule{},
					Egress:      []knet.NetworkPolicyEgressRule{},
				},
			}
			_, err := fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Create(context.TODO(), policy, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("creating a pod so that the network policy ACLs are programmed")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			pod := newAgnhostPod(nsName, "observ-cleanup-pod", cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())

			By("waiting for ACLs to have sample references")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "NetpolNamespace")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("counting Sample objects before deletion")
			sampleCountBefore, err := countNBDBSamples(fr, fr.ClientSet)
			Expect(err).NotTo(HaveOccurred())
			Expect(sampleCountBefore).To(BeNumerically(">", 0))

			By("deleting the network policy")
			err = fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Delete(context.TODO(), policyName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying Sample objects are cleaned up")
			Eventually(func() (int, error) {
				return countNBDBSamples(fr, fr.ClientSet)
			}, 30*time.Second, 2*time.Second).Should(BeNumerically("<", sampleCountBefore),
				"expected Sample count to decrease after NetworkPolicy deletion")
		})
	})

	Context("EgressFirewall ACL sampling", func() {
		It("should attach Sample references to EgressFirewall ACLs", func() {
			nsName := fr.Namespace.Name
			denyCIDR := "0.0.0.0/0"
			allowIP := "172.18.0.1"
			mask := "32"
			if IsIPv6Cluster(fr.ClientSet) {
				denyCIDR = "::/0"
				allowIP = "2001:4860:4860::8888"
				mask = "128"
			}

			By("creating an EgressFirewall")
			Expect(makeEgressFirewall(nsName, allowIP, mask, denyCIDR)).To(Succeed())

			By("creating a pod so that ACLs are programmed")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			pod := newAgnhostPod(nsName, "observ-efw-pod", cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())

			By("verifying EgressFirewall ACLs have sample_new set")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "EgressFirewall")
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected EgressFirewall ACLs to have sample_new references")
		})
	})

	Context("AdminNetworkPolicy ACL sampling", func() {
		const anpName = "observ-anp-test"

		AfterEach(func() {
			_, err := e2ekubectl.RunKubectl("default", "delete", "anp", anpName, "--ignore-not-found=true")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should attach Sample references to AdminNetworkPolicy ACLs", func() {
			nsName := fr.Namespace.Name

			By("creating an AdminNetworkPolicy")
			anpYaml := fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: 50
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "deny-all-egress"
    action: "Deny"
    to:
    - networks:
      - 0.0.0.0/0
`, anpName, nsName)

			_, err := e2ekubectl.RunKubectlInput(nsName, anpYaml, "create", "-f", "-")
			Expect(err).NotTo(HaveOccurred())

			By("creating a pod so that ACLs are programmed")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			pod := newAgnhostPod(nsName, "observ-anp-pod", cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())

			By("verifying AdminNetworkPolicy ACLs have sample_new set")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "AdminNetworkPolicy")
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected AdminNetworkPolicy ACLs to have sample_new references")
		})
	})

	Context("psample end-to-end", func() {
		BeforeEach(func() {
			has611, err := isKernel611OrNewer(fr, fr.ClientSet)
			Expect(err).NotTo(HaveOccurred())
			if !has611 {
				Skip("psample requires kernel 6.11+")
			}
			DeferCleanup(cleanupObservProcesses, fr, fr.ClientSet)
		})

		It("should receive samples for NetworkPolicy deny traffic", func() {
			nsName := fr.Namespace.Name

			By("creating two pods")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-src", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			dstPod := newAgnhostPod(nsName, "observ-psample-dst", cmd...)
			dstPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), dstPod)
			Expect(waitForACLLoggingPod(fr, nsName, dstPod.GetName())).To(Succeed())

			dstIP := dstPod.Status.PodIP

			By("creating a deny-all NetworkPolicy")
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "observ-psample-deny"},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
					Ingress:     []knet.NetworkPolicyIngressRule{},
				},
			}
			_, err := fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Create(context.TODO(), policy, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for policy to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "NetpolNamespace")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			// Collect on dst node since ingress deny ACL is evaluated there
			output := collectObservSamples(fr, fr.ClientSet, dstPod.Spec.NodeName, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
			})

			By("verifying samples contain deny action for namespace isolation")
			// Deny-all NetworkPolicy creates NetpolNamespace owner ACLs which produce
			// "network policies isolation in namespace <ns>" messages
			Expect(output).To(ContainSubstring("Dropped by network policies isolation in namespace "+nsName),
				"expected deny sample for namespace isolation policy, got: %s", output)
			Expect(output).To(ContainSubstring(fmt.Sprintf("src=%s", srcPod.Status.PodIP)))
			Expect(output).To(ContainSubstring(fmt.Sprintf("dst=%s", dstIP)))
		})

		It("should receive samples for NetworkPolicy allow traffic", func() {
			nsName := fr.Namespace.Name

			By("creating two pods")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-allow-src", cmd...)
			srcPod.Labels = map[string]string{"role": "client"}
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			dstPod := newAgnhostPod(nsName, "observ-psample-allow-dst", cmd...)
			dstPod.Labels = map[string]string{"role": "server"}
			dstPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), dstPod)
			Expect(waitForACLLoggingPod(fr, nsName, dstPod.GetName())).To(Succeed())

			dstIP := dstPod.Status.PodIP

			By("creating an allow NetworkPolicy")
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "observ-psample-allow"},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"role": "server"},
					},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
					Ingress: []knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"role": "client"},
							},
						}},
					}},
				},
			}
			_, err := fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Create(context.TODO(), policy, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for policy to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "NetworkPolicy")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			// Collect on dst node since the ingress allow ACL is evaluated there
			output := collectObservSamples(fr, fr.ClientSet, dstPod.Spec.NodeName, func() {
				err = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
				Expect(err).NotTo(HaveOccurred(), "ping should succeed for allow policy")
			})

			By("verifying samples contain allow action for NetworkPolicy")
			Expect(output).To(ContainSubstring("Allowed by network policy observ-psample-allow in namespace "+nsName),
				"expected allow sample for NetworkPolicy, got: %s", output)
			Expect(output).To(ContainSubstring(fmt.Sprintf("src=%s", srcPod.Status.PodIP)))
			Expect(output).To(ContainSubstring(fmt.Sprintf("dst=%s", dstIP)))
		})

		It("should receive samples for EgressFirewall deny traffic", func() {
			nsName := fr.Namespace.Name
			denyCIDR := "0.0.0.0/0"
			allowIP := "172.18.0.1"
			mask := "32"
			dstIP := "1.2.3.4"
			if IsIPv6Cluster(fr.ClientSet) {
				denyCIDR = "::/0"
				allowIP = "2001:4860:4860::8888"
				mask = "128"
				dstIP = "2001:db8::1"
			}

			By("creating a pod")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-efw", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			By("creating an EgressFirewall")
			Expect(makeEgressFirewall(nsName, allowIP, mask, denyCIDR)).To(Succeed())

			By("waiting for EgressFirewall ACLs to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "EgressFirewall")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			output := collectObservSamples(fr, fr.ClientSet, srcPod.Spec.NodeName, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
			})

			By("verifying samples contain deny action for EgressFirewall")
			Expect(output).To(ContainSubstring("Dropped by egress firewall in namespace "+nsName),
				"expected deny sample for EgressFirewall, got: %s", output)
		})

		It("should receive samples for AdminNetworkPolicy deny traffic", func() {
			const anpName = "observ-psample-anp"
			nsName := fr.Namespace.Name

			defer func() {
				_, err := e2ekubectl.RunKubectl("default", "delete", "anp", anpName, "--ignore-not-found=true")
				Expect(err).NotTo(HaveOccurred())
			}()

			By("creating two pods")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-anp-src", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			dstPod := newAgnhostPod(nsName, "observ-psample-anp-dst", cmd...)
			dstPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), dstPod)
			Expect(waitForACLLoggingPod(fr, nsName, dstPod.GetName())).To(Succeed())

			dstIP := dstPod.Status.PodIP
			dstCIDR := dstIP + "/32"
			if IsIPv6Cluster(fr.ClientSet) {
				dstCIDR = dstIP + "/128"
			}

			By("creating an AdminNetworkPolicy that denies egress")
			anpYaml := fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: 50
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "deny-egress"
    action: "Deny"
    to:
    - networks:
      - %s
`, anpName, nsName, dstCIDR)

			_, err := e2ekubectl.RunKubectlInput(nsName, anpYaml, "create", "-f", "-")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for ANP ACLs to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "AdminNetworkPolicy")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			output := collectObservSamples(fr, fr.ClientSet, srcPod.Spec.NodeName, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
			})

			By("verifying samples contain deny action for AdminNetworkPolicy")
			Expect(output).To(ContainSubstring(fmt.Sprintf("Dropped by admin network policy %s", anpName)),
				"expected deny sample for AdminNetworkPolicy, got: %s", output)
			Expect(output).To(ContainSubstring(fmt.Sprintf("src=%s", srcPod.Status.PodIP)))
		})

		It("should receive samples for EgressFirewall allow traffic", func() {
			nsName := fr.Namespace.Name
			allowIP := "172.18.0.1"
			mask := "32"
			denyCIDR := "0.0.0.0/0"
			if IsIPv6Cluster(fr.ClientSet) {
				allowIP = "2001:4860:4860::8888"
				mask = "128"
				denyCIDR = "::/0"
			}

			By("creating a pod")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-efw-allow", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			By("creating an EgressFirewall with allow rule")
			Expect(makeEgressFirewall(nsName, allowIP, mask, denyCIDR)).To(Succeed())

			By("waiting for EgressFirewall ACLs to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "EgressFirewall")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			output := collectObservSamples(fr, fr.ClientSet, srcPod.Spec.NodeName, func() {
				err := generateTraffic(fr, nsName, srcPod.Name, allowIP, 5)
				Expect(err).NotTo(HaveOccurred(), "ping to allowed IP should succeed")
			})

			By("verifying samples contain allow action for EgressFirewall")
			Expect(output).To(ContainSubstring("Allowed by egress firewall in namespace "+nsName),
				"expected allow sample for EgressFirewall, got: %s", output)
		})

		It("should receive samples for AdminNetworkPolicy pass action", func() {
			const anpName = "observ-psample-anp-pass"
			nsName := fr.Namespace.Name

			defer func() {
				_, err := e2ekubectl.RunKubectl("default", "delete", "anp", anpName, "--ignore-not-found=true")
				Expect(err).NotTo(HaveOccurred())
			}()

			By("creating two pods")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-pass-src", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			dstPod := newAgnhostPod(nsName, "observ-psample-pass-dst", cmd...)
			dstPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), dstPod)
			Expect(waitForACLLoggingPod(fr, nsName, dstPod.GetName())).To(Succeed())

			dstIP := dstPod.Status.PodIP
			dstCIDR := dstIP + "/32"
			if IsIPv6Cluster(fr.ClientSet) {
				dstCIDR = dstIP + "/128"
			}

			By("creating an AdminNetworkPolicy with Pass action")
			anpYaml := fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: 50
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "pass-egress"
    action: "Pass"
    to:
    - networks:
      - %s
`, anpName, nsName, dstCIDR)

			_, err := e2ekubectl.RunKubectlInput(nsName, anpYaml, "create", "-f", "-")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for ANP ACLs to be programmed")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "AdminNetworkPolicy")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			output := collectObservSamples(fr, fr.ClientSet, srcPod.Spec.NodeName, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
			})

			By("verifying samples contain pass (delegated) action for AdminNetworkPolicy")
			Expect(output).To(ContainSubstring(fmt.Sprintf("Delegated to network policy by admin network policy %s", anpName)),
				"expected pass/delegated sample for AdminNetworkPolicy, got: %s", output)
		})

	})
})

// hasACLsWithSamples checks if ACLs with the given owner type have sample_new set.
// ownerType should match the k8s.ovn.org/owner-type external_id value, e.g. "NetworkPolicy",
// "NetpolNamespace", "EgressFirewall", "AdminNetworkPolicy".
func hasACLsWithSamples(f *framework.Framework, cs clientset.Interface, ownerType string) (bool, error) {
	output, err := runObservNBCTL(f, cs,
		"--data=bare", "--no-heading", "--columns=sample_new",
		"find", "ACL",
		fmt.Sprintf(`external_ids:"k8s.ovn.org/owner-type"=%s`, ownerType))
	if err != nil {
		return false, err
	}
	if output == "" {
		return false, nil
	}
	// Each line is a sample_new UUID; check that at least one is non-empty
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			return true, nil
		}
	}
	return false, nil
}

// hasSampleObjects checks if any Sample objects exist in NBDB.
func hasSampleObjects(f *framework.Framework, cs clientset.Interface) (bool, error) {
	count, err := countNBDBSamples(f, cs)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// countNBDBSamples returns the number of Sample objects in NBDB.
func countNBDBSamples(f *framework.Framework, cs clientset.Interface) (int, error) {
	output, err := runObservNBCTL(f, cs,
		"--data=bare", "--no-heading", "--columns=_uuid",
		"list", "Sample")
	if err != nil {
		return 0, err
	}
	if output == "" {
		return 0, nil
	}
	return len(strings.Split(output, "\n")), nil
}

// isKernel611OrNewer checks if the kernel version is 6.11 or newer (required for psample).
// Runs uname -r inside an ovnkube-node pod to check the host kernel version.
func isKernel611OrNewer(f *framework.Framework, cs clientset.Interface) (bool, error) {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	// Find any ovnkube-node pod
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
	})
	if err != nil || len(pods.Items) == 0 {
		return false, fmt.Errorf("failed to find ovnkube-node pod: %w", err)
	}

	nodePod := &pods.Items[0]
	output, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb", "uname", "-r")
	if err != nil {
		return false, fmt.Errorf("failed to get kernel version: %w", err)
	}

	kernelVersion := strings.TrimSpace(output)
	// Parse version like "6.11.0-linuxkit" -> major=6, minor=11
	parts := strings.Split(kernelVersion, ".")
	if len(parts) < 2 {
		return false, fmt.Errorf("unexpected kernel version format: %s", kernelVersion)
	}
	var major, minor int
	_, err = fmt.Sscanf(parts[0]+"."+parts[1], "%d.%d", &major, &minor)
	if err != nil {
		return false, fmt.Errorf("failed to parse kernel version %s: %w", kernelVersion, err)
	}
	return major > 6 || (major == 6 && minor >= 11), nil
}

// findOVNKubeNodePod finds a running ovnkube-node pod on the same node as the given pod.
func findOVNKubeNodePod(cs clientset.Interface, ovnNamespace, nodeName string) (*v1.Pod, error) {
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list ovnkube-node pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == v1.PodRunning {
			return pod, nil
		}
	}
	return nil, fmt.Errorf("no running ovnkube-node pod found on node %s", nodeName)
}

// cleanupObservProcesses kills any stale ovnkube-observ processes and removes output
// files on all ovnkube-node pods. Should be called in AfterEach for psample tests.
func cleanupObservProcesses(f *framework.Framework, cs clientset.Interface) {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
	})
	if err != nil {
		framework.Logf("Warning: failed to list ovnkube-node pods for cleanup: %v", err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != v1.PodRunning {
			continue
		}
		cleanupCmd := []string{"/bin/sh", "-c", "pkill -f ovnkube-observ 2>/dev/null; rm -f /tmp/observ-samples.log"}
		_, _, _ = ExecCommandInContainerWithFullOutput(f, ovnNamespace, pod.Name, "nb-ovsdb", cleanupCmd...)
	}
}

// collectObservSamples starts ovnkube-observ in the background on the given node,
// runs trafficFn to generate traffic, polls for sample output, then returns it.
// The listener must be running before traffic is sent.
func collectObservSamples(f *framework.Framework, cs clientset.Interface, nodeName string, trafficFn func()) string {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	nodePod, err := findOVNKubeNodePod(cs, ovnNamespace, nodeName)
	Expect(err).NotTo(HaveOccurred())

	outputFile := "/tmp/observ-samples.log"

	// Start ovnkube-observ fully detached: nohup + redirect all FDs + &
	// The exec API waits for all file descriptors to close, so we must redirect
	// stdout/stderr and use nohup to fully detach from the exec session.
	// --add-ovs-collector creates the Flow_Sample_Collector_Set in OVS, which is
	// required for OVS to actually send sampled packets via psample.
	startCmd := []string{
		"/bin/sh", "-c",
		fmt.Sprintf("nohup timeout 60 /usr/bin/ovnkube-observ --enable-enrichment=true --add-ovs-collector > %s 2>&1 &", outputFile),
	}
	_, _, err = ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb", startCmd...)
	Expect(err).NotTo(HaveOccurred())

	// Ensure cleanup runs even if traffic generation or polling fails
	defer func() {
		cleanupCmd := []string{"/bin/sh", "-c", "pkill -f ovnkube-observ 2>/dev/null; rm -f " + outputFile}
		_, _, _ = ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb", cleanupCmd...)
	}()

	// Poll until ovnkube-observ process is running before generating traffic
	Eventually(func() bool {
		out, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb",
			"/bin/sh", "-c", "pgrep -f ovnkube-observ >/dev/null 2>&1 && echo running")
		return err == nil && strings.Contains(out, "running")
	}, 10*time.Second, 1*time.Second).Should(BeTrue(), "ovnkube-observ did not start")

	// Poll until output file has "OVN-K message" content (enriched sample output).
	// Re-send traffic each iteration to handle the race between netlink listener
	// initialization and traffic generation.
	var output string
	Eventually(func() string {
		trafficFn()
		readCmd := []string{"cat", outputFile}
		out, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb", readCmd...)
		if err != nil {
			return ""
		}
		output = out
		return out
	}, 30*time.Second, 2*time.Second).Should(ContainSubstring("OVN-K message"),
		"ovnkube-observ did not produce enriched sample output")

	framework.Logf("ovnkube-observ output on node %s:\n%s", nodeName, output)
	return output
}

// generateTraffic sends ping packets from srcPod to dstIP to trigger ACL sampling.
// Returns error if ping fails unexpectedly (use for allow rules).
// For deny rules, expect ping to fail.
func generateTraffic(f *framework.Framework, namespace, srcPodName, dstIP string, count int) error {
	cmd := []string{"ping", "-c", fmt.Sprintf("%d", count), "-W", "1", dstIP}
	_, _, err := ExecCommandInContainerWithFullOutput(f, namespace, srcPodName, "", cmd...)
	return err
}
