package loadbalancer

import (
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

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
func EnsureLBs(nbClient libovsdbclient.Client, externalIDs map[string]string, LBs []LB) error {
	lbCache, err := GetLBCache(nbClient)
	if err != nil {
		return fmt.Errorf("failed initialize LBcache: %w", err)
	}

	existingByName := lbCache.Find(externalIDs)
	toDelete := sets.NewString()

	for name, lb := range existingByName {
		if lb.UUIDs.Len() > 1 {
			// Multiple UUIDs found for the same load balancer name, this shouldn't happen
			// see https://bugzilla.redhat.com/show_bug.cgi?id=2042001
			// remove all uuids associated with this load balancer
			klog.Errorf("Multiple OVN load balancer UUIDs detected for load balancer name: %s. UUIDs: %s",
				name, lb.UUIDs)
			delete(existingByName, name)
		}
		toDelete.Insert(name)
	}

	lbs := make([]*nbdb.LoadBalancer, 0, len(LBs))
	existinglbs := make([]*nbdb.LoadBalancer, 0, len(LBs))
	newlbs := make([]*nbdb.LoadBalancer, 0, len(LBs))
	addLBsToSwitch := map[string][]*nbdb.LoadBalancer{}
	removeLBsFromSwitch := map[string][]*nbdb.LoadBalancer{}
	addLBsToRouter := map[string][]*nbdb.LoadBalancer{}
	removesLBsFromRouter := map[string][]*nbdb.LoadBalancer{}
	addLBsToGroups := map[string][]*nbdb.LoadBalancer{}
	removeLBsFromGroups := map[string][]*nbdb.LoadBalancer{}
	wantedByName := make(map[string]*LB, len(LBs))
	for i, lb := range LBs {
		wantedByName[lb.Name] = &LBs[i]
		blb := buildLB(&lb)
		lbs = append(lbs, blb)
		existingRouters := sets.String{}
		existingSwitches := sets.String{}
		existingGroups := sets.String{}
		if existingLB, ok := existingByName[lb.Name]; ok {
			// there must only be one existing UUID here or the LB would have been deleted
			// earlier in the function
			blb.UUID = existingLB.UUIDs.UnsortedList()[0]
			existinglbs = append(existinglbs, blb)
			toDelete.Delete(existingLB.Name)
			existingRouters = existingLB.Routers
			existingSwitches = existingLB.Switches
			existingGroups = existingLB.Groups
		} else {
			newlbs = append(newlbs, blb)
		}
		wantRouters := sets.NewString(lb.Routers...)
		wantSwitches := sets.NewString(lb.Switches...)
		wantGroups := sets.NewString(lb.Groups...)
		mapLBDifferenceByKey(addLBsToSwitch, wantSwitches, existingSwitches, blb)
		mapLBDifferenceByKey(removeLBsFromSwitch, existingSwitches, wantSwitches, blb)
		mapLBDifferenceByKey(addLBsToRouter, wantRouters, existingRouters, blb)
		mapLBDifferenceByKey(removesLBsFromRouter, existingRouters, wantRouters, blb)
		mapLBDifferenceByKey(addLBsToGroups, wantGroups, existingGroups, blb)
		mapLBDifferenceByKey(removeLBsFromGroups, existingGroups, wantGroups, blb)
	}

	ops, err := libovsdbops.CreateOrUpdateLoadBalancersOps(nbClient, nil, existinglbs...)
	if err != nil {
		return fmt.Errorf("failed to create or update load balancer ops: %v", err)
	}

	ops, err = libovsdbops.CreateLoadBalancersOps(nbClient, ops, newlbs...)
	if err != nil {
		return fmt.Errorf("failed to create or load balancer ops: %v", err)
	}

	// cache switches for this round of ops
	lswitches := map[string]*nbdb.LogicalSwitch{}
	getSwitch := func(name string) *nbdb.LogicalSwitch {
		var lswitch *nbdb.LogicalSwitch
		var found bool
		if lswitch, found = lswitches[name]; !found {
			lswitch = &nbdb.LogicalSwitch{Name: name}
			lswitches[name] = lswitch
		}
		return lswitch
	}
	for k, v := range addLBsToSwitch {
		ops, err = libovsdbops.AddLoadBalancersToSwitchOps(nbClient, ops, getSwitch(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make load balancer to switch ops: %v", err)
		}
	}
	for k, v := range removeLBsFromSwitch {
		ops, err = libovsdbops.RemoveLoadBalancersFromSwitchOps(nbClient, ops, getSwitch(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make remove load balancer to switch ops: %v", err)
		}
	}

	// cache routers for this round of ops
	routers := map[string]*nbdb.LogicalRouter{}
	getRouter := func(name string) *nbdb.LogicalRouter {
		var router *nbdb.LogicalRouter
		var found bool
		if router, found = routers[name]; !found {
			router = &nbdb.LogicalRouter{Name: name}
			routers[name] = router
		}
		return router
	}
	for k, v := range addLBsToRouter {
		ops, err = libovsdbops.AddLoadBalancersToRouterOps(nbClient, ops, getRouter(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make load balancer to router ops: %v", err)
		}
	}
	for k, v := range removesLBsFromRouter {
		ops, err = libovsdbops.RemoveLoadBalancersFromRouterOps(nbClient, ops, getRouter(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make remove load balancer to router ops: %v", err)
		}
	}

	// cache groups for this round of ops
	groups := map[string]*nbdb.LoadBalancerGroup{}
	getGroup := func(name string) *nbdb.LoadBalancerGroup {
		var group *nbdb.LoadBalancerGroup
		var found bool
		if group, found = groups[name]; !found {
			group = &nbdb.LoadBalancerGroup{Name: name}
			groups[name] = group
		}
		return group
	}
	for k, v := range addLBsToGroups {
		ops, err = libovsdbops.AddLoadBalancersToGroupOps(nbClient, ops, getGroup(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make load balancer to group ops: %v", err)
		}
	}
	for k, v := range removeLBsFromGroups {
		ops, err = libovsdbops.RemoveLoadBalancersFromGroupOps(nbClient, ops, getGroup(k), v...)
		if err != nil {
			return fmt.Errorf("failed to make remove load balancer to group ops: %v", err)
		}
	}

	deleteLBs := make([]*nbdb.LoadBalancer, 0, len(toDelete))
	for name := range toDelete {
		uuids := lbCache.Get(name)
		for _, uuid := range uuids {
			deleteLBs = append(deleteLBs, &nbdb.LoadBalancer{UUID: uuid})
		}
	}
	ops, err = libovsdbops.DeleteLoadBalancersOps(nbClient, ops, deleteLBs...)
	if err != nil {
		return fmt.Errorf("failed to make delete load balancer to ops: %v", err)
	}

	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(nbClient, lbs, ops)
	if err != nil {
		return fmt.Errorf("failed to execute load balancer ovn txn: %v", err)
	}

	for _, lb := range lbs {
		wantedByName[lb.Name].UUID = lb.UUID
	}

	lbCache.update(LBs, toDelete.UnsortedList())
	klog.V(5).Infof("Deleted %d stale LBs for %#v", len(toDelete), externalIDs)

	return nil
}

// LoadBalancersEqualNoUUID compares load balancer objects excluding uuid
func LoadBalancersEqualNoUUID(lbs1, lbs2 []LB) bool {
	if len(lbs1) != len(lbs2) {
		return false
	}
	new1 := make([]LB, len(lbs1))
	new2 := make([]LB, len(lbs2))
	for _, lb := range lbs1 {
		lb.UUID = ""
		new1 = append(new1, lb)

	}
	for _, lb := range lbs2 {
		lb.UUID = ""
		new2 = append(new2, lb)
	}
	return reflect.DeepEqual(new1, new2)
}

func mapLBDifferenceByKey(keyMap map[string][]*nbdb.LoadBalancer, keyIn sets.String, keyNotIn sets.String, lb *nbdb.LoadBalancer) {
	for _, k := range keyIn.Difference(keyNotIn).UnsortedList() {
		l := keyMap[k]
		if l == nil {
			l = []*nbdb.LoadBalancer{}
		}
		l = append(l, lb)
		keyMap[k] = l
	}
}

func buildLB(lb *LB) *nbdb.LoadBalancer {
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

	options := map[string]string{
		"reject":    reject,
		"event":     event,
		"skip_snat": skipSNAT,
	}

	// Session affinity
	// If enabled, then bucket flows by 3-tuple (proto, srcip, dstip)
	// otherwise, use default ovn value
	selectionFields := []nbdb.LoadBalancerSelectionFields{}
	if lb.Opts.Affinity {
		selectionFields = []string{
			nbdb.LoadBalancerSelectionFieldsIPSrc,
			nbdb.LoadBalancerSelectionFieldsIPDst,
		}
	}

	// vipMap
	vips := buildVipMap(lb.Rules)

	return libovsdbops.BuildLoadBalancer(lb.Name, strings.ToLower(lb.Protocol), selectionFields, vips, options, lb.ExternalIDs)
}

// buildVipMap returns a viups map from a set of rules
func buildVipMap(rules []LBRule) map[string]string {
	vipMap := make(map[string]string, len(rules))
	for _, r := range rules {
		tgts := make([]string, 0, len(r.Targets))
		for _, tgt := range r.Targets {
			tgts = append(tgts, tgt.String())
		}
		vipMap[r.Source.String()] = strings.Join(tgts, ",")
	}

	return vipMap
}

// DeleteLBs deletes all load balancer uuids supplied
// Note: this also automatically removes them from the switches, routers, and the groups :-)
func DeleteLBs(nbClient libovsdbclient.Client, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}

	cache, err := GetLBCache(nbClient)
	if err != nil {
		return err
	}

	lbs := make([]*nbdb.LoadBalancer, 0, len(uuids))
	for _, uuid := range uuids {
		lbs = append(lbs, &nbdb.LoadBalancer{UUID: uuid})
	}

	err = libovsdbops.DeleteLoadBalancers(nbClient, lbs)
	if err != nil {
		return err
	}

	cache.update(nil, uuids)
	return nil
}

type DeleteVIPEntry struct {
	LBName string
	VIPs   []string // ip:string (or v6 equivalent)
}

// DeleteLoadBalancerVIPs removes VIPs from load-balancers in a single shot.
func DeleteLoadBalancerVIPs(nbClient libovsdbclient.Client, toRemove []DeleteVIPEntry) error {
	lbCache, err := GetLBCache(nbClient)
	if err != nil {
		return err
	}

	var ops []libovsdb.Operation
	for _, entry := range toRemove {
		uuids := lbCache.Get(entry.LBName)
		if len(uuids) > 1 {
			klog.Warningf("Multiple load balancers found for load balancer: %s", entry.LBName)
		}
		for _, uuid := range uuids {
			ops, err = libovsdbops.RemoveLoadBalancerVipsOps(nbClient, ops, &nbdb.LoadBalancer{UUID: uuid}, entry.VIPs...)
			if err != nil {
				return err
			}
		}
	}

	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to remove vips from load_balancer: %w", err)
	}

	lbCache.removeVips(toRemove)

	return nil
}
