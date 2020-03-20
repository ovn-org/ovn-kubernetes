package app

import (
	"k8s.io/klog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli"
	kexec "k8s.io/utils/exec"
)

var metricOVNDBRaftIndex = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "log_entry_index",
	Help: "The index of log entry currently exposed to clients. " +
		"This value on all the instances of db should be close to " +
		"each other otherwise they are said to lagging with eaxch other."},
	[]string{
		"db_name",
	},
)

var metricOVNDBLeader = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_leader",
	Help:      "Identifies whether this pod is a leader for given database"},
	[]string{
		"db_name",
	},
)

var metricOVNDBSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "jsonrpc_server_sessions",
	Help:      "Active number of JSON RPC Server sessions to the DB"},
	[]string{
		"db_name",
	},
)

var metricOVNDBMonitor = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "ovsdb_monitors",
	Help:      "Number of OVSDB Monitors on the server"},
	[]string{
		"db_name",
	},
)

func ovnDBStatusMetricsUpdater(direction, database string) {
	status, err := util.GetOVNDBServerInfo(5, direction, database)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
	metricOVNDBRaftIndex.WithLabelValues(database).Set(float64(status.Index))
	if status.Leader {
		metricOVNDBLeader.WithLabelValues(database).Set(1)
	} else {
		metricOVNDBLeader.WithLabelValues(database).Set(0)
	}
}

func ovnDBMemoryMetricsUpdater(direction, database string) {
	var stdout, stderr string
	var err error

	if direction == "sb" {
		stdout, stderr, err = util.RunOVNSBAppCtl("--timeout=5", "memory/show")
	} else {
		stdout, stderr, err = util.RunOVNNBAppCtl("--timeout=5", "memory/show")
	}
	if err != nil {
		klog.Errorf("failed retrieving memory/show output for %q, stderr: %s, err: (%v)",
			strings.ToUpper(database), stderr, err)
		return
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "monitors:") {
			// kvPair will be of the form monitors:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBMonitor.WithLabelValues(database).Set(value)
			} else {
				klog.Errorf("failed to parse the monitor's value %s to float64: err(%v)",
					fields[1], err)
			}
		} else if strings.HasPrefix(kvPair, "sessions:") {
			// kvPair will be of the form sessions:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBSessions.WithLabelValues(database).Set(value)
			} else {
				klog.Errorf("failed to parse the sessions' value %s to float64: err(%v)",
					fields[1], err)
			}
		}
	}
}

var (
	ovnDbVersion      string
	nbDbSchemaVersion string
	sbDbSchemaVersion string
)

func getOvnDbVersionInfo() {
	stdout, _, err := util.RunOVSDBClient("-V")
	if err == nil && strings.HasPrefix(stdout, "ovsdb-client (Open vSwitch) ") {
		ovnDbVersion = strings.Fields(stdout)[2]
	}
	sockPath := "unix:/var/run/openvswitch/ovnnb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Northbound")
	if err == nil {
		nbDbSchemaVersion = strings.TrimSpace(stdout)
	}
	sockPath = "unix:/var/run/openvswitch/ovnsb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Southbound")
	if err == nil {
		sbDbSchemaVersion = strings.TrimSpace(stdout)
	}
}

// ReadinessProbeCommand runs readiness probes against various targets
var OvnDBExporterCommand = cli.Command{
	Name:  "ovn-db-exporter",
	Usage: "",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "metrics-bind-address",
			Usage: `The IP address and port for the metrics server to serve on (default ":9476")`,
		},
	},
	Action: func(ctx *cli.Context) error {
		bindAddress := ctx.String("metrics-bind-address")
		if bindAddress == "" {
			bindAddress = "0.0.0.0:9476"
		}

		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		// get the ovsdb server version info
		getOvnDbVersionInfo()
		// register metrics that will be served off of /metrics path
		prometheus.MustRegister(metricOVNDBRaftIndex)
		prometheus.MustRegister(metricOVNDBLeader)
		prometheus.MustRegister(metricOVNDBMonitor)
		prometheus.MustRegister(metricOVNDBSessions)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: metrics.MetricOvnNamespace,
				Subsystem: metrics.MetricOvnSubsystemDBRaft,
				Name:      "build_info",
				Help: "A metric with a constant '1' value labeled by ovsdb-server version and " +
					"NB and SB schema version",
				ConstLabels: prometheus.Labels{
					"version":           ovnDbVersion,
					"nb_schema_version": nbDbSchemaVersion,
					"sb_schema_version": sbDbSchemaVersion,
				},
			},
			func() float64 { return 1 },
		))

		// functions responsible for collecting the values and updating the prometheus metrics
		go func() {
			for {
				ovnDBStatusMetricsUpdater("nb", "OVN_Northbound")
				ovnDBStatusMetricsUpdater("sb", "OVN_Southbound")
				time.Sleep(30 * time.Second)
			}
		}()

		go func() {
			for {
				ovnDBMemoryMetricsUpdater("nb", "OVN_Northbound")
				ovnDBMemoryMetricsUpdater("sb", "OVN_Southbound")
				time.Sleep(30 * time.Second)
			}
		}()

		err := http.ListenAndServe(bindAddress, mux)
		if err != nil {
			klog.Exitf("starting metrics server failed: %v", err)
		}
		return nil
	},
}
