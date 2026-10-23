// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestUplinkControllerReportsMissingSelectedNodeState(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "red"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
	)

	stateName := uplinkutil.StateName("br-blue", "node-a")
	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		stateName,
		metav1.GetOptions{},
	)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

	_, err = client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName("br-blue", "node-b"),
		metav1.GetOptions{},
	)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

	cond := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonMissingUplinkState),
		gomega.HaveField("Message", gomega.ContainSubstring("1 of 1 selected node(s)")),
	))
}

func TestUplinkControllerAggregatesReadyState(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
	)
	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonReady),
	))
}

func TestUplinkControllerKeepsUplinkReadyForGatewayProgrammingFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newGatewayFailedUplinkState(
			"br-blue",
			"node-a",
			"br-blue",
			uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
		),
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonReady),
	))
}

func TestUplinkControllerReportsBoundedPartialFailureSummary(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "blue"}),
		newNode("node-c", map[string]string{"role": "blue"}),
		newNode("node-d", map[string]string{"role": "blue"}),
		newNode("node-e", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newUnresolvedUplinkState("br-blue", "node-a", "br-blue",
			uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound),
		newUnresolvedUplinkState("br-blue", "node-b", "br-blue",
			uplinkv1alpha1.UplinkStateReasonBridgeNotFound),
		newDPUHostBlockedUplinkState("br-blue", "node-c", "br-blue",
			uplinkv1alpha1.UplinkStateReasonInvalidHostInterface),
		newUnresolvedUplinkState("br-blue", "node-d", "br-blue",
			uplinkv1alpha1.UplinkStateReasonGatewayInfoUnavailable),
		newResolvedUplinkState("br-blue", "node-e", "br-blue"),
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkStateNotResolved),
		gomega.HaveField("Message", gomega.ContainSubstring("4 of 5 selected node(s)")),
		gomega.HaveField("Message", gomega.ContainSubstring("node-a=HostInterfaceNotFound")),
		gomega.HaveField("Message", gomega.ContainSubstring("node-b=BridgeNotFound")),
		gomega.HaveField("Message", gomega.ContainSubstring("node-c=InvalidHostInterface")),
		gomega.HaveField("Message", gomega.Not(gomega.ContainSubstring("node-d="))),
	))
}

func TestUplinkControllerSummarizesSelectorAndStateFailures(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "br-blue")
	uplink.Spec.NodeConfigs = append(uplink.Spec.NodeConfigs,
		newUplinkNodeConfig("group", "overlap", "br-other"))
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "blue", "group": "overlap"}),
		uplink,
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonNodeSelectorOverlap),
		gomega.HaveField("Message", gomega.ContainSubstring("2 of 2 selected node(s)")),
		gomega.HaveField("Message", gomega.ContainSubstring("node-a=MissingUplinkState")),
		gomega.HaveField("Message", gomega.ContainSubstring("node-b=NodeSelectorOverlap")),
	))
}

func TestUplinkControllerReportsUplinkStateSpecIdentityError(t *testing.T) {
	g := gomega.NewWithT(t)
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	state.Spec.UplinkName = "br-red"
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		state,
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkStateIdentityError),
		gomega.HaveField("Message", gomega.ContainSubstring("uplinkName=\"br-red\"")),
	))
}

func TestUplinkControllerClearsObsoleteConditions(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "br-blue")
	uplink.Status.Conditions = []metav1.Condition{
		{
			Type:    obsoleteUplinkConditionDegraded,
			Status:  metav1.ConditionFalse,
			Reason:  reasonReady,
			Message: "all selected nodes are ready",
		},
		{
			Type:    obsoleteUplinkConditionReferenced,
			Status:  metav1.ConditionTrue,
			Reason:  obsoleteUplinkConditionReferenced,
			Message: "obsolete condition",
		},
	}
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		uplink,
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	updated, err := client.UplinkClient.K8sV1alpha1().Uplinks().Get(
		context.Background(),
		"br-blue",
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.FindStatusCondition(
		updated.Status.Conditions,
		obsoleteUplinkConditionReferenced,
	)).To(gomega.BeNil())
	g.Expect(meta.FindStatusCondition(
		updated.Status.Conditions,
		obsoleteUplinkConditionDegraded,
	)).To(gomega.BeNil())
	g.Expect(meta.FindStatusCondition(
		updated.Status.Conditions,
		uplinkv1alpha1.UplinkConditionReady,
	)).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonReady),
	))
}

func TestUplinkControllerReportsOverlappingSelectors(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue", "zone": "a"}),
		&uplinkv1alpha1.Uplink{
			ObjectMeta: metav1.ObjectMeta{Name: "br-blue"},
			Spec: uplinkv1alpha1.UplinkSpec{
				NodeConfigs: []uplinkv1alpha1.UplinkNodeConfig{
					newUplinkNodeConfig("role", "blue", "br-blue"),
					newUplinkNodeConfig("zone", "a", "br-alt"),
				},
			},
		},
	)
	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	cond := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonNodeSelectorOverlap),
		gomega.HaveField("Message", gomega.ContainSubstring("1 of 1 selected node(s)")),
	))

	states, err := client.UplinkClient.K8sV1alpha1().UplinkStates().List(
		context.Background(),
		metav1.ListOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(states.Items).To(gomega.BeEmpty())
}

func TestUplinkControllerReportsInvalidSpec(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "br-blue")
	// A LabelSelector can pass CRD schema validation yet fail to parse:
	// Exists takes no values (LabelSelectorOpIn would be the right operator
	// for matching against values).
	uplink.Spec.NodeConfigs[0].NodeSelector = metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "role",
			Operator: metav1.LabelSelectorOpExists,
			Values:   []string{"blue"},
		}},
	}
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		uplink,
	)

	// A rejected spec is reported but not retried: the cluster admin must
	// fix the Uplink CR, and that update triggers a new reconcile.
	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	cond := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonInvalidSpec),
	))
}

func TestUplinkControllerEnsuresFinalizerFromCUDNReference(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newCUDN("blue", "br-blue"),
	)
	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	finalizers := getUplinkFinalizers(g, client, "br-blue")
	g.Expect(finalizers).To(gomega.ContainElement(finalizerUplink))
}

func TestUplinkControllerInitialSyncRemovesUnreferencedFinalizer(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		withFinalizer(newUplink("br-blue", "role", "blue", "br-blue")),
	)
	g.Expect(controller.initialSync()).To(gomega.Succeed())

	finalizers := getUplinkFinalizers(g, client, "br-blue")
	g.Expect(finalizers).NotTo(gomega.ContainElement(finalizerUplink))
}

func TestUplinkControllerInitialSyncRebuildsCUDNUplinkReferences(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, _ := newTestController(t,
		newCUDN("blue", "br-blue"),
		newCUDN("red", "br-blue"),
	)
	controller.cudnsByUplink = map[string]sets.Set[string]{
		"br-stale": sets.New("stale"),
	}

	g.Expect(controller.initialSync()).To(gomega.Succeed())

	g.Expect(controller.getCUDNsReferencingUplink("br-blue")).To(gomega.Equal([]string{"blue", "red"}))
	g.Expect(controller.getCUDNsReferencingUplink("br-stale")).To(gomega.BeEmpty())
}

func TestUplinkControllerRemovesFinalizerWhenCUDNIsDeleted(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		withFinalizer(newUplink("br-blue", "role", "blue", "br-blue")),
	)
	controller.setCUDNUplinkReferences("blue", []string{"br-blue"})

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	finalizers := getUplinkFinalizers(g, client, "br-blue")
	g.Expect(finalizers).NotTo(gomega.ContainElement(finalizerUplink))
}

func TestUplinkControllerKeepsFinalizerWhenReferencedCUDNIsDeleted(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		withFinalizer(newUplink("br-blue", "role", "blue", "br-blue")),
		newCUDN("red", "br-blue"),
	)
	controller.setCUDNUplinkReferences("blue", []string{"br-blue"})

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	finalizers := getUplinkFinalizers(g, client, "br-blue")
	g.Expect(finalizers).To(gomega.ContainElement(finalizerUplink))
	g.Expect(controller.getCUDNsReferencingUplink("br-blue")).To(gomega.Equal([]string{"red"}))
}

func TestUplinkControllerUpdatesFinalizersWhenCUDNIsRecreated(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		withFinalizer(newUplink("br-blue", "role", "blue", "br-blue")),
		newUplink("br-red", "role", "red", "br-red"),
		newCUDN("blue", "br-red"),
	)
	controller.setCUDNUplinkReferences("blue", []string{"br-blue"})

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	g.Expect(getUplinkFinalizers(g, client, "br-blue")).NotTo(gomega.ContainElement(finalizerUplink))
	g.Expect(getUplinkFinalizers(g, client, "br-red")).To(gomega.ContainElement(finalizerUplink))
	g.Expect(controller.getCUDNsReferencingUplink("br-blue")).To(gomega.BeEmpty())
	g.Expect(controller.getCUDNsReferencingUplink("br-red")).To(gomega.Equal([]string{"blue"}))
}

func TestUplinkControllerBlocksDeleteWhileReferenced(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newCUDN("blue", "br-blue"),
		deletingUplink("br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
	)
	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	finalizers := getUplinkFinalizers(g, client, "br-blue")
	g.Expect(finalizers).To(gomega.ContainElement(finalizerUplink))
	ready := getUplinkCondition(g, client, "br-blue", uplinkv1alpha1.UplinkConditionReady)
	g.Expect(ready).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkTerminating),
	))

	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName("br-blue", "node-a"),
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestUplinkControllerRequeuesReferencedCUDNOnUplinkReconcile(t *testing.T) {
	g := gomega.NewWithT(t)
	reconciledCUDNs := make(chan string, 1)
	controller, _ := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newCUDN("blue", "br-blue"),
	)
	controller.cudnController = controllerutil.NewController(
		"test-cudn-requeue",
		&controllerutil.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				reconciledCUDNs <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.cudnController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.cudnController)
	})

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	g.Eventually(reconciledCUDNs).Should(gomega.Receive(gomega.Equal("blue")))
}

func TestUplinkControllerRequeuesReferencedCUDNOnTerminatingUplink(t *testing.T) {
	g := gomega.NewWithT(t)
	reconciledCUDNs := make(chan string, 1)
	controller, _ := newTestController(t,
		deletingUplink("br-blue"),
		newCUDN("blue", "br-blue"),
	)
	controller.cudnController = controllerutil.NewController(
		"test-cudn-terminating-requeue",
		&controllerutil.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(key string) error {
				reconciledCUDNs <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.cudnController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.cudnController)
	})

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	g.Eventually(reconciledCUDNs).Should(gomega.Receive(gomega.Equal("blue")))
}

func TestUplinkControllerInitialSyncDeletesUnreferencedTerminatingUplink(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		deletingUplink("br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
	)
	g.Expect(controller.initialSync()).To(gomega.Succeed())

	_, err := client.UplinkClient.K8sV1alpha1().Uplinks().Get(
		context.Background(),
		"br-blue",
		metav1.GetOptions{},
	)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())

	_, err = client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName("br-blue", "node-a"),
		metav1.GetOptions{},
	)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

func TestUplinkControllerInitialSyncDeletesStaleUplinkStates(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "red"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newResolvedUplinkState("br-missing", "node-a", "br-missing"),
		newResolvedUplinkState("br-blue", "node-b", "br-blue"),
		newResolvedUplinkState("br-blue", "node-missing", "br-blue"),
	)

	g.Expect(controller.initialSync()).To(gomega.Succeed())

	expectUplinkStateExists(g, client, "br-blue", "node-a")
	expectUplinkStateNotFound(g, client, "br-missing", "node-a")
	expectUplinkStateNotFound(g, client, "br-blue", "node-b")
	expectUplinkStateNotFound(g, client, "br-blue", "node-missing")
}

func TestUplinkControllerTargetsUplinkFromDeletedUplinkStateIdentity(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, _ := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newUplink("br-red", "role", "red", "br-red"),
		newCUDN("blue", "br-blue"),
	)
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	controller.rememberUplinkStateIdentity(state)

	reconciledUplinks := make(chan string, 2)
	controller.uplinkController = controllerutil.NewController(
		"test-uplink-delete-requeue",
		&controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.uplinkLister.List,
			Reconcile: func(key string) error {
				reconciledUplinks <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.uplinkController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.uplinkController)
	})

	g.Expect(controller.reconcileUplinkState(state.Name)).To(gomega.Succeed())

	g.Eventually(reconciledUplinks, time.Second).Should(
		gomega.Receive(gomega.Equal("br-blue")),
	)
	g.Consistently(reconciledUplinks, 100*time.Millisecond).ShouldNot(
		gomega.Receive(gomega.Equal("br-red")),
	)
}

func TestUplinkControllerTargetsUplinkFromDeletedUplinkStateName(t *testing.T) {
	g := gomega.NewWithT(t)
	longUplinkName := "up-" + strings.Repeat("a", 60) + ".up-" + strings.Repeat("b", 60)
	longNodeName := "nd-" + strings.Repeat("c", 60) + ".nd-" + strings.Repeat("d", 60)
	controller, _ := newTestController(t,
		newNode(longNodeName, map[string]string{"role": "blue"}),
		newUplink(longUplinkName, "role", "blue", "br-blue"),
		newUplink("br-red", "role", "red", "br-red"),
	)

	reconciledUplinks := make(chan string, 2)
	controller.uplinkController = controllerutil.NewController(
		"test-uplink-delete-requeue-from-name",
		&controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.uplinkLister.List,
			Reconcile: func(key string) error {
				reconciledUplinks <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.uplinkController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.uplinkController)
	})

	g.Expect(controller.reconcileUplinkState(uplinkutil.StateName(longUplinkName, longNodeName))).To(gomega.Succeed())

	g.Eventually(reconciledUplinks, time.Second).Should(
		gomega.Receive(gomega.Equal(longUplinkName)),
	)
	g.Consistently(reconciledUplinks, 100*time.Millisecond).ShouldNot(
		gomega.Receive(gomega.Equal("br-red")),
	)
}

func TestUplinkControllerReconcilesCUDNFromNetworkRef(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, _ := newTestController(t, newCUDN("blue", "br-blue"))

	reconciledCUDNs := make(chan string, 1)
	controller.cudnController = controllerutil.NewController(
		"test-uplink-network-ref-cudn",
		&controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.cudnLister.List,
			Reconcile: func(key string) error {
				reconciledCUDNs <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.cudnController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.cudnController)
	})

	g.Expect(controller.reconcileNetworkRef(networkRefKey("node-a", util.GenerateUDNNetworkName("ns", "blue")))).To(
		gomega.Succeed())
	g.Consistently(reconciledCUDNs, 100*time.Millisecond).ShouldNot(gomega.Receive())

	g.Expect(controller.reconcileNetworkRef(networkRefKey("node-a", util.GenerateCUDNNetworkName("missing")))).To(
		gomega.Succeed())
	g.Eventually(reconciledCUDNs, time.Second).Should(gomega.Receive(gomega.Equal("missing")))

	g.Expect(controller.reconcileNetworkRef(networkRefKey("node-a", util.GenerateCUDNNetworkName("blue")))).To(
		gomega.Succeed())
	g.Eventually(reconciledCUDNs, time.Second).Should(gomega.Receive(gomega.Equal("blue")))
}

func TestUplinkControllerDeletesStatesForMissingUplink(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newResolvedUplinkState("br-red", "node-a", "br-red"),
	)

	g.Expect(controller.reconcileUplink("br-blue")).To(gomega.Succeed())

	expectUplinkStateNotFound(g, client, "br-blue", "node-a")
	expectUplinkStateExists(g, client, "br-red", "node-a")
}

// TestUplinkControllerDeletesStatesForDeletedNode checks that reconciling a
// deleted node requeues every Uplink and deletes the node's UplinkStates only.
func TestUplinkControllerDeletesStatesForDeletedNode(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newUplink("br-red", "role", "blue", "br-red"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newResolvedUplinkState("br-blue", "node-gone", "br-blue"),
		newResolvedUplinkState("br-red", "node-gone", "br-red"),
	)

	reconciledUplinks := make(chan string, 4)
	controller.uplinkController = controllerutil.NewController(
		"test-uplink-node-delete",
		&controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.uplinkLister.List,
			Reconcile: func(key string) error {
				reconciledUplinks <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.uplinkController)).To(gomega.Succeed(), "failed to start the Uplink controller")
	t.Cleanup(func() {
		controllerutil.Stop(controller.uplinkController)
	})

	// ReconcileAll enqueues the Uplinks in no particular order.
	expectAllUplinksReconciled := func() {
		var keys []string
		for range 2 {
			var key string
			g.Eventually(reconciledUplinks, time.Second).Should(gomega.Receive(&key), "fewer Uplinks reconciled than exist")
			keys = append(keys, key)
		}
		g.Expect(keys).To(gomega.ConsistOf("br-blue", "br-red"), "not every Uplink was reconciled once")
		g.Consistently(reconciledUplinks, 100*time.Millisecond).ShouldNot(gomega.Receive(), "an Uplink was reconciled more than once")
	}

	// An existing node keeps every UplinkState.
	g.Expect(controller.reconcileNode("node-a")).To(gomega.Succeed(), "reconciling existing node node-a failed")
	expectUplinkStateExists(g, client, "br-blue", "node-a")
	expectUplinkStateExists(g, client, "br-blue", "node-gone")
	expectUplinkStateExists(g, client, "br-red", "node-gone")
	expectAllUplinksReconciled()

	// A deleted node loses its UplinkStates across all Uplinks, and the
	// Uplinks are reconciled so their readiness reflects the new node set.
	g.Expect(controller.reconcileNode("node-gone")).To(gomega.Succeed(), "reconciling deleted node node-gone failed")
	expectUplinkStateExists(g, client, "br-blue", "node-a")
	expectUplinkStateNotFound(g, client, "br-blue", "node-gone")
	expectUplinkStateNotFound(g, client, "br-red", "node-gone")
	expectAllUplinksReconciled()
}

// TestUplinkControllerDeletesUplinkStateOfMissingNode checks that an
// UplinkState whose node does not exist is deleted when it is reconciled.
func TestUplinkControllerDeletesUplinkStateOfMissingNode(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newResolvedUplinkState("br-blue", "node-gone", "br-blue"),
	)

	g.Expect(controller.reconcileUplinkState(uplinkutil.StateName("br-blue", "node-a"))).To(gomega.Succeed(), "reconciling the UplinkState of existing node node-a failed")
	expectUplinkStateExists(g, client, "br-blue", "node-a")

	g.Expect(controller.reconcileUplinkState(uplinkutil.StateName("br-blue", "node-gone"))).To(gomega.Succeed(), "reconciling the UplinkState of missing node node-gone failed")
	expectUplinkStateNotFound(g, client, "br-blue", "node-gone")
	expectUplinkStateExists(g, client, "br-blue", "node-a")
}

func TestUplinkControllerReconcilesUplinksForUnknownDeletedUplinkState(t *testing.T) {
	g := gomega.NewWithT(t)
	controller, _ := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newCUDN("blue", "br-blue"),
	)

	reconciledUplinks := make(chan string, 1)
	controller.uplinkController = controllerutil.NewController(
		"test-uplink-delete-requeue-all-uplinks",
		&controllerutil.ControllerConfig[uplinkv1alpha1.Uplink]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.uplinkLister.List,
			Reconcile: func(key string) error {
				reconciledUplinks <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	reconciledCUDNs := make(chan string, 1)
	controller.cudnController = controllerutil.NewController(
		"test-uplink-delete-requeue-no-cudns",
		&controllerutil.ControllerConfig[udnv1.ClusterUserDefinedNetwork]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Lister:      controller.cudnLister.List,
			Reconcile: func(key string) error {
				reconciledCUDNs <- key
				return nil
			},
			Threadiness: 1,
		},
	)
	g.Expect(controllerutil.Start(controller.uplinkController, controller.cudnController)).To(gomega.Succeed())
	t.Cleanup(func() {
		controllerutil.Stop(controller.uplinkController, controller.cudnController)
	})

	g.Expect(controller.reconcileUplinkState("unknown-state")).To(gomega.Succeed())

	g.Eventually(reconciledUplinks, time.Second).Should(
		gomega.Receive(gomega.Equal("br-blue")),
	)
	g.Consistently(reconciledCUDNs, 100*time.Millisecond).ShouldNot(
		gomega.Receive(),
	)
}

func TestUplinkControllerSetsCUDNUplinksReadyWhenNoUplinkIsConfigured(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t, newCUDNWithoutUplink("blue"))

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonUplinksReady),
	))
}

func TestUplinkControllerSetsCUDNUplinksReadyForActiveNodes(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonUplinksReady),
	))
}

func TestUplinkControllerRestoresCUDNReadinessAfterCacheLag(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	cudn := newCUDN("blue", "br-blue")
	state := newResolvedUplinkState("br-blue", "node-a", "br-new")
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		withFinalizer(newUplink("br-blue", "role", "blue", "br-new")),
		state,
		cudn,
	)
	getCUDN := func() *udnv1.ClusterUserDefinedNetwork {
		current, err := client.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
			context.Background(), cudn.Name, metav1.GetOptions{},
		)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return current
	}

	// Control the two informer caches independently to reproduce the event
	// ordering without sleeps or concurrent mutation of cached objects.
	cudnIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	stateIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(cudnIndexer.Add(cudn)).To(gomega.Succeed())
	g.Expect(stateIndexer.Add(state)).To(gomega.Succeed())
	controller.cudnLister = udnlisters.NewClusterUserDefinedNetworkLister(cudnIndexer)
	controller.uplinkStateLister = uplinklisters.NewUplinkStateLister(stateIndexer)
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	readyCUDN := getCUDN()
	g.Expect(meta.FindStatusCondition(readyCUDN.Status.Conditions, conditionTypeUplinksReady).Status).
		To(gomega.Equal(metav1.ConditionTrue))
	g.Expect(cudnIndexer.Update(readyCUDN)).To(gomega.Succeed())

	// A changed Uplink spec can be observed before its resolved state. The
	// resulting False write has not yet reached the CUDN informer cache.
	oldState := state.DeepCopy()
	oldState.Status.HostInterfaceName = "br-old"
	g.Expect(stateIndexer.Update(oldState)).To(gomega.Succeed())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	g.Expect(meta.FindStatusCondition(getCUDN().Status.Conditions, conditionTypeUplinksReady)).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkNotResolvedForNode),
	))

	// Resolution catches up first. The cached True matches the desired
	// condition, so reconciliation skips restoring True in the API.
	g.Expect(stateIndexer.Update(state)).To(gomega.Succeed())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	notReadyCUDN := getCUDN()
	g.Expect(meta.FindStatusCondition(notReadyCUDN.Status.Conditions, conditionTypeUplinksReady).Status).
		To(gomega.Equal(metav1.ConditionFalse))

	// Observing the delayed status write must enqueue another reconcile.
	g.Expect(cudnIndexer.Update(notReadyCUDN)).To(gomega.Succeed())
	g.Expect(cudnNeedsUpdate(readyCUDN, notReadyCUDN)).To(gomega.BeTrue())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	recoveredCUDN := getCUDN()
	g.Expect(meta.FindStatusCondition(recoveredCUDN.Status.Conditions, conditionTypeUplinksReady)).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonUplinksReady),
	))

	// Readiness stays stable once the recovery write reaches the cache.
	g.Expect(cudnIndexer.Update(recoveredCUDN)).To(gomega.Succeed())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	g.Expect(getCUDN()).To(gomega.Equal(recoveredCUDN))
}

func TestUplinkControllerReportsCUDNUplinkNotReadyForActiveNode(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkNotResolvedForNode),
	))
}

func TestUplinkControllerReportsCUDNUplinkTerminating(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		deletingUplink("br-blue"),
		newResolvedUplinkState("br-blue", "node-a", "br-blue"),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkTerminating),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue=UplinkTerminating")),
	))
}

func TestUplinkControllerRequiresHostGatewayReadyInSplitDPUMode(t *testing.T) {
	// A DPU-host-owned HostDataReady condition identifies a split-DPU
	// UplinkState: host-side gateway programming is then reported separately
	// through HostGatewayReady and is required for CUDN uplink readiness.
	cases := []struct {
		name            string
		extraConditions []metav1.Condition
		expectedStatus  metav1.ConditionStatus
		expectedReason  string
	}{
		{
			name: "not ready while HostGatewayReady is missing",
			extraConditions: []metav1.Condition{{
				Type:   uplinkv1alpha1.UplinkStateConditionHostDataReady,
				Status: metav1.ConditionTrue,
				Reason: uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
			}},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonUplinksNotReady,
		},
		{
			name: "not ready while no host-side CUDN has reported programming",
			extraConditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostDataReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
				},
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonNoActiveCUDNs,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonNoActiveCUDNs,
		},
		{
			name: "not ready on host-side VRF attachment failure",
			extraConditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostDataReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
				},
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
					Status: metav1.ConditionFalse,
					Reason: uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
				},
			},
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonUplinkVRFAttachmentFailed,
		},
		{
			name: "ready once both sides report gateway programming",
			extraConditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostDataReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonHostDataDiscovered,
				},
				{
					Type:   uplinkv1alpha1.UplinkStateConditionHostGatewayReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
				},
			},
			expectedStatus: metav1.ConditionTrue,
			expectedReason: reasonUplinksReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			setSharedGatewayMode(t)
			state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
			state.Status.Conditions = append(state.Status.Conditions, tc.extraConditions...)
			controller, client := newTestController(t,
				newNode("node-a", map[string]string{"role": "blue"}),
				newUplink("br-blue", "role", "blue", "br-blue"),
				state,
				newCUDN("blue", "br-blue"),
			)

			g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

			cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
			g.Expect(cond).To(gomega.And(
				gomega.HaveField("Status", tc.expectedStatus),
				gomega.HaveField("Reason", tc.expectedReason),
			))
		})
	}
}

func TestUplinkControllerPropagatesCUDNUplinkStateGatewayFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newGatewayFailedUplinkState(
			"br-blue",
			"node-a",
			"br-blue",
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
		),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkBridgeMappingFailed),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinkBridgeMappingFailed")),
	))
}

func TestUplinkControllerReportsCUDNGatewayProgrammingPending(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	state.Status.Conditions = state.Status.Conditions[:1]
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		state,
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinksNotReady),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinksNotReady")),
	))
}

func TestUplinkControllerWaitsForFirstCUDNGatewayResult(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	state.Status.Conditions[1].Reason = uplinkv1alpha1.UplinkStateReasonNoActiveCUDNs
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		state,
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonNoActiveCUDNs),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=NoActiveCUDNs")),
	))
}

func TestUplinkControllerPropagatesCUDNUplinkGatewayProgrammingFailure(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newGatewayFailedUplinkState(
			"br-blue",
			"node-a",
			"br-blue",
			uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
		),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkGatewayProgrammingFailed),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinkGatewayProgrammingFailed")),
	))
}

func TestUplinkControllerPropagatesCUDNUplinkConfigurationConflict(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newGatewayFailedUplinkState(
			"br-blue",
			"node-a",
			"br-blue",
			uplinkv1alpha1.UplinkStateReasonConfigurationConflict,
		),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkConfigurationConflict),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinkConfigurationConflict")),
	))
}

func TestUplinkControllerSummarizesMixedCUDNUplinkFailures(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	cudn := newCUDN("blue", "br-blue")
	cudn.Spec.Uplinks = append(cudn.Spec.Uplinks, "br-red")
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newUplink("br-red", "role", "blue", "br-red"),
		newGatewayFailedUplinkState(
			"br-blue",
			"node-a",
			"br-blue",
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
		),
		cudn,
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinksNotReady),
		gomega.HaveField("Message", gomega.ContainSubstring("2 active node/uplink readiness check(s) failed")),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinkBridgeMappingFailed")),
		gomega.HaveField("Message", gomega.ContainSubstring("br-red/node-a=UplinkNotResolvedForNode")),
	))
}

func TestUplinkControllerBoundsCUDNUplinksReadyMessage(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newCUDN("blue", "br-blue"),
	)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkNotResolvedForNode),
		gomega.HaveField("Message", gomega.ContainSubstring("2 active node/uplink readiness check(s) failed")),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-a=UplinkNotResolvedForNode")),
		gomega.HaveField("Message", gomega.ContainSubstring("br-blue/node-b=UplinkNotResolvedForNode")),
	))
}

func TestUplinkControllerIgnoresInactiveNodesForCUDNUplinksReady(t *testing.T) {
	g := gomega.NewWithT(t)
	setSharedGatewayMode(t)
	oldDynamicUDN := config.OVNKubernetesFeature.EnableDynamicUDNAllocation
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	t.Cleanup(func() {
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = oldDynamicUDN
	})

	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		newNode("node-b", map[string]string{"role": "blue"}),
		newUplink("br-blue", "role", "blue", "br-blue"),
		newResolvedUplinkState("br-blue", "node-b", "br-blue"),
		newCUDN("blue", "br-blue"),
	)
	fakeNetworkManager := controller.networkManager.(*networkmanager.FakeNetworkManager)
	networkName := util.GenerateCUDNNetworkName("blue")
	fakeNetworkManager.SetNodeActive(networkName, "node-a", false)
	fakeNetworkManager.SetNodeActive(networkName, "node-b", true)

	g.Expect(controller.reconcileCUDN("blue")).To(gomega.Succeed())

	cond := getCUDNCondition(g, client, "blue", conditionTypeUplinksReady)
	g.Expect(cond).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionTrue),
		gomega.HaveField("Reason", reasonUplinksReady),
	))
}

func newTestController(t *testing.T, objects ...runtime.Object) (*Controller, *util.OVNClusterManagerClientset) {
	t.Helper()

	g := gomega.NewWithT(t)

	client := util.GetOVNClientset(objects...).GetClusterManagerClientset()
	ovntest.AddUplinkApplyReactor(client.UplinkClient.(*uplinkfake.Clientset))

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	uplinkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	uplinkStateIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	cudnIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *corev1.Node:
			g.Expect(nodeIndexer.Add(typed)).To(gomega.Succeed())
		case *uplinkv1alpha1.Uplink:
			g.Expect(uplinkIndexer.Add(typed)).To(gomega.Succeed())
		case *uplinkv1alpha1.UplinkState:
			g.Expect(uplinkStateIndexer.Add(typed)).To(gomega.Succeed())
		case *udnv1.ClusterUserDefinedNetwork:
			g.Expect(cudnIndexer.Add(typed)).To(gomega.Succeed())
		}
	}

	controller := &Controller{
		uplinkClient:      client.UplinkClient,
		udnClient:         client.UserDefinedNetworkClient,
		uplinkLister:      uplinklisters.NewUplinkLister(uplinkIndexer),
		uplinkStateLister: uplinklisters.NewUplinkStateLister(uplinkStateIndexer),
		cudnLister:        udnlisters.NewClusterUserDefinedNetworkLister(cudnIndexer),
		nodeLister:        corelisters.NewNodeLister(nodeIndexer),
		networkManager:    &networkmanager.FakeNetworkManager{},
		uplinkController:  newNoopController("test-uplink"),
		cudnController:    newNoopController("test-cudn"),
		cudnsByUplink:     make(map[string]sets.Set[string]),
		uplinkByStateName: make(map[string]string),
	}
	g.Expect(controller.rebuildCUDNUplinkReferences()).To(gomega.Succeed())
	return controller, client
}

func newNoopController(name string) controllerutil.Controller {
	return controllerutil.NewController(
		name,
		&controllerutil.ControllerConfig[string]{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile: func(_ string) error {
				return nil
			},
		},
	)
}

func setSharedGatewayMode(t *testing.T) {
	t.Helper()
	oldMode := config.Gateway.Mode
	config.Gateway.Mode = config.GatewayModeShared
	t.Cleanup(func() {
		config.Gateway.Mode = oldMode
	})
}

func newNode(name string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: nodeLabels,
		},
	}
}

func newUplink(name string, selectorKey string, selectorValue string, hostInterfaceName string) *uplinkv1alpha1.Uplink {
	return &uplinkv1alpha1.Uplink{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: uplinkv1alpha1.UplinkSpec{
			NodeConfigs: []uplinkv1alpha1.UplinkNodeConfig{
				newUplinkNodeConfig(selectorKey, selectorValue, hostInterfaceName),
			},
		},
	}
}

func withFinalizer(uplink *uplinkv1alpha1.Uplink) *uplinkv1alpha1.Uplink {
	uplink.Finalizers = append(uplink.Finalizers, finalizerUplink)
	return uplink
}

func deletingUplink(name string) *uplinkv1alpha1.Uplink {
	now := metav1.Now()
	uplink := newUplink(name, "role", "blue", "br-blue")
	uplink.Finalizers = []string{finalizerUplink}
	uplink.DeletionTimestamp = &now
	return uplink
}

func newCUDN(name, uplinkName string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			Uplinks: []string{uplinkName},
		},
	}
}

func newCUDNWithoutUplink(name string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func newUplinkNodeConfig(
	selectorKey string,
	selectorValue string,
	hostInterfaceName string,
) uplinkv1alpha1.UplinkNodeConfig {
	return uplinkv1alpha1.UplinkNodeConfig{
		Type: uplinkv1alpha1.UplinkTypeOVSBridge,
		NodeSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{selectorKey: selectorValue},
		},
		HostInterfaceName: uplinkv1alpha1.InterfaceName(hostInterfaceName),
	}
}

func newResolvedUplinkState(uplinkName string, nodeName string, hostInterfaceName string) *uplinkv1alpha1.UplinkState {
	return &uplinkv1alpha1.UplinkState{
		ObjectMeta: metav1.ObjectMeta{
			Name: uplinkutil.StateName(uplinkName, nodeName),
		},
		Spec: uplinkv1alpha1.UplinkStateSpec{
			UplinkName: uplinkName,
			NodeName:   nodeName,
		},
		Status: uplinkv1alpha1.UplinkStateStatus{
			Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
			HostInterfaceName: uplinkv1alpha1.InterfaceName(hostInterfaceName),
			Conditions: []metav1.Condition{
				{
					Type:   uplinkv1alpha1.UplinkStateConditionResolved,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonResolved,
				},
				{
					Type:   uplinkv1alpha1.UplinkStateConditionGatewayReady,
					Status: metav1.ConditionTrue,
					Reason: uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
				},
			},
		},
	}
}

func newUnresolvedUplinkState(
	uplinkName string,
	nodeName string,
	hostInterfaceName string,
	reason string,
) *uplinkv1alpha1.UplinkState {
	state := newResolvedUplinkState(uplinkName, nodeName, hostInterfaceName)
	state.Status.Conditions[0].Status = metav1.ConditionFalse
	state.Status.Conditions[0].Reason = reason
	return state
}

func TestUplinkStateNotResolvedReasonSurfacesHostCause(t *testing.T) {
	g := gomega.NewWithT(t)
	nodeConfig := uplinkv1alpha1.UplinkNodeConfig{
		Type:              uplinkv1alpha1.UplinkTypeOVSBridge,
		HostInterfaceName: "br-blue",
	}

	// The DPU is blocked on host data: the host-side cause is surfaced.
	state := newDPUHostBlockedUplinkState("br-blue", "node-a", "br-blue",
		uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound)
	g.Expect(uplinkStateNotResolvedReason(state, nodeConfig)).
		To(gomega.Equal(uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound))

	// Same when the DPU has not written a Resolved condition at all.
	meta.RemoveStatusCondition(&state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionResolved)
	g.Expect(uplinkStateNotResolvedReason(state, nodeConfig)).
		To(gomega.Equal(uplinkv1alpha1.UplinkStateReasonHostInterfaceNotFound))

	// Without a host-side failure either, fall back to the generic reason.
	meta.RemoveStatusCondition(&state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionHostDataReady)
	g.Expect(uplinkStateNotResolvedReason(state, nodeConfig)).
		To(gomega.Equal("ResolvedConditionMissing"))
}

// newDPUHostBlockedUplinkState models a split DPU state where the DPU waits
// on host data and the DPU-host reports the root cause on HostDataReady.
func newDPUHostBlockedUplinkState(
	uplinkName string,
	nodeName string,
	hostInterfaceName string,
	hostDataReason string,
) *uplinkv1alpha1.UplinkState {
	state := newUnresolvedUplinkState(uplinkName, nodeName, hostInterfaceName,
		uplinkv1alpha1.UplinkStateReasonWaitingForDPUHost)
	state.Status.Conditions = append(state.Status.Conditions, metav1.Condition{
		Type:   uplinkv1alpha1.UplinkStateConditionHostDataReady,
		Status: metav1.ConditionFalse,
		Reason: hostDataReason,
	})
	return state
}

func newGatewayFailedUplinkState(
	uplinkName string,
	nodeName string,
	hostInterfaceName string,
	reason string,
) *uplinkv1alpha1.UplinkState {
	state := newResolvedUplinkState(uplinkName, nodeName, hostInterfaceName)
	gatewayReady := meta.FindStatusCondition(state.Status.Conditions, uplinkv1alpha1.UplinkStateConditionGatewayReady)
	gatewayReady.Status = metav1.ConditionFalse
	gatewayReady.Reason = reason
	return state
}

func getUplinkCondition(
	g gomega.Gomega,
	client *util.OVNClusterManagerClientset,
	uplinkName string,
	conditionType string,
) *metav1.Condition {
	uplink, err := client.UplinkClient.K8sV1alpha1().Uplinks().Get(
		context.Background(),
		uplinkName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return meta.FindStatusCondition(uplink.Status.Conditions, conditionType)
}

func getUplinkFinalizers(g gomega.Gomega, client *util.OVNClusterManagerClientset, uplinkName string) []string {
	uplink, err := client.UplinkClient.K8sV1alpha1().Uplinks().Get(
		context.Background(),
		uplinkName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return uplink.Finalizers
}

func getCUDNCondition(
	g gomega.Gomega,
	client *util.OVNClusterManagerClientset,
	cudnName string,
	conditionType string,
) *metav1.Condition {
	cudn, err := client.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(
		context.Background(),
		cudnName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return meta.FindStatusCondition(cudn.Status.Conditions, conditionType)
}

func expectUplinkStateExists(g gomega.Gomega, client *util.OVNClusterManagerClientset, uplinkName, nodeName string) {
	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName(uplinkName, nodeName),
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "UplinkState of Uplink %s on node %s is missing", uplinkName, nodeName)
}

func expectUplinkStateNotFound(g gomega.Gomega, client *util.OVNClusterManagerClientset, uplinkName, nodeName string) {
	_, err := client.UplinkClient.K8sV1alpha1().UplinkStates().Get(
		context.Background(),
		uplinkutil.StateName(uplinkName, nodeName),
		metav1.GetOptions{},
	)
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "UplinkState of Uplink %s on node %s still exists: %v", uplinkName, nodeName, err)
}
