package app

import (
	"fmt"
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

// ClusterStatus metrics
var metricDBClusterCID = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_id",
	Help:      "A metric with a constant '1' value labeled by database name and cluster uuid"},
	[]string{
		"db_name",
		"cluster_id",
	},
)

var metricDBClusterSID = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_server_id",
	Help: "A metric with a constant '1' value labeled by database name, cluster uuid " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterServerStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_server_status",
	Help: "A metric with a constant '1' value labeled by database name, cluster uuid, server uuid " +
		"server status"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
		"server_status",
	},
)

var metricDBClusterServerRole = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_server_role",
	Help: "A metric with a constant '1' value labeled by database name, cluster uuid, server uuid " +
		"and server role"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
		"server_role",
	},
)

var metricDBClusterTerm = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_term",
	Help: "A metric that returns the current election term value labeled by database name, cluster uuid, and " +
		"server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterServerVote = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_server_vote",
	Help: "A metric with a constant '1' value labeled by database name, cluster uuid, server uuid " +
		"and server vote"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
		"server_vote",
	},
)

var metricDBClusterElectionTimer = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_election_timer",
	Help: "A metric that returns the current election timer value labeled by database name, cluster uuid, " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterLogIndexStart = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_log_index_start",
	Help: "A metric that returns the log entry index start value labeled by database name, cluster uuid, " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterLogIndexNext = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_log_index_next",
	Help: "A metric that returns the log entry index next value labeled by database name, cluster uuid, " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterLogNotCommitted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_log_not_committed",
	Help: "A metric that returns the number of log entries not committed labeled by database name, cluster uuid, " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterLogNotApplied = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_log_not_applied",
	Help: "A metric that returns the number of log entries not applied labeled by database name, cluster uuid, " +
		"and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterConnIn = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_inbound_connections_total",
	Help: "A metric that returns the total number of inbound  connections to the server labeled by " +
		"database name, cluster uuid, and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterConnOut = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_outbound_connections_total",
	Help: "A metric that returns the total number of outbound connections from the server labeled by " +
		"database name, cluster uuid, and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterConnInErr = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_inbound_connections_error_total",
	Help: "A metric that returns the total number of failed inbound connections to the server labeled by " +
		" database name, cluster uuid, and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

var metricDBClusterConnOutErr = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.MetricOvnNamespace,
	Subsystem: metrics.MetricOvnSubsystemDBRaft,
	Name:      "cluster_outbound_connections_error_total",
	Help: "A metric that returns the total number of failed  outbound connections from the server labeled by " +
		"database name, cluster uuid, and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
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
		prometheus.MustRegister(metricDBClusterCID)
		prometheus.MustRegister(metricDBClusterSID)
		prometheus.MustRegister(metricDBClusterServerStatus)
		prometheus.MustRegister(metricDBClusterTerm)
		prometheus.MustRegister(metricDBClusterServerRole)
		prometheus.MustRegister(metricDBClusterServerVote)
		prometheus.MustRegister(metricDBClusterElectionTimer)
		prometheus.MustRegister(metricDBClusterLogIndexStart)
		prometheus.MustRegister(metricDBClusterLogIndexNext)
		prometheus.MustRegister(metricDBClusterLogNotCommitted)
		prometheus.MustRegister(metricDBClusterLogNotApplied)
		prometheus.MustRegister(metricDBClusterConnIn)
		prometheus.MustRegister(metricDBClusterConnOut)
		prometheus.MustRegister(metricDBClusterConnInErr)
		prometheus.MustRegister(metricDBClusterConnOutErr)

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

		go func() {
			for {
				ovnDBClusterStatusMetricsUpdater("nb", "OVN_Northbound")
				ovnDBClusterStatusMetricsUpdater("sb", "OVN_Southbound")
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

type OVNDBClusterStatus struct {
	cid             string
	sid             string
	status          string
	role            string
	vote            string
	term            float64
	electionTimer   float64
	logIndexStart   float64
	logIndexNext    float64
	logNotCommitted float64
	logNotApplied   float64
	connIn          float64
	connOut         float64
	connInErr       float64
	connOutErr      float64
}

func getOVNDBClusterStatusInfo(timeout int, direction, database string) (clusterStatus *OVNDBClusterStatus,
	err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the cluster/status output "+
				"for database %q: %v", database, r)
		}
	}()

	if direction == "sb" {
		stdout, stderr, err = util.RunOVNSBAppCtl(fmt.Sprintf("--timeout=%d", timeout),
			"cluster/status", database)
	} else {
		stdout, stderr, err = util.RunOVNSBAppCtl(fmt.Sprintf("--timeout=%d", timeout),
			"cluster/status", database)
	}
	if err != nil {
		klog.Errorf("failed to retrieve cluster/status info for database %q, stderr: %s, err: (%v)",
			database, stderr, err)
		return nil, err
	}

	clusterStatus = &OVNDBClusterStatus{}
	for _, line := range strings.Split(stdout, "\n") {
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		switch line[:idx] {
		case "Cluster ID":
			// the value is of the format `45ef (45ef51b9-9401-46e7-810d-6db0fc344ea2)`
			clusterStatus.cid = strings.Trim(strings.Fields(line[idx+2:])[1], "()")
		case "Server ID":
			clusterStatus.sid = strings.Trim(strings.Fields(line[idx+2:])[1], "()")
		case "Status":
			clusterStatus.status = line[idx+2:]
		case "Role":
			clusterStatus.role = line[idx+2:]
		case "Term":
			if value, err := strconv.ParseFloat(line[idx+2:], 64); err == nil {
				clusterStatus.term = value
			}
		case "Vote":
			clusterStatus.vote = line[idx+2:]
		case "Election timer":
			if value, err := strconv.ParseFloat(line[idx+2:], 64); err == nil {
				clusterStatus.electionTimer = value
			}
		case "Log":
			// the value is of the format [2, 1108]
			values := strings.Split(strings.Trim(line[idx+2:], "[]"), ", ")
			if value, err := strconv.ParseFloat(values[0], 64); err == nil {
				clusterStatus.logIndexStart = value
			}
			if value, err := strconv.ParseFloat(values[1], 64); err == nil {
				clusterStatus.logIndexNext = value
			}
		case "Entries not yet committed":
			if value, err := strconv.ParseFloat(line[idx+2:], 64); err == nil {
				clusterStatus.logNotCommitted = value
			}
		case "Entries not yet applied":
			if value, err := strconv.ParseFloat(line[idx+2:], 64); err == nil {
				clusterStatus.logNotApplied = value
			}
		case "Connections":
			// the value is of the format `->0000 (->56d7) <-46ac <-56d7`
			var connIn, connOut, connInErr, connOutErr float64
			for _, conn := range strings.Fields(line[idx+2:]) {
				if strings.HasPrefix(conn, "->") {
					connOut++
				} else if strings.HasPrefix(conn, "<-") {
					connIn++
				} else if strings.HasPrefix(conn, "(->") {
					connOutErr++
				} else if strings.HasPrefix(conn, "(<-") {
					connInErr++
				}
			}
			clusterStatus.connIn = connIn
			clusterStatus.connOut = connOut
			clusterStatus.connInErr = connInErr
			clusterStatus.connOutErr = connOutErr
		}
	}

	return clusterStatus, nil
}

func ovnDBClusterStatusMetricsUpdater(direction, database string) {
	clusterStatus, err := getOVNDBClusterStatusInfo(5, direction, database)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
	metricDBClusterCID.WithLabelValues(database, clusterStatus.cid).Set(1)
	metricDBClusterSID.WithLabelValues(database, clusterStatus.cid, clusterStatus.sid).Set(1)
	metricDBClusterServerStatus.WithLabelValues(database, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.status).Set(1)
	metricDBClusterTerm.WithLabelValues(database, clusterStatus.cid, clusterStatus.sid).Set(clusterStatus.term)
	metricDBClusterServerRole.WithLabelValues(database, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.role).Set(1)
	metricDBClusterServerVote.WithLabelValues(database, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.vote).Set(1)
	metricDBClusterElectionTimer.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.electionTimer)
	metricDBClusterLogIndexStart.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logIndexStart)
	metricDBClusterLogIndexNext.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logIndexNext)
	metricDBClusterLogNotCommitted.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logNotCommitted)
	metricDBClusterLogNotApplied.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logNotApplied)
	metricDBClusterConnIn.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connIn)
	metricDBClusterConnOut.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connOut)
	metricDBClusterConnInErr.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connInErr)
	metricDBClusterConnOutErr.WithLabelValues(database, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connOutErr)
}
