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
	UUID        string
	ExternalIDs map[string]string

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
			ExternalIDs: lb.ExternalIDs,

			Switches: sets.NewString(lb.Switches...),
			Routers:  sets.NewString(lb.Routers...),
		}
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
		ExternalIDs: lb.ExternalIDs,

		Switches: sets.NewString(),
		Routers:  sets.NewString(),
	}
}

// find searches through the cache for load balancers that match the list of external IDs.
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

func listLBs() ([]CachedLB, error) {
	stdout, _, err := util.RunOVNNbctlRawOutput(15, "--format=json", "--data=json",
		"--columns=name,_uuid,external_ids", "find", "load_balancer")

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
		if len(row) != 3 {
			return nil, fmt.Errorf("failed to parse find load_balancer response: not enough columns in %#v", row)
		}

		res := CachedLB{
			ExternalIDs: map[string]string{},
			Switches:    sets.String{},
			Routers:     sets.String{},
		}
		// deal with all the types

		if str, ok := row[0].(string); ok {
			res.Name = str
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: expected string, got %#v", row[0])
		}

		if cell, ok := row[1].([]interface{}); ok {
			if len(cell) != 2 {
				return nil, fmt.Errorf("failed to parse find load_balancer response: expected uuid pair, got %#v", cell)
			} else {
				if str, ok := cell[1].(string); ok {
					res.UUID = str
				} else {
					return nil, fmt.Errorf("failed to parse find load_balancer response: expected string pair, got %#v", cell[1])
				}
			}
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: expected uuid pair, got %#v", row[1])
		}

		if cell, ok := row[2].([]interface{}); ok {
			if len(cell) != 2 {
				return nil, fmt.Errorf("failed to parse find load_balancer response: expected map pair, got %#v", cell)
			} else {
				if rows, ok := cell[1].([]interface{}); ok {
					for _, row := range rows {
						if pair, ok := row.([]interface{}); ok {
							if len(pair) != 2 {
								return nil, fmt.Errorf("failed to parse find load_balancer response: expected k-v pair, got %#v", pair)
							} else {
								k, ok := pair[0].(string)
								if !ok {
									return nil, fmt.Errorf("failed to parse find load_balancer response: key not a string: %#v", pair[0])
								}
								v, ok := pair[1].(string)
								if !ok {
									return nil, fmt.Errorf("failed to parse find load_balancer response: value not a string: %#v", pair[1])
								}

								res.ExternalIDs[k] = v
							}
						} else {
							return nil, fmt.Errorf("failed to parse find load_balancer response: expected map k-v list, got %#v", row)
						}
					}
				} else {
					return nil, fmt.Errorf("failed to parse find load_balancer response: expected map list, got %#v", cell[1])
				}
			}
		} else {
			return nil, fmt.Errorf("failed to parse find load_balancer response: expected map, got %#v", row[1])
		}

		out = append(out, res)
	}

	return out, nil
}

// findTableLBs returns a list of router name -> lb uuids
func findTableLBs(tablename string) (map[string][]string, error) {
	rows, err := runNBCtlCSV([]string{"--data=bare", "--columns=name,load_balancer", "find", tablename})
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
