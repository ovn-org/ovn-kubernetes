// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	ovnNorthd     = "ovn-northd"
	ovnController = "ovn-controller"
	ovsVswitchd   = "ovs-vswitchd"

	metricsUpdateInterval = 5 * time.Minute

	// defaultCollectionInterval is how often the background loop refreshes OVS/OVN
	// metric values when MetricServerOptions.CollectionInterval is unset or negative.
	defaultCollectionInterval = config.DefaultMetricsCollectionInterval

	// metricsAppctlTimeout (seconds) bounds every ovs-appctl/ovn-appctl subprocess on
	// the scrape path via --timeout=N. A saturated daemon (e.g. ovn-controller) would
	// otherwise block the call and pin the scrape past Prometheus' scrape_timeout,
	// flapping up to 0. Healthy appctls answer in ms, so a low bound sheds stuck calls.
	metricsAppctlTimeout = 2
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

// MetricResourceRetryFailuresCount is the number of times retrying to reconcile a Kubernetes
// resource reached the maximum retry limit and will not be retried. This metric doesn't
// need Subsystem string since it is applicable for both control plane and node.
var MetricResourceRetryFailuresCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Name:      "resource_retry_failures_total",
	Help:      "The total number of times processing a Kubernetes resource reached the maximum retry limit and was no longer processed",
})

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
func registerCoverageShowMetrics(ovnRegistry prometheus.Registerer, target string, metricNamespace string, metricSubsystem string) {
	coverageShowMetricsMap := componentCoverageShowMetricsMap[target]
	for metricName, metricInfo := range coverageShowMetricsMap {
		metricInfo.metric = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      metricName,
			Help:      metricInfo.help,
		})
		ovnRegistry.MustRegister(metricInfo.metric)
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
		stdout, stderr, err = util.RunOVNControllerAppCtlWithTimeout(metricsAppctlTimeout, "coverage/show")
	} else if component == ovnNorthd {
		stdout, stderr, err = util.RunOVNNorthAppCtlWithTimeout(metricsAppctlTimeout, "coverage/show")
	} else if component == ovsVswitchd {
		stdout, stderr, err = util.RunOvsVswitchdAppCtlWithTimeout(metricsAppctlTimeout, "coverage/show")
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

// ovnKubeLogFileSizeMetricsUpdater updates the metrics that obtains the
// size of ovnkube process' logfile
func ovnKubeLogFileSizeMetricsUpdater(ovnKubeLogFileMetric *prometheus.GaugeVec,
	stopChan <-chan struct{}) {
	ticker := time.NewTicker(metricsUpdateInterval)
	defer ticker.Stop()

	logfile := config.Logging.File
	// only start the file watcher if the log file directory is valid
	if logfile == "" {
		klog.Infof("OVN Kube log file not specified in config, therefore not starting the log file metric monitor")
		return
	}
	logFileDirectoryPath := path.Dir(logfile)
	if _, err := os.Stat(logFileDirectoryPath); err != nil && os.IsNotExist(err) {
		klog.Errorf("OVNKube log file directory (%q) doesn't exist, file path: %q", logFileDirectoryPath, logfile)
		return
	}
	fileName := path.Base(logfile)

	for {
		select {
		case <-ticker.C:
			var fileSize float64
			fileInfo, err := os.Stat(logfile)
			if err != nil {
				// file may not yet exist. Metric will be updated to zero when file doesn't exist.
				if !os.IsNotExist(err) {
					klog.Errorf("Failed to get the logfile size for %s: %v", fileName, err)
				}
			} else {
				fileSize = float64(fileInfo.Size())
			}
			ovnKubeLogFileMetric.WithLabelValues(fileName).Set(fileSize)
		case <-stopChan:
			return
		}
	}
}

// coverageShowMetricsUpdate updates the metric by obtaining values from
// getCoverageShowOutputMap for specified component. The counters displayed
// by coverage/show output are called events. It could be that the event never
// happened, and therefore there will be no counter for it in the output. In such
// cases the default value of the counter will be 0.
func coverageShowMetricsUpdate(component string) {
	coverageShowOutputMap, err := getCoverageShowOutputMap(component)
	if err != nil {
		klog.Errorf("Getting coverage/show metrics for %s failed: %s", component, err.Error())
		return
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

// registerStopwatchShowMetrics registers stopwatch/show metrics for
// various components(ovn-northd, ovn-controller) with prometheus
func registerStopwatchShowMetrics(ovnRegistry prometheus.Registerer, component string, metricNamespace string, metricSubsystem string) {
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

		ovnRegistry.MustRegister(metricInfo.metrics.totalSamples)
		ovnRegistry.MustRegister(metricInfo.metrics.min)
		ovnRegistry.MustRegister(metricInfo.metrics.max)
		ovnRegistry.MustRegister(metricInfo.metrics.percentile95th)
		ovnRegistry.MustRegister(metricInfo.metrics.shortTermAvg)
		ovnRegistry.MustRegister(metricInfo.metrics.longTermAvg)
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
		stdout, stderr, err = util.RunOVNControllerAppCtlWithTimeout(metricsAppctlTimeout, "stopwatch/show")
	case ovnNorthd:
		stdout, stderr, err = util.RunOVNNorthAppCtlWithTimeout(metricsAppctlTimeout, "stopwatch/show")
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

// stopwatchShowMetricsUpdate updates the metric by obtaining the stopwatch/show
// metrics for the specified component.
func stopwatchShowMetricsUpdate(component string) {
	stopwatchShowOutputMap, err := getStopwatchShowOutputMap(component)
	if err != nil {
		klog.Errorf("Getting stopwatch/show metrics for %s failed: %s", component, err.Error())
		return
	}

	if len(stopwatchShowOutputMap) == 0 {
		klog.Warningf("No stopwatch/show metrics for component %s", component)
		return
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
		metricInfo.metrics.min.Set(minMetricValue / 1000)
		metricInfo.metrics.max.Set(maxMetricValue / 1000)
		metricInfo.metrics.percentile95th.Set(percentile95thMetricValue / 1000)
		metricInfo.metrics.shortTermAvg.Set(shortTermAvgMetricValue / 1000)
		metricInfo.metrics.longTermAvg.Set(longTermAvgMetricValue / 1000)
	}

}

// stringFlagSetterFunc is a func used for setting string type flag.
type stringFlagSetterFunc func(string) (string, error)

// klogSetter is a setter to set klog level.
func klogSetter(val string) (string, error) {
	var level klog.Level
	if err := level.Set(val); err != nil {
		return "", fmt.Errorf("failed set klog.logging.verbosity %s: %v", val, err)
	}
	return fmt.Sprintf("successfully set klog.logging.verbosity to %s", val), nil
}

// stringFlagPutHandler wraps an http Handler to set string type flag.
func stringFlagPutHandler(setter stringFlagSetterFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == "PUT":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				writePlainText(http.StatusBadRequest, "error reading request body: "+err.Error(), w)
				return
			}
			defer req.Body.Close()
			response, err := setter(string(body))
			if err != nil {
				writePlainText(http.StatusBadRequest, err.Error(), w)
				return
			}
			writePlainText(http.StatusOK, response, w)
			return
		default:
			writePlainText(http.StatusNotAcceptable, "unsupported http method", w)
			return
		}
	})
}

// writePlainText renders a simple string response.
func writePlainText(statusCode int, text string, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(statusCode)
	fmt.Fprintln(w, text)
}

// StartMetricsServer runs the prometheus listener so that metrics can be collected.
// It registers metrics based on the enabled flags in MetricServerOptions and returns
// the MetricServer instance to allow dynamic metric registration via Enable* methods.
func StartMetricsServer(opts MetricServerOptions, stopChan <-chan struct{}, wg *sync.WaitGroup) *MetricServer {
	klog.Infof("Starting Metrics Server on address: %s", opts.BindAddress)
	metricsServer := NewMetricServer(opts)

	// Register metrics based on the enabled flags in opts.
	// This is safe to call unconditionally as registerMetrics only registers
	// metrics for which the corresponding Enable* flag is true.
	metricsServer.registerMetrics()

	wg.Add(1)
	go func() {
		defer wg.Done()
		klog.Infof("Metrics Server starts to run ...")
		metricsServer.Run(stopChan)
	}()

	if opts.CollectionInterval != 0 {
		// Extraction runs out of band from scraping: the loop refreshes the registry
		// on its own interval and the scrape handler only serializes it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			metricsServer.runCollectionLoop(stopChan)
		}()
	} else {
		klog.Infof("Metrics collection is on on-demand mode, no background collection loop will be started")
	}

	return metricsServer
}
