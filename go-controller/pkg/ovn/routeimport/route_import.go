package routeimport

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	nbdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	subscribePeriod = 1 * time.Second
	subscribeBuffer = 100
	reconcileDelay  = 500 * time.Millisecond
)

type Manager interface {
	// AddNetwork instructs the manager to continously reconcile BGP routes from
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

func New(node string, nbClient client.Client) Controller {
	c := &controller{
		ctx:        util.NewCancelableContext(),
		node:       node,
		nbClient:   nbClient,
		networkIDs: map[int]string{},
		networks:   map[string]*netInfo{},
		tables:     map[int]int{},
		log:        klog.LoggerWithName(klog.Background(), "RouteImport"),
		netlink:    util.GetNetLinkOps(),
	}

	c.reconciler = controllerutil.NewReconciler(
		"RouteImport",
		&controllerutil.ReconcilerConfig{
			Threadiness: 1,
			Reconcile:   c.syncNetwork,
			RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		},
	)

	return c
}

type netInfo struct {
	util.NetInfo
	id    int
	table int
}

type controller struct {
	ctx        util.CancelableContext
	nbClient   client.Client
	node       string
	log        logr.Logger
	reconciler controllerutil.Reconciler
	netlink    util.NetLinkOps

	sync.RWMutex
	networkIDs map[int]string
	networks   map[string]*netInfo
	tables     map[int]int
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
		return fmt.Errorf("already tracking network name %q", name)
	}

	info := &netInfo{NetInfo: network, id: networkID}
	if network.IsDefault() {
		c.tables[unix.RT_TABLE_MAIN] = networkID
		info.table = unix.RT_TABLE_MAIN
	}
	c.networkIDs[networkID] = name
	c.networks[name] = info

	c.log.V(5).Info("Added network", "name", name, "id", networkID)
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

	delete(c.networkIDs, network.id)
	delete(c.networks, name)

	c.log.V(5).Info("Forgot network", "name", name)
}

func (c *controller) NeedsReconciliation(network util.NetInfo) bool {
	c.RLock()
	defer c.RUnlock()

	if c.networks[network.GetNetworkName()] == nil {
		return false
	}

	// TODO check if overlay mode changed
	return false
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
	setTimer := func(t *time.Timer, d time.Duration) *time.Timer {
		if t == nil {
			return time.NewTimer(d)
		}
		t.Stop()
		select {
		case <-t.C:
		default:
		}
		t.Reset(d)
		return t
	}

	go func() {
		onError := func(err error) {
			c.log.Error(err, "Error on netlink route event subscription")
		}
		routeEventCh := subscribeNetlinkRouteEvents(c.netlink, stop, onError)
		t := setTimer(nil, subscribePeriod)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if routeEventCh != nil {
					continue
				}
				routeEventCh = subscribeNetlinkRouteEvents(c.netlink, stop, onError)
				if routeEventCh == nil {
					t = setTimer(t, subscribePeriod)
				}
			case r, open := <-routeEventCh:
				if !open {
					c.log.Info("Subscription to netlink route events closed")
					routeEventCh = nil
					t = setTimer(t, subscribePeriod)
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
		t := setTimer(nil, subscribePeriod)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if linkEventCh != nil {
					continue
				}
				linkEventCh = subscribeNetlinkLinkEvents(c.netlink, stop, onError)
				if linkEventCh == nil {
					t = setTimer(t, subscribePeriod)
					continue
				}
				c.tables = map[int]int{}
			case l, open := <-linkEventCh:
				if !open {
					c.log.Info("Subscription to netlink link events closed")
					linkEventCh = nil
					t = setTimer(t, subscribePeriod)
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

	name := vrf.Name
	if !strings.HasPrefix(name, types.UDNVRFDevicePrefix) {
		return
	}
	if !strings.HasSuffix(name, types.UDNVRFDeviceSuffix) {
		return
	}

	id, err := strconv.Atoi(name[len(types.UDNVRFDevicePrefix) : len(name)-len(types.UDNVRFDeviceSuffix)])
	if err != nil {
		c.log.Error(err, "Failed to parse network ID from device name", "name", name)
		return
	}

	c.Lock()
	defer c.Unlock()

	table := int(vrf.Table)
	network := c.networkIDs[id]
	info := c.networks[network]
	current := c.tables[table] == id
	var old int
	if info != nil {
		old = info.table
	}

	switch update.IfInfomsg.Type {
	case unix.RTM_DELLINK:
		if !current {
			c.log.Info("Ignoring VRF delete for old network", "network", id)
			return
		}
		delete(c.tables, table)
		table = 0
	case unix.RTM_NEWLINK:
		delete(c.tables, old)
		c.tables[table] = id
	default:
		c.log.Info("Unexpected VRF update event type", "type", update.IfInfomsg.Type)
		return
	}
	if info != nil {
		info.table = table
	}

	reconcile := info != nil && table != 0 && !current

	c.log.V(5).Info("Associated table with network", "table", table, "network", id, "reconcile", reconcile)
	if reconcile {
		c.reconcile(network)
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

func (c *controller) syncNetwork(network string) error {
	start := time.Now()
	c.log.V(5).Info("Reconciling network", "network", network)

	info := c.getNetwork(network)
	if info == nil {
		return nil
	}
	router := info.GetNetworkScopedGWRouterName(c.node)

	// skip routes in the pod network
	// TODO do not skip these routes in no overlay mode
	ignoreSubnets := make([]*net.IPNet, len(info.Subnets()))
	for i, subnet := range info.Subnets() {
		ignoreSubnets[i] = subnet.CIDR
	}

	table := c.getTableForNetwork(info.id)
	if table == 0 {
		return nil
	}

	expected, err := c.getBGPRoutes(table, ignoreSubnets)
	if err != nil {
		return err
	}

	actual, uuids, err := c.getOVNRoutes(router)
	if err != nil {
		return fmt.Errorf("failed to get routes from OVN: %w", err)
	}

	deletes := actual.Difference(expected)
	adds := expected.Difference(actual)
	if len(deletes)+len(adds) == 0 {
		c.log.V(5).Info("Found no updates for router", "router", router)
		return nil
	}
	c.log.V(5).Info("Found updates for router", "router", router, "adds", stringer{adds}, "deletes", stringer{deletes})

	var errs []error
	var ops []ovsdb.Operation

	p := func(new, db *nbdb.LogicalRouterStaticRoute) bool {
		return db.ExternalIDs["controller"] == "routeimport" && db.IPPrefix == new.IPPrefix && db.Nexthop == new.Nexthop
	}
	for add := range adds {
		lrsr := &nbdb.LogicalRouterStaticRoute{
			UUID:        uuids[add],
			IPPrefix:    add.dst,
			Nexthop:     add.gw,
			ExternalIDs: map[string]string{"controller": "routeimport"},
		}
		p := func(db *nbdb.LogicalRouterStaticRoute) bool { return p(lrsr, db) }
		ops, err = nbdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicateOps(c.nbClient, ops, router, lrsr, p)
		if err != nil {
			err := fmt.Errorf("failed to add routes on router %s: %w", router, err)
			errs = append(errs, err)
			continue
		}
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
	c.log.V(5).Info("Reconciled network", "network", network, "took", time.Since(start), "ops", ops, "errors", err)
	return err
}

func (c *controller) getBGPRoutes(table int, ignoreSubnets []*net.IPNet) (sets.Set[route], error) {
	start := time.Now()
	filter := &netlink.Route{
		Protocol: unix.RTPROT_BGP,
		Table:    table,
	}
	nlroutes, err := c.netlink.RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("failed to list BGP routes: %w", err)
	}

	routes := sets.New[route]()
	for _, nlroute := range nlroutes {
		if util.IsContainedInAnyCIDR(nlroute.Dst, ignoreSubnets...) {
			continue
		}
		routes.Insert(routesFromNetlinkRoute(&nlroute)...)
	}

	c.log.V(5).Info("Listed BGP routes", "table", table, "routes", stringer{routes}, "took", time.Since(start))
	return routes, nil
}

func (c *controller) getOVNRoutes(router string) (sets.Set[route], map[route]string, error) {
	start := time.Now()
	lr := &nbdb.LogicalRouter{
		Name: router,
	}
	p := func(lrsr *nbdb.LogicalRouterStaticRoute) bool {
		return lrsr.ExternalIDs["controller"] == "routeimport"
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

func (c *controller) getNetwork(network string) *netInfo {
	c.RLock()
	defer c.RUnlock()
	return c.networks[network]
}

func (c *controller) getTableForNetwork(network int) int {
	c.RLock()
	defer c.RUnlock()
	if info := c.networks[c.networkIDs[network]]; info != nil {
		return info.table
	}
	return 0
}

func (c *controller) getNetworkForTable(table int) *netInfo {
	c.RLock()
	defer c.RUnlock()
	if network, known := c.tables[table]; known {
		return c.networks[c.networkIDs[network]]
	}
	return nil
}

func routesFromNetlinkRoute(r *netlink.Route) []route {
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
	gw := r.Gw.String()
	if validIP(gw) {
		routes = append(routes, route{dst: dst, gw: gw})
	}
	for _, nh := range r.MultiPath {
		gw = nh.Gw.String()
		if validIP(gw) {
			routes = append(routes, route{dst: dst, gw: gw})
		}
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
