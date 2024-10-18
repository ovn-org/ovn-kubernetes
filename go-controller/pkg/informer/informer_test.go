package informer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

func TestEventHandler(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Event Handler Suite")
}

func newPod(name, namespace string) *kapi.Pod {
	return &kapi.Pod{
		Status: kapi.PodStatus{
			Phase: kapi.PodRunning,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
		Spec: kapi.PodSpec{
			Containers: []kapi.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: "node1",
		},
	}
}

var _ = Describe("Informer Event Handler Tests", func() {
	const (
		namespace string = "test"
	)
	var (
		stopChan chan struct{}
		wg       *sync.WaitGroup
	)

	BeforeEach(func() {
		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}
	})

	AfterEach(func() {
		close(stopChan)
		wg.Wait()
	})

	It("processes an add event", func() {
		adds := int32(0)
		deletes := int32(0)

		k := fake.NewSimpleClientset(
			&kapi.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  types.UID(namespace),
					Name: namespace,
				},
				Spec:   kapi.NamespaceSpec{},
				Status: kapi.NamespaceStatus{},
			},
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			ReceiveAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			ns, err := k.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return ns != nil, nil
		}, 2).Should(BeTrue())

		pod := newPod("foo", namespace)
		_, err = k.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Consistently(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(0)), "deletes")
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")
	})

	It("do not processes an add event if the pod is set for deletion", func() {
		adds := int32(0)
		deletes := int32(0)

		k := fake.NewSimpleClientset(
			&kapi.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  types.UID(namespace),
					Name: namespace,
				},
				Spec:   kapi.NamespaceSpec{},
				Status: kapi.NamespaceStatus{},
			},
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			ReceiveAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			ns, err := k.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return ns != nil, nil
		}, 2).Should(BeTrue())

		pod := newPod("foo", namespace)
		now := metav1.Now()
		pod.SetDeletionTimestamp(&now)

		_, err = k.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Consistently(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(0)), "deletes")
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(0)), "adds")
	})

	It("adds existing pod and processes an update event", func() {
		adds := int32(0)
		deletes := int32(0)

		pod := newPod("foo", namespace)
		k := fake.NewSimpleClientset(
			[]runtime.Object{
				&kapi.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						UID:  types.UID(namespace),
						Name: namespace,
					},
					Spec:   kapi.NamespaceSpec{},
					Status: kapi.NamespaceStatus{},
				},
				pod,
			}...,
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			ReceiveAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			pod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod != nil, nil
		}, 2).Should(BeTrue())

		// Make sure the initial 'add' took place. This is important because the worker may take a
		// while to get started and synced from the informer. If that happened, the add and the update
		// performed below would be combined into a single 'add' event.
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")

		pod.Annotations = map[string]string{"bar": "baz"}
		pod.ResourceVersion = "11"

		_, err = k.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			pod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod.ResourceVersion == "11", nil
		}, 2).Should(BeTrue())

		// no deletes
		Consistently(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(0)), "deletes")
		// two updates, initial add from cache + update event
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(2)), "adds")
	})

	It("adds existing pod and do not processes an update event if it was set for deletion", func() {
		adds := int32(0)
		deletes := int32(0)

		pod := newPod("foo", namespace)
		k := fake.NewSimpleClientset(
			[]runtime.Object{
				&kapi.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						UID:  types.UID(namespace),
						Name: namespace,
					},
					Spec:   kapi.NamespaceSpec{},
					Status: kapi.NamespaceStatus{},
				},
				pod,
			}...,
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			ReceiveAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			pod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod != nil, nil
		}, 2).Should(BeTrue())

		pod.Annotations = map[string]string{"bar": "baz"}
		pod.ResourceVersion = "11"
		now := metav1.Now()
		pod.SetDeletionTimestamp(&now)

		_, err = k.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// no deletes
		Consistently(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(0)), "deletes")
		// only initial add from cache event
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")
	})

	It("adds existing pod and processes a delete event", func() {
		adds := int32(0)
		deletes := int32(0)

		k := fake.NewSimpleClientset(
			[]runtime.Object{
				&kapi.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						UID:  types.UID(namespace),
						Name: namespace,
					},
					Spec:   kapi.NamespaceSpec{},
					Status: kapi.NamespaceStatus{},
				},
				newPod("foo", namespace),
			}...,
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			ReceiveAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			pod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod != nil, nil
		}, 2).Should(BeTrue())

		// initial add from the cache
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")

		err = k.CoreV1().Pods(namespace).Delete(context.TODO(), "foo", *metav1.NewDeleteOptions(0))
		Expect(err).NotTo(HaveOccurred())

		// we stay at 1
		Consistently(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")
		// one delete event
		Eventually(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(1)), "deletes")
	})

	It("ignores updates using DiscardAllUpdates", func() {
		adds := int32(0)
		deletes := int32(0)

		pod := newPod("foo", namespace)
		k := fake.NewSimpleClientset(
			[]runtime.Object{
				&kapi.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						UID:  types.UID(namespace),
						Name: namespace,
					},
					Spec:   kapi.NamespaceSpec{},
					Status: kapi.NamespaceStatus{},
				},
				pod,
			}...,
		)

		f := informers.NewSharedInformerFactory(k, 0)

		e, err := NewDefaultEventHandler(
			"test",
			f.Core().V1().Pods().Informer(),
			func(obj interface{}) error {
				atomic.AddInt32(&adds, 1)
				return nil
			},
			func(obj interface{}) error {
				atomic.AddInt32(&deletes, 1)
				return nil
			},
			DiscardAllUpdates,
		)
		Expect(err).NotTo(HaveOccurred())

		f.Start(stopChan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Run(1, stopChan)
		}()

		err = wait.PollUntilContextTimeout(
			context.Background(),
			500*time.Millisecond,
			5*time.Second,
			true,
			func(context.Context) (done bool, err error) {
				return e.Synced(), nil
			},
		)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() (bool, error) {
			pod, err := k.CoreV1().Pods(namespace).Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod != nil, nil
		}, 2).Should(BeTrue())

		pod.Annotations = map[string]string{"bar": "baz"}
		pod.ResourceVersion = "1"
		_, err = k.CoreV1().Pods(namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// no deletes
		Consistently(func() int32 { return atomic.LoadInt32(&deletes) }).Should(Equal(int32(0)), "deletes")
		// only initial add, no further updates
		Eventually(func() int32 { return atomic.LoadInt32(&adds) }).Should(Equal(int32(1)), "adds")
	})

})

var _ = Describe("Event Handler Internals", func() {
	It("should enqueue a well formed event", func() {
		k := fake.NewSimpleClientset()
		factory := informers.NewSharedInformerFactory(k, 0)
		e := eventHandler{
			name:           "test",
			informer:       factory.Core().V1().Pods().Informer(),
			deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
			workqueue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
			add: func(obj interface{}) error {
				return nil
			},
			delete: func(obj interface{}) error {
				return nil
			},
			updateFilter: ReceiveAllUpdates,
		}

		obj := newPod("bar", "foo")

		e.enqueue(obj)

		Expect(e.workqueue.Len()).To(Equal(1))
	})

	It("should enqueue a well formed delete event", func() {
		k := fake.NewSimpleClientset()
		factory := informers.NewSharedInformerFactory(k, 0)
		e := eventHandler{
			name:           "test",
			informer:       factory.Core().V1().Pods().Informer(),
			deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
			workqueue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
			add: func(obj interface{}) error {
				return nil
			},
			delete: func(obj interface{}) error {
				return nil
			},
			updateFilter: ReceiveAllUpdates,
		}

		obj := newPod("bar", "foo")

		e.enqueueDelete(obj)

		Expect(e.workqueue.Len()).To(Equal(1))

		_, exists, err := e.deletedIndexer.GetByKey("foo/bar")
		Expect(err).NotTo(HaveOccurred())

		Expect(exists).To(BeTrue())
	})

	It("should not enqueue object set for deletion", func() {
		k := fake.NewSimpleClientset()
		factory := informers.NewSharedInformerFactory(k, 0)
		e := eventHandler{
			name:           "test",
			informer:       factory.Core().V1().Pods().Informer(),
			deletedIndexer: cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
			workqueue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
			add: func(obj interface{}) error {
				return nil
			},
			delete: func(obj interface{}) error {
				return nil
			},
			updateFilter: ReceiveAllUpdates,
		}

		obj := newPod("bar", "foo")
		now := metav1.Now()
		obj.SetDeletionTimestamp(&now)

		e.enqueue(obj)

		Expect(e.workqueue.Len()).To(Equal(0))
	})
})
