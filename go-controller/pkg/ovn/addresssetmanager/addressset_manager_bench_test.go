// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

type benchReconciler struct {
	controller.Reconciler
}

func (r *benchReconciler) Reconcile(_ string) {}

// benchNamespaceManager returns a manager whose namespace lister is backed by
// nsCount namespaces, labelled the way the apiserver and a typical workload
// label them.
func benchNamespaceManager(b *testing.B, nsCount int) *AddressSetManager {
	b.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for i := 0; i < nsCount; i++ {
		name := fmt.Sprintf("bench-ns-%d", i)
		if err := indexer.Add(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					corev1.LabelMetadataName: name,
					"tier":                   fmt.Sprintf("tier-%d", i%4),
				},
			},
		}); err != nil {
			b.Fatal(err)
		}
	}
	return &AddressSetManager{namespaceLister: listers.NewNamespaceLister(indexer)}
}

// benchManagerWithAddressSets returns a manager populated with addrSetCount address sets
// reflecting a realistic distribution of network policy peers:
// - 80% exact-name pinned namespace selectors (kubernetes.io/metadata.name: <ns>)
// - 10% label selectors (tier: tier-X)
// - 10% static namespace address sets
func benchManagerWithAddressSets(b *testing.B, nsCount, addrSetCount int) *AddressSetManager {
	b.Helper()
	m := benchNamespaceManager(b, nsCount)
	m.addressSets = syncmap.NewSyncMap[*podSelectorAddressSet]()
	m.addressSetReconciler = &benchReconciler{}

	for i := 0; i < addrSetCount; i++ {
		key := fmt.Sprintf("addrset-%d", i)
		var s *podSelectorAddressSet
		switch {
		case i < addrSetCount*8/10:
			targetNs := fmt.Sprintf("bench-ns-%d", i%nsCount)
			s = benchAddrSet(b, map[string]string{corev1.LabelMetadataName: targetNs})
		case i < addrSetCount*9/10:
			s = benchAddrSet(b, map[string]string{"tier": fmt.Sprintf("tier-%d", i%4)})
		default:
			s = &podSelectorAddressSet{namespace: fmt.Sprintf("bench-ns-%d", i%nsCount)}
		}
		s.selectedNamespaces = &selectedNamespaces{set: sets.New[string]()}
		m.addressSets.Store(key, s)
	}
	return m
}

func benchAddrSet(b *testing.B, sel map[string]string) *podSelectorAddressSet {
	b.Helper()
	nsSel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: sel})
	if err != nil {
		b.Fatal(err)
	}
	return &podSelectorAddressSet{namespaceSelector: nsSel}
}

// BenchmarkGetSelectedNamespacesByName measures resolving the idiomatic
// "namespaceSelector: {kubernetes.io/metadata.name: <ns>}" network policy peer
// across increasing cluster sizes (100, 1,000, 5,000 namespaces).
func BenchmarkGetSelectedNamespacesByName(b *testing.B) {
	for _, nsCount := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("%d_namespaces", nsCount), func(b *testing.B) {
			m := benchNamespaceManager(b, nsCount)
			s := benchAddrSet(b, map[string]string{corev1.LabelMetadataName: fmt.Sprintf("bench-ns-%d", nsCount/2)})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := m.getSelectedNamespaces(s)
				if err != nil {
					b.Fatal(err)
				}
				if got.set.Len() != 1 {
					b.Fatalf("expected 1 namespace, got %d", got.set.Len())
				}
			}
		})
	}
}

// BenchmarkGetSelectedNamespacesByLabel is the full-scan baseline for label selectors.
func BenchmarkGetSelectedNamespacesByLabel(b *testing.B) {
	m := benchNamespaceManager(b, 1000)
	s := benchAddrSet(b, map[string]string{"tier": "tier-2"})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.getSelectedNamespaces(s); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReconcileNamespace benchmarks the full reconcileNamespace loop across
// 500 registered address sets in a 1,000-namespace cluster on a namespace event.
func BenchmarkReconcileNamespace(b *testing.B) {
	m := benchManagerWithAddressSets(b, 1000, 500)
	targetNs := "bench-ns-500"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := m.reconcileNamespace(targetNs); err != nil {
			b.Fatal(err)
		}
	}
}
