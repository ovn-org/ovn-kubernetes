package metrics

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovsops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
	"github.com/prometheus/client_golang/prometheus"
)

// ovnController Configuration metrics
var metricRemoteProbeInterval = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "remote_probe_interval_seconds",
	Help:      "The inactivity probe interval of the connection to the OVN SB DB.",
})

var metricOpenFlowProbeInterval = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "openflow_probe_interval_seconds",
	Help: "The inactivity probe interval of the OpenFlow connection to the " +
		"OpenvSwitch integration bridge.",
})

var metricMonitorAll = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "monitor_all",
	Help: "Specifies if ovn-controller should monitor all records of tables in OVN SB DB. " +
		"If set to false, it will conditionally monitor the records that " +
		"is needed in the current chassis. Values are false(0), true(1).",
})

var metricEncapIP = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "encap_ip",
	Help: "A metric with a constant '1' value labeled by ipadress that " +
		"specifies the encapsulation ip address configured on that node.",
},
	[]string{
		"ipaddress",
	},
)

var metricSbConnectionMethod = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "sb_connection_method",
	Help: "A metric with a constant '1' value labeled by connection_method that " +
		"specifies the ovn-remote value configured on that node.",
},
	[]string{
		"connection_method",
	},
)

var metricEncapType = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "encap_type",
	Help: "A metric with a constant '1' value labeled by type that " +
		"specifies the encapsulation type a chassis should use to " +
		"connect to this node.",
},
	[]string{
		"type",
	},
)

var metricBridgeMappings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "bridge_mappings",
	Help: "A metric with a constant '1' value labeled by mapping that " +
		"specifies list of key-value pairs that map a physical network name " +
		"to a local ovs bridge that provides connectivity to that network.",
},
	[]string{
		"mapping",
	},
)

var metricOVNControllerSBDBConnection = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemController,
	Name:      "southbound_database_connected",
	Help:      "Specifies if OVN controller is connected to OVN southbound database (1) or not (0)",
})

var (
	ovnControllerVersion       string
	ovnControllerOvsLibVersion string
)

func getOvnControllerVersionInfo() {
	stdout, _, err := util.RunOVNControllerAppCtl("version")
	if err != nil {
		return
	}

	// the output looks something like this:
	// ovn-controller 20.06.0.86f64fc1
	// Open vSwitch Library 2.13.0.f945b5c5
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "ovn-controller ") {
			ovnControllerVersion = strings.Fields(line)[1]
		} else if strings.HasPrefix(line, "Open vSwitch Library ") {
			ovnControllerOvsLibVersion = strings.Fields(line)[3]
		}
	}
}

var ovnControllerCoverageShowMetricsMap = map[string]*metricDetails{
	"lflow_run": {
		help: "Number of times ovn-controller has translated " +
			"the Logical_Flow table in the OVN " +
			"SB database into OpenFlow flows.",
	},
	"rconn_sent": {
		help: "Specifies the number of messages " +
			"that have been sent to the underlying virtual " +
			"connection (unix, tcp, or ssl) to OpenFlow devices.",
	},
	"rconn_queued": {
		help: "Specifies the number of messages that have been " +
			"queued because it couldn’t be sent using the " +
			"underlying virtual connection to OpenFlow devices.",
	},
	"rconn_discarded": {
		help: "Specifies the number of messages that " +
			"have been dropped because the send queue " +
			"had to be flushed because of reconnection.",
	},
	"rconn_overflow": {
		help: "Specifies the number of messages that have " +
			"been dropped because of the queue overflow.",
	},
	"vconn_open": {
		help: "Specifies the number of attempts to connect " +
			"to an OpenFlow Device.",
	},
	"vconn_sent": {
		help: "Specifies the number of messages sent " +
			"to the OpenFlow Device.",
	},
	"vconn_received": {
		help: "Specifies the number of messages received " +
			"from the OpenFlow Device.",
	},
	"stream_open": {
		help: "Specifies the number of attempts to connect " +
			"to a remote peer (active connection).",
	},
	"txn_success": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has successfully completed.",
	},
	"txn_error": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has errored out.",
	},
	"txn_uncommitted": {
		help: "Specifies the number of times the OVSDB " +
			"transaction were uncommitted.",
	},
	"txn_unchanged": {
		help: "Specifies the number of times the OVSDB transaction " +
			"resulted in no change to the database.",
	},
	"txn_incomplete": {
		help: "Specifies the number of times the OVSDB transaction " +
			"did not complete and the client had to re-try.",
	},
	"txn_aborted": {
		help: "Specifies the number of times the OVSDB " +
			" transaction has been aborted.",
	},
	"txn_try_again": {
		help: "Specifies the number of times the OVSDB " +
			"transaction failed and the client had to re-try.",
	},
	"netlink_sent": {
		help: "Number of netlink message sent to the kernel.",
	},
	"netlink_received": {
		help: "Number of netlink messages received by the kernel.",
	},
	"netlink_recv_jumbo": {
		help: "Number of netlink messages that were received from " +
			"the kernel were more than the allocated buffer.",
	},
	"netlink_overflow": {
		help: "Netlink messages dropped by the daemon due " +
			"to buffer overflow.",
	},
	"packet_in": {
		srcName: "flow_extract",
		help: "Specifies the number of times ovn-controller has " +
			"handled the packet-ins from ovs-vswitchd.",
	},
	"packet_in_drop": {
		aggregateFrom: []string{
			"pinctrl_drop_put_mac_binding",
			"pinctrl_drop_buffered_packets_map",
			"pinctrl_drop_controller_event",
			"pinctrl_drop_put_vport_binding",
		},
		help: "Specifies the number of times the ovn-controller has dropped the " +
			"packet-ins from ovs-vswitchd due to resource constraints",
	},
}

var ovnControllerStopwatchShowMetricsMap = map[string]*stopwatchMetricDetails{
	"bfd_run": {
		srcName: "bfd-run",
	},
	"flow_installation": {
		srcName: "flow-installation",
	},
	"if_status_mgr_run": {
		srcName: "if-status-mgr-run",
	},
	"if_status_mgr_update": {
		srcName: "if-status-mgr-update",
	},
	"flow_generation": {
		srcName: "flow-generation",
	},
	"pinctrl_run": {
		srcName: "pinctrl-run",
	},
	"ofctrl_seqno_run": {
		srcName: "ofctrl-seqno-run",
	},
	"patch_run": {
		srcName: "patch-run",
	},
	"ct_zone_commit": {
		srcName: "ct-zone-commit",
	},
}

// setOvnControllerConfigurationMetrics updates ovn-controller configuration
// values (ovn-openflow-probe-interval, ovn-remote-probe-interval, ovn-monitor-all,
// ovn-encap-ip, ovn-encap-type, ovn-remote) obtained from Open_vSwitch entry updates
func setOvnControllerConfigurationMetrics(ovsDBClient libovsdbclient.Client) (err error) {
	openvSwitch, err := ovsops.GetOpenvSwitch(ovsDBClient)
	if err != nil {
		return fmt.Errorf("failed to get Open_vSwitch entry (%v)", err)
	}

	openflowProbeField := openvSwitch.ExternalIDs["ovn-openflow-probe-interval"]
	openflowProbeVal := parseMetricToFloat(MetricOvnSubsystemController, "ovn-openflow-probe-interval", openflowProbeField)
	metricOpenFlowProbeInterval.Set(openflowProbeVal)

	remoteProbeField := openvSwitch.ExternalIDs["ovn-remote-probe-interval"]
	remoteProbeValue := parseMetricToFloat(MetricOvnSubsystemController, "ovn-remote-probe-interval", remoteProbeField)
	metricRemoteProbeInterval.Set(remoteProbeValue / 1000)

	var ovnMonitorValue float64
	ovnMonitorField := openvSwitch.ExternalIDs["ovn-monitor-all"]
	if ovnMonitorField == "true" {
		ovnMonitorValue = 1
	}
	metricMonitorAll.Set(ovnMonitorValue)

	// To update not only values but also labels for metrics, we use Reset() to delete previous labels+value
	encapIPValue := openvSwitch.ExternalIDs["ovn-encap-ip"]
	metricEncapIP.Reset()
	metricEncapIP.WithLabelValues(encapIPValue).Set(1)

	encapTypeValue := openvSwitch.ExternalIDs["ovn-encap-type"]
	metricEncapType.Reset()
	metricEncapType.WithLabelValues(encapTypeValue).Set(1)

	ovnRemoteValue := openvSwitch.ExternalIDs["ovn-remote"]
	metricSbConnectionMethod.Reset()
	metricSbConnectionMethod.WithLabelValues(ovnRemoteValue).Set(1)

	var ovnNodePortValue = 1
	nodePortField := openvSwitch.ExternalIDs["ovn-k8s-node-port"]
	if nodePortField == "false" {
		ovnNodePortValue = 0
	}
	metricOvnNodePortEnabled.Set(float64(ovnNodePortValue))

	bridgeMappingValue := openvSwitch.ExternalIDs["ovn-bridge-mappings"]
	metricBridgeMappings.Reset()
	metricBridgeMappings.WithLabelValues(bridgeMappingValue).Set(1)

	return nil
}

func ovnControllerConfigurationMetricsUpdater(ovsDBClient libovsdbclient.Client, metricsScrapeInterval int,
	stopChan <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(metricsScrapeInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := setOvnControllerConfigurationMetrics(ovsDBClient); err != nil {
				klog.Errorf("Setting ovn controller config metrics failed: %s", err.Error())
			}
		case <-stopChan:
			return
		}
	}
}

func getPortCount(ovsDBClient libovsdbclient.Client, portType string) float64 {
	var portCount float64
	p := func(item *vswitchd.Interface) bool {
		return item.Type == portType
	}

	intfList, err := ovsops.FindInterfacesWithPredicate(ovsDBClient, p)
	if err != nil {
		klog.Errorf("Failed to get %s interface count: %v", portType, err)
		return 0
	}
	if portType == "patch" {
		for _, intf := range intfList {
			if strings.Contains(intf.Name, "br-int") {
				portCount++
			}

		}
	} else {
		portCount = float64(len(intfList))
	}

	return portCount
}

// ovnControllerSBDBConnectionCheckUpdater blocks until stopCh closed but before then polls ovn-controllers connection status with
// southbound database periodically.
func ovnControllerSBDBConnectionCheckUpdater(stopCh <-chan struct{}, ovsAppctl ovsClient, period time.Duration) {
	// There maybe transient connection issues to SB DB. We want to minimise the risk of reporting this as the current state between
	// long poll intervals.
	retry := 5
	retrySleep := 5 * time.Second
	retryTotal := retrySleep * time.Duration(retry)

	if retryTotal >= period {
		panic("period must be greater than retry total time")
	}
	// update metric to a good initial state
	updateSBDBConnectionMetric(ovsAppctl, retry, retrySleep)

	ticker := time.NewTicker(period)
	for {
		select {
		case <-ticker.C:
			updateSBDBConnectionMetric(ovsAppctl, retry, retrySleep)
		case <-stopCh:
			ticker.Stop()
			return
		}
	}
}

func updateSBDBConnectionMetric(ovsAppctl ovsClient, retry int, retrySleep time.Duration) {
	var stdOut, stdErr string
	var err error
	var connected bool
	connected = false
	for i := 0; i < retry && !connected; i++ {
		stdOut, stdErr, err = ovsAppctl("connection-status")
		if err != nil {
			klog.Errorf("Failed to get OVN controller southbound database connection status before utilizing "+
				"client ovs-appctl: %v", err)
		} else if stdErr != "" {
			klog.Errorf("Failed to get OVN controller southbound database connection status because "+
				"ovs-appctl command returned an error: %s", stdErr)
		} else if stdOut == "" {
			klog.Errorf("Unexpected blank output while attempting to retrieve OVN controller southbound " +
				"database connection status")
		} else if strings.HasPrefix(stdOut, "connected") {
			connected = true
		} else {
			// sleep and retry
			time.Sleep(retrySleep)
		}
	}

	if connected {
		metricOVNControllerSBDBConnection.Set(1)
	} else {
		metricOVNControllerSBDBConnection.Set(0)
	}
}

func RegisterOvnControllerMetrics(ovsDBClient libovsdbclient.Client,
	metricsScrapeInterval int, stopChan <-chan struct{}) {
	getOvnControllerVersionInfo()
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemController,
			Name:      "build_info",
			Help: "A metric with a constant '1' value labeled by version and library " +
				"from which ovn binaries were built",
			ConstLabels: prometheus.Labels{
				"version":         ovnControllerVersion,
				"ovs_lib_version": ovnControllerOvsLibVersion,
			},
		},
		func() float64 { return 1 },
	))

	// ovn-controller metrics
	ovnRegistry.MustRegister(metricOVNControllerSBDBConnection)
	ovnRegistry.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemController,
			Name:      "integration_bridge_openflow_total",
			Help:      "The total number of OpenFlow flows in the integration bridge.",
		}, func() float64 {
			stdout, stderr, err := util.RunOVSOfctl("-t", "5", "dump-aggregate", "br-int")
			if err != nil {
				klog.Errorf("Failed to get flow count for br-int, stderr(%s): (%v)",
					stderr, err)
				return 0
			}
			for _, kvPair := range strings.Fields(stdout) {
				if strings.HasPrefix(kvPair, "flow_count=") {
					value := strings.Split(kvPair, "=")[1]
					return parseMetricToFloat(MetricOvnSubsystemController, "integration_bridge_openflow_total",
						value)
				}
			}
			return 0
		}))
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemController,
			Name:      "integration_bridge_patch_ports",
			Help: "Captures the number of patch ports that connect br-int OVS " +
				"bridge to physical OVS bridge and br-local OVS bridge.",
		},
		func() float64 {
			return getPortCount(ovsDBClient, "patch")
		}))
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemController,
			Name:      "integration_bridge_geneve_ports",
			Help:      "Captures the number of geneve ports that are on br-int OVS bridge.",
		},
		func() float64 {
			return getPortCount(ovsDBClient, "geneve")
		}))

	// register ovn-controller configuration metrics
	ovnRegistry.MustRegister(metricRemoteProbeInterval)
	ovnRegistry.MustRegister(metricOpenFlowProbeInterval)
	ovnRegistry.MustRegister(metricMonitorAll)
	ovnRegistry.MustRegister(metricEncapIP)
	ovnRegistry.MustRegister(metricSbConnectionMethod)
	ovnRegistry.MustRegister(metricEncapType)
	ovnRegistry.MustRegister(metricBridgeMappings)
	// Register the ovn-controller coverage/show metrics
	componentCoverageShowMetricsMap[ovnController] = ovnControllerCoverageShowMetricsMap
	registerCoverageShowMetrics(ovnController, MetricOvnNamespace, MetricOvnSubsystemController)

	// Register the ovn-controller coverage/show metrics
	componentStopwatchShowMetricsMap[ovnController] = ovnControllerStopwatchShowMetricsMap
	registerStopwatchShowMetrics(ovnController, MetricOvnNamespace, MetricOvnSubsystemController)

	// ovn-controller configuration metrics updater
	go ovnControllerConfigurationMetricsUpdater(ovsDBClient,
		metricsScrapeInterval, stopChan)
	// ovn-controller coverage show metrics updater
	go coverageShowMetricsUpdater(ovnController, stopChan)
	// ovn-controller stopwatch show metrics updater
	go stopwatchShowMetricsUpdater(ovnController, stopChan)
	// ovn-controller southbound database connection status updater
	go ovnControllerSBDBConnectionCheckUpdater(stopChan, util.RunOVNControllerAppCtl, time.Minute*2)
}
