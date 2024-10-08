package metrics

import (
	"fmt"
	"math"
	"time"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclientgo "k8s.io/client-go/kubernetes/fake"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func setupOvn(nbData libovsdbtest.TestSetup) (client.Client, client.Client, *libovsdbtest.Context) {
	nbClient, sbClient, cleanup, err := libovsdbtest.NewNBSBTestHarness(nbData)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return sbClient, nbClient, cleanup
}

func getKubeClient(nodeCount int) *kube.Kube {
	var nodes []corev1.Node
	for i := 0; i < nodeCount; i++ {
		nodes = append(nodes, corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("nodeName-%d", i),
			},
		})
	}
	kubeFakeClient := fakeclientgo.NewSimpleClientset(&corev1.NodeList{Items: nodes})
	return &kube.Kube{KClient: kubeFakeClient}
}

func setHvCfg(nbClient client.Client, hvCfg int, hvCfgTimestamp time.Time) {
	nbGlobal := nbdb.NBGlobal{}
	nbGlobalResp, err := libovsdbops.GetNBGlobal(nbClient, &nbGlobal)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	nbGlobalResp.HvCfg = hvCfg
	nbGlobalResp.HvCfgTimestamp = int(hvCfgTimestamp.UnixMilli())
	ops, err := nbClient.Where(nbGlobalResp).Update(nbGlobalResp, &nbGlobalResp.HvCfg, &nbGlobalResp.HvCfgTimestamp)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(ops).To(gomega.HaveLen(1))
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

var _ = ginkgo.Describe("Config Duration Operations", func() {
	var (
		instance       *ConfigDurationRecorder
		k              *kube.Kube
		nbClient       client.Client
		cleanup        *libovsdbtest.Context
		stop           chan struct{}
		testNamespaceA = "testnamespacea"
		testPodNameA   = "testpoda"
		testNamespaceB = "testnamespaceb"
		testPodNameB   = "testpodb"
	)

	ginkgo.BeforeEach(func() {
		cdr = nil
		instance = GetConfigDurationRecorder()
		k = getKubeClient(1)
		stop = make(chan struct{})
		_, nbClient, cleanup = setupOvn(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{&nbdb.NBGlobal{UUID: "cd-op-uuid"}}})
	})

	ginkgo.AfterEach(func() {
		cleanup.Cleanup()
		close(stop)
	})

	ginkgo.Context("Runtime", func() {
		ginkgo.It("records correctly", func() {
			instance.Run(nbClient, k, 0, time.Millisecond, stop)
			histoMock := mocks.NewHistogramVecMock()
			metricNetworkProgramming = histoMock
			startTimestamp, ok := instance.Start("pod", testNamespaceA, testPodNameA)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(startTimestamp.IsZero()).Should(gomega.BeFalse())
			endTimestamp := instance.End("pod", testNamespaceA, testPodNameA)
			var histoValue float64
			gomega.Eventually(func() bool {
				select {
				case histoValue = <-histoMock.GetCh():
					return true
				default:
					return false
				}
			}).Should(gomega.BeTrue())
			delta := endTimestamp.Sub(startTimestamp).Seconds()
			gomega.Expect(math.Round(histoValue)).Should(gomega.BeNumerically("==", math.Round(delta)))
			histoMock.Cleanup()
		})

		ginkgo.It("records correctly with OVN latency", func() {
			instance.Run(nbClient, k, 0, time.Millisecond, stop)
			histoMock := mocks.NewHistogramVecMock()
			metricNetworkProgramming = histoMock
			startTimestamp, ok := instance.Start("pod", testNamespaceA, testPodNameA)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(startTimestamp.IsZero()).Should(gomega.BeFalse())
			ops, txOkCallback, startOVNTimestamp, err := instance.AddOVN(nbClient, "pod", testNamespaceA, testPodNameA)
			gomega.Expect(ops).Should(gomega.HaveLen(1))
			gomega.Expect(err).Should(gomega.BeNil())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			txOkCallback()
			endTimestamp := instance.End("pod", testNamespaceA, testPodNameA)
			endOVNTimestamp := startOVNTimestamp.Add(10 * time.Second)
			setHvCfg(nbClient, 1, endOVNTimestamp)
			delta := endOVNTimestamp.Sub(startOVNTimestamp).Seconds() + startTimestamp.Sub(endTimestamp).Seconds()
			var histoValue float64
			gomega.Eventually(func() bool {
				select {
				case histoValue = <-histoMock.GetCh():
					return true
				default:
					return false
				}
			}).Should(gomega.BeTrue())
			gomega.Expect(math.Round(histoValue)).Should(gomega.BeNumerically("==", math.Round(delta)))
			histoMock.Cleanup()
		})

		ginkgo.It("records multiple different objs including adding OVN latency", func() {
			instance.Run(nbClient, k, 0, time.Millisecond, stop)
			histoMock := mocks.NewHistogramVecMock()
			metricNetworkProgramming = histoMock
			// recording 1
			startTimestamp, ok := instance.Start("pod", testNamespaceA, testPodNameA)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(startTimestamp.IsZero()).Should(gomega.BeFalse())
			ops, txOkCallback, startOVNTimestamp, err := instance.AddOVN(nbClient, "pod", testNamespaceA, testPodNameA)
			gomega.Expect(ops).Should(gomega.HaveLen(1))
			gomega.Expect(err).Should(gomega.BeNil())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			txOkCallback()
			endTimestamp := instance.End("pod", testNamespaceA, testPodNameA)
			endOVNTimestamp := startOVNTimestamp.Add(10 * time.Second)
			setHvCfg(nbClient, 1, endOVNTimestamp)
			deltaFirstObj := endOVNTimestamp.Sub(startOVNTimestamp).Seconds() + startTimestamp.Sub(endTimestamp).Seconds()
			// recording 2 with different obj
			startTimestamp, ok = instance.Start("networkpolicy", testNamespaceB, testPodNameB)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(startTimestamp.IsZero()).Should(gomega.BeFalse())
			ops, txOkCallback, startOVNTimestamp, err = instance.AddOVN(nbClient, "networkpolicy", testNamespaceB, testPodNameB)
			gomega.Expect(ops).Should(gomega.HaveLen(1))
			gomega.Expect(err).Should(gomega.BeNil())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			txOkCallback()
			endTimestamp = instance.End("networkpolicy", testNamespaceB, testPodNameB)
			endOVNTimestamp = startOVNTimestamp.Add(10 * time.Second)
			setHvCfg(nbClient, 2, endOVNTimestamp)
			deltaSecondObj := endOVNTimestamp.Sub(startOVNTimestamp).Seconds() + startTimestamp.Sub(endTimestamp).Seconds()
			var histoValue float64
			gomega.Eventually(func() bool {
				if len(histoMock.GetCh()) != 2 {
					return false
				}
				var tmp float64
				for i := 0; i < 2; i++ {
					select {
					case tmp = <-histoMock.GetCh():
						histoValue += tmp
					default:
						return false
					}
				}
				return true
			}).Should(gomega.BeTrue())
			gomega.Expect(math.Round(histoValue)).Should(gomega.BeNumerically("==", math.Round(deltaFirstObj+deltaSecondObj)))
			histoMock.Cleanup()
		})

		ginkgo.It("denies recording when no start called", func() {
			instance.Run(nbClient, k, 0, time.Millisecond, stop)
			ops, _, _, _ := instance.AddOVN(nbClient, "pod", testNamespaceA, testPodNameA)
			gomega.Expect(ops).Should(gomega.HaveLen(0))
		})

		ginkgo.It("allows multiple addOVN records for the same obj", func() {
			instance.Run(nbClient, k, 0, time.Millisecond, stop)
			histoMock := mocks.NewHistogramVecMock()
			metricNetworkProgramming = histoMock
			// recording 1
			startTimestamp, ok := instance.Start("pod", testNamespaceA, testPodNameA)
			gomega.Expect(ok).To(gomega.BeTrue())
			gomega.Expect(startTimestamp.IsZero()).Should(gomega.BeFalse())
			// first addOVN
			ops, txOkCallback, firstStartOVNTimestamp, err := instance.AddOVN(nbClient, "pod", testNamespaceA, testPodNameA)
			gomega.Expect(ops).Should(gomega.HaveLen(1))
			gomega.Expect(err).Should(gomega.BeNil())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			txOkCallback()
			// second addOVN
			ops, txOkCallback, secondStartOVNTimestamp, err := instance.AddOVN(nbClient, "pod", testNamespaceA, testPodNameA)
			gomega.Expect(ops).Should(gomega.HaveLen(1))
			gomega.Expect(err).Should(gomega.BeNil())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			txOkCallback()
			endTimestamp := instance.End("pod", testNamespaceA, testPodNameA)
			endOVNTimestamp := secondStartOVNTimestamp.Add(10 * time.Second)
			setHvCfg(nbClient, 2, endOVNTimestamp)
			firstOVNDelta := endOVNTimestamp.Sub(firstStartOVNTimestamp).Seconds()
			secondOVNDelta := endOVNTimestamp.Sub(secondStartOVNTimestamp).Seconds()
			delta := endTimestamp.Sub(startTimestamp).Seconds() + firstOVNDelta + secondOVNDelta
			var histoValue float64
			gomega.Eventually(func() bool {
				if len(histoMock.GetCh()) != 1 {
					return false
				}
				select {
				case histoValue = <-histoMock.GetCh():
					return true
				default:
					return false
				}
			}).Should(gomega.BeTrue())
			gomega.Expect(math.Round(histoValue)).Should(gomega.BeNumerically("==", math.Round(delta)))
			histoMock.Cleanup()
		})
	})
})
