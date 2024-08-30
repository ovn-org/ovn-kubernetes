package services

import (
	"fmt"
	"reflect"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/klog/v2"
)

// LB is a desired or existing load_balancer configuration in OVN.
type LB struct {
	Name        string
	UUID        string
	Protocol    string // one of TCP, UDP, SCTP
	ExternalIDs map[string]string
	Opts        LBOpts

	Rules []LBRule

	Templates TemplateMap // Templates that this LB uses as backends.

	// the names of logical switches, routers and LB groups that this LB should be attached to
	Switches []string
	Routers  []string
	Groups   []string
}

type LBOpts struct {
	// if the service should send back tcp REJECT in case of no endpoints
	Reject bool

	// if the service should raise empty_lb events
	EmptyLBEvents bool

	// If greater than 0, then enable per-client-IP affinity.
	AffinityTimeOut int32

	// If true, then disable SNAT entirely
	SkipSNAT bool

	// If true, this is a LB template.
	Template bool

	// Only useful for template LBs.
	AddressFamily corev1.IPFamily
}

type Addr struct {
	IP       string
	Port     int32
	Template *Template
}

type LBRule struct {
	Source  Addr
	Targets []Addr
}

func (a *Addr) String() string {
	if a.Template == nil {
		return util.JoinHostPortInt32(a.IP, a.Port)
	} else if a.Port != 0 {
		return fmt.Sprintf("%s:%d", a.Template.toReferenceString(), a.Port)
	} else {
		return a.Template.toReferenceString()
	}
}

// addrsToString joins together the textual representation of a list of Addr.
func addrsToString(addrs []Addr) string {
	s := ""
	for _, a := range addrs {
		s += a.String() + ","
	}
	s = strings.TrimRight(s, ",")
	return s
}

// templateLoadBalancer enriches a NB load balancer record with the
// associated template maps it requires provisioned in the NB database.
type templateLoadBalancer struct {
	nbLB      *nbdb.LoadBalancer
	templates TemplateMap
}

func toNBLoadBalancerList(tlbs []*templateLoadBalancer) []*nbdb.LoadBalancer {
	result := make([]*nbdb.LoadBalancer, 0, len(tlbs))
	for _, tlb := range tlbs {
		result = append(result, tlb.nbLB)
	}
	return result
}

func toNBTemplateList(tlbs []*templateLoadBalancer) []TemplateMap {
	templateVars := make([]TemplateMap, 0, len(tlbs))
	for _, tlb := range tlbs {
		templateVars = append(templateVars, tlb.templates)
	}
	return templateVars
}

// EnsureLBs provides a generic load-balancer reconciliation engine.
//
// It assures that, for a given set of ExternalIDs, only the configured
// list of load balancers exist. Existing load-balancers will be updated,
// new ones will be created as needed, and stale ones will be deleted.
//
// For example, you might want to ensure that service ns/foo has the
// correct set of load balancers. You would call it with something like
//
//	EnsureLBs( { kind: Service, owner: ns/foo}, { {Name: Service_ns/foo_cluster_tcp, ...}})
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
func EnsureLBs(nbClient libovsdbclient.Client, service *corev1.Service, existingCacheLBs []LB, LBs []LB, netInfo util.NetInfo) error {
	externalIDs := getExternalIDsForLoadBalancer(service, netInfo)
	existingByName := make(map[string]*LB, len(existingCacheLBs))
	toDelete := make(map[string]*LB, len(existingCacheLBs))

	for i := range existingCacheLBs {
		lb := &existingCacheLBs[i]
		existingByName[lb.Name] = lb
		toDelete[lb.UUID] = lb
	}

	tlbs := make([]*templateLoadBalancer, 0, len(LBs))
	addLBsToSwitch := map[string][]*templateLoadBalancer{}
	removeLBsFromSwitch := map[string][]*templateLoadBalancer{}
	addLBsToRouter := map[string][]*templateLoadBalancer{}
	removesLBsFromRouter := map[string][]*templateLoadBalancer{}
	addLBsToGroups := map[string][]*templateLoadBalancer{}
	removeLBsFromGroups := map[string][]*templateLoadBalancer{}
	wantedByName := make(map[string]*LB, len(LBs))
	for i, lb := range LBs {
		wantedByName[lb.Name] = &LBs[i]
		blb := buildLB(&lb)
		tlbs = append(tlbs, blb)
		existingLB := existingByName[lb.Name]
		existingRouters := sets.Set[string]{}
		existingSwitches := sets.Set[string]{}
		existingGroups := sets.Set[string]{}
		if existingLB != nil {
			blb.nbLB.UUID = existingLB.UUID
			delete(toDelete, existingLB.UUID)
			existingRouters = sets.New[string](existingLB.Routers...)
			existingSwitches = sets.New[string](existingLB.Switches...)
			existingGroups = sets.New[string](existingLB.Groups...)
		}
		wantRouters := sets.New(lb.Routers...)
		wantSwitches := sets.New(lb.Switches...)
		wantGroups := sets.New(lb.Groups...)
		mapLBDifferenceByKey(addLBsToSwitch, wantSwitches, existingSwitches, blb)
		mapLBDifferenceByKey(removeLBsFromSwitch, existingSwitches, wantSwitches, blb)
		mapLBDifferenceByKey(addLBsToRouter, wantRouters, existingRouters, blb)
		mapLBDifferenceByKey(removesLBsFromRouter, existingRouters, wantRouters, blb)
		mapLBDifferenceByKey(addLBsToGroups, wantGroups, existingGroups, blb)
		mapLBDifferenceByKey(removeLBsFromGroups, existingGroups, wantGroups, blb)
	}

	ops, err := libovsdbops.CreateOrUpdateLoadBalancersOps(nbClient, nil, toNBLoadBalancerList(tlbs)...)
	if err != nil {
		return err
	}

	ops, err = svcCreateOrUpdateTemplateVarOps(nbClient, ops, toNBTemplateList(tlbs))
	if err != nil {
		return fmt.Errorf("failed to create ops for ensuring creation of service %s/%s load balancers: %w",
			service.Namespace, service.Name, err)
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
		ops, err = libovsdbops.AddLoadBalancersToLogicalSwitchOps(nbClient, ops, getSwitch(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for adding load balancers to switch %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
		}
	}
	for k, v := range removeLBsFromSwitch {
		ops, err = libovsdbops.RemoveLoadBalancersFromLogicalSwitchOps(nbClient, ops, getSwitch(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for removing load balancers from switch %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
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
		ops, err = libovsdbops.AddLoadBalancersToLogicalRouterOps(nbClient, ops, getRouter(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for adding load balancers to router %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
		}
	}
	for k, v := range removesLBsFromRouter {
		ops, err = libovsdbops.RemoveLoadBalancersFromLogicalRouterOps(nbClient, ops, getRouter(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for removing load balancers from router %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
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
		ops, err = libovsdbops.AddLoadBalancersToGroupOps(nbClient, ops, getGroup(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for adding load balancers to group %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
		}
	}
	for k, v := range removeLBsFromGroups {
		ops, err = libovsdbops.RemoveLoadBalancersFromGroupOps(nbClient, ops, getGroup(k), toNBLoadBalancerList(v)...)
		if err != nil {
			return fmt.Errorf("failed to create ops for removing load balancers from group %s for service %s/%s: %w",
				k, service.Namespace, service.Name, err)
		}
	}

	deleteLBs := make([]*nbdb.LoadBalancer, 0, len(toDelete))
	deleteTemplates := make([]TemplateMap, 0, len(toDelete))
	for _, clb := range toDelete {
		deleteLBs = append(deleteLBs, &nbdb.LoadBalancer{UUID: clb.UUID})
		deleteTemplates = append(deleteTemplates, clb.Templates)
	}
	ops, err = libovsdbops.DeleteLoadBalancersOps(nbClient, ops, deleteLBs...)
	if err != nil {
		return fmt.Errorf("failed to create ops for removing %d load balancers for service %s/%s: %w",
			len(deleteLBs), service.Namespace, service.Name, err)
	}

	ops, err = svcDeleteTemplateVarOps(nbClient, ops, deleteTemplates)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := metrics.GetConfigDurationRecorder().AddOVN(nbClient, "service",
		service.Namespace, service.Name)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)

	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(nbClient, toNBLoadBalancerList(tlbs), ops)
	if err != nil {
		return fmt.Errorf("failed to ensure load balancers for service %s/%s: %w", service.Namespace, service.Name, err)
	}
	txOkCallBack()

	// Store UUID of newly created load balancers for future calls.
	// This is accomplished by the caching of LBs by the caller of this function.
	for _, tlb := range tlbs {
		wantedByName[tlb.nbLB.Name].UUID = tlb.nbLB.UUID
	}

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

func mapLBDifferenceByKey(keyMap map[string][]*templateLoadBalancer, keyIn sets.Set[string], keyNotIn sets.Set[string], lb *templateLoadBalancer) {
	for _, k := range keyIn.Difference(keyNotIn).UnsortedList() {
		l := keyMap[k]
		if l == nil {
			l = []*templateLoadBalancer{}
		}
		l = append(l, lb)
		keyMap[k] = l
	}
}

func buildLB(lb *LB) *templateLoadBalancer {
	skipSNAT := "false"
	if lb.Opts.SkipSNAT {
		skipSNAT = "true"
	}

	reject := "false"
	if lb.Opts.Reject {
		reject = "true"
	}

	emptyLb := "false"
	if lb.Opts.EmptyLBEvents {
		emptyLb = "true"
	}

	options := map[string]string{
		"reject":             reject,
		"event":              emptyLb,
		"skip_snat":          skipSNAT,
		"neighbor_responder": "none",
		"hairpin_snat_ip":    fmt.Sprintf("%s %s", config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP.String(), config.Gateway.MasqueradeIPs.V6OVNServiceHairpinMasqueradeIP.String()),
	}

	// Session affinity
	// If enabled, then bucket flows by 3-tuple (proto, srcip, dstip) for the specific timeout value
	// otherwise, use default ovn value
	selectionFields := []nbdb.LoadBalancerSelectionFields{}
	if lb.Opts.AffinityTimeOut > 0 {
		if lb.Opts.AffinityTimeOut != core.MaxClientIPServiceAffinitySeconds {
			options["affinity_timeout"] = fmt.Sprintf("%d", lb.Opts.AffinityTimeOut)
		} else {
			selectionFields = []string{
				nbdb.LoadBalancerSelectionFieldsIPSrc,
				nbdb.LoadBalancerSelectionFieldsIPDst,
			}
		}
	}

	if lb.Opts.Template {
		options["template"] = "true"

		// Address family is used only for template LBs.
		if lb.Opts.AddressFamily != "" {
			// OVN expects the family as lowercase...
			options["address-family"] = strings.ToLower(fmt.Sprintf("%v", lb.Opts.AddressFamily))
		}
	}

	return &templateLoadBalancer{
		nbLB:      libovsdbops.BuildLoadBalancer(lb.Name, strings.ToLower(lb.Protocol), selectionFields, buildVipMap(lb.Rules), options, lb.ExternalIDs),
		templates: lb.Templates,
	}
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

	lbs := make([]*nbdb.LoadBalancer, 0, len(uuids))
	for _, uuid := range uuids {
		lbs = append(lbs, &nbdb.LoadBalancer{UUID: uuid})
	}

	err := libovsdbops.DeleteLoadBalancers(nbClient, lbs)
	if err != nil {
		return err
	}

	return nil
}

// getAllLBs returns a slice of load balancers found in OVN.
func getAllLBs(nbClient libovsdbclient.Client, allTemplates TemplateMap) ([]*LB, error) {
	_, out, err := _getLBsCommon(nbClient, allTemplates, false, true, nil)
	return out, err
}

// getServiceLBsForNetwork returns the services and OVN load balancers for the network specified in netInfo.
func getServiceLBsForNetwork(nbClient libovsdbclient.Client, allTemplates TemplateMap, netInfo util.NetInfo) (sets.Set[string], []*LB, error) {
	return _getLBsCommon(nbClient, allTemplates, true, false, netInfo)
}

func _getLBsCommon(nbClient libovsdbclient.Client, allTemplates TemplateMap, withServiceOwner bool, includeAllNetworks bool, netInfo util.NetInfo) (sets.Set[string], []*LB, error) {

	// Lookup network name and network role in the OVN external IDs to check whether
	// the OVN element with the input externalIDs belongs to this network.
	belongsToThisNetwork := func(externalIDs map[string]string) bool {
		if !util.IsNetworkSegmentationSupportEnabled() {
			return true
		}

		network, ok := externalIDs[types.NetworkExternalID]

		if netInfo == nil {
			return true
		}

		if netInfo.IsDefault() {
			return !ok
		}

		// filter out anything belonging to default and (if any) secondary networks
		role, ok := externalIDs[types.NetworkRoleExternalID]
		return ok && role == types.NetworkRolePrimary && network == netInfo.GetNetworkName()
	}

	p := func(item *nbdb.LoadBalancer) bool {
		return includeAllNetworks || belongsToThisNetwork(item.ExternalIDs)
	}

	lbs, err := libovsdbops.FindLoadBalancersWithPredicate(nbClient, p)
	if err != nil {
		return nil, nil, fmt.Errorf("could not list load_balancer: %w", err)
	}

	services := sets.New[string]()           // all services found in load balancers
	outMap := make(map[string]*LB, len(lbs)) // UUID -> *LB
	for _, lb := range lbs {

		// Skip load balancers unrelated to service, or w/out an owner (aka namespace+name)
		if lb.ExternalIDs[types.LoadBalancerKindExternalID] != "Service" {
			continue
		}

		if withServiceOwner {
			service, ok := lb.ExternalIDs[types.LoadBalancerOwnerExternalID]
			if !ok {
				continue
			}
			services.Insert(service)
		}

		// Note: no need to fill in Opts and Rules: syncServices populates them later.
		// Switches, Routers and Groups for each load balancer will get filled in below.
		res := LB{
			UUID:        lb.UUID,
			Name:        lb.Name,
			ExternalIDs: lb.ExternalIDs,
			Opts:        LBOpts{},
			Rules:       []LBRule{},
			Templates:   getLoadBalancerTemplates(lb, allTemplates),
			Switches:    []string{},
			Routers:     []string{},
			Groups:      []string{},
		}
		if lb.Protocol != nil {
			res.Protocol = *lb.Protocol
		}

		outMap[lb.UUID] = &res
	}

	// Switches
	ps := func(item *nbdb.LogicalSwitch) bool {
		return len(item.LoadBalancer) > 0 && (includeAllNetworks || belongsToThisNetwork(item.ExternalIDs))
	}

	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, ps)
	if err != nil {
		return nil, nil, fmt.Errorf("could not list logical switches: %w", err)
	}
	for _, ls := range switches {
		for _, lbuuid := range ls.LoadBalancer {
			if ovnLb, ok := outMap[lbuuid]; ok {
				outMap[lbuuid].Switches = append(ovnLb.Switches, ls.Name)
			}
		}
	}

	// Routers
	pr := func(item *nbdb.LogicalRouter) bool {
		return len(item.LoadBalancer) > 0 && (includeAllNetworks || belongsToThisNetwork(item.ExternalIDs))
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, pr)
	if err != nil {
		return nil, nil, fmt.Errorf("could not list logical routers: %w", err)
	}
	for _, router := range routers {
		for _, lbuuid := range router.LoadBalancer {
			if ovnLb, ok := outMap[lbuuid]; ok {
				outMap[lbuuid].Routers = append(ovnLb.Routers, router.Name)
			}
		}
	}

	// LB Groups
	pg := func(item *nbdb.LoadBalancerGroup) bool {
		if len(item.LoadBalancer) == 0 {
			return false
		}

		if !util.IsNetworkSegmentationSupportEnabled() || includeAllNetworks {
			return true
		}

		if netInfo == nil {
			return true
		}

		// LB groups have no external ID in OVN, so parse their name instead
		if netInfo.IsDefault() {
			knownDefaultLBGroups := []string{
				types.ClusterLBGroupName,
				types.ClusterSwitchLBGroupName,
				types.ClusterRouterLBGroupName,
			}
			for _, knownGroup := range knownDefaultLBGroups {
				if item.Name == knownGroup {
					return true

				}
			}
			return false
		}
		// UDN
		return strings.HasPrefix(item.Name, netInfo.GetNetworkName())
	}

	groups, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(nbClient, pg)
	if err != nil {
		return nil, nil, fmt.Errorf("could not list load balancer groups: %w", err)
	}
	for _, group := range groups {
		for _, lbuuid := range group.LoadBalancer {
			if ovnLb, ok := outMap[lbuuid]; ok {
				outMap[lbuuid].Groups = append(ovnLb.Groups, group.Name)
			}
		}
	}

	out := make([]*LB, 0, len(outMap))
	for _, value := range outMap {
		out = append(out, value)
	}

	return services, out, nil
}
