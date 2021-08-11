package loadbalancer

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// EnsureLBs provides a generic load-balancer reconciliation engine.
//
// It assures that, for a given set of ExternalIDs, only the configured
// list of load balancers exist. Existing load-balancers will be updated,
// new ones will be created as needed, and stale ones will be deleted.
//
// For example, you might want to ensure that service ns/foo has the
// correct set of load balancers. You would call it with something like
//
//     EnsureLBs( { kind: Service, owner: ns/foo}, { {Name: Service_ns/foo_cluster_tcp, ...}})
//
// This will ensure that, for this example, only that one LB exists and
// has the desired configuration.
//
// It will commit all updates in a single transaction, so updates will be
// atomic and users should see no disruption. However, concurrent calls
// that modify the same externalIDs are not allowed.
//
// It is assumed that names are meaningful and somewhat stable, to minimize churn. This
// function doesn't work with Load_Balancers without a name.
func EnsureLBs(externalIDs map[string]string, LBs []LB) error {
	lbCache, err := GetLBCache()
	if err != nil {
		return fmt.Errorf("failed initialize LBcache: %w", err)
	}

	existing := lbCache.Find(externalIDs)
	existingByName := make(map[string]*CachedLB, len(existing))
	toDelete := sets.NewString()

	for _, lb := range existing {
		existingByName[lb.Name] = lb
		toDelete.Insert(lb.UUID)
	}

	createdUUIDs := sets.NewString()

	txn := util.NewNBTxn()

	// create or update each LB, logging UUID created (so we can clean up)
	// missing load balancers will be created immediately
	// existing load-balancers will be updated in the transaction
	for i, lb := range LBs {
		uuid, err := ensureLB(txn, lbCache, &lb, existingByName[lb.Name])
		if err != nil {
			return err
		}
		createdUUIDs.Insert(uuid)
		toDelete.Delete(uuid)
		LBs[i].UUID = uuid
	}

	uuidsToDelete := make([]string, 0, len(toDelete))
	for uuid := range toDelete {
		uuidsToDelete = append(uuidsToDelete, uuid)
	}
	if err := DeleteLBs(txn, uuidsToDelete); err != nil {
		return fmt.Errorf("failed to delete %d stale load balancers for %#v: %w",
			len(uuidsToDelete), externalIDs, err)
	}

	_, _, err = txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit load balancer changes for %#v: %w", externalIDs, err)
	}
	klog.V(5).Infof("Deleted %d stale LBs for %#v", len(uuidsToDelete), externalIDs)

	lbCache.update(LBs, uuidsToDelete)
	return nil
}

// ensureLB creates or updates a load balancer as necessary.
// TODO: make this use libovsdb and generally be more efficient
// returns the uuid of the LB
func ensureLB(txn *util.NBTxn, lbCache *LBCache, lb *LB, existing *CachedLB) (string, error) {
	uuid := ""
	if existing == nil {
		cmds := []string{
			"create", "load_balancer",
		}
		cmds = append(cmds, lbToColumns(lb)...)
		// note: load-balancer creation is not in the transaction
		stdout, _, err := util.RunOVNNbctl(cmds...)
		if err != nil {
			return "", fmt.Errorf("failed to create load_balancer %s: %w", lb.Name, err)
		}
		uuid = stdout
		lb.UUID = uuid

		// Since this short-cut the transation, immediately add it to the cache.
		lbCache.addNewLB(lb)
	} else {
		cmds := []string{
			"set", "load_balancer", existing.UUID,
		}
		cmds = append(cmds, lbToColumns(lb)...)
		_, _, err := txn.AddOrCommit(cmds)
		if err != nil {
			return "", fmt.Errorf("failed to update load_balancer %s: %w", lb.Name, err)
		}
		uuid = existing.UUID
		lb.UUID = uuid
	}

	// List existing routers and switches, to see if there are any for which we should remove
	existingRouters := sets.String{}
	existingSwitches := sets.String{}
	if existing != nil {
		existingRouters = existing.Routers
		existingSwitches = existing.Switches
	}

	wantRouters := sets.NewString(lb.Routers...)
	wantSwitches := sets.NewString(lb.Switches...)

	// add missing switches
	for _, sw := range wantSwitches.Difference(existingSwitches).List() {
		_, _, err := txn.AddOrCommit([]string{"--may-exist", "ls-lb-add", sw, uuid})
		if err != nil {
			return uuid, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}
	// remove old switches
	for _, sw := range existingSwitches.Difference(wantSwitches).List() {
		_, _, err := txn.AddOrCommit([]string{"--if-exists", "ls-lb-del", sw, uuid})
		if err != nil {
			return uuid, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}

	// add missing routers
	for _, rtr := range wantRouters.Difference(existingRouters).List() {
		_, _, err := txn.AddOrCommit([]string{"--may-exist", "lr-lb-add", rtr, uuid})
		if err != nil {
			return uuid, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}
	// remove old routers
	for _, rtr := range existingRouters.Difference(wantRouters).List() {
		_, _, err := txn.AddOrCommit([]string{"--if-exists", "lr-lb-del", rtr, uuid})
		if err != nil {

			return uuid, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}

	return uuid, nil
}

// lbToColumns turns a load balancer in to a set of column arguments
// that can be passed to nbctl create or set
func lbToColumns(lb *LB) []string {
	reject := "true"
	event := "false"

	if lb.Opts.Unidling {
		reject = "false"
		event = "true"
	}

	skipSNAT := "false"
	if lb.Opts.SkipSNAT {
		skipSNAT = "true"
	}

	// Session affinity
	// If enabled, then bucket flows by 3-tuple (proto, srcip, dstip)
	// otherwise, use default ovn value
	selectionFields := "[]" // empty set
	if lb.Opts.Affinity {
		selectionFields = "ip_src,ip_dst"
	}

	// vipSet
	vipSet := make([]string, 0, len(lb.Rules))
	for _, rule := range lb.Rules {
		vipSet = append(vipSet, rule.nbctlString())
	}

	out := []string{
		"name=" + lb.Name,
		"protocol=" + strings.ToLower(lb.Protocol),
		"selection_fields=" + selectionFields,
		"options:reject=" + reject,
		"options:event=" + event,
		"options:skip_snat=" + skipSNAT,
		fmt.Sprintf(`vips={%s}`, strings.Join(vipSet, ",")),
	}

	for k, v := range lb.ExternalIDs {
		out = append(out, "external_ids:"+k+"="+v)
	}

	// for unit testing - stable order
	sort.Strings(out)

	return out
}

// Returns a nbctl column update string for this rule
func (r *LBRule) nbctlString() string {
	tgts := make([]string, 0, len(r.Targets))
	for _, tgt := range r.Targets {
		tgts = append(tgts, tgt.String())
	}

	return fmt.Sprintf(`"%s"="%s"`,
		r.Source.String(),
		strings.Join(tgts, ","))
}

// DeleteLBs deletes all load balancer uuids supplied
// Note: this also automatically removes them from the switches and the routers :-)
func DeleteLBs(txn *util.NBTxn, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}

	cache, err := GetLBCache()
	if err != nil {
		return err
	}

	commit := false
	if txn == nil {
		txn = util.NewNBTxn()
		commit = true
	}

	args := append([]string{"--if-exists", "destroy", "Load_Balancer"}, uuids...)

	_, _, err = txn.AddOrCommit(args)
	if err != nil {
		return err
	}
	if commit {
		if _, _, err := txn.Commit(); err != nil {
			return err
		}
		cache.update(nil, uuids)

	}
	return err
}

type DeleteVIPEntry struct {
	LBUUID string
	VIPs   []string // ip:string (or v6 equivalent)
}

// DeleteLoadBalancerVIPs removes VIPs from load-balancers in a single shot.
func DeleteLoadBalancerVIPs(toRemove []DeleteVIPEntry) error {
	lbCache, err := GetLBCache()
	if err != nil {
		return err
	}

	txn := util.NewNBTxn()

	for _, entry := range toRemove {
		if len(entry.VIPs) == 0 {
			continue
		}

		vipsStr := strings.Builder{}
		// neat trick: leading spaces don't matter
		for _, vip := range entry.VIPs {
			vipsStr.WriteString(fmt.Sprintf(` "%s"`, vip))
		}

		_, _, err := txn.AddOrCommit([]string{
			"--if-exists", "remove", "load_balancer", entry.LBUUID, "vips", vipsStr.String(),
		})
		if err != nil {
			return fmt.Errorf("failed to remove vips from load_balancer: %w", err)
		}
	}

	_, _, err = txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to remove vips from load_balancer: %w", err)
	}

	lbCache.removeVips(toRemove)

	return nil
}
