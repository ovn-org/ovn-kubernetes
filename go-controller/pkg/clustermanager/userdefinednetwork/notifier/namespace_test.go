package notifier

import (
	"context"
	"maps"
	"strconv"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	netv1fake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	udnv1fake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("NamespaceNotifier", func() {
	var (
		kubeClient     *fake.Clientset
		wf             *factory.WatchFactory
		testNsNotifier *NamespaceNotifier
	)

	BeforeEach(func() {
		kubeClient = fake.NewSimpleClientset()

		// enable features to make watch-factory start the namespace informer
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		fakeClient := &util.OVNClusterManagerClientset{
			KubeClient:               kubeClient,
			NetworkAttchDefClient:    netv1fake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnv1fake.NewSimpleClientset(),
		}
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())
	})

	AfterEach(func() {
		wf.Shutdown()
	})

	var s *testSubscriber

	BeforeEach(func() {
		s = &testSubscriber{reconciledKeys: map[string]int64{}}
		testNsNotifier = NewNamespaceNotifier(wf.NamespaceInformer(), s)
		Expect(controller.Start(testNsNotifier.Controller)).Should(Succeed())

		// create tests namespaces
		for i := 0; i < 3; i++ {
			nsName := "test-" + strconv.Itoa(i)
			_, err := kubeClient.CoreV1().Namespaces().Create(context.Background(), testNamespace(nsName), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		if testNsNotifier != nil {
			controller.Stop(testNsNotifier.Controller)
		}
	})

	It("should notify namespace create events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-0": 1,
			"test-1": 1,
			"test-2": 1,
		}))
	})

	It("should notify namespace delete events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-0": 1,
			"test-1": 1,
			"test-2": 1,
		}))

		Expect(kubeClient.CoreV1().Namespaces().Delete(context.Background(), "test-2", metav1.DeleteOptions{})).To(Succeed())
		Expect(kubeClient.CoreV1().Namespaces().Delete(context.Background(), "test-0", metav1.DeleteOptions{})).To(Succeed())

		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-0": 2,
			"test-1": 1,
			"test-2": 2,
		}), "should record additional two events, following namespaces deletion")
	})

	It("should notify namespace labels change events", func() {
		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-0": 1,
			"test-1": 1,
			"test-2": 1,
		}))

		ns, err := kubeClient.CoreV1().Namespaces().Get(context.Background(), "test-1", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		ns.Labels["test.io"] = "example"
		ns, err = kubeClient.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() map[string]int64 {
			return s.GetReconciledKeys()
		}).Should(Equal(map[string]int64{
			"test-0": 1,
			"test-1": 2,
			"test-2": 1,
		}), "should record additional event following namespace update")
	})
})

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelMetadataName: name,
			},
		},
	}
}

type testSubscriber struct {
	err            error
	reconciledKeys map[string]int64
	lock           sync.RWMutex
}

func (s *testSubscriber) ReconcileNamespace(key string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.reconciledKeys[key]++
	return s.err
}

func (s *testSubscriber) GetReconciledKeys() map[string]int64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	cp := map[string]int64{}
	maps.Copy(cp, s.reconciledKeys)
	return cp
}
