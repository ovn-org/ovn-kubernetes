// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

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
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	efcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/egressfirewall"
	dnsnameresolver "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	t "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
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

func getEFExpectedDb(initialData []libovsdb.TestData, fakeOVN *FakeOVN, nsName string, dstMatch, portMatch string,
	action nbdb.ACLAction) []libovsdb.TestData {
	pgName := fakeOVN.controller.getNamespacePortGroupName(nsName)
	dbIDs := fakeOVN.controller.efController.GetEgressFirewallACLDbIDs(nsName, 0)
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
	pgIDs := getNamespacePortGroupDbIDs(nsName, t.DefaultNetworkControllerName)
	namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{acl})
	namespacePortGroup.UUID = pgName + "-UUID"
	return append(initialData, acl, namespacePortGroup)
}

func getEFExpectedDbUDN(initialData []libovsdb.TestData, fakeOVN *FakeOVN, nsName string, dstMatch, portMatch string,
	action nbdb.ACLAction, udnName string) []libovsdb.TestData {
	//pgName := fakeOVN.controller.getNamespacePortGroupName(nsName)
	ownerController := udnName + "-network-controller"
	pgIDs := getNamespacePortGroupDbIDs(nsName, ownerController)
	dbIDs := fakeOVN.controller.efController.GetEgressFirewallACLDbIDs(nsName, 0)
	match := dstMatch + " && inport == @" + libovsdbutil.GetPortGroupName(pgIDs)
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
	//pgIDs := getNamespacePortGroupDbIDs(nsName, ownerController)
	namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{acl})
	namespacePortGroup.UUID = libovsdbutil.GetPortGroupName(pgIDs) + "-UUID"

	defaultPGIDs := getNamespacePortGroupDbIDs(nsName, t.DefaultNetworkControllerName)
	namespaceDefaultPortGroup := libovsdbutil.BuildPortGroup(defaultPGIDs, nil, nil)
	namespaceDefaultPortGroup.UUID = libovsdbutil.GetPortGroupName(defaultPGIDs) + "-UUID"
	return append(initialData, namespaceDefaultPortGroup, acl, namespacePortGroup)
}

func getEFExpectedDbAfterDelete(prevExpectedData []libovsdb.TestData) []libovsdb.TestData {
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
		initialData            []libovsdb.TestData
		dbSetup                libovsdb.TestSetup
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
			{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
			{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
			{
				OnCallMethodName:    "Exchange",
				OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
				RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(dnsName, retIP, "300")}}, 500 * time.Second, nil},
				CallTimes:           1,
			},
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

	startOvnWithNodes := func(dbSetup libovsdb.TestSetup, namespaces []corev1.Namespace, egressFirewalls []egressfirewallapi.EgressFirewall,
		nodes []corev1.Node, oldDNS bool) {
		fakeOVN.startWithDBSetup(dbSetup,
			&egressfirewallapi.EgressFirewallList{
				Items: egressFirewalls,
			},
			&corev1.NamespaceList{
				Items: namespaces,
			},
			&corev1.NodeList{
				Items: nodes,
			},
		)

		err := fakeOVN.controller.WatchNamespaces()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		startDNSNameResolver(oldDNS)

		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fakeOVN.controller.efController, err = efcontroller.NewEFController(
			"egress-firewall-controller",
			fakeOVN.controller.nodeName,
			fakeOVN.controller.kube,
			fakeOVN.controller.nbClient,
			fakeOVN.controller.watchFactory.NamespaceInformer().Lister(),
			fakeOVN.controller.watchFactory.NodeCoreInformer(),
			fakeOVN.controller.watchFactory.EgressFirewallInformer(),
			fakeOVN.controller.networkManager,
			fakeOVN.controller.dnsNameResolver,
			fakeOVN.controller.observManager,
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = fakeOVN.controller.efController.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	startOvn := func(dbSetup libovsdb.TestSetup, namespaces []corev1.Namespace, egressFirewalls []egressfirewallapi.EgressFirewall, oldDNS bool) {
		startOvnWithNodes(dbSetup, namespaces, egressFirewalls, nil, oldDNS)
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN(false, node1Name)
		clusterPortGroup = newClusterPortGroup()
		nodeSwitch = &nbdb.LogicalSwitch{
			UUID: node1Name + "-UUID",
			Name: node1Name,
		}
		joinSwitch = &nbdb.LogicalSwitch{
			UUID: "join-UUID",
			Name: "join",
		}
		initialData = []libovsdb.TestData{
			nodeSwitch,
			joinSwitch,
			clusterPortGroup,
			clusterRouter,
		}
		dbSetup = libovsdb.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
		if fakeOVN.controller.efController != nil {
			fakeOVN.controller.efController.Stop()
		}
	})

	for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
		gwMode := gwMode
		ginkgo.Context("on startup", func() {
			ginkgo.It(fmt.Sprintf("reconciles stale ACLs, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					// owned by non-existing namespace
					fakeController := getFakeController(t.DefaultNetworkControllerName)
					purgeIDs := fakeController.efController.GetEgressFirewallACLDbIDs("none", 0)
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

					// Leftover ACLs on the cluster port group are not migrated and stay put.
					purgeACL2 := libovsdbops.BuildACL(
						"purgeACL2",
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
					purgeACL2.UUID = "purgeACL2-UUID"

					namespace1 := *ovntest.NewNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					updateIDs := fakeController.efController.GetEgressFirewallACLDbIDs(namespace1.Name, 0)
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

					clusterPortGroup.ACLs = []string{purgeACL2.UUID}

					dbSetup := libovsdb.TestSetup{
						NBData: []libovsdb.TestData{
							purgeACL,
							purgeACL2,
							updateACL,
							nodeSwitch,
							joinSwitch,
							clusterRouter,
							clusterPortGroup,
						},
					}

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					// updateACL will be updated
					// Direction of both ACLs will be converted to
					updateACL.Direction = nbdb.ACLDirectionToLport
					newName := libovsdbutil.GetACLName(updateIDs)
					updateACL.Name = &newName
					// check severity was reset from default to nil
					updateACL.Severity = nil
					// match shouldn't have cluster exclusion
					pgIDs := getNamespacePortGroupDbIDs(namespace1.Name, t.DefaultNetworkControllerName)
					namespacePG := libovsdbutil.BuildPortGroup(pgIDs, nil, []*nbdb.ACL{updateACL})
					namespacePG.UUID = namespacePG.Name + "-UUID"
					updateACL.Match = "(ip4.dst == 1.2.3.4/23) && inport == @" + namespacePG.Name
					updateACL.Tier = t.DefaultACLTier // ensure the tier of the ACL is updated from 0 to 2

					expectedDatabaseState := []libovsdb.TestData{
						purgeACL2,
						updateACL,
						nodeSwitch,
						joinSwitch,
						clusterRouter,
						clusterPortGroup,
						namespacePG,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv4 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv6 CIDR, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
							},
						},
					})

					config.IPv6Mode = true
					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("removes stale acl for delete egress firewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {

					fakeController := getFakeController(t.DefaultNetworkControllerName)
					fakeOVN.controller = fakeController

					namespace1 := *ovntest.NewNamespace("namespace1")
					// no egress firewalls exist
					dbSetup := getEFExpectedDb(initialData, fakeOVN, "namespace1", "(ip4.dst == 1.2.3.4/23)",
						"", nbdb.ACLActionAllow)
					startOvn(libovsdb.TestSetup{NBData: dbSetup}, []corev1.Namespace{namespace1}, nil, true)

					// re-create initial db, since startOvn may add more objects to initialData
					initialDatabaseState := getEFExpectedDb(initialData, fakeOVN, "namespace1", "(ip4.dst == 1.2.3.4/23)",
						"", nbdb.ACLActionAllow)
					expectedDatabaseState := getEFExpectedDbAfterDelete(initialDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
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

				app.Action = func(*cli.Context) error {
					resolvedIP := "1.1.1.1"
					namespace1 := *ovntest.NewNamespace("namespace1")
					dnsName := util.LowerCaseFQDN("www.example.com")

					fakeController := getFakeController(t.DefaultNetworkControllerName)
					fakeOVN.controller = fakeController

					// add dns address set along with the acl and pg to the initial db.
					addrSet, _ := addressset.GetTestDbAddrSets(
						dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsName, fakeOVN.controller.controllerName),
						[]string{resolvedIP})
					addrSetUUID := strings.TrimSuffix(addrSet.UUID, "-UUID")
					dbWithACLAndPG := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionAllow)
					addrSetDbState := append(dbWithACLAndPG, addrSet)

					startOvn(libovsdb.TestSetup{NBData: addrSetDbState}, []corev1.Namespace{namespace1}, nil, oldDNS)

					// re-create initial db, since startOvn may add more objects to initialData.
					dbWithACLAndPG = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionAllow)
					expectedDatabaseState := getEFExpectedDbAfterDelete(dbWithACLAndPG)

					// check dns address set is cleaned up on initial sync.
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
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
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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
					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "((udp && ( udp.dst == 100 )))", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall with UDN denying traffic udp traffic on port 100, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				app.Action = func(*cli.Context) error {
					namespace1 := *newUDNNamespace("namespace1")
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
					netconf := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
					networkConfig, err := util.NewNetInfo(netconf.netconf())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOVN.networkManager = &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{
						namespace1.Name: networkConfig,
					}}
					// add UDN namespaced port group
					ownerController := networkConfig.GetNetworkName() + "-network-controller"
					pgIDs := getNamespacePortGroupDbIDs(namespace1.Name, ownerController)
					namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
					dbSetup.NBData = append(dbSetup.NBData, namespacePortGroup)
					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDbUDN(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "((udp && ( udp.dst == 100 )))", nbdb.ACLActionDrop, networkConfig.GetNetworkName())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall with UDN, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				app.Action = func(*cli.Context) error {
					namespace1 := *newUDNNamespace("namespace1")
					netconf := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
					networkConfig, err := util.NewNetInfo(netconf.netconf())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOVN.networkManager = &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{
						namespace1.Name: networkConfig,
					}}
					// add UDN namespaced port group
					ownerController := networkConfig.GetNetworkName() + "-network-controller"
					pgIDs := getNamespacePortGroupDbIDs(namespace1.Name, ownerController)
					namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
					dbSetup.NBData = append(dbSetup.NBData, namespacePortGroup)
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

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDbUDN(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow, networkConfig.GetNetworkName())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly handles egressFirewall with UDN change, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				app.Action = func(*cli.Context) error {
					namespace1 := *newUDNNamespace("namespace1")
					netconf := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
					networkConfig, err := util.NewNetInfo(netconf.netconf())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeNM := &networkmanager.FakeNetworkManager{
						PrimaryNetworks: map[string]util.NetInfo{
							namespace1.Name: networkConfig,
						},
						UDNNamespaces: sets.New[string](namespace1.Name),
					}
					fakeOVN.networkManager = fakeNM
					// add UDN namespaced port group
					ownerController := networkConfig.GetNetworkName() + "-network-controller"
					pgIDs := getNamespacePortGroupDbIDs(namespace1.Name, ownerController)
					namespacePortGroup := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
					dbSetup.NBData = append(dbSetup.NBData, namespacePortGroup)
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

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDbUDN(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow, networkConfig.GetNetworkName())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					ginkgo.By("triggering fake network manager to simulate NAD deletion, OVN EF config should be removed")
					// remove primary NAD to simulate network being totally deleted
					fakeNM.Lock()
					delete(fakeNM.PrimaryNetworks, namespace1.Name)
					fakeNM.Unlock()

					fakeNM.TriggerHandlers(netconf.nadName, nil, true)

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					ginkgo.By("creating new UDN for namespace, EF should get configured")
					netconf2 := dummyLayer2PrimaryUserDefinedNetwork("128.168.0.0/16")
					networkConfig2, err := util.NewNetInfo(netconf2.netconf())
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeNM.Lock()
					fakeNM.PrimaryNetworks[namespace1.Name] = networkConfig2
					fakeNM.Unlock()
					fakeNM.TriggerHandlers(netconf.nadName, networkConfig2, false)
					expectedDatabaseState2 := getEFExpectedDbUDN(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow, networkConfig2.GetNetworkName())
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState2))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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
					egressFirewall.ResourceVersion = "1"
					egressFirewall1.ResourceVersion = "2"

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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

				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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

					startOvnWithNodes(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]corev1.Node{
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
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					ginkgo.By("Updating a node to not match nodeSelector on Egress Firewall")
					patch.Metadata = map[string]interface{}{"labels": map[string]string{labelKey: libovsdbutil.UnspecifiedL4Match}}
					patchData, err = json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					// trigger update event
					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Patch(context.TODO(), nodeName,
						types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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

				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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

					startOvnWithNodes(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]corev1.Node{
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
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					ginkgo.By("Deleting a node")
					// trigger delete event
					err = fakeOVN.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName,
						metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}

				err = app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries deleting an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")

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

					startOvnWithNodes(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
						[]corev1.Node{
							{
								Status: corev1.NodeStatus{
									Phase: corev1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node1Name, ""),
							},
							{
								Status: corev1.NodeStatus{
									Phase: corev1.NodeRunning,
								},
								ObjectMeta: newObjectMeta(node2Name, ""),
							},
						}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "((tcp && ( tcp.dst == 100 )))", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
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
					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
					defer cancel()
					resetNBClient(connCtx, fakeOVN.controller.nbClient)

					expectedDatabaseState = getEFExpectedDbAfterDelete(expectedDatabaseState)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly retries adding and updating an egressfirewall, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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
					egressFirewall.ResourceVersion = "1"
					egressFirewall1.ResourceVersion = "2"

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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

					connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
					defer cancel()
					ginkgo.By("bringing up NBDB and requesting retry of entry")
					resetNBClient(connCtx, fakeOVN.controller.nbClient)

					expectedDatabaseState = getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall's ACL logging, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.4/23)", "", nbdb.ACLActionAllow)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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
					acl.Meter = ptr.To(t.OvnACLLoggingMeter)
					acl.Severity = &logSeverity
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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
					app.Action = func(*cli.Context) error {
						labelKey := "name"
						labelValue := "test"
						selector := metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}}
						namespace1 := *ovntest.NewNamespace("namespace1")
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

						startOvnWithNodes(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall},
							[]corev1.Node{
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
						gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

						return nil
					}

					err := app.Run([]string{app.Name})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})
			}
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall with subnet exclusion, gateway mode %s", gwMode), func() {
				config.Gateway.Mode = gwMode
				app.Action = func(*cli.Context) error {
					clusterSubnetStr := "10.128.0.0/14"
					_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
					config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

					namespace1 := *ovntest.NewNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					})
					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedDatabaseState := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 0.0.0.0/0 && ip4.dst != "+clusterSubnetStr+")", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall for namespace name > 43 symbols, gateway mode %s", gwMode), func() {
				app.Action = func(*cli.Context) error {
					// 52 characters namespace
					namespace1 := *ovntest.NewNamespace("abcdefghigklmnopqrstuvwxyzabcdefghigklmnopqrstuvwxyz")
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

					startOvn(dbSetup, []corev1.Namespace{namespace1}, []egressfirewallapi.EgressFirewall{*egressFirewall}, true)

					dbWith1ACL := getEFExpectedDb(initialData, fakeOVN, namespace1.Name,
						"(ip4.dst == 1.2.3.5/23)", "", nbdb.ACLActionAllow)

					pg := dbWith1ACL[len(dbWith1ACL)-1].(*nbdb.PortGroup)
					aclIDs2 := fakeOVN.controller.efController.GetEgressFirewallACLDbIDs(egressFirewall.Namespace, 1)
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
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ACL should be removed from the port group egfw is deleted
					expectedDatabaseState = getEFExpectedDbAfterDelete(dbWith1ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

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
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
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
					ginkgo.By("creating Egress Firewall object without namespace should trigger failure")
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
					gomega.Eventually(func(g gomega.Gomega) {
						ef, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().
							EgressFirewalls(egressFirewall.Namespace).
							Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(ef.Status.Messages).To(gomega.ContainElement(gomega.ContainSubstring(t.EgressFirewallErrorMsg)),
							fmt.Sprintf("EgressFirewall should remain in error state, got: %+v", ef.Status.Messages))
					}).WithTimeout(1 * time.Second).WithPolling(200 * time.Millisecond).Should(gomega.Succeed())
					gomega.Consistently(func(g gomega.Gomega) {
						ef, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().
							EgressFirewalls(egressFirewall.Namespace).
							Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(ef.Status.Messages).To(gomega.ContainElement(gomega.ContainSubstring(t.EgressFirewallErrorMsg)),
							fmt.Sprintf("EgressFirewall should remain in error state, got: %+v", ef.Status.Messages))
					}).WithTimeout(2 * time.Second).WithPolling(200 * time.Millisecond).Should(gomega.Succeed())

					ginkgo.By("creating namespace object should create egress firewall and DNS address set")

					_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.Background(), &namespace1,
						metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func(g gomega.Gomega) {
						ef, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().
							EgressFirewalls(egressFirewall.Namespace).
							Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(ef.Status.Messages).To(gomega.ContainElement(gomega.ContainSubstring(efcontroller.EgressFirewallAppliedCorrectly)),
							fmt.Sprintf("EgressFirewall should remain in error state, got: %+v", ef.Status.Messages))
					}).WithTimeout(2 * time.Second).WithPolling(200 * time.Millisecond).Should(gomega.Succeed())

					dnsNameForAddrSet := dnsName
					if !oldDNS {
						dnsNameForAddrSet = dnsNameLowerCaseFQDN
					}
					// check dns address set was created
					addrSet, _ := addressset.GetTestDbAddrSets(
						dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs(dnsNameForAddrSet, fakeOVN.controller.controllerName),
						[]string{resolvedIP})
					addrSetUUID := strings.TrimSuffix(addrSet.UUID, "-UUID")
					expectedDatabaseState := getEFExpectedDb(append(initialData, addrSet), fakeOVN, namespace1.Name, "(ip4.dst == $"+addrSetUUID+")", "", nbdb.ACLActionDrop)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))

					ginkgo.By("deleting egress firewall, DNS, and namespace")
					// delete failed object
					err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
						Delete(context.TODO(), egressFirewall.Name, metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					if !oldDNS {
						// delete the dns name resolver object.
						err = fakeOVN.fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
							Delete(context.TODO(), "dns-default", metav1.DeleteOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}

					err = fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.Background(), namespace1.Name,
						metav1.DeleteOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("NBDB should be in a clean state")
					// check dns address set is cleaned up on delete
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(initialData))
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
				app.Action = func(*cli.Context) error {
					namespace1 := *ovntest.NewNamespace("namespace1")
					dnsNameLowerCaseFQDN := util.LowerCaseFQDN(dnsName)
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: dnsName,
							},
						},
					})
					startOvn(dbSetup, []corev1.Namespace{namespace1}, nil, oldDNS)

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
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(addrSetDbState))

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
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdb.HaveData(expectedDatabaseState))
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
