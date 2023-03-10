package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/utils/pointer"

	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
)

var _ = Describe("OVN Kubevirt Operations", func() {
	const (
		node1 = "node1"
		node2 = "node2"
		vm1   = "vm1"

		dhcpv4OptionsUUID = "dhcpv4"
		dhcpv6OptionsUUID = "dhcpv6"
		dnsServiceIPv4    = "10.127.5.3"
		dnsServiceIPv6    = "fd7b:6b4d:7b25:d22f::3"
		clusterCIDRIPv4   = "10.128.0.0/16"
		clusterCIDRIPv6   = "fe00::/64"
	)
	type testDHCPOptions struct {
		cidr     string
		dns      string
		hostname string
	}
	type testPolicy struct {
		match   string
		nexthop string
		vmName  string
		uuid    string
	}
	type testStaticRoute struct {
		prefix      string
		nexthop     string
		outputPort  string
		vmName      string
		policy      *nbdb.LogicalRouterStaticRoutePolicy
		uuid        string
		externalIDs map[string]string
		zone        string
	}
	type testVirtLauncherPod struct {
		testPod
		vmName, subnetIPv4, subnetIPv6, addressIPv4, addressIPv6 string
		labels                                                   map[string]string
		annotations                                              map[string]string
		skipPodAnnotations                                       bool
	}
	type testMigrationTarget struct {
		testVirtLauncherPod
		lrpNetworks []string
	}
	type testData struct {
		testVirtLauncherPod
		migrationTarget      testMigrationTarget
		remoteNodes          []string
		interconnected       bool
		dnsServiceIPs        []string
		lrpNetworks          []string
		dhcpv4               []testDHCPOptions
		dhcpv6               []testDHCPOptions
		policies             []testPolicy
		staticRoutes         []testStaticRoute
		expectedDhcpv4       []testDHCPOptions
		expectedDhcpv6       []testDHCPOptions
		expectedPolicies     []testPolicy
		expectedStaticRoutes []testStaticRoute
	}
	type testNode struct {
		lrpNetworkIPv4        string
		lrpNetworkIPv6        string
		subnetIPv4            string
		subnetIPv6            string
		transitSwitchPortIPv4 string
		transitSwitchPortIPv6 string
	}

	type testVM struct {
		addressIPv4 string
		addressIPv6 string
		mac         string
	}

	var (
		app        *cli.App
		fakeOvn    *FakeOVN
		initialDB  libovsdb.TestSetup
		nodeByName = map[string]testNode{
			node1: {
				subnetIPv4:            "10.128.1.0/24",
				subnetIPv6:            "fd11::/64",
				lrpNetworkIPv4:        "100.64.0.4/24",
				lrpNetworkIPv6:        "fd12::4/64",
				transitSwitchPortIPv4: "100.65.0.4/24",
				transitSwitchPortIPv6: "fd13::4/64",
			},
			node2: {
				subnetIPv4:            "10.128.2.0/24",
				subnetIPv6:            "fd12::/64",
				lrpNetworkIPv4:        "100.64.0.5/24",
				lrpNetworkIPv6:        "fd12::5/64",
				transitSwitchPortIPv4: "100.65.0.5/24",
				transitSwitchPortIPv6: "fd13::5/64",
			},
		}
		vmByName = map[string]testVM{
			vm1: {
				addressIPv4: "10.128.1.3",
				addressIPv6: "fd11::3",
			},
		}
		logicalSwitch                            *nbdb.LogicalSwitch
		ovnClusterRouter                         *nbdb.LogicalRouter
		logicalRouterPort                        *nbdb.LogicalRouterPort
		migrationSourceLSRP, migrationTargetLSRP *nbdb.LogicalSwitchPort

		lrpIP = func(network string) string {
			return strings.Split(network, "/")[0]
		}

		externalIDs = func(namespace, vmName, ovnZone string) map[string]string {
			if vmName == "" {
				return nil
			}
			ids := map[string]string{
				kubevirt.OvnZoneExternalIDKey:         ovnZone,
				kubevirt.NamespaceExternalIDsKey:      namespace,
				kubevirt.VirtualMachineExternalIDsKey: vmName,
			}
			return ids
		}

		newVirtLauncherTPod = func(annotations, labels map[string]string, vmName, nodeName, nodeSubnet, nodeGWIP, podName, podIP, podMAC, namespace string) testVirtLauncherPod {
			return testVirtLauncherPod{
				testPod:     newTPod(nodeName, nodeSubnet, "", nodeGWIP, podName, podIP, podMAC, namespace),
				annotations: annotations,
				labels:      labels,
				vmName:      vmName,
			}
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

		isLocalNode = func(t testData, node string) bool {
			for _, remoteNode := range t.remoteNodes {
				if node == remoteNode {
					return false
				}
			}
			return true
		}

		kubevirtOVNTestData = func(t testData, previousData []libovsdbtest.TestData) []libovsdbtest.TestData {
			testVirtLauncherPods := []testVirtLauncherPod{t.testVirtLauncherPod}
			testPods := []testPod{}
			nodeSet := map[string]bool{t.nodeName: true}
			if t.podName != "" && isLocalNode(t, t.nodeName) {
				testPods = append(testPods, t.testPod)
			}
			if t.migrationTarget.nodeName != "" && isLocalNode(t, t.migrationTarget.nodeName) {
				testVirtLauncherPods = append(testVirtLauncherPods, t.migrationTarget.testVirtLauncherPod)
				testPods = append(testPods, t.migrationTarget.testVirtLauncherPod.testPod)
				nodeSet[t.migrationTarget.nodeName] = true
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
			if t.podName == "" {
				return nil
			}
			pod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
			pod.Annotations = t.annotations
			pod.Labels = t.labels
			return pod
		}
		addOVNPodAnnotations = func(pod *kapi.Pod, t testVirtLauncherPod) {
			pod.Annotations = map[string]string{}
			if t.skipPodAnnotations {
				pod.Annotations = map[string]string{}
			} else {
				setPodAnnotations(pod, t.testPod)
			}
			for k, v := range t.annotations {
				pod.Annotations[k] = v
			}
		}
		ComposeDHCPv4Options = func(uuid, namespace string, t *testDHCPOptions) *nbdb.DHCPOptions {
			dhcpOptions := kubevirt.ComposeDHCPv4Options(
				t.cidr,
				t.dns,
				DefaultNetworkControllerName,
				ktypes.NamespacedName{
					Namespace: namespace,
					Name:      t.hostname,
				},
			)
			dhcpOptions.UUID = uuid

			return dhcpOptions
		}
		ComposeDHCPv6Options = func(uuid, namespace string, t *testDHCPOptions) *nbdb.DHCPOptions {
			dhcpOptions := kubevirt.ComposeDHCPv6Options(
				t.cidr,
				t.dns,
				DefaultNetworkControllerName,
				ktypes.NamespacedName{
					Namespace: namespace,
					Name:      t.hostname,
				},
			)
			dhcpOptions.UUID = uuid
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
				ExternalIDs: externalIDs(t.namespace, vmName, kubevirt.OvnLocalZone),
				Priority:    ovntypes.EgressLiveMigrationReroutePiority,
			}
		}
		composeStaticRoute = func(uuid string, r testStaticRoute, t testData) *nbdb.LogicalRouterStaticRoute {
			vmName := t.vmName
			namespace := t.namespace
			if r.vmName != "" {
				vmName = r.vmName
			}
			policy := &nbdb.LogicalRouterStaticRoutePolicyDstIP
			if r.policy != nil {
				policy = r.policy
			}
			var outputPort *string
			if r.outputPort != "" {
				outputPort = &r.outputPort
			}
			zone := kubevirt.OvnLocalZone
			if r.zone != "" {
				zone = r.zone
			}
			ids := externalIDs(namespace, vmName, zone)
			if r.externalIDs != nil {
				if len(r.externalIDs) == 0 {
					ids = nil
				} else {
					ids = r.externalIDs
				}
			}
			return &nbdb.LogicalRouterStaticRoute{
				UUID:        uuid,
				IPPrefix:    r.prefix,
				Nexthop:     r.nexthop,
				Policy:      policy,
				OutputPort:  outputPort,
				ExternalIDs: ids,
			}
		}

		expectedNBDBAfterCleanup = func(expectedStaticRoutes []*nbdb.LogicalRouterStaticRoute) []libovsdb.TestData {
			data := []libovsdb.TestData{}
			expectedPoliciesAfterCleanup := []string{}
			expectedStaticRoutesAfterCleanup := []string{}
			var expectedOvnClusterRouterAfterCleanup *nbdb.LogicalRouter
			for _, nbData := range initialDB.NBData {
				// If they don't have virtual machine name external ID they
				// will survive VM deletion
				if policy, ok := nbData.(*nbdb.LogicalRouterPolicy); ok {
					if policy.ExternalIDs[kubevirt.VirtualMachineExternalIDsKey] != "" {
						continue
					}
					expectedPoliciesAfterCleanup = append(expectedPoliciesAfterCleanup, policy.UUID)
				} else if staticRoute, ok := nbData.(*nbdb.LogicalRouterStaticRoute); ok {
					if staticRoute.ExternalIDs[kubevirt.VirtualMachineExternalIDsKey] != "" {
						continue
					}
					expectedStaticRoutesAfterCleanup = append(expectedStaticRoutesAfterCleanup, staticRoute.UUID)
					// OVN Garbage collector remove them if not referenced at LSP
				} else if _, ok := nbData.(*nbdb.DHCPOptions); ok {
					continue
				} else if lr, ok := nbData.(*nbdb.LogicalRouter); ok {
					expectedOvnClusterRouterAfterCleanup = lr
				}
				data = append(data, nbData)
			}
			for _, expectedStaticRoute := range expectedStaticRoutes {
				// Expected static routes not belonging to any VM should survive deletion
				if expectedStaticRoute.ExternalIDs[kubevirt.VirtualMachineExternalIDsKey] != "" {
					continue
				}
				expectedStaticRoutesAfterCleanup = append(expectedStaticRoutesAfterCleanup, expectedStaticRoute.UUID)
				data = append(data, expectedStaticRoute)

			}
			expectedOvnClusterRouterAfterCleanup.Policies = expectedPoliciesAfterCleanup
			expectedOvnClusterRouterAfterCleanup.StaticRoutes = expectedStaticRoutesAfterCleanup
			return data
		}

		// All the virt-launcher pod for same VM has same subnet, address and
		// mac
		virtLauncher = func(suffix, node, vm, ipFamily string, skipPodAnnotations bool, extraLabels, extraAnnotations map[string]string) testVirtLauncherPod {
			var subnetIPv4, subnetIPv6, subnets, addressIPv4, addressIPv6, addresses, mac, nodeGWIP string
			if ipFamily == "ipv4" {
				subnetIPv4 = nodeByName[node].subnetIPv4
				subnets = subnetIPv4
				addressIPv4 = vmByName[vm].addressIPv4
				addresses = addressIPv4
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv4)).String()
				nodeGWIP = kubevirt.ARPProxyIPv4
			} else if ipFamily == "ipv6" {
				subnetIPv6 = nodeByName[node].subnetIPv6
				subnets = subnetIPv6
				addressIPv6 = vmByName[vm].addressIPv6
				addresses = addressIPv6
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv6)).String()
				nodeGWIP = kubevirt.ARPProxyIPv6
			} else if ipFamily == "dualstack" {
				subnetIPv4 = nodeByName[node].subnetIPv4
				subnetIPv6 = nodeByName[node].subnetIPv6
				subnets = subnetIPv4 + " " + subnetIPv6
				addressIPv4 = vmByName[vm].addressIPv4
				addressIPv6 = vmByName[vm].addressIPv6
				addresses = addressIPv4 + " " + addressIPv6
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv4)).String()
				nodeGWIP = kubevirt.ARPProxyIPv4 + " " + kubevirt.ARPProxyIPv6
			}
			labels := map[string]string{
				kubevirtv1.VirtualMachineNameLabel: "vm1",
				kubevirtv1.NodeNameLabel:           node,
			}
			for k, v := range extraLabels {
				labels[k] = v
			}
			annotations := map[string]string{
				kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
			}
			for k, v := range extraAnnotations {
				annotations[k] = v
			}
			t := newVirtLauncherTPod(
				annotations,
				labels,
				"vm1",
				node,
				subnets,
				nodeGWIP,
				"virt-launcher-"+suffix,
				addresses,
				mac,
				"namespace1",
			)
			t.subnetIPv4 = subnetIPv4
			t.subnetIPv6 = subnetIPv6
			t.addressIPv4 = addressIPv4
			t.addressIPv6 = addressIPv6
			return t
		}
		virtLauncher1 = func(node, vm, ipFamily string) testVirtLauncherPod {
			return virtLauncher("1", node, vm, ipFamily, true, map[string]string{}, map[string]string{})
		}
		virtLauncher2 = func(node, vm, ipFamily string) testVirtLauncherPod {
			return virtLauncher("2", node, vm, ipFamily, true, map[string]string{}, map[string]string{})
		}
		virtLauncher2WithExtraAnnotations = func(node, vm, ipFamily string, extraAnnotations map[string]string) testVirtLauncherPod {
			return virtLauncher("2", node, vm, ipFamily, true, map[string]string{}, extraAnnotations)
		}
		virtLauncher2WithExtraLabels = func(node, vm, ipFamily string, extraLabels map[string]string) testVirtLauncherPod {
			return virtLauncher("2", node, vm, ipFamily, true, extraLabels, map[string]string{})
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

			_, parsedClusterCIDRIPv4, err := net.ParseCIDR(clusterCIDRIPv4)
			Expect(err).ToNot(HaveOccurred())

			_, parsedClusterCIDRIPv6, err := net.ParseCIDR(clusterCIDRIPv6)
			Expect(err).ToNot(HaveOccurred())

			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{
					CIDR:             parsedClusterCIDRIPv4,
					HostSubnetLength: 24,
				},
				{
					CIDR:             parsedClusterCIDRIPv6,
					HostSubnetLength: 24,
				},
			}

			// By default the test a dual stack node with name node1
			if t.nodeName == "" {
				t.nodeName = node1
				t.nodeSubnet = "10.128.1.0/24 fd11::/64"
			}

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

			initialOvnClusterRouter := ovnClusterRouter.DeepCopy()

			for i, p := range t.policies {
				policy := composePolicy(fmt.Sprintf("policy%d", i), p, t)
				initialOvnClusterRouter.Policies = append(initialOvnClusterRouter.Policies, policy.UUID)
				initialDB.NBData = append(initialDB.NBData, policy)
			}

			for i, r := range t.staticRoutes {
				staticRoute := composeStaticRoute(fmt.Sprintf("route%d", i), r, t)
				initialOvnClusterRouter.StaticRoutes = append(initialOvnClusterRouter.StaticRoutes, staticRoute.UUID)
				initialDB.NBData = append(initialDB.NBData, staticRoute)
			}

			for i, d := range t.dhcpv4 {
				initialDB.NBData = append(initialDB.NBData, ComposeDHCPv4Options(fmt.Sprintf("dhcpv4%d%s", i, d.hostname), t.namespace, &d))
			}

			for i, d := range t.dhcpv6 {
				initialDB.NBData = append(initialDB.NBData, ComposeDHCPv6Options(fmt.Sprintf("dhcpv6%d%s", i, d.hostname), t.namespace, &d))
			}

			initialDB.NBData = append(initialDB.NBData,
				logicalSwitch,
				initialOvnClusterRouter,
				logicalRouterPort,
				migrationSourceLSRP)

			if t.migrationTarget.nodeName != "" {
				migrationTargetLRP = &nbdb.LogicalRouterPort{
					UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.migrationTarget.nodeName + "-UUID",
					Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.migrationTarget.nodeName,
					Networks: t.migrationTarget.lrpNetworks,
				}
				migrationTargetLSRP = &nbdb.LogicalSwitchPort{
					Name: ovntypes.SwitchToRouterPrefix + t.migrationTarget.nodeName,
					UUID: ovntypes.SwitchToRouterPrefix + t.migrationTarget.nodeName + "-UUID",
					Type: "router",
					Options: map[string]string{
						"router-port": migrationTargetLRP.Name,
						"arp_proxy":   kubevirt.ComposeARPProxyLSPOption(),
					},
				}
				migrationTargetLS = &nbdb.LogicalSwitch{
					Name: t.migrationTarget.nodeName,
					UUID: t.migrationTarget.nodeName + "-UUID",
				}
				initialDB.NBData = append(initialDB.NBData,
					migrationTargetLRP,
					migrationTargetLSRP,
					migrationTargetLS,
				)
			}
			pods := []v1.Pod{}
			sourcePod := newPodFromTestVirtLauncherPod(t.testVirtLauncherPod)
			if sourcePod != nil {
				addOVNPodAnnotations(sourcePod, t.testVirtLauncherPod)
				if t.migrationTarget.nodeName != "" {
					pods = append(pods, *sourcePod)
				}
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
									Annotations: map[string]string{
										"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf(`{"ipv4": %q, "ipv6": %q}`, nodeByName[node1].transitSwitchPortIPv4, nodeByName[node1].transitSwitchPortIPv6),
										"k8s.ovn.org/node-subnets":                    fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[node1].subnetIPv4, nodeByName[node1].subnetIPv6),
									},
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node2,
									Annotations: map[string]string{
										"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf(`{"ipv4": %q, "ipv6": %q}`, nodeByName[node2].transitSwitchPortIPv4, nodeByName[node2].transitSwitchPortIPv6),
										"k8s.ovn.org/node-subnets":                    fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[node2].subnetIPv4, nodeByName[node2].subnetIPv6),
									},
								},
							},
						},
					},
				)
				t.testPod.populateLogicalSwitchCache(fakeOvn)
				if t.migrationTarget.nodeName != "" {
					t.migrationTarget.populateLogicalSwitchCache(fakeOvn)
				}

				config.OVNKubernetesFeature.EnableInterconnect = t.interconnected
				for _, remoteNode := range t.remoteNodes {
					fakeOvn.controller.localZoneNodes.Delete(remoteNode)
				}

				err := fakeOvn.controller.WatchNamespaces()
				Expect(err).NotTo(HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				Expect(err).NotTo(HaveOccurred())
				podToCreate := sourcePod
				if podToCreate != nil {
					if t.migrationTarget.nodeName != "" {
						podToCreate = newPodFromTestVirtLauncherPod(t.migrationTarget.testVirtLauncherPod)
						podToCreate.Labels = t.migrationTarget.labels
						podToCreate.Annotations = t.migrationTarget.annotations
					}
					pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), podToCreate.Name, metav1.GetOptions{})
					Expect(pod).To(BeNil())

					podToCreate.CreationTimestamp = metav1.NewTime(time.Now())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), podToCreate, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

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
				expectedStaticRoutes := []*nbdb.LogicalRouterStaticRoute{}
				for i, r := range t.expectedStaticRoutes {
					uuid := fmt.Sprintf("route%d", i)
					if r.uuid != "" {
						uuid = r.uuid
					}
					expectedStaticRoute := composeStaticRoute(uuid, r, t)
					expectedStaticRoutes = append(expectedStaticRoutes, expectedStaticRoute)
					expectedOVNClusterRouter.StaticRoutes = append(expectedOVNClusterRouter.StaticRoutes, expectedStaticRoute.UUID)
					expectedOVN = append(expectedOVN, expectedStaticRoute)
				}
				for _, d := range t.expectedDhcpv4 {
					expectedOVN = append(expectedOVN, ComposeDHCPv4Options(dhcpv4OptionsUUID+d.hostname, t.namespace, &d))
				}

				for _, d := range t.expectedDhcpv6 {
					expectedOVN = append(expectedOVN, ComposeDHCPv6Options(dhcpv6OptionsUUID+d.hostname, t.namespace, &d))
				}
				expectedSourceLSRP := migrationSourceLSRP.DeepCopy()
				expectedOVN = append(expectedOVN,
					expectedOVNClusterRouter,
					logicalRouterPort,
					expectedSourceLSRP,
				)
				expectedOVN = kubevirtOVNTestData(t, expectedOVN)

				if t.migrationTarget.nodeName != "" {
					expectedTargetLSRP := migrationTargetLSRP.DeepCopy()
					expectedOVN = append(expectedOVN,
						migrationTargetLRP,
						expectedTargetLSRP,
					)
				}
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedOVN), "should populate ovn")

				if t.podName != "" {
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					if t.migrationTarget.nodeName != "" {
						err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.migrationTarget.podName, metav1.DeleteOptions{})
						Expect(err).NotTo(HaveOccurred())
					}
					Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedNBDBAfterCleanup(expectedStaticRoutes)), "should cleanup ovn")
				}

				return nil
			}
			err = app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("for single stack ipv4 at global zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4},
				dnsServiceIPs:       []string{dnsServiceIPv4},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv4"),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv4 at local zone", testData{
				interconnected:      true,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4},
				dnsServiceIPs:       []string{dnsServiceIPv4},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv4"),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv4 at remote zone", testData{
				interconnected:      true,
				remoteNodes:         []string{node1},
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4},
				dnsServiceIPs:       []string{dnsServiceIPv4},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv4"),
			}),
			Entry("for single stack ipv6 at global zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv6"),
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv6 at local zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv6},
				interconnected:      true,
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv6"),
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv6 at remote zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv6},
				interconnected:      true,
				remoteNodes:         []string{node1},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "ipv6"),
			}),
			Entry("for dual stack at global zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for dual stack at local zone", testData{
				interconnected:      true,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for dual stack at remote zone", testData{
				interconnected:      true,
				remoteNodes:         []string{node1},
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
			}),

			Entry("for pre-copy live migration at global zone", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node2, vm1, "dualstack"),
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
				expectedPolicies: []testPolicy{
					{
						match:   "ip4.src == " + vmByName[vm1].addressIPv4,
						nexthop: lrpIP(nodeByName[node2].lrpNetworkIPv4),
					},
					{
						match:   "ip6.src == " + vmByName[vm1].addressIPv6,
						nexthop: lrpIP(nodeByName[node2].lrpNetworkIPv6),
					},
				},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     vmByName[vm1].addressIPv4,
						nexthop:    vmByName[vm1].addressIPv4,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     vmByName[vm1].addressIPv6,
						nexthop:    vmByName[vm1].addressIPv6,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
			}),
			Entry("for pre-copy live migration at local zone", testData{
				interconnected:      true,
				remoteNodes:         []string{node1},
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node2, vm1, "dualstack"),
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     vmByName[vm1].addressIPv4,
						nexthop:    vmByName[vm1].addressIPv4,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     vmByName[vm1].addressIPv6,
						nexthop:    vmByName[vm1].addressIPv6,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:      clusterCIDRIPv4,
						nexthop:     strings.Split(nodeByName[node2].lrpNetworkIPv4, "/")[0],
						policy:      &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						externalIDs: map[string]string{},
					},
					{
						prefix:      clusterCIDRIPv6,
						nexthop:     strings.Split(nodeByName[node2].lrpNetworkIPv6, "/")[0],
						policy:      &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						externalIDs: map[string]string{},
					},
				},
			}),
			Entry("for pre-copy live migration at remote zone", testData{
				interconnected:      true,
				remoteNodes:         []string{node2},
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node2, vm1, "dualstack"),
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:  vmByName[vm1].addressIPv4,
						nexthop: strings.Split(nodeByName[node2].transitSwitchPortIPv4, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
					{
						prefix:  vmByName[vm1].addressIPv6,
						nexthop: strings.Split(nodeByName[node2].transitSwitchPortIPv6, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
				},
			}),
			Entry("for pre-copy live migration to node owning subnet at global zone", testData{
				lrpNetworks:   []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				policies: []testPolicy{
					{
						match:   "ip4.src == " + vmByName[vm1].addressIPv4,
						nexthop: nodeByName[node2].lrpNetworkIPv4,
					},
					{
						match:   "ip6.src == " + vmByName[vm1].addressIPv6,
						nexthop: nodeByName[node2].lrpNetworkIPv6,
					},
				},
				staticRoutes: []testStaticRoute{
					{
						prefix:     vmByName[vm1].addressIPv4,
						nexthop:    vmByName[vm1].addressIPv4,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     vmByName[vm1].addressIPv6,
						nexthop:    vmByName[vm1].addressIPv6,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
				testVirtLauncherPod: virtLauncher("1", node2, vm1, "dualstack", false, /* add ovn pod annotation */
					map[string]string{},
					map[string]string{},
				),
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node1, vm1, "dualstack"),
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: virtLauncher2(node1, vm1, "dualstack").vmName,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: virtLauncher2(node1, vm1, "dualstack").vmName,
				}},
			}),
			Entry("for pre-copy live migration to node owning subnet at remote zone", testData{
				interconnected: true,
				remoteNodes:    []string{node1},
				lrpNetworks:    []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
				dnsServiceIPs:  []string{dnsServiceIPv4, dnsServiceIPv6},
				policies: []testPolicy{
					{
						match:   "ip4.src == " + vmByName[vm1].addressIPv4,
						nexthop: nodeByName[node2].lrpNetworkIPv4,
					},
					{
						match:   "ip6.src == " + vmByName[vm1].addressIPv6,
						nexthop: nodeByName[node2].lrpNetworkIPv6,
					},
				},
				staticRoutes: []testStaticRoute{
					{
						prefix:     vmByName[vm1].addressIPv4,
						nexthop:    vmByName[vm1].addressIPv4,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     vmByName[vm1].addressIPv6,
						nexthop:    vmByName[vm1].addressIPv6,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
				testVirtLauncherPod: virtLauncher2(node1, vm1, "dualstack"),
			}),
			Entry("for pre-copy live migration to node owning subnet at local", testData{
				interconnected: true,
				remoteNodes:    []string{node2},
				lrpNetworks:    []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:  []string{dnsServiceIPv4, dnsServiceIPv6},
				staticRoutes: []testStaticRoute{
					{
						prefix:  vmByName[vm1].addressIPv4,
						nexthop: strings.Split(nodeByName[node2].transitSwitchPortIPv4, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
					{
						prefix:  vmByName[vm1].addressIPv6,
						nexthop: strings.Split(nodeByName[node2].transitSwitchPortIPv6, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
				},
				testVirtLauncherPod: virtLauncher2(node1, vm1, "dualstack"),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for post-copy live migration", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2WithExtraAnnotations(node2, vm1, "dualstack", map[string]string{
						kubevirtv1.MigrationTargetReadyTimestamp: time.Now().String(),
					}),
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
				expectedPolicies: []testPolicy{
					{
						match:   "ip4.src == " + vmByName[vm1].addressIPv4,
						nexthop: lrpIP(nodeByName[node2].lrpNetworkIPv4),
					},
					{
						match:   "ip6.src == " + vmByName[vm1].addressIPv6,
						nexthop: lrpIP(nodeByName[node2].lrpNetworkIPv6),
					},
				},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     vmByName[vm1].addressIPv4,
						nexthop:    vmByName[vm1].addressIPv4,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
					{
						prefix:     vmByName[vm1].addressIPv6,
						nexthop:    vmByName[vm1].addressIPv6,
						outputPort: ovntypes.RouterToSwitchPrefix + node2,
					},
				},
			}),
			Entry("for live migration in progress", testData{
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1, "dualstack"),
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2WithExtraLabels(node2, vm1, "dualstack", map[string]string{
						kubevirtv1.NodeNameLabel: node1,
					}),
				},
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("should remove orphan routes and policies and keep not kubevirt related on startup", testData{
				policies: []testPolicy{
					{
						match:   "ip4.src == 10.128.1.3",
						nexthop: "100.64.0.5",
						vmName:  "vm1",
					},
					{
						match:   "ip4.src == 10.128.1.0/24",
						nexthop: "100.64.0.3",
					},
				},
				staticRoutes: []testStaticRoute{
					{
						prefix:     "10.128.1.3",
						nexthop:    "10.128.1.3",
						outputPort: ovntypes.RouterToSwitchPrefix + node1,
						vmName:     "vm1",
					},
					{
						prefix:     "10.128.0.4/24",
						outputPort: ovntypes.RouterToSwitchPrefix + node1,
					},
				},
				expectedPolicies: []testPolicy{
					{
						match:   "ip4.src == 10.128.1.0/24",
						nexthop: "100.64.0.3",
					},
				},
				expectedStaticRoutes: []testStaticRoute{
					{
						prefix:     "10.128.0.4/24",
						outputPort: ovntypes.RouterToSwitchPrefix + node1,
					},
				},
			}),
		)
	})
})
