package loadbalancer

import (
	"fmt"
	"strings"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"k8s.io/apimachinery/pkg/util/sets"
)

var globalCache *LBCache
var globalCacheLock sync.Mutex = sync.Mutex{}

// GetLBCache returns the global load balancer cache, and initializes it
// if not yet set.
func GetLBCache(nbClient libovsdbclient.Client) (*LBCache, error) {
	globalCacheLock.Lock()
	defer globalCacheLock.Unlock()

	if globalCache != nil {
		return globalCache, nil
	}

	c, err := newCache(nbClient)
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
	Groups   sets.String
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
			panic(fmt.Sprintf("coding error: cache add LB %s with no UUID", lb.Name))
		}
		c.existing[lb.UUID] = &CachedLB{
			Name:        lb.Name,
			UUID:        lb.UUID,
			Protocol:    strings.ToLower(lb.Protocol),
			ExternalIDs: lb.ExternalIDs,
			VIPs:        getVips(&lb),

			Switches: sets.NewString(lb.Switches...),
			Routers:  sets.NewString(lb.Routers...),
			Groups:   sets.NewString(lb.Groups...),
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
func newCache(nbClient libovsdbclient.Client) (*LBCache, error) {
	// first, list all load balancers
	lbs, err := listLBs(nbClient)
	if err != nil {
		return nil, err
	}

	c := LBCache{}
	c.existing = make(map[string]*CachedLB, len(lbs))

	for i := range lbs {
		c.existing[lbs[i].UUID] = &lbs[i]
	}

	ps := func(item *nbdb.LogicalSwitch) bool {
		return len(item.LoadBalancer) > 0
	}
	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, ps)
	if err != nil {
		return nil, err
	}

	for _, ls := range switches {
		for _, lbuuid := range ls.LoadBalancer {
			if lb, ok := c.existing[lbuuid]; ok {
				lb.Switches.Insert(ls.Name)
			}
		}
	}

	routers, err := libovsdbops.ListRoutersWithLoadBalancers(nbClient)
	if err != nil {
		return nil, err
	}

	for _, router := range routers {
		for _, lbuuid := range router.LoadBalancer {
			if lb, ok := c.existing[lbuuid]; ok {
				lb.Routers.Insert(router.Name)
			}
		}
	}

	// Get non-empty LB groups
	pg := func(item *nbdb.LoadBalancerGroup) bool {
		return len(item.LoadBalancer) > 0
	}
	groups, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(nbClient, pg)
	if err != nil {
		return nil, err
	}

	for _, group := range groups {
		for _, lbuuid := range group.LoadBalancer {
			if lb, ok := c.existing[lbuuid]; ok {
				lb.Groups.Insert(group.Name)
			}
		}
	}

	return &c, nil
}

// listLBs lists all load balancers in nbdb
func listLBs(nbClient libovsdbclient.Client) ([]CachedLB, error) {
	lbs, err := libovsdbops.ListLoadBalancers(nbClient)
	if err != nil {
		return nil, fmt.Errorf("could not list load_balancer: %w", err)
	}

	out := make([]CachedLB, 0, len(lbs))
	for _, lb := range lbs {
		res := CachedLB{
			UUID:        lb.UUID,
			Name:        lb.Name,
			ExternalIDs: lb.ExternalIDs,
			VIPs:        sets.String{},
			Switches:    sets.String{},
			Routers:     sets.String{},
			Groups:      sets.String{},
		}

		if lb.Protocol != nil {
			res.Protocol = *lb.Protocol
		}

		for vip := range lb.Vips {
			res.VIPs.Insert(vip)
		}

		out = append(out, res)
	}

	return out, nil
}

func TestOnlySetCache(cache *LBCache) {
	globalCache = cache
}
