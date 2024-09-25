package topology

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("Topology factory", func() {
	const (
		coopUUID       = "somestring"
		networkName    = "angrytenant"
		networkSubnets = "10.200.0.0/16,fd14:1234::0/60"
		routerName     = "mydearrouter"
	)
	var (
		factory  *GatewayTopologyFactory
		netInfo  util.NetInfo
		nbClient libovsdbclient.Client
		ovnDB    *libovsdbtest.Context
	)

	When("the original OVN DBs are empty", func() {
		BeforeEach(func() {
			// required so that NewNetInfo can properly determine the IP families the cluster supports
			config.IPv4Mode = true
			config.IPv6Mode = true
			initialNBDB := []libovsdbtest.TestData{}
			initialSBDB := []libovsdbtest.TestData{}
			dbSetup := libovsdbtest.TestSetup{
				NBData: initialNBDB,
				SBData: initialSBDB,
			}
			libovsdbNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())
			ovnDB = libovsdbCleanup
			nbClient = libovsdbNBClient

			netInfo, err = util.NewNetInfo(newLayer3PrimaryNetworkConf(networkName, networkSubnets))
			Expect(err).NotTo(HaveOccurred())

			factory = NewGatewayTopologyFactory(libovsdbNBClient)
		})

		AfterEach(func() {
			if ovnDB != nil {
				ovnDB.Cleanup()
			}
		})

		It("creating a cluster router for a layer3 primary network with multicast disabled", func() {
			clusterRouter, err := factory.NewClusterRouter(routerName, netInfo, coopUUID)
			Expect(err).NotTo(HaveOccurred())
			expectedExternalIDs := map[string]string{
				ovntypes.NetworkExternalID:  networkName,
				ovntypes.TopologyExternalID: ovntypes.Layer3Topology,
				"k8s-cluster-router":        "yes",
			}
			expectedOptions := map[string]string{"always_learn_from_arp_request": "false"}
			Expect(clusterRouter).To(
				WithTransform(
					removeUUID,
					Equal(
						expectedClusterRouter(routerName, coopUUID, expectedOptions, expectedExternalIDs),
					),
				),
			)
		})

		It("creating a cluster router for a layer3 primary network with multicast enabled", func() {
			clusterRouter, err := factory.NewClusterRouterWithMulticastSupport(routerName, netInfo, coopUUID)
			Expect(err).NotTo(HaveOccurred())
			expectedExternalIDs := map[string]string{
				ovntypes.NetworkExternalID:  networkName,
				ovntypes.TopologyExternalID: ovntypes.Layer3Topology,
				"k8s-cluster-router":        "yes",
			}
			expectedOptions := map[string]string{"mcast_relay": "true"}
			Expect(clusterRouter).To(
				WithTransform(
					removeUUID,
					Equal(
						expectedClusterRouter(routerName, coopUUID, expectedOptions, expectedExternalIDs),
					),
				),
			)
		})

		It("creating a join switch for a layer3 primary network fails", func() {
			clusterRouter := expectedClusterRouter(
				routerName,
				coopUUID,
				map[string]string{},
				map[string]string{},
			)
			clusterRouter.UUID = "pepe"
			Expect(factory.NewJoinSwitch(clusterRouter, netInfo, dummyIPs())).To(
				And(
					MatchError(HavePrefix("failed to add logical router port")),
					MatchError(HaveSuffix("on router mydearrouter: object not found")),
				),
			)
		})
	})

	When("a cluster router object is present on the OVN northbound DB", func() {
		var initialClusterRouter *nbdb.LogicalRouter

		BeforeEach(func() {
			initialClusterRouter = expectedClusterRouter(
				routerName,
				coopUUID,
				map[string]string{"asd": "abc"},
				map[string]string{"123": "456"},
			)
			initialClusterRouter.UUID = routerName

			initialNBDB := []libovsdbtest.TestData{initialClusterRouter}
			initialSBDB := []libovsdbtest.TestData{}
			dbSetup := libovsdbtest.TestSetup{
				NBData: initialNBDB,
				SBData: initialSBDB,
			}
			libovsdbNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
			Expect(err).NotTo(HaveOccurred())
			ovnDB = libovsdbCleanup
			nbClient = libovsdbNBClient

			netInfo, err = util.NewNetInfo(newLayer3PrimaryNetworkConf(networkName, networkSubnets))
			Expect(err).NotTo(HaveOccurred())

			factory = NewGatewayTopologyFactory(libovsdbNBClient)
		})

		AfterEach(func() {
			if ovnDB != nil {
				ovnDB.Cleanup()
			}
		})

		It("creating a join switch for a layer3 primary network", func() {
			gwRoutersIPs := dummyIPs()
			Expect(factory.NewJoinSwitch(initialClusterRouter, netInfo, gwRoutersIPs)).To(Succeed())

			const clusterRouterPortName = "rtoj-mydearrouter"
			expectedDatabaseState := []libovsdbtest.TestData{
				expectedLogicalSwitch("angrytenant_join", networkName, "jtor-mydearrouter"),
				expectedLogicalSwitchPort("jtor-mydearrouter"),
				mutatedClusterRouter(initialClusterRouter, clusterRouterPortName),
				expectedLogicalRouterPort(
					clusterRouterPortName,
					"0a:58:c0:a8:c8:0a",
					nil,
					map[string]string{
						ovntypes.NetworkExternalID:  "angrytenant",
						ovntypes.TopologyExternalID: "layer3",
					},
					ips(gwRoutersIPs...)...,
				),
			}
			Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
		})
	})
})

func newLayer3PrimaryNetworkConf(name, subnets string) *ovncnitypes.NetConf {
	return &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: name},
		Topology: ovntypes.Layer3Topology,
		Subnets:  subnets,
		Role:     ovntypes.NetworkRolePrimary,
	}
}

func removeUUID(lr *nbdb.LogicalRouter) *nbdb.LogicalRouter {
	lr.UUID = ""
	return lr
}

func expectedClusterRouter(
	routerName, coopUUID string,
	options map[string]string,
	externalIDs map[string]string,
	ports ...string,
) *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		Copp:              &coopUUID,
		ExternalIDs:       externalIDs,
		LoadBalancer:      nil,
		LoadBalancerGroup: nil,
		Name:              routerName,
		Nat:               nil,
		Options:           options,
		Policies:          nil,
		Ports:             ports,
		StaticRoutes:      nil,
	}
}

func dummyIPs() []*net.IPNet {
	var ips []*net.IPNet

	for _, cidr := range []string{"192.168.200.10", "192.168.200.20", "192.168.200.30"} {
		ips = append(ips, &net.IPNet{IP: net.ParseIP(cidr), Mask: net.CIDRMask(24, 32)})
	}

	return ips
}

func expectedLogicalSwitch(switchName, networkName string, ports ...string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID: switchName,
		ExternalIDs: map[string]string{
			ovntypes.NetworkExternalID:  networkName,
			ovntypes.TopologyExternalID: ovntypes.Layer3Topology,
		},
		Name:  switchName,
		Ports: ports,
	}
}

func expectedLogicalSwitchPort(portName string) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		UUID:      portName,
		Addresses: []string{"router"},
		Name:      portName,
		Options: map[string]string{
			"router-port": "rtoj-mydearrouter",
		},
		ParentName:   nil,
		PortSecurity: nil,
		Tag:          nil,
		TagRequest:   nil,
		Type:         "router",
		Up:           nil,
	}
}

func mutatedClusterRouter(clusterRouter *nbdb.LogicalRouter, ports ...string) *nbdb.LogicalRouter {
	updatedClusterRouter := clusterRouter.DeepCopy()
	updatedClusterRouter.Copp = nil // for whatever reason, the COPP is cleared when we attach the ports. I suspect libovsdb doesn't know about this parameter.
	updatedClusterRouter.Ports = ports
	return updatedClusterRouter
}

func expectedLogicalRouterPort(portName, mac string, options, externalIDs map[string]string, ips ...string) *nbdb.LogicalRouterPort {
	return &nbdb.LogicalRouterPort{
		UUID:        portName,
		ExternalIDs: externalIDs,
		MAC:         mac,
		Name:        portName,
		Networks:    ips,
		Options:     options,
	}
}

func ips(inputIP ...*net.IPNet) []string {
	var ipsStringFormat []string
	for _, ip := range inputIP {
		ipsStringFormat = append(ipsStringFormat, ip.String())
	}
	return ipsStringFormat
}
