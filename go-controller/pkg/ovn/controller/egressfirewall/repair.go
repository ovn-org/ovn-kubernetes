package egressfirewall

import (
	"fmt"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	eflister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

// Repair is a controller loop that can work a one-shot or periodic mode
// it checks the OVN EgressFirewalls and deletes the ones that do not exist in
// the Kubernetes EgressFirewall Informer cache.
// Based on:
// https://raw.githubusercontent.com/kubernetes/kubernetes/release-1.19/pkg/registry/core/service/ipallocator/controller/repair.go
type Repair struct {
	interval     time.Duration
	kube         kube.Interface
	efLister     eflister.EgressFirewallLister
	watchFactory *factory.WatchFactory
}

func NewRepair(interval time.Duration, kube kube.Interface, efLister eflister.EgressFirewallLister, watchFactory *factory.WatchFactory) *Repair {
	return &Repair{
		interval:     interval,
		kube:         kube,
		efLister:     efLister,
		watchFactory: watchFactory,
	}
}

// runOnce verifies the state of the cluster EgressFirewalls and returns an error if an unrecoverable problem occurs
func (r *Repair) runOnce() error {
	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for egressFirewalls")
	defer func() {
		klog.V(4).Infof("Finished repairing loop for egressFirewalls")
		metrics.MetricSyncEgressFirewallLatency.WithLabelValues("repair-loop").Observe(time.Since(startTime).Seconds())
	}()

	if config.Gateway.Mode == config.GatewayModeShared {
		// Mode is shared gateway mode, make sure to delete all ACLs on the node switches
		egressFirewallACLIDs, stderr, err := util.RunOVNNbctl(
			"--data=bare",
			"--no-heading",
			"--columns=_uuid",
			"--format=table",
			"find",
			"acl",
			fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
			fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
		)
		if err != nil {
			return fmt.Errorf("unable to list egress firewall logical router policies, cannot cleanup old stale data, stderr: %s, err: %v", stderr, err)
		}
		if egressFirewallACLIDs != "" {
			nodes, err := r.watchFactory.GetNodes()
			if err != nil {
				return fmt.Errorf("unable to cleanup egress firewall ACLs remaining from local gateway mode, cannot list nodes, err: %v", err)
			}
			logicalSwitches := []string{}
			for _, node := range nodes {
				logicalSwitches = append(logicalSwitches, node.Name)
			}
			for _, logicalSwitch := range logicalSwitches {
				switchACLs, stderr, err := util.RunOVNNbctl(
					"--data=bare",
					"--no-heading",
					"--columns=acls",
					"list",
					"logical_switch",
					logicalSwitch,
				)
				if err != nil {
					klog.Errorf("Unable to remove egress firewall acl, cannot list ACLs on switch: %s, stderr: %s, err: %v", logicalSwitch, stderr, err)
				}
				for _, egressFirewallACLID := range strings.Split(egressFirewallACLIDs, "\n") {
					if strings.Contains(switchACLs, egressFirewallACLID) {
						_, stderr, err := util.RunOVNNbctl(
							"remove",
							"logical_switch",
							logicalSwitch,
							"acls",
							egressFirewallACLID,
						)
						if err != nil {
							klog.Errorf("Unable to remove egress firewall acl: %s on %s, cannot cleanup old stale data, stderr: %s, err: %v", egressFirewallACLID, logicalSwitch, stderr, err)
						}
					}
				}
			}
		}
	}

	egressFirewallACLIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=_uuid",
		"--format=table",
		"find",
		"acl",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
		fmt.Sprintf("direction=%s", types.DirectionFromLPort),
	)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall logical router policies, cannot convert old ACL data, stderr: %s, err: %v", stderr, err)
	}
	if egressFirewallACLIDs != "" {
		for _, egressFirewallACLID := range strings.Split(egressFirewallACLIDs, "\n") {
			_, stderr, err := util.RunOVNNbctl(
				"set",
				"acl",
				egressFirewallACLID,
				fmt.Sprintf("direction=%s", types.DirectionToLPort),
			)
			if err != nil {
				klog.Errorf("Unable to set ACL direction on egress firewall acl: %s, cannot convert old ACL data, stderr: %s, err: %v", egressFirewallACLID, stderr, err)
			}
		}
	}

	// In any gateway mode, make sure to delete all LRPs on ovn_cluster_router.
	// This covers old local GW mode -> shared GW and old local GW mode -> new local GW mode
	egressFirewallPolicyIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=_uuid",
		"--format=table",
		"find",
		"logical_router_policy",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
	)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall logical router policies, cannot cleanup old stale data, stderr: %s, err: %v", stderr, err)
	}
	if egressFirewallPolicyIDs != "" {
		for _, egressFirewallPolicyID := range strings.Split(egressFirewallPolicyIDs, "\n") {
			_, stderr, err := util.RunOVNNbctl(
				"remove",
				"logical_router",
				types.OVNClusterRouter,
				"policies",
				egressFirewallPolicyID,
			)
			if err != nil {
				return fmt.Errorf("unable to remove egress firewall policy: %s on %s, cannot cleanup old stale data, stderr: %s, err: %v", egressFirewallPolicyID, types.OVNClusterRouter, stderr, err)
			}
		}
	}

	// sync the ovn and k8s egressFirewall states
	ovnEgressFirewallExternalIDs, stderr, err := util.RunOVNNbctl(
		"--data=bare",
		"--no-heading",
		"--columns=external_id",
		"--format=table",
		"find",
		"acl",
		fmt.Sprintf("priority<=%s", types.EgressFirewallStartPriority),
		fmt.Sprintf("priority>=%s", types.MinimumReservedEgressFirewallPriority),
	)
	if err != nil {
		return fmt.Errorf("cannot reconcile the state of egressfirewalls in ovn database and k8s. stderr: %s, err: %v", stderr, err)
	}
	splitOVNEgressFirewallExternalIDs := strings.Split(ovnEgressFirewallExternalIDs, "\n")

	// represents the namespaces that have firewalls according to  ovn
	ovnEgressFirewalls := make(map[string]struct{})

	for _, externalID := range splitOVNEgressFirewallExternalIDs {
		if strings.Contains(externalID, "egressFirewall=") {
			// Most egressFirewalls will have more then one ACL but we only need to know if there is one for the namespace
			// so a map is fine and we will add an entry every iteration but because it is a map will overwrite the previous
			// entry if it already existed
			ovnEgressFirewalls[strings.Split(externalID, "egressFirewall=")[1]] = struct{}{}
		}
	}

	// get all the k8s EgressFirewall Objects
	egressFirewallList, err := r.kube.GetEgressFirewalls()
	if err != nil {
		klog.Errorf("Cannot reconcile the state of egressfirewalls in ovn database and k8s. err: %v", err)
	}
	// delete entries from the map that exist in k8s and ovn
	txn := util.NewNBTxn()
	for _, egressFirewall := range egressFirewallList.Items {
		delete(ovnEgressFirewalls, egressFirewall.Namespace)
	}
	// any that are left are spurious and should be cleaned up
	for spuriousEF := range ovnEgressFirewalls {
		nodes, err := r.watchFactory.GetNodes()
		if err != nil {
			return fmt.Errorf("cannot reconcile the state of egrressfirewall in ovn database and k8s. err: %v", err)
		}
		err = deleteEgressFirewallRules(spuriousEF, nodes, txn)
		if err != nil {
			return fmt.Errorf("cannot fully reconcile the state of egressfirewalls ACLs for namespace %s still exist in ovn db: %v", spuriousEF, err)
		}
		_, stderr, err := txn.Commit()
		if err != nil {
			klog.Errorf("Cannot fully reconcile the state of egressfirewalls ACLs that still exist in ovn db: stderr: %q, err: %+v", stderr, err)
		}

	}

	return nil
}
