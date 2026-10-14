// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeimport

import (
	"fmt"
	"maps"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	nbdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	subscribePeriod         = 1 * time.Second
	subscribeBuffer         = 100
	reconcileDelay          = 500 * time.Millisecond
	noTable                 = -1
	noRouteLinkIndex        = -1
	controllerExternalIDKey = string(nbdbops.OwnerControllerKey)
	controllerName          = "RouteImport"

	// sync outcomes, used as the "result" label of the sync duration metric
	syncResultSuccess = "success"
	syncResultError   = "error"
)

type Manager interface {
	// AddNetwork instructs the manager to continuously reconcile BGP routes from
	// the network host vrf to the network gateway router. A network can only be
	// added once otherwise an error will be returned.
	AddNetwork(network util.NetInfo) error

	// NeedsReconciliation checks the provided network information against the
	// stored one and returns whether there is any change requires
	// reconciliation. If the network is not known to the manager, it returns
	// false.
	NeedsReconciliation(network util.NetInfo) bool

	// ReconcileNetwork triggers a manual reconciliation.
	ReconcileNetwork(name string) error

	// ForgetNetwork instructs the manager to stop reconciling BGP routes from
	// the network host vrf to the network gateway router.
	ForgetNetwork(name string)
}

type Controller interface {
	Manager
	Start() error
	Stop()
}

func New(node string, nbClient client.Client, uplinkStateLister uplinklisters.UplinkStateLister) Controller {
	c := &controller{
		ctx:               util.NewCancelableContext(),
		node:              node,
		nbClient:          nbClient,
		networkIDs:        map[int]string{},
		networks:          map[string]util.NetInfo{},
		tables:            map[int]int{},
		log:               klog.LoggerWithName(klog.Background(), controllerName),
		netlink:           util.GetNetLinkOps(),
		uplinkStateLister: uplinkStateLister,
	}

	c.reconciler = controllerutil.NewReconciler(
		controllerName,
		&controllerutil.ReconcilerConfig{
			Threadiness: 1,
			Reconcile:   c.syncNetwork,
		},
	)

	return c
}

type controller struct {
	ctx               util.CancelableContext
	nbClient          client.Client
	node              string
	log               logr.Logger
	reconciler        controllerutil.Reconciler
	netlink           util.NetLinkOps
	uplinkStateLister uplinklisters.UplinkStateLister

	sync.RWMutex
	networks map[string]util.NetInfo
	// network IDs to names
	networkIDs map[int]string
	// tables to network IDs, hint for syncRouteUpdate
	tables map[int]int
}

func (c *controller) AddNetwork(network util.NetInfo) error {
	c.Lock()
	defer c.Unlock()

	networkID := network.GetNetworkID()
	if c.networkIDs[networkID] != "" {
		return fmt.Errorf("already tracking network %q with ID %d",
			c.networkIDs[networkID],
			networkID,
		)
	}

	name := network.GetNetworkName()
	if c.networks[name] != nil {
		// this shouldn't happen as the network ID is correlated uniquely with
		// the network name, but do the check anyway in case this is not being
		// handled correctly
		return fmt.Errorf("already tracking network name %q", name)
	}

	c.networkIDs[networkID] = name
	c.networks[name] = network
	if network.IsDefault() {
		c.tables[unix.RT_TABLE_MAIN] = networkID
	}

	c.log.V(5).Info("Started tracking network", "name", name, "id", networkID)
	c.reconcile(name)

	return nil
}

func (c *controller) ForgetNetwork(name string) {
	c.Lock()
	defer c.Unlock()

	network := c.networks[name]
	if network == nil {
		return
	}

	delete(c.networkIDs, network.GetNetworkID())
	delete(c.networks, name)
	c.setTableForNetworkUnlocked(network.GetNetworkID(), noTable)

	c.log.V(5).Info("Stopped tracking network", "name", name)
}

func (c *controller) NeedsReconciliation(network util.NetInfo) bool {
	c.RLock()
	defer c.RUnlock()

	stored := c.networks[network.GetNetworkName()]
	if stored == nil {
		return false
	}

	// TODO check if overlay mode changed
	return stored.Uplink() != network.Uplink()
}

func (c *controller) ReconcileNetwork(name string) error {
	c.RLock()
	defer c.RUnlock()
	if c.networks[name] == nil {
		return fmt.Errorf("unknown network with name %q", name)
	}
	c.log.V(5).Info("Reconciling network", "name", name)
	c.reconcile(name)
	return nil
}

func (c *controller) Start() error {
	defer c.log.Info("Controller started")
	c.subscribe(c.ctx.Done())
	return controllerutil.Start(c.reconciler)
}

func (c *controller) Stop() {
	controllerutil.Stop(c.reconciler)
	c.ctx.Cancel()
	c.log.Info("Controller stopped")
}

func (c *controller) subscribe(stop <-chan struct{}) {
	go func() {
		onError := func(err error) {
			c.log.Error(err, "Error on netlink route event subscription")
		}
		routeEventCh := subscribeNetlinkRouteEvents(c.netlink, stop, onError)
		subscribeTicker := time.NewTicker(subscribePeriod)
		defer subscribeTicker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-subscribeTicker.C:
				if routeEventCh != nil {
					continue
				}
				routeEventCh = subscribeNetlinkRouteEvents(c.netlink, stop, onError)
			case r, open := <-routeEventCh:
				if !open {
					routeEventCh = subscribeNetlinkRouteEvents(c.netlink, stop, onError)
					continue
				}
				c.log.V(5).Info("Received route event", "event", r)
				c.syncRouteUpdate(&r)
			}
		}
	}()

	go func() {
		onError := func(err error) {
			c.log.Error(err, "Error on netlink link event subscription")
		}
		linkEventCh := subscribeNetlinkLinkEvents(c.netlink, stop, onError)
		subscribeTicker := time.NewTicker(subscribePeriod)
		defer subscribeTicker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-subscribeTicker.C:
				if linkEventCh != nil {
					continue
				}
				linkEventCh = subscribeNetlinkLinkEvents(c.netlink, stop, onError)
			case l, open := <-linkEventCh:
				if !open {
					c.tables = map[int]int{}
					linkEventCh = subscribeNetlinkLinkEvents(c.netlink, stop, onError)
					continue
				}
				c.log.V(5).Info("Received link event", "event", l)
				c.syncLinkUpdate(&l)
			}
		}
	}()
}

func (c *controller) syncRouteUpdate(update *netlink.RouteUpdate) {
	if update.Protocol != unix.RTPROT_BGP {
		return
	}

	table := update.Table
	network := c.getNetworkForTable(table)
	if network != nil {
		c.reconcile(network.GetNetworkName())
	}
}

func (c *controller) syncLinkUpdate(update *netlink.LinkUpdate) {
	vrf, isVrf := update.Link.(*netlink.Vrf)
	if !isVrf {
		return
	}

	networkID := util.ParseNetworkIDFromVRFName(vrf.Name)

	c.Lock()
	defer c.Unlock()

	// for CUDNs, VRF name equals network name sans prefix
	network := c.networks[util.GenerateCUDNNetworkName(vrf.Name)]
	// but if we got an ID, this can't be a CUDN, it's a UDN.
	if networkID != types.InvalidID {
		network = c.networks[c.networkIDs[networkID]]
	}

	// if the network is unknown do nothing for now and wait for the
	// reconciliation after AddNetwork to handle things
	if network == nil {
		c.log.V(5).Info("Ignoring VRF event of unknown network", "vrf", vrf.Name)
		return
	}

	// we only care about VRF updates. If a VRF is deleted we assume the network
	// itself is being deleted and that will be handled through ForgetNetwork
	if update.Header.Type != unix.RTM_NEWLINK {
		return
	}

	table := int(vrf.Table)
	networkID = network.GetNetworkID()
	needsReconcile := c.tables[table] != networkID
	if needsReconcile {
		c.setTableForNetworkUnlocked(networkID, table)
		networkName := network.GetNetworkName()
		c.log.V(5).Info("Associated table with network", "table", table, "network", networkName)
		c.reconcile(networkName)
	}
}

func (c *controller) reconcile(network string) {
	c.reconciler.ReconcileAfter(network, reconcileDelay)
}

type route struct {
	dst string
	gw  string
}

type stringer struct {
	v any
}

func (s stringer) String() string {
	return fmt.Sprintf("%v", s.v)
}

func (c *controller) syncNetwork(network string) (err error) {
	start := time.Now()
	forgotten := false
	defer func() {
		if forgotten {
			// drop the per-network timeseries last: observing the duration
			// here would recreate the series we just removed
			metrics.DeleteRouteImportMetrics(network)
			return
		}
		// derived from the returned error rather than tracked per branch, so
		// an early return added later cannot report the wrong outcome
		result := syncResultSuccess
		if err != nil {
			result = syncResultError
		}
		metrics.RecordRouteImportSync(network, result, time.Since(start))
	}()
	c.log.V(5).Info("Reconciling network", "network", network)

	info := c.getNetwork(network)
	if info == nil {
		forgotten = true
		return nil
	}

	// get the table from the network VRF. Note we go to netlink for this as
	// source of truth instead of using c.tables cache which is just a hint for
	// syncRouteUpdate. This avoids implementing a more complicated logic to
	// mantain c.tables
	table, err := c.getRoutingTableForNetwork(network)
	if err != nil {
		return fmt.Errorf("failed to get VRF table from network: %w", err)
	}
	if table == noTable {
		// no VRF exists yet for the network
		return nil
	}

	// sneakily set the hint for syncRouteUpdate. Handles this sequence of events:
	// 1. link create event
	// 2. add Network
	// 3. Route update event <- we wouldn't know the network of a table to add
	//    routes to
	c.Lock()
	c.setTableForNetworkUnlocked(info.GetNetworkID(), table)
	c.Unlock()

	var ignoreSubnets []*net.IPNet
	if info.Transport() != types.NetworkTransportNoOverlay {
		// if the network is overlay mode, skip routes to the pod network
		ignoreSubnets = make([]*net.IPNet, len(info.Subnets()))
		for i, subnet := range info.Subnets() {
			ignoreSubnets[i] = subnet.CIDR
		}
	}

	routeLinkIndex, err := c.getRouteLinkIndexFilter(info)
	if err != nil {
		return err
	}

	expected, err := c.getBGPRoutes(table, ignoreSubnets, routeLinkIndex)
	if err != nil {
		return err
	}

	router := info.GetNetworkScopedGWRouterName(c.node)
	// we set the outport incase our IPv6 next hops are link local addresses
	outport := types.GWRouterToExtSwitchPrefix + router
	actual, uuids, err := c.getOVNRoutes(router)
	if err != nil {
		return fmt.Errorf("failed to get routes from OVN: %w", err)
	}
	metrics.RecordRouteImportRoutes(network, expected.Len(), actual.Len())

	deletes := actual.Difference(expected)
	adds := expected.Difference(actual)
	if len(deletes)+len(adds) == 0 {
		c.log.V(5).Info("Found no updates for router", "router", router)
		return nil
	}
	c.log.V(5).Info("Found updates for router", "router", router, "adds", stringer{adds}, "deletes", stringer{deletes})

	var errs []error
	var ops []ovsdb.Operation

	newRoutes := make([]*nbdb.LogicalRouterStaticRoute, 0, len(adds))
	for add := range adds {
		newRoutes = append(newRoutes, &nbdb.LogicalRouterStaticRoute{
			IPPrefix:    add.dst,
			Nexthop:     add.gw,
			OutputPort:  &outport,
			ExternalIDs: map[string]string{controllerExternalIDKey: controllerName},
		})
	}
	// The difference already identifies missing routes. Batch their creation to
	// avoid per-route cache lookups that deep-copy the router and its StaticRoutes.
	ops, err = nbdbops.CreateLogicalRouterStaticRoutesOps(c.nbClient, ops, router, newRoutes...)
	if err != nil {
		return fmt.Errorf("failed to add routes on router %s: %w", router, err)
	}

	lrsrs := make([]*nbdb.LogicalRouterStaticRoute, 0, len(deletes))
	for delete := range deletes {
		lrsrs = append(lrsrs, &nbdb.LogicalRouterStaticRoute{UUID: uuids[delete]})
	}
	if len(lrsrs) > 0 {
		ops, err = nbdbops.DeleteLogicalRouterStaticRoutesOps(c.nbClient, ops, router, lrsrs...)
		if err != nil {
			err := fmt.Errorf("failed to delete routes on router %s: %w", router, err)
			errs = append(errs, err)
		}
	}

	_, err = nbdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		err := fmt.Errorf("failed to transact ops %v: %w", ops, err)
		errs = append(errs, err)
	}

	err = errors.Join(errs...)
	if err == nil {
		metrics.RecordRouteImportOps(network, len(adds), len(deletes))
		// the transaction applied, so OVN now holds what the kernel holds;
		// the two gauges only diverge while a sync is pending or failing
		metrics.RecordRouteImportRoutes(network, expected.Len(), expected.Len())
	}
	c.log.V(5).Info("Reconciled network", "network", network, "took", time.Since(start), "ops", ops, "errors", err)
	return err
}

func (c *controller) getBGPRoutes(table int, ignoreSubnets []*net.IPNet, routeLinkIndex int) (sets.Set[route], error) {
	start := time.Now()
	filter := &netlink.Route{
		Protocol: unix.RTPROT_BGP,
		Table:    table,
	}
	routes := sets.New[route]()
	// Convert routes as they arrive instead of retaining a second, much larger
	// representation of the complete routing table during reconciliation.
	err := c.netlink.RouteListFilteredIter(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE, func(nlroute netlink.Route) bool {
		if util.IsContainedInAnyCIDR(nlroute.Dst, ignoreSubnets...) {
			c.log.V(5).Info("Ignore BGP route", "table", table, "route", stringer{nlroute})
			return true
		}
		routes.Insert(routesFromNetlinkRoute(&nlroute, routeLinkIndex)...)
		return true
	})
	if err != nil {
		// An interrupted dump is not an authoritative snapshot: applying its
		// partial result could withdraw valid OVN routes.
		return nil, fmt.Errorf("failed to list BGP routes: %w", err)
	}

	c.log.V(5).Info("Listed BGP routes", "table", table, "routes", stringer{routes}, "took", time.Since(start))
	return routes, nil
}

// getRouteLinkIndexFilter returns the Linux link index that imported routes
// must use for Uplink-backed networks. Those GRs are attached only to the
// selected Uplink bridge, so BGP routes learned or leaked through other
// interfaces cannot be installed into OVN for that network.
func (c *controller) getRouteLinkIndexFilter(network util.NetInfo) (int, error) {
	uplinkName := network.Uplink()
	if uplinkName == "" {
		return 0, nil
	}
	if c.uplinkStateLister == nil {
		return 0, fmt.Errorf("cannot filter BGP routes for Uplink %q without UplinkState lister", uplinkName)
	}

	stateName := uplinkutil.StateName(uplinkName, c.node)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.node)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return noRouteLinkIndex, nil
		}
		return 0, fmt.Errorf("failed to get UplinkState %s for route import: %w", stateName, err)
	}
	condition := meta.FindStatusCondition(
		state.Status.Conditions,
		uplinkv1alpha1.UplinkStateConditionResolved,
	)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return noRouteLinkIndex, nil
	}
	if state.Status.OVSBridge == nil || state.Status.OVSBridge.Name == "" {
		return noRouteLinkIndex, nil
	}

	link, err := c.netlink.LinkByName(state.Status.OVSBridge.Name)
	if err != nil {
		if c.netlink.IsLinkNotFoundError(err) {
			return noRouteLinkIndex, nil
		}
		return 0, fmt.Errorf("failed to resolve Uplink bridge %s for route import: %w",
			state.Status.OVSBridge.Name, err)
	}
	if link.Attrs() == nil {
		return 0, fmt.Errorf("uplink bridge %s has no link attributes", state.Status.OVSBridge.Name)
	}
	return link.Attrs().Index, nil
}

func (c *controller) getOVNRoutes(router string) (sets.Set[route], map[route]string, error) {
	start := time.Now()
	lr := &nbdb.LogicalRouter{
		Name: router,
	}
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs[controllerExternalIDKey] == controllerName
	}
	lrsrs, err := nbdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(c.nbClient, lr, p)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get routes from router %s: %w", router, err)
	}
	uuids := make(map[route]string, len(lrsrs))
	routes := make(sets.Set[route], len(lrsrs))
	for _, lrsr := range lrsrs {
		r := route{dst: lrsr.IPPrefix, gw: lrsr.Nexthop}
		routes.Insert(r)
		uuids[r] = lrsr.UUID
	}
	c.log.V(5).Info("Listed OVN routes", "router", router, "routes", stringer{routes}, "took", time.Since(start))
	return routes, uuids, nil
}

func (c *controller) getNetwork(network string) util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	return c.networks[network]
}

func (c *controller) getRoutingTableForNetwork(name string) (int, error) {
	network := c.getNetwork(name)
	if network == nil {
		// unknown network, shouldn't happen but in any case will reconcile
		// later if network is added
		return noTable, nil
	}
	if network.IsDefault() {
		return unix.RT_TABLE_MAIN, nil
	}
	vrf := util.GetNetworkVRFName(network)
	link, err := c.netlink.LinkByName(vrf)
	if c.netlink.IsLinkNotFoundError(err) {
		// unknown link, will reconcile later if link is updated
		return noTable, nil
	}
	if err != nil {
		return noTable, err
	}
	vrfLink, isVrf := link.(*netlink.Vrf)
	if !isVrf {
		// unexpected type, log error, will reconcile later if link is updated
		c.log.Error(nil, "Expected a VRF but got a different device type", "name", vrf, "type", link.Type())
		return noTable, nil
	}

	return int(vrfLink.Table), nil
}

func (c *controller) getNetworkForTable(table int) util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	if network, known := c.tables[table]; known {
		return c.networks[c.networkIDs[network]]
	}
	return nil
}

// setTableForNetworkUnlocked needs to be called with lock
func (c *controller) setTableForNetworkUnlocked(networkID, table int) {
	maps.DeleteFunc(c.tables, func(_, id int) bool { return id == networkID })
	if table == noTable {
		return
	}
	c.tables[table] = networkID
}

func routesFromNetlinkRoute(r *netlink.Route, routeLinkIndex int) []route {
	if routeLinkIndex == noRouteLinkIndex {
		return nil
	}
	validIP := func(ip string) bool {
		if ip == "" || ip == "<nil>" {
			return false
		}
		return true
	}
	if r.Dst == nil {
		return nil
	}
	dst := r.Dst.String()
	if !validIP(dst) {
		return nil
	}
	var routes []route
	if len(r.MultiPath) > 0 {
		for _, nh := range r.MultiPath {
			if routeLinkIndex != 0 && nh.LinkIndex != routeLinkIndex {
				continue
			}
			gw := nh.Gw.String()
			if validIP(gw) {
				routes = append(routes, route{dst: dst, gw: gw})
			}
		}
		return routes
	}
	if routeLinkIndex != 0 && r.LinkIndex != routeLinkIndex {
		return nil
	}
	gw := r.Gw.String()
	if validIP(gw) {
		routes = append(routes, route{dst: dst, gw: gw})
	}
	return routes
}

func subscribeNetlinkRouteEvents(nlops util.NetLinkOps, stopCh <-chan struct{}, onError func(error)) chan netlink.RouteUpdate {
	routeEventCh := make(chan netlink.RouteUpdate, subscribeBuffer)
	options := netlink.RouteSubscribeOptions{
		ErrorCallback: onError,
		ListExisting:  true,
	}
	err := nlops.RouteSubscribeWithOptions(routeEventCh, stopCh, options)
	if err != nil {
		onError(err)
		return nil
	}
	return routeEventCh
}

func subscribeNetlinkLinkEvents(nlops util.NetLinkOps, stopCh <-chan struct{}, onError func(error)) chan netlink.LinkUpdate {
	linkEventCh := make(chan netlink.LinkUpdate, subscribeBuffer)
	options := netlink.LinkSubscribeOptions{
		ErrorCallback: onError,
		ListExisting:  true,
	}
	if err := nlops.LinkSubscribeWithOptions(linkEventCh, stopCh, options); err != nil {
		onError(err)
		return nil
	}
	return linkEventCh
}
