package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"regexp"
	"strconv"
	"strings"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	MetricOvnkubeNamespace       = "ovnkube"
	MetricOvnkubeSubsystemMaster = "master"
	MetricOvnkubeSubsystemNode   = "node"
	MetricOvnNamespace           = "ovn"
	MetricOvnSubsystemDB         = "db"
	MetricOvnSubsystemNorthd     = "northd"
	MetricOvnSubsystemController = "controller"
	MetricOvsNamespace           = "ovs"
	MetricOvsSubsystemVswitchd   = "vswitchd"

	ovnNorthd     = "ovn-northd"
	ovnController = "ovn-controller"
	ovsVswitchd   = "ovs-vswitchd"

	metricsUpdateInterval = 5 * time.Minute
)

type metricDetails struct {
	srcName       string
	aggregateFrom []string
	help          string
	metric        prometheus.Gauge
}

type stopwatchMetricDetails struct {
	srcName string
	metrics struct {
		totalSamples   prometheus.Gauge
		max            prometheus.Gauge
		min            prometheus.Gauge
		percentile95th prometheus.Gauge
		shortTermAvg   prometheus.Gauge
		longTermAvg    prometheus.Gauge
	}
}

type stopwatchStatistics struct {
	totalSamples   string
	max            string
	min            string
	percentile95th string
	shortTermAvg   string
	longTermAvg    string
}

// OVN/OVS components, namely ovn-northd, ovn-controller, and ovs-vswitchd provide various
// metrics through the 'coverage/show' command. The following data structure holds all the
// metrics we are interested in that output for a given component. We generalize capturing
// these metrics across all OVN/OVS components.
var componentCoverageShowMetricsMap = map[string]map[string]*metricDetails{}

// OVN components, namely ovn-northd and ovn-controller provide various metrics through
// the 'stopwatch/show' command. The following data structure holds all the metrics we are
// interested in that output for a given component. We generalize capturing these metrics
// across all OVN components.
var componentStopwatchShowMetricsMap = map[string]map[string]*stopwatchMetricDetails{}

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
	for range time.Tick(metricsUpdateInterval) {
		coverageShowOutputMap, err := getCoverageShowOutputMap(component)
		if err != nil {
			klog.Errorf("%s", err.Error())
			continue
		}
		coverageShowMetricsMap := componentCoverageShowMetricsMap[component]
		for metricName, metricInfo := range coverageShowMetricsMap {
			var metricValue float64
			if metricInfo.srcName != "" {
				metricName = metricInfo.srcName
			}
			if metricInfo.aggregateFrom != nil {
				for _, aggregateMetricName := range metricInfo.aggregateFrom {
					if value, ok := coverageShowOutputMap[aggregateMetricName]; ok {
						metricValue += parseMetricToFloat(component, aggregateMetricName, value)
					}
				}
			} else {
				if value, ok := coverageShowOutputMap[metricName]; ok {
					metricValue = parseMetricToFloat(component, metricName, value)
				}
			}
			metricInfo.metric.Set(metricValue)
		}
	}
}

// registerStopwatchShowMetrics registers stopwatch/show metrics for
// various components(ovn-northd, ovn-controller) with prometheus
func registerStopwatchShowMetrics(component string, metricNamespace string, metricSubsystem string) {
	stopwatchShowMetricsMap := componentStopwatchShowMetricsMap[component]
	for metricName, metricInfo := range stopwatchShowMetricsMap {
		metricInfo.metrics.totalSamples = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_total_samples", metricName),
		})

		metricInfo.metrics.max = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_maximum", metricName),
		})

		metricInfo.metrics.min = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_minimum", metricName),
		})

		metricInfo.metrics.percentile95th = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_95th_percentile", metricName),
		})

		metricInfo.metrics.shortTermAvg = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_short_term_avg", metricName),
		})

		metricInfo.metrics.longTermAvg = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      fmt.Sprintf("%s_long_term_avg", metricName),
		})

		prometheus.MustRegister(metricInfo.metrics.totalSamples)
		prometheus.MustRegister(metricInfo.metrics.min)
		prometheus.MustRegister(metricInfo.metrics.max)
		prometheus.MustRegister(metricInfo.metrics.percentile95th)
		prometheus.MustRegister(metricInfo.metrics.shortTermAvg)
		prometheus.MustRegister(metricInfo.metrics.longTermAvg)
	}
}

// getStopwatchShowOutputMap obtains the stopwatch/show metric values for the specified component.
func getStopwatchShowOutputMap(component string) (map[string]stopwatchStatistics, error) {
	var stdout, stderr string
	var err error

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the stopwatch/show output for %s: %v", component, r)
		}
	}()

	switch component {
	case ovnController:
		stdout, stderr, err = util.RunOVNControllerAppCtl("stopwatch/show")
	case ovnNorthd:
		stdout, stderr, err = util.RunOVNNorthAppCtl("stopwatch/show")
	default:
		return nil, fmt.Errorf("unknown component %s for stopwatch/show", component)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get stopwatch/show output for %s (stderr: %s): %w", component, stderr, err)
	}

	parsedOutput := parseStopwatchShowOutput(stdout)
	return parsedOutput, err //need to return err, as it could be set in the defer
}

// parseStopwatchShowOutput returns the number of total samples for each poll loop
func parseStopwatchShowOutput(output string) map[string]stopwatchStatistics {
	result := make(map[string]stopwatchStatistics)

	reStatisticsBlock := regexp.MustCompile(`(?m)^Statistics for '(?P<name>.+)'$(?:\n  .*)+`)
	reTotalSamples := regexp.MustCompile(`(?m)^  Total samples: (?P<totalSamples>\d*)$`)
	reMaximum := regexp.MustCompile(`(?m)^  Maximum: (?P<max>\d*) msec$`)
	reMinumum := regexp.MustCompile(`(?m)^  Minimum: (?P<min>\d*) msec$`)
	rePercentile95th := regexp.MustCompile(`(?m)^  95th percentile: (?P<percentile95th>\d*\.?\d*) msec$`)
	reShortTermAvg := regexp.MustCompile(`(?m)^  Short term average: (?P<shortTermAvg>\d*\.?\d*) msec$`)
	reLongTermAvg := regexp.MustCompile(`(?m)^  Long term average: (?P<longTermAvg>\d*\.?\d*) msec$`)

	nameIndex := reStatisticsBlock.SubexpIndex("name")
	totalSamplesIndex := reTotalSamples.SubexpIndex("totalSamples")
	maxIndex := reMaximum.SubexpIndex("max")
	minIndex := reMinumum.SubexpIndex("min")
	percentile95thIndex := rePercentile95th.SubexpIndex("percentile95th")
	shortTermAvgIndex := reShortTermAvg.SubexpIndex("shortTermAvg")
	longTermAvgIndex := reLongTermAvg.SubexpIndex("longTermAvg")

	for _, match := range reStatisticsBlock.FindAllStringSubmatch(output, -1) {
		sws := stopwatchStatistics{}

		if totalSamplesMatch := reTotalSamples.FindStringSubmatch(match[0]); totalSamplesMatch != nil {
			sws.totalSamples = totalSamplesMatch[totalSamplesIndex]
		}
		if minMatch := reMinumum.FindStringSubmatch(match[0]); minMatch != nil {
			sws.min = minMatch[minIndex]
		}
		if maxMatch := reMaximum.FindStringSubmatch(match[0]); maxMatch != nil {
			sws.max = maxMatch[maxIndex]
		}
		if percentile95thMatch := rePercentile95th.FindStringSubmatch(match[0]); percentile95thMatch != nil {
			sws.percentile95th = percentile95thMatch[percentile95thIndex]
		}
		if shortTermAvgMatch := reShortTermAvg.FindStringSubmatch(match[0]); shortTermAvgMatch != nil {
			sws.shortTermAvg = shortTermAvgMatch[shortTermAvgIndex]
		}
		if longTermAvgMatch := reLongTermAvg.FindStringSubmatch(match[0]); longTermAvgMatch != nil {
			sws.longTermAvg = longTermAvgMatch[longTermAvgIndex]
		}

		metricName := match[nameIndex]
		result[metricName] = sws
	}

	return result
}

// stopwatchShowMetricsUpdater updates the metric by obtaining the stopwatch/show
// metrics for the specified component.
func stopwatchShowMetricsUpdater(component string) {
	for range time.Tick(metricsUpdateInterval) {
		stopwatchShowOutputMap, err := getStopwatchShowOutputMap(component)
		if err != nil {
			klog.Error(err)
			continue
		}

		if len(stopwatchShowOutputMap) == 0 {
			klog.Warningf("No stopwatch/show metrics for component %s", component)
			continue
		}

		stopwatchShowInterestingMetrics := componentStopwatchShowMetricsMap[component]
		for metricName, metricInfo := range stopwatchShowInterestingMetrics {
			var totalSamplesMetricValue, maxMetricValue, minMetricValue, percentile95thMetricValue, shortTermAvgMetricValue, longTermAvgMetricValue float64

			if metricInfo.srcName != "" {
				metricName = metricInfo.srcName
			}

			if value, ok := stopwatchShowOutputMap[metricName]; ok {
				totalSamplesMetricValue = parseMetricToFloat(component, metricName, value.totalSamples)
				minMetricValue = parseMetricToFloat(component, metricName, value.min)
				maxMetricValue = parseMetricToFloat(component, metricName, value.max)
				percentile95thMetricValue = parseMetricToFloat(component, metricName, value.percentile95th)
				shortTermAvgMetricValue = parseMetricToFloat(component, metricName, value.shortTermAvg)
				longTermAvgMetricValue = parseMetricToFloat(component, metricName, value.longTermAvg)
			}

			metricInfo.metrics.totalSamples.Set(totalSamplesMetricValue)
			metricInfo.metrics.min.Set(minMetricValue)
			metricInfo.metrics.max.Set(maxMetricValue)
			metricInfo.metrics.percentile95th.Set(percentile95thMetricValue)
			metricInfo.metrics.shortTermAvg.Set(shortTermAvgMetricValue)
			metricInfo.metrics.longTermAvg.Set(longTermAvgMetricValue)
		}
	}
}

// The `keepTrying` boolean when set to true will not return an error if we can't find pods with one of the given labels.
// This is so that the caller can re-try again to see if the pods have appeared in the k8s cluster.
func checkPodRunsOnGivenNode(clientset kubernetes.Interface, labels []string, k8sNodeName string,
	keepTrying bool) (bool, error) {
	for _, label := range labels {
		pods, err := clientset.CoreV1().Pods(config.Kubernetes.OVNConfigNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			klog.V(5).Infof("Failed to list Pods with label %q: %v. Retrying..", label, err)
			return false, nil
		}
		for _, pod := range pods.Items {
			if pod.Spec.NodeName == k8sNodeName {
				return true, nil
			}
		}
	}
	if keepTrying {
		return false, nil
	}
	return false, fmt.Errorf("a Pod matching at least one of the labels %q doesn't exist on this node %s",
		strings.Join(labels, ","), k8sNodeName)
}

// StartMetricsServer runs the prometheus listener so that OVN K8s metrics can be collected
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

var ovnRegistry = prometheus.NewRegistry()

// StartOVNMetricsServer runs the prometheus listener so that OVN metrics can be collected
func StartOVNMetricsServer(bindAddress string) {
	handler := promhttp.InstrumentMetricHandler(ovnRegistry,
		promhttp.HandlerFor(ovnRegistry, promhttp.HandlerOpts{}))
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)

	go utilwait.Until(func() {
		err := http.ListenAndServe(bindAddress, mux)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("starting metrics server failed: %v", err))
		}
	}, 5*time.Second, utilwait.NeverStop)
}

func RegisterOvnMetrics(clientset kubernetes.Interface, k8sNodeName string, nbClient libovsdbclient.Client,
	sbClient libovsdbclient.Client) {
	go RegisterOvnDBMetrics(clientset, k8sNodeName, nbClient, sbClient)
	go RegisterOvnControllerMetrics()
	go RegisterOvnNorthdMetrics(clientset, k8sNodeName, nbClient)
}
