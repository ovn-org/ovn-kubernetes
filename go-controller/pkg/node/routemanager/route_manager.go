package routemanager

import (
	"fmt"
	"net"
	"reflect"
	"time"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type Controller struct {
	// netlinkOps is used to swap netlink lib with a mock lib in order to allow consumers to test without creating netns
	netlinkOps NetLinkOps
	store      map[string]RoutesPerLink // key is link name
	addRouteCh chan RoutesPerLink
	delRouteCh chan RoutesPerLink
}

// NewController manages routes which include adding and deletion of routes. It also manages restoration of managed routes.
// Begin managing routes by calling Run() to start the manager.
// Routes should be added via add(route) and deletion via del(route) functions only.
// All other functions are used internally.
func NewController() *Controller {
	return &Controller{
		netlinkOps: &defaultNetLinkOps{},
		store:      make(map[string]RoutesPerLink),
		addRouteCh: make(chan RoutesPerLink, 5),
		delRouteCh: make(chan RoutesPerLink, 5),
	}
}

// Run starts route manager and syncs at least every syncPeriod
func (c *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	var err error
	var subscribed bool
	var routeEventCh chan netlink.RouteUpdate
	// netlink provides subscribing only to route events from the default table. Periodic sync will restore non-main table routes
	subscribed, routeEventCh = subscribeNetlinkRouteEvents(stopCh)
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			// continue existing behaviour of not cleaning up routes upon exit
			return
		case newRouteEvent, ok := <-routeEventCh:
			if !ok {
				klog.Info("Route Manager: failed to read netlink route event - resubscribing")
				subscribed, routeEventCh = subscribeNetlinkRouteEvents(stopCh)
				continue
			}
			if err = c.processNetlinkEvent(newRouteEvent); err != nil {
				// TODO: make util.GetNetLinkOps().IsLinkNotFoundError(err) smarter to unwrap error
				// and use it here to log errors that are not IsLinkNotFoundError
				klog.V(5).Info("Route Manager: failed to process route update event (%s): %v", newRouteEvent.String(), err)
			}
		case <-ticker.C:
			if !subscribed {
				klog.Info("Route Manager: netlink route events aren't subscribed - resubscribing")
				subscribed, routeEventCh = subscribeNetlinkRouteEvents(stopCh)
			}
			c.sync()
		case newRoute := <-c.addRouteCh:
			if err = c.addRoutesPerLink(newRoute); err != nil {
				klog.Errorf("Route Manager: failed to add route (%s): %v", newRoute.String(), err)
			}
		case delRoute := <-c.delRouteCh:
			if err = c.delRoutesPerLink(delRoute); err != nil {
				klog.Errorf("Route Manager: failed to delete route (%s): %v", delRoute.String(), err)
			}
		}
	}
}

// Add submits a request to add one or more routes for a link
func (c *Controller) Add(rl RoutesPerLink) {
	c.addRouteCh <- rl
}

// Del submits a request to del one or more routes for a link
func (c *Controller) Del(rl RoutesPerLink) {
	c.delRouteCh <- rl
}

func (c *Controller) addRoutesPerLink(rl RoutesPerLink) error {
	klog.Infof("Route Manager: attempting to add routes for link: %s", rl.String())
	if err := rl.validate(); err != nil {
		return fmt.Errorf("failed to validate addition of new routes for link (%s): %w", rl.String(), err)
	}
	if err := c.addRoutesPerLinkStore(rl); err != nil {
		return fmt.Errorf("failed to add route for link to store: %w", err)
	}
	if err := c.applyRoutesPerLink(rl); err != nil {
		return fmt.Errorf("failed to apply route for link: %w", err)
	}
	klog.Infof("Route Manager: completed adding route: %s", rl.String())
	return nil
}

func (c *Controller) delRoutesPerLink(rl RoutesPerLink) error {
	klog.Infof("Route Manager: attempting to delete routes for link: %s", rl.String())
	if err := rl.validate(); err != nil {
		return fmt.Errorf("failed to validate route for link (%s): %v", rl.String(), err)
	}
	var deletedRoutes []Route
	for _, r := range rl.Routes {
		if err := c.netlinkDelRoute(rl.Link, r.Subnet, r.Table); err != nil {
			return err
		}
		deletedRoutes = append(deletedRoutes, r)
	}
	infName, err := rl.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to delete route (%+v) because we failed to get link name: %v",
			rl, err)
	}
	routesPerLinkFound, ok := c.store[infName]
	if !ok {
		routesPerLinkFound = RoutesPerLink{rl.Link, []Route{}}
	}
	if len(deletedRoutes) > 0 {
		routesPerLinkFound.delRoutes(deletedRoutes)
	}
	if len(routesPerLinkFound.Routes) == 0 {
		delete(c.store, infName)
	} else {
		c.store[infName] = routesPerLinkFound
	}
	klog.Infof("Route Manager: deletion of routes for link complete: %s", rl.String())
	return nil
}

// processNetlinkEvent will check if a deleted route is managed by route manager and if so, determine if a sync is needed
// to restore any managed routes.
func (c *Controller) processNetlinkEvent(ru netlink.RouteUpdate) error {
	if ru.Type == unix.RTM_NEWROUTE {
		// An event resulting from `ip route change` will be seen as type RTM_NEWROUTE event and therefore this function will only
		// log the changes and not attempt to restore the change. This will be accomplished by the sync function.
		klog.Infof("Route Manager: netlink route addition event: %q", ru.String())
		return nil
	}
	if ru.Type != unix.RTM_DELROUTE {
		return nil
	}
	klog.V(5).Infof("Route Manager: netlink route deletion event: %q", ru.String())
	link, err := c.netlinkOps.LinkByIndex(ru.LinkIndex)
	if err != nil {
		return fmt.Errorf("failed to get link by index %d for netlink route event (%s): %w", ru.LinkIndex, ru.String(), err)
	}
	rlEvent, err := convertRouteUpdateToRoutesPerLink(link, ru)
	if err != nil {
		return fmt.Errorf("failed to convert netlink event to routesPerLink: %w", err)
	}
	infName, err := rlEvent.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to get link name: %v", err)
	}
	rl, ok := c.store[infName]
	if !ok {
		// we don't manage this interface
		return nil
	}

	var syncNeeded bool
	var syncReason string
	for _, managedRoute := range rl.Routes {
		for _, routeEvent := range rlEvent.Routes {
			if managedRoute.Equal(routeEvent) {
				syncNeeded = true
				syncReason = fmt.Sprintf("managed route was modified: %s", managedRoute.String())
			}
		}
	}
	if syncNeeded {
		klog.Infof("Route Manager: sync required for routes associated with link %q. Reason: %s", infName, syncReason)
		if err = c.applyRoutesPerLink(rl); err != nil {
			klog.Errorf("Route Manager: failed to apply route for link (%s): %w", rl.String(), err)
		}
	}
	return nil
}

func (c *Controller) applyRoutesPerLink(rl RoutesPerLink) error {
	for _, r := range rl.Routes {
		if err := c.applyRoute(rl.Link, r.GwIP, r.Subnet, r.MTU, r.SrcIP, r.Table); err != nil {
			return fmt.Errorf("failed to apply route (%s) because of error: %v", r.String(), err)
		}
	}
	return nil
}

func (c *Controller) applyRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, src net.IP, table int) error {
	filterRoute, filterMask := filterRouteByDstAndTable(link.Attrs().Index, subnet, table)
	nlRoutes, err := c.netlinkOps.RouteListFiltered(getNetlinkIPFamily(subnet), filterRoute, filterMask)
	if err != nil {
		return fmt.Errorf("failed to list filtered routes: %v", err)
	}
	if len(nlRoutes) == 0 {
		return c.netlinkAddRoute(link, gwIP, subnet, mtu, src, table)
	}
	if len(nlRoutes) > 1 {
		return fmt.Errorf("unexpected number of routes after filtering. Expecting one route but found: %+v", nlRoutes)
	}
	netlinkRoute := &nlRoutes[0]
	if netlinkRoute.MTU != mtu || !src.Equal(netlinkRoute.Src) || !gwIP.Equal(netlinkRoute.Gw) {
		netlinkRoute.MTU = mtu
		netlinkRoute.Src = src
		netlinkRoute.Gw = gwIP
		err = c.netlinkOps.RouteReplace(netlinkRoute)
		if err != nil {
			return fmt.Errorf("failed to replace route (using route '%s'): %v", netlinkRoute.String(), err)
		}
	}
	return nil
}

func (c *Controller) netlinkAddRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, srcIP net.IP, table int) error {
	newNlRoute := &netlink.Route{
		Dst:       subnet,
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        gwIP,
		Table:     table,
	}
	if len(srcIP) > 0 {
		newNlRoute.Src = srcIP
	}
	if mtu != 0 {
		newNlRoute.MTU = mtu
	}
	err := c.netlinkOps.RouteAdd(newNlRoute)
	if err != nil {
		return fmt.Errorf("failed to add route (gw: %v, subnet %v, mtu %d, src IP %v): %v", gwIP, subnet, mtu, srcIP, err)
	}
	return nil
}

func (c *Controller) netlinkDelRoute(link netlink.Link, subnet *net.IPNet, table int) error {
	filter, mask := filterRouteByTable(link.Attrs().Index, table)
	nlRoutes, err := c.netlinkOps.RouteListFiltered(netlink.FAMILY_ALL, filter, mask)
	if err != nil {
		return fmt.Errorf("failed to get routes for link %s from table %d: %v", link.Attrs().Name, table, err)
	}
	for _, nlRoute := range nlRoutes {
		if nlRoute.Dst != nil {
			if nlRoute.Dst.String() == subnet.String() {
				if err = c.netlinkOps.RouteDel(&nlRoute); err != nil {
					return fmt.Errorf("failed to delete route '%v' for link %s: %v", nlRoute, link.Attrs().Name, err)
				}
				break
			}
		}
	}
	return nil
}

func (c *Controller) addRoutesPerLinkStore(rl RoutesPerLink) error {
	infName, err := rl.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to add route for link (%s) to store because we could not get link name", rl.String())
	}
	managedRl, ok := c.store[infName]
	if !ok {
		c.store[infName] = rl
		return nil
	}
	newRoutes := make([]Route, 0)
	for _, newRoute := range rl.Routes {
		var found bool
		for _, managedRoute := range managedRl.Routes {
			if managedRoute.Equal(newRoute) {
				found = true
				break
			}
		}
		if !found {
			newRoutes = append(newRoutes, newRoute)
		}
	}
	if len(newRoutes) == 0 {
		klog.Infof("Route Manager: nothing to process for new route for link as it is already managed: %s", rl.String())
		return nil
	}
	managedRl.Routes = append(managedRl.Routes, newRoutes...)
	c.store[infName] = managedRl
	return nil
}

// sync will iterate through all links routes seen on a node and ensure any route manager managed routes are applied. Any additional
// routes for this link are preserved. sync only inspects routes for links which we managed and ignore routes for non-managed links.
func (c *Controller) sync() {
	for infName, rl := range c.store {
		for _, route := range rl.Routes {
			filterRoute, filterMask := filterRouteByTable(rl.Link.Attrs().Index, route.Table)
			activeNlRoutes, err := c.netlinkOps.RouteListFiltered(netlink.FAMILY_ALL, filterRoute, filterMask)
			if err != nil {
				klog.Errorf("Route Manager: failed to list routes for link %q usin filter route (%s) and filter mask %d: %w",
					filterRoute.String(), filterMask, infName, err)
				continue
			}
			var activeRoutes []Route
			for _, activeNlRoute := range activeNlRoutes {
				activeRoute := ConvertNetlinkRouteToRoute(activeNlRoute)
				activeRoutes = append(activeRoutes, activeRoute)
			}
			var syncNeeded bool
			var syncReason string
			for _, expectedRoute := range rl.Routes {
				var found bool
				for _, activeRoute := range activeRoutes {
					if activeRoute.Equal(expectedRoute) {
						found = true
					}
				}
				if !found {
					syncReason = fmt.Sprintf("failed to find route: %s", expectedRoute.String())
					syncNeeded = true
					break
				}
			}
			if syncNeeded {
				if err := c.applyRoute(rl.Link, route.GwIP, route.Subnet, route.MTU, route.SrcIP, route.Table); err != nil {
					klog.Errorf("Route Manager: failed to apply route because %s (%s): %v", syncReason, route.String(), err)
				}
			}
		}
	}
}

// SetNetLinkOpMockInst method would be used by unit tests in other packages
// Used only for testing when we do not wish to create network namespaces and a subset of consumers of route manager
// do not use network namespaces
func (c *Controller) SetNetLinkOpMockInst(mockInst NetLinkOps) {
	c.netlinkOps = mockInst
}

type RoutesPerLink struct {
	Link   netlink.Link
	Routes []Route
}

func (rl RoutesPerLink) Equal(rpl2 RoutesPerLink) bool {
	return reflect.DeepEqual(rl, rpl2)
}

func (rl RoutesPerLink) validate() error {
	if rl.Link == nil || rl.Link.Attrs() == nil || rl.Link.Attrs().Name == "" {
		return fmt.Errorf("link must be valid")
	}
	if len(rl.Routes) == 0 {
		return fmt.Errorf("route must have a least one route entry")
	}
	for _, r := range rl.Routes {
		if r.Subnet == nil || r.Subnet.String() == "<nil>" {
			return fmt.Errorf("invalid subnet for route entry")
		}
	}
	return nil
}

func (rl RoutesPerLink) getLinkName() (string, error) {
	if rl.Link == nil || rl.Link.Attrs() == nil || rl.Link.Attrs().Name == "" {
		return "", fmt.Errorf("unable to get link name from: '%+v'", rl.Link)
	}
	return rl.Link.Attrs().Name, nil
}

func (rl RoutesPerLink) String() string {
	var routes string
	for i, r := range rl.Routes {
		routes = fmt.Sprintf("%s Route %d: %q", routes, i+1, r.String())
	}
	return fmt.Sprintf("Route(s) for link name: %q, with %d routes: %s", rl.Link.Attrs().Name, len(rl.Routes), routes)
}

func (rl *RoutesPerLink) delRoutes(delRoutes []Route) {
	if len(delRoutes) == 0 {
		return
	}
	routes := make([]Route, 0)
	for _, existingRoute := range rl.Routes {
		var found bool
		for _, delRoute := range delRoutes {
			if existingRoute.Equal(delRoute) {
				found = true
			}
		}
		if !found {
			routes = append(routes, existingRoute)
		}
	}
	rl.Routes = routes
}

type Route struct {
	GwIP   net.IP
	Subnet *net.IPNet
	MTU    int
	SrcIP  net.IP
	Table  int
}

// Equal compares two routes and returns true if they are equal
func (r Route) Equal(r2 Route) bool {
	if r.Table != r2.Table {
		return false
	}
	if r.MTU != r2.MTU {
		return false
	}
	if r.Subnet != nil && r2.Subnet == nil {
		return false
	} else if r.Subnet == nil && r2.Subnet != nil {
		return false
	} else if r.Subnet == nil && r2.Subnet == nil {
		// pass: both subnets are nil
	} else if r.Subnet != nil && r2.Subnet != nil {
		if r.Subnet.String() != r2.Subnet.String() {
			return false
		}
	}
	if r.GwIP.String() != r2.GwIP.String() {
		return false
	}
	if r.SrcIP.String() != r2.SrcIP.String() {
		return false
	}
	return true
}

func (r Route) String() string {
	s := fmt.Sprintf("Table %d", r.Table)
	if r.Subnet != nil {
		s = fmt.Sprintf("%s Subnet: %s", s, r.Subnet.String())
	}
	if r.MTU != 0 {
		s = fmt.Sprintf("%s MTU: %d ", s, r.MTU)
	}
	if len(r.SrcIP) > 0 {
		s = fmt.Sprintf("%s Source IP: %q ", s, r.SrcIP.String())
	}
	if len(r.GwIP) > 0 {
		s = fmt.Sprintf("%s Gateway IP: %q", s, r.GwIP.String())
	}
	return s
}

func ConvertNetlinkRouteToRoute(nlRoute netlink.Route) Route {
	return Route{
		GwIP:   nlRoute.Gw,
		Subnet: nlRoute.Dst,
		MTU:    nlRoute.MTU,
		SrcIP:  nlRoute.Src,
		Table:  nlRoute.Table,
	}
}

func convertRouteUpdateToRoutesPerLink(link netlink.Link, ru netlink.RouteUpdate) (RoutesPerLink, error) {
	return RoutesPerLink{
		Link: link,
		Routes: []Route{
			{
				GwIP:   ru.Gw,
				Subnet: ru.Dst,
				MTU:    ru.MTU,
				SrcIP:  ru.Src,
				Table:  ru.Table,
			},
		},
	}, nil
}

func convertNetlinkRouteToRoutesPerLink(link netlink.Link, nlRoute netlink.Route) (RoutesPerLink, error) {
	return RoutesPerLink{
		Link: link,
		Routes: []Route{
			{
				GwIP:   nlRoute.Gw,
				Subnet: nlRoute.Dst,
				MTU:    nlRoute.MTU,
				SrcIP:  nlRoute.Src,
				Table:  nlRoute.Table,
			},
		},
	}, nil
}

func getNetlinkIPFamily(ipNet *net.IPNet) int {
	if utilnet.IsIPv6(ipNet.IP) {
		return netlink.FAMILY_V6
	} else {
		return netlink.FAMILY_V4
	}
}

func filterRouteByDstAndTable(linkIndex int, subnet *net.IPNet, table int) (*netlink.Route, uint64) {
	return &netlink.Route{
			Dst:       subnet,
			LinkIndex: linkIndex,
			Table:     table,
		},
		netlink.RT_FILTER_DST | netlink.RT_FILTER_OIF | netlink.RT_FILTER_TABLE
}

func filterRouteByTable(linkIndex, table int) (*netlink.Route, uint64) {
	return &netlink.Route{
			LinkIndex: linkIndex,
			Table:     table,
		},
		netlink.RT_FILTER_OIF | netlink.RT_FILTER_TABLE
}

func subscribeNetlinkRouteEvents(stopCh <-chan struct{}) (bool, chan netlink.RouteUpdate) {
	routeEventCh := make(chan netlink.RouteUpdate, 20)
	if err := netlink.RouteSubscribe(routeEventCh, stopCh); err != nil {
		klog.Errorf("Route Manager: failed to subscribe to netlink route events: %v", err)
		return false, routeEventCh
	}
	return true, routeEventCh
}

type NetLinkOps interface {
	LinkList() ([]netlink.Link, error)
	LinkByName(ifaceName string) (netlink.Link, error)
	LinkByIndex(index int) (netlink.Link, error)
	IsLinkNotFoundError(err error) bool
	RouteDel(route *netlink.Route) error
	RouteAdd(route *netlink.Route) error
	RouteReplace(route *netlink.Route) error
	RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error)
}

type defaultNetLinkOps struct {
}

func (defaultNetLinkOps) LinkList() ([]netlink.Link, error) {
	return netlink.LinkList()
}

func (defaultNetLinkOps) LinkByName(ifaceName string) (netlink.Link, error) {
	return netlink.LinkByName(ifaceName)
}

func (defaultNetLinkOps) LinkByIndex(index int) (netlink.Link, error) {
	return netlink.LinkByIndex(index)
}

func (defaultNetLinkOps) IsLinkNotFoundError(err error) bool {
	return reflect.TypeOf(err) == reflect.TypeOf(netlink.LinkNotFoundError{})
}

func (defaultNetLinkOps) RouteDel(route *netlink.Route) error {
	return netlink.RouteDel(route)
}

func (defaultNetLinkOps) RouteAdd(route *netlink.Route) error {
	return netlink.RouteAdd(route)
}

func (defaultNetLinkOps) RouteReplace(route *netlink.Route) error {
	return netlink.RouteReplace(route)
}

func (defaultNetLinkOps) RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error) {
	return netlink.RouteListFiltered(family, filter, filterMask)
}
