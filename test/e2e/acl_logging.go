package e2e

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"time"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	logSeverityNamespaceAnnotation = "k8s.ovn.org/acl-logging"
	maxPokeRetries                 = 15
	ovnControllerLogPath           = "/var/log/openvswitch/ovn-controller.log"
	pokeInterval                   = 1 * time.Second
)

var _ = Describe("ACL Logging", func() {
	const (
		denyAllPolicyName       = "default-deny-all"
		initialDenyACLSeverity  = "alert"
		initialAllowACLSeverity = "notice"
		namespacePrefix         = "acl-logging"
		pokerPodIndex           = 0
		pokedPodIndex           = 1
	)

	fr := wrappedTestFramework(namespacePrefix)

	var (
		nsName string
		pods   []v1.Pod
	)

	setNamespaceACLLogSeverity := func(namespaceToUpdate *v1.Namespace, desiredDenyLogLevel string, desiredAllowLogLevel string) error {
		if namespaceToUpdate.ObjectMeta.Annotations == nil {
			namespaceToUpdate.ObjectMeta.Annotations = map[string]string{}
		}
		By("updating the namespace's ACL logging severity")
		updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
		namespaceToUpdate.Annotations[logSeverityNamespaceAnnotation] = updatedLogSeverity

		_, err := fr.ClientSet.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
		return err
	}

	BeforeEach(func() {
		By("configuring the ACL logging level within the namespace")
		nsName = fr.Namespace.Name
		namespace, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to retrieve the namespace")
		Expect(setNamespaceACLLogSeverity(namespace, initialDenyACLSeverity, initialAllowACLSeverity)).To(Succeed())

		By("creating a \"default deny\" network policy")
		_, err = makeDenyAllPolicy(fr, nsName, denyAllPolicyName)
		Expect(err).NotTo(HaveOccurred())

		By("creating pods")
		cmd := []string{"/bin/bash", "-c", "/agnhost netexec --http-port 8000"}
		for i := 0; i < 2; i++ {
			pod := newAgnhostPod(fmt.Sprintf("pod%d", i+1), cmd...)
			pod = fr.PodClient().CreateSync(pod)
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
		Expect(
			pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)).To(HaveOccurred(),
			"traffic should be blocked since we only use a deny all traffic policy")
	})

	AfterEach(func() {
		pods = nil
	})

	It("the logs have the expected log level", func() {
		clientPodScheduledPodName := pods[pokerPodIndex].Spec.NodeName
		// Retry here in the case where OVN acls have not been programmed yet
		Eventually(func() (bool, error) {
			return assertDenyLogs(
				clientPodScheduledPodName,
				nsName,
				denyAllPolicyName,
				initialDenyACLSeverity)
		}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
	})

	When("the namespace's ACL logging annotation is updated", func() {
		const updatedAllowACLLogSeverity = "debug"

		BeforeEach(func() {
			By(fmt.Sprintf("updating the namespace's ACL logging level to %s", updatedAllowACLLogSeverity))

			namespace, err := fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to retrieve the namespace")
			Expect(setNamespaceACLLogSeverity(namespace, updatedAllowACLLogSeverity, updatedAllowACLLogSeverity)).To(Succeed())
			namespace, err = fr.ClientSet.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
		})

		BeforeEach(func() {
			By("poking some more...")
			clientPod := pods[pokerPodIndex]
			pokedPod := pods[pokedPodIndex]

			framework.Logf(
				"Poke pod %s (on node %s) from pod %s (on node %s)",
				pokedPod.GetName(),
				pokedPod.Spec.NodeName,
				clientPod.GetName(),
				clientPod.Spec.NodeName)
			Expect(
				pokePod(fr, clientPod.GetName(), pokedPod.Status.PodIP)).To(HaveOccurred(),
				"traffic should be blocked since we only use a deny all traffic policy")
		})

		It("the ACL logs are updated accordingly", func() {
			clientPodScheduledPodName := pods[pokerPodIndex].Spec.NodeName
			Eventually(func() (bool, error) {
				return assertDenyLogs(
					clientPodScheduledPodName,
					nsName,
					denyAllPolicyName,
					updatedAllowACLLogSeverity)
			}, maxPokeRetries*pokeInterval, pokeInterval).Should(BeTrue())
		})
	})
})

func makeDenyAllPolicy(f *framework.Framework, ns string, policyName string) (*knet.NetworkPolicy, error) {
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
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

func waitForACLLoggingPod(f *framework.Framework, namespace string, podName string) error {
	return e2epod.WaitForPodCondition(f.ClientSet, namespace, podName, "running", 5*time.Second, func(pod *v1.Pod) (bool, error) {
		podIP := pod.Status.PodIP
		return podIP != "" && pod.Status.Phase != v1.PodPending, nil
	})
}
