// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package uplink

import (
	"context"
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
)

func TestUplinkControllerRecoversReadinessAfterDelayedStatusUpdate(t *testing.T) {
	g := gomega.NewWithT(t)
	uplink := newUplink("br-blue", "role", "blue", "br-blue")
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	state.UID = "original-state"
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}), uplink, state,
	)
	uplinkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(uplinkIndexer.Add(uplink)).To(gomega.Succeed())
	controller.uplinkLister = uplinklisters.NewUplinkLister(uplinkIndexer)
	stateIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(stateIndexer.Add(state)).To(gomega.Succeed())
	controller.uplinkStateLister = uplinklisters.NewUplinkStateLister(stateIndexer)
	uplinkClient := client.UplinkClient.K8sV1alpha1().Uplinks()

	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	readyUplink, err := uplinkClient.Get(context.Background(), uplink.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.IsStatusConditionTrue(readyUplink.Status.Conditions, uplinkv1alpha1.UplinkConditionReady)).To(gomega.BeTrue())
	g.Expect(uplinkIndexer.Update(readyUplink)).To(gomega.Succeed())

	// Keep the Uplink informer at Ready=True while the state deletion causes
	// reconciliation to publish Ready=False to the API.
	g.Expect(stateIndexer.Delete(state)).To(gomega.Succeed())
	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	unreadyUplink, err := uplinkClient.Get(context.Background(), uplink.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.IsStatusConditionFalse(unreadyUplink.Status.Conditions, uplinkv1alpha1.UplinkConditionReady)).To(gomega.BeTrue())

	// The independent UplinkState watch sees recovery first. The stale Uplink
	// cache makes this reconcile skip restoring True in the API.
	recreatedState := state.DeepCopy()
	recreatedState.UID = "recreated-state"
	g.Expect(stateIndexer.Add(recreatedState)).To(gomega.Succeed())
	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	g.Expect(getUplinkCondition(g, client, uplink.Name, uplinkv1alpha1.UplinkConditionReady).Status).
		To(gomega.Equal(metav1.ConditionFalse))

	// Delivery of the delayed status update must schedule the repair even
	// though the Uplink spec did not change.
	g.Expect(uplinkIndexer.Update(unreadyUplink)).To(gomega.Succeed())
	g.Expect(uplinkNeedsUpdate(readyUplink, unreadyUplink)).To(gomega.BeTrue())
	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	recoveredUplink, err := uplinkClient.Get(context.Background(), uplink.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.IsStatusConditionTrue(recoveredUplink.Status.Conditions, uplinkv1alpha1.UplinkConditionReady)).To(gomega.BeTrue())

	// Observing our corrective write also queues a reconcile, but it must
	// settle without another write rather than creating a status feedback loop.
	g.Expect(uplinkIndexer.Update(recoveredUplink)).To(gomega.Succeed())
	g.Expect(uplinkNeedsUpdate(unreadyUplink, recoveredUplink)).To(gomega.BeTrue())
	fakeClient := client.UplinkClient.(*uplinkfake.Clientset)
	fakeClient.ClearActions()
	g.Expect(controller.reconcileUplink(uplink.Name)).To(gomega.Succeed())
	g.Expect(fakeClient.Actions()).To(gomega.BeEmpty())
}

func TestUplinkControllerRecoversCUDNReadinessAfterDelayedStatusUpdate(t *testing.T) {
	setSharedGatewayMode(t)
	g := gomega.NewWithT(t)
	cudn := newCUDN("blue", "br-blue")
	state := newResolvedUplinkState("br-blue", "node-a", "br-blue")
	state.UID = "original-state"
	controller, client := newTestController(t,
		newNode("node-a", map[string]string{"role": "blue"}),
		withFinalizer(newUplink("br-blue", "role", "blue", "br-blue")),
		cudn, state,
	)
	cudnIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(cudnIndexer.Add(cudn)).To(gomega.Succeed())
	controller.cudnLister = udnlisters.NewClusterUserDefinedNetworkLister(cudnIndexer)
	stateIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	g.Expect(stateIndexer.Add(state)).To(gomega.Succeed())
	controller.uplinkStateLister = uplinklisters.NewUplinkStateLister(stateIndexer)
	cudnClient := client.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks()

	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	readyCUDN, err := cudnClient.Get(context.Background(), cudn.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.IsStatusConditionTrue(readyCUDN.Status.Conditions, conditionTypeUplinksReady)).To(gomega.BeTrue())
	g.Expect(cudnIndexer.Update(readyCUDN)).To(gomega.Succeed())

	// Hold back the CUDN status event while delivering deletion and recovery
	// through the independent UplinkState informer.
	g.Expect(stateIndexer.Delete(state)).To(gomega.Succeed())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	unreadyCUDN, err := cudnClient.Get(context.Background(), cudn.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.FindStatusCondition(unreadyCUDN.Status.Conditions, conditionTypeUplinksReady)).To(gomega.And(
		gomega.HaveField("Status", metav1.ConditionFalse),
		gomega.HaveField("Reason", reasonUplinkNotResolvedForNode),
	))
	recreatedState := state.DeepCopy()
	recreatedState.UID = "recreated-state"
	g.Expect(stateIndexer.Add(recreatedState)).To(gomega.Succeed())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	g.Expect(getCUDNCondition(g, client, cudn.Name, conditionTypeUplinksReady).Status).
		To(gomega.Equal(metav1.ConditionFalse))

	// The delayed False event must trigger reconciliation of the ready state.
	g.Expect(cudnIndexer.Update(unreadyCUDN)).To(gomega.Succeed())
	g.Expect(cudnNeedsUpdate(readyCUDN, unreadyCUDN)).To(gomega.BeTrue())
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	recoveredCUDN, err := cudnClient.Get(context.Background(), cudn.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(meta.IsStatusConditionTrue(recoveredCUDN.Status.Conditions, conditionTypeUplinksReady)).To(gomega.BeTrue())

	// Reconciliation of the corrective status event must not write again.
	g.Expect(cudnIndexer.Update(recoveredCUDN)).To(gomega.Succeed())
	g.Expect(cudnNeedsUpdate(unreadyCUDN, recoveredCUDN)).To(gomega.BeTrue())
	fakeClient := client.UserDefinedNetworkClient.(*udnfake.Clientset)
	fakeClient.ClearActions()
	g.Expect(controller.reconcileCUDN(cudn.Name)).To(gomega.Succeed())
	g.Expect(fakeClient.Actions()).To(gomega.BeEmpty())
}

func TestUplinkReadinessUpdateFilters(t *testing.T) {
	for _, resource := range []struct {
		name          string
		conditionType string
		needsUpdate   func(oldConditions, newConditions []metav1.Condition) bool
	}{
		{
			name:          "Uplink",
			conditionType: uplinkv1alpha1.UplinkConditionReady,
			needsUpdate: func(oldConditions, newConditions []metav1.Condition) bool {
				oldUplink := newUplink("br-blue", "role", "blue", "br-blue")
				newUplink := oldUplink.DeepCopy()
				oldUplink.Status.Conditions = oldConditions
				newUplink.Status.Conditions = newConditions
				return uplinkNeedsUpdate(oldUplink, newUplink)
			},
		},
		{
			name:          "CUDN",
			conditionType: conditionTypeUplinksReady,
			needsUpdate: func(oldConditions, newConditions []metav1.Condition) bool {
				oldCUDN := newCUDN("blue", "br-blue")
				newCUDN := oldCUDN.DeepCopy()
				oldCUDN.Status.Conditions = oldConditions
				newCUDN.Status.Conditions = newConditions
				return cudnNeedsUpdate(oldCUDN, newCUDN)
			},
		},
	} {
		t.Run(resource.name, func(t *testing.T) {
			ready := metav1.Condition{Type: resource.conditionType, Status: metav1.ConditionTrue, Reason: "Ready"}
			unready := ready
			unready.Status = metav1.ConditionFalse
			changedReason := ready
			changedReason.Reason = "ChangedReason"
			changedMessage := ready
			changedMessage.Message = "changed message"
			other := metav1.Condition{Type: "OtherCondition", Status: metav1.ConditionTrue}
			changedOther := other
			changedOther.Status = metav1.ConditionFalse
			for _, tc := range []struct {
				name          string
				oldConditions []metav1.Condition
				newConditions []metav1.Condition
				wantUpdate    bool
			}{
				{name: "absent"},
				{name: "added", newConditions: []metav1.Condition{ready}, wantUpdate: true},
				{name: "removed", oldConditions: []metav1.Condition{ready}, wantUpdate: true},
				{name: "unchanged", oldConditions: []metav1.Condition{ready}, newConditions: []metav1.Condition{ready}},
				{name: "not ready", oldConditions: []metav1.Condition{ready}, newConditions: []metav1.Condition{unready}, wantUpdate: true},
				{name: "recovered", oldConditions: []metav1.Condition{unready}, newConditions: []metav1.Condition{ready}, wantUpdate: true},
				{name: "reason changed", oldConditions: []metav1.Condition{ready}, newConditions: []metav1.Condition{changedReason}, wantUpdate: true},
				{name: "message changed", oldConditions: []metav1.Condition{ready}, newConditions: []metav1.Condition{changedMessage}, wantUpdate: true},
				{name: "unrelated condition added", oldConditions: []metav1.Condition{ready}, newConditions: []metav1.Condition{ready, other}},
				{name: "unrelated condition changed", oldConditions: []metav1.Condition{ready, other}, newConditions: []metav1.Condition{ready, changedOther}},
				{name: "conditions reordered", oldConditions: []metav1.Condition{ready, other}, newConditions: []metav1.Condition{other, ready}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					gomega.NewWithT(t).Expect(resource.needsUpdate(tc.oldConditions, tc.newConditions)).To(gomega.Equal(tc.wantUpdate))
				})
			}
		})
	}
}
