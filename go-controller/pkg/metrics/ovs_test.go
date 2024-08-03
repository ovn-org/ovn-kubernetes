package metrics

import (
	"fmt"
	"sync/atomic"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/mocks"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cryptorand"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
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

// buildNamedUUID builds an id that can be used as a named-uuid
func buildUUID() string {
	namedUUIDPrefix := 'u'
	namedUUIDCounter := cryptorand.Uint32()
	return fmt.Sprintf("%c%010d", namedUUIDPrefix, atomic.AddUint32(&namedUUIDCounter, 1))
}

const (
	ovsAppctlDumpAggregateSampleOutput = "NXST_AGGREGATE reply (xid=0x4): packet_count=856244 byte_count=3464651294 flow_count=30"
)

var _ = ginkgo.Describe("OVS metrics", func() {
	var stopChan chan struct{}
	var resetsTotalMock, rxDroppedTotalMock, txDroppedTotalMock *mocks.GaugeMock
	var rxErrorsTotalMock, txErrorsTotalMock, collisionsTotalMock, bridgeTotalMock *mocks.GaugeMock
	var hwOffloadMock, tcPolicyMock *mocks.GaugeMock

	linkResets := 1
	intf1 := vswitchd.Interface{Name: "porta", UUID: buildUUID()}
	intf2 := vswitchd.Interface{Name: "portb", UUID: buildUUID()}
	intf3 := vswitchd.Interface{Name: "portc", UUID: buildUUID()}
	intf4 := vswitchd.Interface{Name: "portd", UUID: buildUUID()}
	intf5 := vswitchd.Interface{Name: "porte", UUID: buildUUID()}
	port1 := vswitchd.Port{Name: "porta", UUID: buildUUID()}
	port2 := vswitchd.Port{Name: "portb", UUID: buildUUID()}
	port3 := vswitchd.Port{Name: "portc", UUID: buildUUID()}
	port4 := vswitchd.Port{Name: "portd", UUID: buildUUID()}
	port5 := vswitchd.Port{Name: "porte", UUID: buildUUID()}
	br1 := vswitchd.Bridge{Name: "br-int", UUID: buildUUID()}
	br2 := vswitchd.Bridge{Name: "br-ex", UUID: buildUUID()}

	testDB := []libovsdbtest.TestData{
		&vswitchd.Interface{UUID: intf1.UUID, Name: intf1.Name,
			LinkResets: &linkResets, Statistics: map[string]int{"collisions": 10,
				"rx_bytes": 0, "rx_crc_err": 0, "rx_dropped": 5, "rx_errors": 100,
				"rx_frame_err": 0, "rx_missed_errors": 0, "rx_over_err": 0, "rx_packets": 0,
				"tx_bytes": 0, "tx_dropped": 50, "tx_errors": 20, "tx_packets": 0}},
		&vswitchd.Interface{UUID: intf2.UUID, Name: intf2.Name,
			LinkResets: &linkResets, Statistics: map[string]int{"rx_bytes": 0,
				"rx_packets": 1000, "tx_bytes": 0, "tx_packets": 80}},
		&vswitchd.Interface{UUID: intf3.UUID, Name: intf3.Name,
			Statistics: map[string]int{"collisions": 10,
				"rx_bytes": 0, "rx_crc_err": 0, "rx_dropped": 5, "rx_errors": 100,
				"rx_frame_err": 0, "rx_missed_errors": 0, "rx_over_err": 0, "rx_packets": 0,
				"tx_bytes": 0, "tx_dropped": 50, "tx_errors": 20, "tx_packets": 0}},
		&vswitchd.Interface{UUID: intf4.UUID, Name: intf4.Name},
		&vswitchd.Interface{UUID: intf5.UUID, Name: intf5.Name},
		&vswitchd.Port{UUID: port1.UUID, Name: port1.Name,
			Interfaces: []string{intf1.UUID}},
		&vswitchd.Port{UUID: port2.UUID, Name: port2.Name,
			Interfaces: []string{intf2.UUID}},
		&vswitchd.Port{UUID: port3.UUID, Name: port3.Name,
			Interfaces: []string{intf3.UUID}},
		&vswitchd.Port{UUID: port4.UUID, Name: port4.Name,
			Interfaces: []string{intf4.UUID}},
		&vswitchd.Port{UUID: port5.UUID, Name: port5.Name,
			Interfaces: []string{intf5.UUID}},
		&vswitchd.Bridge{UUID: br1.UUID, Name: br1.Name, Ports: []string{port1.UUID, port2.UUID, port3.UUID}},
		&vswitchd.Bridge{UUID: br2.UUID, Name: br2.Name, Ports: []string{port4.UUID, port5.UUID}},
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{br1.UUID, br2.UUID}},
	}
	dbSetup := libovsdbtest.TestSetup{
		OVSData: testDB,
	}

	ginkgo.BeforeEach(func() {
		// replace all the prom gauges with mocks
		bridgeTotalMock = mocks.NewGaugeMock()
		metricOvsBridgeTotal = bridgeTotalMock
		stopChan = make(chan struct{})
	})

	ginkgo.AfterEach(func() {
		close(stopChan)
	})

	ginkgo.Context("On update of bridge metrics", func() {
		ginkgo.It("sets bridge metrics when input valid", func() {

			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
			err = updateOvsBridgeMetrics(ovsClient, ovsOfctl.FakeCall)
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
			libovsdbCleanup.Cleanup()
		})

		ginkgo.It("returns error when OVS appctl client returns an error", func() {
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    fmt.Errorf("bad server connection"),
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err = updateOvsBridgeMetrics(ovsClient, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
			libovsdbCleanup.Cleanup()
		})

		ginkgo.It("returns error when OVS appctl returns non-blank stderr", func() {
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "very bad command",
					err:    nil,
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err = updateOvsBridgeMetrics(ovsClient, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
			libovsdbCleanup.Cleanup()
		})

		ginkgo.It("returns error when OVS appctl client returns a blank output", func() {
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ovsAppctlOutput := []clientOutput{
				{
					stdout: "",
					stderr: "",
					err:    nil,
				},
			}
			ovsAppctl := NewFakeOVSClient(ovsAppctlOutput)
			err = updateOvsBridgeMetrics(ovsClient, ovsAppctl.FakeCall)
			gomega.Expect(err).ToNot(gomega.BeNil())
			libovsdbCleanup.Cleanup()
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
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = updateOvsInterfaceMetrics(ovsClient)
			gomega.Expect(err).Should(gomega.BeNil())
			gomega.Expect(resetsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 2))
			gomega.Expect(rxDroppedTotalMock.GetValue()).Should(gomega.BeNumerically("==", 10))
			gomega.Expect(txDroppedTotalMock.GetValue()).Should(gomega.BeNumerically("==", 100))
			gomega.Expect(rxErrorsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 200))
			gomega.Expect(txErrorsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 40))
			gomega.Expect(collisionsTotalMock.GetValue()).Should(gomega.BeNumerically("==", 20))
			libovsdbCleanup.Cleanup()
		})
	})

	ginkgo.Context("On update of OVS HwOffload metrics", func() {
		ginkgo.BeforeEach(func() {
			// replace all the prom gauges with mocks
			hwOffloadMock = mocks.NewGaugeMock()
			metricOvsHwOffload = hwOffloadMock
			tcPolicyMock = mocks.NewGaugeMock()
			metricOvsTcPolicy = tcPolicyMock
		})

		ginkgo.It("sets Hw offload metrics when input is valid", func() {
			testDB := []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", OtherConfig: map[string]string{
					"hw-offload": "true", "tc-policy": "skip_sw"}},
			}

			dbSetup := libovsdbtest.TestSetup{
				OVSData: testDB,
			}
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = setOvsHwOffloadMetrics(ovsClient)
			gomega.Expect(err).Should(gomega.BeNil())
			gomega.Expect(hwOffloadMock.GetValue()).Should(gomega.BeNumerically("==", 1))
			gomega.Expect(tcPolicyMock.GetValue()).Should(gomega.BeNumerically("==", 1))
			libovsdbCleanup.Cleanup()
		})
		ginkgo.It("returns error when openvswitch table is not found", func() {
			testDB := []libovsdbtest.TestData{}

			dbSetup := libovsdbtest.TestSetup{
				OVSData: testDB,
			}
			ovsClient, libovsdbCleanup, err := libovsdbtest.NewOVSTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = setOvsHwOffloadMetrics(ovsClient)
			gomega.Expect(err).ToNot(gomega.BeNil())
			libovsdbCleanup.Cleanup()
		})
	})
})
