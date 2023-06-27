package routemanager

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

type RouteManager struct {
	// log all route netlink events received
	logRouteChanges bool
	// period for which we want to check that the routes we manage are applied. This is needed for the rare case
	// we miss a route event.
	syncPeriod time.Duration
	store      map[string]RoutesPerLink // key is link name
	addRouteCh chan RoutesPerLink
	delRouteCh chan RoutesPerLink
}

// NewRouteManager manages routes which include adding and deletion of routes. It also manages restoration of managed routes.
// Begin managing routes by calling Run() to start the manager.
// Routes should be added via add(route) and deletion via del(route) functions only.
// All other functions are used internally.
func NewRouteManager(logRouteChanges bool, syncPeriod time.Duration) *RouteManager {
	return &RouteManager{
		logRouteChanges: logRouteChanges,
		syncPeriod:      syncPeriod,
		store:           make(map[string]RoutesPerLink),
		addRouteCh:      make(chan RoutesPerLink, 5),
		delRouteCh:      make(chan RoutesPerLink, 5),
	}
}

func (rm *RouteManager) Run(stopCh <-chan struct{}) {
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

func (rm *RouteManager) Add(rl RoutesPerLink) {
	rm.addRouteCh <- rl
}

func (rm *RouteManager) Del(rl RoutesPerLink) {
	rm.delRouteCh <- rl
}

func (rm *RouteManager) addRoutesPerLink(rl RoutesPerLink) error {
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

func (rm *RouteManager) delRoutesPerLink(rl RoutesPerLink) error {
	klog.Infof("Route Manager: attempting to delete routes for link: %s", rl.String())
	if err := rl.validate(); err != nil {
		return fmt.Errorf("failed to validate route for link (%s): %v", rl.String(), err)
	}
	var deletedRoutes []Route
	for _, r := range rl.Routes {
		if err := rm.netlinkDelRoute(rl.Link, r.Subnet); err != nil {
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
		routesPerLinkFound = RoutesPerLink{rl.Link, []Route{}}
	}
	if len(deletedRoutes) > 0 {
		routesPerLinkFound.delRoutes(deletedRoutes)
	}
	if len(routesPerLinkFound.Routes) == 0 {
		delete(rm.store, infName)
	} else {
		rm.store[infName] = routesPerLinkFound
	}
	klog.Infof("Route Manager: deletion of routes for link complete: %s", rl.String())
	return nil
}

// processNetlinkEvent will log new and deleted routes if logRouteChanges is true. It will also check if a deleted route
// is managed by route manager and if so, determine if a sync is needed to restore any managed routes.
func (rm *RouteManager) processNetlinkEvent(ru netlink.RouteUpdate) error {
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
	for _, managedRoute := range rl.Routes {
		for _, routeEvent := range rlEvent.Routes {
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

func (rm *RouteManager) applyRoutesPerLink(rl RoutesPerLink) error {
	for _, r := range rl.Routes {
		if err := rm.applyRoute(rl.Link, r.GWIP, r.Subnet, r.MTU, r.SRCIP); err != nil {
			return fmt.Errorf("failed to apply route (%s) because of error: %v", r.string(), err)
		}
	}
	return nil
}

func (rm *RouteManager) applyRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, src net.IP) error {
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

func (rm *RouteManager) netlinkAddRoute(link netlink.Link, gwIP net.IP, subnet *net.IPNet, mtu int, srcIP net.IP) error {
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

func (rm *RouteManager) netlinkDelRoute(link netlink.Link, subnet *net.IPNet) error {
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

func (rm *RouteManager) addRoutesPerLinkStore(rl RoutesPerLink) error {
	infName, err := rl.getLinkName()
	if err != nil {
		return fmt.Errorf("failed to add route for link (%s) to store because we could not get link name", rl.String())
	}
	managedRl, ok := rm.store[infName]
	if !ok {
		rm.store[infName] = rl
		return nil
	}
	newRoutes := make([]Route, 0)
	for _, newRoute := range rl.Routes {
		var found bool
		for _, managedRoute := range managedRl.Routes {
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
	managedRl.Routes = append(managedRl.Routes, newRoutes...)
	rm.store[infName] = managedRl
	return nil
}

// sync will iterate through all links routes seen on a node and ensure any route manager managed routes are applied. Any additional
// routes for this link are preserved. sync only inspects routes for links which we managed and ignore routes for non-managed links.
func (rm *RouteManager) sync() {
	for infName, rl := range rm.store {
		activeNlRoutes, err := netlink.RouteList(rl.Link, nl.FAMILY_ALL)
		if err != nil {
			klog.Errorf("Route Manager: failed to list routes for link %q: %w", infName, err)
			continue
		}
		var activeRoutes []Route
		for _, activeNlRoute := range activeNlRoutes {
			activeRoute, err := convertNetlinkRouteToRoutesPerLink(activeNlRoute)
			if err != nil {
				klog.Errorf("Route Manager: failed to convert netlink route (%s) to route: %v",
					activeRoute.String(), err)
				continue
			}
			activeRoutes = append(activeRoutes, activeRoute.Routes...)
		}
		var syncNeeded bool
		var syncReason string
		for _, expectedRoute := range rl.Routes {
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

type RoutesPerLink struct {
	Link   netlink.Link
	Routes []Route
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
		routes = fmt.Sprintf("%s Route %d: %q", routes, i+1, r.string())
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
			if existingRoute.equal(delRoute) {
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
	GWIP   net.IP
	Subnet *net.IPNet
	MTU    int
	SRCIP  net.IP
}

func (r Route) equal(r2 Route) bool {
	if r.MTU != r2.MTU {
		return false
	}
	if r.Subnet.String() != r2.Subnet.String() {
		return false
	}
	if r.GWIP.String() != r2.GWIP.String() {
		return false
	}
	if r.SRCIP.String() != r2.SRCIP.String() {
		return false
	}
	return true
}

func (r Route) string() string {
	var s string
	if r.Subnet != nil {
		s = fmt.Sprintf("Subnet: %s", r.Subnet.String())
	}
	if r.MTU != 0 {
		s = fmt.Sprintf("%s MTU: %d ", s, r.MTU)
	}
	if len(r.SRCIP) > 0 {
		s = fmt.Sprintf("%s Source IP: %q ", s, r.SRCIP.String())
	}
	if len(r.GWIP) > 0 {
		s = fmt.Sprintf("%s Gateway IP: %q", s, r.GWIP.String())
	}
	return s
}

func convertRouteUpdateToRoutesPerLink(ru netlink.RouteUpdate) (RoutesPerLink, error) {
	link, err := netlink.LinkByIndex(ru.LinkIndex)
	if err != nil {
		return routesPerLink{}, fmt.Errorf("failed to get link by index from route update (%v): %w", ru, err)
	}

	return RoutesPerLink{
		Link: link,
		Routes: []Route{
			{
				GWIP:   ru.Gw,
				Subnet: ru.Dst,
				MTU:    ru.MTU,
				SRCIP:  ru.Src,
			},
		},
	}, nil
}

func convertNetlinkRouteToRoutesPerLink(nlRoute netlink.Route) (RoutesPerLink, error) {
	link, err := netlink.LinkByIndex(nlRoute.LinkIndex)
	if err != nil {
		return RoutesPerLink{}, fmt.Errorf("failed to get link by index (%d) from route (%s): %w", nlRoute.LinkIndex,
			nlRoute.String(), err)
	}
	return RoutesPerLink{
		Link: link,
		Routes: []Route{
			{
				GWIP:   nlRoute.Gw,
				Subnet: nlRoute.Dst,
				MTU:    nlRoute.MTU,
				SRCIP:  nlRoute.Src,
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
