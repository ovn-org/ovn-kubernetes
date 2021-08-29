package loadbalancer

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/util/sets"
)

var globalCache *LBCache
var globalCacheLock sync.Mutex = sync.Mutex{}

// GetLBCache returns the global load balancer cache, and initializes it
// if not yet set.
func GetLBCache() (*LBCache, error) {
	globalCacheLock.Lock()
	defer globalCacheLock.Unlock()

	if globalCache != nil {
		return globalCache, nil
	}

	c, err := newCache()
	if err != nil {
		return nil, err
	}
	globalCache = c
	return globalCache, nil
}

// LBCache caches the state of load balancers in ovn.
// It is used to prevent unnecessary accesses to the database
type LBCache struct {
	sync.Mutex

	existing map[string]*CachedLB
}

// we don't need to store / populate all information, just a subset
type CachedLB struct {
	Name        string
	Protocol    string
	UUID        string
	ExternalIDs map[string]string
	VIPs        sets.String // don't care about backend IPs, just the vips

	Switches sets.String
	Routers  sets.String
}

// update the database with any existing LBs, along with any
// that were deleted.
func (c *LBCache) update(existing []LB, toDelete []string) {
	c.Lock()
	defer c.Unlock()
	for _, uuid := range toDelete {
		delete(c.existing, uuid)
	}

	for _, lb := range existing {
		if lb.UUID == "" {
			panic("coding error: cache add LB with no UUID")
		}
		c.existing[lb.UUID] = &CachedLB{
			Name:        lb.Name,
			UUID:        lb.UUID,
			Protocol:    strings.ToLower(lb.Protocol),
			ExternalIDs: lb.ExternalIDs,
			VIPs:        getVips(&lb),

			Switches: sets.NewString(lb.Switches...),
			Routers:  sets.NewString(lb.Routers...),
		}
	}
}

// removeVIPs updates the cache after a successful DeleteLoadBalancerVIPs call
func (c *LBCache) removeVips(toRemove []DeleteVIPEntry) {
	c.Lock()
	defer c.Unlock()

	for _, entry := range toRemove {
		lb := c.existing[entry.LBUUID]
		if lb == nil {
			continue
		}

		// lb is a pointer, this is immediately effecting.
		lb.VIPs.Delete(entry.VIPs...)
	}
}

// RemoveSwitch removes the provided switchname from all the lb.Switches in the LBCache.
func (c *LBCache) RemoveSwitch(switchname string) {
	c.Lock()
	defer c.Unlock()
	for _, lbCache := range c.existing {
		lbCache.Switches.Delete(switchname)
	}
}

// RemoveRouter removes the provided routername from all the lb.Routers in the LBCache.
func (c *LBCache) RemoveRouter(routername string) {
	c.Lock()
	defer c.Unlock()
	for _, lbCache := range c.existing {
		lbCache.Routers.Delete(routername)
	}
}

// addNewLB is a shortcut when creating a load balancer; we know it won't have any switches or routers
func (c *LBCache) addNewLB(lb *LB) {
	c.Lock()
	defer c.Unlock()
	if lb.UUID == "" {
		panic("coding error! LB has empty UUID")
	}
	c.existing[lb.UUID] = &CachedLB{
		Name:        lb.Name,
		UUID:        lb.UUID,
		Protocol:    strings.ToLower(lb.Protocol),
		ExternalIDs: lb.ExternalIDs,
		VIPs:        getVips(lb),

		Switches: sets.NewString(),
		Routers:  sets.NewString(),
	}
}

func getVips(lb *LB) sets.String {
	out := sets.NewString()
	for _, rule := range lb.Rules {
		out.Insert(rule.Source.String())
	}
	return out
}

// Find searches through the cache for load balancers that match the list of external IDs.
// It returns all found load balancers, indexed by uuid.
func (c *LBCache) Find(externalIDs map[string]string) map[string]*CachedLB {
	c.Lock()
	defer c.Unlock()

	out := map[string]*CachedLB{}

	for uuid, lb := range c.existing {
		if extIDsMatch(externalIDs, lb.ExternalIDs) {
			out[uuid] = lb
		}
	}

	return out
}

// extIDsMatch returns true if have is a superset of want.
func extIDsMatch(want, have map[string]string) bool {
	for k, v := range want {
		actual, ok := have[k]
		if !ok {
			return false
		}
		if actual != v {
			return false
		}
	}

	return true
}

// newCache creates a lbCache, populated with all existing load balancers
func newCache() (*LBCache, error) {
	// first, list all load balancers
	lbs, err := listLBs()
	if err != nil {
		return nil, err
	}

	c := LBCache{}
	c.existing = make(map[string]*CachedLB, len(lbs))

	for i := range lbs {
		c.existing[lbs[i].UUID] = &lbs[i]
	}

	switches, err := findTableLBs("logical_switch")
	if err != nil {
		return nil, err
	}

	for switchname, lbuuids := range switches {
		for _, lbuuid := range lbuuids {
			if lb, ok := c.existing[lbuuid]; ok {
				lb.Switches.Insert(switchname)
			}
		}
	}

	routers, err := findTableLBs("logical_router")
	if err != nil {
		return nil, err
	}

	for routername, lbuuids := range routers {
		for _, lbuuid := range lbuuids {
			if lb, ok := c.existing[lbuuid]; ok {
				lb.Routers.Insert(routername)
			}
		}
	}

	return &c, nil
}

// listLBs lists all load balancers in nbdb
func listLBs() ([]CachedLB, error) {
	stdout, _, err := util.RunOVNNbctlRawOutput(15, "--format=json", "--data=json",
		"--columns=name,_uuid,protocol,external_ids,vips", "find", "load_balancer")

	if err != nil {
		return nil, fmt.Errorf("could not list load_balancer: %w", err)
	}

	data := struct {
		Data     [][]interface{} `json:"data"`
		Headings []string        `json:"headings"`
	}{}

	err = json.Unmarshal([]byte(stdout), &data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse find load_balancer response: %w", err)
	}

	out := make([]CachedLB, 0, len(data.Data))

	for _, row := range data.Data {
		if len(row) != 5 {
			return nil, fmt.Errorf("failed to parse find load_balancer response: wrong number of columns (want 5) in row %#v", row)
		}

		res := CachedLB{
			VIPs:     sets.String{},
			Switches: sets.String{},
			Routers:  sets.String{},
		}
		// parse the row

		// name
		if str, ok := row[0].(string); ok {
			res.Name = str
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: name: expected string, got %#v", row[0])
		}

		// uuid is a pair
		if cell, ok := row[1].([]interface{}); ok {
			if len(cell) != 2 {
				return nil, fmt.Errorf("failed to parse find load_balancer response: uuid: expected pair, got %#v", cell)
			} else {
				if str, ok := cell[1].(string); ok {
					res.UUID = str
				} else {
					return nil, fmt.Errorf("failed to parse find load_balancer response: uuid: expected string, got %#v", cell[1])
				}
			}
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: uuid: expected slice, got %#v", row[1])
		}

		// protocol may be a string or an empty set
		if str, ok := row[2].(string); ok {
			res.Protocol = str
		} else if _, ok := row[2].([]interface{}); ok { //empty set
			// empty protocol, assume tcp
			res.Protocol = "tcp"
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: protocol: expected string, got %#v", row[2])
		}

		res.ExternalIDs, err = extractMap(row[3])
		if err != nil {
			return nil, fmt.Errorf("failed to parse find load_balancer response: external_ids: %w", err)
		}

		vips, err := extractMap(row[4])
		if err != nil {
			return nil, fmt.Errorf("failed to parse find load_balancer response: vips: %w", err)
		}
		for vip := range vips {
			res.VIPs.Insert(vip)
		}
		out = append(out, res)
	}

	return out, nil
}

// findTableLBs returns a list of router name -> lb uuids
func findTableLBs(tablename string) (map[string][]string, error) {
	// this is CSV and not JSON because ovn-nbctl is inconsistent when a set has a single value :-/
	rows, err := util.RunOVNNbctlCSV([]string{"--data=bare", "--columns=name,load_balancer", "find", tablename})
	if err != nil {
		return nil, fmt.Errorf("failed to find existing LBs for %s: %w", tablename, err)
	}

	out := make(map[string][]string, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("invalid row returned when listing LBs for %s: %#v", tablename, row)
		}
		if row[0] == "" {
			continue
		}

		lbs := strings.Split(row[1], " ")
		if row[1] == "" {
			lbs = []string{}
		}
		out[row[0]] = lbs
	}

	return out, nil
}

func TestOnlySetCache(cache *LBCache) {
	globalCache = cache
}

// extractMap converts an untyped ovn json map in to a real map
// it looks like this:
// [ "map", [ ["k1", "v1"], ["k2", "v2"] ]]
func extractMap(in interface{}) (map[string]string, error) {
	out := map[string]string{}

	// ["map", [ pairs]]
	if cell, ok := in.([]interface{}); ok {
		if len(cell) != 2 {
			return nil, fmt.Errorf("expected outer pair, got %#v", cell)
		} else {
			// rows: [ [k,v], [k, v], ...]
			if rows, ok := cell[1].([]interface{}); ok {
				for _, row := range rows {
					if pair, ok := row.([]interface{}); ok {
						if len(pair) != 2 {
							return nil, fmt.Errorf("expected k-v pair, got %#v", pair)
						} else {
							k, ok := pair[0].(string)
							if !ok {
								return nil, fmt.Errorf("key not a string: %#v", pair[0])
							}
							v, ok := pair[1].(string)
							if !ok {
								return nil, fmt.Errorf("value not a string: %#v", pair[1])
							}

							out[k] = v
						}
					} else {
						return nil, fmt.Errorf("expected k-v slice, got %#v", row)
					}
				}
			} else {
				return nil, fmt.Errorf("expected map list, got %#v", cell[1])
			}
		}
	} else {
		return nil, fmt.Errorf("expected outer slice, got %#v", in)
	}
	return out, nil
}
