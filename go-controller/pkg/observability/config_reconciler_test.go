// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	observabilityconfigfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/clientset/versioned/fake"
	observabilityconfiginformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/informers/externalversions"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeNodeWatcher implements NodeWatcher for tests. GetNode reads live from client when set
// (so a node Update is reflected), otherwise it returns the fixed node/err. informer is what
// NodeInformer returns; it may be left nil in unit tests that call localNodeLabels directly
// (without start()), but start() requires a real informer.
type fakeNodeWatcher struct {
	node     *corev1.Node
	err      error
	client   kubernetes.Interface
	informer cache.SharedIndexInformer
}

func (f fakeNodeWatcher) GetNode(name string) (*corev1.Node, error) {
	if f.client != nil {
		return f.client.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	}
	return f.node, f.err
}

func (f fakeNodeWatcher) NodeInformer() cache.SharedIndexInformer { return f.informer }

// newTestNodeWatcher returns a NodeWatcher backed by a single fixed node and a real (unstarted)
// node informer. It satisfies the non-nil NodeWatcher contract for tests that call start() but do
// not exercise node-label watching: the node resolves so localNodeLabels succeeds.
func newTestNodeWatcher(nodeName string) fakeNodeWatcher {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	kubeClient := k8sfake.NewSimpleClientset(node)
	informer := informers.NewSharedInformerFactory(kubeClient, 0).Core().V1().Nodes().Informer()
	return fakeNodeWatcher{node: node, informer: informer}
}

// recordingApplier records how many times the reconciler applied or cleared configs.
type recordingApplier struct {
	mu      sync.Mutex
	applied int
	cleared int
}

func (r *recordingApplier) applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied++
	return nil
}

func (r *recordingApplier) clearConfig() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared++
}

func (r *recordingApplier) appliedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied
}

var _ = Describe("Observability config reconciler", func() {
	Describe("localNodeLabels", func() {
		It("returns the error when the node lookup fails, so the caller can requeue", func() {
			labels, err := localNodeLabels(fakeNodeWatcher{err: fmt.Errorf("boom")}, "node1")
			Expect(err).To(HaveOccurred())
			Expect(labels).To(BeNil())
		})

		It("returns an error when the node is not found (nil node, nil error)", func() {
			labels, err := localNodeLabels(fakeNodeWatcher{}, "node1")
			Expect(err).To(HaveOccurred())
			Expect(labels).To(BeNil())
		})

		It("normalizes a resolved node with no labels to a non-nil empty map", func() {
			labels, err := localNodeLabels(fakeNodeWatcher{node: &corev1.Node{}}, "node1")
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).NotTo(BeNil())
			Expect(labels).To(BeEmpty())
		})

		It("returns the node's labels when present", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"env": "prod"}}}
			labels, err := localNodeLabels(fakeNodeWatcher{node: node}, "node1")
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).To(Equal(map[string]string{"env": "prod"}))
		})
	})

	Describe("configAppliesToNode", func() {
		matchAll := func() *observabilityconfigv1alpha1.ObservabilityConfig {
			return &observabilityconfigv1alpha1.ObservabilityConfig{
				Spec: observabilityconfigv1alpha1.ObservabilitySpec{
					Filter: &observabilityconfigv1alpha1.Filter{NodeSelector: &metav1.LabelSelector{}},
				},
			}
		}

		It("applies a match-all NodeSelector to a resolved node with no labels (empty map)", func() {
			Expect(configAppliesToNode(matchAll(), map[string]string{})).To(BeTrue())
		})

		It("does not apply a NodeSelector config when no labels resolved (nil labels, defensive)", func() {
			Expect(configAppliesToNode(matchAll(), nil)).To(BeFalse())
		})

		It("applies a config with no Filter regardless of labels", func() {
			cfg := &observabilityconfigv1alpha1.ObservabilityConfig{}
			Expect(configAppliesToNode(cfg, nil)).To(BeTrue())
		})
	})

	Describe("reconcile", func() {
		It("returns an error (requeues) when the local node cannot be resolved", func() {
			fakeClient := observabilityconfigfake.NewSimpleClientset()
			factory := observabilityconfiginformerfactory.NewSharedInformerFactory(fakeClient, 0)
			informer := factory.K8s().V1alpha1().ObservabilityConfigs()

			watcher := fakeNodeWatcher{err: fmt.Errorf("boom")}
			r := newConfigReconciler(&recordingApplier{}, informer, watcher, "node1")
			Expect(r.reconcile(reconcileKey)).To(HaveOccurred())
		})
	})

	Describe("node label watching", func() {
		It("re-reconciles when the local node's labels start matching a NodeSelector", func() {
			// A config that only applies to nodes labelled env=prod.
			cr := &observabilityconfigv1alpha1.ObservabilityConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: observabilityconfigv1alpha1.ObservabilitySpec{
					CollectorID: defaultObservabilityCollectorSetID,
					Features: []observabilityconfigv1alpha1.FeatureConfig{
						{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100},
					},
					Filter: &observabilityconfigv1alpha1.Filter{
						NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
					},
				},
			}
			obsClient := observabilityconfigfake.NewSimpleClientset(cr)
			obsFactory := observabilityconfiginformerfactory.NewSharedInformerFactory(obsClient, 0)
			obsInformer := obsFactory.K8s().V1alpha1().ObservabilityConfigs()
			// Materialize both shared informers before Start so the factory actually runs them.
			obsShared := obsInformer.Informer()

			// node1 starts without the env=prod label, so the config does not apply yet.
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
			kubeClient := k8sfake.NewSimpleClientset(node)
			kubeFactory := informers.NewSharedInformerFactory(kubeClient, 0)
			nodeInformer := kubeFactory.Core().V1().Nodes().Informer()

			stopCh := make(chan struct{})
			defer close(stopCh)
			obsFactory.Start(stopCh)
			kubeFactory.Start(stopCh)
			Expect(cache.WaitForCacheSync(stopCh, obsShared.HasSynced, nodeInformer.HasSynced)).To(BeTrue())

			applier := &recordingApplier{}
			watcher := fakeNodeWatcher{client: kubeClient, informer: nodeInformer}
			r := newConfigReconciler(applier, obsInformer, watcher, "node1")
			Expect(r.start()).To(Succeed())
			defer r.stop()

			// Before the label is set, the config does not apply.
			Consistently(applier.appliedCount).Should(Equal(0))

			// Label the node so the NodeSelector starts matching: the node handler must re-reconcile.
			node.Labels = map[string]string{"env": "prod"}
			_, err := kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(applier.appliedCount).Should(BeNumerically(">", 0))
		})
	})
})
