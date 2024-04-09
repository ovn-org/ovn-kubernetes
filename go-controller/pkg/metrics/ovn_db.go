package metrics

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var metricOVNDBSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "jsonrpc_server_sessions",
	Help:      "Active number of JSON RPC Server sessions to the DB"},
	[]string{
		"db_name",
	},
)

var metricOVNDBMonitor = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "ovsdb_monitors",
	Help:      "Number of OVSDB Monitors on the server"},
	[]string{
		"db_name",
	},
)

var metricDBSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "db_size_bytes",
	Help:      "The size of the database file associated with the OVN DB component."},
	[]string{
		"db_name",
	},
)

// ClusterStatus metrics
var metricDBClusterCID = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "cluster_id",
	Help:      "A metric with a constant '1' value labeled by database name and cluster uuid"},
	[]string{
		"db_name",
		"cluster_id",
	},
)

var metricDBClusterSID = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
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
	Namespace: MetricOvnNamespace,
	Subsystem: MetricOvnSubsystemDB,
	Name:      "cluster_outbound_connections_error_total",
	Help: "A metric that returns the total number of failed  outbound connections from the server labeled by " +
		"database name, cluster uuid, and server uuid"},
	[]string{
		"db_name",
		"cluster_id",
		"server_id",
	},
)

func ovnDBSizeMetricsUpdater(dbProps *util.OvsDbProperties) {
	if size, err := getOvnDBSizeViaPath(dbProps); err != nil {
		klog.Errorf("Failed to update OVN DB size metric: %v", err)
	} else {
		metricDBSize.WithLabelValues(dbProps.DbName).Set(float64(size))
	}
}

func resetOvnDbSizeMetric() {
	metricDBSize.Reset()
}

// isOvnDBFoundViaPath attempts to find the OVN DBs, return false if not found.
func isOvnDBFoundViaPath(dbProperties []*util.OvsDbProperties) bool {
	enabled := true
	for _, dbProperty := range dbProperties {
		if _, err := getOvnDBSizeViaPath(dbProperty); err != nil {
			enabled = false
			break
		}
	}
	return enabled
}

func getOvnDBSizeViaPath(dbProperties *util.OvsDbProperties) (int64, error) {
	fileInfo, err := os.Stat(dbProperties.DbAlias)
	if err != nil {
		return 0, fmt.Errorf("failed to find OVN DB database %s at path %s: %v",
			dbProperties.DbName, dbProperties.DbAlias, err)
	}
	return fileInfo.Size(), nil
}

func ovnDBMemoryMetricsUpdater(dbProperties *util.OvsDbProperties) {
	var stdout, stderr string
	var err error

	stdout, stderr, err = dbProperties.AppCtl(5, "memory/show")
	if err != nil {
		klog.Errorf("Failed retrieving memory/show output for %q, stderr: %s, err: (%v)",
			strings.ToUpper(dbProperties.DbName), stderr, err)
		return
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "monitors:") {
			// kvPair will be of the form monitors:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBMonitor.WithLabelValues(dbProperties.DbName).Set(value)
			} else {
				klog.Errorf("Failed to parse the monitor's value %s to float64: err(%v)",
					fields[1], err)
			}
		} else if strings.HasPrefix(kvPair, "sessions:") {
			// kvPair will be of the form sessions:2
			fields := strings.Split(kvPair, ":")
			if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
				metricOVNDBSessions.WithLabelValues(dbProperties.DbName).Set(value)
			} else {
				klog.Errorf("Failed to parse the sessions' value %s to float64: err(%v)",
					fields[1], err)
			}
		}
	}
}

func resetOvnDbMemoryMetrics() {
	metricOVNDBMonitor.Reset()
	metricOVNDBSessions.Reset()
}

var (
	ovnDbVersion      string
	nbDbSchemaVersion string
	sbDbSchemaVersion string
)

func getNBDBSockPath() (string, error) {
	paths := []string{"/var/run/openvswitch/", "/var/run/ovn/"}
	for _, basePath := range paths {
		if _, err := os.Stat(basePath + "ovnnb_db.sock"); err == nil {
			klog.Infof("ovnnb_db.sock found at %s", basePath)
			return basePath, nil
		} else {
			klog.Infof("%sovnnb_db.sock getting info failed: %s", basePath, err)
		}
	}
	return "", fmt.Errorf("ovn db sock files weren't found in %s", strings.Join(paths, " or "))
}

func getOvnDbVersionInfo() {
	stdout, _, err := util.RunOVNNBAppCtl("version")
	if err == nil && strings.HasPrefix(stdout, "ovsdb-server (Open vSwitch) ") {
		ovnDbVersion = strings.Fields(stdout)[3]
	}
	basePath, err := getNBDBSockPath()
	if err != nil {
		klog.Errorf("OVN db schema versions can't be fetched: %s", err)
		return
	}
	sockPath := "unix:" + basePath + "ovnnb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Northbound")
	if err == nil {
		nbDbSchemaVersion = strings.TrimSpace(stdout)
	} else {
		klog.Errorf("OVN nbdb schema version can't be fetched: %s", err)
	}
	sockPath = "unix:" + basePath + "ovnsb_db.sock"
	stdout, _, err = util.RunOVSDBClient("get-schema-version", sockPath, "OVN_Southbound")
	if err == nil {
		sbDbSchemaVersion = strings.TrimSpace(stdout)
	} else {
		klog.Errorf("OVN sbdb schema version can't be fetched: %s", err)
	}
}

func RegisterOvnDBMetrics(clientset kubernetes.Interface, k8sNodeName string, stopChan <-chan struct{}) {
	err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 300*time.Second, true, func(ctx context.Context) (bool, error) {
		return checkPodRunsOnGivenNode(clientset, []string{"ovn-db-pod=true"}, k8sNodeName, false)
	})
	if err != nil {
		if wait.Interrupted(err) {
			klog.Errorf("Timed out while checking if OVN DB Pod runs on this %q K8s Node: %v. "+
				"Not registering OVN DB Metrics on this Node.", k8sNodeName, err)
		} else {
			klog.Infof("Not registering OVN DB Metrics on this Node since OVN DBs are not running on this node.")
		}
		return
	}
	klog.Info("Found OVN DB Pod running on this node. Registering OVN DB Metrics")

	// get the ovsdb server version info
	getOvnDbVersionInfo()
	// register metrics that will be served off of /metrics path
	ovnRegistry.MustRegister(metricOVNDBMonitor)
	ovnRegistry.MustRegister(metricOVNDBSessions)
	ovnRegistry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: MetricOvnNamespace,
			Subsystem: MetricOvnSubsystemDB,
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
	var dbProperties []*util.OvsDbProperties
	nbdbProps, err := util.GetOvsDbProperties(util.OvnNbdbLocation)
	if err != nil {
		klog.Errorf("Failed to init nbdb properties: %s", err)
	} else {
		dbProperties = append(dbProperties, nbdbProps)
	}
	sbdbProps, err := util.GetOvsDbProperties(util.OvnSbdbLocation)
	if err != nil {
		klog.Errorf("Failed to init sbdb properties: %s", err)
	} else {
		dbProperties = append(dbProperties, sbdbProps)
	}
	if len(dbProperties) == 0 {
		klog.Errorf("Failed to init properties for all databases")
		return
	}
	// check if DB is clustered or not
	// the usual way would be to call `ovsdb-tool db-is-standalone`,
	// but that command requires access to the db, and we don't always have it.
	// Therefore, we check cluster/status output for "not valid command" error
	dbIsClustered := true
	_, stderr, err := dbProperties[0].AppCtl(5, "cluster/status", dbProperties[0].DbName)
	if err != nil && strings.Contains(stderr, "is not a valid command") {
		dbIsClustered = false
		klog.Info("Found db is standalone, don't register db_cluster metrics")
	}
	if dbIsClustered {
		klog.Info("Found db is clustered, register db_cluster metrics")
		ovnRegistry.MustRegister(metricDBClusterCID)
		ovnRegistry.MustRegister(metricDBClusterSID)
		ovnRegistry.MustRegister(metricDBClusterServerStatus)
		ovnRegistry.MustRegister(metricDBClusterTerm)
		ovnRegistry.MustRegister(metricDBClusterServerRole)
		ovnRegistry.MustRegister(metricDBClusterServerVote)
		ovnRegistry.MustRegister(metricDBClusterElectionTimer)
		ovnRegistry.MustRegister(metricDBClusterLogIndexStart)
		ovnRegistry.MustRegister(metricDBClusterLogIndexNext)
		ovnRegistry.MustRegister(metricDBClusterLogNotCommitted)
		ovnRegistry.MustRegister(metricDBClusterLogNotApplied)
		ovnRegistry.MustRegister(metricDBClusterConnIn)
		ovnRegistry.MustRegister(metricDBClusterConnOut)
		ovnRegistry.MustRegister(metricDBClusterConnInErr)
		ovnRegistry.MustRegister(metricDBClusterConnOutErr)
	}

	dbFoundViaPath := isOvnDBFoundViaPath(dbProperties)

	if dbFoundViaPath {
		ovnRegistry.MustRegister(metricDBSize)
	} else {
		klog.Infof("Unable to enable OVN DB size metric because no OVN DBs found")
	}

	// functions responsible for collecting the values and updating the prometheus metrics
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// To update not only values but also labels for metrics, we use Reset() to delete previous labels+value
				if dbIsClustered {
					resetOvnDbClusterMetrics()
				}
				if dbFoundViaPath {
					resetOvnDbSizeMetric()
				}
				resetOvnDbMemoryMetrics()
				for _, dbProperty := range dbProperties {
					if dbIsClustered {
						ovnDBClusterStatusMetricsUpdater(dbProperty)
					}
					if dbFoundViaPath {
						ovnDBSizeMetricsUpdater(dbProperty)
					}
					ovnDBMemoryMetricsUpdater(dbProperty)
				}
			case <-stopChan:
				return
			}
		}
	}()
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

func getOVNDBClusterStatusInfo(timeout int, dbProperties *util.OvsDbProperties) (clusterStatus *OVNDBClusterStatus,
	err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the cluster/status output "+
				"for database %q: %v", dbProperties.DbName, r)
		}
	}()

	stdout, stderr, err = dbProperties.AppCtl(timeout, "cluster/status", dbProperties.DbName)
	if err != nil {
		klog.Errorf("Failed to retrieve cluster/status info for database %q, stderr: %s, err: (%v)",
			dbProperties.DbName, stderr, err)
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
			// db cluster with 1 member has empty Connections list
			if idx+2 >= len(line) {
				continue
			}
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

func ovnDBClusterStatusMetricsUpdater(dbProperties *util.OvsDbProperties) {
	clusterStatus, err := getOVNDBClusterStatusInfo(5, dbProperties)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
	metricDBClusterCID.WithLabelValues(dbProperties.DbName, clusterStatus.cid).Set(1)
	metricDBClusterSID.WithLabelValues(dbProperties.DbName, clusterStatus.cid, clusterStatus.sid).Set(1)
	metricDBClusterServerStatus.WithLabelValues(dbProperties.DbName, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.status).Set(1)
	metricDBClusterTerm.WithLabelValues(dbProperties.DbName, clusterStatus.cid, clusterStatus.sid).Set(clusterStatus.term)
	metricDBClusterServerRole.WithLabelValues(dbProperties.DbName, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.role).Set(1)
	metricDBClusterServerVote.WithLabelValues(dbProperties.DbName, clusterStatus.cid, clusterStatus.sid,
		clusterStatus.vote).Set(1)
	metricDBClusterElectionTimer.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.electionTimer)
	metricDBClusterLogIndexStart.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logIndexStart)
	metricDBClusterLogIndexNext.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logIndexNext)
	metricDBClusterLogNotCommitted.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logNotCommitted)
	metricDBClusterLogNotApplied.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.logNotApplied)
	metricDBClusterConnIn.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connIn)
	metricDBClusterConnOut.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connOut)
	metricDBClusterConnInErr.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connInErr)
	metricDBClusterConnOutErr.WithLabelValues(dbProperties.DbName, clusterStatus.cid,
		clusterStatus.sid).Set(clusterStatus.connOutErr)
}

func resetOvnDbClusterMetrics() {
	metricDBClusterCID.Reset()
	metricDBClusterSID.Reset()
	metricDBClusterServerStatus.Reset()
	metricDBClusterTerm.Reset()
	metricDBClusterServerRole.Reset()
	metricDBClusterServerVote.Reset()
	metricDBClusterElectionTimer.Reset()
	metricDBClusterLogIndexStart.Reset()
	metricDBClusterLogIndexNext.Reset()
	metricDBClusterLogNotCommitted.Reset()
	metricDBClusterLogNotApplied.Reset()
	metricDBClusterConnIn.Reset()
	metricDBClusterConnOut.Reset()
	metricDBClusterConnInErr.Reset()
	metricDBClusterConnOutErr.Reset()
}
