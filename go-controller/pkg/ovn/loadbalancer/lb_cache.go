package loadbalancer

import (
	"fmt"
	"strings"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
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

	// cache keys on lb name
	existing map[string]*CachedLB
}

// we don't need to store / populate all information, just a subset
type CachedLB struct {
	Name     string
	Protocol string
	// UUIDs are plural because there may be more than one LB in OVN with the same name
	// The Name on a load_balancer is not unique in OVN, but it is in OVNK
	UUIDs       sets.String
	ExternalIDs map[string]string
	VIPs        sets.String // don't care about backend IPs, just the vips

	Switches sets.String
	Routers  sets.String
	Groups   sets.String
}

// AddOrUpdateEntry adds an entry to the cache or updates it with a UUID
// returns a list of UUIDs associated with this entry
func (c *LBCache) AddOrUpdateEntry(name, uuid string) []string {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.existing[name]; ok {
		c.existing[name].UUIDs.Insert(uuid)
	} else {
		c.existing[name] = &CachedLB{
			Name:  name,
			UUIDs: sets.NewString(uuid),
		}
	}

	return c.existing[name].UUIDs.UnsortedList()
}

// update the database with any existing LBs, along with any
// that were deleted.
func (c *LBCache) update(existing []LB, toDelete []string) {
	c.Lock()
	defer c.Unlock()
	for _, name := range toDelete {
		delete(c.existing, name)
	}

	for _, lb := range existing {
		if lb.UUID == "" {
			panic(fmt.Sprintf("coding error: cache add LB %s with no UUID", lb.Name))
		}

		if _, ok := c.existing[lb.Name]; !ok {
			existingLB := &CachedLB{Name: lb.Name, UUIDs: sets.NewString()}
			c.existing[lb.Name] = existingLB
		}

		c.existing[lb.Name].UUIDs.Insert(lb.UUID)
		c.existing[lb.Name].Protocol = strings.ToLower(lb.Protocol)
		c.existing[lb.Name].ExternalIDs = lb.ExternalIDs
		c.existing[lb.Name].VIPs = getVips(&lb)
		c.existing[lb.Name].Switches = sets.NewString(lb.Switches...)
		c.existing[lb.Name].Routers = sets.NewString(lb.Routers...)
		c.existing[lb.Name].Groups = sets.NewString(lb.Groups...)

	}
}

// removeVIPs updates the cache after a successful DeleteLoadBalancerVIPs call
func (c *LBCache) removeVips(toRemove []DeleteVIPEntry) {
	c.Lock()
	defer c.Unlock()

	for _, entry := range toRemove {
		lb := c.existing[entry.LBName]
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
// It returns all found load balancers, indexed by name.
func (c *LBCache) Find(externalIDs map[string]string) map[string]CachedLB {
	c.Lock()
	defer c.Unlock()

	out := map[string]CachedLB{}

	for name, lb := range c.existing {
		if extIDsMatch(externalIDs, lb.ExternalIDs) {
			out[name] = *lb
		}
	}

	return out
}

// Get fetches UUIDs for LB name
func (c *LBCache) Get(name string) []string {
	c.Lock()
	defer c.Unlock()

	return c.existing[name].UUIDs.UnsortedList()
}

// fetches LB name by UUID
func (c *LBCache) getByUUID(uuid string) string {
	for name, lb := range c.existing {
		if lb.UUIDs.Has(uuid) {
			return name
		}
	}
	return ""
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

// FindLegacyLBExtIDKey takes into account legacy lbs use a format of <name> = "yes"
// find the corresponding ID and return it
func FindLegacyLBExtIDKey(extIDs map[string]string) string {
	for k, v := range extIDs {
		if v == "yes" {
			return k
		}
	}
	return ""
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
		key := lbs[i].Name
		// legacy LBs do not use a name and instead use external ids
		// copy those to name for the cache
		if len(key) == 0 {
			key = FindLegacyLBExtIDKey(lbs[i].ExternalIDs)
		}
		if len(key) > 0 {
			// check if this LB is already in cache
			if _, ok := c.existing[lbs[i].Name]; ok {
				c.existing[lbs[i].Name].UUIDs.Insert(lbs[i].UUIDs.UnsortedList()...)
				klog.Warningf("Duplicate Load Balancers found in OVN for name %q, UUIDs: %+v",
					lbs[i].Name, lbs[i].UUIDs.UnsortedList())
			} else {
				c.existing[lbs[i].Name] = &lbs[i]
			}
		} else {
			klog.Warningf("Load balancer has no name and no legacy UUID, will ignore from LB Cache! LB: %#v",
				lbs[i])
		}

	}

	switches, err := libovsdbops.ListSwitchesWithLoadBalancers(nbClient)
	if err != nil {
		return nil, err
	}

	for _, ls := range switches {
		for _, lbuuid := range ls.LoadBalancer {
			cacheLBName := c.getByUUID(lbuuid)
			if cacheLBName != "" {
				c.existing[cacheLBName].Switches.Insert(ls.Name)
			}
		}
	}

	routers, err := libovsdbops.ListRoutersWithLoadBalancers(nbClient)
	if err != nil {
		return nil, err
	}

	for _, router := range routers {
		for _, lbuuid := range router.LoadBalancer {
			cacheLBName := c.getByUUID(lbuuid)
			if cacheLBName != "" {
				c.existing[cacheLBName].Routers.Insert(router.Name)
			}
		}
	}

	groups, err := libovsdbops.ListGroupsWithLoadBalancers(nbClient)
	if err != nil {
		return nil, err
	}

	for _, group := range groups {
		for _, lbuuid := range group.LoadBalancer {
			cacheLBName := c.getByUUID(lbuuid)
			if cacheLBName != "" {
				c.existing[cacheLBName].Groups.Insert(group.Name)
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
			UUIDs:       sets.NewString(lb.UUID),
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
