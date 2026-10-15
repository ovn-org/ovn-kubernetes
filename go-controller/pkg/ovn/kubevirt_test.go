// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OVN Kubevirt Operations", func() {
	const (
		node1 = "node1"
		node2 = "node2"
		node3 = "node3"
		vm1   = "vm1"

		dhcpv4OptionsUUID = "dhcpv4"
		dhcpv6OptionsUUID = "dhcpv6"
		dnsServiceIPv4    = "10.127.5.3"
		dnsServiceIPv6    = "fd7b:6b4d:7b25:d22f::3"
		clusterCIDRIPv4   = "10.128.0.0/16"
		clusterCIDRIPv6   = "fe00::/64"
		subnetSuffixIPv4  = "/24"
		subnetSuffixIPv6  = "/64"
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
		suffix                                                   string
		skipPodAnnotations                                       bool
		extraLabels, extraAnnotations                            map[string]string
		updatePhase                                              *corev1.PodPhase
	}
	type testMigrationTarget struct {
		testVirtLauncherPod
		lrpNetworks []string
	}
	type testData struct {
		testVirtLauncherPod
		migrationTarget      testMigrationTarget
		localNode            string
		ipv4                 bool
		ipv6                 bool
		replaceNode          string
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
		nodeID                string
		lrpNetworkIPv4        string
		lrpNetworkIPv6        string
		subnetIPv4            string
		subnetIPv6            string
		gwIPv4                string
		gwIPv6                string
		transitSwitchPortIPv4 string
		transitSwitchPortIPv6 string
		addressIPv4           string
		addressIPv6           string
	}

	type testVM struct {
		addressIPv4 string
		addressIPv6 string
	}

	var (
		app        *cli.App
		fakeOvn    *FakeOVN
		initialDB  libovsdb.TestSetup
		nodeByName = map[string]testNode{
			node1: {
				nodeID:                "4",
				subnetIPv4:            "10.128.1.0/24",
				subnetIPv6:            "fd11::/64",
				gwIPv4:                "10.128.1.1",
				gwIPv6:                "fd11::1",
				lrpNetworkIPv4:        "100.64.0.4/24",
				lrpNetworkIPv6:        "fd98::4/64",
				transitSwitchPortIPv4: "100.65.0.4/24",
				transitSwitchPortIPv6: "fd13::4/64",
				addressIPv4:           "10.89.0.1/24",
				addressIPv6:           "fc00:f853:ccd:e793::1/64",
			},
			node2: {
				nodeID:                "5",
				subnetIPv4:            "10.128.2.0/24",
				subnetIPv6:            "fd12::/64",
				gwIPv4:                "10.128.2.1",
				gwIPv6:                "fd12::1",
				lrpNetworkIPv4:        "100.64.0.5/24",
				lrpNetworkIPv6:        "fd98::5/64",
				transitSwitchPortIPv4: "100.65.0.5/24",
				transitSwitchPortIPv6: "fd13::5/64",
				addressIPv4:           "10.89.0.2/24",
				addressIPv6:           "fc00:f853:ccd:e793::2/64",
			},
			node3: {
				nodeID:                "6",
				subnetIPv4:            "10.128.3.0/24",
				subnetIPv6:            "fd13::/64",
				gwIPv4:                "10.128.3.1",
				gwIPv6:                "fd13::1",
				lrpNetworkIPv4:        "100.64.0.6/24",
				lrpNetworkIPv6:        "fd98::6/64",
				transitSwitchPortIPv4: "100.65.0.6/24",
				transitSwitchPortIPv6: "fd13::6/64",
				addressIPv4:           "10.89.0.3/24",
				addressIPv6:           "fc00:f853:ccd:e793::3/64",
			},
		}
		vmByName = map[string]testVM{
			vm1: {
				addressIPv4: "10.128.1.3",
				addressIPv6: "fd11::3",
			},
		}

		phasePointer = func(phase corev1.PodPhase) *corev1.PodPhase {
			return &phase
		}

		virtLauncherCompleted = func(t testVirtLauncherPod) bool {
			if t.updatePhase == nil {
				return false
			}
			return *t.updatePhase == corev1.PodSucceeded || *t.updatePhase == corev1.PodFailed
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

		localNode = func(t testData) string {
			if t.localNode != "" {
				return t.localNode
			}
			return node1
		}

		isLocalNode = func(t testData, node string) bool {
			return node == localNode(t)
		}

		kubevirtOVNTestData = func(t testData, previousData []libovsdb.TestData) []libovsdb.TestData {
			testVirtLauncherPods := []testVirtLauncherPod{t.testVirtLauncherPod}
			testPods := []testPod{}
			nodeSet := map[string]bool{t.nodeName: true}
			if t.podName != "" && isLocalNode(t, t.nodeName) {
				if !virtLauncherCompleted(t.testVirtLauncherPod) {
					testPods = append(testPods, t.testPod)
				}
			}
			if t.migrationTarget.nodeName != "" && isLocalNode(t, t.migrationTarget.nodeName) {
				if !virtLauncherCompleted(t.migrationTarget.testVirtLauncherPod) {
					testVirtLauncherPods = append(testVirtLauncherPods, t.migrationTarget.testVirtLauncherPod)
					testPods = append(testPods, t.migrationTarget.testVirtLauncherPod.testPod)
				}
				nodeSet[t.migrationTarget.nodeName] = true
			}

			nodes := []string{}
			for node := range nodeSet {
				nodes = append(nodes, node)
			}
			data := getDefaultNetExpectedPodsAndSwitches(testPods, nodes)
			for _, d := range data {
				switch model := d.(type) {
				case *nbdb.LogicalSwitchPort:
					for _, p := range testVirtLauncherPods {
						portName := util.GetLogicalPortName(p.namespace, p.podName)
						if model.Name == portName {
							if findDHCPOptionsWithHostname(t.expectedDhcpv4, p.vmName) != nil {
								model.Dhcpv4Options = ptr.To(dhcpv4OptionsUUID + p.vmName)
							}
							if findDHCPOptionsWithHostname(t.expectedDhcpv6, p.vmName) != nil {
								model.Dhcpv6Options = ptr.To(dhcpv6OptionsUUID + p.vmName)
							}
						}
					}
				case *nbdb.LogicalSwitch:
					if nodeSet[model.Name] {
						model.Ports = append(model.Ports, ovntypes.SwitchToRouterPrefix+model.Name+"-UUID")
					}
				}
			}
			// Prepend previous data
			return append(previousData, data...)
		}

		filterOutStaleVirtLauncherExpectedTestData = func(namespace, name string, previousData []libovsdb.TestData) []libovsdb.TestData {
			var data []libovsdb.TestData
			lspUUID := util.GetLogicalPortName(namespace, name) + "-UUID"
			for _, d := range previousData {
				switch model := d.(type) {
				case *nbdb.LogicalSwitch:
					lsp := *model
					lsp.Ports = slices.Clone(lsp.Ports)
					lsp.Ports = slices.DeleteFunc(lsp.Ports, func(port string) bool { return port == lspUUID })
					d = &lsp
				case *nbdb.LogicalSwitchPort:
					if model.UUID == lspUUID {
						continue
					}
				}
				data = append(data, d)

			}
			return data
		}

		newPodFromTestVirtLauncherPod = func(t testVirtLauncherPod) *corev1.Pod {
			if t.podName == "" {
				return nil
			}
			pod := testing.NewPod(t.namespace, t.podName, t.nodeName, t.podIP)
			pod.Annotations = t.annotations
			pod.Labels = t.labels
			return pod
		}
		addOVNPodAnnotations = func(pod *corev1.Pod, t testVirtLauncherPod) {
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
		ComposeDHCPv4Options = func(uuid, namespace string, nodeName string, t *testDHCPOptions) *nbdb.DHCPOptions {
			GinkgoHelper()
			dhcpOptions := kubevirt.ComposeDHCPv4Options(
				t.cidr,
				ovntypes.DefaultNetworkControllerName,
				ktypes.NamespacedName{
					Namespace: namespace,
					Name:      t.hostname,
				},
			)

			dhcpOptions.Options["mtu"] = "1400"
			dhcpOptions.Options["dns_server"] = t.dns
			dhcpOptions.Options["router"] = nodeByName[nodeName].gwIPv4
			dhcpOptions.UUID = uuid

			return dhcpOptions
		}
		ComposeDHCPv6Options = func(uuid, namespace string, t *testDHCPOptions) *nbdb.DHCPOptions {
			dhcpOptions := kubevirt.ComposeDHCPv6Options(
				t.cidr,
				ovntypes.DefaultNetworkControllerName,
				ktypes.NamespacedName{
					Namespace: namespace,
					Name:      t.hostname,
				},
			)
			dhcpOptions.UUID = uuid
			dhcpOptions.Options["dns_server"] = t.dns
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
				Priority:    ovntypes.EgressLiveMigrationReroutePriority,
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
		composeNats = func(pod testVirtLauncherPod) ([]string, []*nbdb.NAT) {
			var ids []string
			var nats []*nbdb.NAT
			if config.IPv4Mode {
				id := pod.podName + "-IPv4-NAD-UUID"
				nats = append(nats, &nbdb.NAT{
					UUID:       id,
					LogicalIP:  pod.addressIPv4,
					ExternalIP: testing.MustParseIPNet(nodeByName[pod.nodeName].addressIPv4).IP.String(),
					Options:    map[string]string{"stateless": "false"},
					Type:       "snat",
				})
				ids = append(ids, id)
			}
			if config.IPv6Mode {
				id := pod.podName + "-IPv6-NAD-UUID"
				nats = append(nats, &nbdb.NAT{
					UUID:       id,
					LogicalIP:  pod.addressIPv6,
					ExternalIP: testing.MustParseIPNet(nodeByName[pod.nodeName].addressIPv6).IP.String(),
					Options:    map[string]string{"stateless": "false"},
					Type:       "snat",
				})
				ids = append(ids, id)
			}
			return ids, nats
		}

		expectedNBDBAfterCleanup = func(expectedStaticRoutes []*nbdb.LogicalRouterStaticRoute, expectedNATs map[string][]*nbdb.NAT) []libovsdb.TestData {
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
				} else if _, ok := nbData.(*nbdb.DHCPOptions); ok {
					// OVN Garbage collector removes them if not referenced at LSP
					continue
				} else if lr, ok := nbData.(*nbdb.LogicalRouter); ok && lr.Name == ovntypes.OVNClusterRouter {
					expectedOvnClusterRouterAfterCleanup = lr
				} else if lr, ok := nbData.(*nbdb.LogicalRouter); ok && expectedNATs[lr.Name] != nil {
					for _, nat := range expectedNATs[lr.Name] {
						lr.Nat = append(lr.Nat, nat.UUID)
						data = append(data, nat)
					}
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
		initVirtLauncherPod = func(t *testVirtLauncherPod) {
			var subnetIPv4, subnetIPv6, subnets, addressIPv4, addressIPv6, addresses, mac, nodeGWIP string
			if config.IPv4Mode && !config.IPv6Mode {
				subnetIPv4 = nodeByName[t.nodeName].subnetIPv4
				subnets = subnetIPv4
				addressIPv4 = vmByName[t.vmName].addressIPv4
				addresses = addressIPv4
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv4)).String()
				nodeGWIP = nodeByName[t.nodeName].gwIPv4
			} else if config.IPv6Mode && !config.IPv4Mode {
				subnetIPv6 = nodeByName[t.nodeName].subnetIPv6
				subnets = subnetIPv6
				addressIPv6 = vmByName[t.vmName].addressIPv6
				addresses = addressIPv6
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv6)).String()
				nodeGWIP = nodeByName[t.nodeName].gwIPv6
			} else if config.IPv4Mode && config.IPv6Mode {
				subnetIPv4 = nodeByName[t.nodeName].subnetIPv4
				subnetIPv6 = nodeByName[t.nodeName].subnetIPv6
				subnets = subnetIPv4 + " " + subnetIPv6
				addressIPv4 = vmByName[t.vmName].addressIPv4
				addressIPv6 = vmByName[t.vmName].addressIPv6
				addresses = addressIPv4 + " " + addressIPv6
				mac = util.IPAddrToHWAddr(net.ParseIP(addressIPv4)).String()
				nodeGWIP = nodeByName[t.nodeName].gwIPv4 + " " + nodeByName[t.nodeName].gwIPv6
			}
			labels := map[string]string{
				kubevirtv1.VirtualMachineNameLabel: t.vmName,
				kubevirtv1.NodeNameLabel:           t.nodeName,
				kubevirtv1.AppLabel:                "virt-launcher",
			}
			for k, v := range t.extraLabels {
				labels[k] = v
			}
			annotations := map[string]string{
				kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
				kubevirtv1.DomainAnnotation:                             t.vmName,
			}
			for k, v := range t.extraAnnotations {
				annotations[k] = v
			}
			t.testPod = newTPod(t.nodeName, subnets, "", nodeGWIP, "virt-launcher-"+t.suffix, addresses, mac, "namespace1")
			t.annotations = annotations
			t.labels = labels
			t.subnetIPv4 = subnetIPv4
			t.subnetIPv6 = subnetIPv6
			t.addressIPv4 = addressIPv4
			t.addressIPv6 = addressIPv6
		}
		virtLauncher1 = func(node, vm string) testVirtLauncherPod {
			return testVirtLauncherPod{
				suffix: "1",
				testPod: testPod{
					nodeName: node,
				},
				vmName:             vm,
				skipPodAnnotations: true,
			}
		}
		virtLauncher2 = func(node, vm string) testVirtLauncherPod {
			return testVirtLauncherPod{
				suffix: "2",
				testPod: testPod{
					nodeName: node,
				},
				vmName:             vm,
				skipPodAnnotations: true,
			}
		}
		virtLauncher2WithExtraAnnotations = func(node, vm string, extraAnnotations map[string]string) testVirtLauncherPod {
			return testVirtLauncherPod{
				suffix: "2",
				testPod: testPod{
					nodeName: node,
				},
				vmName:             vm,
				skipPodAnnotations: true,
				extraAnnotations:   extraAnnotations,
			}
		}
		virtLauncher2WithExtraLabels = func(node, vm string, extraLabels map[string]string) testVirtLauncherPod {
			return testVirtLauncherPod{
				suffix: "2",
				testPod: testPod{
					nodeName: node,
				},
				vmName:             vm,
				skipPodAnnotations: true,
				extraLabels:        extraLabels,
			}
		}
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		// To skip port group not found error
		config.EnableMulticast = false
		config.Gateway.DisableSNATMultipleGWs = true

		fakeOvn = NewFakeOVN(true, node1)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("during execution", func() {
		DescribeTable("reconcile migratable vm pods", func(t testData) {
			var (
				logicalSwitch                            *nbdb.LogicalSwitch
				ovnClusterRouter                         *nbdb.LogicalRouter
				logicalRouterPort                        *nbdb.LogicalRouterPort
				migrationSourceLSRP, migrationTargetLSRP *nbdb.LogicalSwitchPort
			)

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
			gwRouter := &nbdb.LogicalRouter{
				UUID:  ovntypes.GWRouterPrefix + t.nodeName + "-UUID",
				Name:  ovntypes.GWRouterPrefix + t.nodeName,
				Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.nodeName + "-UUID"},
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
					libovsdbops.RouterPort: logicalRouterPort.Name,
					"arp_proxy":            kubevirt.ComposeARPProxyLSPOption(),
				},
			}
			logicalSwitch = &nbdb.LogicalSwitch{
				Name:  t.nodeName,
				UUID:  t.nodeName + "-UUID",
				Ports: []string{ovntypes.SwitchToRouterPrefix + t.nodeName + "-UUID"},
			}
			var migrationTargetLRP *nbdb.LogicalRouterPort
			var migrationTargetLS *nbdb.LogicalSwitch
			var migrationTargetGWRouter *nbdb.LogicalRouter

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
				initialDB.NBData = append(initialDB.NBData, ComposeDHCPv4Options(fmt.Sprintf("dhcpv4%d%s", i, d.hostname), t.namespace, t.nodeName, &d))
			}

			for i, d := range t.dhcpv6 {
				initialDB.NBData = append(initialDB.NBData, ComposeDHCPv6Options(fmt.Sprintf("dhcpv6%d%s", i, d.hostname), t.namespace, &d))
			}

			initialDB.NBData = append(initialDB.NBData,
				logicalSwitch,
				initialOvnClusterRouter,
				gwRouter,
				logicalRouterPort,
				migrationSourceLSRP)

			if t.migrationTarget.nodeName != "" {
				migrationTargetGWRouter = &nbdb.LogicalRouter{
					UUID:  ovntypes.GWRouterPrefix + t.migrationTarget.nodeName + "-UUID",
					Name:  ovntypes.GWRouterPrefix + t.migrationTarget.nodeName,
					Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + t.migrationTarget.nodeName + "-UUID"},
				}
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
						libovsdbops.RouterPort: migrationTargetLRP.Name,
						"arp_proxy":            kubevirt.ComposeARPProxyLSPOption(),
					},
				}
				migrationTargetLS = &nbdb.LogicalSwitch{
					Name:  t.migrationTarget.nodeName,
					UUID:  t.migrationTarget.nodeName + "-UUID",
					Ports: []string{ovntypes.SwitchToRouterPrefix + t.migrationTarget.nodeName + "-UUID"},
				}
				initialDB.NBData = append(initialDB.NBData,
					migrationTargetLRP,
					migrationTargetLSRP,
					migrationTargetGWRouter,
					migrationTargetLS,
				)
			}

			config.IPv4Mode = t.ipv4
			config.IPv6Mode = t.ipv6

			if t.testVirtLauncherPod.vmName != "" {
				initVirtLauncherPod(&t.testVirtLauncherPod)
			}
			if t.migrationTarget.nodeName != "" {
				initVirtLauncherPod(&t.migrationTarget.testVirtLauncherPod)
			}

			pods := []corev1.Pod{}
			sourceVirtLauncherPod := t.testVirtLauncherPod
			sourcePod := newPodFromTestVirtLauncherPod(sourceVirtLauncherPod)
			if sourcePod != nil {
				addOVNPodAnnotations(sourcePod, sourceVirtLauncherPod)
				if t.migrationTarget.nodeName != "" {
					pods = append(pods, *sourcePod)
				}
			}

			app.Action = func(*cli.Context) error {
				fakeOvn.nodeName = localNode(t)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*testing.NewNamespace(t.namespace),
						},
					},
					&corev1.PodList{
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
					&corev1.NodeList{
						Items: []corev1.Node{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node1,
									Annotations: map[string]string{
										"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf(`{"ipv4": %q, "ipv6": %q}`, nodeByName[node1].transitSwitchPortIPv4, nodeByName[node1].transitSwitchPortIPv6),
										"k8s.ovn.org/node-subnets":                    fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[node1].subnetIPv4, nodeByName[node1].subnetIPv6),
										"k8s.ovn.org/l3-gateway-config":               fmt.Sprintf(`{"default": {"mode": "local", "mac-address":"7e:57:f8:f0:3c:51", "ip-addresses":[%q, %q]}}`, nodeByName[node1].addressIPv4, nodeByName[node1].addressIPv6),
										"k8s.ovn.org/node-chassis-id":                 chassisIDForNode(node1),
										util.OvnNodeID:                                nodeByName[node1].nodeID,
									},
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node2,
									Annotations: map[string]string{
										"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf(`{"ipv4": %q, "ipv6": %q}`, nodeByName[node2].transitSwitchPortIPv4, nodeByName[node2].transitSwitchPortIPv6),
										"k8s.ovn.org/node-subnets":                    fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[node2].subnetIPv4, nodeByName[node2].subnetIPv6),
										"k8s.ovn.org/l3-gateway-config":               fmt.Sprintf(`{"default": {"mode": "local", "mac-address":"7e:57:f8:f0:3c:52", "ip-addresses":[%q, %q]}}`, nodeByName[node2].addressIPv4, nodeByName[node2].addressIPv6),
										"k8s.ovn.org/node-chassis-id":                 chassisIDForNode(node2),
										util.OvnNodeID:                                nodeByName[node2].nodeID,
									},
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: node3,
									Annotations: map[string]string{
										"k8s.ovn.org/node-transit-switch-port-ifaddr": fmt.Sprintf(`{"ipv4": %q, "ipv6": %q}`, nodeByName[node3].transitSwitchPortIPv4, nodeByName[node3].transitSwitchPortIPv6),
										"k8s.ovn.org/node-subnets":                    fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[node3].subnetIPv4, nodeByName[node3].subnetIPv6),
										"k8s.ovn.org/l3-gateway-config":               fmt.Sprintf(`{"default": {"mode": "local", "mac-address":"7e:57:f8:f0:3c:53", "ip-addresses":[%q, %q]}}`, nodeByName[node3].addressIPv4, nodeByName[node3].addressIPv6),
										"k8s.ovn.org/node-chassis-id":                 chassisIDForNode(node3),
										util.OvnNodeID:                                nodeByName[node3].nodeID,
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
				fakeOvn.controller.nodeName = localNode(t)

				Expect(fakeOvn.controller.WatchNamespaces()).ToNot(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).ToNot(HaveOccurred())

				virtLauncherPodToCreate := sourceVirtLauncherPod
				podToCreate := sourcePod
				if podToCreate != nil {
					if t.migrationTarget.nodeName != "" {
						virtLauncherPodToCreate = t.migrationTarget.testVirtLauncherPod
						podToCreate = newPodFromTestVirtLauncherPod(virtLauncherPodToCreate)
						podToCreate.Labels = t.migrationTarget.labels
						podToCreate.Annotations = t.migrationTarget.annotations
					}
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), podToCreate.Name, metav1.GetOptions{})
					Expect(err).To(MatchError(apierrors.IsNotFound, "IsNotFound"))

					podToCreate.CreationTimestamp = metav1.NewTime(time.Now())
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), podToCreate, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func() error {
						_, err := fakeOvn.controller.watchFactory.GetPod(podToCreate.Namespace, podToCreate.Name)
						return err
					}).
						WithTimeout(time.Minute).
						WithPolling(time.Second).
						Should(Succeed(), "should fill in the cache with the pod")

					// Change the phase by patching the status subresource to
					// emulate the logic of transition. Using Patch instead of
					// UpdateStatus avoids overwriting controller annotations
					// (the fake client's UpdateStatus replaces the entire object).
					if virtLauncherPodToCreate.updatePhase != nil {
						patch := []byte(fmt.Sprintf(`{"status":{"phase":%q}}`, *virtLauncherPodToCreate.updatePhase))
						_, err = fakeOvn.fakeClient.KubeClient.CoreV1().
							Pods(t.namespace).
							Patch(
								context.TODO(),
								podToCreate.Name,
								ktypes.MergePatchType,
								patch,
								metav1.PatchOptions{},
								"status",
							)
						Expect(err).NotTo(HaveOccurred())
						Eventually(func() (corev1.PodPhase, error) {
							updatedPod, err := fakeOvn.controller.watchFactory.GetPod(podToCreate.Namespace, podToCreate.Name)
							return updatedPod.Status.Phase, err
						}).
							WithTimeout(time.Minute).
							WithPolling(time.Second).
							Should(Equal(*virtLauncherPodToCreate.updatePhase), "should be in the updated phase")

					}
				}

				expectedOVN := []libovsdb.TestData{}

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
					expectedOVN = append(expectedOVN, ComposeDHCPv4Options(dhcpv4OptionsUUID+d.hostname, t.namespace, t.nodeName, &d))
				}

				for _, d := range t.expectedDhcpv6 {
					expectedOVN = append(expectedOVN, ComposeDHCPv6Options(dhcpv6OptionsUUID+d.hostname, t.namespace, &d))
				}
				expectedSourceLSRP := migrationSourceLSRP.DeepCopy()
				expectedGWRouter := gwRouter.DeepCopy()
				expectedOVN = append(expectedOVN,
					expectedOVNClusterRouter,
					expectedGWRouter,
					logicalRouterPort,
					expectedSourceLSRP,
				)
				expectedOVN = kubevirtOVNTestData(t, expectedOVN)

				var expectedMigrationTargetGWRouter *nbdb.LogicalRouter
				if t.migrationTarget.nodeName != "" {
					expectedTargetLSRP := migrationTargetLSRP.DeepCopy()
					expectedMigrationTargetGWRouter = migrationTargetGWRouter.DeepCopy()
					expectedOVN = append(expectedOVN,
						migrationTargetLRP,
						expectedTargetLSRP,
						expectedMigrationTargetGWRouter,
					)
				}

				for router, testpod := range map[*nbdb.LogicalRouter]testVirtLauncherPod{expectedGWRouter: t.testVirtLauncherPod, expectedMigrationTargetGWRouter: t.migrationTarget.testVirtLauncherPod} {
					if testpod.nodeName == fakeOvn.controller.nodeName && router != nil && testpod.podName != "" {
						natIDs, nats := composeNats(testpod)
						router.Nat = append(router.Nat, natIDs...)
						for _, nat := range nats {
							expectedOVN = append(expectedOVN, nat)
						}
					}
				}

				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedOVN), "should populate ovn")

				if t.replaceNode != "" {
					By("Replace vm node with newNode at the logical switch manager")
					newNode := &corev1.Node{
						ObjectMeta: metav1.ObjectMeta{
							Name: "newNode1",
							Annotations: map[string]string{
								"k8s.ovn.org/node-subnets": fmt.Sprintf(`{"default":[%q,%q]}`, nodeByName[t.replaceNode].subnetIPv4, nodeByName[t.replaceNode].subnetIPv6),
								util.OvnNodeID:             nodeByName[t.replaceNode].nodeID,
							},
						},
					}
					fakeOvn.controller.lsManager.DeleteSwitch(t.replaceNode)

					// We don't have portgroup
					fakeOvn.controller.multicastSupport = false

					podToCreate, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), podToCreate.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					podAnnotation, err := util.UnmarshalPodAnnotation(podToCreate.Annotations, ovntypes.DefaultNetworkName)
					Expect(err).ToNot(HaveOccurred())

					_, err = fakeOvn.controller.addNode(newNode)
					Expect(err).ToNot(HaveOccurred())

					Expect(fakeOvn.controller.lsManager.AllocateIPs(newNode.Name, "test-owner", podAnnotation.IPs)).ToNot(Succeed(), "should allocate the pod IPs when node is replaced")

					// Make ovn expectations after pod deletion happy
					Expect(libovsdbops.DeleteLogicalSwitchPorts(fakeOvn.nbClient, &nbdb.LogicalSwitch{Name: newNode.Name}, &nbdb.LogicalSwitchPort{Name: "stor-newNode1"})).To(Succeed())
					Expect(fakeOvn.controller.deleteNodeEvent(newNode)).ToNot(HaveOccurred())
					Expect(err).ToNot(HaveOccurred())
				}

				var vmIPs []string
				if t.addressIPv4 != "" {
					vmIPs = append(vmIPs, t.addressIPv4+subnetSuffixIPv4)
				}
				if t.addressIPv6 != "" {
					vmIPs = append(vmIPs, t.addressIPv6+subnetSuffixIPv6)
				}
				vmIPNets := testing.MustParseIPNets(vmIPs...)
				subnet, checkRelease := fakeOvn.controller.lsManager.GetSubnetName(vmIPNets)

				if t.podName == "" {
					return nil
				}

				completeAndDeletePod := func(namespace, name string) {
					GinkgoHelper()
					pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					pod.Status.Phase = corev1.PodSucceeded
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				deleteFirst := t.testVirtLauncherPod
				deleteSecond := t.migrationTarget.testVirtLauncherPod
				deleteFirstRouter := gwRouter
				hasMigration := t.migrationTarget.nodeName != ""
				hasUnsuccesfulMigration := hasMigration && t.migrationTarget.updatePhase != nil
				if hasUnsuccesfulMigration {
					deleteFirst = t.migrationTarget.testVirtLauncherPod
					deleteFirstRouter = migrationTargetGWRouter
					deleteSecond = t.testVirtLauncherPod
				}

				completeAndDeletePod(t.namespace, deleteFirst.podName)
				if !hasMigration {
					if checkRelease {
						Eventually(fakeOvn.controller.lsManager.AllocateIPs).
							WithArguments(subnet, "test-owner", vmIPNets).
							Should(Succeed(), "should have de-allocated VM IP after termination")
					}
					Eventually(fakeOvn.nbClient).Should(
						libovsdb.HaveData(expectedNBDBAfterCleanup(expectedStaticRoutes, nil)),
						"should cleanup terminated pod data from ovn",
					)
					return nil
				}
				if checkRelease {
					Consistently(fakeOvn.controller.lsManager.AllocateIPs).
						WithArguments(subnet, "test-owner", vmIPNets).
						ShouldNot(Succeed(), "should have not de-allocated VM IP after migration")
				}

				Eventually(fakeOvn.nbClient).Should(
					libovsdb.HaveData(filterOutStaleVirtLauncherExpectedTestData(t.namespace, deleteFirst.podName, expectedOVN)),
					"should cleanup source pod data from ovn",
				)

				completeAndDeletePod(t.namespace, deleteSecond.podName)
				if checkRelease {
					Eventually(fakeOvn.controller.lsManager.AllocateIPs).
						WithArguments(subnet, "test-owner", vmIPNets).
						Should(Succeed(), "should have de-allocated target VM IP after termination")
				}

				// FIXME: for some reason we don't remove stale NATs of migrated
				// VMs. One possible reason is if VMs can migrate within the
				// same node and we can race between creation and deletion. Can
				// it happen?
				// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5627
				expectedNATs := map[string][]*nbdb.NAT{}
				if deleteFirst.nodeName == fakeOvn.controller.nodeName {
					_, nats := composeNats(deleteFirst)
					expectedNATs[deleteFirstRouter.Name] = nats
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdb.HaveData(expectedNBDBAfterCleanup(expectedStaticRoutes, expectedNATs)),
					"should cleanup terminated target pod data from ovn",
				)

				return nil
			}
			err = app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		},
			Entry("for single stack ipv4 at local zone", testData{
				ipv4:                true,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4},
				dnsServiceIPs:       []string{dnsServiceIPv4},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
				expectedDhcpv4: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv4,
					dns:      dnsServiceIPv4,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv4 at remote zone", testData{
				ipv4:                true,
				localNode:           node2,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4},
				dnsServiceIPs:       []string{dnsServiceIPv4},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
			}),
			Entry("for single stack ipv6 at local zone", testData{
				ipv6:                true,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
				expectedDhcpv6: []testDHCPOptions{{
					cidr:     nodeByName[node1].subnetIPv6,
					dns:      dnsServiceIPv6,
					hostname: vm1,
				}},
			}),
			Entry("for single stack ipv6 at remote zone", testData{
				ipv6:                true,
				localNode:           node2,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
			}),
			Entry("for dual stack at local zone", testData{
				ipv4:                true,
				ipv6:                true,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
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
				ipv4:                true,
				ipv6:                true,
				localNode:           node2,
				lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs:       []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: virtLauncher1(node1, vm1),
			}),

			Entry("for pre-copy live migration at local zone", testData{
				ipv4:          true,
				ipv6:          true,
				localNode:     node2,
				lrpNetworks:   []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node1,
					},
					vmName: vm1,
				},
				replaceNode: node1,
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node2, vm1),
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
			Entry("for pre-copy live migration between zones that do not own the original subnet", testData{
				ipv4:          true,
				ipv6:          true,
				localNode:     node2,
				lrpNetworks:   []string{nodeByName[node3].lrpNetworkIPv4, nodeByName[node3].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node3,
					},
					vmName: vm1,
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node2, vm1),
				},
				staticRoutes: []testStaticRoute{
					{
						prefix:  vmByName[vm1].addressIPv4,
						nexthop: strings.Split(nodeByName[node3].transitSwitchPortIPv4, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
					{
						prefix:  vmByName[vm1].addressIPv6,
						nexthop: strings.Split(nodeByName[node3].transitSwitchPortIPv6, "/")[0],
						zone:    kubevirt.OvnRemoteZone,
					},
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
				ipv4:          true,
				ipv6:          true,
				lrpNetworks:   []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node2,
					},
					vmName: vm1,
				},
				replaceNode: node1,
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
			Entry("for pre-copy live migration to node owning subnet at remote zone", testData{
				ipv4:          true,
				ipv6:          true,
				localNode:     node2,
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
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "2",
					testPod: testPod{
						nodeName: node1,
					},
					vmName: vm1,
				},
			}),
			Entry("for pre-copy live migration to node owning subnet at local", testData{
				ipv4:          true,
				ipv6:          true,
				lrpNetworks:   []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
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
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node2,
					},
					vmName:             vm1,
					skipPodAnnotations: false, /* add ovn pod annotation */
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks:         []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2(node1, vm1),
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
			Entry("for post-copy live migration", testData{
				ipv4:          true,
				ipv6:          true,
				localNode:     node2,
				lrpNetworks:   []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node1,
					},
					vmName:             vm1,
					skipPodAnnotations: false,
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2WithExtraAnnotations(node2, vm1, map[string]string{
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
			Entry("for live migration in progress", testData{
				ipv4:          true,
				ipv6:          true,
				localNode:     node2,
				lrpNetworks:   []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node1,
					},
					vmName:             vm1,
					skipPodAnnotations: false,
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
					testVirtLauncherPod: virtLauncher2WithExtraLabels(node2, vm1, map[string]string{
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
			Entry("for failing live migration", testData{
				ipv4:          true,
				ipv6:          true,
				lrpNetworks:   []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node2,
					},
					vmName:             vm1,
					skipPodAnnotations: false, /* add ovn pod annotation */
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
					testVirtLauncherPod: testVirtLauncherPod{
						suffix:             "2",
						testPod:            testPod{nodeName: node1},
						vmName:             vm1,
						skipPodAnnotations: true,
						extraLabels:        map[string]string{kubevirtv1.NodeNameLabel: node2},
						updatePhase:        phasePointer(corev1.PodSucceeded),
					},
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
			Entry("for failing target virt-launcher after live migration", testData{
				ipv4:          true,
				ipv6:          true,
				lrpNetworks:   []string{nodeByName[node2].lrpNetworkIPv4, nodeByName[node2].lrpNetworkIPv6},
				dnsServiceIPs: []string{dnsServiceIPv4, dnsServiceIPv6},
				testVirtLauncherPod: testVirtLauncherPod{
					suffix: "1",
					testPod: testPod{
						nodeName: node2,
					},
					vmName:             vm1,
					skipPodAnnotations: false, /* add ovn pod annotation */
				},
				migrationTarget: testMigrationTarget{
					lrpNetworks: []string{nodeByName[node1].lrpNetworkIPv4, nodeByName[node1].lrpNetworkIPv6},
					testVirtLauncherPod: testVirtLauncherPod{
						suffix:             "2",
						testPod:            testPod{nodeName: node1},
						vmName:             vm1,
						skipPodAnnotations: true,
						extraLabels:        map[string]string{kubevirtv1.NodeNameLabel: node2},
						updatePhase:        phasePointer(corev1.PodFailed),
					},
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
			Entry("should remove orphan routes and policies and keep not kubevirt related on startup", testData{
				ipv4: true,
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
