package ovn

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// metricE2ETimestamp is a timestamp value we have persisted to nbdb. We will
// also export a metric with the same column in sbdb. We will also bump this
// every 30 seconds, so we can detect a hung northd.
var metricE2ETimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "ovn_nb_e2e_timestamp",
	Help: "The current e2e-timestamp value as written to the northbound database"})

var registerMetricsOnce sync.Once
var startUpdaterOnce sync.Once

// RegisterMetrics registers some ovn-controller metrics with the Prometheus
// registry
func RegisterMetrics() {
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(metricE2ETimestamp)

		prometheus.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Name: "ovn_sb_e2e_timestamp",
				Help: "The current e2e-timestamp value as observed in the southbound database",
			}, scrapeOvnTimestamp))
	})
}

func scrapeOvnTimestamp() float64 {
	output, stderr, err := util.RunOVNSbctl("--if-exists",
		"get", "SB_Global", ".", "options:e2e_timestamp")
	if err != nil {
		logrus.Errorf("failed to scrape timestamp: %s (%v)", stderr, err)
		return 0
	}

	out, err := strconv.ParseFloat(output, 64)
	if err != nil {
		logrus.Errorf("failed to parse timestamp %s: %v", output, err)
		return 0
	}
	return out
}

// startOvnUpdater adds a goroutine that updates a "timestamp" value in the
// nbdb every 30 seconds. This is so we can determine freshness of the database
func startOvnUpdater() {
	startUpdaterOnce.Do(func() {
		go func() {
			for {
				t := time.Now().Unix()
				_, stderr, err := util.RunOVNNbctl("set", "NB_Global", ".",
					fmt.Sprintf(`options:e2e_timestamp="%d"`, t))
				if err != nil {
					logrus.Errorf("failed to bump timestamp: %s (%v)", stderr, err)
				} else {
					metricE2ETimestamp.Set(float64(t))
				}
				time.Sleep(30 * time.Second)
			}
		}()
	})
}
