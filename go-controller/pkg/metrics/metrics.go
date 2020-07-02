package metrics

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	MetricOvnkubeNamespace       = "ovnkube"
	MetricOvnkubeSubsystemMaster = "master"
	MetricOvnkubeSubsystemNode   = "node"
	MetricOvnNamespace           = "ovn"
	MetricOvnSubsystemDBRaft     = "db_raft"
	MetricOvnSubsystemNorthd     = "northd"
	MetricOvnSubsystemController = "controller"
	MetricOvsNamespace           = "ovs"
	MetricOvsSubsystemVswitchd   = "vswitchd"

	ovnNorthd     = "ovn-northd"
	ovnController = "ovn-controller"
	ovsVswitchd   = "ovs-vswitchd"
)

// Build information. Populated at build-time.
var (
	Commit    string
	Branch    string
	BuildUser string
	BuildDate string
)

type metricDetails struct {
	help   string
	metric prometheus.Gauge
}

// OVN/OVS components, namely ovn-northd, ovn-controller, and ovs-vswitchd provide various
// metrics through the 'coverage/show' command. The following data structure holds all the
// metrics we are interested in that output for a given component. We generalize capturing
// these metrics across all OVN/OVS components.
var componentCoverageShowMetricsMap = map[string]map[string]*metricDetails{}

func parseMetricToFloat(componentName, metricName, value string) float64 {
	f64Value, err := strconv.ParseFloat(value, 64)
	if err != nil {
		klog.Errorf("Failed to parse value %s into float for metric %s_%s :(%v)",
			value, componentName, metricName, err)
		return 0
	}
	return f64Value
}

// registerCoverageShowMetrics registers coverage/show metricss for
// various components(ovn-northd, ovn-controller, and ovs-vswitchd) with prometheus
func registerCoverageShowMetrics(target string, metricNamespace string, metricSubsystem string) {
	coverageShowMetricsMap := componentCoverageShowMetricsMap[target]
	for metricName, metricInfo := range coverageShowMetricsMap {
		metricInfo.metric = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      metricName,
			Help:      metricInfo.help,
		})
		prometheus.MustRegister(metricInfo.metric)
	}
}

// getCoverageShowOutputMap obtains the coverage/show metric values for the specified component.
func getCoverageShowOutputMap(component string) (map[string]string, error) {
	var stdout, stderr string
	var err error

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the coverage/show output "+
				"for %s: %v", component, r)
		}
	}()

	if component == ovnController {
		stdout, stderr, err = util.RunOVNControllerAppCtl("coverage/show")
	} else if component == ovnNorthd {
		stdout, stderr, err = util.RunOVNNorthAppCtl("coverage/show")
	} else if component == ovsVswitchd {
		stdout, stderr, err = util.RunOvsVswitchdAppCtl("coverage/show")
	} else {
		return nil, fmt.Errorf("component is unknown, and it isn't %s, %s, or %s",
			ovnNorthd, ovnController, ovsVswitchd)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get coverage/show output for %s "+
			"stderr(%s): (%v)", component, stderr, err)
	}

	coverageShowMetricsMap := make(map[string]string)
	output := strings.Split(stdout, "\n")
	for _, kvPair := range output {
		if strings.Contains(kvPair, "total:") {
			fields := strings.Fields(kvPair)
			coverageShowMetricsMap[fields[0]] = fields[len(fields)-1]
		}
	}
	return coverageShowMetricsMap, nil
}

// coverageShowMetricsUpdater updates the metric
// by obtaining values from getCoverageShowOutputMap for specified component.
func coverageShowMetricsUpdater(component string) {
	for {
		time.Sleep(30 * time.Second)
		coverageShowOutputMap, err := getCoverageShowOutputMap(component)
		if err != nil {
			klog.Errorf("%s", err.Error())
			continue
		}
		coverageShowMetricsMap := componentCoverageShowMetricsMap[component]
		for metricName, metricInfo := range coverageShowMetricsMap {
			if metricName == "packet_in" {
				metricName = "flow_extract"
			}
			if value, ok := coverageShowOutputMap[metricName]; ok {
				count := parseMetricToFloat(component, metricName, value)
				metricInfo.metric.Set(count)
			} else {
				metricInfo.metric.Set(0)
			}
		}
	}
}

// StartMetricsServer runs the prometheus listner so that metrics can be collected
func StartMetricsServer(bindAddress string, enablePprof bool) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	go utilwait.Until(func() {
		err := http.ListenAndServe(bindAddress, mux)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("starting metrics server failed: %v", err))
		}
	}, 5*time.Second, utilwait.NeverStop)

}
