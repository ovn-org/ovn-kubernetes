//go:build linux
// +build linux

package node

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"

	"github.com/Mellanox/sriovnet"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	linkMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"

	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func shareGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR string, gatewayVLANID uint) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"
	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		fexec := ovntest.NewLooseCompareFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 -- --may-exist add-br breth0 -- br-set-external-id breth0 bridge-id breth0 -- br-set-external-id breth0 bridge-uplink eth0 -- set bridge breth0 fail-mode=standalone other_config:hwaddr=" + eth0MAC + " -- --may-exist add-port breth0 eth0 -- set port eth0 other-config:transient=true",
			Action: func() error {
				return testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					// Create breth0 as a dummy link
					err := netlink.LinkAdd(&netlink.Dummy{
						LinkAttrs: netlink.LinkAttrs{
							Name:         "br" + eth0Name,
							HardwareAddr: ovntest.MustParseMAC(eth0MAC),
						},
					})
					Expect(err).NotTo(HaveOccurred())
					_, err = netlink.LinkByName("br" + eth0Name)
					Expect(err).NotTo(HaveOccurred())
					return nil
				})
			},
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set bridge breth0 other-config:hwaddr=" + eth0MAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
		})

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features breth0",
			Output: "Check pkt length action: Yes",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
			Output: "7",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 172.16.1.0/24 via 10.1.1.1 dev ovn-k8s-mp0",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
			Output: "0",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
		})
		// nodePortWatcher()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface patch-breth0_" + nodeName + "-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
			Output: "7",
		})
		// syncServices()

		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		}}

		_, nodeNet, err := net.ParseCIDR(nodeSubnet)
		Expect(err).NotTo(HaveOccurred())

		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
			ipt:        nil,
			allSubnets: nil,
			ifAddr:     nodeNet,
			gwIP:       nodeNet.IP,
		}

		fakeMgmtPortConfig := managementPortConfig{
			ifName:    nodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortIPFamilyConfig,
			ipv6:      nil,
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{fakeClient.KubeClient, egressIPFakeClient, egressFirewallFakeClient, nil}

		iptV4, iptV6 := util.SetFakeIPTablesHelpers()

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			sharedGw, err := newSharedGateway(nodeName, ovntest.MustParseIPNets(nodeSubnet), gatewayNextHops, gatewayIntf, "", nil, nodeAnnotator, k,
				&fakeMgmtPortConfig, wf)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(wf)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			sharedGw.Start(stop, wg)

			// Verify the code moved eth0's IP address, MAC, and routes
			// over to breth0
			l, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(l, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found bool
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			for _, a := range addrs {
				if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))
			return nil
		})

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		expectedTables := map[string]util.FakeTable{
			"nat": {
				"PREROUTING": []string{
					"-j OVN-KUBE-ETP",
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OUTPUT": []string{
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-NODEPORT":      []string{},
				"OVN-KUBE-EXTERNALIP":    []string{},
				"OVN-KUBE-SNAT-MGMTPORT": []string{},
				"OVN-KUBE-ETP":           []string{},
				"OVN-KUBE-ITP":           []string{},
			},
			"filter": {},
			"mangle": {
				"OUTPUT": []string{
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-ITP": []string{},
			},
		}
		// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
		expectedMCSRules := []string{
			"-p tcp -m tcp --dport 22624 --syn -j REJECT",
			"-p tcp -m tcp --dport 22623 --syn -j REJECT",
		}
		expectedTables["filter"]["FORWARD"] = append(expectedMCSRules, expectedTables["filter"]["FORWARD"]...)
		expectedTables["filter"]["OUTPUT"] = append(expectedMCSRules, expectedTables["filter"]["OUTPUT"]...)
		// END OCP HACK
		f4 := iptV4.(*util.FakeIPTables)
		err = f4.MatchState(expectedTables)
		Expect(err).NotTo(HaveOccurred())

		expectedTables = map[string]util.FakeTable{
			"nat":    {},
			"filter": {},
			"mangle": {},
		}
		f6 := iptV6.(*util.FakeIPTables)
		err = f6.MatchState(expectedTables)
		Expect(err).NotTo(HaveOccurred())
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + eth0Name,
		"--nodeport",
		"--gateway-vlanid=" + fmt.Sprintf("%d", gatewayVLANID),
		"--mtu=" + mtu,
	})
	Expect(err).NotTo(HaveOccurred())
}

func shareGatewayInterfaceDPUTest(app *cli.App, testNS ns.NetNS,
	brphys, hostMAC, hostCIDR string) {
	const mtu string = "1400"
	const clusterCIDR string = "10.1.0.0/16"
	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
			uplinkPort string = "p0"
			uplinkMAC  string = "11:22:33:44:55:66"
			hostRep    string = "pf0hpf"
		)

		// sriovnet mocks
		sriovnetMock := &utilMock.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetMock)
		sriovnetMock.On("GetRepresentorPortFlavour", hostRep).Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)
		sriovnetMock.On("GetRepresentorPeerMacAddress", hostRep).Return(ovntest.MustParseMAC(hostMAC), nil)

		// exec Mocks
		fexec := ovntest.NewLooseCompareFakeExec()
		// gatewayInitInternal
		// bridgeForInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br " + brphys,
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists " + brphys,
			Err: nil,
		})
		// getIntfName
		// GetNicName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: "p0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port " + uplinkPort + " Interfaces",
			Output: "p0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface " + uplinkPort + " Type",
			Output: "system",
		})
		// getIntfName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 get interface p0 ofport",
		})
		// bridgedGatewayNodeSetup
		// GetOVSPortMACAddress
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + brphys + " mac_in_use",
			Output: uplinkMAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":" + brphys,
		})
		// GetNodeChassisID
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		// DetectCheckPktLengthSupport
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features " + brphys,
			Output: "Check pkt length action: Yes",
		})
		// GetDPUHostInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port " + hostRep + " Interfaces",
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface " + hostRep + " Name",
			Output: hostRep,
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-" + brphys + "_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + uplinkPort + " ofport",
			Output: "7",
		})
		// GetDPUHostInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port pf0hpf Interfaces",
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface pf0hpf Name",
			Output: hostRep,
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + hostRep + " ofport",
			Output: "9",
		})
		// cleanup flows
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows " + brphys + " -",
		})
		// nodePortWatcher()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface patch-" + brphys + "_" + nodeName + "-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + uplinkPort + " ofport",
			Output: "7",
		})
		// syncServices()

		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		}}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
		}

		_, nodeNet, err := net.ParseCIDR(nodeSubnet)
		Expect(err).NotTo(HaveOccurred())

		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
			ipt:        nil,
			allSubnets: nil,
			ifAddr:     nodeNet,
			gwIP:       nodeNet.IP,
		}

		fakeMgmtPortConfig := managementPortConfig{
			ifName:    nodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortIPFamilyConfig,
			ipv6:      nil,
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{fakeClient.KubeClient, egressIPFakeClient, egressFirewallFakeClient, nil}

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			// provide host IP as GR IP
			gwIPs := []*net.IPNet{ovntest.MustParseIPNet(hostCIDR)}
			sharedGw, err := newSharedGateway(nodeName, ovntest.MustParseIPNets(nodeSubnet), gatewayNextHops,
				gatewayIntf, "", gwIPs, nodeAnnotator, k, &fakeMgmtPortConfig, wf)

			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(wf)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			sharedGw.Start(stop, wg)
			return nil
		})

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		// ensure correct l3 gw config were set in Node annotation
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		l3gwConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
		Expect(err).To(Not(HaveOccurred()))
		Expect(l3gwConfig.MACAddress.String()).To(Equal(hostMAC))
		Expect(l3gwConfig.IPAddresses[0]).To(Equal(ovntest.MustParseIPNet(hostCIDR)))
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + brphys,
		"--nodeport",
		"--mtu=" + mtu,
		"--ovnkube-node-mode=" + types.NodeModeDPU,
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}

func shareGatewayInterfaceDPUHostTest(app *cli.App, testNS ns.NetNS, uplinkName, hostIP, gwIP string) {
	const (
		clusterCIDR string = "10.1.0.0/16"
		svcCIDR     string = "172.16.1.0/24"
		nodeName    string = "node1"
	)

	app.Action = func(ctx *cli.Context) error {
		fexec := ovntest.NewLooseCompareFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		}}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		n := OvnNode{watchFactory: wf}

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := n.initGatewayDPUHost(net.ParseIP(hostIP))
			Expect(err).NotTo(HaveOccurred())

			// Check svc and masquerade routes added towards eth0GWIP
			expectedRoutes := []string{svcCIDR, types.V4MasqueradeSubnet}
			link, err := netlink.LinkByName(uplinkName)
			Expect(err).NotTo(HaveOccurred())
			routes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
			for _, expRoute := range expectedRoutes {
				found := false
				for _, route := range routes {
					if route.Dst != nil {
						if route.Dst.String() == expRoute && route.Gw.String() == gwIP {
							found = true
						}
					}
				}
				Expect(found).To(BeTrue(), fmt.Sprintf("Expected route %s was not found", expRoute))
			}
			return nil
		})
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + uplinkName,
		"--k8s-service-cidrs=" + svcCIDR,
		"--ovnkube-node-mode=dpu-host",
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}

func localGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR string) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"
	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		fexec := ovntest.NewLooseCompareFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 -- --may-exist add-br breth0 -- br-set-external-id breth0 bridge-id breth0 -- br-set-external-id breth0 bridge-uplink eth0 -- set bridge breth0 fail-mode=standalone other_config:hwaddr=" + eth0MAC + " -- --may-exist add-port breth0 eth0 -- set port eth0 other-config:transient=true",
			Action: func() error {
				return testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					// Create breth0 as a dummy link
					err := netlink.LinkAdd(&netlink.Dummy{
						LinkAttrs: netlink.LinkAttrs{
							Name:         "br" + eth0Name,
							HardwareAddr: ovntest.MustParseMAC(eth0MAC),
						},
					})
					Expect(err).NotTo(HaveOccurred())
					_, err = netlink.LinkByName("br" + eth0Name)
					Expect(err).NotTo(HaveOccurred())
					return nil
				})
			},
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set bridge breth0 other-config:hwaddr=" + eth0MAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
		})

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features breth0",
			Output: "Check pkt length action: Yes",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
			Output: "7",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 172.16.1.0/24 via 10.1.1.1 dev ovn-k8s-mp0",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-ofctl show breth0",
			Output: `
OFPT_FEATURES_REPLY (xid=0x2): dpid:00000242ac120002
n_tables:254, n_buffers:0
capabilities: FLOW_STATS TABLE_STATS PORT_STATS QUEUE_STATS ARP_MATCH_IP
actions: output enqueue set_vlan_vid set_vlan_pcp strip_vlan mod_dl_src mod_dl_dst mod_nw_src mod_nw_dst mod_nw_tos mod_tp_src mod_tp_dst
 1(eth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     current:    10GB-FD COPPER
     speed: 10000 Mbps now, 0 Mbps max
 2(patch-breth0_ov): addr:8e:8d:f4:cd:4f:76
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
 LOCAL(breth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
OFPT_GET_CONFIG_REPLY (xid=0x4): frags=normal miss_send_len=0`,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
		})
		// nodePortWatcher()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface patch-breth0_" + nodeName + "-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
			Output: "7",
		})
		// syncServices()

		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		}}
		externalIP := "1.1.1.1"
		externalIPPort := int32(8032)
		service := *newService("service1", "namespace1", "10.129.0.2",
			[]v1.ServicePort{
				{
					Port:     externalIPPort,
					Protocol: v1.ProtocolTCP,
				},
			},
			v1.ServiceTypeClusterIP,
			[]string{externalIP},
			v1.ServiceStatus{},
			false, false,
		)
		endpoints := *newEndpoints("service1", "namespace1", []v1.EndpointSubset{})

		_, nodeNet, err := net.ParseCIDR(nodeSubnet)
		Expect(err).NotTo(HaveOccurred())

		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
			ipt:        nil,
			allSubnets: nil,
			ifAddr:     nodeNet,
			gwIP:       nodeNet.IP,
		}

		fakeMgmtPortConfig := managementPortConfig{
			ifName:    nodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortIPFamilyConfig,
			ipv6:      nil,
		}

		kubeFakeClient := fake.NewSimpleClientset(
			&v1.NodeList{
				Items: []v1.Node{existingNode},
			},
			&service,
			&endpoints,
		)
		egressFirewallFakeClient := &egressfirewallfake.Clientset{}
		egressIPFakeClient := &egressipfake.Clientset{}
		egressQoSFakeClient := &egressqosfake.Clientset{}
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
			EgressQoSClient:      egressQoSFakeClient,
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{fakeClient.KubeClient, egressIPFakeClient, egressFirewallFakeClient, nil}

		iptV4, iptV6 := util.SetFakeIPTablesHelpers()

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			localGw, err := newLocalGateway(nodeName, ovntest.MustParseIPNets(nodeSubnet), gatewayNextHops, gatewayIntf, "", nil,
				nodeAnnotator, &fakeMgmtPortConfig, k, wf)
			Expect(err).NotTo(HaveOccurred())
			err = localGw.Init(wf)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			localGw.Start(stop, wg)

			// Verify the code moved eth0's IP address, MAC, and routes
			// over to breth0
			l, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(l, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found bool
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			for _, a := range addrs {
				if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))
			return nil
		})

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		expectedTables := map[string]util.FakeTable{
			"nat": {
				"PREROUTING": []string{
					"-j OVN-KUBE-ETP",
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OUTPUT": []string{
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-NODEPORT": []string{},
				"OVN-KUBE-EXTERNALIP": []string{
					fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
				},
				"POSTROUTING": []string{
					"-s 10.1.1.0/24 -j MASQUERADE",
				},
				"OVN-KUBE-SNAT-MGMTPORT": []string{},
				"OVN-KUBE-ETP":           []string{},
				"OVN-KUBE-ITP":           []string{},
			},
			"filter": {
				"FORWARD": []string{
					"-o ovn-k8s-mp0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
					"-i ovn-k8s-mp0 -j ACCEPT",
				},
				"INPUT": []string{
					"-i ovn-k8s-mp0 -m comment --comment from OVN to localhost -j ACCEPT",
				},
			},
			"mangle": {
				"OUTPUT": []string{
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-ITP": []string{},
			},
		}
		// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
		expectedMCSRules := []string{
			"-p tcp -m tcp --dport 22624 --syn -j REJECT",
			"-p tcp -m tcp --dport 22623 --syn -j REJECT",
		}
		expectedTables["filter"]["FORWARD"] = append(expectedMCSRules, expectedTables["filter"]["FORWARD"]...)
		expectedTables["filter"]["OUTPUT"] = append(expectedMCSRules, expectedTables["filter"]["OUTPUT"]...)
		// END OCP HACK
		f4 := iptV4.(*util.FakeIPTables)
		err = f4.MatchState(expectedTables)
		Expect(err).NotTo(HaveOccurred())

		expectedTables = map[string]util.FakeTable{
			"nat":    {},
			"filter": {},
			"mangle": {},
		}
		f6 := iptV6.(*util.FakeIPTables)
		err = f6.MatchState(expectedTables)
		Expect(err).NotTo(HaveOccurred())
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--gateway-mode=local",
		"--gateway-interface=" + eth0Name,
		"--nodeport",
		"--mtu=" + mtu,
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Gateway Init Operations", func() {

	var (
		testNS ns.NetNS
		app    *cli.App
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	Context("Setting up gateway bridge", func() {
		const (
			eth0Name string = "eth0"
			eth0IP   string = "192.168.1.10"
			eth0CIDR string = eth0IP + "/24"
			eth0GWIP string = "192.168.1.1"
		)
		var eth0MAC string

		BeforeEach(func() {
			// Set up a fake eth0
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(eth0Name)

				l, err := netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(eth0CIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = l.Attrs().HardwareAddr.String()

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(eth0GWIP),
				})
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("newLocalGateway sets up a local interface gateway", func() {
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR)
		})

	})

	Context("for NIC-based operations", func() {
		const (
			eth0Name string = "eth0"
			eth0IP   string = "192.168.1.10"
			eth0CIDR string = eth0IP + "/24"
			eth0GWIP string = "192.168.1.1"
		)
		var eth0MAC string

		BeforeEach(func() {
			// Set up a fake eth0
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(eth0Name)

				l, err := netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(eth0CIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = l.Attrs().HardwareAddr.String()

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(eth0GWIP),
				})
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 0)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 3000)
		})

		config.Gateway.Interface = eth0Name
		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 0)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN + predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 3000)
		})
	})
})

var _ = Describe("Gateway Operations DPU", func() {
	var (
		testNS ns.NetNS
		app    *cli.App
	)

	BeforeEach(func() {
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// Restore global default values before each testcase
		config.PrepareTestConfig()
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		_, _ = util.SetFakeIPTablesHelpers()
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
	})

	Context("DPU Operations", func() {
		const (
			brphys   string = "brp0"
			dpuIP    string = "192.168.1.101"
			hostIP   string = "192.168.1.10"
			hostMAC  string = "aa:bb:cc:dd:ee:ff"
			hostCIDR string = hostIP + "/24"
			dpuCIDR  string = dpuIP + "/24"
			gwIP     string = "192.168.1.1"
		)

		BeforeEach(func() {
			// Create "bridge interface"
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(brphys)
				l, err := netlink.LinkByName(brphys)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(dpuCIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(gwIP),
				})
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway DPU", func() {
			shareGatewayInterfaceDPUTest(app, testNS, brphys, hostMAC, hostCIDR)
		})
	})

	Context("DPU Host Operations", func() {
		const (
			uplinkName string = "enp3s0f0"
			hostIP     string = "192.168.1.10"
			hostCIDR   string = hostIP + "/24"
			gwIP       string = "192.168.1.1"
		)

		BeforeEach(func() {
			// Create "uplink" interface
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(uplinkName)
				l, err := netlink.LinkByName(uplinkName)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(hostCIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(gwIP),
				})
				Expect(err).NotTo(HaveOccurred())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway DPU host", func() {
			shareGatewayInterfaceDPUHostTest(app, testNS, uplinkName, hostIP, gwIP)
		})
	})
})

var _ = Describe("Gateway unit tests", func() {
	var netlinkMock *utilMock.NetLinkOps
	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		config.PrepareTestConfig()
		netlinkMock = &utilMock.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkMock)
	})

	AfterEach(func() {
		util.SetNetLinkOpMockInst(origNetlinkInst)
	})

	Context("getDPUHostPrimaryIPAddresses", func() {

		It("returns Gateway IP/Subnet for kubernetes node IP", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(24, 32)},
			}
			gwSubnet, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in host subnets", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.1.11")
			_, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})

		It("returns node IP with config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.1.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(16, 32)},
			}
			gwSubnet, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			_, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("getInterfaceByIP", func() {
		It("Finds correct interface", func() {
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name: "ens1f0",
			}
			lnkIpnet1 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.11"),
				Mask: net.CIDRMask(24, 32),
			}
			lnkIpnet2 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.12"),
				Mask: net.CIDRMask(24, 32),
			}
			addrs := []netlink.Addr{{IPNet: lnkIpnet1}, {IPNet: lnkIpnet2}}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkList").Return([]netlink.Link{lnk}, nil)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("AddrList", lnk, mock.Anything).Return(addrs, nil)

			iface, err := getInterfaceByIP(net.ParseIP("10.0.0.12"))
			Expect(err).ToNot(HaveOccurred())
			Expect(iface).To(Equal(lnkAttr.Name))
		})

		It("Fails if interface not found", func() {
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name: "ens1f0",
			}
			lnkIpnet1 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.11"),
				Mask: net.CIDRMask(24, 32),
			}
			lnkIpnet2 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.12"),
				Mask: net.CIDRMask(24, 32),
			}
			addrs := []netlink.Addr{{IPNet: lnkIpnet1}, {IPNet: lnkIpnet2}}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkList").Return([]netlink.Link{lnk}, nil)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("AddrList", lnk, mock.Anything).Return(addrs, nil)

			_, err := getInterfaceByIP(net.ParseIP("10.0.1.12"))
			Expect(err).To(HaveOccurred())
		})

		It("Fails if link list call fails", func() {
			netlinkMock.On("LinkList").Return(nil, fmt.Errorf("failed to list links"))

			_, err := getInterfaceByIP(net.ParseIP("10.0.1.12"))
			Expect(err).To(HaveOccurred())
		})
	})

	Context("configureSvcRouteViaInterface", func() {

		It("Configures kubernetes service routes on interface", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			expectedRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU,
			}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			netlinkMock.On("RouteAdd", expectedRoute).Return(nil)

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Replaces previous kubernetes service routes on interface when MTU changes", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			previousRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU - 100,
			}

			expectedRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU,
			}

			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{*previousRoute}, nil)
			netlinkMock.On("RouteReplace", expectedRoute).Return(nil)

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if link route list fails", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}

			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			lnk.On("Attrs").Return(lnkAttr)

			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, fmt.Errorf("failed to list routes"))

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if link route add fails", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}

			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}

			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
			netlinkMock.On("RouteAdd", mock.Anything).Return(fmt.Errorf("failed to replace route"))

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if link route replace fails", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}

			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			previousRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU - 100,
			}

			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{*previousRoute}, nil)
			netlinkMock.On("RouteReplace", mock.Anything).Return(fmt.Errorf("failed to replace route"))

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if link set up fails", func() {
			netlinkMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf("failed to find interface"))
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			err := configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if IP family missmatch", func() {
			_, ipnet, err := net.ParseCIDR("fc00:123:456:15::/64")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}

			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			netlinkMock.On("LinkByName", mock.Anything).Return(nil, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})
	})
})
