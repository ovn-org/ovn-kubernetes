package node

import (
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type routeManager struct {
	// log all route netlink events received
	logRouteChanges bool
	// period for which we want to check that the routes we manage are applied. This is needed for the rare case
	// we miss a route event.
	syncPeriod time.Duration
	store      map[string]routesPerLink // key is link name
	addRouteCh chan routesPerLink
	delRouteCh chan routesPerLink
}

// newRouteManager manages routes which include adding and deletion of routes. It also manages restoration of managed routes.
// Begin managing routes by calling run() to start the manager.
// Routes should be added via add(route) and deletion via del(route) functions only.
// All other functions are used internally.
func newRouteManager(logRouteChanges bool, syncPeriod time.Duration) *routeManager {
	return &routeManager{
		logRouteChanges: logRouteChanges,
		syncPeriod:      syncPeriod,
		store:           make(map[string]routesPerLink),
		addRouteCh:      make(chan routesPerLink, 5),
		delRouteCh:      make(chan routesPerLink, 5),
	}
}

func (rm *routeManager) run(stopCh <-chan struct{}) {
	var err error
	var subscribed bool
	var routeEventCh chan netlink.RouteUpdate
	subscribed, routeEventCh = subscribeNetlinkRouteEvents(stopCh)
	ticker := time.NewTicker(rm.syncPeriod)
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
			if err = rm.processNetlinkEvent(newRouteEvent); err != nil {
				// TODO: make util.GetNetLinkOps().IsLinkNotFoundError(err) smarter to unwrap error
				// and use it here to log errors that are not IsLinkNotFoundError
				klog.V(5).Info("Route Manager: failed to process route update event (%s): %v", newRouteEvent.String(), err)
			}
		case <-ticker.C:
			if !subscribed {
				klog.Info("Route Manager: netlink route events aren't subscribed - resubscribing")
				subscribed, routeEventCh = subscribeNetlinkRouteEvents(stopCh)
			}
			rm.sync()
		case newRoute := <-rm.addRouteCh:
			if err = rm.addRoutesPerLink(newRoute); err != nil {
				klog.Errorf("Route Manager: failed to add route (%s): %v", newRoute.String(), err)
			}
		case delRoute := <-rm.delRouteCh:
			if err = rm.delRoutesPerLink(delRoute); err != nil {
				klog.Errorf("Route Manager: failed to delete route (%s): %v", delRoute.String(), err)
			}
		}
	}
}

func (rm *routeManager) add(rl routesPerLink) {
	rm.addRouteCh <- rl
}

func (rm *routeManager) del(rl routesPerLink) {
	rm.delRouteCh <- rl
}

func (rm *routeManager) addRoutesPerLink(rl routesPerLink) error {
	klog.Infof("Route Manager: attempting to add routes for link: %s", rl.String())
	if err := rl.validate(); err != nil {
		return fmt.Errorf("failed to validate addition of new routes for link (%s): %v", rl.String(), err)
	}
	if err := rm.addRoutesPerLinkStore(rl); err != nil {
		return fmt.Errorf("failed to add route for link to store: %w", err)
	}
	if err := rm.applyRoutesPerLink(rl); err != nil {
		return fmt.Errorf("failed to apply route for link: %v", err)
	}
	klog.Infof("Route Manager: completed adding route: %s", rl.String())
	return nil
}

func (rm *routeManager) delRoutesPerLink(rl routesPerLink) error {
	klog.Infof("Route Manager: attempting to delete routes for link: %s", rl.String())
	if err := rl.validate(); err != nil {
		return fmt.Errorf("failed to validate route for link (%s): %v", rl.String(), err)
	}
	var deletedRoutes []route
	for _, r := range rl.routes {
		if err := rm.netlinkDelRoute(rl.link, r.subnet); err != nil {
			return err
		}
		deletedRoutes = append(deletedRoutes, r)
	}
	infName, err := rl.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to delete route (%+v) because we failed to get link name: %v",
			rl, err)
	}
	routesPerLinkFound, ok := rm.store[infName]
	if !ok {
		routesPerLinkFound = routesPerLink{rl.link, []route{}}
	}
	if len(deletedRoutes) > 0 {
		routesPerLinkFound.delRoutes(deletedRoutes)
	}
	if len(routesPerLinkFound.routes) == 0 {
		delete(rm.store, infName)
	} else {
		rm.store[infName] = routesPerLinkFound
	}
	klog.Infof("Route Manager: deletion of routes for link complete: %s", rl.String())
	return nil
}

// processNetlinkEvent will log new and deleted routes if logRouteChanges is true. It will also check if a deleted route
// is managed by route manager and if so, determine if a sync is needed to restore any managed routes.
func (rm *routeManager) processNetlinkEvent(ru netlink.RouteUpdate) error {
	if ru.Type == unix.RTM_NEWROUTE {
		// An event resulting from `ip route change` will be seen as type RTM_NEWROUTE event and therefore this function will only
		// log the changes and not attempt to restore the change. This will be accomplished by the sync function.
		if rm.logRouteChanges {
			klog.Infof("Route Manager: netlink route addition event: %q", ru.String())
		}
		return nil
	}
	if ru.Type != unix.RTM_DELROUTE {
		return nil
	}
	if rm.logRouteChanges {
		klog.V(5).Infof("Route Manager: netlink route deletion event: %q", ru.String())
	}
	rlEvent, err := convertRouteUpdateToRoutesPerLink(ru)
	if err != nil {
		return fmt.Errorf("failed to convert netlink event to routesPerLink: %w", err)
	}
	infName, err := rlEvent.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to get link name: %v", err)
	}
	rl, ok := rm.store[infName]
	if !ok {
		// we don't manage this interface
		return nil
	}

	var syncNeeded bool
	var syncReason string
	for _, managedRoute := range rl.routes {
		for _, routeEvent := range rlEvent.routes {
			if managedRoute.equal(routeEvent) {
				syncNeeded = true
				syncReason = fmt.Sprintf("managed route was modified: %s", managedRoute.string())
			}
		}
	}
	if syncNeeded {
		klog.Infof("Route Manager: sync required for routes associated with link %q. Reason: %s", infName, syncReason)
		if err = rm.applyRoutesPerLink(rl); err != nil {
			klog.Errorf("Route Manager: failed to apply route for link (%s): %w", rl.String(), err)
		}
	}
	return nil
}

func (rm *routeManager) applyRoutesPerLink(rl routesPerLink) error {
	for _, r := range rl.routes {
		if err := rm.applyRoute(rl.link, r.gwIP, r.subnet, r.mtu, r.srcIP); err != nil {
			return fmt.Errorf("failed to apply route (%s) because of error: %v", r.string(), err)
		}
	}
	return nil
}

func (rm *routeManager) applyRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, src net.IP) error {
	filterRoute, filterMask := filterRouteByDst(link, subnet)
	nlRoutes, err := netlink.RouteListFiltered(getNetlinkIPFamily(gwIP), filterRoute, filterMask)
	if err != nil {
		return fmt.Errorf("failed to list filtered routes: %v", err)
	}
	if len(nlRoutes) == 0 {
		return rm.netlinkAddRoute(link, gwIP, subnet, mtu, src)
	}
	netlinkRoute := &nlRoutes[0]
	if netlinkRoute.MTU != mtu || !src.Equal(netlinkRoute.Src) || !gwIP.Equal(netlinkRoute.Gw) {
		netlinkRoute.MTU = mtu
		netlinkRoute.Src = src
		netlinkRoute.Gw = gwIP
		err = netlink.RouteReplace(netlinkRoute)
		if err != nil {
			return fmt.Errorf("failed to replace route for subnet %s via gateway %s with mtu %d: %v",
				subnet.String(), gwIP.String(), mtu, err)
		}
	}
	return nil
}

func (rm *routeManager) netlinkAddRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, srcIP net.IP) error {
	newNlRoute := &netlink.Route{
		Dst:       subnet,
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        gwIP,
	}
	if len(srcIP) > 0 {
		newNlRoute.Src = srcIP
	}
	if mtu != 0 {
		newNlRoute.MTU = mtu
	}
	err := netlink.RouteAdd(newNlRoute)
	if err != nil {
		return fmt.Errorf("failed to add route (%s): %v", newNlRoute.String(), err)
	}
	return nil
}

func (rm *routeManager) netlinkDelRoute(link netlink.Link, subnet *net.IPNet) error {
	// List routes for the link in the default routing table
	nlRoutes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to get routes for link %s: %v", link.Attrs().Name, err)
	}
	for _, nlRoute := range nlRoutes {
		deleteRoute := false
		// Delete if subnet is nil and netlink route dst is nil or if they are equal
		if subnet == nil {
			deleteRoute = nlRoute.Dst == nil
		} else if nlRoute.Dst != nil {
			deleteRoute = nlRoute.Dst.String() == subnet.String()
		}

		if deleteRoute {
			err = netlink.RouteDel(&nlRoute)
			if err != nil {
				net := "default"
				if nlRoute.Dst != nil {
					net = nlRoute.Dst.String()
				}
				return fmt.Errorf("failed to delete route '%s via %s' for link %s : %v\n",
					net, nlRoute.Gw.String(), link.Attrs().Name, err)
			}
			break
		}
	}
	return nil
}

func (rm *routeManager) addRoutesPerLinkStore(rl routesPerLink) error {
	infName, err := rl.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to add route for link (%s) to store because we could not get link name", rl.String())
	}
	managedRl, ok := rm.store[infName]
	if !ok {
		rm.store[infName] = rl
		return nil
	}
	newRoutes := make([]route, 0)
	for _, newRoute := range rl.routes {
		var found bool
		for _, managedRoute := range managedRl.routes {
			if managedRoute.equal(newRoute) {
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
	managedRl.routes = append(managedRl.routes, newRoutes...)
	rm.store[infName] = managedRl
	return nil
}

// sync will iterate through all links routes seen on a node and ensure any route manager managed routes are applied. Any additional
// routes for this link are preserved. sync only inspects routes for links which we managed and ignore routes for non-managed links.
func (rm *routeManager) sync() {
	for infName, rl := range rm.store {
		activeNlRoutes, err := netlink.RouteList(rl.link, nl.FAMILY_ALL)
		if err != nil {
			klog.Errorf("Route Manager: failed to list routes for link %q: %w", infName, err)
			continue
		}
		var activeRoutes []route
		for _, activeNlRoute := range activeNlRoutes {
			activeRoute, err := convertNetlinkRouteToRoutesPerLink(activeNlRoute)
			if err != nil {
				klog.Errorf("Route Manager: failed to convert netlink route (%s) to route: %v",
					activeRoute.String(), err)
				continue
			}
			activeRoutes = append(activeRoutes, activeRoute.routes...)
		}
		var syncNeeded bool
		var syncReason string
		for _, expectedRoute := range rl.routes {
			var found bool
			for _, activeRoute := range activeRoutes {
				if activeRoute.equal(expectedRoute) {
					found = true
				}
			}
			if !found {
				syncReason = fmt.Sprintf("failed to find route: %s", expectedRoute.string())
				syncNeeded = true
				break
			}
		}
		if syncNeeded {
			klog.Infof("Route Manager: sync required for routes associated with link %q. Reason: %s", infName, syncReason)
			if err = rm.applyRoutesPerLink(rl); err != nil {
				klog.Errorf("Route Manager: sync failed to apply route (%s): %v", rl.String(), err)
			}
		}
	}
}

type routesPerLink struct {
	link   netlink.Link
	routes []route
}

func (rl routesPerLink) validate() error {
	if rl.link == nil || rl.link.Attrs() == nil || rl.link.Attrs().Name == "" {
		return fmt.Errorf("link must be valid")
	}
	if len(rl.routes) == 0 {
		return fmt.Errorf("route must have a least one route entry")
	}
	for _, r := range rl.routes {
		if r.subnet == nil || r.subnet.String() == "<nil>" {
			return fmt.Errorf("invalid subnet for route entry")
		}
	}
	return nil
}

func (rl routesPerLink) getLinkName() (string, error) {
	if rl.link == nil || rl.link.Attrs() == nil || rl.link.Attrs().Name == "" {
		return "", fmt.Errorf("unable to get link name from: '%+v'", rl.link)
	}
	return rl.link.Attrs().Name, nil
}

func (rl routesPerLink) String() string {
	var routes string
	for i, r := range rl.routes {
		routes = fmt.Sprintf("%s Route %d: %q", routes, i+1, r.string())
	}
	return fmt.Sprintf("Route(s) for link name: %q, with %d routes: %s", rl.link.Attrs().Name, len(rl.routes), routes)
}

func (rl *routesPerLink) delRoutes(delRoutes []route) {
	if len(delRoutes) == 0 {
		return
	}
	routes := make([]route, 0)
	for _, existingRoute := range rl.routes {
		var found bool
		for _, delRoute := range delRoutes {
			if existingRoute.equal(delRoute) {
				found = true
			}
		}
		if !found {
			routes = append(routes, existingRoute)
		}
	}
	rl.routes = routes
}

type route struct {
	gwIP   net.IP
	subnet *net.IPNet
	mtu    int
	srcIP  net.IP
}

func (r route) equal(r2 route) bool {
	if r.mtu != r2.mtu {
		return false
	}
	if r.subnet.String() != r2.subnet.String() {
		return false
	}
	if r.gwIP.String() != r2.gwIP.String() {
		return false
	}
	if r.srcIP.String() != r2.srcIP.String() {
		return false
	}
	return true
}

func (r route) string() string {
	var s string
	if r.subnet != nil {
		s = fmt.Sprintf("Subnet: %s", r.subnet.String())
	}
	if r.mtu != 0 {
		s = fmt.Sprintf("%s MTU: %d ", s, r.mtu)
	}
	if len(r.srcIP) > 0 {
		s = fmt.Sprintf("%s Source IP: %q ", s, r.srcIP.String())
	}
	if len(r.gwIP) > 0 {
		s = fmt.Sprintf("%s Gateway IP: %q", s, r.gwIP.String())
	}
	return s
}

func convertRouteUpdateToRoutesPerLink(ru netlink.RouteUpdate) (routesPerLink, error) {
	link, err := netlink.LinkByIndex(ru.LinkIndex)
	if err != nil {
		return routesPerLink{}, fmt.Errorf("failed to get link by index from route update (%v): %w", ru, err)
	}

	return routesPerLink{
		link: link,
		routes: []route{
			{
				gwIP:   ru.Gw,
				subnet: ru.Dst,
				mtu:    ru.MTU,
				srcIP:  ru.Src,
			},
		},
	}, nil
}

func convertNetlinkRouteToRoutesPerLink(nlRoute netlink.Route) (routesPerLink, error) {
	link, err := netlink.LinkByIndex(nlRoute.LinkIndex)
	if err != nil {
		return routesPerLink{}, fmt.Errorf("failed to get link by index (%d) from route (%s): %w", nlRoute.LinkIndex,
			nlRoute.String(), err)
	}
	return routesPerLink{
		link: link,
		routes: []route{
			{
				gwIP:   nlRoute.Gw,
				subnet: nlRoute.Dst,
				mtu:    nlRoute.MTU,
				srcIP:  nlRoute.Src,
			},
		},
	}, nil
}

func getNetlinkIPFamily(ip net.IP) int {
	if utilnet.IsIPv6(ip) {
		return netlink.FAMILY_V6
	} else {
		return netlink.FAMILY_V4
	}
}

func filterRouteByDst(link netlink.Link, subnet *net.IPNet) (*netlink.Route, uint64) {
	return &netlink.Route{
			Dst:       subnet,
			LinkIndex: link.Attrs().Index,
		},
		netlink.RT_FILTER_DST | netlink.RT_FILTER_OIF
}

func subscribeNetlinkRouteEvents(stopCh <-chan struct{}) (bool, chan netlink.RouteUpdate) {
	routeEventCh := make(chan netlink.RouteUpdate, 20)
	if err := netlink.RouteSubscribe(routeEventCh, stopCh); err != nil {
		klog.Errorf("Route Manager: failed to subscribe to netlink route events: %v", err)
		return false, routeEventCh
	}
	return true, routeEventCh
}
