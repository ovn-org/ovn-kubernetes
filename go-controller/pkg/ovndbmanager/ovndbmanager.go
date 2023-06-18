package ovndbmanager

import (
	"context"
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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/asaskevich/govalidator"
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

func RunDBChecker(kclient kube.Interface, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	klog.Info("Starting DB Checker to ensure cluster membership and DB consistency")
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := convertNBDBSchema(); err != nil {
			klog.Fatalf("NBDB conversion failed: %v", err)
		}
		ensureOvnDBState(util.OvnNbdbLocation, kclient, stopCh)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := convertSBDBSchema(); err != nil {
			klog.Fatalf("SBDB conversion failed: %v", err)
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
	dbProperties, err := util.GetOvsDbProperties(db)
	if err != nil {
		klog.Fatalf("Failed to init db properties: %s", err)
		os.Exit(1)
	}

	var dbRetry int32

	for {
		select {
		case <-ticker.C:
			klog.V(5).Infof("Ensure routines for Raft db: %s kicked off by ticker", db)
			if err := ensureLocalRaftServerID(dbProperties); err != nil {
				klog.Error(err)
				if errors.Is(err, DBError) {
					updateDBRetryCounter(&dbRetry, dbProperties)
				}
			} else {
				dbRetry = 0
			}
			if err := ensureClusterRaftMembership(dbProperties, kclient); err != nil {
				klog.Error(err)
				if errors.Is(err, DBError) {
					updateDBRetryCounter(&dbRetry, dbProperties)
				}
			} else {
				dbRetry = 0
			}
			if dbProperties.ElectionTimer != 0 {
				if err := ensureElectionTimeout(dbProperties); err != nil {
					klog.Error(err)
					if errors.Is(err, DBError) {
						updateDBRetryCounter(&dbRetry, dbProperties)
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

func updateDBRetryCounter(retryCounter *int32, db *util.OvsDbProperties) {
	if *retryCounter > maxDBRetry {
		//delete the db file and start master
		err := resetRaftDB(db)
		if err != nil {
			klog.Warningf(err.Error())
		}
		*retryCounter = 0
	} else {
		*retryCounter += 1
		klog.Infof("Failed to get cluster status for: %s, number of retries: %d", db, *retryCounter)
	}
}

// ensureLocalRaftServerID is used to ensure there is no stale member in the Raft cluster with our address
func ensureLocalRaftServerID(db *util.OvsDbProperties) error {
	out, stderr, err := db.AppCtl(5, "cluster/sid", db.DbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get db server ID for: %s, stderr: %v, err: %v", DBError, db.DbAlias, stderr, err)
	}
	if len(out) < 4 {
		return fmt.Errorf("%w: invalid db id found: %s for db: %s", DBError, out, db.DbAlias)
	}
	// server ID in raft membership is only first 4 char prefix
	serverID := out[:4]
	out, stderr, err = db.AppCtl(5, "cluster/status", db.DbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get cluster status for: %s, stderr: %v, err: %v", DBError, db.DbAlias, stderr, err)
	}

	r := regexp.MustCompile(`Address: *((ssl|tcp):[?[a-z0-9\-.:]+]?)`)
	matches := r.FindStringSubmatch(out)
	if len(matches) < 2 {
		return fmt.Errorf("unable to parse Address for db: %s, output: %s", db.DbAlias, out)
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
			return fmt.Errorf("unable to find server id submatch in %s from %s", member, db.DbName)
		}
		if member[1] != serverID {
			// stale entry found for this node with same address, need to kick
			klog.Infof("Previous stale member found in %s: %s... kicking", db.DbName, member[1])
			_, stderr, err = db.AppCtl(5, "cluster/kick", db.DbName, member[1])
			if err != nil {
				return fmt.Errorf("error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, error: %v", member[1], addr, db.DbAlias, stderr, err)
			}
		}
	}
	return nil
}

// ensureClusterRaftMembership ensures there are no unknown members in the current Raft cluster
func ensureClusterRaftMembership(db *util.OvsDbProperties, kclient kube.Interface) error {
	var knownMembers, knownServers []string

	// IPv4 example: tcp:172.18.0.2:6641
	// IPv6 example: tcp:[fc00:f853:ccd:e793::3]:6642
	dbServerRegexp := `(ssl|tcp):(\[?([a-z0-9\-.:]+)\]?:\d+)`
	r := regexp.MustCompile(dbServerRegexp)

	if db.DbName == "OVN_Northbound" {
		knownMembers = strings.Split(config.OvnNorth.Address, ",")
	} else if db.DbName == "OVN_Southbound" {
		knownMembers = strings.Split(config.OvnSouth.Address, ",")
	} else {
		return fmt.Errorf("invalid database name %s for database %s", db.DbName, db.DbAlias)
	}
	for _, knownMember := range knownMembers {
		match := r.FindStringSubmatch(knownMember)
		if len(match) < 4 {
			klog.Warningf("Failed to parse known %s member: %s", db.DbName, knownMember)
			continue
		}
		server := match[3]
		if !(utilnet.IsIPv4String(server) || utilnet.IsIPv6String(server) || govalidator.IsDNSName(server)) {
			klog.Warningf("Found invalid value for IP address of known %s member %s: %s",
				db.DbName, knownMember, server)
			continue
		}
		knownServers = append(knownServers, server)
	}
	out, stderr, err := db.AppCtl(5, "cluster/status", db.DbName)
	if err != nil {
		return fmt.Errorf("%w: Unable to get cluster status for: %s, stderr: %s, err: %v", DBError, db.DbAlias, stderr, err)
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
			return fmt.Errorf("unable to parse member in %s: %s", db.DbName, member)
		}
		matchedServer := member[4]
		if !(utilnet.IsIPv4String(matchedServer) || utilnet.IsIPv6String(matchedServer) || govalidator.IsDNSName(matchedServer)) {
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
					if ip.IP == matchedServer || utilnet.ParseIPSloppy(ip.IP).String() == matchedServer {
						memberFound = true
						break
					}
				}
			}
		}
		if !memberFound && (len(members)-kickedMembersCount) > 3 {
			// unknown member and we have enough members its safe to kick the unknown address
			klog.Infof("Unknown Raft member found in %s: %s, %s... kicking", db.DbName, member[1], member[3])
			_, stderr, err = db.AppCtl(5, "cluster/kick", db.DbName, member[1])
			if err != nil {
				// warn only: we might fail to kick since other nodes will also be trying to kick the member
				klog.Warningf("Error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, err: %v", member[1], member[3], db.DbAlias, stderr, err)
				continue
			}
			kickedMembersCount = kickedMembersCount + 1
		}
	}
	return nil
}

// ensureElectionTimeout ensures that the election timer is increased on the leader only
// the election timer can be raised to max 2 times the current election timer per call of this function
func ensureElectionTimeout(db *util.OvsDbProperties) error {
	out, stderr, err := db.AppCtl(5, "cluster/status", db.DbName)
	if err != nil {
		return fmt.Errorf("%w: unable to get cluster status for: %s, stderr: %v, err: %v", DBError, db.DbName, stderr, err)
	}

	if !strings.Contains(out, "Role: leader") { // we only update on the leader
		return nil
	}

	r := regexp.MustCompile(`Election timer: (\d+)`)
	match := r.FindStringSubmatch(out)
	if len(match) < 2 {
		return fmt.Errorf("failed to get current election timer for %s from status", db.DbName)
	}
	currentElectionTimer, err := strconv.Atoi(match[1])
	if err != nil {
		return fmt.Errorf("failed to convert election timer %v for %s", match[2], db.DbName)
	}
	if currentElectionTimer == db.ElectionTimer {
		return nil
	}

	maxElectionTimer := currentElectionTimer * 2
	if db.ElectionTimer <= maxElectionTimer {
		_, stderr, err := db.AppCtl(5, "cluster/change-election-timer", db.DbName, fmt.Sprint(db.ElectionTimer))
		if err != nil {
			return fmt.Errorf("failed to change election timer for %s, %v, %v", db.DbName, err, stderr)
		}
	} else {
		_, stderr, err = db.AppCtl(5, "cluster/change-election-timer", db.DbName, fmt.Sprint(maxElectionTimer))
		if err != nil {
			return fmt.Errorf("failed to change election timer for %s, %v, %v", db.DbName, err, stderr)
		}
	}
	return nil
}

// resetRaftDB backs up the db by renaming it and then stops the nb/sb ovsdb process.
// Returns an error if anything goes wrong.
func resetRaftDB(db *util.OvsDbProperties) error {
	dbFile := filepath.Base(db.DbAlias)
	backupFile := strings.TrimSuffix(dbFile, filepath.Ext(dbFile)) +
		time.Now().UTC().Format("2006-01-02_150405") + "db_bak"
	backupDB := filepath.Join(filepath.Dir(db.DbAlias), backupFile)
	err := os.Rename(db.DbAlias, backupDB)
	if err != nil {
		return fmt.Errorf("failed to back up the db to backupFile: %s, error: %s", backupDB, err)
	}

	klog.Infof("Backed up the db to backupFile: %s", backupFile)
	_, stderr, err := db.AppCtl(5, "exit")
	if err != nil {
		return fmt.Errorf("unable to restart the ovn db: %s ,"+
			"stderr: %v, err: %v", db.DbName, stderr, err)
	}
	klog.Infof("Stopped %s db after backing up the db: %s", db.DbName, backupFile)

	return nil
}

func convertNBDBSchema() error {
	return convertDBSchemaWithRetries(nbdbSchema, nbdbServerSock, "OVN_Northbound")
}

func convertSBDBSchema() error {
	return convertDBSchemaWithRetries(sbdbSchema, sbdbServerSock, "OVN_Southbound")
}

func convertDBSchemaWithRetries(schemaFile, serverSock, dbName string) error {
	var lastMigrationErr error
	if err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		lastMigrationErr = convertDBSchema(schemaFile, serverSock, dbName)
		if lastMigrationErr != nil {
			klog.ErrorS(lastMigrationErr, dbName+" scheme conversion failed")
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("failed to convert db schema: %v. Error from last attempt: %w", err, lastMigrationErr)
	}

	return nil
}

func convertDBSchema(schemaFile, serverSock, dbName string) error {
	if _, err := os.Stat(schemaFile); err != nil {
		return err
	}

	// needs-conversion returns string (via stdout) 'yes' if a conversion is needed, and if not 'no'.
	isConversionRequired, _, err := util.RunOVSDBClient("needs-conversion", serverSock, schemaFile)
	if err != nil {
		return fmt.Errorf("failed to determine if cluster schema requires conversion: %w", err)
	}

	if isConversionRequired == "yes" {
		_, stderr, err := util.RunOVSDBClient("-t", "30", "convert", serverSock, schemaFile)
		if err != nil {
			return fmt.Errorf("failed to convert schema, stderr: %q, error: %w", stderr, err)
		}
		klog.Infof("%s DB schema successfully converted", dbName)
	}

	return nil
}
