package ovn

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ipMode struct {
	IPv4Mode bool
	IPv6Mode bool
}

// FIXME DUAL-STACK: FakeOVN doesn't really support adding more than one
// pod to the namespace. All logical ports would share the same fakeUUID.
// When this is addressed we can add an entry for
// IPv4Mode = true, IPv6Mode = true.
func getIpModes() []ipMode {
	return []ipMode{
		{true, false},
		{false, true},
	}
}

func ipModeStr(m ipMode) string {
	if m.IPv4Mode && m.IPv6Mode {
		return "dualstack"
	} else if m.IPv4Mode {
		return "ipv4"
	} else if m.IPv6Mode {
		return "ipv6"
	} else {
		return "no IP mode set"
	}
}

func setIpMode(m ipMode) {
	config.IPv4Mode = m.IPv4Mode
	config.IPv6Mode = m.IPv6Mode
}

func getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) []libovsdb.TestData {
	match := getMulticastACLMatch()
	aclIDs := getDefaultMcastACLDbIDs(mcastDefaultDenyID, libovsdbutil.ACLEgress, DefaultNetworkControllerName)
	aclName := libovsdbutil.GetACLName(aclIDs)
	defaultDenyEgressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastDenyPriority,
		match,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	defaultDenyEgressACL.UUID = "defaultDenyEgressACL_UUID"

	aclIDs = getDefaultMcastACLDbIDs(mcastDefaultDenyID, libovsdbutil.ACLIngress, DefaultNetworkControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	defaultDenyIngressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastDenyPriority,
		match,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	defaultDenyIngressACL.UUID = "defaultDenyIngressACL_UUID"
	clusterPortGroup.ACLs = []string{defaultDenyEgressACL.UUID, defaultDenyIngressACL.UUID}

	aclIDs = getDefaultMcastACLDbIDs(mcastAllowInterNodeID, libovsdbutil.ACLEgress, DefaultNetworkControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	egressMatch := libovsdbutil.GetACLMatch(clusterRtrPortGroup.Name, match, libovsdbutil.ACLEgress)
	defaultAllowEgressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastAllowPriority,
		egressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	defaultAllowEgressACL.UUID = "defaultAllowEgressACL_UUID"

	aclIDs = getDefaultMcastACLDbIDs(mcastAllowInterNodeID, libovsdbutil.ACLIngress, DefaultNetworkControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	ingressMatch := libovsdbutil.GetACLMatch(clusterRtrPortGroup.Name, match, libovsdbutil.ACLIngress)
	defaultAllowIngressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastAllowPriority,
		ingressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	defaultAllowIngressACL.UUID = "defaultAllowIngressACL_UUID"
	clusterRtrPortGroup.ACLs = []string{defaultAllowEgressACL.UUID, defaultAllowIngressACL.UUID}
	return []libovsdb.TestData{
		defaultDenyIngressACL,
		defaultDenyEgressACL,
		defaultAllowEgressACL,
		defaultAllowIngressACL,
		clusterPortGroup,
		clusterRtrPortGroup,
	}
}

func getMulticastDefaultStaleData(clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) []libovsdb.TestData {
	testData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
	defaultDenyIngressACL := testData[0].(*nbdb.ACL)
	newName := libovsdbutil.JoinACLName(types.ClusterPortGroupNameBase, "DefaultDenyMulticastIngress")
	defaultDenyIngressACL.Name = &newName
	defaultDenyIngressACL.Options = nil

	defaultDenyEgressACL := testData[1].(*nbdb.ACL)
	newName1 := libovsdbutil.JoinACLName(types.ClusterPortGroupNameBase, "DefaultDenyMulticastEgress")
	defaultDenyEgressACL.Name = &newName1
	defaultDenyEgressACL.Options = nil

	defaultAllowEgressACL := testData[2].(*nbdb.ACL)
	newName2 := libovsdbutil.JoinACLName(types.ClusterRtrPortGroupNameBase, "DefaultAllowMulticastEgress")
	defaultAllowEgressACL.Name = &newName2
	defaultAllowEgressACL.Options = nil

	defaultAllowIngressACL := testData[3].(*nbdb.ACL)
	newName3 := libovsdbutil.JoinACLName(types.ClusterRtrPortGroupNameBase, "DefaultAllowMulticastIngress")
	defaultAllowIngressACL.Name = &newName3
	defaultAllowIngressACL.Options = nil

	return []libovsdb.TestData{
		defaultDenyIngressACL,
		defaultDenyEgressACL,
		defaultAllowEgressACL,
		defaultAllowIngressACL,
		testData[4],
		testData[5],
	}
}

func getDefaultPortGroups() (clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) {
	clusterPortGroup = newClusterPortGroup()
	clusterRtrPortGroup = newRouterPortGroup()
	return
}

func getMulticastPolicyExpectedData(ns string, ports []string) []libovsdb.TestData {
	fakeController := getFakeController(DefaultNetworkControllerName)
	pg_hash := fakeController.getNamespacePortGroupName(ns)
	egressMatch := libovsdbutil.GetACLMatch(pg_hash, fakeController.getMulticastACLEgrMatch(), libovsdbutil.ACLEgress)

	ip4AddressSet, ip6AddressSet := getNsAddrSetHashNames(ns)
	mcastMatch := getACLMatchAF(getMulticastACLIgrMatchV4(ip4AddressSet), getMulticastACLIgrMatchV6(ip6AddressSet), config.IPv4Mode, config.IPv6Mode)
	ingressMatch := libovsdbutil.GetACLMatch(pg_hash, mcastMatch, libovsdbutil.ACLIngress)

	aclIDs := getNamespaceMcastACLDbIDs(ns, libovsdbutil.ACLEgress, DefaultNetworkControllerName)
	aclName := libovsdbutil.GetACLName(aclIDs)
	egressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultMcastAllowPriority,
		egressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	egressACL.UUID = ns + "mc-egress-UUID"

	aclIDs = getNamespaceMcastACLDbIDs(ns, libovsdbutil.ACLIngress, DefaultNetworkControllerName)
	aclName = libovsdbutil.GetACLName(aclIDs)
	ingressACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultMcastAllowPriority,
		ingressMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	ingressACL.UUID = ns + "mc-ingress-UUID"

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	pgDbIDs := getNamespacePortGroupDbIDs(ns, fakeController.controllerName)
	pg := libovsdbutil.BuildPortGroup(
		pgDbIDs,
		lsps,
		[]*nbdb.ACL{egressACL, ingressACL},
	)
	pg.UUID = pg.Name + "-UUID"

	return []libovsdb.TestData{
		egressACL,
		ingressACL,
		pg,
	}
}

func getNamespacePG(ns, controllerName string) *nbdb.PortGroup {
	pgDbIDs := getNamespacePortGroupDbIDs(ns, controllerName)
	pg := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getMulticastPolicyStaleData(ns string, ports []string) []libovsdb.TestData {
	testData := getMulticastPolicyExpectedData(ns, ports)

	egressACL := testData[0].(*nbdb.ACL)
	newName := libovsdbutil.JoinACLName(ns, "MulticastAllowEgress")
	egressACL.Name = &newName
	egressACL.Options = nil

	ingressACL := testData[1].(*nbdb.ACL)
	newName1 := libovsdbutil.JoinACLName(ns, "MulticastAllowIngress")
	ingressACL.Name = &newName1
	ingressACL.Options = nil

	return []libovsdb.TestData{
		egressACL,
		ingressACL,
		testData[2],
	}
}

func getNodeSwitch(nodeName string) []libovsdb.TestData {
	return []libovsdb.TestData{
		&nbdb.LogicalSwitch{
			UUID: nodeName + "_UUID",
			Name: nodeName,
		},
	}
}

func createTestPods(nodeName, namespace string, ipM ipMode) (pods []v1.Pod, tPods []testPod, tPodIPs []string) {
	nPodTestV4 := newTPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod1",
		"10.128.1.3",
		"0a:58:0a:80:01:03",
		namespace,
	)
	nPodTestV6 := newTPod(
		nodeName,
		"fd00:10:244::/64",
		"fd00:10:244::2",
		"fd00:10:244::1",
		"myPod2",
		"fd00:10:244::3",
		"0a:58:dd:33:05:d8",
		namespace,
	)
	if ipM.IPv4Mode {
		tPods = append(tPods, nPodTestV4)
		tPodIPs = append(tPodIPs, nPodTestV4.podIP)
	}
	if ipM.IPv6Mode {
		tPods = append(tPods, nPodTestV6)
		tPodIPs = append(tPodIPs, nPodTestV6.podIP)
	}
	for _, tPod := range tPods {
		pods = append(pods, *newPod(tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP))
	}
	return
}

func updateMulticast(fakeOvn *FakeOVN, ns *v1.Namespace, enable bool) {
	if enable {
		ns.Annotations[util.NsMulticastAnnotation] = "true"
	} else {
		ns.Annotations[util.NsMulticastAnnotation] = "false"
	}
	_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), ns, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

var _ = ginkgo.Describe("OVN Multicast with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
		nodeName       = "node1"
	)
	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.EnableMulticast = true

		app = cli.NewApp()
		app.Name = "test"
		// flags are written to config.EnableMulticast
		// if there is no --enable-multicast flag, it will set to false.
		// alternative approach is to give this flag to app.Run, but that require more changes.
		//app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("creates default Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						clusterPortGroup,
						clusterRtrPortGroup,
					},
				})
				// this is "if oc.multicastSupport" part of SetupMaster
				err := fakeOvn.controller.createDefaultDenyMulticastPolicy()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.createDefaultAllowMulticastPolicy()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("updates stale default Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				// start with stale ACLs
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: getMulticastDefaultStaleData(clusterPortGroup, clusterRtrPortGroup),
				})
				// this is "if oc.multicastSupport" part of SetupMaster
				err := fakeOvn.controller.createDefaultDenyMulticastPolicy()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.createDefaultAllowMulticastPolicy()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check acls are updated
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("cleans up Multicast resources when multicast is disabled", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				initialData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)

				nsData := getMulticastPolicyExpectedData(namespaceName1, nil)
				initialData = append(initialData, nsData...)
				// namespace is still present, but multicast support is disabled
				namespace1 := *newNamespace(namespaceName1)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				// this "if !oc.multicastSupport" part of SetupMaster
				err := fakeOvn.controller.disableMulticast()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check acls are deleted when multicast is disabled
				clusterPortGroup, clusterRtrPortGroup = getDefaultPortGroups()
				namespacePortGroup := getNamespacePG(namespaceName1, fakeOvn.controller.controllerName)
				expectedData := []libovsdb.TestData{
					clusterPortGroup,
					clusterRtrPortGroup,
					namespacePortGroup,
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("creates namespace Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				expectedData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
				// namespace exists, but multicast acls do not
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = append(expectedData, getMulticastPolicyExpectedData(namespaceName1, nil)...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("updates stale namespace Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				// start with stale ACLs for existing namespace
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				expectedData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
				expectedData = append(expectedData, getMulticastPolicyStaleData(namespaceName1, nil)...)
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
				expectedData = append(expectedData, getMulticastPolicyExpectedData(namespaceName1, nil)...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("cleans up namespace Multicast ACLs when multicast is disabled for namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				// start with stale ACLs
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
				defaultMulticastData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
				namespaceMulticastData := getMulticastPolicyExpectedData(namespaceName1, nil)
				namespace1 := *newNamespace(namespaceName1)

				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: append(defaultMulticastData, namespaceMulticastData...)},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// only namespaced acls should be dereferenced, default acls will stay
				namespacePortGroup := getNamespacePG(namespaceName1, fakeOvn.controller.controllerName)
				expectedData := append(defaultMulticastData, namespacePortGroup)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("during execution", func() {
		for _, m := range getIpModes() {
			m := m
			ginkgo.It("tests enabling/disabling multicast in a namespace "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)
					setIpMode(m)

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Multicast is denied by default.
					_, ok := ns.Annotations[util.NsMulticastAnnotation]
					gomega.Expect(ok).To(gomega.BeFalse())

					// Enable multicast in the namespace.
					updateMulticast(fakeOvn, ns, true)
					expectedData := getMulticastPolicyExpectedData(namespace1.Name, nil)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

					// Disable multicast in the namespace.
					updateMulticast(fakeOvn, ns, false)

					namespacePortGroup := getNamespacePG(namespaceName1, fakeOvn.controller.controllerName)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(namespacePortGroup))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("tests enabling multicast in a namespace with a pod "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)
					pods, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, m)

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								*newNode("node1", "192.168.126.202/24"),
							},
						},
						&v1.PodList{
							Items: pods,
						},
					)
					setIpMode(m)

					for _, tPod := range tPods {
						tPod.populateLogicalSwitchCache(fakeOvn)
					}

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace
					updateMulticast(fakeOvn, ns, true)
					// calculate expected data
					ports := []string{}
					for _, tPod := range tPods {
						ports = append(ports, tPod.portUUID)
					}
					expectedData := getMulticastPolicyExpectedData(namespace1.Name, ports)
					expectedData = append(expectedData, getDefaultNetExpectedPodsAndSwitches(tPods, []string{nodeName})...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
					fakeOvn.asf.ExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("tests enabling multicast in multiple namespaces with a long name > 42 characters "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					longNameSpace1Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
					namespace1 := *newNamespace(longNameSpace1Name)
					longNameSpace2Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijl" // create with 63 characters
					namespace2 := *newNamespace(longNameSpace2Name)

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
								namespace2,
							},
						},
					)
					setIpMode(m)

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()
					ns1, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns1).NotTo(gomega.BeNil())
					ns2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace2.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns2).NotTo(gomega.BeNil())

					portsns1 := []string{}
					expectedData := getMulticastPolicyExpectedData(longNameSpace1Name, portsns1)
					acl := expectedData[0].(*nbdb.ACL)
					// Post ACL indexing work, multicast ACL's don't have names
					// We use externalIDs instead; so we can check if the expected IDs exist for the long namespace so that
					// isEquivalent logic will be correct
					gomega.Expect(acl.Name).To(gomega.BeNil())
					gomega.Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(gomega.Equal(longNameSpace1Name))
					expectedData = append(expectedData, getMulticastPolicyExpectedData(longNameSpace2Name, nil)...)
					acl = expectedData[3].(*nbdb.ACL)
					gomega.Expect(acl.Name).To(gomega.BeNil())
					gomega.Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(gomega.Equal(longNameSpace2Name))
					expectedData = append(expectedData, getDefaultNetExpectedPodsAndSwitches([]testPod{}, []string{"node1"})...)
					// Enable multicast in the namespace.
					updateMulticast(fakeOvn, ns1, true)
					updateMulticast(fakeOvn, ns2, true)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("tests adding a pod to a multicast enabled namespace "+ipModeStr(m), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace(namespaceName1)
					_, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, m)

					ports := []string{}
					for _, pod := range tPods {
						ports = append(ports, pod.portUUID)
					}

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								*newNode("node1", "192.168.126.202/24"),
							},
						},
					)
					setIpMode(m)

					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ns).NotTo(gomega.BeNil())

					// Enable multicast in the namespace.
					updateMulticast(fakeOvn, ns, true)
					// Check expected data without pods
					expectedDataWithoutPods := getMulticastPolicyExpectedData(namespace1.Name, nil)
					expectedDataWithoutPods = append(expectedDataWithoutPods, getNodeSwitch(nodeName)...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

					// Create pods
					for _, tPod := range tPods {
						tPod.populateLogicalSwitchCache(fakeOvn)
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), newPod(
							tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					// Check pods were added
					fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
					expectedDataWithPods := getMulticastPolicyExpectedData(namespace1.Name, ports)
					expectedDataWithPods = append(expectedDataWithPods, getDefaultNetExpectedPodsAndSwitches(tPods, []string{nodeName})...)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPods...))

					// Delete the pod from the namespace.
					for _, tPod := range tPods {
						err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
							tPod.podName, *metav1.NewDeleteOptions(0))
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespace1.Name)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		}
	})
})
