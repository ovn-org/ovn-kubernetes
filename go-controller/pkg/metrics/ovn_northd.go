// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	nbConnectionStatus = "nb-connection-status"
	sbConnectionStatus = "sb-connection-status"
)

var (
	ovnNorthdVersion       string
	ovnNorthdOvsLibVersion string
)

var metricOvnNorthdStatus = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemNorthd,
	Name:      "status",
	Help:      "Specifies whether this instance of ovn-northd is standby(0) or active(1) or paused(2).",
})

var metricOvnNorthdNbConnectionStatus = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemNorthd,
	Name:      "nb_connection_status",
	Help:      "Specifies nb-connection-status of ovn-northd, not connected(0) or connected(1).",
})

var metricOvnNorthdSbConnectionStatus = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemNorthd,
	Name:      "sb_connection_status",
	Help:      "Specifies sb-connection-status of ovn-northd, not connected(0) or connected(1).",
})

func getOvnNorthdStatusInfo() float64 {
	stdout, stderr, err := util.RunOVNNorthAppCtlWithTimeout(metricsAppctlTimeout, "status")
	if err != nil {
		klog.Errorf("Failed to get ovn-northd status stderr(%s) :(%v)", stderr, err)
		return -1
	}
	northdStatusMap := map[string]float64{
		"standby": 0,
		"active":  1,
		"paused":  2,
	}
	if strings.HasPrefix(stdout, "Status:") {
		output := strings.TrimSpace(strings.Split(stdout, ":")[1])
		if value, ok := northdStatusMap[output]; ok {
			return value
		}
	}
	return -1
}

// updateOvnNorthdStatusMetrics refreshes the ovn-northd status and connection
// metrics by exec-ing ovn-appctl.
func updateOvnNorthdStatusMetrics() {
	metricOvnNorthdStatus.Set(getOvnNorthdStatusInfo())
	metricOvnNorthdNbConnectionStatus.Set(getOvnNorthdConnectionStatusInfo(nbConnectionStatus))
	metricOvnNorthdSbConnectionStatus.Set(getOvnNorthdConnectionStatusInfo(sbConnectionStatus))
}

func getOvnNorthdVersionInfo() {
	stdout, _, err := util.RunOVNNorthAppCtl("version")
	if err != nil {
		return
	}

	// the output looks like:
	// ovn-northd 20.06.0.86f64fc1
	// Open vSwitch Library 2.13.0.f945b5c5
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "ovn-northd ") {
			ovnNorthdVersion = strings.Fields(line)[1]
		} else if strings.HasPrefix(line, "Open vSwitch Library ") {
			ovnNorthdOvsLibVersion = strings.Fields(line)[3]
		}
	}
}

func getOvnNorthdConnectionStatusInfo(command string) float64 {
	stdout, stderr, err := util.RunOVNNorthAppCtlWithTimeout(metricsAppctlTimeout, command)
	if err != nil {
		klog.Errorf("Failed to get ovn-northd %s stderr(%s): (%v)", command, stderr, err)
		return -1
	}
	connectionStatusMap := map[string]float64{
		"not connected": 0,
		"connected":     1,
	}
	if value, ok := connectionStatusMap[stdout]; ok {
		return value
	}
	return -1
}

var ovnNorthdCoverageShowMetricsMap = map[string]*metricDetails{
	"pstream_open": {
		help: "Specifies the number of time passive connections " +
			"were opened for the remote peer to connect.",
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
}

var ovnNorthdStopwatchShowMetricsMap = map[string]*stopwatchMetricDetails{
	"ovnnb_db_run":    {},
	"build_flows_ctx": {},
	"ovn_northd_loop": {
		srcName: "ovn-northd-loop",
	},
	"build_lflows":     {},
	"lflows_lbs":       {},
	"clear_lflows_ctx": {},
	"lflows_ports":     {},
	"lflows_dp_groups": {},
	"lflows_datapaths": {},
	"lflows_igmp":      {},
	"ovnsb_db_run":     {},
}

// RegisterOvnNorthdMetrics registers the ovn-northd metrics
func RegisterOvnNorthdMetrics(ovnRegistry prometheus.Registerer) {
	// ovn-northd metrics
	getOvnNorthdVersionInfo()
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnNamespace,
			Subsystem: types.MetricOvnSubsystemNorthd,
			Name:      "build_info",
			Help: "A metric with a constant '1' value labeled by version and library " +
				"from which ovn binaries were built",
			ConstLabels: prometheus.Labels{
				"version":         ovnNorthdVersion,
				"ovs_lib_version": ovnNorthdOvsLibVersion,
			},
		},
		func() float64 { return 1 },
	))
	ovnRegistry.MustRegister(metricOvnNorthdStatus)
	ovnRegistry.MustRegister(metricOvnNorthdNbConnectionStatus)
	ovnRegistry.MustRegister(metricOvnNorthdSbConnectionStatus)

	// Register the ovn-northd coverage/show metrics with prometheus
	componentCoverageShowMetricsMap[ovnNorthd] = ovnNorthdCoverageShowMetricsMap
	registerCoverageShowMetrics(ovnRegistry, ovnNorthd, types.MetricOvnNamespace, types.MetricOvnSubsystemNorthd)

	// Register the ovn-northd stopwatch/show metrics with prometheus
	componentStopwatchShowMetricsMap[ovnNorthd] = ovnNorthdStopwatchShowMetricsMap
	registerStopwatchShowMetrics(ovnRegistry, ovnNorthd, types.MetricOvnNamespace, types.MetricOvnSubsystemNorthd)
}
