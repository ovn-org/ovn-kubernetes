// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	egressserviceapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicelisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
)

// TestEgressServicePodIPs verifies namespace ownership independently of selectors
// and excludes pods that cannot supply valid egress endpoints.
func TestEgressServicePodIPs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "tenant", Labels: map[string]string{"app": "backend"}},
		Spec:       corev1.PodSpec{NodeName: "node1"},
		Status:     corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.128.1.5"}, {IP: "fe00:10:128:1::5"}}},
	}
	for _, tc := range []struct {
		name     string
		modify   func(*corev1.Pod)
		selector map[string]string
		want     map[string]string
	}{
		{name: "dual stack", selector: map[string]string{"app": "backend"}, want: map[string]string{"10.128.1.5": "node1", "fe00:10:128:1::5": "node1"}},
		{name: "selectorless", want: map[string]string{"10.128.1.5": "node1", "fe00:10:128:1::5": "node1"}},
		{name: "other namespace", selector: map[string]string{"app": "backend"}, modify: func(p *corev1.Pod) { p.Namespace = "victim" }},
		{name: "selector mismatch", selector: map[string]string{"app": "other"}, want: map[string]string{"10.128.1.5": "node1", "fe00:10:128:1::5": "node1"}},
		{name: "host network", modify: func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
		{name: "unscheduled", modify: func(p *corev1.Pod) { p.Spec.NodeName = "" }},
		{name: "succeeded", modify: func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
		{name: "failed", modify: func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }},
		{name: "no status IP", modify: func(p *corev1.Pod) { p.Status.PodIPs = nil }},
		{name: "legacy PodIP", modify: func(p *corev1.Pod) { p.Status.PodIPs = nil; p.Status.PodIP = "10.128.1.5" }, want: map[string]string{"10.128.1.5": "node1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			p := pod.DeepCopy()
			if tc.modify != nil {
				tc.modify(p)
			}
			if err := indexer.Add(p); err != nil {
				t.Fatalf("failed to add pod to indexer: %v", err)
			}
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"}, Spec: corev1.ServiceSpec{Selector: tc.selector}}
			got, err := EgressServicePodIPs(svc, corelisters.NewPodLister(indexer))
			if err != nil {
				t.Fatalf("failed to calculate eligible pod IPs: %v", err)
			}
			want := tc.want
			if want == nil {
				want = map[string]string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			if err := indexer.Delete(p); err != nil {
				t.Fatalf("failed to delete pod from indexer: %v", err)
			}
			got, err = EgressServicePodIPs(svc, corelisters.NewPodLister(indexer))
			if err != nil || len(got) != 0 {
				t.Fatalf("deleted pod still eligible: %v, %v", got, err)
			}
		})
	}
}

// TestEgressServicePodEvents verifies that ownership changes reconcile EgressServices
// while unrelated namespaces and label-only updates do not.
func TestEgressServicePodEvents(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range []*egressserviceapi.EgressService{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "egress"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "egress"}},
	} {
		if err := indexer.Add(svc); err != nil {
			t.Fatalf("failed to add EgressService to indexer: %v", err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "backend", Labels: map[string]string{"app": "backend"}}}
	var queued []string
	handler := EgressServicePodEventHandler(egressservicelisters.NewEgressServiceLister(indexer), func(key string) { queued = append(queued, key) })
	for _, tc := range []struct {
		name  string
		event func()
	}{
		{"add after slice", func() { handler.AddFunc(pod) }},
		{"delete with stale slice", func() { handler.DeleteFunc(pod) }},
		{"tombstone", func() { handler.DeleteFunc(cache.DeletedFinalStateUnknown{Obj: pod}) }},
		{"IP assigned", func() {
			p := pod.DeepCopy()
			p.Status.PodIPs = []corev1.PodIP{{IP: "10.128.1.5"}}
			handler.UpdateFunc(pod, p)
		}},
		{"completed", func() { p := pod.DeepCopy(); p.Status.Phase = corev1.PodSucceeded; handler.UpdateFunc(pod, p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queued = nil
			tc.event()
			if !reflect.DeepEqual(queued, []string{"tenant/egress"}) {
				t.Fatalf("unexpected queue: %v", queued)
			}
		})
	}
	queued = nil
	podWithoutEgressService := pod.DeepCopy()
	podWithoutEgressService.Namespace = "no-egress-service"
	handler.AddFunc(podWithoutEgressService)
	if len(queued) != 0 {
		t.Fatalf("pod in namespace without EgressServices queued: %v", queued)
	}
	changedLabels := pod.DeepCopy()
	changedLabels.Labels = nil
	handler.UpdateFunc(pod, changedLabels)
	if len(queued) != 0 {
		t.Fatalf("label-only change queued: %v", queued)
	}
	handler.UpdateFunc(pod, pod.DeepCopy())
	if len(queued) != 0 {
		t.Fatalf("unchanged pod queued: %v", queued)
	}
}
