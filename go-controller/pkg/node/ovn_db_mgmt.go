package node

import (
	"regexp"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (n *OvnNode) ensureOvnRaft(db string) {
	ticker := time.NewTicker(60 * time.Second)
	klog.Infof("Starting ensure routine for Raft db: %s", db)
	kclient := n.Kube.(*kube.Kube)
	for {
		select {
		case <-ticker.C:
			if dbOnNode, _ := metrics.CheckPodRunsOnGivenNode(kclient.KClient.(*kubernetes.Clientset),
				"name=ovnkube-db", n.name, false); dbOnNode {
				klog.Infof("Node %s has a OVN DB pod, ensuring OVN Raft membership", n.name)
				n.ensureLocalRaftServerID(db)
				n.ensureClusterRaftMembership(db)
			}
		case <-n.stopChan:
			ticker.Stop()
			return
		}
	}
}

// ensureLocalRaftServerID is used to ensure there is no stale member in the Raft cluster with our address
func (n *OvnNode) ensureLocalRaftServerID(db string) {
	var dbName string
	var appCtl func(args ...string) (string, string, error)

	if strings.Contains(db, "ovnnb") {
		dbName = "OVN_Northbound"
		appCtl = util.RunOVNNBAppCtl
	} else {
		dbName = "OVN_Southbound"
		appCtl = util.RunOVNSBAppCtl
	}

	out, stderr, err := util.RunOVSDBTool("db-sid", db)
	if err != nil {
		klog.Warningf("Unable to get db server ID for: %s, stderr: %v, err: %v", db, stderr, err)
		return
	}
	if len(out) < 4 {
		klog.Errorf("Invalid db id found: %s for db: %s", out, db)
		return
	}
	// server ID in raft membership is only first 4 char prefix
	serverID := out[:4]
	out, stderr, err = appCtl("cluster/status", dbName)
	if err != nil {
		klog.Warningf("Unable to get cluster status for: %s, stderr: %v, err: %v", db, stderr, err)
		return
	}
	r, _ := regexp.Compile(`Address: *((ssl|tcp):[?[a-z0-9.:]+]?)`)
	matches := r.FindStringSubmatch(out)
	if len(matches) < 2 {
		klog.Warningf("Unable to parse Address for db: %s, output: %s", db, out)
		return
	}
	addr := matches[1]
	// look for current servers in raft cluster with the same address
	r, _ = regexp.Compile("([a-z0-9]{4}) at " + addr)
	members := r.FindAllStringSubmatch(out, -1)
	for _, member := range members {
		if len(member) < 2 {
			klog.Warningf("Unable to find server id submatch in %s", member)
			return
		}
		if member[1] != serverID {
			// stale entry found for this node with same adddress, need to kick
			klog.Infof("Previous stale member found: %s...kicking", member[1])
			_, stderr, err = appCtl("cluster/kick", dbName, member[1])
			if err != nil {
				klog.Errorf("Error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, error: %v", member[1], addr, db, stderr, err)
			}
		}
	}
}

// ensureClusterRaftMembership ensures there are no unknown members in the current Raft cluster
func (n *OvnNode) ensureClusterRaftMembership(db string) {
	var knownMembers []string
	var dbName string
	var appCtl func(args ...string) (string, string, error)

	if strings.Contains(db, "ovnnb") {
		dbName = "OVN_Northbound"
		appCtl = util.RunOVNNBAppCtl
		knownMembers = strings.Split(config.OvnNorth.Address, ",")
	} else {
		dbName = "OVN_Southbound"
		appCtl = util.RunOVNSBAppCtl
		knownMembers = strings.Split(config.OvnSouth.Address, ",")
	}

	out, stderr, err := appCtl("cluster/status", dbName)
	if err != nil {
		klog.Warningf("Unable to get cluster status for: %s, stderr: %v, err: %v", db, stderr, err)
		return
	}
	r, _ := regexp.Compile(`([a-z0-9]{4}) at ((ssl|tcp):\[?[a-z0-9.:]+\]?)`)
	members := r.FindAllStringSubmatch(out, -1)
	for _, member := range members {
		if len(member) < 3 {
			klog.Warningf("Unable to find parse member: %s", member)
			return
		}
		memberFound := false
		for _, knownMember := range knownMembers {
			if knownMember == member[2] {
				memberFound = true
				break
			}
		}
		if !memberFound && len(knownMembers) > 3 {
			// unknown member and we have enough members its safe to kick the unknown address
			klog.Infof("Unknown Raft member found: %s, %s...kicking", member[1], member[2])
			_, stderr, err = appCtl("cluster/kick", dbName, member[1])
			if err != nil {
				// warn only: we might fail to kick since other nodes will also be trying to kick the member
				klog.Warningf("Error while kicking old Raft member: %s, for address: %s in db: %s,"+
					"stderr: %v, err: %v", member[1], member[2], db, stderr, err)
			}
		}
	}
}
