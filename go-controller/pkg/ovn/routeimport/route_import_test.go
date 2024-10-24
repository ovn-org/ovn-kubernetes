package routeimport

import (
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/util/workqueue"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntesting "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func Test_controller_syncNetwork(t *testing.T) {
	node := "testnode"
	defaultNetwork := &util.DefaultNetInfo{}
	type fields struct {
		networkIDs map[int]string
		networks   map[string]*netInfo
		tables     map[int]int
	}
	type args struct {
		network string
	}
	tests := []struct {
		name      string
		fields    fields
		args      args
		initial   []libovsdb.TestData
		expected  []libovsdb.TestData
		routes    []netlink.Route
		routesErr bool
		wantErr   bool
	}{
		{
			name: "reconciled ignored if network not known",
			args: args{"default"},
		},
		{
			name: "reconciled ignored if network table not known",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]*netInfo{"default": {NetInfo: defaultNetwork, id: 0}},
			},
		},
		{
			name: "fails if kernel routes cannot be fetched",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]*netInfo{"default": {NetInfo: defaultNetwork, id: 0, table: unix.RT_TABLE_MAIN}},
			},
			routesErr: true,
			wantErr:   true,
		},
		{
			name: "fails if OVN routes cannot be fetched (i.e. router does not exist)",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]*netInfo{"default": {NetInfo: defaultNetwork, id: 0, table: unix.RT_TABLE_MAIN}},
			},
			wantErr: true,
		},
		{
			name: "adds and removes routes as necessary",
			args: args{"default"},
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]*netInfo{"default": {NetInfo: defaultNetwork, id: 0, table: unix.RT_TABLE_MAIN}},
			},
			initial: []libovsdb.TestData{
				&nbdb.LogicalRouter{Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1", "keep-2", "remove"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", ExternalIDs: map[string]string{"controller": "routeimport"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-2", IPPrefix: "5.5.5.0/24", Nexthop: "5.5.5.1"},
				&nbdb.LogicalRouterStaticRoute{UUID: "remove", IPPrefix: "6.6.6.0/24", Nexthop: "6.6.6.1", ExternalIDs: map[string]string{"controller": "routeimport"}},
			},
			routes: []netlink.Route{
				{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
				{Dst: ovntesting.MustParseIPNet("2.2.2.0/24"), Gw: ovntesting.MustParseIP("2.2.2.1")},
				{Dst: ovntesting.MustParseIPNet("3.3.3.0/24"), MultiPath: []*netlink.NexthopInfo{{Gw: ovntesting.MustParseIP("3.3.3.1")}, {Gw: ovntesting.MustParseIP("3.3.3.2")}}},
			},
			expected: []libovsdb.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: defaultNetwork.GetNetworkScopedGWRouterName(node), StaticRoutes: []string{"keep-1", "keep-2", "add-1", "add-2", "add-3"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-1", IPPrefix: "1.1.1.0/24", Nexthop: "1.1.1.1", ExternalIDs: map[string]string{"controller": "routeimport"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "keep-2", IPPrefix: "5.5.5.0/24", Nexthop: "5.5.5.1"},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-1", IPPrefix: "2.2.2.0/24", Nexthop: "2.2.2.1", ExternalIDs: map[string]string{"controller": "routeimport"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-2", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.1", ExternalIDs: map[string]string{"controller": "routeimport"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "add-3", IPPrefix: "3.3.3.0/24", Nexthop: "3.3.3.2", ExternalIDs: map[string]string{"controller": "routeimport"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			nlmock := &mocks.NetLinkOps{}
			if info := tt.fields.networks[tt.args.network]; info != nil && info.table != 0 {
				matchFilter := func(r *netlink.Route) bool {
					return r != nil && r.Equal(netlink.Route{Protocol: unix.RTPROT_BGP, Table: info.table})
				}
				nlcall := nlmock.On("RouteListFiltered", netlink.FAMILY_ALL, mock.MatchedBy(matchFilter), netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
				if tt.routesErr {
					nlcall.Return(nil, errors.New("test error"))
				} else {
					nlcall.Return(tt.routes, nil)
				}
			}

			client, ctx, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{NBData: tt.initial}, nil)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			t.Cleanup(ctx.Cleanup)

			c := &controller{
				nbClient:   client,
				node:       node,
				log:        testr.New(t),
				networkIDs: tt.fields.networkIDs,
				networks:   tt.fields.networks,
				tables:     tt.fields.tables,
				netlink:    nlmock,
			}

			err = c.syncNetwork(tt.args.network)
			if tt.wantErr {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}

			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(client).To(libovsdb.HaveData(tt.expected...))
		})
	}
}

func Test_controller_syncRouteUpdate(t *testing.T) {
	defaultNetwork := &util.DefaultNetInfo{}
	type fields struct {
		networkIDs map[int]string
		networks   map[string]*netInfo
		tables     map[int]int
	}
	type args struct {
		update *netlink.RouteUpdate
	}
	tests := []struct {
		name     string
		fields   fields
		args     args
		expected []string
	}{
		{
			name: "ignores route updates with protocol != BGP",
			args: args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_STATIC}}},
		},
		{
			name: "ignores route updates for unknown tables",
			args: args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_UNSPEC}}},
		},
		{
			name:   "ignores route updates for unknown networks",
			fields: fields{tables: map[int]int{unix.RT_TABLE_MAIN: 0}},
			args:   args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_MAIN}}},
		},
		{
			name: "processes route updates",
			fields: fields{
				networkIDs: map[int]string{0: "default"},
				networks:   map[string]*netInfo{"default": {NetInfo: defaultNetwork, id: 0, table: unix.RT_TABLE_MAIN}},
				tables:     map[int]int{unix.RT_TABLE_MAIN: 0},
			},
			args:     args{&netlink.RouteUpdate{Route: netlink.Route{Protocol: unix.RTPROT_BGP, Table: unix.RT_TABLE_MAIN}}},
			expected: []string{"default"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			var reconciled []string
			var m sync.Mutex
			reconcile := func(key string) error {
				m.Lock()
				defer m.Unlock()
				reconciled = append(reconciled, key)
				return nil
			}
			matchReconcile := func(g gomega.Gomega, expected []string) {
				m.Lock()
				defer m.Unlock()
				g.Expect(reconciled).To(gomega.Equal(expected))
			}
			r := controllerutil.NewReconciler(
				"test",
				&controllerutil.ReconcilerConfig{Reconcile: reconcile, Threadiness: 1, RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[string](0, 0, 0)})
			c := &controller{
				log:        testr.New(t),
				networkIDs: tt.fields.networkIDs,
				networks:   tt.fields.networks,
				tables:     tt.fields.tables,
				reconciler: r,
			}
			err := controllerutil.Start(r)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c.syncRouteUpdate(tt.args.update)

			g.Eventually(matchReconcile).WithArguments(tt.expected).Should(gomega.Succeed())
			g.Consistently(matchReconcile).WithArguments(tt.expected).Should(gomega.Succeed())
		})
	}
}

func Test_controller_syncLinkUpdate(t *testing.T) {
	someNetwork := &util.DefaultNetInfo{}
	type fields struct {
		networkIDs map[int]string
		networks   map[string]*netInfo
		tables     map[int]int
	}
	type args struct {
		update *netlink.LinkUpdate
	}
	tests := []struct {
		name             string
		fields           fields
		args             args
		expectTables     map[int]int
		expectReconciles []string
	}{
		{
			name: "ignores link updates with type != VRF",
			args: args{&netlink.LinkUpdate{Link: &netlink.Dummy{}}},
		},
		{
			name: "ignores link updates with incorrect prefix",
			args: args{&netlink.LinkUpdate{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "something10" + types.UDNVRFDeviceSuffix}}}},
		},
		{
			name: "ignores link updates with incorrect suffix",
			args: args{&netlink.LinkUpdate{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "10-something"}}}},
		},
		{
			name: "ignores link updates with incorrect format",
			args: args{&netlink.LinkUpdate{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "something" + types.UDNVRFDeviceSuffix}}}},
		},
		{
			name: "ignores unknown event types",
			fields: fields{
				networkIDs: map[int]string{1: "net1"},
				networks:   map[string]*netInfo{"net1": {NetInfo: someNetwork, id: 1, table: 2}},
				tables:     map[int]int{2: 1},
			},
			args: args{&netlink.LinkUpdate{
				IfInfomsg: nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Type: unix.RTM_BASE}},
				Link:      &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 2}},
			},
			expectTables: map[int]int{2: 1},
		},
		{
			name: "ignores removal of old VRFs",
			fields: fields{
				networkIDs: map[int]string{1: "net1", 2: "net2"},
				networks:   map[string]*netInfo{"net1": {NetInfo: someNetwork, id: 1, table: 2}, "net2": {NetInfo: someNetwork, id: 2, table: 2}},
				tables:     map[int]int{2: 2},
			},
			args: args{&netlink.LinkUpdate{
				IfInfomsg: nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Type: unix.RTM_DELLINK}},
				Link:      &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 2}},
			},
			expectTables: map[int]int{2: 2},
		},
		{
			name: "processes link removals",
			fields: fields{
				networkIDs: map[int]string{1: "net1"},
				networks:   map[string]*netInfo{"net1": {NetInfo: someNetwork, id: 1, table: 2}},
				tables:     map[int]int{2: 1},
			},
			args: args{&netlink.LinkUpdate{
				IfInfomsg: nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Type: unix.RTM_DELLINK}},
				Link:      &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 2}},
			},
			expectTables: map[int]int{},
		},
		{
			name: "does not reconcile on link updates with no actual changes",
			fields: fields{
				networkIDs: map[int]string{1: "net1"},
				networks:   map[string]*netInfo{"net1": {NetInfo: someNetwork, id: 1, table: 2}},
				tables:     map[int]int{2: 1},
			},
			args: args{&netlink.LinkUpdate{
				IfInfomsg: nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Type: unix.RTM_NEWLINK}},
				Link:      &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 2}},
			},
			expectTables: map[int]int{2: 1},
		},
		{
			name: "does reconcile on link updates with actual changes",
			fields: fields{
				networkIDs: map[int]string{1: "net1"},
				networks:   map[string]*netInfo{"net1": {NetInfo: someNetwork, id: 1, table: 2}},
				tables:     map[int]int{2: 1},
			},
			args: args{&netlink.LinkUpdate{
				IfInfomsg: nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Type: unix.RTM_NEWLINK}},
				Link:      &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: types.UDNVRFDevicePrefix + "1" + types.UDNVRFDeviceSuffix}, Table: 3}},
			},
			expectTables:     map[int]int{3: 1},
			expectReconciles: []string{"net1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			var reconciled []string
			var m sync.Mutex
			reconcile := func(key string) error {
				m.Lock()
				defer m.Unlock()
				reconciled = append(reconciled, key)
				return nil
			}
			matchReconcile := func(g gomega.Gomega, expected []string) {
				m.Lock()
				defer m.Unlock()
				g.Expect(reconciled).To(gomega.Equal(expected))
			}
			r := controllerutil.NewReconciler(
				"test",
				&controllerutil.ReconcilerConfig{Reconcile: reconcile, Threadiness: 1, RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[string](0, 0, 0)})
			c := &controller{
				log:        testr.New(t),
				networkIDs: tt.fields.networkIDs,
				networks:   tt.fields.networks,
				tables:     tt.fields.tables,
				reconciler: r,
			}
			err := controllerutil.Start(r)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c.syncLinkUpdate(tt.args.update)

			g.Expect(c.tables).To(gomega.Equal(tt.expectTables))
			g.Eventually(matchReconcile).WithArguments(tt.expectReconciles).Should(gomega.Succeed())
			g.Consistently(matchReconcile).WithArguments(tt.expectReconciles).Should(gomega.Succeed())
		})
	}
}

func Test_controller_subscribe(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	var m sync.Mutex
	var routeEventCh chan<- netlink.RouteUpdate
	var linkEventCh chan<- netlink.LinkUpdate
	setRouteEventCh := func(ch chan<- netlink.RouteUpdate) {
		m.Lock()
		defer m.Unlock()
		routeEventCh = ch
	}
	setLinkEventCh := func(ch chan<- netlink.LinkUpdate) {
		m.Lock()
		defer m.Unlock()
		linkEventCh = ch
	}
	isRouteEventChSet := func(g gomega.Gomega) {
		m.Lock()
		defer m.Unlock()
		g.Expect(routeEventCh).ToNot(gomega.BeNil())
	}
	isLinkEventChSet := func(g gomega.Gomega) {
		m.Lock()
		defer m.Unlock()
		g.Expect(linkEventCh).ToNot(gomega.BeNil())
	}

	matchOptions := func(options any) bool {
		switch o := options.(type) {
		case netlink.RouteSubscribeOptions:
			return o.ListExisting == true
		case netlink.LinkSubscribeOptions:
			return o.ListExisting == true
		}
		return false
	}

	var stopArg <-chan struct{} = stop
	nlmock := &mocks.NetLinkOps{}
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setRouteEventCh(args.Get(0).(chan<- netlink.RouteUpdate)) }).
		Return(nil).Twice()
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Return(errors.New("test error")).Once()
	nlmock.On("RouteSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.RouteUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setRouteEventCh(args.Get(0).(chan<- netlink.RouteUpdate)) }).
		Return(nil).Once()

	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(nil).Twice()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Return(errors.New("test error")).Once()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(nil).Once()
	nlmock.On("LinkSubscribeWithOptions", mock.AnythingOfType("chan<- netlink.LinkUpdate"), stopArg, mock.MatchedBy(matchOptions)).
		Run(func(args mock.Arguments) { setLinkEventCh(args.Get(0).(chan<- netlink.LinkUpdate)) }).
		Return(errors.New("test error"))

	c := &controller{
		log:     testr.New(t),
		netlink: nlmock,
		tables:  map[int]int{1: 1},
	}

	c.subscribe(stop)

	g := gomega.NewWithT(t)

	g.Eventually(isRouteEventChSet).Should(gomega.Succeed())
	g.Eventually(isLinkEventChSet).Should(gomega.Succeed())

	rch := routeEventCh
	routeEventCh = nil
	close(rch)

	g.Eventually(isRouteEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	rch = routeEventCh
	routeEventCh = nil
	close(rch)

	g.Eventually(isRouteEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch := linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch = linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())

	lch = linkEventCh
	linkEventCh = nil
	close(lch)

	g.Eventually(isLinkEventChSet).WithTimeout(subscribePeriod * 3).Should(gomega.Succeed())
	g.Expect(c.tables).To(gomega.BeEmpty())
}
