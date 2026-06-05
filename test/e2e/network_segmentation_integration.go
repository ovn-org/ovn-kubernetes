// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpclient "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = Describe("Network Segmentation: integration", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-integration")
	f.SkipNamespaceCreation = true

	var cs clientset.Interface

	BeforeEach(func() {
		cs = f.ClientSet
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		f.Namespace = namespace
		Expect(err).NotTo(HaveOccurred())
	})

	It("should recover ovnkube pods after restart with primary and secondary UDN resources", func(ctx SpecContext) {
		const (
			primaryUDNName     = "primary-udn"
			secondaryUDNName   = "secondary-udn"
			egressIPName       = "udn-egressip"
			udnPodName         = "udn-egress-pod"
			udnServiceName     = "udn-service"
			serviceTargetPort  = 80
			nodeHostnameKey    = "kubernetes.io/hostname"
			egressPodLabelKey  = "udn-egress-pod"
			egressPodLabelVal  = "enabled"
			egressNSLabelKey   = "udn-egress-namespace"
			egressNSLabelValue = "enabled"
		)
		DeferCleanup(func() {
			e2ekubectl.RunKubectlOrDie("", "delete", "eip", egressIPName, "--ignore-not-found=true")
		})

		primaryNamespace := f.Namespace.Name

		By("creating a primary UDN and waiting until it is ready")
		cleanupPrimaryUDN, err := createManifest(primaryNamespace, newPrimaryUserDefinedNetworkManifest(cs, primaryUDNName))
		Expect(err).NotTo(HaveOccurred())
		defer cleanupPrimaryUDN()
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, primaryNamespace, primaryUDNName), 30*time.Second, time.Second).Should(Succeed())

		By("creating a secondary UDN and waiting until it is ready")
		cleanupSecondaryUDN, err := createManifest(primaryNamespace, newL2SecondaryUDNManifest(secondaryUDNName))
		Expect(err).NotTo(HaveOccurred())
		defer cleanupSecondaryUDN()
		Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, primaryNamespace, secondaryUDNName), 30*time.Second, time.Second).Should(Succeed())

		By("labeling the primary namespace so it matches the EgressIP namespace selector")
		primaryNSObj, err := cs.CoreV1().Namespaces().Get(context.Background(), primaryNamespace, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if primaryNSObj.Labels == nil {
			primaryNSObj.Labels = map[string]string{}
		}
		primaryNSObj.Labels[egressNSLabelKey] = egressNSLabelValue
		_, err = cs.CoreV1().Namespaces().Update(context.Background(), primaryNSObj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("selecting one schedulable node for both pod placement and EgressIP assignment")
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes.Items).NotTo(BeEmpty())
		targetNode := nodes.Items[0].Name

		By(fmt.Sprintf("labeling node %s as egress assignable", targetNode))
		labelNodeForEgress(f, targetNode)
		DeferCleanup(func() {
			e2ekubectl.RunKubectlOrDie("default", "label", "node", targetNode, "k8s.ovn.org/egress-assignable-")
		})

		By("creating an EgressIP object selected by the primary UDN namespace and pod label")
		var egressIP string
		if isIPv4Supported(cs) {
			egressIPv4, allocErr := allocators.NewPrimaryIPv4(nodes.Items[0].Name)
			Expect(allocErr).NotTo(HaveOccurred())
			egressIP = egressIPv4.String()
		} else {
			egressIPv6, allocErr := allocators.NewPrimaryIPv6(nodes.Items[0].Name)
			Expect(allocErr).NotTo(HaveOccurred())
			egressIP = egressIPv6.String()
		}
		cleanupEIP, err := createManifest("", createEIPManifest(
			egressIPName,
			map[string]string{egressPodLabelKey: egressPodLabelVal},
			map[string]string{egressNSLabelKey: egressNSLabelValue},
			egressIP,
		))
		Expect(err).NotTo(HaveOccurred())
		defer cleanupEIP()

		By("creating a pod, service and network policy in the primary UDN namespace")
		udnPodCfg := *podConfig(
			udnPodName,
			withCommand(func() []string {
				return httpServerContainerCmd(serviceTargetPort)
			}),
			withLabels(map[string]string{egressPodLabelKey: egressPodLabelVal}),
			withNodeSelector(map[string]string{nodeHostnameKey: targetNode}),
			withNetworkAttachment([]nadapi.NetworkSelectionElement{
				{Name: secondaryUDNName},
			}),
		)
		udnPodCfg.namespace = primaryNamespace
		udnPod := runUDNPod(cs, primaryNamespace, udnPodCfg, nil)
		Expect(udnPod).NotTo(BeNil())
		var secondaryAttachmentStatus []nadapi.NetworkStatus
		Eventually(func() ([]nadapi.NetworkStatus, error) {
			udnPod, err = cs.CoreV1().Pods(primaryNamespace).Get(context.Background(), udnPod.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			secondaryAttachmentStatus, err = podNetworkStatus(udnPod, func(status nadapi.NetworkStatus) bool {
				return status.Name == namespacedName(primaryNamespace, secondaryUDNName)
			})
			return secondaryAttachmentStatus, err
		}, 30*time.Second, time.Second).Should(HaveLen(1))

		By("ensuring EgressIP is assigned to the same node as the pod")
		Expect(waitForEgressIPAssignedNode(egressIPName, targetNode)).To(Succeed())

		By("creating a multi network policy for the secondary UDN")
		mnpCli, err := mnpclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		const secondaryUDNMNPName = "secondary-udn-default-deny"
		secondaryUDNMNP := &mnpapi.MultiNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: secondaryUDNMNPName,
				Annotations: map[string]string{
					PolicyForAnnotation: secondaryUDNName,
				},
			},
			Spec: mnpapi.MultiNetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{egressPodLabelKey: egressPodLabelVal},
				},
				PolicyTypes: []mnpapi.MultiPolicyType{
					mnpapi.PolicyTypeIngress,
					mnpapi.PolicyTypeEgress,
				},
			},
		}
		_, err = mnpCli.MultiNetworkPolicies(primaryNamespace).Create(context.Background(), secondaryUDNMNP, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = mnpCli.MultiNetworkPolicies(primaryNamespace).Delete(context.Background(), secondaryUDNMNPName, metav1.DeleteOptions{})
		})

		_, err = cs.CoreV1().Services(primaryNamespace).Create(context.Background(), &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: udnServiceName,
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{egressPodLabelKey: egressPodLabelVal},
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Port:       serviceTargetPort,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(serviceTargetPort),
					},
				},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = makeDenyAllPolicy(f, primaryNamespace, "deny-all")
		Expect(err).NotTo(HaveOccurred())

		By("restarting each ovnkube pod and ensuring all pods recover without crash loops")
		Expect(restartAllOVNKubePodsAndAssertHealthy(f)).To(Succeed())
	})
})

func restartAllOVNKubePodsAndAssertHealthy(f *framework.Framework) error {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list ovnkube pods in namespace %s: %w", ovnNamespace, err)
	}

	restartedPods := 0
	for i := range pods.Items {
		pod := pods.Items[i]
		if !strings.HasPrefix(pod.Name, "ovnkube-") || pod.Status.Phase != v1.PodRunning {
			continue
		}
		restartedPods++
		framework.Logf("restarting ovnkube pod %s/%s", pod.Namespace, pod.Name)
		if err := deletePodWithWait(context.Background(), f.ClientSet, &pod); err != nil {
			return fmt.Errorf("failed restarting ovnkube pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	if restartedPods == 0 {
		return fmt.Errorf("no running ovnkube pods found in namespace %s", ovnNamespace)
	}

	if err := waitOVNKubernetesHealthy(f); err != nil {
		return fmt.Errorf("ovn-kubernetes did not become healthy after restarting %d pods: %w", restartedPods, err)
	}

	return wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		if err := assertOVNKubePodsReadyAndNotCrashLooping(f.ClientSet, ovnNamespace); err != nil {
			framework.Logf("ovnkube pod readiness/crashloop check still failing: %v", err)
			return false, nil
		}
		return true, nil
	})
}

func assertOVNKubePodsReadyAndNotCrashLooping(cs clientset.Interface, namespace string) error {
	pods, err := cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed listing ovnkube pods: %w", err)
	}

	found := 0
	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Name, "ovnkube-") {
			continue
		}
		found++
		if pod.Status.Phase != v1.PodRunning {
			return fmt.Errorf("pod %s is not running (phase=%s)", pod.Name, pod.Status.Phase)
		}

		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == v1.PodReady && condition.Status == v1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("pod %s is not ready", pod.Name)
		}

		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
				return fmt.Errorf("pod %s container %s is in CrashLoopBackOff", pod.Name, status.Name)
			}
		}
	}

	if found == 0 {
		return fmt.Errorf("no ovnkube pods found in namespace %s", namespace)
	}
	return nil
}

func waitForEgressIPAssignedNode(egressIPName, nodeName string) error {
	return wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		egressIPStdout, err := e2ekubectl.RunKubectl("", "get", "eip", egressIPName, "-o", "json")
		if err != nil {
			framework.Logf("failed to fetch EgressIP %s status: %v", egressIPName, err)
			return false, nil
		}

		var eip egressIP
		if err := json.Unmarshal([]byte(egressIPStdout), &eip); err != nil {
			return false, fmt.Errorf("failed to unmarshal EgressIP %s status: %w", egressIPName, err)
		}

		if len(eip.Status.Items) == 0 {
			framework.Logf("EgressIP %s has no status items yet", egressIPName)
			return false, nil
		}

		for _, status := range eip.Status.Items {
			if status.Node == nodeName {
				return true, nil
			}
		}
		framework.Logf("EgressIP %s not assigned to node %s yet (statuses: %+v)", egressIPName, nodeName, eip.Status.Items)
		return false, nil
	})
}
