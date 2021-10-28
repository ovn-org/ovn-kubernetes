// +build linux

package node

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
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

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func setupNodeAccessBridgeTest(fexec *ovntest.FakeExec, nodeName, brLocalnetMAC, mtu string) {
	gwPortMac := util.IPAddrToHWAddr(net.ParseIP(types.V4NodeLocalNATSubnetNextHop)).String()

	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --may-exist add-br br-local",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface br-local mac_in_use",
		Output: brLocalnetMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set bridge br-local other-config:hwaddr=" + brLocalnetMAC,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
		Output: types.PhysicalNetworkName + ":breth0",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0" + "," + types.LocalNetworkName + ":br-local",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --may-exist add-port br-local " + localnetGatewayNextHopPort +
			" -- set interface " + localnetGatewayNextHopPort + " type=internal mtu_request=" + mtu + " mac=" + strings.ReplaceAll(gwPortMac, ":", "\\:"),
	})
}

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

		k := &kube.Kube{fakeClient.KubeClient, egressIPFakeClient, egressFirewallFakeClient}

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
				&fakeMgmtPortConfig, nil)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(wf)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			go sharedGw.Run(stop, &sync.WaitGroup{})

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
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OUTPUT": []string{
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OVN-KUBE-NODEPORT":   []string{},
				"OVN-KUBE-EXTERNALIP": []string{},
			},
		}
		f4 := iptV4.(*util.FakeIPTables)
		err = f4.MatchState(expectedTables)
		Expect(err).NotTo(HaveOccurred())

		expectedTables = map[string]util.FakeTable{
			"nat": {},
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

func shareGatewayInterfaceSmartNICTest(app *cli.App, testNS ns.NetNS,
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
		// GetSmartNICHostInterface
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
		// newSharedGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-" + brphys + "_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + uplinkPort + " ofport",
			Output: "7",
		})
		// GetSmartNICHostInterface
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
		// newSharedGatewayOpenFlowManager
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
		fakeClient := &util.OVNClientset{
			KubeClient:           kubeFakeClient,
			EgressFirewallClient: egressFirewallFakeClient,
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
		defer func() {
			close(stop)
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{fakeClient.KubeClient, egressIPFakeClient, egressFirewallFakeClient}

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
				gatewayIntf, "", gwIPs, nodeAnnotator, k, &fakeMgmtPortConfig, nil)

			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(wf)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			go sharedGw.Run(stop, &sync.WaitGroup{})
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
		"--ovnkube-node-mode=" + types.NodeModeSmartNIC,
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}

func shareGatewayInterfaceSmartNICHostTest(app *cli.App, testNS ns.NetNS, uplinkName, hostIP, gwIP string) {
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

			err := n.initGatewaySmartNicHost(net.ParseIP(hostIP))
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
		"--ovnkube-node-mode=smart-nic-host",
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}

/* FIXME with updated local gw mode
func localNetInterfaceTest(app *cli.App, testNS ns.NetNS,
	subnets []*net.IPNet, brNextHopCIDRs []*netlink.Addr, ipts []*util.FakeIPTables,
	expectedIPTablesRules []map[string]util.FakeTable) {

	const mtu string = "1234"

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName      string = "node1"
			brLocalnetMAC string = "11:22:33:44:55:66"
			systemID      string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
		)

		fexec := ovntest.NewLooseCompareFakeExec()
		fakeOvnNode := NewFakeOVNNode(fexec)

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --may-exist add-br br-local",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface br-local mac_in_use",
			Output: brLocalnetMAC,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set bridge br-local other-config:hwaddr=" + brLocalnetMAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":br-local",
			"ovs-vsctl --timeout=15 --if-exists del-port br-local " + legacyLocalnetGatewayNextHopPort +
				" -- --may-exist add-port br-local " + localnetGatewayNextHopPort + " -- set interface " + localnetGatewayNextHopPort + " type=internal mtu_request=" + mtu + " mac=00\\:00\\:a9\\:fe\\:21\\:01",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ip rule",
			Output: "0:	from all lookup local\n32766:	from all lookup main\n32767:	from all lookup default\n",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ip rule add from all table " + localnetGatewayExternalIDTable,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ip route list table " + localnetGatewayExternalIDTable,
		})

		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		}}

		fakeOvnNode.start(ctx,
			&v1.NodeList{
				Items: []v1.Node{
					existingNode,
				},
			},
		)

		nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeOvnNode.fakeClient.KubeClient, &egressipfake.Clientset{}, &egressfirewallfake.Clientset{}}, existingNode.Name)
		err := util.SetNodeHostSubnetAnnotation(nodeAnnotator, subnets)
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			config.IPv4Mode = false
			config.IPv6Mode = false
			for _, subnet := range subnets {
				if utilnet.IsIPv6CIDR(subnet) {
					config.IPv6Mode = true
				} else {
					config.IPv4Mode = true
				}
			}

			gw, err := newLocalGateway(fakeOvnNode.watcher, fakeNodeName, subnets, nodeAnnotator, fakeOvnNode.recorder, primaryLinkName)
			Expect(err).NotTo(HaveOccurred())
			startGateway(gw, fakeOvnNode.watcher)

			// Check if IP has been assigned to LocalnetGatewayNextHopPort
			link, err := netlink.LinkByName(localnetGatewayNextHopPort)
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(link, syscall.AF_UNSPEC)
			Expect(err).NotTo(HaveOccurred())

			var foundAddr bool
			for _, expectedAddr := range brNextHopCIDRs {
				foundAddr = false
				for _, a := range addrs {
					if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
						foundAddr = true
						break
					}
				}
				Expect(foundAddr).To(BeTrue())
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

		for i := 0; i < len(ipts); i++ {
			err = ipts[i].MatchState(expectedIPTablesRules[i])
			Expect(err).NotTo(HaveOccurred())
		}
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--init-gateways",
		"--gateway-local",
		"--nodeport",
		"--mtu=" + mtu,
	})
	Expect(err).NotTo(HaveOccurred())
}
*/

func expectedIPTablesRules(gatewayIP string) map[string]util.FakeTable {
	table := map[string]util.FakeTable{
		"filter": {
			"INPUT": []string{
				"-i " + localnetGatewayNextHopPort + " -m comment --comment from OVN to localhost -j ACCEPT",
			},
			"FORWARD": []string{
				"-o " + localnetGatewayNextHopPort + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
				"-i " + localnetGatewayNextHopPort + " -j ACCEPT",
			},
		},
		"nat": {
			"POSTROUTING": []string{
				"-s " + gatewayIP + " -j MASQUERADE",
			},
			"OUTPUT": []string{
				"-j OVN-KUBE-EXTERNALIP",
				"-j OVN-KUBE-NODEPORT",
			},
			"OVN-KUBE-NODEPORT":   []string{},
			"OVN-KUBE-EXTERNALIP": []string{},
		},
	}

	// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
	table["filter"]["FORWARD"] = append(table["filter"]["FORWARD"],
		"-p tcp -m tcp --dport 22624 --syn -j REJECT",
		"-p tcp -m tcp --dport 22623 --syn -j REJECT",
	)
	table["filter"]["OUTPUT"] = append(table["filter"]["OUTPUT"],
		"-p tcp -m tcp --dport 22624 --syn -j REJECT",
		"-p tcp -m tcp --dport 22623 --syn -j REJECT",
	)
	// END OCP HACK

	return table
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

		// Set up a fake br-local & LocalnetGatewayNextHopPort
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			ovntest.AddLink("br-local")
			ovntest.AddLink(localnetGatewayNextHopPort)

			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	/* FIXME for updated local gw mode
	Context("for localnet operations", func() {
		const (
			v4BrNextHopIP       = "169.254.33.1"
			v4BrNextHopCIDR     = v4BrNextHopIP + "/24"
			v4NodeSubnet        = "10.1.1.0/24"
			v4localnetGatewayIP = "169.254.33.2"

			v6BrNextHopIP       = "fd99::1"
			v6BrNextHopCIDR     = v6BrNextHopIP + "/64"
			v6NodeSubnet        = "2001:db8:abcd:0012::0/64"
			v6localnetGatewayIP = "fd99::2"
		)
		var (
			brNextHopCIDRs []*netlink.Addr
			ipts           []*util.FakeIPTables
			ipTablesRules  []map[string]util.FakeTable
		)

		It("sets up a IPv4 localnet gateway", func() {
			nextHopCIDRIPv4, err := netlink.ParseAddr(v4BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())
			brNextHopCIDRs := append(brNextHopCIDRs, nextHopCIDRIPv4)

			v4ipt, _ := util.SetFakeIPTablesHelpers()
			ipts := append(ipts, v4ipt.(*util.FakeIPTables))
			ipTablesRules := append(ipTablesRules, expectedIPTablesRules(v4localnetGatewayIP))

			localNetInterfaceTest(app, testNS, ovntest.MustParseIPNets(v4NodeSubnet), brNextHopCIDRs, ipts, ipTablesRules)
		})

		It("sets up a IPv6 localnet gateway", func() {
			nextHopCIDRIPv6, err := netlink.ParseAddr(v6BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())
			brNextHopCIDRs := append(brNextHopCIDRs, nextHopCIDRIPv6)

			_, v6ipt := util.SetFakeIPTablesHelpers()
			ipts := append(ipts, v6ipt.(*util.FakeIPTables))
			ipTablesRules := append(ipTablesRules, expectedIPTablesRules(v6localnetGatewayIP))

			localNetInterfaceTest(app, testNS, ovntest.MustParseIPNets(v6NodeSubnet), brNextHopCIDRs, ipts, ipTablesRules)
		})

		It("sets up a dual stack localnet gateway", func() {
			nextHopCIDRIPv4, err := netlink.ParseAddr(v4BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())
			brNextHopCIDRs := append(brNextHopCIDRs, nextHopCIDRIPv4)
			nextHopCIDRIPv6, err := netlink.ParseAddr(v6BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())
			brNextHopCIDRs = append(brNextHopCIDRs, nextHopCIDRIPv6)

			v4ipt, v6ipt := util.SetFakeIPTablesHelpers()
			ipts := append(ipts, v4ipt.(*util.FakeIPTables))
			ipts = append(ipts, v6ipt.(*util.FakeIPTables))
			ipTablesRules := append(ipTablesRules, expectedIPTablesRules(v4localnetGatewayIP))
			ipTablesRules = append(ipTablesRules, expectedIPTablesRules(v6localnetGatewayIP))

			localNetInterfaceTest(app, testNS, ovntest.MustParseIPNets(v4NodeSubnet, v6NodeSubnet), brNextHopCIDRs,
				ipts, ipTablesRules)
		})
	})
	*/

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

		It("sets up a shared interface gateway", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 0)
		})

		It("sets up a shared interface gateway with tagged VLAN", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0IP, eth0GWIP, eth0CIDR, 3000)
		})
	})
})

var _ = Describe("Gateway Operations Smart-NIC", func() {
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

	Context("Smart-NIC Operations", func() {
		const (
			brphys       string = "brp0"
			smartNICIP   string = "192.168.1.101"
			hostIP       string = "192.168.1.10"
			hostMAC      string = "aa:bb:cc:dd:ee:ff"
			hostCIDR     string = hostIP + "/24"
			smartNICCIDR string = smartNICIP + "/24"
			gwIP         string = "192.168.1.1"
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
				addr, err := netlink.ParseAddr(smartNICCIDR)
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

		It("sets up a shared interface gateway Smart-NIC", func() {
			shareGatewayInterfaceSmartNICTest(app, testNS, brphys, hostMAC, hostCIDR)
		})
	})

	Context("Smart-NIC Host Operations", func() {
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

		It("sets up a shared interface gateway Smart-NIC host", func() {
			shareGatewayInterfaceSmartNICHostTest(app, testNS, uplinkName, hostIP, gwIP)
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

	Context("getSmartNICHostPrimaryIPAddresses", func() {

		It("returns Gateway IP/Subnet for kubernetes node IP", func() {
			_, smartNicSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(24, 32)},
			}
			gwSubnet, err := getSmartNICHostPrimaryIPAddresses(nodeIP, []*net.IPNet{smartNicSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in host subnets", func() {
			_, smartNicSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.1.11")
			_, err := getSmartNICHostPrimaryIPAddresses(nodeIP, []*net.IPNet{smartNicSubnet})
			Expect(err).To(HaveOccurred())
		})

		It("returns node IP with config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, smartNicSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.1.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(16, 32)},
			}
			gwSubnet, err := getSmartNICHostPrimaryIPAddresses(nodeIP, []*net.IPNet{smartNicSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, smartNicSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			_, err := getSmartNICHostPrimaryIPAddresses(nodeIP, []*net.IPNet{smartNicSubnet})
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
			netlinkMock.On("RouteAdd", expectedRoute).Return(nil)

			err = configureSvcRouteViaInterface("ens1f0", gwIPs)
			Expect(err).ToNot(HaveOccurred())
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
			netlinkMock.On("RouteAdd", mock.Anything).Return(fmt.Errorf("failed to add route"))

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
