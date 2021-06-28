package loadbalancer

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// DeleteLoadBalancerVIPs removes the VIPs across lbs in a single shot
// In this case, vip includes port, i.e. "1.2.3.4:80"
func DeleteLoadBalancerVIPs(loadBalancers, vips []string) error {
	txn := util.NewNBTxn()
	for _, loadBalancer := range loadBalancers {
		for _, vip := range vips {
			vipQuotes := fmt.Sprintf("\"%s\"", vip)
			request := []string{"--if-exists", "remove", "load_balancer", loadBalancer, "vips", vipQuotes}
			stdout, stderr, err := txn.AddOrCommit(request)
			if err != nil {
				return fmt.Errorf("error in deleting load balancer vip %v for %v"+
					"stdout: %q, stderr: %q, error: %v",
					vips, loadBalancers, stdout, stderr, err)
			}
		}
	}
	stdout, stderr, err := txn.Commit()
	if err != nil {
		return fmt.Errorf("error in deleting load balancer vip %v for %v"+
			"stdout: %q, stderr: %q, error: %v",
			vips, loadBalancers, stdout, stderr, err)
	}
	return nil
}

// LB is a desired load_balancer configuration in OVN.
type LB struct {
	Name        string
	Protocol    string // one of TCP, UDP, SCTP
	ExternalIDs map[string]string
	Opts        LBOpts

	Rules []LBRule

	// the names of logical switches and routers that this LB should be attached to
	Switches []string
	Routers  []string
}

type LBOpts struct {
	// if true, then enable unidling. Otherwise, generate reject
	Unidling bool

	// If true, then enable per-client-IP affinity.
	Affinity bool
}

type Addr struct {
	IP   string
	Port int32
}

type LBRule struct {
	Source  Addr
	Targets []Addr
}

func (a *Addr) String() string {
	return util.JoinHostPortInt32(a.IP, a.Port)
}

func (a *Addr) Equals(b *Addr) bool {
	return a.Port == b.Port && a.IP == b.IP
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

func JoinHostsPort(ips []string, port int32) []Addr {
	out := make([]Addr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, Addr{IP: ip, Port: port})
	}
	return out
}

// EnsureLBs ensures that, for a given set of externalIDs, exactly
// the set of load balancers exists, and any stale are deleted.
//
// Additionally, those LBs are added to the switches and routers
// as requested.
func EnsureLBs(externalIDs map[string]string, LBs []LB) error {
	existing, err := FindLBs(externalIDs)
	if err != nil {
		return fmt.Errorf("failed to list existing LBs: %w", err)
	}

	nameToUUID := map[string]string{}
	UUIDToName := map[string]string{}

	for _, lb := range existing {
		nameToUUID[lb.Name] = lb.UUID
		UUIDToName[lb.UUID] = lb.Name
	}

	// We rely on names being unique so we can key off of existing LBs
	if len(nameToUUID) != len(UUIDToName) {
		klog.V(2).Infof("Warning: duplicate Load_Balancer name(s) for EnsureLBs %#v", externalIDs)
	}

	// create or update each LB, logging UUID created (so we can clean up)
	createdUUIDs := make([]string, 0, len(LBs))
	for _, lb := range LBs {
		newUUID, err := ensureLB(lb, nameToUUID[lb.Name])
		if err != nil {
			return err
		}
		createdUUIDs = append(createdUUIDs, newUUID)
	}

	toDelete := UUIDToName // not really a meaningful operation, just clearer naming
	for _, uuid := range createdUUIDs {
		delete(toDelete, uuid)
	}

	uuids := make([]string, 0, len(toDelete))
	for uuid := range toDelete {
		uuids = append(uuids, uuid)
	}
	if err := DeleteLBs(uuids); err != nil {
		return fmt.Errorf("failed to delete %d stale load balancers for %#v: %w",
			len(uuids), externalIDs, err)
	}
	klog.V(5).Infof("Deleted %d stale LBs for %#v", len(uuids), externalIDs)
	return nil
}

// ensureLB creates or updates a load balancer as necessary.
// TODO: make this use libovsdb and generally be more efficient
// returns the uuid of the LB
func ensureLB(lb LB, existingUUID string) (string, error) {
	created := false
	if existingUUID == "" {
		created = true
		cmds := []string{
			"create", "load_balancer",
		}
		cmds = append(cmds, lbToColumns(lb)...)
		stdout, _, err := util.RunOVNNbctl(cmds...)
		if err != nil {
			return "", fmt.Errorf("failed to create load_balancer %s: %w", lb.Name, err)
		}
		existingUUID = stdout
	} else {
		cmds := []string{
			"set", "load_balancer", existingUUID,
		}
		cmds = append(cmds, lbToColumns(lb)...)
		_, _, err := util.RunOVNNbctl(cmds...)
		if err != nil {
			return "", fmt.Errorf("failed to update load_balancer %s: %w", lb.Name, err)
		}
	}

	// List existing routers and switches, to see if there are any for which we should remove
	existingRouters := sets.String{}
	existingSwitches := sets.String{}
	if !created {
		// this is ovn-nbctl set query notation - find all logical routers whose load_balancer column includes this uuid.
		existingRows, err := runNBCtlCSV([]string{"--data=bare", "--columns=name", "find", "logical_router", "load_balancer{>=}" + existingUUID})
		if err != nil {
			return existingUUID, fmt.Errorf("failed to find existing GRs for LB %s: %w", lb.Name, err)
		}
		for _, row := range existingRows {
			if len(row) != 1 {
				return existingUUID, fmt.Errorf("invalid row returned when listing GRs for LB %s: %#v", lb.Name, row)
			}
			if row[0] == "" {
				continue
			}
			existingRouters.Insert(row[0])
		}

		existingRows, err = runNBCtlCSV([]string{"--data=bare", "--columns=name", "find", "logical_switch", "load_balancer{>=}" + existingUUID})
		if err != nil {
			return existingUUID, fmt.Errorf("failed to find existing LSs for LB %s: %w", lb.Name, err)
		}
		for _, row := range existingRows {
			if len(row) != 1 {
				return existingUUID, fmt.Errorf("invalid row returned when listing LSs for LB %s: %#v", lb.Name, row)
			}
			if row[0] == "" {
				continue
			}
			existingSwitches.Insert(row[0])
		}
	}

	wantRouters := sets.NewString(lb.Routers...)
	wantSwitches := sets.NewString(lb.Switches...)

	txn := util.NewNBTxn()
	// add missing switches
	for _, sw := range wantSwitches.Difference(existingSwitches).List() {
		_, _, err := txn.AddOrCommit([]string{"--may-exist", "ls-lb-add", sw, existingUUID})
		if err != nil {
			return existingUUID, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}
	// remove old switches
	for _, sw := range existingSwitches.Difference(wantSwitches).List() {
		_, _, err := txn.AddOrCommit([]string{"--if-exists", "ls-lb-del", sw, existingUUID})
		if err != nil {
			return existingUUID, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}

	// add missing routers
	for _, rtr := range wantRouters.Difference(existingRouters).List() {
		_, _, err := txn.AddOrCommit([]string{"--may-exist", "lr-lb-add", rtr, existingUUID})
		if err != nil {
			return existingUUID, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}
	// remove old routers
	for _, rtr := range existingRouters.Difference(wantRouters).List() {
		_, _, err := txn.AddOrCommit([]string{"--if-exists", "lr-lb-del", rtr, existingUUID})
		if err != nil {

			return existingUUID, fmt.Errorf("failed to synchronize LB %s switches / routers: %w", lb.Name, err)
		}
	}
	_, _, err := txn.Commit()
	if err != nil {
		return existingUUID, fmt.Errorf("failed to add LB %s to switches / routers: %w", lb.Name, err)
	}

	return existingUUID, nil
}

// lbToColumns turns a load balancer in to a set of column arguments
// that can be passed to nbctl create or set
func lbToColumns(lb LB) []string {
	reject := "true"
	event := "false"

	if lb.Opts.Unidling {
		reject = "false"
		event = "true"
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
		fmt.Sprintf(`vips={%s}`, strings.Join(vipSet, ",")),
	}

	for k, v := range lb.ExternalIDs {
		out = append(out, "external_ids:"+k+"="+v)
	}

	// for unit testing - stable order
	sort.Strings(out)

	return out
}

type LBRow struct {
	UUID        string
	Name        string
	ExternalIDs map[string]string
	VIPs        map[string]string
	Protocol    string
}

// FindLBs finds all LBs that match a set of external IDs
func FindLBs(externalIDs map[string]string) ([]LBRow, error) {
	conds := make([]string, 0, len(externalIDs))
	for k, v := range externalIDs {
		conds = append(conds, "external_ids:"+k+"="+v)
	}
	sort.Strings(conds)

	// kind of insane: csv of json :-)
	args := append([]string{"--data=json", "--columns=_uuid,name,external_ids,vips,protocol",
		"find", "load_balancer"},
		conds...)

	records, err := runNBCtlCSV(args)
	if err != nil {
		return nil, fmt.Errorf("failed to list load balancers: %w", err)
	}

	out := make([]LBRow, 0, len(records))
	for _, record := range records {
		out = append(out, LBRow{
			UUID:        extractUUID(record[0]),
			Name:        extractString(record[1]),
			ExternalIDs: extractMap(record[2]),
			VIPs:        extractMap(record[3]),
			Protocol:    extractString(record[4]),
		})
	}
	return out, nil
}

// extractUUID unmarshals ovn-nbctl uuids. It's json:
// ["uuid","226557ac-a070-46b1-a6e8-ca9b69e0ab5c"]
func extractUUID(input string) string {
	d := []string{}
	err := json.Unmarshal([]byte(input), &d)
	if err != nil || len(d) != 2 || d[0] != "uuid" {
		klog.Warningf("Failed to parse OVN uuid %s %v", input, err)
		return ""
	}
	return d[1]
}

func extractString(input string) string {
	out := ""
	err := json.Unmarshal([]byte(input), &out)
	if err != nil {
		klog.Warningf("Failed to parse OVN string %s %v", input, err)
		return ""
	}

	return out
}

// extractExternalIDs unmarshals ovn-nbctl maps. Input json:
// ["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","default/kubernetes"]]]
func extractMap(input string) map[string]string {
	d := []interface{}{}
	err := json.Unmarshal([]byte(input), &d)
	if err != nil || len(d) != 2 {
		klog.Warningf("Failed to parse OVN map %s %v", input, err)
		return nil
	}

	kind, ok := d[0].(string)
	if !ok || kind != "map" {
		klog.Warningf("Failed to parse OVN map %s kind", input)
		return nil
	}

	out := map[string]string{}

	pairs, ok := d[1].([]interface{})
	if !ok {
		klog.Warningf("Failed to parse OVN map %s pairs", input)
		return nil
	}

	for _, pair := range pairs {
		pair, ok := pair.([]interface{})
		if !ok || len(pair) != 2 {
			klog.Warningf("Failed to parse OVN map pair %#v", pair)
			return nil
		}

		key, ok := pair[0].(string)
		if !ok {
			klog.Warningf("Failed to parse OVN map pair key %v", pair[0])
		}

		value, ok := pair[1].(string)
		if !ok {
			klog.Warningf("Failed to parse OVN map pair value %v", pair[1])
		}

		out[key] = value
	}

	return out
}

// AddLBsToTargets add a set of LBs to one or more switches and/or routers
func AddLBsToTargets(lbs []string, switches []string, routers []string) error {
	if len(lbs) == 0 {
		return nil
	}

	txn := util.NewNBTxn()

	for _, sw := range switches {
		if sw == "" {
			continue
		}
		for _, lb := range lbs {
			if _, _, err := txn.AddOrCommit([]string{"--may-exist", "ls-lb-add", sw, lb}); err != nil {
				return fmt.Errorf("failed to add load-balancer to switches / routers: %w", err)
			}
		}
	}

	for _, rtr := range routers {
		if rtr == "" {
			continue
		}
		for _, lb := range lbs {
			if _, _, err := txn.AddOrCommit([]string{"--may-exist", "lr-lb-add", rtr, lb}); err != nil {
				return fmt.Errorf("failed to add load-balancer to switches / routers: %w", err)
			}
		}
	}

	_, _, err := txn.Commit()
	if err != nil {
		return fmt.Errorf("failed to add load-balancer to switches / routers: %w", err)
	}
	return nil
}

// DeleteLBs deletes all load balancer uuids supplied
// Note: this also automatically removes them from the switches and the routers :-)
func DeleteLBs(uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}

	args := append([]string{"--if-exists", "destroy", "Load_Balancer"}, uuids...)

	_, _, err := util.RunOVNNbctl(args...)
	return err
}

// runNBCtlCSV runs an nbctl command that results in CSV output, parses the rows returned,
// and returns the records
func runNBCtlCSV(args []string) ([][]string, error) {
	args = append([]string{"--no-heading", "--format=csv"}, args...)

	stdout, _, err := util.RunOVNNbctlRawOutput(15, args...)
	if err != nil {
		return nil, err
	}
	if len(stdout) == 0 {
		return nil, nil
	}

	r := csv.NewReader(strings.NewReader(stdout))
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse nbctl CSV response: %w", err)
	}
	return records, nil
}
