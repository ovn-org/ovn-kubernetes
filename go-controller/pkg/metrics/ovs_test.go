package metrics

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/mocks"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
)

type clientOutput struct {
	stdout string
	stderr string
	err    error
}

type fakeOVSClient struct {
	dataIndex int
	data      []clientOutput
}

func NewFakeOVSClient(data []clientOutput) fakeOVSClient {
	return fakeOVSClient{data: data}
}

func (c *fakeOVSClient) FakeCall(args ...string) (string, string, error) {
	output := c.data[c.dataIndex]
	c.dataIndex++
	return output.stdout, output.stderr, output.err
}

const (
	ovsAppctlDumpAggregateSampleOutput = "NXST_AGGREGATE reply (xid=0x4): packet_count=856244 byte_count=3464651294 flow_count=30"
	ovsVsctlListBridgeOutput           = "br-int,porta portb portc\nbr-ex,portd porte"
	ovsVsctlListInterfaceOutput        = "1,collisions=10 rx_bytes=0 rx_crc_err=0 rx_dropped=5 rx_errors=100 rx_frame_err=0 rx_missed_errors=0 rx_over_err=0 rx_packets=0 tx_bytes=0 tx_dropped=50 tx_errors=20 tx_packets=0\n1,rx_bytes=0 rx_packets=1000 tx_bytes=0 tx_packets=80\n0,collisions=10 rx_bytes=0 rx_crc_err=0 rx_dropped=5 rx_errors=100 rx_frame_err=0 rx_missed_errors=0 rx_over_err=0 rx_packets=0 tx_bytes=0 tx_dropped=50 tx_errors=20 tx_packets=0"
)

var _ = ginkgo.Describe("OVS metrics", func() {
	var stopChan chan struct{}
	var resetsTotalMock, rxDroppedTotalMock, txDroppedTotalMock *mocks.GaugeMock
	var rxErrorsTotalMock, txErrorsTotalMock, collisionsTotalMock, bridgeTotalMock *mocks.GaugeMock

	ginkgo.BeforeEach(func() {
		// replace all the prom gauges with mocks
		bridgeTotalMock = mocks.NewGaugeMock()
		metricOvsBridgeTotal = bridgeTotalMock
		stopChan = make(chan struct{})
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
	})

	ginkgo.Context("On update bridge metrics", func() {
		ginkgo.It("sets bridge metrics when input valid", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: ovsVsctlListBridgeOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsOfctlOutput := []clientOutput{
				{
					stdout: ovsAppctlDumpAggregateSampleOutput,
					stderr: "",
					err:    nil,
				},
				{
					stdout: ovsAppctlDumpAggregateSampleOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsOfctl := NewFakeOVSClient(ovsOfctlOutput)
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsOfctl.FakeCall)
			gomega.Expect(err).To(gomega.BeNil())
			// There is no easy way (that I can think of besides creating my own interface - none exist upstream) to
			// mock prometheus.gaugevec.
			// Validate the number of expected prom time series only.
			// There are two prom time series per metric expected because we input two bridges.
			ovsBridgesCh := make(chan prometheus.Metric, 20)
			defer close(ovsBridgesCh)
			metricOvsBridge.Collect(ovsBridgesCh)
			metricOvsBridgePortsTotal.Collect(ovsBridgesCh)
			metricOvsBridgeFlowsTotal.Collect(ovsBridgesCh)
			gomega.Expect(ovsBridgesCh).Should(gomega.HaveLen(6))
			gomega.Expect(bridgeTotalMock.GetValue()).Should(gomega.BeNumerically("==", 2))
		})

		ginkgo.It("returns error when OVS vsctl client returns an error", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    fmt.Errorf("could not connect to ovsdb"),
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctl := NewFakeOVSClient([]clientOutput{})
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS vsctl client returns non-blank stderr", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "big bad error",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctl := NewFakeOVSClient([]clientOutput{})
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS vsctl client returns a blank output", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctl := NewFakeOVSClient([]clientOutput{})
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS appctl client returns an error", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: ovsVsctlListBridgeOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    fmt.Errorf("bad server connection"),
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS appctl returns non-blank stderr", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: ovsVsctlListBridgeOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "very bad command",
					err:    nil,
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS appctl client returns a blank output", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: ovsVsctlListBridgeOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    nil,
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err := updateOvsBridgeMetrics(ovsVsctl.FakeCall, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})
	})

	ginkgo.Context("On update of OVS interface metrics", func() {
		ginkgo.BeforeEach(func() {
			// replace all the prom gauges with mocks
			resetsTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceResetsTotal = resetsTotalMock
			rxDroppedTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceRxDroppedTotal = rxDroppedTotalMock
			txDroppedTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceTxDroppedTotal = txDroppedTotalMock
			rxErrorsTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceRxErrorsTotal = rxErrorsTotalMock
			txErrorsTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceTxErrorsTotal = txErrorsTotalMock
			collisionsTotalMock = mocks.NewGaugeMock()
			metricOvsInterfaceCollisionsTotal = collisionsTotalMock
		})

		ginkgo.It("sets interface metrics when input is valid", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: ovsVsctlListInterfaceOutput,
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			err := updateOvsInterfaceMetrics(ovsVsctl.FakeCall)
			gomega.Expect(err).Should(gomega.BeNil())
			gomega.Expect(resetsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 2))
			gomega.Expect(rxDroppedTotalMock.GetValue()).Should(gomega.BeNumerically("==", 10))
			gomega.Expect(txDroppedTotalMock.GetValue()).Should(gomega.BeNumerically("==", 100))
			gomega.Expect(rxErrorsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 200))
			gomega.Expect(txErrorsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 40))
			gomega.Expect(collisionsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 20))
		})

		ginkgo.It("returns error when OVS vsctl client returns an error", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    fmt.Errorf("could not connect to ovsdb"),
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			err := updateOvsInterfaceMetrics(ovsVsctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS vsctl client returns non-blank stderr", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    fmt.Errorf("could not connect to ovsdb"),
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			err := updateOvsInterfaceMetrics(ovsVsctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})

		ginkgo.It("returns error when OVS vsctl client returns a blank output", func() {
			ovsVsctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    nil,
				},
			}
			ovsVsctl := NewFakeOVSClient(ovsVsctlOutput)
			err := updateOvsInterfaceMetrics(ovsVsctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
		})
	})
})
