// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestPendingReprogramSNATRestorePersistsAcrossRetries(t *testing.T) {
	status := egressipv1.EgressIPStatusItem{Node: "egress-node", EgressIP: "192.0.2.10"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "namespace", Name: "pod"},
		Spec:       corev1.PodSpec{NodeName: "pod-node"},
	}
	podState := &podAssignmentState{
		egressIPName: "egress-ip",
		egressStatuses: egressStatuses{statusMap{
			status: egressStatusStatePendingReprogramSNATRestore,
		}},
	}
	controller := &EgressIPController{
		podAssignment: syncmap.NewSyncMap[*podAssignmentState](),
		nodeState:     syncmap.NewSyncMap[struct{}](),
	}

	restoreCalls := 0
	restoreErr := errors.New("injected default SNAT restore failure")
	err := controller.restorePendingReprogrammedPodSNATsWithFn(
		&util.DefaultNetInfo{}, pod, podState,
		func(got egressipv1.EgressIPStatusItem) error {
			restoreCalls++
			if got != status {
				t.Fatalf("restore status = %#v, want %#v", got, status)
			}
			return restoreErr
		},
	)
	if !errors.Is(err, restoreErr) {
		t.Fatalf("first restore error = %v, want %v", err, restoreErr)
	}
	cachedState, ok := controller.podAssignment.Load(getPodKey(pod))
	if !ok {
		t.Fatal("pending compensation was not persisted in pod assignment state")
	}
	if got := cachedState.statusMap[status]; got != egressStatusStatePendingReprogramSNATRestore {
		t.Fatalf("state after failed restore = %q, want %q", got, egressStatusStatePendingReprogramSNATRestore)
	}

	err = controller.restorePendingReprogrammedPodSNATsWithFn(
		&util.DefaultNetInfo{}, pod, cachedState,
		func(got egressipv1.EgressIPStatusItem) error {
			restoreCalls++
			if got != status {
				t.Fatalf("restore status = %#v, want %#v", got, status)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("retry restore failed: %v", err)
	}
	if restoreCalls != 2 {
		t.Fatalf("restore calls = %d, want 2", restoreCalls)
	}
	if got := cachedState.statusMap[status]; got != egressStatusStatePending {
		t.Fatalf("state after successful compensation = %q, want %q", got, egressStatusStatePending)
	}
}

func TestShouldRetryEgressIPPodAfterLogicalPortReconcile(t *testing.T) {
	newController := func(eIPC *EgressIPController) *DefaultNetworkController {
		return &DefaultNetworkController{
			BaseNetworkController: BaseNetworkController{
				CommonNetworkControllerInfo: CommonNetworkControllerInfo{
					nodeName: "node-a",
				},
			},
			eIPC: eIPC,
		}
	}
	newPod := func(node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "namespace",
				Name:      "pod",
			},
			Spec: corev1.PodSpec{
				NodeName: node,
			},
		}
	}

	tests := []struct {
		name    string
		oc      *DefaultNetworkController
		pod     *corev1.Pod
		addPort bool
		want    bool
	}{
		{
			name:    "local node pod with logical port work",
			oc:      newController(&EgressIPController{}),
			pod:     newPod("node-a"),
			addPort: true,
			want:    true,
		},
		{
			name:    "remote node pod does not wake retry",
			oc:      newController(&EgressIPController{}),
			pod:     newPod("node-b"),
			addPort: true,
			want:    false,
		},
		{
			name:    "local node pod without logical port work",
			oc:      newController(&EgressIPController{}),
			pod:     newPod("node-a"),
			addPort: false,
			want:    false,
		},
		{
			name:    "nil egress IP controller",
			oc:      newController(nil),
			pod:     newPod("node-a"),
			addPort: true,
			want:    false,
		},
		{
			name:    "nil pod",
			oc:      newController(&EgressIPController{}),
			pod:     nil,
			addPort: true,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.oc.shouldRetryEgressIPPodAfterLogicalPortReconcile(tt.pod, tt.addPort); got != tt.want {
				t.Fatalf("shouldRetryEgressIPPodAfterLogicalPortReconcile() = %t, want %t", got, tt.want)
			}
		})
	}
}
