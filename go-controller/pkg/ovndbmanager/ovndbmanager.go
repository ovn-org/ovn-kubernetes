package ovndbmanager

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var DBError = errors.New("error interacting with OVN database")

const (
	maxDBRetry     = 10
	nbdbSchema     = "/usr/share/ovn/ovn-nb.ovsschema"
	nbdbServerSock = "unix:/var/run/ovn/ovnnb_db.sock"
	sbdbSchema     = "/usr/share/ovn/ovn-sb.ovsschema"
	sbdbServerSock = "unix:/var/run/ovn/ovnsb_db.sock"
)

type dbProperties struct {
	appCtl        func(timeout int, args ...string) (string, string, error)
	dbName        string
	electionTimer int
}

func RunDBChecker(kclient kube.Interface, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	klog.Info("Starting DB Checker to ensure cluster membership and DB consistency")
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := upgradeNBDBSchema(); err != nil {
			klog.Fatalf("NBDB Upgrade failed: %w", err)
		}
		ensureOvnDBState(util.OvnNbdbLocation, kclient, stopCh)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := upgradeSBDBSchema(); err != nil {
			klog.Fatalf("SBDB Upgrade failed: %w", err)
		}
		ensureOvnDBState(util.OvnSbdbLocation, kclient, stopCh)
	}()
	<-stopCh
	klog.Info("Shutting down db checker")
	wg.Wait()
	klog.Info("Shut down db checker")
}

func ensureOvnDBState(db string, kclient kube.Interface, stopCh <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	klog.Infof("Starting ensure routine for Raft db: %s", db)
	_, _, err := util.RunOVSDBTool("db-is-standalone", db)
	if err == nil {
		klog.Info("The db is running in standalone mode")
		return
	} else {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			klog.Infof("Exit code for the db-is-standalone: %v", ee.ExitCode())
			if ee.ExitCode() != 2 {
				klog.Fatalf("Can't determine if the db is clustered/stand-alone" +
					" restarting the checker")
				os.Exit(1)
			}
		}
	}
	properties := propertiesForDB(db)

	var dbRetry int32

	for {
		select {
		case <-ticker.C:
			klog.V(5).Infof("Ensure routines for Raft db: %s kicked off by ticker", db)
			if err := ensureLocalRaftServerID(db); err != nil {
				klog.Error(err)
				if errors.Is(err, DBError) {
					updateDBRetryCounter(&dbRetry, db)
				}
			} else {
				dbRetry = 0
			}
			if err := ensureClusterRaftMembership(db, kclient); err != nil {
				klog.Error(err)
				if errors.Is(err, DBError) {
					updateDBRetryCounter(&dbRetry, db)
				}
			} else {
				dbRetry = 0
			}
			if properties.electionTimer != 0 {
				if err := ensureElectionTimeout(properties); err != nil {
					klog.Error(err)
					if errors.Is(err, DBError) {
						updateDBRetryCounter(&dbRetry, db)
					}
				} else {
					dbRetry = 0
				}
			}
		case <-stopCh:
			ticker.Stop()
			return
		}
	}
}

func updateDBRetryCounter(retryCounter *int32, db string) {
	if *retryCounter > maxDBRetry {
		//delete the db file and start master
		resetRaftDB(db)
		*retryCounter = 0
	} else {
		*retryCounter += 1
		klog.Infof("Failed to get cluster status for: %s, number of retries: %d", db, *retryCounter)
	}
}

// ensureLocalRaftServerID is used to ensure there is no stale member in the Raft cluster with our address
func ensureLocalRaftServerID(db string) error {
	var dbName string
	var appCtl func(timeout int, args ...string) (string, string, error)
	if strings.Contains(db, "ovnnb") {
		dbName = "OVN_Northbound"
		appCtl = util.RunOVNNBAppCtlWithTimeout
	} else {
		dbName = "OVN_Southbound"
		appCtl = util.RunOVNSBAppCtlWithTimeout
	}

	out, stderr, err := appCtl(5, "cluster/sid", dbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get db server ID for: %s, stderr: %v, err: %v", DBError, db, stderr, err)
	}
	if len(out) < 4 {
		return fmt.Errorf("%w: invalid db id found: %s for db: %s", DBError, out, db)
	}
	// server ID in raft membership is only first 4 char prefix
	serverID := out[:4]
	out, stderr, err = appCtl(5, "cluster/status", dbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get cluster status for: %s, stderr: %v, err: %v", DBError, db, stderr, err)
	}

	r := regexp.MustCompile(`Address: *((ssl|tcp):[?[a-z0-9.:]+]?)`)
	matches := r.FindStringSubmatch(out)
	if len(matches) < 2 {
		return fmt.Errorf("unable to parse Address for db: %s, output: %s", db, out)
	}
	addr := matches[1]

	// make sure IPV6 addresses are correctly escaped in regexp
	escapingBrackets := strings.NewReplacer("[", "\\[", "]", "\\]")
	addr = escapingBrackets.Replace(addr)

	// look for current servers in raft cluster with the same address
	r = regexp.MustCompile("([a-z0-9]{4}) at " + addr)
	members := r.FindAllStringSubmatch(out, -1)
	for _, member := range members {
		if len(member) < 2 {
			return fmt.Errorf("unable to find server id submatch in %s from %s", member, db)
		}
		if member[1] != serverID {
			// stale entry found for this node with same address, need to kick
			klog.Infof("Previous stale member found in %s: %s... kicking", db, member[1])
			_, stderr, err = appCtl(5, "cluster/kick", dbName, member[1])
			if err != nil {
				klog.Errorf("Error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, error: %v", member[1], addr, db, stderr, err)
			}
		}
	}
	return nil
}

// ensureClusterRaftMembership ensures there are no unknown members in the current Raft cluster
func ensureClusterRaftMembership(db string, kclient kube.Interface) error {
	var knownMembers, knownServers []string

	var dbName string
	var appCtl func(timeout int, args ...string) (string, string, error)

	// IPv4 example: tcp:172.18.0.2:6641
	// IPv6 example: tcp:[fc00:f853:ccd:e793::3]:6642
	dbServerRegexp := `(ssl|tcp):(\[?([a-z0-9.:]+)\]?:\d+)`
	r := regexp.MustCompile(dbServerRegexp)

	if strings.Contains(db, "ovnnb") {
		dbName = "OVN_Northbound"
		appCtl = util.RunOVNNBAppCtlWithTimeout
		knownMembers = strings.Split(config.OvnNorth.Address, ",")
	} else {
		dbName = "OVN_Southbound"
		appCtl = util.RunOVNSBAppCtlWithTimeout
		knownMembers = strings.Split(config.OvnSouth.Address, ",")
	}
	for _, knownMember := range knownMembers {
		match := r.FindStringSubmatch(knownMember)
		if len(match) < 4 {
			klog.Warningf("Failed to parse known %s member: %s", dbName, knownMember)
			continue
		}
		server := match[3]
		if !(utilnet.IsIPv4String(server) || utilnet.IsIPv6String(server)) {
			klog.Warningf("Found invalid value for IP address of known %s member %s: %s",
				dbName, knownMember, server)
			continue
		}
		knownServers = append(knownServers, server)
	}
	out, stderr, err := appCtl(5, "cluster/status", dbName)
	if err != nil {
		return fmt.Errorf("%w: Unable to get cluster status for: %s, stderr: %s, err: %v", DBError, db, stderr, err)
	}

	r = regexp.MustCompile(`([a-z0-9]{4}) at ` + dbServerRegexp)
	members := r.FindAllStringSubmatch(out, -1)
	kickedMembersCount := 0
	dbAppLabel := map[string]string{"ovn-db-pod": "true"}
	dbPods, err := kclient.GetPods(config.Kubernetes.OVNConfigNamespace,
		metav1.LabelSelector{
			MatchLabels: dbAppLabel,
		})
	if err != nil {
		return fmt.Errorf("unable to get db pod list from kubeclient: %v", err)
	}
	for _, member := range members {
		if len(member) < 5 {
			return fmt.Errorf("unable to parse member in %s: %s", db, member)
		}
		matchedServer := member[4]
		if !(utilnet.IsIPv4String(matchedServer) || utilnet.IsIPv6String(matchedServer)) {
			klog.Warningf("Unable to parse address portion of member entry in %s: %s",
				db, matchedServer)
			continue
		}
		memberFound := false
		for _, knownServer := range knownServers {
			if knownServer == matchedServer {
				memberFound = true
				break
			}
		}
		// check if there's a db pod with the same IP address, then it's a match
		if !memberFound {
			for _, dbPod := range dbPods.Items {
				for _, ip := range dbPod.Status.PodIPs {
					if ip.IP == matchedServer {
						memberFound = true
						break
					}
				}
			}
		}
		if !memberFound && (len(members)-kickedMembersCount) > 3 {
			// unknown member and we have enough members its safe to kick the unknown address
			klog.Infof("Unknown Raft member found in %s: %s, %s... kicking", db, member[1], member[3])
			_, stderr, err = appCtl(5, "cluster/kick", dbName, member[1])
			if err != nil {
				// warn only: we might fail to kick since other nodes will also be trying to kick the member
				klog.Warningf("Error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, err: %v", member[1], member[3], db, stderr, err)
				continue
			}
			kickedMembersCount = kickedMembersCount + 1
		}
	}
	return nil
}

// ensureElectionTimeout ensures that the election timer is increased on the leader only
// the election timer can be raised to max 2 times the current election timer per call of this function
func ensureElectionTimeout(db *dbProperties) error {
	out, stderr, err := db.appCtl(5, "cluster/status", db.dbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get cluster status for: %s, stderr: %v, err: %v", DBError, db.dbName, stderr, err)
	}

	if !strings.Contains(out, "Role: leader") { // we only update on the leader
		return nil
	}

	r := regexp.MustCompile(`Election timer: (\d+)`)
	match := r.FindStringSubmatch(out)
	if len(match) < 2 {
		return fmt.Errorf("failed to get current election timer for %s from status", db.dbName)
	}
	currentElectionTimer, err := strconv.Atoi(match[1])
	if err != nil {
		return fmt.Errorf("failed to convert election timer %v for %s", match[2], db.dbName)
	}
	if currentElectionTimer == db.electionTimer {
		return nil
	}

	maxElectionTimer := currentElectionTimer * 2
	if db.electionTimer <= maxElectionTimer {
		_, stderr, err := db.appCtl(5, "cluster/change-election-timer", db.dbName, fmt.Sprint(db.electionTimer))
		if err != nil {
			return fmt.Errorf("failed to change election timer for %s %v %v", db.dbName, err, stderr)
		}
	} else {
		_, stderr, err = db.appCtl(5, "cluster/change-election-timer", db.dbName, fmt.Sprint(maxElectionTimer))
		if err != nil {
			return fmt.Errorf("failed to change election timer for %s %v %v", db.dbName, err, stderr)
		}
	}
	return nil
}

func resetRaftDB(db string) {
	// backup the db by renaming it and then stop the nb/sb ovsdb process.
	dbFile := filepath.Base(db)
	backupFile := strings.TrimSuffix(dbFile, filepath.Ext(dbFile)) +
		time.Now().UTC().Format("2006-01-02_150405") + "db_bak"
	backupDB := filepath.Join(filepath.Dir(db), backupFile)
	err := os.Rename(db, backupDB)
	if err != nil {
		klog.Warningf("Failed to back up the db to backupFile: %s", backupFile)
	} else {
		klog.Infof("Backed up the db to backupFile: %s", backupFile)
		var dbName string
		var appCtl func(timeout int, args ...string) (string, string, error)
		if strings.Contains(db, "ovnnb") {
			dbName = "OVN_Northbound"
			appCtl = util.RunOVNNBAppCtlWithTimeout
		} else {
			dbName = "OVN_Southbound"
			appCtl = util.RunOVNSBAppCtlWithTimeout
		}
		_, stderr, err := appCtl(5, "exit")
		if err != nil {
			klog.Warningf("Unable to restart the ovn db: %s ,"+
				"stderr: %v, err: %v", dbName, stderr, err)
		}
		klog.Infof("Stopped %s db after backing up the db: %s", dbName, backupFile)
	}
}

// EnableDBMemTrimming enables memory trimming on DB compaction for NBDB and SBDB. Every 10 minutes the DBs are compacted
// and excess memory on the heap is freed. By enabling memory trimming, the freed memory will be returned back to the OS
func EnableDBMemTrimming() error {
	out, stderr, err := util.RunOVNNBAppCtlWithTimeout(5, "list-commands")
	if err != nil {
		return fmt.Errorf("unable to list supported commands for ovn-appctl, stderr: %s, error: %v", stderr, err)
	}
	if !strings.Contains(out, "memory-trim-on-compaction") {
		klog.Warning("memory-trim-on-compaction unsupported in this version of OVN. OVN DBs may experience high " +
			"memory growth")
		return nil
	}
	_, stderr, err = util.RunOVNNBAppCtlWithTimeout(5, "ovsdb-server/memory-trim-on-compaction", "on")
	if err != nil {
		return fmt.Errorf("unable to turn on memory trimming for NB DB, stderr: %s, error: %v", stderr, err)
	}
	_, stderr, err = util.RunOVNSBAppCtlWithTimeout(5, "ovsdb-server/memory-trim-on-compaction", "on")
	if err != nil {
		return fmt.Errorf("unable to turn on memory trimming for SB DB, stderr: %s, error: %v", stderr, err)
	}
	return nil
}

func propertiesForDB(db string) *dbProperties {
	if strings.Contains(db, "ovnnb") {
		return &dbProperties{
			electionTimer: int(config.OvnNorth.ElectionTimer) * 1000,
			appCtl:        util.RunOVNNBAppCtlWithTimeout,
			dbName:        "OVN_Northbound",
		}
	}
	return &dbProperties{
		electionTimer: int(config.OvnSouth.ElectionTimer) * 1000,
		appCtl:        util.RunOVNSBAppCtlWithTimeout,
		dbName:        "OVN_Southbound",
	}
}

func upgradeNBDBSchema() error {
	return upgradeDBSchema(nbdbSchema, nbdbServerSock, "OVN_Northbound")
}

func upgradeSBDBSchema() error {
	return upgradeDBSchema(sbdbSchema, sbdbServerSock, "OVN_Southbound")
}

func upgradeDBSchema(schemaFile, serverSock, dbName string) error {
	if _, err := os.Stat(schemaFile); err != nil {
		return err
	}

	stdout, stderr, err := util.RunOVSDBTool("schema-version", schemaFile)
	if err != nil {
		return fmt.Errorf("failed to get schema name: %s, %w", stderr, err)
	}
	schemaTarget := strings.TrimSpace(stdout)

	stdout, stderr, err = util.RunOVSDBClient("-t", "10", "get-schema-version", serverSock, dbName)
	if err != nil {
		return fmt.Errorf("failed to get schema version for NBDB, stderr: %q, error: %w", stderr, err)
	}
	dbSchemaVersion := strings.TrimSpace(stdout)

	_, _, err = util.RunOVSDBTool("compare-versions", dbSchemaVersion, "<", schemaTarget)
	if err != nil {
		klog.Infof("No %s DB schema upgrade is required. Current version: %s, target: %s",
			dbName, dbSchemaVersion, schemaTarget)
		return nil
	}

	_, stderr, err = util.RunOVSDBClient("-t", "30", "convert", serverSock, schemaFile)
	if err != nil {
		return fmt.Errorf("failed to upgrade schema, stderr: %q, error: %w", stderr, err)
	}

	klog.Infof("%s DB schema successfully upgraded from: %q, to %q", dbName, dbSchemaVersion, schemaTarget)
	return nil
}
