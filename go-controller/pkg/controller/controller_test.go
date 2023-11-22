package controller

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

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

	startController := func(config *Config[v1.Pod], objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		coreFactory := informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)

		config.Informer = coreFactory.Core().V1().Pods().Informer()
		config.Lister = coreFactory.Core().V1().Pods().Lister().List
		if config.RateLimiter == nil {
			config.RateLimiter = workqueue.NewItemFastSlowRateLimiter(100*time.Millisecond, 1*time.Second, 5)
		}
		controller = NewController[v1.Pod]("controller-name", config)

		coreFactory.Start(stopChan)

		err := controller.Start(1)
		Expect(err).NotTo(HaveOccurred())
	}

	getDefaultConfig := func() *Config[v1.Pod] {
		return &Config[v1.Pod]{
			ObjNeedsUpdate: func(oldObj, newObj *v1.Pod) bool {
				if oldObj == nil || newObj == nil {
					return true
				}
				return !reflect.DeepEqual(oldObj, newObj)
			},
			Reconcile: func(key string) error {
				reconcileCounter.Add(1)
				return nil
			},
		}
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
		controller.Stop()
	})

	It("handles initial objects once", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		startController(getDefaultConfig(), namespace, pod1, pod2)
		checkReconcileCounterConsistently(2)
	})
	It("retries on failure", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		failureCounter := atomic.Uint64{}
		config.Reconcile = func(key string) error {
			failureCounter.Add(1)
			if failureCounter.Load() < 3 {
				return fmt.Errorf("failure")
			}
			return nil
		}
		startController(config, namespace, pod)
		Eventually(failureCounter.Load, 2).Should(BeEquivalentTo(3))
		time.Sleep(time.Second)
		Expect(failureCounter.Load()).To(BeEquivalentTo(3))
	})
	It("drops key after maxRetries", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		failureCounter := atomic.Uint64{}
		config.Reconcile = func(key string) error {
			failureCounter.Add(1)
			return fmt.Errorf("failure")
		}
		config.RateLimiter = workqueue.NewItemFastSlowRateLimiter(100*time.Millisecond, 1*time.Second, maxRetries)
		startController(config, namespace, pod)

		Eventually(failureCounter.Load, (maxRetries+1)*100*time.Millisecond).Should(BeEquivalentTo(maxRetries))
		time.Sleep(1 * time.Second)
		Expect(failureCounter.Load()).To(BeEquivalentTo(maxRetries + 1))

		// trigger update, check it is handled
		pod.Labels = map[string]string{"key": "value"}
		_, err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool { return failureCounter.Load() > maxRetries+1 }).Should(BeTrue())
	})
	It("ignores events when ObjNeedsUpdate returns false", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		config.ObjNeedsUpdate = func(oldObj, newObj *v1.Pod) bool {
			// only return true on add
			return oldObj == nil
		}
		startController(config, namespace, pod)
		// only add event will be handled
		checkReconcileCounter(1)

		pod.Labels = map[string]string{"key": "value"}
		_, err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// check update is ignored
		checkReconcileCounterConsistently(1)
	})
	It("runs InitialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		config := getDefaultConfig()
		synced := atomic.Bool{}
		config.InitialSync = func() error {
			// ensure reconcile is not called until initial sync is finished
			checkReconcileCounterConsistently(0)

			synced.Store(true)
			return nil
		}
		startController(config, namespace, pod)
		// start only returns after initial sync is finished, we can check synced value immediately
		Expect(synced.Load()).To(BeEquivalentTo(true))
		checkReconcileCounter(1)
	})
	It("handles events after InitialSync", func() {
		namespace := util.NewNamespace(namespace1Name)
		pod1 := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod1", namespace.Name),
		}
		pod2 := &v1.Pod{
			ObjectMeta: util.NewObjectMeta("pod2", namespace.Name),
		}
		config := getDefaultConfig()
		updatedPods := sync.Map{}
		config.Reconcile = func(key string) error {
			// add keys that were reconciled
			updatedPods.LoadOrStore(key, true)
			return nil
		}
		config.InitialSync = func() error {
			// trigger events after initial objects were listed
			err := fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(namespace.Name).Create(context.TODO(), pod2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			return nil
		}
		startController(config, namespace, pod1)
		// check that both pod1 and pod2 triggered a reconcile
		pod1Key, _ := cache.MetaNamespaceKeyFunc(pod1)
		pod2Key, _ := cache.MetaNamespaceKeyFunc(pod2)
		Eventually(func() sets.Set[string] {
			keys := sets.New[string]()
			updatedPods.Range(func(key, value any) bool {
				keys.Insert(key.(string))
				return true
			})
			return keys
		}).Should(BeEquivalentTo(sets.New[string](pod1Key, pod2Key)))
	})
})
