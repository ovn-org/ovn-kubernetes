// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func getDefaultConfig[T any](reconcileCounter *atomic.Uint64) *ControllerConfig[T] {
	return &ControllerConfig[T]{
		ObjNeedsUpdate: func(oldObj, newObj *T) bool {
			if oldObj == nil || newObj == nil {
				return true
			}
			return !reflect.DeepEqual(oldObj, newObj)
		},
		Reconcile: func(string) error {
			reconcileCounter.Add(1)
			return nil
		},
		Threadiness: 1,
	}
}

var _ = Describe("Level-driven controller", func() {
	var (
		fakeClient       *util.OVNClusterManagerClientset
		stopChan         chan struct{}
		reconcileCounter atomic.Uint64
		controller       Controller
	)

	const (
		namespace1Name = "namespace1"
	)

	startController := func(config *ControllerConfig[corev1.Pod], initialSync func() error, objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		coreFactory := informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)

		config.Informer = coreFactory.Core().V1().Pods().Informer()
		config.Lister = coreFactory.Core().V1().Pods().Lister().List
		controller = NewController("controller-name", config)

		coreFactory.Start(stopChan)

		err := StartWithInitialSync(initialSync, controller)
		Expect(err).NotTo(HaveOccurred())
	}

	getDefaultConfig := func() *ControllerConfig[corev1.Pod] {
		return getDefaultConfig[corev1.Pod](&reconcileCounter)
	}

	checkReconcileCounter := func(expected int) {
		Eventually(reconcileCounter.Load).Should(BeEquivalentTo(expected), fmt.Sprintf("expected %v reconcile calls", expected))
	}

	checkReconcileCounterConsistently := func(expected int) {
		checkReconcileCounter(expected)
		time.Sleep(500 * time.Millisecond)
		Expect(reconcileCounter.Load()).To(BeEquivalentTo(expected), fmt.Sprintf("expected %v consistent reconcile calls", expected))
	}

	BeforeEach(func() {
		reconcileCounter.Store(0)
		stopChan = make(chan struct{})
	})

	AfterEach(func() {
		close(stopChan)
		Stop(controller)
	})

	It("has idempotent Stop", func() {
		startController(getDefaultConfig(), nil)
		Stop(controller)
		Stop(controller)
	})
	It("handles initial objects once", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		startController(getDefaultConfig(), nil, namespace, pod1, pod2)
		checkReconcileCounterConsistently(2)
	})
	It("retries on failure", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		failureCounter := atomic.Uint64{}
		config.Reconcile = func(string) error {
			failureCounter.Add(1)
			if failureCounter.Load() < 3 {
				return fmt.Errorf("failure")
			}
			return nil
		}
		startController(config, nil, namespace, pod)
		Eventually(failureCounter.Load, 2).Should(BeEquivalentTo(3))
		time.Sleep(time.Second)
		Expect(failureCounter.Load()).To(BeEquivalentTo(3))
	})
	It("drops key after maxRetries", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		failureCounter := atomic.Uint64{}
		config.Reconcile = func(string) error {
			failureCounter.Add(1)
			return fmt.Errorf("failure")
		}
		config.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Millisecond, 50*time.Millisecond)
		startController(config, nil, namespace, pod)

		Eventually(failureCounter.Load, 5*time.Second).Should(BeEquivalentTo(DefaultMaxAttempts))
		time.Sleep(200 * time.Millisecond)
		Expect(failureCounter.Load()).To(BeEquivalentTo(DefaultMaxAttempts + 1))

		// trigger update, check it is handled
		pod.Labels = map[string]string{"key": "value"}
		_, err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool { return failureCounter.Load() > DefaultMaxAttempts+1 }).Should(BeTrue())
	})
	It("ignores events when ObjNeedsUpdate returns false", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		config.ObjNeedsUpdate = func(oldObj, _ *corev1.Pod) bool {
			// only return true on add
			return oldObj == nil
		}
		startController(config, nil, namespace, pod)
		// only add event will be handled
		checkReconcileCounter(1)

		pod.Labels = map[string]string{"key": "value"}
		_, err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// check update is ignored
		checkReconcileCounterConsistently(1)
	})
	It("runs initialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		synced := atomic.Bool{}
		initialSync := func() error {
			// ensure reconcile is not called until initial sync is finished
			checkReconcileCounterConsistently(0)

			synced.Store(true)
			return nil
		}
		startController(config, initialSync, namespace, pod)
		// start only returns after initial sync is finished, we can check synced value immediately
		Expect(synced.Load()).To(BeEquivalentTo(true))
		checkReconcileCounter(1)
	})
	It("handles events after InitialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		config := getDefaultConfig()
		updatedPods := sync.Map{}
		config.Reconcile = func(key string) error {
			// add keys that were reconciled
			updatedPods.LoadOrStore(key, true)
			return nil
		}
		initialSync := func() error {
			// trigger events after initial objects were listed
			err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Create(context.TODO(), pod2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			return nil
		}
		startController(config, initialSync, namespace, pod1)
		// check that both pod1 and pod2 triggered a reconcile
		pod1Key, _ := cache.MetaNamespaceKeyFunc(pod1)
		pod2Key, _ := cache.MetaNamespaceKeyFunc(pod2)
		Eventually(func() sets.Set[string] {
			keys := sets.New[string]()
			updatedPods.Range(func(key, _ any) bool {
				keys.Insert(key.(string))
				return true
			})
			return keys
		}).Should(BeEquivalentTo(sets.New(pod1Key, pod2Key)))
	})
	It("invokes OnDelete with the deleted object", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		// Record namespace by name to prove OnDelete receives the actual
		// object (not just a key).
		deleted := sync.Map{}
		config.OnDelete = func(obj *corev1.Pod) {
			deleted.Store(obj.Name, obj.Namespace)
		}
		startController(config, nil, namespace, pod)
		checkReconcileCounter(1)

		err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			ns, ok := deleted.Load("pod1")
			return ok && ns == namespace.Name
		}).Should(BeTrue(), "OnDelete should be called with the deleted pod object")
	})
})

var _ = Describe("Level-driven controller deletedObj", func() {
	c := &controller[corev1.Pod]{name: "test"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1"}}

	It("returns the object for a plain delete", func() {
		Expect(c.deletedObj(pod)).To(BeIdenticalTo(pod))
	})
	It("unwraps a DeletedFinalStateUnknown tombstone", func() {
		tombstone := cache.DeletedFinalStateUnknown{Key: "ns1/pod1", Obj: pod}
		Expect(c.deletedObj(tombstone)).To(BeIdenticalTo(pod))
	})
	It("returns nil for an unexpected type", func() {
		Expect(c.deletedObj("not-a-pod")).To(BeNil())
	})
	It("returns nil for a tombstone wrapping an unexpected type", func() {
		tombstone := cache.DeletedFinalStateUnknown{Key: "ns1/pod1", Obj: "not-a-pod"}
		Expect(c.deletedObj(tombstone)).To(BeNil())
	})
})

var _ = Describe("Level-driven controller onDelete tombstone enqueue", func() {
	var (
		stopChan         chan struct{}
		reconcileCounter atomic.Uint64
		ctrl             Controller
	)

	startNodeController := func(config *ControllerConfig[corev1.Node], objects ...runtime.Object) {
		fakeClient := util.GetOVNClientset(objects...).GetClusterManagerClientset()
		coreFactory := informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)
		config.Informer = coreFactory.Core().V1().Nodes().Informer()
		config.Lister = coreFactory.Core().V1().Nodes().Lister().List
		if config.RateLimiter == nil {
			config.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](100*time.Millisecond, 1*time.Second)
		}
		ctrl = NewController("controller-name", config)
		coreFactory.Start(stopChan)
		err := StartWithInitialSync(nil, ctrl)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		reconcileCounter.Store(0)
		stopChan = make(chan struct{})
	})

	AfterEach(func() {
		close(stopChan)
		Stop(ctrl)
	})

	It("enqueues reconcile for DeletedFinalStateUnknown delete events", func() {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a"}}
		config := getDefaultConfig[corev1.Node](&reconcileCounter)
		startNodeController(config, node)
		Eventually(reconcileCounter.Load).Should(BeEquivalentTo(1))

		cc := ctrl.(*controller[corev1.Node])
		cc.onDelete(cache.DeletedFinalStateUnknown{Key: node.Name, Obj: node})
		Eventually(reconcileCounter.Load).Should(BeEquivalentTo(2))
	})
})

var _ = Describe("Level-driven controllers with shared initialSync", func() {
	var (
		fakeClient          *util.OVNClusterManagerClientset
		stopChan            chan struct{}
		reconcilePodCounter atomic.Uint64
		reconcileNsCounter  atomic.Uint64

		podController       Controller
		namespaceController Controller
	)

	const (
		namespace1Name = "namespace1"
	)

	startController := func(podConfig *ControllerConfig[corev1.Pod], nsConfig *ControllerConfig[corev1.Namespace], initialSync func() error, objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		coreFactory := informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)

		podConfig.Informer = coreFactory.Core().V1().Pods().Informer()
		podConfig.Lister = coreFactory.Core().V1().Pods().Lister().List
		if podConfig.RateLimiter == nil {
			podConfig.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](100*time.Millisecond, 1*time.Second)
		}
		podController = NewController("podController", podConfig)

		nsConfig.Informer = coreFactory.Core().V1().Namespaces().Informer()
		nsConfig.Lister = coreFactory.Core().V1().Namespaces().Lister().List
		if nsConfig.RateLimiter == nil {
			nsConfig.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](100*time.Millisecond, 1*time.Second)
		}
		namespaceController = NewController("namespaceController", nsConfig)

		coreFactory.Start(stopChan)

		err := StartWithInitialSync(initialSync, podController, namespaceController)
		Expect(err).NotTo(HaveOccurred())
	}

	checkReconcileCounters := func(expectedPod, expectedNs int) {
		Eventually(reconcilePodCounter.Load).Should(BeEquivalentTo(expectedPod), fmt.Sprintf("expected %v pod reconcile calls", expectedPod))
		Eventually(reconcileNsCounter.Load).Should(BeEquivalentTo(expectedNs), fmt.Sprintf("expected %v namespace reconcile calls", expectedNs))
	}

	checkReconcileCountersConsistently := func(expectedPod, expectedNs int) {
		checkReconcileCounters(expectedPod, expectedNs)
		time.Sleep(500 * time.Millisecond)
		Expect(reconcilePodCounter.Load()).To(BeEquivalentTo(expectedPod), fmt.Sprintf("expected %v consistent pod reconcile calls", expectedPod))
		Expect(reconcileNsCounter.Load()).To(BeEquivalentTo(expectedNs), fmt.Sprintf("expected %v consistent namespace reconcile calls", expectedNs))
	}

	BeforeEach(func() {
		reconcilePodCounter.Store(0)
		reconcileNsCounter.Store(0)
		stopChan = make(chan struct{})
	})

	AfterEach(func() {
		close(stopChan)
		Stop(podController, namespaceController)
	})

	It("handle initial objects once", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		startController(getDefaultConfig[corev1.Pod](&reconcilePodCounter), getDefaultConfig[corev1.Namespace](&reconcileNsCounter),
			nil, namespace, pod1, pod2)
		checkReconcileCountersConsistently(2, 1)
	})
	It("run InitialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		synced := atomic.Bool{}
		initialSync := func() error {
			// ensure reconcile is not called until initial sync is finished
			checkReconcileCountersConsistently(0, 0)

			synced.Store(true)
			return nil
		}
		startController(getDefaultConfig[corev1.Pod](&reconcilePodCounter), getDefaultConfig[corev1.Namespace](&reconcileNsCounter),
			initialSync, namespace, pod)
		// start only returns after initial sync is finished, we can check synced value immediately
		Expect(synced.Load()).To(BeEquivalentTo(true))
		checkReconcileCounters(1, 1)
	})
	It("handle events after initialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &corev1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		podConfig := getDefaultConfig[corev1.Pod](&reconcilePodCounter)
		updatedObjs := sync.Map{}
		podConfig.Reconcile = func(key string) error {
			reconcilePodCounter.Add(1)
			// add keys that were reconciled
			updatedObjs.LoadOrStore(key, true)
			return nil
		}
		nsConfig := getDefaultConfig[corev1.Namespace](&reconcileNsCounter)
		nsConfig.Reconcile = func(key string) error {
			reconcileNsCounter.Add(1)
			// add keys that were reconciled
			updatedObjs.LoadOrStore(key, true)
			return nil
		}
		initialSync := func() error {
			// trigger events after initial objects were listed
			err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Create(context.TODO(), pod2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			namespaceUpd := util.NewNamespace(namespace1Name)
			namespace.Labels = map[string]string{"key": "value"}
			_, err = fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpd, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			checkReconcileCounters(0, 0)
			return nil
		}
		startController(podConfig, nsConfig, initialSync, namespace, pod1)
		// check that pod1, pod2, and namespace triggered a reconcile
		pod1Key, _ := cache.MetaNamespaceKeyFunc(pod1)
		pod2Key, _ := cache.MetaNamespaceKeyFunc(pod2)
		Eventually(func() sets.Set[string] {
			keys := sets.New[string]()
			updatedObjs.Range(func(key, _ any) bool {
				keys.Insert(key.(string))
				return true
			})
			return keys
		}).Should(BeEquivalentTo(sets.New(pod1Key, pod2Key, namespace.Name)))
		fmt.Println(reconcilePodCounter.Load())
		fmt.Println(reconcileNsCounter.Load())
	})
})
