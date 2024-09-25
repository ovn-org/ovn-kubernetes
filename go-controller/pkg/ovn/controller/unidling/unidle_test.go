package unidling

import (
	"testing"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"golang.org/x/net/context"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUnidlingContoller(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Unilding Controller Suite")
	defer GinkgoRecover()
}

var _ = Describe("Unidling Controller", func() {
	var cleanup *libovsdbtest.Context

	BeforeEach(func() {
		cleanup = nil
	})

	AfterEach(func() {
		if cleanup != nil {
			cleanup.Cleanup()
		}
	})

	It("should respond to a controller event", func() {
		client := fake.NewSimpleClientset()
		recorder := record.NewFakeRecorder(10)
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		serviceInformer := informerFactory.Core().V1().Services().Informer()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testSetup := libovsdbtest.TestSetup{
			SBData: []libovsdbtest.TestData{
				&sbdb.ControllerEvent{
					EventType: sbdb.ControllerEventEventTypeEmptyLbBackends,
					SeqNum:    8,
					EventInfo: map[string]string{
						"vip":      "10.10.10.10:80",
						"protocol": "tcp",
					},
				},
			},
		}

		var sbClient libovsdbclient.Client
		var err error
		sbClient, cleanup, err = libovsdbtest.NewSBTestHarness(testSetup, nil)
		Expect(err).NotTo(HaveOccurred())

		config.OvnSouth.Scheme = config.OvnDBSchemeTCP
		config.OvnSouth.Address = "tcp::56640"

		c, err := NewController(
			recorder,
			serviceInformer,
			sbClient,
		)
		Expect(err).NotTo(HaveOccurred())

		informerFactory.Start(ctx.Done())

		svc := &kapi.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo_ns", Name: "foo_service",
				Annotations: map[string]string{"ovn/idled-at": "2022-02-22T22:22:22Z"},
			},
			Spec: kapi.ServiceSpec{
				ClusterIP: "10.10.10.10",
				Ports:     []kapi.ServicePort{{Port: 80, Protocol: kapi.ProtocolTCP}},
				Type:      kapi.ServiceTypeClusterIP,
			},
		}
		client.CoreV1().Services("foo_ns").Create(context.Background(), svc, metav1.CreateOptions{})
		cache.WaitForCacheSync(ctx.Done(), serviceInformer.HasSynced)

		go c.Run(ctx.Done())

		// Controller_Event is deleted
		Eventually(
			func() int {
				ctx, _ := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				var events []sbdb.ControllerEvent
				err = sbClient.List(ctx, &events)
				Expect(err).NotTo(HaveOccurred())
				return len(events)
			},
			5*time.Second,
		).Should(Equal(0))

		timeout := time.Tick(5 * time.Second)
		select {
		case event := <-recorder.Events:
			// Recorder event is sent
			Expect(event).To(Equal("Normal NeedPods The service foo_service needs pods"))
		case <-timeout:
			Fail("did not receive controller_event event")
		}
	})

	It("should update unidled-at annotation when unidling", func() {
		client := fake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		serviceInformer := informerFactory.Core().V1().Services().Informer()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testStartTime := time.Now().Format(time.RFC3339)

		kube := &kube.Kube{
			KClient: client,
		}
		NewUnidledAtController(kube, serviceInformer)

		informerFactory.Start(ctx.Done())
		cache.WaitForCacheSync(ctx.Done(), serviceInformer.HasSynced)

		svc := &kapi.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "svc1",
			},
		}
		client.CoreV1().Services("default").Create(context.Background(), svc, metav1.CreateOptions{})

		err := kube.SetAnnotationsOnService("default", "svc1",
			map[string]interface{}{"k8s.ovn.org/idled-at": "2023-02-06T13:48:49Z"})
		Expect(err).ToNot(HaveOccurred())

		err = kube.SetAnnotationsOnService("default", "svc1",
			map[string]interface{}{"k8s.ovn.org/idled-at": nil})
		Expect(err).ToNot(HaveOccurred())

		Eventually(func(g Gomega) {
			alteredSvc, err := client.CoreV1().Services("default").Get(context.Background(), "svc1", metav1.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			unidledAt := alteredSvc.Annotations["k8s.ovn.org/unidled-at"]
			g.Expect(unidledAt).ToNot(BeNil())
			g.Expect(unidledAt >= testStartTime).To(BeTrue(), "expected %s >= %s", unidledAt, testStartTime)
		})
	})
})
