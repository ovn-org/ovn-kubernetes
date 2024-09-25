package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	dnsnameresolver "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	t "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"
)

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
	}
}

func newEgressFirewallObject(name, namespace string, egressRules []egressfirewallapi.EgressFirewallRule) *egressfirewallapi.EgressFirewall {
	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: egressRules,
		},
	}
}

func newDNSNameResolverObject(name, namespace, dnsName string, ip string) *ocpnetworkapiv1alpha1.DNSNameResolver {
	return &ocpnetworkapiv1alpha1.DNSNameResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
			Name: ocpnetworkapiv1alpha1.DNSName(dnsName),
		},
		Status: ocpnetworkapiv1alpha1.DNSNameResolverStatus{
			ResolvedNames: []ocpnetworkapiv1alpha1.DNSNameResolverResolvedName{
				{
					DNSName: ocpnetworkapiv1alpha1.DNSName(dnsName),
					ResolvedAddresses: []ocpnetworkapiv1alpha1.DNSNameResolverResolvedAddress{
						{
							IP: ip,
						},
					},
				},
			},
		},
	}
}

func getEFExpectedDb(initialData []libovsdbtest.TestData, fakeOVN *FakeOVN, nsName string, dstMatch, portMatch string,
	action nbdb.ACLAction) []libovsdbtest.TestData {
	pgName := fakeOVN.controller.getNamespacePortGroupName(nsName)
	dbIDs := fakeOVN.controller.getEgressFirewallACLDbIDs(nsName, 0)
	match := dstMatch + " && inport == @" + pgName
	if portMatch != "" {
		match += " && " + portMatch
	}
	acl := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(dbIDs),
		nbdb.ACLDirectionToLport,
		t.EgressFirewallStartPriority,
		match,
		action,
		t.OvnACLLoggingMeter,
		"",
		false,
		dbIDs.GetExternalIDs(),
		nil,
		t.DefaultACLTier,
	)
	acl.UUID = "acl-UUID"

	// new ACL will be added to the port group
	pgIDs := getNamespacePortGroupDbIDs(nsName, DefaultNetworkControllerName)
	namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{acl})
	namespacePortGroup.UUID = pgName + "-UUID"
	return append(initialData, acl, namespacePortGroup)
}

func getEFExpectedDbAfterDelete(prevExpectedData []libovsdbtest.TestData) []libovsdbtest.TestData {
	pg := prevExpectedData[len(prevExpectedData)-1].(*nbdb.PortGroup)
	pg.ACLs = nil
	return append(prevExpectedData[:len(prevExpectedData)-2], pg)
}

var _ = ginkgo.Describe("OVN EgressFirewall Operations", func() {
	var (
		app                    *cli.App
		fakeOVN                *FakeOVN
		clusterPortGroup       *nbdb.PortGroup
		nodeSwitch, joinSwitch *nbdb.LogicalSwitch
		initialData            []libovsdbtest.TestData
		dbSetup                libovsdbtest.TestSetup
		mockDnsOps             *util_mocks.DNSOps
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	clusterRouter := &nbdb.LogicalRouter{
		UUID: t.OVNClusterRouter + "-UUID",
		Name: t.OVNClusterRouter,
	}

	setMockDnsOps := func() {
		mockDnsOps = new(util_mocks.DNSOps)
		util.SetDNSLibOpsMockInst(mockDnsOps)
	}

	setDNSMockServer := func() {
		mockClientConfigFromFile := ovntest.TestifyMockHelper{
			OnCallMethodName:    "ClientConfigFromFile",
			OnCallMethodArgType: []string{"string"},
			OnCallMethodArgs:    []interface{}{},
			RetArgList: []interface{}{&dns.ClientConfig{
				Servers: []string{"1.1.1.1"},
				Port:    "1234"}, nil},
			OnCallMethodsArgsStrTypeAppendCount: 0,
			CallTimes:                           1,
		}
		call := mockDnsOps.On(mockClientConfigFromFile.OnCallMethodName)
		for _, arg := range mockClientConfigFromFile.OnCallMethodArgType {
			call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
		}
		for _, ret := range mockClientConfigFromFile.RetArgList {
			call.ReturnArguments = append(call.ReturnArguments, ret)
		}
		call.Once()
	}

	generateRR := func(dnsName, ip, nextQueryTime string) dns.RR {
		var rr dns.RR
		if utilnet.IsIPv6(net.ParseIP(ip)) {
			rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      AAAA       " + ip)
		} else {
			rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      A       " + ip)
		}
		return rr
	}

	setDNSOpsMock := func(dnsName, retIP string) {
		methods := []ovntest.TestifyMockHelper{
			{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{dnsName}, 0, 1},
			{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
			{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(dnsName, retIP, "300")}}, 500 * time.Second, nil}, 0, 1},
		}
		for _, item := range methods {
			call := mockDnsOps.On(item.OnCallMethodName)
			for _, arg := range item.OnCallMethodArgType {
				call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
			}
			for _, ret := range item.RetArgList {
				call.ReturnArguments = append(call.ReturnArguments, ret)
			}
			call.Once()
		}
	}

	startDNSNameResolver := func(oldDNS bool) {
		var err error
		if oldDNS {
			setMockDnsOps()
			setDNSMockServer()
			fakeOVN.controller.dnsNameResolver, err = dnsnameresolver.NewEgressDNS(fakeOVN.controller.addressSetFactory,
				fakeOVN.controller.controllerName, fakeOVN.controller.stopChan, egressFirewallDNSDefaultDuration)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			// Initialize the dnsNameResolver.
			fakeOVN.controller.dnsNameResolver, err = dnsnameresolver.NewExternalEgressDNS(fakeOVN.controller.addressSetFactory,
				fakeOVN.controller.controllerName, true, fakeOVN.watcher.DNSNameResolverInformer().Informer(),
				fakeOVN.watcher.EgressFirewallInformer().Lister())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fakeOVN.controller.dnsNameResolver.Run()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	}

	startOvnWithNodes := func(dbSetup libovsdb.TestSetup, namespaces []v1.Namespace, egressFirewalls []egressfirewallapi.EgressFirewall,
		nodes []v1.Node, oldDNS bool) {
		fakeOVN.startWithDBSetup(dbSetup,
			&egressfirewallapi.EgressFirewallList{
				Items: egressFirewalls,
			},
			&v1.NamespaceList{
				Items: namespaces,
			},
			&v1.NodeList{
				Items: nodes,
			},
		)

		err := fakeOVN.controller.WatchNamespaces()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		startDNSNameResolver(oldDNS)

		err = fakeOVN.controller.WatchEgressFirewall()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fakeOVN.controller.efNodeController = fakeOVN.controller.newEFNodeController(fakeOVN.controller.watchFactory.NodeCoreInformer())
		err = controller.Start(fakeOVN.controller.efNodeController)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		for _, namespace := range namespaces {
			namespaceASip4, namespaceASip6 := buildNamespaceAddressSets(namespace.Name, []string{})
			if config.IPv4Mode {
				initialData = append(initialData, namespaceASip4)
			}
			if config.IPv6Mode {
				initialData = append(initialData, namespaceASip6)
			}
		}
	}

	startOvn := func(dbSetup libovsdb.TestSetup, namespaces []v1.Namespace, egressFirewalls []egressfirewallapi.EgressFirewall, oldDNS bool) {
		startOvnWithNodes(dbSetup, namespaces, egressFirewalls, nil, oldDNS)
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(false)
		clusterPortGroup = newClusterPortGroup()
		nodeSwitch = &nbdb.LogicalSwitch{
			UUID: node1Name + "-UUID",
			Name: node1Name,
		}
		joinSwitch = &nbdb.LogicalSwitch{
			UUID: "join-UUID",
			Name: "join",
		}
		initialData = []libovsdbtest.TestData{
			nodeSwitch,
			joinSwitch,
			clusterPortGroup,
			clusterRouter,
		}
		dbSetup = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
		if fakeOVN.controller.efNodeController != nil {
			controller.Stop(fakeOVN.controller.efNodeController)
		}
	})

	for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
		gwMode := gwMode
		ginkgo.Context("on startup", func() {
			ginkgo.It(fmt.Sprintf("reconciles stale ACLs, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					// owned by non-existing namespace
					fakeController := getFakeController(DefaultNetworkControllerName)
					purgeIDs := fakeController.getEgressFirewallACLDbIDs("none", 0)
					purgeACL := libovsdbops.BuildACL(
						"purgeACL1",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						purgeIDs.GetExternalIDs(),
						nil,
						t.PrimaryACLTier,
					)
					purgeACL.UUID = "purgeACL-UUID"
					// no externalIDs present => dbIDs can't be built
					purgeACL2 := libovsdbops.BuildACL(
						"purgeACL2",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						nil,
						nil,
						// we should not be in a situation where we have ACLs without externalIDs
						// but if we do have such lame ACLs then they will interfere with AdminNetPol logic
						t.PrimaryACLTier,
					)
					purgeACL2.UUID = "purgeACL2-UUID"

					namespace1 := *newNamespace("namespace1")
					namespace1ASip4, _ := buildNamespaceAddressSets(namespace1.Name, []string{})

					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					updateIDs := fakeController.getEgressFirewallACLDbIDs(namespace1.Name, 0)
					updateACL := libovsdbops.BuildACL(
						"",
						nbdb.ACLDirectionFromLport,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						nbdb.ACLSeverityInfo,
						false,
						updateIDs.GetExternalIDs(),
						nil,
						t.PrimaryACLTier,
					)
					updateACL.UUID = "updateACL-UUID"

					// this ACL is not in the egress firewall priority range and should be untouched
					ignoreACL := libovsdbops.BuildACL(
						"ignoreACL",
						nbdb.ACLDirectionFromLport,
						t.MinimumReservedEgressFirewallPriority-1,
						"",
						nbdb.ACLActionDrop,
						t.OvnACLLoggingMeter,
						"",
						false,
						nil,
						nil,
						// we should not be in a situation where we have unknown ACL that doesn't belong to any feature
						// but if we do have such lame ACLs then they will interfere with AdminNetPol logic
						t.PrimaryACLTier,
					)
					ignoreACL.UUID = "ignoreACL-UUID"

					nodeSwitch.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}
					joinSwitch.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}
					clusterPortGroup.ACLs = []string{purgeACL.UUID, purgeACL2.UUID, updateACL.UUID, ignoreACL.UUID}

					dbSetup := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							purgeACL,
							purgeACL2,
							ignoreACL,
							updateACL,
							nodeSwitch,
							joinSwitch,
							clusterRouter,
							clusterPortGroup,
							namespace1ASip4,
						},
					}

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					// All ACLs in the egress firewall priority range will be removed from the switches
					joinSwitch.ACLs = []string{ignoreACL.UUID}
					nodeSwitch.ACLs = []string{ignoreACL.UUID}
					// purgeACL will be deleted as its namespace doesn't exist
					clusterPortGroup.ACLs = []string{ignoreACL.UUID, purgeACL2.UUID}

					// updateACL will be updated
					// Direction of both ACLs will be converted to
					updateACL.Direction = nbdb.ACLDirectionToLport
					newName := libovsdbutil.GetACLName(updateIDs)
					updateACL.Name = &newName
					// check severity was reset from default to nil
					updateACL.Severity = nil
					// match shouldn't have cluster exclusion
					pgIDs := getNamespacePortGroupDbIDs(namespace1.Name, DefaultNetworkControllerName)
					namespacePG := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{updateACL})
					namespacePG.UUID = namespacePG.Name + "-UUID"
					updateACL.Match = "(ip4.dst == 1.2.3.4/23) && inport == @" + namespacePG.Name
					updateACL.Tier = t.DefaultACLTier // ensure the tier of the ACL is updated from 0 to 2

					expectedDatabaseState := []libovsdb.TestData{
						purgeACL2,
						ignoreACL,
						updateACL,
						nodeSwitch,
						joinSwitch,
						clusterRouter,
						clusterPortGroup,
						namespace1ASip4,
						namespacePG,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv4 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv6 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
							},
						},
					})

					config.IPv6Mode = true
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("removes stale acl for delete egress firewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {

					fakeController := getFakeController(DefaultNetworkControllerName)
					fakeOVN.controller = fakeController

					namespace1 := *newNamespace("namespace1")
					// no egress firewalls exist
					dbSetup := getEFExpectedDb(initialData, fakeOVN, "namespace1", "(ip4.dst == 1.2.3.4/23)",
						"", nbdb.ACLActionAllow)
					startOvn(libovsdbtest.TestSetup{NBData: dbSetup}, []v1.Namespace{namespace1}, nil, true)

					// re-create initial db, since startOvn may add more objects to initialData
					initialDatabaseState := getEFExpectedDb(initialData, fakeOVN, "namespace1", "(ip4.dst == 1.2.3.4/23)",
						"", nbdb.ACLActionAllow)
					expectedDatabaseState := getEFExpectedDbAfterDelete(initialDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.DescribeTable("correctly removes stale acl and DNS address set created", func(gwMode config.GatewayMode, oldDNS bool) {
				if !oldDNS {
					// enable the dns name resolver flag.
					config.OVNKubernetesFeature.EnableDNSNameResolver = true
				}
				config.Gateway.Mode = gwMode

				app.Action = func(ctx *cli.Context) error {
					resolvedIP := "1.1.1.1"
					namespace1 := *newNamespace("namespace1")
					dnsName := util.LowerCaseFQDN("www.example.com")

					fakeController := getFakeController(DefaultNetworkControllerName)
					fakeOVN.controller = fakeController

					// add dns address set along with the acl and pg to the initial db.
					addrSet, _ := addressset.GetTestDbAddrSets(
						dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsName, fakeOVN.controller.controllerName),
						[]string{resolvedIP})
					addrSetUUID := strings.TrimSuffix(addrSet.UUID, "-UUID")
					dbWithACLAndPG := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionAllow)
					addrSetDbState := append(dbWithACLAndPG, addrSet)

					startOvn(libovsdbtest.TestSetup{NBData: addrSetDbState}, []v1.Namespace{namespace1}, nil, oldDNS)

					// re-create initial db, since startOvn may add more objects to initialData.
					dbWithACLAndPG = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionAllow)
					expectedDatabaseState := getEFExpectedDbAfterDelete(dbWithACLAndPG)

					// check dns address set is cleaned up on initial sync.
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			},
				ginkgo.Entry(fmt.Sprintf("correctly removes stale acl and DNS address set created using old dns resolution, gateway mode %s", gwMode), gwMode, true),
				ginkgo.Entry(fmt.Sprintf("correctly removes stale acl and DNS address set created using new dns resolution, gateway mode %s", gwMode), gwMode, false),
			)
		})
		ginkgo.Context("during execution", func() {
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall denying traffic udp traffic on port 100, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "UDP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "((udp && ( udp.dst == 100 )))", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "TCP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("egress firewall with node selector updates during node update, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				var err error
				nodeName := "node1"
				nodeIP := "9.9.9.9"
				nodeIP2 := "11.11.11.11"
				nodeIP3 := "fc00:f853:ccd:e793::2"
				config.IPv4Mode = true
				config.IPv6Mode = true

				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					labelKey := "name"
					labelValue := "test"
					selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								NodeSelector: &selector,
							},
						},
					})

					startOvnWithNodes(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]v1.Node{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: nodeName,
									Annotations: map[string]string{
										util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\",\"%s/24\",\"%s/64\"]", nodeIP, nodeIP2, nodeIP3),
									},
								},
							},
						}, true)

					// update the node to match the selector
					patch := struct {
						Metadata map[string]interface{} `json:"metadata"`
					}{
						Metadata: map[string]interface{}{
							"labels": map[string]string{labelKey: labelValue},
						},
					}
					ginkgo.By("Updating a node to match nodeSelector on Egress Firewall")
					patchData, err := json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// trigger update event
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName,
						types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData,
						fakeOVN, namespace1.Name,
						fmt.Sprintf("(ip4.dst == %s || ip4.dst == %s || ip6.dst == %s)", nodeIP2, nodeIP, nodeIP3), "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Updating a node to not match nodeSelector on Egress Firewall")
					patch.Metadata = map[string]interface{}{"labels": map[string]string{labelKey: libovsdbutil.UnspecifiedL4Match}}
					patchData, err = json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// trigger update event
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName,
						types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err = app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("egress firewall with node selector updates during node delete, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				var err error
				nodeName := "node1"
				nodeIP := "9.9.9.9"
				nodeIP2 := "11.11.11.11"
				nodeIP3 := "fc00:f853:ccd:e793::2"
				config.IPv4Mode = true
				config.IPv6Mode = true

				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					labelKey := "name"
					labelValue := "test"
					selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								NodeSelector: &selector,
							},
						},
					})

					startOvnWithNodes(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]v1.Node{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: nodeName,
									Annotations: map[string]string{
										util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\",\"%s/24\",\"%s/64\"]", nodeIP, nodeIP2, nodeIP3),
									},
									Labels: map[string]string{labelKey: labelValue},
								},
							},
						}, true)

					expectedDatabaseState := getEFExpectedDb(initialData,
						fakeOVN, namespace1.Name,
						fmt.Sprintf("(ip4.dst == %s || ip4.dst == %s || ip6.dst == %s)", nodeIP2, nodeIP, nodeIP3), "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Deleting a node")
					// trigger delete event
					err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName,
						metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err = app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("egress firewall with node selector doesn't affect node handler, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				nodeName := "node1"
				nodeIP := "9.9.9.9"
				nodeIP2 := "11.11.11.11"
				nodeIP3 := "fc00:f853:ccd:e793::2"
				v4NodeSubnet := "10.128.0.0/16"

				config.IPv4Mode = true
				app.Action = func(ctx *cli.Context) error {

					namespace1 := *newNamespace("namespace1")
					labelKey := "name"
					labelValue := "test"
					selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
					var err error
					egressFirewallObj := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								NodeSelector: &selector,
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewallObj}, true)
					err = fakeOVN.controller.WatchNodes()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// lock internal egressfirewall object
					// that will lock ef node handler until the lock is released
					obj, loaded := fakeOVN.controller.egressFirewalls.Load(namespace1.Name)
					gomega.Expect(loaded).To(gomega.BeTrue())
					ef, ok := obj.(*egressFirewall)
					gomega.Expect(ok).To(gomega.BeTrue())
					ef.Lock()

					// now add node, then update node, check that both events are handled immediately
					// check that egressfirewall node event is only handled after lock release
					node := &v1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: nodeName,
							Annotations: map[string]string{
								// this will cause node add failure, stolen from
								// master_test:reconciles node host subnets after dual-stack to single-stack downgrade
								"k8s.ovn.org/node-subnets":    fmt.Sprintf("{\"default\":[\"%s\", \"fd02:0:0:2::2895/64\"]}", v4NodeSubnet),
								util.OVNNodeHostCIDRs:         fmt.Sprintf("[\"%s/24\",\"%s/24\",\"%s/64\"]", nodeIP, nodeIP2, nodeIP3),
								"k8s.ovn.org/node-chassis-id": "2",
								util.OVNNodeGRLRPAddrs:        "{\"default\":{\"ipv4\":\"100.64.0.2/16\"}}",
							},
						},
					}
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// check switch subnet is not set
					newNodeLS := &nbdb.LogicalSwitch{Name: node.Name}
					gomega.Consistently(func() bool {
						sw, err := libovsdbops.GetLogicalSwitch(fakeOVN.nbClient, newNodeLS)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						return sw.OtherConfig["subnet"] == v4NodeSubnet
					}).WithTimeout(500 * time.Millisecond).Should(gomega.BeFalse())

					ginkgo.By("Updating node network and labels")
					// update the node to match the selector and to fix node-subnets
					// fixing node-subnets will cause switch add
					newNode, err := fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					newNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":[\"%s\"]}", v4NodeSubnet)
					newNode.Labels = map[string]string{labelKey: labelValue}

					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), newNode, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("Check node handler was called on update")
					// check switch subnet is set, meaning the node handler was called
					gomega.Eventually(func() bool {
						sw, err := libovsdbops.GetLogicalSwitch(fakeOVN.nbClient, newNodeLS)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						return sw.OtherConfig["subnet"] == v4NodeSubnet
					}).WithTimeout(500 * time.Millisecond).Should(gomega.BeTrue())

					// make sure egress firewall acl was not updated as we are still holding a lock
					getACLs := func() int {
						acls, err := libovsdbops.FindACLsWithPredicate(fakeOVN.nbClient, func(acl *nbdb.ACL) bool {
							return true
						})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						return len(acls)
					}
					ginkgo.By("Check egress firewall node handler is still locked")
					gomega.Consistently(getACLs).WithTimeout(1 * time.Second).Should(gomega.Equal(0))
					ginkgo.By("Unlocking egress firewall object")
					ef.Unlock()
					gomega.Eventually(getACLs).Should(gomega.Equal(1))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It(fmt.Sprintf("correctly retries deleting an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")

					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "TCP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
					})

					startOvnWithNodes(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]v1.Node{
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
							{
								Status: v1.NodeStatus{
									Phase: v1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node2Name, ""),
							},
						}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egress firewall Add to fail
					time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
					// check to see if the retry cache has an entry for this egress firewall
					key := getEgressFirewallNamespacedName(egressFirewall)
					ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key,
						fakeOVN.controller.retryEgressFirewalls,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						gomega.BeNil(),             // newObj should be nil
					)

					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOVN.controller.nbClient)
					retry.SetRetryObjWithNoBackoff(key, fakeOVN.controller.retryEgressFirewalls)
					fakeOVN.controller.retryEgressFirewalls.RequestRetryObjs()

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOVN.controller.retryEgressFirewalls)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries adding and updating an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					ginkgo.By("Bringing down NBDB")
					// inject transient problem, nbdb is down
					fakeOVN.controller.nbClient.Close()
					gomega.Eventually(func() bool {
						return fakeOVN.controller.nbClient.Connected()
					}).Should(gomega.BeFalse())

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// sleep long enough for TransactWithRetry to fail, causing egress firewall Add to fail
					time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
					// check to see if the retry cache has an entry for this egress firewall
					key, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("retry entry: old obj should not be nil, new obj should not be nil")
					retry.CheckRetryObjectMultipleFieldsEventually(
						key,
						fakeOVN.controller.retryEgressFirewalls,
						gomega.Not(gomega.BeNil()), // oldObj should not be nil
						gomega.Not(gomega.BeNil()), // newObj should not be nil
					)

					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
					defer cancel()
					ginkgo.By("bringing up NBDB and requesting retry of entry")
					resetNBClient(connCtx, fakeOVN.controller.nbClient)

					retry.SetRetryObjWithNoBackoff(key, fakeOVN.controller.retryEgressFirewalls)
					ginkgo.By("request immediate retry object")
					fakeOVN.controller.retryEgressFirewalls.RequestRetryObjs()
					// check the cache no longer has the entry
					retry.CheckRetryObjectEventually(key, false, fakeOVN.controller.retryEgressFirewalls)

					expectedDatabaseState = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall's ACL logging, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					// get the current namespace
					namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// enable ACL logging with severity alert, alert
					logSeverity := "alert"
					updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, logSeverity, logSeverity)
					namespace.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespace, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// eventually, we should see the changes in the namespace reflected in the database
					acl := expectedDatabaseState[len(expectedDatabaseState)-2].(*nbdb.ACL)
					acl.Log = true
					acl.Severity = &logSeverity
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			for _, ipMode := range []string{"IPv4", "IPv6"} {
				ginkgo.It(fmt.Sprintf("configures egress firewall correctly with node selector, gateway mode: %s, IP mode: %s", gwMode, ipMode), func() {
					nodeIP4CIDR := "10.10.10.1/24"
					nodeIP, _, _ := net.ParseCIDR(nodeIP4CIDR)
					nodeIP6CIDR := "fc00:f853:ccd:e793::2/64"
					nodeIP6, _, _ := net.ParseCIDR(nodeIP6CIDR)
					config.Gateway.Mode = gwMode
					var nodeCIDR string
					if ipMode == "IPv4" {
						config.IPv4Mode = true
						config.IPv6Mode = false
						nodeCIDR = nodeIP4CIDR

					} else {
						config.IPv4Mode = false
						config.IPv6Mode = true
						nodeCIDR = nodeIP6CIDR
					}
					app.Action = func(ctx *cli.Context) error {
						labelKey := "name"
						labelValue := "test"
						selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
						namespace1 := *newNamespace("namespace1")
						egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
							{
								Type: "Allow",
								To: egressfirewallapi.EgressFirewallDestination{
									NodeSelector: &selector,
								},
							},
						})
						mdata := newObjectMeta(node1Name, "")
						mdata.Labels = map[string]string{labelKey: labelValue}
						mdata.Annotations = map[string]string{util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s\"]", nodeCIDR)}

						startOvnWithNodes(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
							[]v1.Node{
								{
									ObjectMeta: mdata,
								},
							}, true)
						var match string
						if config.IPv4Mode {
							match = fmt.Sprintf("(ip4.dst == %s)", nodeIP)
						} else {
							match = fmt.Sprintf("(ip6.dst == %s)", nodeIP6)
						}
						expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
							match, "", nbdb.ACLActionAllow)
						gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

						return nil
					}

					err := app.Run([]string{app.Name})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})
			}
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall with subnet exclusion, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					clusterSubnetStr := "10.128.0.0/14"
					_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
					config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 0.0.0.0/0 && ip4.dst != "+clusterSubnetStr+")", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall for namespace name > 43 symbols, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					// 52 characters namespace
					namespace1 := *newNamespace("abcdefghigklmnopqrstuvwxyzabcdefghigklmnopqrstuvwxyz")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2.2.3.5/23",
							},
						},
					})

					startOvn(dbSetup, []v1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					dbWith1ACL := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "", nbdb.ACLActionAllow)

					pg := dbWith1ACL[len(dbWith1ACL)-1].(*nbdb.PortGroup)
					aclIDs2 := fakeOVN.controller.getEgressFirewallACLDbIDs(egressFirewall.Namespace, 1)
					ipv4ACL2 := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(aclIDs2),
						nbdb.ACLDirectionToLport,
						t.EgressFirewallStartPriority-1,
						"(ip4.dst == 2.2.3.5/23) && inport == @"+pg.Name,
						nbdb.ACLActionAllow,
						t.OvnACLLoggingMeter,
						"",
						false,
						aclIDs2.GetExternalIDs(),
						nil,
						t.DefaultACLTier,
					)
					ipv4ACL2.UUID = "ipv4ACL2-UUID"
					pg.ACLs = append(pg.ACLs, ipv4ACL2.UUID)

					expectedDatabaseState := append(dbWith1ACL, ipv4ACL2)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err := fakeOVN.controller.syncEgressFirewall([]interface{}{egressFirewall})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ACL should be removed from the port group egfw is deleted
					expectedDatabaseState = getEFExpectedDbAfterDelete(dbWith1ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes object that failed to be created, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								// wrong CIDR format, creation will fail
								CIDRSelector: "1.2.3.4",
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, nil, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// creation will fail, check retry object exists
					efKey, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(efKey, true, fakeOVN.controller.retryEgressFirewalls)

					// delete wrong object
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// retry object should not be present
					gomega.Eventually(func() bool {
						return retry.CheckRetryObj(efKey, fakeOVN.controller.retryEgressFirewalls)
					}, time.Second).Should(gomega.BeFalse())

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.DescribeTable("correctly cleans up object that failed to be created", func(gwMode config.GatewayMode, oldDNS bool) {
				config.Gateway.Mode = gwMode
				if !oldDNS {
					// enable the dns name resolver flag.
					config.OVNKubernetesFeature.EnableDNSNameResolver = true
				}
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					dnsName := "a.b.c"
					dnsNameLowerCaseFQDN := util.LowerCaseFQDN(dnsName)
					resolvedIP := "2.2.2.2"
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: dnsName,
							},
						},
					})
					// start ovn without namespaces, that will cause egress firewall creation failure
					startOvn(dbSetup, nil, nil, oldDNS)

					if oldDNS {
						setDNSOpsMock(dnsName, resolvedIP)
					} else {
						dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsNameLowerCaseFQDN, resolvedIP)
						// Create the dns name resolver object.
						_, err := fakeOVN.fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
							Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// creation will fail, check retry object exists
					efKey, err := retry.GetResourceKey(egressFirewall)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					retry.CheckRetryObjectEventually(efKey, true, fakeOVN.controller.retryEgressFirewalls)

					dnsNameForAddrSet := dnsName
					if !oldDNS {
						dnsNameForAddrSet = dnsNameLowerCaseFQDN
					}
					// check dns address set was created
					addrSet, _ := addressset.GetTestDbAddrSets(
						dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsNameForAddrSet, fakeOVN.controller.controllerName),
						[]string{resolvedIP})
					expectedDatabaseState := append(initialData, addrSet)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					// delete failed object
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// retry object should not be present
					gomega.Eventually(func() bool {
						return retry.CheckRetryObj(efKey, fakeOVN.controller.retryEgressFirewalls)
					}, time.Second).Should(gomega.BeFalse())

					if !oldDNS {
						// delete the dns name resolver object.
						err = fakeOVN.fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
							Delete(context.TODO(), "dns-default", metav1.DeleteOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}

					// check dns address set is cleaned up on delete
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(initialData))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
				ginkgo.Entry(fmt.Sprintf("correctly cleans up object that failed to be created using old dns resolution, gateway mode %s", gwMode), gwMode, true),
				ginkgo.Entry(fmt.Sprintf("correctly cleans up object that failed to be created using new dns resolution, gateway mode %s", gwMode), gwMode, false),
			)
			ginkgo.DescribeTable("correctly creates egress firewall using different dns resolution methods, dns name types and ip families", func(gwMode config.GatewayMode, oldDNS bool, dnsName, resolvedIP string) {
				if !oldDNS {
					// enable the dns name resolver flag.
					config.OVNKubernetesFeature.EnableDNSNameResolver = true
				}
				config.Gateway.Mode = gwMode
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					dnsNameLowerCaseFQDN := util.LowerCaseFQDN(dnsName)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: dnsName,
							},
						},
					})
					startOvn(dbSetup, []v1.Namespace{namespace1}, nil, oldDNS)

					if oldDNS {
						setDNSOpsMock(dnsName, resolvedIP)
					} else {
						dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsNameLowerCaseFQDN, resolvedIP)
						// Create the dns name resolver object.
						_, err := fakeOVN.fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
							Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}

					// Create the egress firewall object.
					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					dnsNameForAddrSet := dnsName
					if !oldDNS {
						dnsNameForAddrSet = dnsNameLowerCaseFQDN
					}
					// check dns address set was created along with the acl and pg.
					addrSet, _ := addressset.GetTestDbAddrSets(
						dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsNameForAddrSet, fakeOVN.controller.controllerName),
						[]string{resolvedIP})
					addrSetUUID := strings.TrimSuffix(addrSet.UUID, "-UUID")
					dbWithACLAndPG := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionAllow)
					addrSetDbState := append(dbWithACLAndPG, addrSet)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(addrSetDbState))

					// delete the egress firewall object.
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					if !oldDNS {
						// delete the dns name resolver object.
						err = fakeOVN.fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
							Delete(context.TODO(), "dns-default", metav1.DeleteOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}

					// check dns address set is cleaned up on delete.
					expectedDatabaseState := getEFExpectedDbAfterDelete(dbWithACLAndPG)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using old dns resolution for regular DNS name with IPv4 address, gateway mode %s", gwMode), gwMode, true, "a.b.c", "2.2.2.2"),
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using new dns resolution for regular DNS name with IPv4 address, gateway mode %s", gwMode), gwMode, false, "a.b.c", "2.2.2.2"),
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using old dns resolution for regular DNS name with IPv6 address, gateway mode %s", gwMode), gwMode, true, "a.b.c", "2002::1234:abcd:ffff:c0a8:101"),
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using new dns resolution for regular DNS name with IPv6 address, gateway mode %s", gwMode), gwMode, false, "a.b.c", "2002::1234:abcd:ffff:c0a8:101"),
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using new dns resolution for wildcard DNS name  with IPv4 address, gateway mode %s", gwMode), gwMode, false, "*.b.c", "2.2.2.2"),
				ginkgo.Entry(fmt.Sprintf("correctly creates egress firewall using new dns resolution for wildcard DNS name  with IPv6 address, gateway mode %s", gwMode), gwMode, false, "*.b.c", "2002::1234:abcd:ffff:c0a8:101"),
			)
		})
	}
})

var _ = ginkgo.Describe("OVN test basic functions", func() {
	var (
		app       *cli.App
		fakeOVN   *FakeOVN
		nodeLabel = map[string]string{"use": "this"}
	)

	const (
		node1Name string = "node1"
		node1Addr string = "9.9.9.9"
		node2Name string = "node2"
		node2Addr string = "10.10.10.10"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each test
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		dbSetup := libovsdbtest.TestSetup{}
		fakeOVN = NewFakeOVN(false)
		a := newObjectMeta(node1Name, "")
		a.Labels = nodeLabel
		a.Annotations = map[string]string{
			util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\"]", node1Addr),
		}
		node1 := v1.Node{ObjectMeta: a}
		b := newObjectMeta(node2Name, "")
		b.Annotations = map[string]string{
			util.OVNNodeHostCIDRs: fmt.Sprintf("[\"%s/24\"]", node2Addr),
		}
		node2 := v1.Node{
			ObjectMeta: b,
		}
		fakeOVN.startWithDBSetup(dbSetup, &v1.NodeList{Items: []v1.Node{node1, node2}})
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "((tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "((udp) || (tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "((udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 )))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			gomega.Expect(test.expectedMatch).To(gomega.Equal(l4Match))
		}
	})
	ginkgo.It("computes correct match function", func() {
		type testcase struct {
			clusterSubnets []string
			pgName         string
			ipv4Mode       bool
			ipv6Mode       bool
			destinations   []matchTarget
			ports          []egressfirewallapi.EgressFirewallPort
			output         string
		}
		testcases := []testcase{
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", false}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", false}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", false}, {matchKindV6AddressSet, "destv6", false}},
				ports:          nil,
				output:         "(ip4.dst == $destv4 || ip6.dst == $destv6) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4AddressSet, "destv4", false}, {matchKindV6AddressSet, "", false}},
				ports:          nil,
				output:         "(ip4.dst == $destv4) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6CIDR, "2001::/64", false}},
				ports:          nil,
				output:         "(ip6.dst == 2001::/64) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", false}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && inport == @a123456",
			},
			// with cluster subnet exclusion
			{
				clusterSubnets: []string{"10.128.0.0/14"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       false,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", true}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       false,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV6AddressSet, "destv6", true}},
				ports:          nil,
				output:         "(ip6.dst == $destv6) && inport == @a123456",
			},
			{
				clusterSubnets: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				pgName:         "a123456",
				ipv4Mode:       true,
				ipv6Mode:       true,
				destinations:   []matchTarget{{matchKindV4CIDR, "1.2.3.4/32", true}},
				ports:          nil,
				output:         "(ip4.dst == 1.2.3.4/32 && ip4.dst != 10.128.0.0/14) && inport == @a123456",
			},
		}

		for _, tc := range testcases {
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets

			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.pgName, tc.destinations, tc.ports)
			gomega.Expect(matchExpression).To(gomega.Equal(tc.output))
		}
	})
	ginkgo.It("correctly parses egressFirewallRules", func() {
		type testcase struct {
			egressFirewallRule egressfirewallapi.EgressFirewallRule
			id                 int
			err                bool
			errOutput          string
			output             egressFirewallRule
			clusterSubnets     []string
		}
		testcases := []testcase{
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32"},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3./32"},
				},
				id:        1,
				err:       true,
				errOutput: "invalid CIDR address: 1.2.3./32",
				output:    egressFirewallRule{},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
				id:  2,
				err: false,
				output: egressFirewallRule{
					id:     2,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002::1235:abcd:ffff:c0a8:101/64"},
				},
			},
			// check clusterSubnet intersection
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32", clusterSubnetIntersection: false},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.0/24"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.0/24", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1235::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1235::/80", clusterSubnetIntersection: false},
				},
			},
			// dual stack
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "10.128.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "10.128.3.4/32", clusterSubnetIntersection: true},
				},
			},
			{
				clusterSubnets: []string{"10.128.0.0/16", "2002:0:0:1234::/64"},
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002:0:0:1234:0001::/80"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002:0:0:1234:0001::/80", clusterSubnetIntersection: true},
				},
			},
			// nodeSelector tests
			// selector matches nothing
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To: egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to: destination{nodeAddrs: map[string][]string{}, nodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"no": "match"}}},
				},
			},
			// empty selector, match all
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: map[string][]string{node1Name: {node1Addr}, node2Name: {node2Addr}}, nodeSelector: &metav1.LabelSelector{}},
				},
			},
			// match one node
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{NodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{nodeAddrs: map[string][]string{node1Name: {node1Addr}}, nodeSelector: &metav1.LabelSelector{MatchLabels: nodeLabel}},
				},
			},
		}
		for _, tc := range testcases {
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.clusterSubnets {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets
			output, err := fakeOVN.controller.newEgressFirewallRule(tc.egressFirewallRule, tc.id)
			if tc.err == true {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(tc.errOutput).To(gomega.Equal(err.Error()))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(*output).To(gomega.Equal(tc.output))
			}
		}
	})
})
