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

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
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

var _ = Describe("OVN Observability NBDB state", feature.Observability, func() {
	fr := wrappedTestFramework("observability")

	BeforeEach(func() {
		if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.ObservabilityConfig) {
			Skip("OVN Observability is not enabled")
		}
	})

	Context("Sampling infrastructure", func() {
		It("should have SamplingApp entries for drop, acl-new and acl-est", func() {
			output, err := runOVNNBCTL(fr, fr.ClientSet,
				"--data=bare", "--no-heading", "--columns=type", "list", "Sampling_App")
			Expect(err).NotTo(HaveOccurred())

			types := strings.Split(output, "\n")
			Expect(types).To(ContainElement("drop"))
			Expect(types).To(ContainElement("acl-new"))
			Expect(types).To(ContainElement("acl-est"))
		})

		It("should have a SampleCollector with expected probability and set_id", func() {
			output, err := runOVNNBCTL(fr, fr.ClientSet,
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
			output, err := runOVNNBCTL(fr, fr.ClientSet,
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

			By("deleting the network policy")
			err = fr.ClientSet.NetworkingV1().NetworkPolicies(nsName).Delete(context.TODO(), policyName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying NetpolNamespace ACLs for this namespace are cleaned up")
			// Scope to this namespace to avoid interference from parallel tests.
			// NetpolNamespace ACLs have external_ids with k8s.ovn.org/owner containing the namespace.
			Eventually(func() (bool, error) {
				return hasACLsWithSamplesForNamespace(fr, fr.ClientSet, "NetpolNamespace", nsName)
			}, 30*time.Second, 2*time.Second).Should(BeFalse(),
				"expected NetpolNamespace ACLs for namespace %s to be cleaned up after policy deletion", nsName)
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
			denyNetwork := "0.0.0.0/0"
			if IsIPv6Cluster(fr.ClientSet) {
				denyNetwork = "::/0"
			}
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
      - %s
`, anpName, nsName, denyNetwork)

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

	Context("Multicast ACL sampling", func() {
		It("should attach Sample references to Multicast ACLs when multicast is enabled", func() {
			nsName := fr.Namespace.Name

			By("enabling multicast for the test namespace")
			enableMulticastForNamespace(fr)

			By("creating a pod so that multicast ACLs are programmed")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			pod := newAgnhostPod(nsName, "observ-mcast-pod", cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())

			By("verifying MulticastNS ACLs for this namespace have sample_new set")
			// MulticastNS ACLs use k8s.ovn.org/name for the namespace (ObjectNameKey), not k8s.ovn.org/owner.
			Eventually(func() (bool, error) {
				output, err := runOVNNBCTL(fr, fr.ClientSet,
					"--data=bare", "--no-heading", "--columns=sample_new",
					"find", "ACL",
					`external_ids:"k8s.ovn.org/owner-type"=MulticastNS`,
					fmt.Sprintf(`external_ids:"k8s.ovn.org/name"=%s`, nsName))
				if err != nil {
					return false, err
				}
				for _, line := range strings.Split(output, "\n") {
					if strings.TrimSpace(line) != "" {
						return true, nil
					}
				}
				return false, nil
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected MulticastNS ACLs to have sample_new references for namespace %s", nsName)
		})
	})

	Context("UDN isolation ACL sampling", func() {
		It("should attach Sample references to UDNIsolation ACLs", func() {
			By("checking if UDN CRD is available")
			_, err := e2ekubectl.RunKubectl("", "get", "crd", "userdefinednetworks.k8s.ovn.org", "--no-headers")
			if err != nil {
				Skip("UserDefinedNetwork CRD not available — network segmentation not enabled")
			}

			By("creating a namespace with UDN label")
			ns, err := fr.ClientSet.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "observ-udn-",
					Labels: map[string]string{
						RequiredUDNNamespaceLabel: "",
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := fr.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}()

			By("creating a primary UserDefinedNetwork")
			udnManifest := generateUserDefinedNetworkManifest(&networkAttachmentConfigParams{
				name:      "observ-udn",
				namespace: ns.Name,
				topology:  "layer2",
				cidr:      filterCIDRsAndJoin(fr.ClientSet, "172.16.0.0/16,2014:100:200::0/60"),
				role:      "primary",
			}, fr.ClientSet)
			cleanup, err := createManifest(ns.Name, udnManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanup)

			By("waiting for UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(fr.DynamicClient, ns.Name, "observ-udn"),
				10*time.Second, time.Second).Should(Succeed())

			By("creating a pod on the UDN namespace")
			pc := *podConfig("observ-udn-pod")
			pc.namespace = ns.Name
			_ = runUDNPod(fr.ClientSet, ns.Name, pc, nil)

			By("verifying UDNIsolation ACLs have sample_new set")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "UDNIsolation")
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected UDNIsolation ACLs to have sample_new references")
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
			// Listen on both nodes: psample events may appear on src or dst node depending
			// on OVN logical flow evaluation location.
			nodeNames := []string{srcPod.Spec.NodeName}
			if srcPod.Spec.NodeName != dstPod.Spec.NodeName {
				nodeNames = append(nodeNames, dstPod.Spec.NodeName)
			}
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, nodeNames, srcPod.Status.PodIP, dstIP, func() {
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
			// allow-related ACL samples fire on the dst node (ingress ACL evaluated there).
			// Listen on both nodes to handle any scheduling outcome.
			allowNodeNames := []string{dstPod.Spec.NodeName}
			if srcPod.Spec.NodeName != dstPod.Spec.NodeName {
				allowNodeNames = append(allowNodeNames, srcPod.Spec.NodeName)
			}
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, allowNodeNames, srcPod.Status.PodIP, dstIP, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
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
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, []string{srcPod.Spec.NodeName}, srcPod.Status.PodIP, "", func() {
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
			// ANP egress deny: listen on both nodes as sample node depends on OVN flow placement.
			anpNodeNames := []string{srcPod.Spec.NodeName}
			if srcPod.Spec.NodeName != dstPod.Spec.NodeName {
				anpNodeNames = append(anpNodeNames, dstPod.Spec.NodeName)
			}
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, anpNodeNames, srcPod.Status.PodIP, dstIP, func() {
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
			// Don't Expect inside trafficFn — it's called in an Eventually loop
			// and early iterations may fail before EgressFirewall is fully enforced.
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, []string{srcPod.Spec.NodeName}, srcPod.Status.PodIP, "", func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, allowIP, 5)
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
			passNodeNames := []string{srcPod.Spec.NodeName}
			if srcPod.Spec.NodeName != dstPod.Spec.NodeName {
				passNodeNames = append(passNodeNames, dstPod.Spec.NodeName)
			}
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, passNodeNames, srcPod.Status.PodIP, dstIP, func() {
				_ = generateTraffic(fr, nsName, srcPod.Name, dstIP, 5)
			})

			By("verifying samples contain pass (delegated) action for AdminNetworkPolicy")
			Expect(output).To(ContainSubstring(fmt.Sprintf("Delegated to network policy by admin network policy %s", anpName)),
				"expected pass/delegated sample for AdminNetworkPolicy, got: %s", output)
		})

		It("should receive samples for multicast traffic", func() {
			nsName := fr.Namespace.Name

			By("enabling multicast for the namespace")
			enableMulticastForNamespace(fr)

			By("creating a pod")
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			srcPod := newAgnhostPod(nsName, "observ-psample-mcast", cmd...)
			srcPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), srcPod)
			Expect(waitForACLLoggingPod(fr, nsName, srcPod.GetName())).To(Succeed())

			By("waiting for MulticastCluster ACLs to be programmed with samples")
			// Assert MulticastCluster specifically since the later assertion checks for
			// "cluster multicast policy" which is produced by MulticastCluster ACLs.
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "MulticastCluster")
			}, 30*time.Second, 2*time.Second).Should(BeTrue(),
				"expected MulticastCluster ACLs to have sample_new references")

			By("starting ovnkube-observ, generating multicast traffic, and collecting samples")
			// Send IGMP join + multicast traffic to 239.1.1.1
			multicastIP := "239.1.1.1"
			if IsIPv6Cluster(fr.ClientSet) {
				multicastIP = "ff05::1"
			}

			output := collectObservSamplesOnNodes(fr, fr.ClientSet, []string{srcPod.Spec.NodeName}, srcPod.Status.PodIP, "", func() {
				// Use ping to multicast address to trigger multicast ACL evaluation
				_ = generateTraffic(fr, nsName, srcPod.Name, multicastIP, 3)
			})

			By("verifying samples contain multicast message")
			Expect(output).To(ContainSubstring("cluster multicast policy"),
				"expected multicast sample, got: %s", output)
		})

		It("should receive samples for UDN isolation traffic", func() {
			By("checking if UDN CRD is available")
			_, err := e2ekubectl.RunKubectl("", "get", "crd", "userdefinednetworks.k8s.ovn.org", "--no-headers")
			if err != nil {
				Skip("UserDefinedNetwork CRD not available — network segmentation not enabled")
			}

			By("creating a namespace with UDN label")
			udnNs, err := fr.ClientSet.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "observ-udn-psample-",
					Labels: map[string]string{
						RequiredUDNNamespaceLabel: "",
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := fr.ClientSet.CoreV1().Namespaces().Delete(context.TODO(), udnNs.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}()

			By("creating a primary UserDefinedNetwork")
			udnManifest := generateUserDefinedNetworkManifest(&networkAttachmentConfigParams{
				name:      "observ-udn-psample",
				namespace: udnNs.Name,
				topology:  "layer2",
				cidr:      filterCIDRsAndJoin(fr.ClientSet, "172.16.0.0/16,2014:100:200::0/60"),
				role:      "primary",
			}, fr.ClientSet)
			cleanup, err := createManifest(udnNs.Name, udnManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanup)

			By("waiting for UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(fr.DynamicClient, udnNs.Name, "observ-udn-psample"),
				10*time.Second, time.Second).Should(Succeed())

			By("creating a UDN pod to trigger isolation ACLs")
			pc := *podConfig("observ-udn-psample-pod")
			pc.namespace = udnNs.Name
			udnPod := runUDNPod(fr.ClientSet, udnNs.Name, pc, nil)

			By("waiting for UDN isolation ACLs to be programmed with samples")
			Eventually(func() (bool, error) {
				return hasACLsWithSamples(fr, fr.ClientSet, "UDNIsolation")
			}, 30*time.Second, 2*time.Second).Should(BeTrue())

			By("getting UDN pod's default cluster network IP via network-status annotation")
			// With primary UDN, the non-default network-status entry is the cluster network.
			clusterNetStatus, err := podNetworkStatus(udnPod, func(status nadapi.NetworkStatus) bool {
				return !status.Default
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterNetStatus).NotTo(BeEmpty(), "expected cluster network status for UDN pod")
			Expect(clusterNetStatus[0].IPs).NotTo(BeEmpty(), "expected cluster network IP for UDN pod")
			clusterNetIP := clusterNetStatus[0].IPs[0]

			By("creating a default-network pod to send traffic to the UDN pod's cluster network IP")
			// Traffic from a default-network pod to the UDN pod's cluster network IP
			// triggers the UDN isolation ingress deny ACL on the UDN pod's default network port.
			defaultNs := fr.Namespace.Name
			cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
			defaultPod := newAgnhostPod(defaultNs, "observ-udn-default-sender", cmd...)
			defaultPod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), defaultPod)
			Expect(waitForACLLoggingPod(fr, defaultNs, defaultPod.GetName())).To(Succeed())

			By("starting ovnkube-observ, generating traffic, and collecting samples")
			// Collect on UDN pod's node since isolation ACL is evaluated there
			output := collectObservSamplesOnNodes(fr, fr.ClientSet, []string{udnPod.Spec.NodeName}, "", clusterNetIP, func() {
				_ = generateTraffic(fr, defaultNs, defaultPod.Name, clusterNetIP, 5)
			})

			By("verifying samples contain UDN isolation message")
			Expect(output).To(ContainSubstring("UDN isolation"),
				"expected UDN isolation sample, got: %s", output)
		})

	})
})

// hasACLsWithSamples checks if ACLs with the given owner type have sample_new set.
// ownerType should match the k8s.ovn.org/owner-type external_id value, e.g. "NetworkPolicy",
// "NetpolNamespace", "EgressFirewall", "AdminNetworkPolicy".
func hasACLsWithSamples(f *framework.Framework, cs clientset.Interface, ownerType string) (bool, error) {
	output, err := runOVNNBCTL(f, cs,
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

// hasACLsWithSamplesForNamespace checks if ACLs with the given owner type and namespace
// have sample_new set. This is namespace-scoped to avoid interference from parallel tests.
func hasACLsWithSamplesForNamespace(f *framework.Framework, cs clientset.Interface, ownerType, namespace string) (bool, error) {
	output, err := runOVNNBCTL(f, cs,
		"--data=bare", "--no-heading", "--columns=sample_new",
		"find", "ACL",
		fmt.Sprintf(`external_ids:"k8s.ovn.org/owner-type"=%s`, ownerType),
		fmt.Sprintf(`external_ids:"k8s.ovn.org/owner"=%s`, namespace))
	if err != nil {
		return false, err
	}
	if output == "" {
		return false, nil
	}
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
	output, err := runOVNNBCTL(f, cs,
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
	if err != nil {
		return false, fmt.Errorf("failed to list ovnkube-node pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return false, fmt.Errorf("no ovnkube-node pod found in namespace %s", ovnNamespace)
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
		cleanupCmd := []string{"/bin/sh", "-c", "pkill -f /usr/bin/ovnkube-observ 2>/dev/null; rm -f /tmp/observ-samples-*.log"}
		_, _, _ = ExecCommandInContainerWithFullOutput(f, ovnNamespace, pod.Name, "nb-ovsdb", cleanupCmd...)
	}
}

// collectObservSamplesOnNodes runs ovnkube-observ on multiple nodes, starting all
// instances simultaneously, then polls all output files until any produces enriched
// output. Use when the node that receives psample events is uncertain.
func collectObservSamplesOnNodes(f *framework.Framework, cs clientset.Interface, nodeNames []string, srcIP, dstIP string, trafficFn func()) string {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	type nodeState struct {
		pod        *v1.Pod
		outputFile string
		pid        string
	}
	states := make([]nodeState, 0, len(nodeNames))

	observCmd := "/usr/bin/ovnkube-observ --enable-enrichment=true --add-ovs-collector"
	if srcIP != "" {
		observCmd += " --filter-src-ip=" + srcIP
	}
	if dstIP != "" {
		observCmd += " --filter-dst-ip=" + dstIP
	}

	// Start ovnkube-observ on all nodes, capturing the PID for precise cleanup.
	for _, nodeName := range nodeNames {
		nodePod, err := findOVNKubeNodePod(cs, ovnNamespace, nodeName)
		Expect(err).NotTo(HaveOccurred())
		outputFile := fmt.Sprintf("/tmp/observ-samples-%d-%s.log", time.Now().UnixNano(), nodeName)
		// Start the process and echo its PID to stdout so we can capture it.
		startCmd := []string{"/bin/sh", "-c",
			fmt.Sprintf("nohup timeout 60 %s > %s 2>&1 & echo $!", observCmd, outputFile),
		}
		pidOut, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, nodePod.Name, "nb-ovsdb", startCmd...)
		Expect(err).NotTo(HaveOccurred())
		pid := strings.TrimSpace(pidOut)
		states = append(states, nodeState{pod: nodePod, outputFile: outputFile, pid: pid})
		defer func(pod *v1.Pod, file, pid string) {
			// Kill only this specific process by PID to avoid affecting parallel tests.
			cleanupCmd := []string{"/bin/sh", "-c",
				fmt.Sprintf("kill %s 2>/dev/null; rm -f %s", pid, file)}
			_, _, _ = ExecCommandInContainerWithFullOutput(f, ovnNamespace, pod.Name, "nb-ovsdb", cleanupCmd...)
		}(nodePod, outputFile, pid)
	}

	// Wait for all instances to start, checking by PID.
	for _, st := range states {
		st := st
		Eventually(func() bool {
			out, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, st.pod.Name, "nb-ovsdb",
				"/bin/sh", "-c", fmt.Sprintf("kill -0 %s 2>/dev/null && echo running", st.pid))
			return err == nil && strings.Contains(out, "running")
		}, 10*time.Second, 1*time.Second).Should(BeTrue(), "ovnkube-observ did not start on node %s", st.pod.Spec.NodeName)
	}

	// Poll all output files until any produces enriched output
	var combinedOutput string
	Eventually(func() string {
		trafficFn()
		var allOutput strings.Builder
		for _, st := range states {
			out, _, err := ExecCommandInContainerWithFullOutput(f, ovnNamespace, st.pod.Name, "nb-ovsdb",
				"cat", st.outputFile)
			if err == nil {
				allOutput.WriteString(out)
			}
		}
		combinedOutput = allOutput.String()
		return combinedOutput
	}, 60*time.Second, 3*time.Second).Should(ContainSubstring("OVN-K message"),
		"ovnkube-observ did not produce enriched sample output on any node")

	framework.Logf("ovnkube-observ combined output:\n%s", combinedOutput)
	return combinedOutput
}


// generateTraffic sends ping packets from srcPod to dstIP to trigger ACL sampling.
// Returns error if ping fails unexpectedly (use for allow rules).
// For deny rules, expect ping to fail.
func generateTraffic(f *framework.Framework, namespace, srcPodName, dstIP string, count int) error {
	cmd := []string{"ping", "-c", fmt.Sprintf("%d", count), "-W", "1", dstIP}
	_, _, err := ExecCommandInContainerWithFullOutput(f, namespace, srcPodName, "", cmd...)
	return err
}
