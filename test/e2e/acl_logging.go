// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	kexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"
)

const (
	logSeverityAnnotation      = "k8s.ovn.org/acl-logging"
	maxPokeRetries             = 15
	ovnControllerLogPath       = "/var/log/openvswitch/ovn-controller.log"
	ovnControllerContainerName = "ovn-controller"
	pokeInterval               = 1 * time.Second
	aclLogRequestTimeout       = 5 * time.Second
	podExecRequestTimeout      = 5 * time.Second
)

var _ = Describe("ACL Logging for NetworkPolicy", feature.NetworkPolicy, func() {
	const (
		denyAllPolicyName       = "default-deny-all"
		initialDenyACLSeverity  = "alert"
		initialAllowACLSeverity = "notice"
		denyACLVerdict          = "drop"
		namespacePrefix         = "acl-logging-netpol"
		pokerPodIndex           = 0
		pokedPodIndex           = 1
		egressDefaultDenySuffix = "Egress"
	)

	fr := wrappedTestFramework(namespacePrefix)

	var (
		nsName string
		pods   []v1.Pod
	)

	BeforeEach(func(specCtx context.Context) {
		By("configuring the ACL logging level within the namespace")
		nsName = fr.Namespace.Name
		Expect(setNamespaceACLLogSeverity(fr, nsName, initialDenyACLSeverity, initialAllowACLSeverity, aclRemoveOptionDelete)).To(Succeed())

		By("creating a \"default deny\" network policy")
		createPolicyCtx, cancelCreatePolicy := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := makeDenyAllPolicyWithContext(createPolicyCtx, fr, nsName, denyAllPolicyName)
		cancelCreatePolicy()
		Expect(err).NotTo(HaveOccurred())

		By("creating pods")
		cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		for i := 0; i < 2; i++ {
			pod := newAgnhostPod(nsName, fmt.Sprintf("pod%d", i+1), cmd...)
			pod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, nsName, pod.GetName())).To(Succeed())
			pods = append(pods, *pod)
		}

		By("sending traffic between acl-logging test pods we trigger ACL logging")
		clientPod := pods[pokerPodIndex]
		pokedPod := pods[pokedPodIndex]
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		By("waiting for traffic between the pods to be blocked by the NetworkPolicy")
		Eventually(func(probeCtx context.Context) (bool, error) {
			return probePodIPsWithContext(probeCtx, fr, clientPod.GetName(), getPodIPs(&pokedPod))
		}, maxPokeRetries*pokeInterval, pokeInterval).WithContext(specCtx).Should(BeTrue(),
			"traffic to every target pod IP should be blocked by the deny-all policy")
	})

	AfterEach(func() {
		pods = nil
	})

	It("the logs have the expected log level", func(specCtx context.Context) {
		clientPod := pods[pokerPodIndex]
		pokedPod := pods[pokedPodIndex]
		policyName := fmt.Sprintf("NP:%s:%s", nsName, egressDefaultDenySuffix)
		podIPs := getPodIPs(&pokedPod)
		Expect(podIPs).NotTo(BeEmpty(), "target pod should have at least one IP")
		Eventually(func(logCtx context.Context) (bool, error) {
			return assertACLLogsAfterProbingPodIPsWithContext(logCtx, fr, &clientPod, podIPs,
				policyName, denyACLVerdict, initialDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).WithContext(specCtx).Should(BeTrue(),
			fmt.Sprintf("traffic should remain blocked and ACL logs should show severity=%s for policy %s", initialDenyACLSeverity, policyName))
	})

	It("should still process OVN annotation changes after rapid non-OVN annotation spam", func(specCtx context.Context) {
		const (
			updatedDenyACLSeverity = "warning"
			annotationSpamCount    = 50
			apiRequestTimeout      = 30 * time.Second
		)

		clientPod := pods[pokerPodIndex]
		pokedPod := pods[pokedPodIndex]
		policyName := fmt.Sprintf("NP:%s:%s", nsName, egressDefaultDenySuffix)
		podIPs := getPodIPs(&pokedPod)
		Expect(podIPs).NotTo(BeEmpty(), "target pod should have at least one IP")

		By("confirming traffic to every pod IP is blocked and initial ACL logging is active")
		Eventually(func(logCtx context.Context) (bool, error) {
			return assertACLLogsAfterProbingPodIPsWithContext(logCtx, fr, &clientPod, podIPs,
				policyName, "drop", initialDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).WithContext(specCtx).Should(BeTrue(),
			fmt.Sprintf("traffic to every target pod IP should be blocked and ACL logs should show severity=%s for policy %s", initialDenyACLSeverity, policyName))

		By(fmt.Sprintf("spamming namespace with %d non-OVN annotation updates", annotationSpamCount))
		annotationResults := make(chan error, annotationSpamCount)
		for i := 0; i < annotationSpamCount; i++ {
			go func(revision int) {
				patchCtx, cancelPatch := context.WithTimeout(specCtx, apiRequestTimeout)
				defer cancelPatch()
				patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"argocd.argoproj.io/managed-by":"revision-%d"}}}`, revision))
				_, err := fr.ClientSet.CoreV1().Namespaces().Patch(patchCtx, nsName, ktypes.MergePatchType, patch, metav1.PatchOptions{})
				if err != nil {
					err = fmt.Errorf("namespace annotation spam iteration %d failed: %w", revision, err)
				}
				annotationResults <- err
			}(i)
		}
		var annotationErrors []error
		for i := 0; i < annotationSpamCount; i++ {
			if err := <-annotationResults; err != nil {
				annotationErrors = append(annotationErrors, err)
			}
		}
		Expect(annotationErrors).To(BeEmpty(), "namespace annotation spam failed")

		By("updating ACL logging severity after the annotation spam")
		updatedACLContext, cancelUpdatedACL := context.WithTimeout(specCtx, apiRequestTimeout)
		err := setNamespaceACLLogSeverityWithContext(updatedACLContext, fr, nsName,
			updatedDenyACLSeverity, updatedDenyACLSeverity, aclRemoveOptionDelete)
		cancelUpdatedACL()
		Expect(err).To(Succeed(),
			fmt.Sprintf("failed to set ACL log severity to %s after annotation spam", updatedDenyACLSeverity))

		By("verifying traffic remains blocked for every pod IP and ACL logs reflect the updated severity")
		Eventually(func(logCtx context.Context) (bool, error) {
			return assertACLLogsAfterProbingPodIPsWithContext(logCtx, fr, &clientPod, podIPs,
				policyName, "drop", updatedDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).WithContext(specCtx).Should(BeTrue(),
			fmt.Sprintf("traffic to every target pod IP should remain blocked and ACL logs should show severity=%s for policy %s after annotation spam", updatedDenyACLSeverity, policyName))
	})

	When("the namespace's ACL logging annotation is updated", func() {
		const updatedAllowACLLogSeverity = "debug"

		BeforeEach(func() {
			By(fmt.Sprintf("updating the namespace's ACL logging level to %s", updatedAllowACLLogSeverity))
			Expect(setNamespaceACLLogSeverity(fr, nsName, updatedAllowACLLogSeverity, updatedAllowACLLogSeverity, aclRemoveOptionDelete)).To(Succeed())
		})

		It("the ACL logs are updated accordingly", func(specCtx context.Context) {
			policyName := fmt.Sprintf("NP:%s:%s", nsName, egressDefaultDenySuffix)
			clientPod := pods[pokerPodIndex]
			pokedPod := pods[pokedPodIndex]
			podIPs := getPodIPs(&pokedPod)
			Expect(podIPs).NotTo(BeEmpty(), "target pod should have at least one IP")
			Eventually(func(logCtx context.Context) (bool, error) {
				return assertACLLogsAfterProbingPodIPsWithContext(logCtx, fr, &clientPod, podIPs,
					policyName, denyACLVerdict, updatedAllowACLLogSeverity)
			}, maxPokeRetries*pokeInterval, pokeInterval).WithContext(specCtx).Should(BeTrue(),
				fmt.Sprintf("traffic should remain blocked and ACL logs should show severity=%s for policy %s", updatedAllowACLLogSeverity, policyName))
		})
	})

	When("the namespace's ACL logging annotation is removed", func() {
		BeforeEach(func() {
			By("removing the ACL logging annotation")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "", "", aclRemoveOptionDelete)).To(Succeed())
		})

		It("ACL logging is disabled", func() {
			clientPod := pods[pokerPodIndex]
			pokedPod := pods[pokedPodIndex]
			composedPolicyNameRegex := fmt.Sprintf("NP:%s:%s", nsName, egressDefaultDenySuffix)
			Consistently(func() (bool, error) {
				return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
			}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())
		})
	})

	When("the namespace's ACL allow and deny logging annotations are set to invalid values", func() {
		BeforeEach(func() {
			By("setting invalid values for ACL logging annotation")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "invalid", "invalid", aclRemoveOptionDelete)).To(Succeed())
		})

		It("ACL logging is disabled", func() {
			clientPod := pods[pokerPodIndex]
			pokedPod := pods[pokedPodIndex]
			composedPolicyNameRegex := fmt.Sprintf("NP:%s:%s", nsName, egressDefaultDenySuffix)
			Consistently(func() (bool, error) {
				return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
			}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())
		})
	})
})

var _ = Describe("ACL Logging for AdminNetworkPolicy and BaselineAdminNetworkPolicy", feature.AdminNetworkPolicy, feature.BaselineNetworkPolicy, func() {
	const (
		initialDenyACLSeverity  = "alert"
		initialAllowACLSeverity = "notice"
		initialPassACLSeverity  = "warning"
		denyACLVerdict          = "drop"
		allowACLVerdict         = "allow"
		passACLVerdict          = "pass"
		anpName                 = "harry-potter"
	)
	fr := wrappedTestFramework("anp-subject")
	var (
		pods    []v1.Pod
		nsNames [4]string
	)
	BeforeEach(func() {
		nsNames[0] = fr.Namespace.Name
		suffix := framework.RandomSuffix()
		nsNames[1] = fmt.Sprintf("anp-peer-restricted-%s", suffix)
		nsNames[2] = fmt.Sprintf("anp-peer-open-%s", suffix)
		nsNames[3] = fmt.Sprintf("anp-peer-unknown-%s", suffix)

		By("creating an admin network policy")
		err := makeAdminNetworkPolicy(anpName, "10", fr.Namespace.Name, nsNames[1], nsNames[2], nsNames[3])
		Expect(err).NotTo(HaveOccurred())

		By("configuring the ACL logging level for the ANP")
		Expect(setANPACLLogSeverity(anpName, initialDenyACLSeverity, initialAllowACLSeverity, initialPassACLSeverity)).To(Succeed())

		By("creating peer namespaces that are selected by the admin network policy")
		for _, ns := range nsNames[1:] {
			_, err = e2ekubectl.RunKubectl("default", "create", "ns", ns)
			Expect(err).NotTo(HaveOccurred())
		}

		By("creating pods in subject and peer namespaces")
		cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		for _, ns := range nsNames {
			pod := newAgnhostPod(ns, fmt.Sprintf("pod-%s", ns), cmd...)
			pod = e2epod.PodClientNS(fr, ns).CreateSync(context.TODO(), pod)
			Expect(waitForACLLoggingPod(fr, ns, pod.GetName())).To(Succeed())
			pods = append(pods, *pod)
			framework.Logf("Created %s in namespace %s", pod.Name, pod.Namespace)
		}
	})
	AfterEach(func() {
		By("deleting the admin network policy")
		_, err := e2ekubectl.RunKubectl("default", "delete", "anp", anpName, "--ignore-not-found=true")
		Expect(err).NotTo(HaveOccurred())
		By("deleting the baseline admin network policy")
		_, err = e2ekubectl.RunKubectl("default", "delete", "banp", "default", "--ignore-not-found=true")
		Expect(err).NotTo(HaveOccurred())
		By("deleting subject and peer namespaces that are selected by the admin network policy")
		for _, ns := range nsNames {
			_, err := e2ekubectl.RunKubectl("default", "delete", "ns", ns, "--ignore-not-found=true")
			Expect(err).NotTo(HaveOccurred())
		}
		// reset caches
		pods = nil
		nsNames = [4]string{}
	})

	It("the ANP ACL logs have the expected log level", func() {

		By("sending traffic between acl-logging test pods we trigger ALLOW ACL logging")
		clientPod := pods[0] // subject pod
		pokedPod := pods[1]  // peer allow pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err := pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("traffic should be allowed since we use an ALLOW all traffic policy rule, err: %v", err))

		By("verify the ALLOW ACL log level at Tier1")
		clientPodScheduledPodName := pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex := fmt.Sprintf("ANP:%s:Egress:0", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				allowACLVerdict,
				initialAllowACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("sending traffic between acl-logging test pods we trigger DENY ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[2]  // peer deny pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be denied since we use an DENY all traffic policy rule, err %v", err))

		By("verify the DENY ACL log level at Tier1")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:1", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				denyACLVerdict,
				initialDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("sending traffic between acl-logging test pods we trigger PASS ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[3]  // peer pass pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("traffic should be allowed since we only use an PASS all traffic policy rule, err %v", err))

		By("verify the PASS ACL log level at Tier1")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:2", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				passACLVerdict,
				initialPassACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("creating a baseline admin network policy")
		err = makeBaselineAdminNetworkPolicy(fr.Namespace.Name, nsNames[1], nsNames[3])
		Expect(err).NotTo(HaveOccurred())

		By("configuring the ACL logging level for the BANP")
		Expect(setBANPACLLogSeverity(initialDenyACLSeverity, initialAllowACLSeverity)).To(Succeed())

		// BANP Deny will be hit
		By("sending traffic between acl-logging test pods we trigger PASS ACL logging followed by DENY ACL logging(BANP)")
		clientPod = pods[0] // subject pod
		pokedPod = pods[3]  // ANP peer pass pod + BANP peer deny pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be blocked since we use an PASS traffic policy followed by a deny at lower tier, err %v", err))

		By("verify the DENY ACL log level at Tier3")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		composedPolicyNameRegex = "BANP:default:Egress:1"
		// Retry here in the case where OVN acls have not been programmed yet
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				denyACLVerdict,
				initialDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("updating the ACL logging level for the ANP")
		Expect(setANPACLLogSeverity(anpName, "warning", "info", "notice")).To(Succeed())

		By("updating the ACL logging level for the BANP")
		Expect(setBANPACLLogSeverity("warning", "info")).To(Succeed())

		By("sending traffic between acl-logging test pods we trigger ALLOW ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[1]  // peer allow pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("traffic should be allowed since we use an ALLOW all traffic policy rule, err %v", err))

		By("verify the ALLOW ACL log level at Tier1")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:0", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				allowACLVerdict,
				"info") // updated log level
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("sending traffic between acl-logging test pods we trigger DENY ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[2]  // peer deny pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be denied since we use an DENY all traffic policy rule, err %v", err))

		By("verify the DENY ACL log level at Tier1")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:1", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				denyACLVerdict,
				"warning") // updated log level
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("sending traffic between acl-logging test pods we trigger PASS ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[3]  // peer pass pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be blocked since we use an PASS traffic policy followed by a deny at lower tier, err %v", err))

		By("verify the PASS ACL log level at Tier1")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:2", anpName)
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				passACLVerdict,
				"notice") // updated log level
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		// BANP Deny will be hit
		By("verify the DENY ACL log level at Tier3")
		clientPodScheduledPodName = pods[0].Spec.NodeName
		composedPolicyNameRegex = "BANP:default:Egress:1"
		// Retry here in the case where OVN acls have not been programmed yet
		Eventually(func() (bool, error) {
			return assertACLLogs(
				clientPodScheduledPodName,
				composedPolicyNameRegex,
				denyACLVerdict,
				"warning") // updated log level
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

		By("disabling the ACL logging for the ANP")
		Expect(setANPACLLogSeverity(anpName, "", "", "")).To(Succeed())

		By("disabling the ACL logging for the BANP")
		Expect(setBANPACLLogSeverity("", "")).To(Succeed())

		By("sending traffic between acl-logging test pods we trigger NO ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[3]  // peer pass pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be blocked since we use an PASS traffic policy followed by a deny at lower tier, err %v", err))

		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:2", anpName)
		Consistently(func() (bool, error) {
			return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

		composedPolicyNameRegex = "BANP:default:Egress:1"
		Consistently(func() (bool, error) {
			return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

		By("invalid ACL logging for the ANP")
		Expect(setANPACLLogSeverity(anpName, "pooh", "poop", "peep")).To(Succeed())

		By("invalid ACL logging for the BANP")
		Expect(setBANPACLLogSeverity("boop", "beep")).To(Succeed())

		By("sending traffic between acl-logging test pods we trigger NO ACL logging")
		clientPod = pods[0] // subject pod
		pokedPod = pods[3]  // peer pass pod
		framework.Logf(
			"Poke pod %s (on node %s) from pod %s (on node %s)",
			pokedPod.GetName(),
			pokedPod.Spec.NodeName,
			clientPod.GetName(),
			clientPod.Spec.NodeName)
		err = pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("traffic should be blocked since we use an PASS traffic policy followed by a deny at lower tier, err %v", err))

		composedPolicyNameRegex = fmt.Sprintf("ANP:%s:Egress:2", anpName)
		Consistently(func() (bool, error) {
			return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

		composedPolicyNameRegex = "BANP:default:Egress:1"
		Consistently(func() (bool, error) {
			return isCountUpdatedAfterPokePod(fr, &clientPod, &pokedPod, composedPolicyNameRegex, denyACLVerdict, "")
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())
	})
})

var _ = Describe("ACL Logging for EgressFirewall", feature.EgressFirewall, func() {
	const (
		denyAllPolicyName        = "default-deny-all"
		initialDenyACLSeverity   = "alert"
		initialAllowACLSeverity  = "notice"
		updatedDenyACLSeverity   = "debug"
		updatedAllowACLSeverity  = "debug"
		denyACLVerdict           = "drop"
		allowACLVerdict          = "allow"
		namespacePrefix          = "acl-log-egressfw"
		secondaryNamespacePrefix = "acl-log-egressfw-sec"
		dstPort                  = 8080
	)

	fr := wrappedTestFramework(namespacePrefix)

	var (
		nsName           string
		nsNameSecondary  string
		pokePod          *v1.Pod
		pokePodSecondary *v1.Pod
		allowedDstIP     string
		deniedDstIP      string
	)

	BeforeEach(func() {
		// These targets must be off cluster - traffic to the cluster should always be
		// allowed: https://docs.openshift.com/container-platform/4.10/networking/openshift_sdn/configuring-egress-firewall.html
		// "As a cluster administrator, you can create an egress firewall for a project that restricts egress traffic leaving
		// your OpenShift Container Platform cluster."
		// Because the egress firewall feature only affects traffic leaving the cluster, we will not log for on-cluster targets.
		allowedDstIP = "172.18.0.1"
		deniedDstIP = "172.19.0.10"
		mask := "32"
		denyCIDR := "0.0.0.0/0"
		if IsIPv6Cluster(fr.ClientSet) {
			allowedDstIP = "2001:4860:4860::8888"
			deniedDstIP = "2001:4860:4860::8844"
			mask = "128"
			denyCIDR = "::/0"
		}
		By("configuring the ACL logging level within the namespace")
		nsName = fr.Namespace.Name
		Expect(setNamespaceACLLogSeverity(fr, nsName, initialDenyACLSeverity, initialAllowACLSeverity, aclRemoveOptionDelete)).To(Succeed())

		By("creating a \"default deny\" Egress Firewall")
		err := makeEgressFirewall(nsName, allowedDstIP, mask, denyCIDR)
		Expect(err).NotTo(HaveOccurred())

		By("creating a pod running agnhost netexec")
		cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		pod := newAgnhostPod(nsName, "pod", cmd...)
		pokePod = e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
		Expect(waitForACLLoggingPod(fr, nsName, pokePod.GetName())).To(Succeed())

		// The secondary Namespace is required to make sure that 2 namespaces with different logging
		// settings can coexist and that updates to a specific namespace only affect that namespace and
		// not other namespaces.
		By("creating a secondary namespace")
		ns2, err := fr.CreateNamespace(context.TODO(), secondaryNamespacePrefix, map[string]string{})
		Expect(err).NotTo(HaveOccurred(), "failed to create secondary namespace")

		By("configuring the ACL logging level within the secondary namespace")
		nsNameSecondary = ns2.Name
		Expect(setNamespaceACLLogSeverity(fr, nsNameSecondary, initialDenyACLSeverity, initialAllowACLSeverity, aclRemoveOptionDelete)).To(Succeed())

		By("creating a \"default deny\" Egress Firewall inside the secondary namespace")
		err = makeEgressFirewall(nsNameSecondary, allowedDstIP, mask, denyCIDR)
		Expect(err).NotTo(HaveOccurred())

		By("creating a pod running agnhost netexec inside the secondary namespace")
		cmdSecondary := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		podSecondary := newAgnhostPod(nsNameSecondary, "pod-secondary", cmdSecondary...)
		// There seems to be a bug in CreateSync for secondary pod. Need to do this here instead:
		pps := e2epod.PodClientNS(fr, nsNameSecondary).Create(context.TODO(), podSecondary)
		Eventually(func() (bool, error) {
			time.Sleep(15 * time.Second)
			pokePodSecondary, err = fr.ClientSet.CoreV1().Pods(nsNameSecondary).Get(context.TODO(), pps.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pokePodSecondary.Status.Phase == v1.PodRunning, nil
		}, 60, 5).Should(BeTrue())
		Expect(waitForACLLoggingPod(fr, nsNameSecondary, pokePodSecondary.GetName())).To(Succeed())
	})

	AfterEach(func() {
		pokePod = nil
	})

	When("the namespace is brought up with the initial ACL log severity", func() {
		When("the denied destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("the namespace's ACL logging annotation is updated", func() {
		BeforeEach(func() {
			By(fmt.Sprintf("updating the namespace's ACL logging level to %s for deny and %s for allow", updatedDenyACLSeverity, updatedAllowACLSeverity))
			Expect(setNamespaceACLLogSeverity(fr, nsName, updatedDenyACLSeverity, updatedAllowACLSeverity, aclRemoveOptionDelete)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, updatedDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, updatedAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("the namespace's ACL logging allow annotation is removed", func() {
		BeforeEach(func() {
			By("removing the namespace's ACL logging allow configuration")
			Expect(setNamespaceACLLogSeverity(fr, nsName, initialDenyACLSeverity, "", aclRemoveOptionDelete)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("the namespace's entire ACL logging annotation is removed", func() {
		BeforeEach(func() {
			By("removing the namespace's entire ACL logging configuration")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "", "", aclRemoveOptionDelete)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("the namespace's entire ACL logging annotation is set to {}", func() {
		BeforeEach(func() {
			By("setting the namespace's entire ACL logging configuration to {}")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "", "", aclRemoveOptionEmptyMap)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("both the namespace's ACL logging deny and allow annotation are set to \"\"", func() {
		BeforeEach(func() {
			By("setting the namespace's deny annotation to \"\" and the allow annotation to \"\"")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "", "", aclRemoveOptionEmptyString)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("an invalid value is provided to the allow rule", func() {
		BeforeEach(func() {
			By(fmt.Sprintf("setting the namespace's allow annotation to \"%s\" and the allow annotation to \"invalid\"",
				initialDenyACLSeverity))
			Expect(setNamespaceACLLogSeverity(fr, nsName, initialDenyACLSeverity, "invalid", aclRemoveOptionDelete)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("the logs should have the expected log level", func() {
				// Retry here in the case where OVN acls have not been programmed yet
				// Make sure that we see an increment in count
				By("testing the primary namespace")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("both the namespace's ACL logging deny and allow annotation are set to \"invalid\"", func() {
		BeforeEach(func() {
			By("setting the namespace's deny annotation to \"invalid\" and the allow annotation to \"invalid\"")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "invalid", "invalid", aclRemoveOptionEmptyString)).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})

	When("the namespace's ACL logging annotation cannot be parsed", func() {
		BeforeEach(func() {
			By("setting the namespace's annotation to value \"cannot-be-parsed\"")
			Expect(setNamespaceACLLogSeverity(fr, nsName, "invalid", "invalid", aclRemoveOptionEmptyString)).To(Succeed())
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				namespaceToUpdate, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
				if err != nil {
					return err
				}

				if namespaceToUpdate.ObjectMeta.Annotations == nil {
					namespaceToUpdate.ObjectMeta.Annotations = map[string]string{}
				}

				namespaceToUpdate.Annotations[logSeverityAnnotation] = "cannot-be-parsed"
				_, err = fr.ClientSet.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
				return err
			})).To(Succeed())
		})

		When("the denied destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, deniedDstIP, dstPort, denyACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, deniedDstIP, dstPort, denyACLVerdict, initialDenyACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})

		When("the allowed destination is poked", func() {
			It("there should be no trace in the ACL logs", func() {
				// Retry here until timeout is reached
				// Make sure that we see no increment in count
				By("testing the primary namespace")
				Consistently(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePod, nsName, allowedDstIP, dstPort, allowACLVerdict, "")
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeFalse())

				By("making sure that the secondary namespace logs as expected")
				Eventually(func() (bool, error) {
					return isCountUpdatedAfterPokeExternalHost(fr, pokePodSecondary, nsNameSecondary, allowedDstIP, dstPort, allowACLVerdict, initialAllowACLSeverity)
				}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
			})
		})
	})
})

// makeDenyAllPolicy creates a deny-all policy with a bounded request context.
func makeDenyAllPolicy(f *framework.Framework, ns, policyName string) (*knet.NetworkPolicy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return makeDenyAllPolicyWithContext(ctx, f, ns, policyName)
}

// makeDenyAllPolicyWithContext creates a deny-all policy using the caller's request context.
func makeDenyAllPolicyWithContext(ctx context.Context, f *framework.Framework, ns, policyName string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress, knet.PolicyTypeIngress},
			Ingress:     []knet.NetworkPolicyIngressRule{},
			Egress:      []knet.NetworkPolicyEgressRule{},
		},
	}
	createdPolicy, err := f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating NetworkPolicy %s/%s: %w", ns, policyName, err)
	}
	return createdPolicy, nil
}

func makeAdminNetworkPolicy(anpName, priority, anpSubjectNS, restrictedPeerNS, openPeerNS, unknownPeerNS string) error {
	anpYaml := "anp.yaml"
	var anpConfig = fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
metadata:
  name: %s
spec:
  priority: %s
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "allow-to-restricted"
    action: "Allow"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: %s
  - name: "deny-to-open"
    action: "Deny"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: %s
  - name: "pass-to-unknown"
    action: "Pass"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: %s
`, anpName, priority, anpSubjectNS, restrictedPeerNS, openPeerNS, unknownPeerNS)

	if err := os.WriteFile(anpYaml, []byte(anpConfig), 0644); err != nil {
		framework.Failf("Unable to write CRD config to disk: %v", err)
	}

	defer func() {
		if err := os.Remove(anpYaml); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	}()

	_, err := e2ekubectl.RunKubectl("default", "create", "-f", anpYaml)
	return err
}

func makeBaselineAdminNetworkPolicy(banpSubjectNS, restrictedPeerNS, unknownPeerNS string) error {
	banpYaml := "banp.yaml"
	var banpConfig = fmt.Sprintf(`apiVersion: policy.networking.k8s.io/v1alpha1
kind: BaselineAdminNetworkPolicy
metadata:
  name: default
spec:
  subject:
    namespaces:
      matchLabels:
        kubernetes.io/metadata.name: %s
  egress:
  - name: "allow-to-restricted"
    action: "Allow"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: %s
  - name: "deny-to-unknown"
    action: "Deny"
    to:
    - namespaces:
        matchLabels:
          kubernetes.io/metadata.name: %s
`, banpSubjectNS, restrictedPeerNS, unknownPeerNS)

	if err := os.WriteFile(banpYaml, []byte(banpConfig), 0644); err != nil {
		framework.Failf("Unable to write CRD config to disk: %v", err)
	}

	defer func() {
		if err := os.Remove(banpYaml); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	}()

	_, err := e2ekubectl.RunKubectl("default", "create", "-f", banpYaml)
	return err
}

func makeEgressFirewall(ns, allowedDstIP, mask, denyCIDR string) error {
	egressFirewallYaml := "egressfirewall.yaml"
	var egressFirewallConfig = fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: EgressFirewall
metadata:
  name: default
  namespace: `+ns+`
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Deny
    to:
      cidrSelector: %s
`, allowedDstIP, mask, denyCIDR)

	if err := os.WriteFile(egressFirewallYaml, []byte(egressFirewallConfig), 0644); err != nil {
		framework.Failf("Unable to write CRD config to disk: %v", err)
	}

	defer func() {
		if err := os.Remove(egressFirewallYaml); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	}()

	_, err := e2ekubectl.RunKubectl(ns, "create", "-f", egressFirewallYaml)
	return err
}

func waitForACLLoggingPod(f *framework.Framework, namespace string, podName string) error {
	return e2epod.WaitForPodCondition(context.TODO(), f.ClientSet, namespace, podName, "running", 5*time.Second, func(pod *v1.Pod) (bool, error) {
		podIP := pod.Status.PodIP
		return podIP != "" && pod.Status.Phase != v1.PodPending, nil
	})
}

func isCountUpdatedAfterPokeExternalHost(fr *framework.Framework, pokePod *v1.Pod, nsName, dstIP string, dstPort int, aclVerdict, aclSeverity string) (bool, error) {
	startCount, err := countACLLogs(
		pokePod.Spec.NodeName,
		generateEgressFwRegex(pokePod.Namespace),
		aclVerdict,
		aclSeverity)
	if err != nil {
		return false, err
	}
	pokeExternalHost(fr, pokePod, dstIP, dstPort)
	endCount, _ := countACLLogs(
		pokePod.Spec.NodeName,
		generateEgressFwRegex(pokePod.Namespace),
		aclVerdict,
		aclSeverity)
	if err != nil {
		return false, err
	}
	return startCount < endCount, nil
}

func isCountUpdatedAfterPokePod(fr *framework.Framework, clientPod, pokedPod *v1.Pod, regex, aclVerdict, aclSeverity string) (bool, error) {
	startCount, err := countACLLogs(
		clientPod.Spec.NodeName,
		regex,
		aclVerdict,
		aclSeverity)
	if err != nil {
		return false, err
	}
	pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)
	endCount, _ := countACLLogs(
		clientPod.Spec.NodeName,
		regex,
		aclVerdict,
		aclSeverity)
	if err != nil {
		return false, err
	}
	return startCount < endCount, nil
}

func generateEgressFwRegex(nsName string) string {
	return fmt.Sprintf("EF:%s:.*", nsName)
}

func pokeExternalHost(fr *framework.Framework, pokePod *v1.Pod, dstIP string, dstPort int) {
	framework.Logf("sending traffic outside to test triggering ACL logging")
	framework.Logf(
		"Poke destination %s:%d from pod %s/%s (on node %s)",
		dstIP,
		dstPort,
		pokePod.Namespace,
		pokePod.GetName(),
		pokePod.Spec.NodeName,
	)
	pokeExternalHostFromPod(fr, pokePod.Namespace, pokePod.GetName(), dstIP, dstPort)
}

const (
	aclRemoveOptionEmptyMap    = "empty-map"    // Set the annotation to "{}" if both allow and deny are "".
	aclRemoveOptionEmptyString = "empty-string" // Set the field's value to "".
	aclRemoveOptionDelete      = ""             // Delete the field entry if it's value is "".
)

// setNamespaceACLLogSeverity updates namespace ACL logging using a bounded request context.
func setNamespaceACLLogSeverity(fr *framework.Framework, nsName string, desiredDenyLogLevel string, desiredAllowLogLevel string, removeOption string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return setNamespaceACLLogSeverityWithContext(ctx, fr, nsName, desiredDenyLogLevel, desiredAllowLogLevel, removeOption)
}

// setNamespaceACLLogSeverityWithContext updates the ACL logging annotation using the caller's request context.
func setNamespaceACLLogSeverityWithContext(ctx context.Context, fr *framework.Framework, nsName string, desiredDenyLogLevel string, desiredAllowLogLevel string, removeOption string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		namespaceToUpdate, err := fr.ClientSet.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting namespace %q for ACL logging update: %w", nsName, err)
		}

		if namespaceToUpdate.ObjectMeta.Annotations == nil {
			namespaceToUpdate.ObjectMeta.Annotations = map[string]string{}
		}

		aclLogSeverity := ""
		if removeOption == aclRemoveOptionEmptyString || desiredDenyLogLevel != "" && desiredAllowLogLevel != "" {
			aclLogSeverity = fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
			By(fmt.Sprintf("updating the namespace's ACL logging severity to %s", aclLogSeverity))
			namespaceToUpdate.Annotations[logSeverityAnnotation] = aclLogSeverity
		} else if removeOption == aclRemoveOptionEmptyMap && desiredDenyLogLevel == "" && desiredAllowLogLevel == "" {
			aclLogSeverity = "{}"
			By(fmt.Sprintf("updating the namespace's ACL logging severity to %s", aclLogSeverity))
			namespaceToUpdate.Annotations[logSeverityAnnotation] = aclLogSeverity
		} else {
			if desiredDenyLogLevel != "" {
				aclLogSeverity = fmt.Sprintf(`{ "deny": "%s" }`, desiredDenyLogLevel)
				By(fmt.Sprintf("updating the namespace's ACL logging severity to %s", aclLogSeverity))
				namespaceToUpdate.Annotations[logSeverityAnnotation] = aclLogSeverity
			} else if desiredAllowLogLevel != "" {
				aclLogSeverity = fmt.Sprintf(`{ "allow": "%s" }`, desiredAllowLogLevel)
				By(fmt.Sprintf("updating the namespace's ACL logging severity to %s", aclLogSeverity))
				namespaceToUpdate.Annotations[logSeverityAnnotation] = aclLogSeverity
			} else {
				By("removing the namespace's ACL logging severity annotation if it exists")
				delete(namespaceToUpdate.Annotations, logSeverityAnnotation)
			}
		}

		_, err = fr.ClientSet.CoreV1().Namespaces().Update(ctx, namespaceToUpdate, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("updating ACL logging annotation on namespace %q: %w", nsName, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("retrying ACL logging update for namespace %q: %w", nsName, err)
	}
	return nil
}

// setANPACLLogSeverity updates ANP with the deny, pass and allow annotations, e.g. k8s.ovn.org/acl-logging={ "deny": "%s", "allow": "%s", "pass": "%s" }.
func setANPACLLogSeverity(anpName, desiredDenyLogLevel, desiredAllowLogLevel, desiredPassLogLevel string) error {
	_, err := e2ekubectl.RunKubectl("default", "annotate", "anp", anpName,
		fmt.Sprintf(`%s={ "deny": "%s", "allow": "%s", "pass": "%s" }`, logSeverityAnnotation, desiredDenyLogLevel, desiredAllowLogLevel, desiredPassLogLevel),
		"--overwrite=true")
	if err != nil {
		return fmt.Errorf("unable to annotate admin network policy %s: err %v", anpName, err)
	}
	return nil
}

// assertACLLogsWithContext reads matching ACL logs through the node's ovnkube pod with a bounded Kubernetes exec.
func assertACLLogsWithContext(ctx context.Context, fr *framework.Framework, targetNodeName, policyName, expectedACLVerdict, expectedACLSeverity string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, aclLogRequestTimeout)
	defer cancel()

	policyNameMatcher := fmt.Sprintf(`name="%s"`, policyName)

	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	nodePods, err := fr.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + targetNodeName,
	})
	if err != nil {
		return false, fmt.Errorf("listing ovnkube pods on node %s: %w", targetNodeName, err)
	}
	if len(nodePods.Items) == 0 {
		return false, fmt.Errorf("no ovnkube pod found on node %s", targetNodeName)
	}
	nodePod := &nodePods.Items[0]
	containerName := getNodeContainerName()
	for _, container := range nodePod.Spec.Containers {
		// KIND mounts the OVN controller log in its dedicated container.
		if container.Name == ovnControllerContainerName {
			containerName = container.Name
			break
		}
	}

	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	logOutput, stderr, err := e2epod.ExecWithOptionsContext(ctx, fr, e2epod.ExecOptions{
		Command:            []string{"grep", "-F", policyNameMatcher, ovnControllerLogPath},
		Namespace:          ovnNamespace,
		PodName:            nodePod.Name,
		ContainerName:      containerName,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: false,
	})
	if err != nil {
		var exitError kexec.CodeExitError
		if errors.As(err, &exitError) && exitError.ExitStatus() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("reading ACL logs from node %s (stderr: %q): %w", targetNodeName, stderr, err)
	}

	for _, logLine := range strings.Split(logOutput, "\n") {
		if strings.Contains(logLine, policyNameMatcher) &&
			strings.Contains(logLine, fmt.Sprintf("verdict=%s", expectedACLVerdict)) &&
			strings.Contains(logLine, fmt.Sprintf("severity=%s", expectedACLSeverity)) {
			return true, nil
		}
	}
	return false, nil
}

func getPodIPs(pod *v1.Pod) []string {
	podIPs := make([]string, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		if podIP.IP != "" {
			podIPs = append(podIPs, podIP.IP)
		}
	}
	if len(podIPs) == 0 && pod.Status.PodIP != "" {
		podIPs = append(podIPs, pod.Status.PodIP)
	}
	return podIPs
}

func probePodIPsWithContext(ctx context.Context, fr *framework.Framework, srcPodName string, targetPodIPs []string) (bool, error) {
	if len(targetPodIPs) == 0 {
		return false, fmt.Errorf("no target pod IPs to probe from pod %s/%s", fr.Namespace.Name, srcPodName)
	}

	allTrafficBlocked := true
	for _, podIP := range targetPodIPs {
		probeCtx, cancelProbe := context.WithTimeout(ctx, podExecRequestTimeout)
		reachable, err := pokePodWithContext(probeCtx, fr, srcPodName, podIP)
		cancelProbe()
		if err != nil {
			return false, fmt.Errorf("probing target pod IP %s failed: %w", podIP, err)
		}
		if reachable {
			allTrafficBlocked = false
		}
	}
	return allTrafficBlocked, nil
}

// assertACLLogsAfterProbingPodIPsWithContext regenerates blocked traffic before checking its ACL log entries.
func assertACLLogsAfterProbingPodIPsWithContext(ctx context.Context, fr *framework.Framework, clientPod *v1.Pod, targetPodIPs []string,
	policyName, expectedACLVerdict, expectedACLSeverity string) (bool, error) {
	allTrafficBlocked, err := probePodIPsWithContext(ctx, fr, clientPod.GetName(), targetPodIPs)
	if err != nil {
		return false, err
	}
	if !allTrafficBlocked {
		return false, nil
	}

	return assertACLLogsWithContext(ctx, fr, clientPod.Spec.NodeName, policyName, expectedACLVerdict, expectedACLSeverity)
}

// pokePodWithContext probes a pod and returns exec failures separately from blocked traffic.
func pokePodWithContext(ctx context.Context, fr *framework.Framework, srcPodName, dstPodIP string) (bool, error) {
	targetIP := dstPodIP
	if utilnet.IsIPv6String(dstPodIP) {
		targetIP = fmt.Sprintf("[%s]", dstPodIP)
	}
	namespace := fr.Namespace.Name
	pod, err := fr.ClientSet.CoreV1().Pods(namespace).Get(ctx, srcPodName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("getting pod %s/%s for HTTP probe: %w", namespace, srcPodName, err)
	}
	if len(pod.Spec.Containers) == 0 {
		return false, fmt.Errorf("pod %s/%s has no containers for HTTP probe", namespace, srcPodName)
	}
	stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, fr, e2epod.ExecOptions{
		Command:            []string{"/bin/sh", "-c", podHTTPProbeCommand(targetIP)},
		Namespace:          namespace,
		PodName:            srcPodName,
		ContainerName:      pod.Spec.Containers[0].Name,
		CaptureStdout:      true,
		CaptureStderr:      true,
		PreserveWhitespace: false,
	})
	if err != nil {
		framework.Logf("pod exec failed during HTTP probe; stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
		return false, fmt.Errorf("executing HTTP probe from pod %q: %w", srcPodName, err)
	}
	if stdout == "reachable" {
		return true, nil
	}
	return false, nil
}

// podHTTPProbeCommand preserves curl failures while treating its timeout as blocked traffic.
func podHTTPProbeCommand(targetIP string) string {
	return fmt.Sprintf("curl --globoff --output /dev/null -m 1 -I '%s:8000'\n"+
		"curl_status=$?\n"+
		"if [ \"$curl_status\" -eq 28 ]; then exit 0; fi\n"+
		"if [ \"$curl_status\" -ne 0 ]; then exit \"$curl_status\"; fi\n"+
		"printf 'reachable\\n'", targetIP)
}

// setBANPACLLogSeverity updates BANP with the deny and allow annotations, e.g. k8s.ovn.org/acl-logging={ "deny": "%s", "allow": "%s" }.
func setBANPACLLogSeverity(desiredDenyLogLevel, desiredAllowLogLevel string) error {
	_, err := e2ekubectl.RunKubectl("default", "annotate", "banp", "default",
		fmt.Sprintf(`%s={ "deny": "%s", "allow": "%s" }`, logSeverityAnnotation, desiredDenyLogLevel, desiredAllowLogLevel),
		"--overwrite=true")
	if err != nil {
		return fmt.Errorf("unable to annotate baseline admin network policy default: err %v", err)
	}
	return nil
}
