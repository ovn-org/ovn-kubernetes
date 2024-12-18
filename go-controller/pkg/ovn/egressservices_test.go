package ovn

import (
	"context"
	"fmt"
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	egresssvc "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("OVN Egress Service Operations", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)
	const (
		node1Name        string = "node1"
		node1IPv4        string = "100.100.100.0"
		node1IPv6        string = "fc00:f853:ccd:e793::1"
		node1IPv4Subnet  string = "10.128.1.0/24"
		node1IPv6Subnet  string = "fe00:10:128:1::/64"
		node1transitIPv4 string = "100.88.0.2"
		node1transitIPv6 string = "fd97::2"
		node2Name        string = "node2"
		node2IPv4        string = "200.200.200.0"
		node2IPv6        string = "fc00:f853:ccd:e793::2"
		node2IPv4Subnet  string = "10.128.2.0/24"
		node2IPv6Subnet  string = "fe00:10:128:2::/64"
		node2transitIPv4 string = "100.88.0.3"
		node2transitIPv6 string = "fd97::3"
		vipIPv4          string = "192.168.126.10"
		vipIPv6          string = "fc00:f853:ccd:e793::10"
		controllerName          = DefaultNetworkControllerName
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		// disabling EgressIP to be sure we're creating the no reroute policies ourselves
		config.OVNKubernetesFeature.EnableEgressIP = false
		config.OVNKubernetesFeature.EnableEgressService = true
		_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
		_, cidr6, _ := net.ParseCIDR("fe00::/16")
		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr4, 24}, {cidr6, 64}}

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(true)
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.XContext("on startup repair", func() {
		ginkgo.It("should delete stale logical router policies and EgressService address set IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, "", "")
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, "", "")
				config.IPv6Mode = true

				nolongeregresssvc := v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "nolongeregresssvc", Namespace: "testns"},
				}

				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1Name,
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				esvc2 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node2Name,
					},
				}
				svc2 := lbSvcFor("testns", "svc2")

				esvc3 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc3",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/hostname": node2.Name,
							},
						},
					},
				}
				svc3 := lbSvcFor("testns", "svc3")

				esvc4 := egressserviceapi.EgressService{ // sourceIPBy=Network
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc4",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPNetwork,
					},
				}
				svc4 := lbSvcFor("testns", "svc4")

				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc2EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				svc3EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc3-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc3",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.50"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc4",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.27"},
							NodeName:  &node1.Name,
						},
					},
				}

				staleLRP1 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/gonesvc"}, // the service was deleted
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP1-UUID",
					Match:       "ip4.src == 10.10.10.10",
					Nexthops:    []string{"1.2.3.4"},
				}

				staleLRP2 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/nolongeregresssvc"}, // corresponding EgressService removed
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP2-UUID",
					Match:       "ip4.src == 10.10.10.10",
					Nexthops:    []string{"1.2.3.4"},
				}

				staleLRP3 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc1"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP3-UUID",
					Match:       "ip4.src == 10.128.1.100", // gone endpoint
					Nexthops:    []string{"10.128.1.2"},
				}

				staleLRP4 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc2"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP4-UUID",
					Match:       "ip4.src == 10.128.2.5",
					Nexthops:    []string{"10.128.1.2"}, // wrong nexthop
				}

				staleLRP5 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc3"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP5-UUID",
					Match:       "ip4.src == 10.128.2.50",
					Nexthops:    []string{"10.128.2.2"}, // node2 can't be used for svc3 which is not assigned to any node
				}

				staleLRP6 := &nbdb.LogicalRouterPolicy{ // belongs to an EgressService with srcIPBy=Network
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc4"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "staleLRP6-UUID",
					Match:       "ip4.src == 10.128.1.27",
					Nexthops:    []string{"10.128.1.2"},
				}

				toKeepLRP1 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc1"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "toKeepLRP1-UUID",
					Match:       "ip4.src == 10.128.1.5",
					Nexthops:    []string{"10.128.1.2"},
				}

				toKeepLRP2 := &nbdb.LogicalRouterPolicy{
					ExternalIDs: map[string]string{"EgressSVC": "testns/svc2"},
					Priority:    ovntypes.EgressSVCReroutePriority,
					UUID:        "toKeepLRP2-UUID",
					Match:       "ip4.src == 10.128.2.5",
					Nexthops:    []string{"10.128.2.2"},
				}

				clusterRouter := &nbdb.LogicalRouter{
					Name:     ovntypes.OVNClusterRouter,
					UUID:     ovntypes.OVNClusterRouter + "-UUID",
					Policies: []string{"staleLRP1-UUID", "staleLRP2-UUID", "staleLRP3-UUID", "staleLRP4-UUID", "toKeepLRP1-UUID", "toKeepLRP2-UUID"},
				}

				noRerouteLRPS := getDefaultNoReroutePolicies(controllerName)

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						staleLRP1,
						staleLRP2,
						staleLRP3,
						staleLRP4,
						staleLRP5,
						staleLRP6,
						toKeepLRP1,
						toKeepLRP2,
						clusterRouter,
					},
				}
				for _, lrp := range noRerouteLRPS {
					dbSetup.NBData = append(dbSetup.NBData, lrp)
				}

				// Create the global EgressService address set containing a stale IP: 1.2.3.4
				_, err := fakeOVN.asf.NewAddressSet(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName),
					[]string{"1.2.3.4", "10.128.1.5", "10.128.2.5"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							nolongeregresssvc,
							svc1,
							svc2,
							svc3,
							svc4,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
							svc2EpSlice,
							svc3EpSlice,
							svc4EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
							esvc2,
							esvc3,
							esvc4,
						},
					},
				)

				fakeOVN.InitAndRunEgressSVCController()
				clusterRouter.Policies = []string{"toKeepLRP1-UUID", "toKeepLRP2-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					toKeepLRP1,
					toKeepLRP2,
					clusterRouter,
				}
				for _, lrp := range noRerouteLRPS {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				// Verify that the global EgressService address set does not contain the stale IP
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), []string{"10.128.1.5", "10.128.2.5"})
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("OVN-IC: should delete stale logical router policies", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				config.IPv6Mode = true
				config.OVNKubernetesFeature.EnableInterconnect = true

				nolongeregresssvc := v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "nolongeregresssvc", Namespace: "testns"},
				}

				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1Name,
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				esvc2 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node2Name,
					},
				}
				svc2 := lbSvcFor("testns", "svc2")

				esvc3 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc3",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/hostname": node2.Name,
							},
						},
					},
				}

				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name, // Remote zone endpoint
						},
					},
				}

				svc2EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.6"},
							NodeName:  &node2.Name, // Remote zone endpoint
						},
						{
							Addresses: []string{"10.128.1.6"},
							NodeName:  &node1.Name,
						},
					},
				}

				// invalid next hop - Local node transit IP instead of mgmt IP
				staleLRP1 := egressServiceRouterPolicy("staleLRP1-UUID", "testns/svc1", "10.128.1.5", node1transitIPv4)
				// invalid next hop - Remote node mgmt IP instead of transit IP
				staleLRP2 := egressServiceRouterPolicy("staleLRP2-UUID", "testns/svc2", "10.128.1.6", "10.128.1.2")
				// remote endpoint configured in local zone
				staleLRP3 := egressServiceRouterPolicy("staleLRP3-UUID", "testns/svc1", "10.128.2.5", node1transitIPv4)
				// Local endpoint to local service
				toKeepLRP1 := egressServiceRouterPolicy("toKeepLRP1-UUID", "testns/svc1", "10.128.1.5", "10.128.1.2")
				// Local endpoint to remote service
				toKeepLRP2 := egressServiceRouterPolicy("toKeepLRP2-UUID", "testns/svc2", "10.128.1.6", node2transitIPv4)

				// Invalid IC policy
				staleICLRP1 := egressServiceRouterPolicy("staleLRSR1-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")
				// egress service doesn't exist
				staleICLRP2 := egressServiceRouterPolicy("staleLRSR2-UUID", "testns/svc9000:ic", "10.128.2.5", "10.128.1.2")
				// egress service not in local zone
				staleICLRP3 := egressServiceRouterPolicy("staleLRSR3-UUID", "testns/svc2:ic", "10.128.1.5", "10.128.2.2")
				// endpoint is not in the remote zone
				staleICLRP4 := egressServiceRouterPolicy("staleLRSR4-UUID", "testns/svc1:ic", "10.128.1.5", "10.128.2.2")
				// invalid next hop - Local node transit IP instead of mgmt IP
				staleICLRP5 := egressServiceRouterPolicy("staleLRSR5-UUID", "testns/svc1:ic", "10.128.2.5", node1transitIPv4)

				// Valid static route from remote endpoint to local service
				toKeepLRSR1 := egressServiceRouterPolicy("toKeepLRSR1-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")

				clusterRouter := &nbdb.LogicalRouter{
					Name: ovntypes.OVNClusterRouter,
					UUID: ovntypes.OVNClusterRouter + "-UUID",
					Policies: []string{"staleLRP1-UUID", "staleLRP2-UUID", "staleLRP3-UUID", "toKeepLRP1-UUID", "toKeepLRP2-UUID",
						"staleLRSR1-UUID", "staleLRSR2-UUID", "staleLRSR3-UUID", "staleLRSR4-UUID", "staleLRSR5-UUID", "toKeepLRSR1-UUID"},
				}

				noRerouteLRPS := getDefaultNoReroutePolicies(controllerName)

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						staleLRP1,
						staleLRP2,
						staleLRP3,
						staleICLRP1,
						staleICLRP2,
						staleICLRP3,
						staleICLRP4,
						staleICLRP5,
						toKeepLRP1,
						toKeepLRP2,
						toKeepLRSR1,
						clusterRouter,
					},
				}

				for _, lrp := range noRerouteLRPS {
					dbSetup.NBData = append(dbSetup.NBData, lrp)
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							nolongeregresssvc,
							svc1,
							svc2,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
							svc2EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
							esvc2,
							esvc3,
						},
					},
				)

				fakeOVN.controller.zone = node1Name
				fakeOVN.InitAndRunEgressSVCController()
				clusterRouter.Policies = []string{"toKeepLRP1-UUID", "toKeepLRP2-UUID", "toKeepLRSR1-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					toKeepLRP1,
					toKeepLRP2,
					toKeepLRSR1,
					clusterRouter,
				}
				for _, lrp := range append(noRerouteLRPS) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})

	ginkgo.Context("on egress service changes", func() {

		ginkgo.DescribeTable("should create/update/delete OVN configuration", func(interconnectEnabled bool) {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				config.OVNKubernetesFeature.EnableInterconnect = interconnectEnabled
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node1.Labels = map[string]string{"house": "Gryffindor"}
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				node2.Labels = map[string]string{"house": "Slytherin"}

				clusterRouter := &nbdb.LogicalRouter{
					Name: ovntypes.OVNClusterRouter,
					UUID: ovntypes.OVNClusterRouter + "-UUID",
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						clusterRouter,
					},
				}

				ginkgo.By("creating a service allocated to the first node with v4 and v6 endpoints")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"house": "Gryffindor",
							},
						},
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1Name,
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				v4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				v6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"fe00:10:128:2::5"},
							NodeName:  &node2.Name,
						},
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							v4EpSlice,
							v6EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				)

				if interconnectEnabled {
					fakeOVN.controller.zone = node1Name
				}
				fakeOVN.InitAndRunEgressSVCController()

				v4lrp1 := egressServiceRouterPolicy("v4lrp1-UUID", "testns/svc1", "10.128.1.5", "10.128.1.2")
				v4lrp2 := egressServiceRouterPolicy("v4lrp2-UUID", "testns/svc1", "10.128.2.5", "10.128.1.2")
				v6lrp1 := egressServiceRouterPolicy("v6lrp1-UUID", "testns/svc1", "fe00:10:128:1::5", "fe00:10:128:1::2")
				v6lrp2 := egressServiceRouterPolicy("v6lrp2-UUID", "testns/svc1", "fe00:10:128:2::5", "fe00:10:128:1::2")
				v4lrsr := egressServiceRouterPolicy("v4lrsr-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")
				v6lrsr := egressServiceRouterPolicy("v6lrsr-UUID", "testns/svc1:ic", "fe00:10:128:2::5", "fe00:10:128:1::2")

				expectedDatabaseState := []libovsdbtest.TestData{}
				expectedEgressSvcAddrSet := []string{}
				if !interconnectEnabled {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID", "v6lrp1-UUID", "v6lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						v4lrp1,
						v4lrp2,
						v6lrp1,
						v6lrp2,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "10.128.2.5", "fe00:10:128:1::5", "fe00:10:128:2::5"}
				} else {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v6lrp1-UUID", "v4lrsr-UUID", "v6lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						v4lrp1,
						v6lrp1,
						v4lrsr,
						v6lrsr,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "fe00:10:128:1::5"}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("updating the EgressService's status to the second node so its setup will be updated")
				esvc1.Status.Host = node2Name
				esvc1.ResourceVersion = "2"
				_, err := fakeOVN.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Update(context.TODO(), &esvc1, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				if !interconnectEnabled {
					v4lrp1.Nexthops[0] = "10.128.2.2"
					v4lrp2.Nexthops[0] = "10.128.2.2"
					v6lrp1.Nexthops[0] = "fe00:10:128:2::2"
					v6lrp2.Nexthops[0] = "fe00:10:128:2::2"

					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID", "v6lrp1-UUID", "v6lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						v4lrp1,
						v4lrp2,
						v6lrp1,
						v6lrp2,
					}
				} else {
					v4lrp1 = egressServiceRouterPolicy("v4lrp1-UUID", "testns/svc1", "10.128.1.5", node2transitIPv4)
					v6lrp1 = egressServiceRouterPolicy("v6lrp1-UUID", "testns/svc1", "fe00:10:128:1::5", node2transitIPv6)
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v6lrp1-UUID"}

					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						v4lrp1,
						v6lrp1,
					}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("removing the EgressService its lrps will be removed")
				err = fakeOVN.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Delete(context.TODO(), esvc1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				clusterRouter.Policies = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{clusterRouter}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), []string{})

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		},
			ginkgo.Entry("IC Disabled, all nodes are in a single zone", false),
			ginkgo.Entry("IC Enabled, node1 is in the local zone, node2 in remote", true),
		)

		ginkgo.DescribeTable("should delete resources when host changes to ALL", func(interconnectEnabled bool) {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				config.OVNKubernetesFeature.EnableInterconnect = interconnectEnabled
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				clusterRouter := &nbdb.LogicalRouter{
					Name: ovntypes.OVNClusterRouter,
					UUID: ovntypes.OVNClusterRouter + "-UUID",
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						clusterRouter,
					},
				}

				ginkgo.By("creating a service with v4 and v6 endpoints allocated on the first node")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{Host: node1Name},
				}
				svc1 := lbSvcFor("testns", "svc1")

				v4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				v6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"fe00:10:128:2::5"},
							NodeName:  &node2.Name,
						},
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							v4EpSlice,
							v6EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				)

				if interconnectEnabled {
					fakeOVN.controller.zone = node1Name
				}
				fakeOVN.InitAndRunEgressSVCController()

				v4lrp1 := egressServiceRouterPolicy("v4lrp1-UUID", "testns/svc1", "10.128.1.5", "10.128.1.2")
				v4lrp2 := egressServiceRouterPolicy("v4lrp2-UUID", "testns/svc1", "10.128.2.5", "10.128.1.2")
				v6lrp1 := egressServiceRouterPolicy("v6lrp1-UUID", "testns/svc1", "fe00:10:128:1::5", "fe00:10:128:1::2")
				v6lrp2 := egressServiceRouterPolicy("v6lrp2-UUID", "testns/svc1", "fe00:10:128:2::5", "fe00:10:128:1::2")
				v4lrpic := egressServiceRouterPolicy("v4lrsr-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")
				v6lrpic := egressServiceRouterPolicy("v6lrsr-UUID", "testns/svc1:ic", "fe00:10:128:2::5", "fe00:10:128:1::2")

				clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID", "v6lrp1-UUID", "v6lrp2-UUID"}
				expectedDatabaseState := []libovsdbtest.TestData{
					clusterRouter,
					v4lrp1,
					v4lrp2,
					v6lrp1,
					v6lrp2,
				}
				expectedEgressSvcAddrSet := []string{"10.128.1.5", "10.128.2.5", "fe00:10:128:1::5", "fe00:10:128:2::5"}

				if interconnectEnabled {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v6lrp1-UUID", "v4lrsr-UUID", "v6lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						v4lrp1,
						v6lrp1,
						v4lrpic,
						v6lrpic,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "fe00:10:128:1::5"}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("setting the EgressService's host to ALL the resources should be removed")
				esvc1.Status.Host = "ALL"
				esvc1.ResourceVersion = "2"
				_, err := fakeOVN.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Update(context.TODO(), &esvc1, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				clusterRouter.Policies = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{clusterRouter}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), []string{})
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		},
			ginkgo.Entry("IC Disabled, all nodes are in a single zone", false),
			ginkgo.Entry("IC Enabled, node1 is in the local zone, node2 in remote", true))
	})

	ginkgo.Context("on endpointslices changes", func() {
		ginkgo.DescribeTable("should create/update/delete OVN configuration", func(interconnectEnabled bool) {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				config.OVNKubernetesFeature.EnableInterconnect = interconnectEnabled
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)

				clusterRouter := &nbdb.LogicalRouter{
					Name: ovntypes.OVNClusterRouter,
					UUID: ovntypes.OVNClusterRouter + "-UUID",
				}
				gwRouter := &nbdb.LogicalRouter{
					Name:  ovntypes.GWRouterPrefix + node1.Name,
					UUID:  ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID"},
				}
				node1LRP := &nbdb.LogicalRouterPort{
					UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name + "-UUID",
					Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + node1.Name,
					Networks: []string{nodeLogicalRouterIfAddrV4, nodeLogicalRouterIfAddrV6},
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
					},
				}

				ginkgo.By("creating a service with v4 endpoints that will be allocated on the node")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1.Name,
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				v4EpSlice := &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				v6EpSlice := &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::5"},
							NodeName:  &node1.Name,
						},
						{
							Addresses: []string{"fe00:10:128:2::5"},
							NodeName:  &node2.Name,
						},
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1, *node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							*v4EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				)

				if interconnectEnabled {
					fakeOVN.controller.zone = node1Name
				}
				fakeOVN.InitAndRunEgressSVCController()

				v4lrp1 := egressServiceRouterPolicy("v4lrp1-UUID", "testns/svc1", "10.128.1.5", "10.128.1.2")
				v4lrp2 := egressServiceRouterPolicy("v4lrp2-UUID", "testns/svc1", "10.128.2.5", "10.128.1.2")
				v4lrpic := egressServiceRouterPolicy("v4lrsr-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")
				var expectedDatabaseState []libovsdbtest.TestData
				var expectedEgressSvcAddrSet []string
				v4DefaultReRoute := getReRouteStaticRoute("10.128.0.0/16", nodeLogicalRouterIPv4[0])
				v6DefaultReRoute := getReRouteStaticRoute("fe00::/16", nodeLogicalRouterIPv6[0])
				v6DefaultReRoute.UUID = "reroute-static-route-UUID-v6"
				if !interconnectEnabled {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrp2,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "10.128.2.5"}
				} else {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrpic,
						v4DefaultReRoute,
						v6DefaultReRoute,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5"}
					expectedDatabaseState[0].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID", "reroute-static-route-UUID-v6"}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("creating the v6 endpoints corresponding lrps will be added")
				v6EpSlice, err := fakeOVN.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Create(context.TODO(), v6EpSlice, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				v6lrp1 := egressServiceRouterPolicy("v6lrp1-UUID", "testns/svc1", "fe00:10:128:1::5", "fe00:10:128:1::2")
				v6lrp2 := egressServiceRouterPolicy("v6lrp2-UUID", "testns/svc1", "fe00:10:128:2::5", "fe00:10:128:1::2")
				v6lrpic := egressServiceRouterPolicy("v6lrsr-UUID", "testns/svc1:ic", "fe00:10:128:2::5", "fe00:10:128:1::2")

				if !interconnectEnabled {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID", "v6lrp1-UUID", "v6lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrp2,
						v6lrp1,
						v6lrp2,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "10.128.2.5", "fe00:10:128:1::5", "fe00:10:128:2::5"}
				} else {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v6lrp1-UUID", "v4lrsr-UUID", "v6lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrpic,
						v6lrp1,
						v6lrpic,
						v4DefaultReRoute,
						v6DefaultReRoute,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "fe00:10:128:1::5"}
					expectedDatabaseState[0].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID", "reroute-static-route-UUID-v6"}
				}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("updating the v4 and v6 endpoints OVN will be updated accordingly")
				v4EpSlice.Endpoints = append(v4EpSlice.Endpoints, discovery.Endpoint{Addresses: []string{"10.128.1.7"}, NodeName: &node1.Name})
				v4EpSlice.ResourceVersion = "2"
				v4EpSlice, err = fakeOVN.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Update(context.TODO(), v4EpSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				v6EpSlice.Endpoints = []discovery.Endpoint{{Addresses: []string{"fe00:10:128:2::5"}, NodeName: &node2.Name}}
				v6EpSlice.ResourceVersion = "2"
				v6EpSlice, err = fakeOVN.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Update(context.TODO(), v6EpSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				v4lrp3 := egressServiceRouterPolicy("v4lrp3-UUID", "testns/svc1", "10.128.1.7", "10.128.1.2") // local to zone
				if !interconnectEnabled {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp2-UUID", "v4lrp3-UUID", "v6lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrp2,
						v4lrp3,
						v6lrp2,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "10.128.2.5", "10.128.1.7", "fe00:10:128:2::5"}
				} else {
					clusterRouter.Policies = []string{"v4lrp1-UUID", "v4lrp3-UUID", "v4lrsr-UUID", "v6lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v4lrp1,
						v4lrpic,
						v4lrp3,
						v6lrpic,
						v4DefaultReRoute,
						v6DefaultReRoute,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.5", "10.128.1.7"}
					expectedDatabaseState[0].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID", "reroute-static-route-UUID-v6"}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("deleting the v4 endpoints the corresponding entries will be deleted")
				err = fakeOVN.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Delete(context.TODO(), v4EpSlice.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				if !interconnectEnabled {
					clusterRouter.Policies = []string{"v6lrp2-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v6lrp2,
					}
					expectedEgressSvcAddrSet = []string{"fe00:10:128:2::5"}
				} else {
					clusterRouter.Policies = []string{"v6lrsr-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						gwRouter,
						node1LRP,
						v6lrpic,
						v4DefaultReRoute,
						v6DefaultReRoute,
					}
					expectedEgressSvcAddrSet = []string{}
					expectedDatabaseState[0].(*nbdb.LogicalRouter).StaticRoutes = []string{"reroute-static-route-UUID", "reroute-static-route-UUID-v6"}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("deleting the v6 endpoints the corresponding lrps will be deleted")
				err = fakeOVN.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Delete(context.TODO(), v6EpSlice.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				clusterRouter.Policies = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{
					clusterRouter,
					gwRouter,
					node1LRP,
				}
				if interconnectEnabled {
					expectedDatabaseState = append(expectedDatabaseState, v4DefaultReRoute)
					expectedDatabaseState = append(expectedDatabaseState, v6DefaultReRoute)
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		},
			ginkgo.Entry("IC Disabled, all nodes are in a single zone", false),
			ginkgo.Entry("IC Enabled, node1 is in the local zone, node2 in remote", true))
	})

	ginkgo.Context("on nodes changes", func() {
		ginkgo.DescribeTable("should create/update/delete logical router policies and address sets", func(interconnectEnabled bool) {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				config.OVNKubernetesFeature.EnableInterconnect = interconnectEnabled
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet, node1transitIPv4, node1transitIPv6)
				node1.Labels = map[string]string{"home": "pineapple"}
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet, node2transitIPv4, node2transitIPv6)
				node2.Labels = map[string]string{"home": "rock"}

				clusterRouter := &nbdb.LogicalRouter{
					Name: ovntypes.OVNClusterRouter,
					UUID: ovntypes.OVNClusterRouter + "-UUID",
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						clusterRouter,
					},
				}

				ginkgo.By("creating an egress service assigned to the first node")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"home": "pineapple",
							},
						},
					},
					Status: egressserviceapi.EgressServiceStatus{Host: node1Name},
				}
				svc1 := lbSvcFor("testns", "svc1")

				svc1V4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				svc1V6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:2::5"},
							NodeName:  &node2.Name,
						},
					},
				}

				svc2 := lbSvcFor("testns", "svc2")

				svc2V4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.6"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc2V6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::6"},
							NodeName:  &node1.Name,
						},
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
							svc2,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1V4EpSlice,
							svc1V6EpSlice,
							svc2V4EpSlice,
							svc2V6EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				)

				if interconnectEnabled {
					fakeOVN.controller.zone = node1Name
				}
				fakeOVN.InitAndRunEgressSVCController()

				svc1v4lrp1 := egressServiceRouterPolicy("svc1v4lrp1-UUID", "testns/svc1", "10.128.2.5", "10.128.1.2")
				svc1v6lrp1 := egressServiceRouterPolicy("svc1v6lrp1-UUID", "testns/svc1", "fe00:10:128:2::5", "fe00:10:128:1::2")
				svc1v4iclrp1 := egressServiceRouterPolicy("svc1v4lrsr1-UUID", "testns/svc1:ic", "10.128.2.5", "10.128.1.2")
				svc1v6iclrp1 := egressServiceRouterPolicy("svc1v6lrsr1-UUID", "testns/svc1:ic", "fe00:10:128:2::5", "fe00:10:128:1::2")

				expectedDatabaseState := []libovsdbtest.TestData{}
				expectedEgressSvcAddrSet := []string{}
				if !interconnectEnabled {
					clusterRouter.Policies = []string{"svc1v4lrp1-UUID", "svc1v6lrp1-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc1v4lrp1,
						svc1v6lrp1,
					}
					expectedEgressSvcAddrSet = []string{"10.128.2.5", "fe00:10:128:2::5"}
				} else {
					clusterRouter.Policies = []string{"svc1v4lrsr1-UUID", "svc1v6lrsr1-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc1v4iclrp1,
						svc1v6iclrp1,
					}
				}

				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("creating an additional egress service assigned to the second node")
				esvc2 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						SourceIPBy: egressserviceapi.SourceIPLoadBalancer,
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"home": "moai",
							},
						},
					},
					Status: egressserviceapi.EgressServiceStatus{Host: node2Name},
				}
				_, err := fakeOVN.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Create(context.TODO(), &esvc2, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				svc2v4lrp1 := egressServiceRouterPolicy("svc2v4lrp1-UUID", "testns/svc2", "10.128.1.6", "10.128.2.2")
				svc2v6lrp1 := egressServiceRouterPolicy("svc2v6lrp1-UUID", "testns/svc2", "fe00:10:128:1::6", "fe00:10:128:2::2")
				svc2v4lrp1remote := egressServiceRouterPolicy("svc2v4lrp1remote-UUID", "testns/svc2", "10.128.1.6", node2transitIPv4)
				svc2v6lrp1remote := egressServiceRouterPolicy("svc2v6lrp1remote-UUID", "testns/svc2", "fe00:10:128:1::6", node2transitIPv6)

				if !interconnectEnabled {
					clusterRouter.Policies = []string{"svc1v4lrp1-UUID", "svc1v6lrp1-UUID", "svc2v4lrp1-UUID", "svc2v6lrp1-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc1v4lrp1,
						svc1v6lrp1,
						svc2v4lrp1,
						svc2v6lrp1,
					}
					expectedEgressSvcAddrSet = []string{"10.128.2.5", "fe00:10:128:2::5", "10.128.1.6", "fe00:10:128:1::6"}
				} else {
					clusterRouter.Policies = []string{"svc2v4lrp1remote-UUID", "svc2v6lrp1remote-UUID", "svc1v4lrsr1-UUID", "svc1v6lrsr1-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc1v4iclrp1,
						svc1v6iclrp1,
						svc2v4lrp1remote,
						svc2v6lrp1remote,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.6", "fe00:10:128:1::6"}
				}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("updating the second node host cidr the node ip no re-route address set will be updated")
				nodeIPsASdbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, ovntypes.DefaultNetworkName, DefaultNetworkControllerName)
				fakeOVN.asf.EventuallyExpectAddressSetWithAddresses(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6})

				node2.ObjectMeta.Annotations[util.OVNNodeHostCIDRs] = fmt.Sprintf("[\"%s\", \"%s\", \"%s\", \"%s\"]", node2IPv4+"/24", node2IPv6+"/64", vipIPv4+"/24", vipIPv6+"/64")
				node2.ResourceVersion = "3"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.asf.EventuallyExpectAddressSetWithAddresses(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6, vipIPv4, vipIPv6})
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				node2.ObjectMeta.Annotations[util.OVNNodeHostCIDRs] = fmt.Sprintf("[\"%s\", \"%s\"]", node2IPv4+"/24", node2IPv6+"/64")
				node2.ResourceVersion = "4"
				_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.asf.EventuallyExpectAddressSetWithAddresses(nodeIPsASdbIDs, []string{node1IPv4, node2IPv4, node1IPv6, node2IPv6})
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				ginkgo.By("deleting the first node, the node ip no re-route address set will be updated and service resources will be deleted")
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				fakeOVN.asf.EventuallyExpectAddressSetWithAddresses(nodeIPsASdbIDs, []string{node2IPv4, node2IPv6})

				if !interconnectEnabled {
					clusterRouter.Policies = []string{"svc2v4lrp1-UUID", "svc2v6lrp1-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc2v4lrp1,
						svc2v6lrp1,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.6", "fe00:10:128:1::6"}
				} else {
					clusterRouter.Policies = []string{"svc2v4lrp1remote-UUID", "svc2v6lrp1remote-UUID"}
					expectedDatabaseState = []libovsdbtest.TestData{
						clusterRouter,
						svc2v4lrp1remote,
						svc2v6lrp1remote,
					}
					expectedEgressSvcAddrSet = []string{"10.128.1.6", "fe00:10:128:1::6"}
				}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), expectedEgressSvcAddrSet)

				ginkgo.By("deleting the second node the second service's resources will be deleted")
				err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				clusterRouter.Policies = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{
					clusterRouter,
				}
				for _, lrp := range getDefaultNoReroutePolicies(controllerName) {
					expectedDatabaseState = append(expectedDatabaseState, lrp)
					clusterRouter.Policies = append(clusterRouter.Policies, lrp.UUID)
				}
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				fakeOVN.asf.EventuallyExpectEmptyAddressSetExist(nodeIPsASdbIDs)
				fakeOVN.asf.ExpectAddressSetWithAddresses(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName), []string{})
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		},
			ginkgo.Entry("IC Disabled, all nodes are in a single zone", false),
			ginkgo.Entry("IC Enabled, node1 is in the local zone, node2 in remote", true))
	})

})

func (o *FakeOVN) InitAndRunEgressSVCController(tweak ...func(*DefaultNetworkController)) {
	c, err := o.controller.InitEgressServiceZoneController()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	o.controller.egressSvcController = c
	for _, t := range tweak {
		t(o.controller)
	}
	err = o.controller.egressSvcController.Run(o.egressSVCWg, 1)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
}

// creates a logical router policy for egress service
func egressServiceRouterPolicy(uuid, key, addr, nexthop string) *nbdb.LogicalRouterPolicy {
	match := fmt.Sprintf("ip4.src == %s", addr)
	if utilnet.IsIPv6String(addr) {
		match = fmt.Sprintf("ip6.src == %s", addr)
	}
	return &nbdb.LogicalRouterPolicy{
		UUID:        uuid,
		Action:      nbdb.LogicalRouterPolicyActionReroute,
		ExternalIDs: map[string]string{"EgressSVC": key},
		Match:       match,
		Nexthops:    []string{nexthop},
		Priority:    ovntypes.EgressSVCReroutePriority,
	}
}

func getDefaultNoReroutePolicies(controllerName string) []*nbdb.LogicalRouterPolicy {
	allLRPS := []*nbdb.LogicalRouterPolicy{}
	egressSvcPodsV4, egressSvcPodsV6 := addressset.GetHashNamesForAS(egresssvc.GetEgressServiceAddrSetDbIDs(controllerName))
	egressipPodsV4, egressipPodsV6 := addressset.GetHashNamesForAS(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ovntypes.DefaultNetworkName, controllerName))
	nodeIPsV4, nodeIPsV6 := addressset.GetHashNamesForAS(getEgressIPAddrSetDbIDs(NodeIPAddrSetName, ovntypes.DefaultNetworkName, controllerName))
	v4ExtIDs := getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()
	v6ExtIDs := getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()

	allLRPS = append(allLRPS,
		&nbdb.LogicalRouterPolicy{
			Priority: ovntypes.DefaultNoRereoutePriority,
			Match: fmt.Sprintf("(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s",
				egressipPodsV4, egressSvcPodsV4, nodeIPsV4),
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			UUID:        "default-no-reroute-node-UUID",
			ExternalIDs: v4ExtIDs,
			Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
		},
		&nbdb.LogicalRouterPolicy{
			Priority: ovntypes.DefaultNoRereoutePriority,
			Match: fmt.Sprintf("(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s",
				egressipPodsV6, egressSvcPodsV6, nodeIPsV6),
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			UUID:        "default-v6-no-reroute-node-UUID",
			ExternalIDs: v6ExtIDs,
			Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
		},
		getNoReRouteReplyTrafficPolicy(ovntypes.DefaultNetworkName, controllerName),
	)

	v4Pod2PodExtIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()
	v6Pod2PodExtIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()
	v4Pod2JoinExtIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()
	v6Pod2JoinExtIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, ovntypes.DefaultNetworkName, controllerName).GetExternalIDs()

	allLRPS = append(allLRPS,
		&nbdb.LogicalRouterPolicy{
			Priority:    ovntypes.DefaultNoRereoutePriority,
			Match:       "ip4.src == 10.128.0.0/16 && ip4.dst == 10.128.0.0/16",
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			ExternalIDs: v4Pod2PodExtIDs,
			UUID:        "default-pod2pod-no-reroute-UUID",
		},
		&nbdb.LogicalRouterPolicy{
			Priority:    ovntypes.DefaultNoRereoutePriority,
			Match:       fmt.Sprintf("ip4.src == 10.128.0.0/16 && ip4.dst == %s", config.Gateway.V4JoinSubnet),
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			ExternalIDs: v4Pod2JoinExtIDs,
			UUID:        "no-reroute-service-UUID",
		},
		&nbdb.LogicalRouterPolicy{
			Priority:    ovntypes.DefaultNoRereoutePriority,
			Match:       "ip6.src == fe00::/16 && ip6.dst == fe00::/16",
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			ExternalIDs: v6Pod2PodExtIDs,
			UUID:        "default-v6-pod2pod-no-reroute-UUID",
		},
		&nbdb.LogicalRouterPolicy{
			Priority:    ovntypes.DefaultNoRereoutePriority,
			Match:       fmt.Sprintf("ip6.src == fe00::/16 && ip6.dst == %s", config.Gateway.V6JoinSubnet),
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			ExternalIDs: v6Pod2JoinExtIDs,
			UUID:        "no-reroute-v6-service-UUID",
		},
	)

	return allLRPS
}

func lbSvcFor(namespace, name string) v1.Service {
	return v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: "1",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{
						IP: "1.1.1.1", // arbitrary ip, we don't care about it for the lrps as long as it's there
					},
				},
			},
		},
	}
}

func nodeFor(name, ipv4, ipv6, v4subnet, v6subnet, transitIPv4, transitIPv6 string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", ipv4, ipv6),
				util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\",\"%s\"]", fmt.Sprintf("%s/24", ipv4), fmt.Sprintf("%s/64", ipv6)),
				"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\",\"%s\"]}", v4subnet, v6subnet),

				// Used only with IC tests
				"k8s.ovn.org/zone-name":                       name,
				"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf("{\"ipv4\":\"%s/16\", \"ipv6\":\"%s/64\"}", transitIPv4, transitIPv6),
			},
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
			ResourceVersion: "1",
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: ipv4,
				},
				{
					Type:    v1.NodeInternalIP,
					Address: ipv6,
				},
			},
		},
	}
}
