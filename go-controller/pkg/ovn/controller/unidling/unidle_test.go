package unidling

import (
	"testing"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"golang.org/x/net/context"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestUnidlingContoller(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Unilding Controller Suite")
	defer GinkgoRecover()
}

var _ = Describe("Unidling Controller", func() {
	var cleanup *libovsdbtest.Cleanup

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
		stopCh := make(chan struct{})
		defer close(stopCh)

		var sbClient *libovsdb.Client
		var err error
		sbClient, cleanup, err = libovsdbtest.NewSBTestHarness(testSetup)
		Expect(err).NotTo(HaveOccurred())
		err = sbClient.Run()
		Expect(err).NotTo(HaveOccurred())

		config.OvnSouth.Scheme = config.OvnDBSchemeTCP
		config.OvnSouth.Address = "tcp::56640"

		c, err := NewController(
			recorder,
			informerFactory.Core().V1().Services().Informer(),
			sbClient,
		)
		Expect(err).NotTo(HaveOccurred())
		c.AddServiceVIPToName("10.10.10.10:80", kapi.ProtocolTCP, "foo_ns", "foo_service")
		go c.Run(stopCh)

		// Controller_Event is deleted
		Eventually(
			func() int {
				ctx, _ := context.WithTimeout(context.Background(), types.OVSDBTimeout)
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
})
