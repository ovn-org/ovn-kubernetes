package ovn

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	kvv1 "kubevirt.io/api/core/v1"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/utils/pointer"

	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("OVN Kubevirt Operations", func() {
	const (
		node1             = "node1"
		node2             = "node2"
		dhcpv4OptionsUUID = "dhcpv4"
		dhcpv6OptionsUUID = "dhcpv6"
	)
	type testDHCPOptions struct {
		cidr     string
		dns      string
		router   string
		hostname string
	}
	type testPolicy struct {
		match   string
		nexthop string
		vmName  string
		uuid    string
	}
	type testStaticRoute struct {
		prefix     string
		nexthop    string
		outputPort string
		vmName     string
		uuid       string
	}
	type testVirtLauncherPod struct {
		testPod
		vmName      string
		labels      map[string]string
		annotations map[string]string
	}
	type testMigrationTarget struct {
		testVirtLauncherPod
		lrpNetworks []string
	}
	type testData struct {
		testVirtLauncherPod
		migrationTarget      testMigrationTarget
		dnsServiceIPs        []string
		lrpNetworks          []string
		policies             []testPolicy
		staticRoutes         []testStaticRoute
		expectedDhcpv4       []testDHCPOptions
		expectedDhcpv6       []testDHCPOptions
		expectedPolicies     []testPolicy
		expectedStaticRoutes []testStaticRoute
	}
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdb.TestSetup

		logicalSwitch                            *nbdb.LogicalSwitch
		ovnClusterRouter                         *nbdb.LogicalRouter
		logicalRouterPort                        *nbdb.LogicalRouterPort
		migrationSourceLSRP, migrationTargetLSRP *nbdb.LogicalSwitchPort
		externalIDs                              = func(namespace, vmName string) map[string]string {
			return map[string]string{
				kubevirt.NamespaceExternalIDKey: namespace,
				kvv1.VirtualMachineNameLabel:    vmName,
			}
		}

		newVirtLauncherTPod = func(annotations, labels map[string]string, vmName, nodeName, nodeSubnet, podName, podIP, podMAC, namespace string) testVirtLauncherPod {
			return testVirtLauncherPod{
				testPod:     newTPod(nodeName, nodeSubnet, "", kubevirt.ARPProxyIPv4, podName, podIP, podMAC, namespace),
				annotations: annotations,
				labels:      labels,
				vmName:      vmName,
			}
		}

		completeVirtLauncherTPodFromMigrationTarget = func(t testData) *testVirtLauncherPod {
			if t.migrationTarget.nodeName == "" {
				return nil
			}
			migrationTargetVirtLauncherTPod := newVirtLauncherTPod(
				t.migrationTarget.annotations,
				t.migrationTarget.labels,
				t.vmName,
				t.migrationTarget.nodeName,
				t.migrationTarget.nodeSubnet,
				t.migrationTarget.podName,
				t.podIP,
				t.podMAC,
				t.namespace,
			)
			return &migrationTargetVirtLauncherTPod
		}

		findDHCPOptionsWithHostname = func(dhcpOptions []testDHCPOptions, hostname string) *testDHCPOptions {
			if dhcpOptions == nil {
				return nil
			}
			for _, d := range dhcpOptions {
				if d.hostname == hostname {
					return &d
				}
			}
			return nil
		}

		kubevirtOVNTestData = func(t testData, previousData []libovsdbtest.TestData) []libovsdbtest.TestData {
			testVirtLauncherPods := []testVirtLauncherPod{t.testVirtLauncherPod}
			testPods := []testPod{t.testPod}
			nodeSet := map[string]bool{t.testPod.nodeName: true}
			migrationTargetVirtLauncherTPod := completeVirtLauncherTPodFromMigrationTarget(t)
			if migrationTargetVirtLauncherTPod != nil {
				testVirtLauncherPods = append(testVirtLauncherPods, *migrationTargetVirtLauncherTPod)
				testPods = append(testPods, migrationTargetVirtLauncherTPod.testPod)
				nodeSet[migrationTargetVirtLauncherTPod.nodeName] = true
			}

			nodes := []string{}
			for node := range nodeSet {
				nodes = append(nodes, node)
			}
			data := getExpectedDataPodsAndSwitches(testPods, nodes)
			for _, d := range data {
				lsp, ok := d.(*nbdb.LogicalSwitchPort)
				if ok {
					for _, p := range testVirtLauncherPods {
						portName := util.GetLogicalPortName(p.namespace, p.podName)
						if lsp.Name == portName {
							if findDHCPOptionsWithHostname(t.expectedDhcpv4, p.vmName) != nil {
								lsp.Dhcpv4Options = pointer.String(dhcpv4OptionsUUID + p.vmName)
							}
							if findDHCPOptionsWithHostname(t.expectedDhcpv6, p.vmName) != nil {
								lsp.Dhcpv6Options = pointer.String(dhcpv6OptionsUUID + p.vmName)
							}
						}
					}
				}
			}
			// Prepend previous data
			return append(previousData, data...)
		}

		newPodFromTestVirtLauncherPod = func(t testVirtLauncherPod) *corev1.Pod {
			pod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
			pod.Annotations = t.annotations
			pod.Labels = t.labels
			return pod
		}
		addOVNPodAnnotations = func(pod *kapi.Pod, t testVirtLauncherPod) {
			pod.Annotations = map[string]string{}
			setPodAnnotations(pod, t.testPod)
			for k, v := range t.annotations {
				pod.Annotations[k] = v
			}
		}
		expectedDHCPv4Options = func(uuid, namespace string, t *testDHCPOptions) *nbdb.DHCPOptions {
			dhcpOptions := kubevirt.ComposeDHCPv4Options(
				t.cidr,
				t.router,
				t.dns,
				t.hostname)
			dhcpOptions.UUID = uuid
			dhcpOptions.ExternalIDs = map[string]string{
				types.PrimaryIDKey:                     fmt.Sprintf("%s:%s:%s:%s", DefaultNetworkControllerName, libovsdbops.VirtualMachineOwnerType, t.cidr, t.hostname),
				string(libovsdbops.OwnerControllerKey): DefaultNetworkControllerName,
				string(libovsdbops.OwnerTypeKey):       string(libovsdbops.VirtualMachineOwnerType),
				string(libovsdbops.ObjectNameKey):      t.cidr,
				string(libovsdbops.HostnameIndex):      t.hostname,
				kvv1.VirtualMachineNameLabel:           t.hostname,
				kubevirt.NamespaceExternalIDKey:        namespace,
			}

			return dhcpOptions
		}
		expectedDHCPv6Options = func(uuid, namespace string, t *testDHCPOptions) *nbdb.DHCPOptions {
			dhcpOptions := kubevirt.ComposeDHCPv6Options(
				t.cidr,
				t.router,
				t.dns)
			dhcpOptions.UUID = uuid
			dhcpOptions.ExternalIDs = map[string]string{
				types.PrimaryIDKey:                     fmt.Sprintf("%s:%s:%s:%s", DefaultNetworkControllerName, libovsdbops.VirtualMachineOwnerType, t.cidr, t.hostname),
				string(libovsdbops.OwnerControllerKey): DefaultNetworkControllerName,
				string(libovsdbops.OwnerTypeKey):       string(libovsdbops.VirtualMachineOwnerType),
				string(libovsdbops.ObjectNameKey):      t.cidr,
				string(libovsdbops.HostnameIndex):      t.hostname,
				kvv1.VirtualMachineNameLabel:           t.hostname,
				kubevirt.NamespaceExternalIDKey:        namespace,
			}

			return dhcpOptions
		}
		composePolicy = func(uuid string, p testPolicy, t testData) *nbdb.LogicalRouterPolicy {
			vmName := t.vmName
			if p.vmName != "" {
				vmName = p.vmName
			}
			return &nbdb.LogicalRouterPolicy{
				UUID:        uuid,
				Match:       p.match,
				Action:      nbdb.LogicalRouterPolicyActionReroute,
				Nexthops:    []string{p.nexthop},
				ExternalIDs: externalIDs(t.namespace, vmName),
				Priority:    1,
			}
		}
		composeStaticRoute = func(uuid string, r testStaticRoute, t testData) *nbdb.LogicalRouterStaticRoute {
			vmName := t.vmName
			if r.vmName != "" {
				vmName = r.vmName
			}
			return &nbdb.LogicalRouterStaticRoute{
				UUID:        uuid,
				IPPrefix:    r.prefix,
				Nexthop:     r.nexthop,
				Policy:      &nbdb.LogicalRouterStaticRoutePolicyDstIP,
				OutputPort:  &r.outputPort,
				ExternalIDs: externalIDs(t.namespace, vmName),
			}
		}

		initialNBDBWithoutPoliciesAndStaticRoutes = func() []libovsdb.TestData {
			data := []libovsdb.TestData{}
			for _, nbData := range initialDB.NBData {
				if _, ok := nbData.(*nbdb.LogicalRouterPolicy); ok {
					continue
				}
				if _, ok := nbData.(*nbdb.LogicalRouterStaticRoute); ok {
					continue
				}
				if lr, ok := nbData.(*nbdb.LogicalRouter); ok {
					lr = lr.DeepCopy()
					lr.Policies = nil
					lr.StaticRoutes = nil
					nbData = lr
				}

				data = append(data, nbData)
			}
			return data
		}
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("during execution", func() {
		DescribeTable("reconcile migratable vm pods", func(t testData) {

			ovnClusterRouter = &nbdb.LogicalRouter{
				Name: ovntypes.OVNClusterRouter,
				UUID: ovntypes.OVNClusterRouter + "-UUID",
			}
			logicalRouterPort = &nbdb.LogicalRouterPort{
				UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.nodeName + "-UUID",
				Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.nodeName,
				Networks: t.lrpNetworks,
			}
			migrationSourceLSRP = &nbdb.LogicalSwitchPort{
				Name: ovntypes.SwitchToRouterPrefix + t.nodeName,
				UUID: ovntypes.SwitchToRouterPrefix + t.nodeName + "-UUID",
				Type: "router",
				Options: map[string]string{
					"router-port": logicalRouterPort.Name,
					"arp_proxy":   kubevirt.ComposeARPProxyLSPOption(),
				},
			}
			logicalSwitch = &nbdb.LogicalSwitch{
				Name: t.nodeName,
				UUID: t.nodeName + "-UUID",
			}
			var migrationTargetLRP *nbdb.LogicalRouterPort
			var migrationTargetLS *nbdb.LogicalSwitch

			initialDB = libovsdb.TestSetup{NBData: []libovsdb.TestData{}}
			for i, p := range t.policies {
				policy := composePolicy(fmt.Sprintf("policy%d", i), p, t)
				ovnClusterRouter.Policies = append(ovnClusterRouter.Policies, policy.UUID)
				initialDB.NBData = append(initialDB.NBData, policy)
			}

			for i, r := range t.staticRoutes {
				staticRoute := composeStaticRoute(fmt.Sprintf("route%d", i), r, t)
				ovnClusterRouter.StaticRoutes = append(ovnClusterRouter.StaticRoutes, staticRoute.UUID)
				initialDB.NBData = append(initialDB.NBData, staticRoute)
			}

			initialDB.NBData = append(initialDB.NBData,
				logicalSwitch,
				ovnClusterRouter,
				logicalRouterPort,
				migrationSourceLSRP)

			migrationTargetVirtLauncherTPod := completeVirtLauncherTPodFromMigrationTarget(t)
			if migrationTargetVirtLauncherTPod != nil {
				migrationTargetLRP = &nbdb.LogicalRouterPort{
					UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + migrationTargetVirtLauncherTPod.nodeName + "-UUID",
					Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + migrationTargetVirtLauncherTPod.nodeName,
					Networks: t.migrationTarget.lrpNetworks,
				}
				migrationTargetLSRP = &nbdb.LogicalSwitchPort{
					Name: ovntypes.SwitchToRouterPrefix + migrationTargetVirtLauncherTPod.nodeName,
					UUID: ovntypes.SwitchToRouterPrefix + migrationTargetVirtLauncherTPod.nodeName + "-UUID",
					Type: "router",
					Options: map[string]string{
						"router-port": migrationTargetLRP.Name,
						"arp_proxy":   kubevirt.ComposeARPProxyLSPOption(),
					},
				}
				migrationTargetLS = &nbdb.LogicalSwitch{
					Name: migrationTargetVirtLauncherTPod.nodeName,
					UUID: migrationTargetVirtLauncherTPod.nodeName + "-UUID",
				}
				initialDB.NBData = append(initialDB.NBData,
					migrationTargetLRP,
					migrationTargetLSRP,
					migrationTargetLS,
				)
			}
			pods := []v1.Pod{}
			sourcePod := newPodFromTestVirtLauncherPod(t.testVirtLauncherPod)
			if migrationTargetVirtLauncherTPod != nil {
				addOVNPodAnnotations(sourcePod, t.testVirtLauncherPod)
				pods = append(pods, *sourcePod)
			}
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(t.namespace),
						},
					},
					&v1.PodList{
						Items: pods,
					},
					&corev1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "kube-system",
							Name:      "kube-dns",
						},
						Spec: corev1.ServiceSpec{
							ClusterIPs: t.dnsServiceIPs,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node1,
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node2,
								},
							},
						},
					},
				)

				t.testPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, t.nodeName))
				if migrationTargetVirtLauncherTPod != nil {
					migrationTargetVirtLauncherTPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, migrationTargetVirtLauncherTPod.nodeName))
				}
				err := fakeOvn.controller.WatchNamespaces()
				Expect(err).NotTo(HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				Expect(err).NotTo(HaveOccurred())
				podToCreate := sourcePod
				if migrationTargetVirtLauncherTPod != nil {
					podToCreate = newPodFromTestVirtLauncherPod(*migrationTargetVirtLauncherTPod)
					podToCreate.Labels = t.migrationTarget.labels
					podToCreate.Annotations = t.migrationTarget.annotations
				}
				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), podToCreate.Name, metav1.GetOptions{})
				Expect(pod).To(BeNil())

				podToCreate.CreationTimestamp = metav1.NewTime(time.Now())
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), podToCreate, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				expectedOVN := []libovsdbtest.TestData{}
				ovnClusterRouter.Policies = []string{}
				expectedOVNClusterRouter := ovnClusterRouter.DeepCopy()
				expectedOVNClusterRouter.Policies = []string{}
				for i, p := range t.expectedPolicies {
					uuid := fmt.Sprintf("policy%d", i)
					if p.uuid != "" {
						uuid = p.uuid
					}
					expectedPolicy := composePolicy(uuid, p, t)
					expectedOVNClusterRouter.Policies = append(expectedOVNClusterRouter.Policies, expectedPolicy.UUID)
					expectedOVN = append(expectedOVN, expectedPolicy)
				}
				expectedOVNClusterRouter.StaticRoutes = []string{}
				for i, r := range t.expectedStaticRoutes {
					uuid := fmt.Sprintf("route%d", i)
					if r.uuid != "" {
						uuid = r.uuid
					}
					expectedStaticRoute := composeStaticRoute(uuid, r, t)
					expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, expectedStaticRoute.UUID)
					expectedOVN = append(expectedOVN, expectedStaticRoute)
				}
				for _, d := range t.expectedDhcpv4 {
					expectedOVN = append(expectedOVN, expectedDHCPv4Options(dhcpv4OptionsUUID+d.hostname, t.namespace, &d))
				}

				for _, d := range t.expectedDhcpv6 {
					expectedOVN = append(expectedOVN, expectedDHCPv6Options(dhcpv6OptionsUUID+d.hostname, t.namespace, &d))
				}
				expectedSourceLSRP := migrationSourceLSRP.DeepCopy()
				expectedOVN = append(expectedOVN,
					expectedOVNClusterRouter,
					logicalRouterPort,
					expectedSourceLSRP,
				)
				expectedOVN = kubevirtOVNTestData(t, expectedOVN)

				if migrationTargetVirtLauncherTPod != nil {
					expectedTargetLSRP := migrationTargetLSRP.DeepCopy()
					expectedOVN = append(expectedOVN,
						migrationTargetLRP,
						expectedTargetLSRP,
					)
				}
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedOVN), "should populate ovn")

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())

				if migrationTargetVirtLauncherTPod != nil {
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), migrationTargetVirtLauncherTPod.podName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(initialNBDBWithoutPoliciesAndStaticRoutes()), "should cleanup ovn")

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("for single stack ipv4", testData{
				lrpNetworks:   []string{"100.64.0.4/24"},
				dnsServiceIPs: []string{"10.127.5.3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"10.128.1.0/24",
					"virt-launcher-1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					"namespace1",
				),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.1.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
			}),
			Entry("for single stack ipv6", testData{
				lrpNetworks:   []string{"fd12::4/64"},
				dnsServiceIPs: []string{"fd7b:6b4d:7b25:d22f::3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"fd11::/64",
					"virt-launcher-1",
					"fd11::3",
					"0a:58:c9:c8:3f:1c",
					"namespace1",
				),
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd11::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
			}),
			Entry("for dual stack", testData{
				lrpNetworks:   []string{"100.64.0.4/24", "fd12::4/64"},
				dnsServiceIPs: []string{"10.127.5.3", "fd7b:6b4d:7b25:d22f::3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"10.128.1.0/24 fd11::/64",
					"virt-launcher-1",
					"10.128.1.3 fd11::3",
					"0a:58:0a:80:01:03",
					"namespace1",
				),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.1.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd11::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
			}),
			Entry("for pre-copy live migration", testData{
				lrpNetworks:   []string{"100.64.0.4/24", "fd12::4/64"},
				dnsServiceIPs: []string{"10.127.5.3", "fd7b:6b4d:7b25:d22f::3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"10.128.1.0/24 fd11::/64",
					"virt-launcher-1",
					"10.128.1.3 fd11::3",
					"0a:58:0a:80:01:03",
					"namespace1",
				),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{"100.64.0.5/24", "fd12::5/64"},
					testVirtLauncherPod: testVirtLauncherPod{
						labels: map[string]string{
							kvv1.VirtualMachineNameLabel: "vm1",
							kvv1.NodeNameLabel:           node2,
						},
						annotations: map[string]string{
							kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
						},
						testPod: testPod{
							podName:    "virt-launcher-2",
							nodeName:   node2,
							nodeSubnet: "10.128.2.0/24 fd12::/64",
						},
					},
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.1.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd11::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
				expectedPolicies: []testPolicy{
					{
						match:   "ip4.src == 10.128.1.3",
						nexthop: "100.64.0.5",
					},
					{
						match:   "ip6.src == fd11::3",
						nexthop: "fd12::5",
					},
				},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     "10.128.1.3",
						nexthop:    "10.128.1.3",
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     "fd11::3",
						nexthop:    "fd11::3",
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
			}),
			Entry("for pre-copy live migration to node owning subnet", testData{
				lrpNetworks:   []string{"100.64.0.4/24", "fd12::4/64"},
				dnsServiceIPs: []string{"10.127.5.3", "fd7b:6b4d:7b25:d22f::3"},
				policies: []testPolicy{
					{
						match:   "ip4.src == 10.128.2.3",
						nexthop: "100.64.0.4",
					},
					{
						match:   "ip6.src == fd12::3",
						nexthop: "fd12::4",
					},
				},
				staticRoutes: []testStaticRoute{
					{
						prefix:     "10.128.2.3",
						nexthop:    "10.128.2.3",
						outputPort: ovntypes.RouterToSwitchPrefix + node1,
					},
					{
						prefix:     "fd12::3",
						nexthop:    "fd12::3",
						outputPort: ovntypes.RouterToSwitchPrefix + node1,
					},
				},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel:     "vm1",
						kvv1.NodeNameLabel:               node1,
						kubevirt.OriginalSwitchNameLabel: node2, // was created at node2
					},
					"vm1",
					node1,
					"10.128.1.0/24 fd11::/64",
					"virt-launcher-1",
					"10.128.2.3 fd12::3",
					"0a:58:0a:80:02:03",
					"namespace1",
				),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{"100.64.0.5/24", "fd12::5/64"},
					testVirtLauncherPod: testVirtLauncherPod{
						labels: map[string]string{
							kvv1.VirtualMachineNameLabel: "vm1",
							kvv1.NodeNameLabel:           node2,
						},
						annotations: map[string]string{
							kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
						},
						testPod: testPod{
							podName:    "virt-launcher-2",
							nodeName:   node2,
							nodeSubnet: "10.128.2.0/24 fd12::/64",
						},
					},
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.2.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd12::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
			}),
			Entry("for post-copy live migration", testData{
				lrpNetworks:   []string{"100.64.0.4/24", "fd12::4/64"},
				dnsServiceIPs: []string{"10.127.5.3", "fd7b:6b4d:7b25:d22f::3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"10.128.1.0/24 fd11::/64",
					"virt-launcher-1",
					"10.128.1.3 fd11::3",
					"0a:58:0a:80:01:03",
					"namespace1",
				),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{"100.64.0.5/24", "fd12::5/64"},
					testVirtLauncherPod: testVirtLauncherPod{
						labels: map[string]string{
							kvv1.VirtualMachineNameLabel: "vm1",
							kvv1.NodeNameLabel:           node1,
						},
						annotations: map[string]string{
							kvv1.MigrationTargetReadyTimestamp:                time.Now().String(),
							kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
						},
						testPod: testPod{
							podName:    "virt-launcher-2",
							nodeName:   node2,
							nodeSubnet: "10.128.2.0/24 fd12::/64",
						},
					},
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.1.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd11::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
				expectedPolicies: []testPolicy{
					{
						match:   "ip4.src == 10.128.1.3",
						nexthop: "100.64.0.5",
					},
					{
						match:   "ip6.src == fd11::3",
						nexthop: "fd12::5",
					},
				},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     "10.128.1.3",
						nexthop:    "10.128.1.3",
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     "fd11::3",
						nexthop:    "fd11::3",
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
			}),
			Entry("for live migration in progress", testData{
				lrpNetworks:   []string{"100.64.0.4/24", "fd12::4/64"},
				dnsServiceIPs: []string{"10.127.5.3", "fd7b:6b4d:7b25:d22f::3"},
				testVirtLauncherPod: newVirtLauncherTPod(
					map[string]string{
						kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					},
					map[string]string{
						kvv1.VirtualMachineNameLabel: "vm1",
						kvv1.NodeNameLabel:           node1,
					},
					"vm1",
					node1,
					"10.128.1.0/24 fd11::/64",
					"virt-launcher-1",
					"10.128.1.3 fd11::3",
					"0a:58:0a:80:01:03",
					"namespace1",
				),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{"100.64.0.5/24", "fd12::5/64"},
					testVirtLauncherPod: testVirtLauncherPod{
						labels: map[string]string{
							kvv1.VirtualMachineNameLabel: "vm1",
							kvv1.NodeNameLabel:           node1,
						},
						annotations: map[string]string{
							kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
						},
						testPod: testPod{
							podName:    "virt-launcher-2",
							nodeName:   node2,
							nodeSubnet: "10.128.2.0/24 fd12::/64",
						},
					},
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     "10.128.1.0/24",
					dns:      "10.127.5.3",
					router:   kubevirt.ARPProxyIPv4,
					hostname: "vm1",
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     "fd11::/64",
					dns:      "fd7b:6b4d:7b25:d22f::3",
					router:   kubevirt.ARPProxyIPv6,
					hostname: "vm1",
				}},
				expectedPolicies:     []testPolicy{},
				expectedStaticRoutes: []testStaticRoute{},
			}),
		)
	})
})
