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
	discovery "k8s.io/api/discovery/v1"
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

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func shareGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR string, gatewayVLANID uint, l netlink.Link, hwOffload, setNodeIP bool) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"

	var err error
	if len(eth0GWIP) > 0 {
		// And a default route
		err := testNS.Do(func(ns.NetNS) error {
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        ovntest.MustParseIP(eth0GWIP),
			}
			return netlink.RouteAdd(defRoute)
		})
		Expect(err).NotTo(HaveOccurred())
	}

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
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: fmt.Sprintf("%t", hwOffload),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
			Output: "7",
		})
		if setNodeIP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.10",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
			})
		}
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
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter=2",
			Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
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

		existingNode := v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
		}
		if setNodeIP {
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: expectedAddr.IP.String()}
			existingNode.Status = v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}}
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

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{KClient: kubeFakeClient}

		iptV4, iptV6 := util.SetFakeIPTablesHelpers()

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			ifAddrs := ovntest.MustParseIPNets(eth0CIDR)
			sharedGw, err := newSharedGateway(nc, ovntest.MustParseIPNets(nodeSubnet), nodeAnnotator, &fakeMgmtPortConfig)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init()
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			sharedGw.Start()

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

			// check that the masquerade route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)
		// Make sure that annotation 'k8s.ovn.org/gateway-mtu-support' is set to "false" (because hw-offload is true).
		if hwOffload {
			Eventually(func() bool {
				node, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), existingNode.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return node.Annotations["k8s.ovn.org/gateway-mtu-support"] == "false"
			}, 5).Should(BeTrue(), "invalid annotation, hw-offload is enabled but annotation "+
				"'k8s.ovn.org/gateway-mtu-support' != \"false\"")
		} else {
			Consistently(func() bool {
				node, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), existingNode.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return node.Annotations["k8s.ovn.org/gateway-mtu-support"] != "false"
			}, 5).Should(BeTrue(), "invalid annotation, hw-offload is disabled but found annotation "+
				"'k8s.ovn.org/gateway-mtu-support' with value == \"false\"")
		}

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
				"POSTROUTING": []string{
					"-j OVN-KUBE-EGRESS-SVC",
				},
				"OVN-KUBE-NODEPORT":      []string{},
				"OVN-KUBE-EXTERNALIP":    []string{},
				"OVN-KUBE-SNAT-MGMTPORT": []string{},
				"OVN-KUBE-ETP":           []string{},
				"OVN-KUBE-ITP":           []string{},
				"OVN-KUBE-EGRESS-SVC":    []string{"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN"},
			},
			"filter": {},
			"mangle": {
				"OUTPUT": []string{
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-ITP": []string{},
			},
		}
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

	err = app.Run([]string{
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
	brphys, hostMAC, hostCIDR, dpuIP string) {
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
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists get bridge " + brphys + " external_ids:ovn-bypass-cfg",
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
		// IsOvsHwOffloadEnabled
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
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
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.101",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
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

		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: dpuIP}
		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{KClient: kubeFakeClient}

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		ifAddrs := ovntest.MustParseIPNets(hostCIDR)
		ifAddrs[0].IP = ovntest.MustParseIP(dpuIP)

		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			sharedGw, err := newSharedGateway(nc, ovntest.MustParseIPNets(nodeSubnet), nodeAnnotator, &fakeMgmtPortConfig)

			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init()
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			sharedGw.Start()

			// check that the masquerade route was not added
			l, err := netlink.LinkByName(brphys)
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).To(BeNil())

			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		// ensure correct l3 gw config were set in Node annotation
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		l3gwConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
		Expect(err).To(Not(HaveOccurred()))
		Expect(l3gwConfig.MACAddress.String()).To(Equal(hostMAC))
		Expect(l3gwConfig.IPAddresses[0].String()).To(Equal(ifAddrs[0].String()))
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

		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: hostIP}
		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{
			Items: []v1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := nc.initGatewayDPUHost()
			Expect(err).NotTo(HaveOccurred())

			link, err := netlink.LinkByName(uplinkName)
			Expect(err).NotTo(HaveOccurred())

			// check that the service route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(svcCIDR),
				LinkIndex: link.Attrs().Index,
				Gw:        ovntest.MustParseIP(gwIP),
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_GW,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			// check that the masquerade route was added
			expRoute = &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				LinkIndex: link.Attrs().Index,
				Src:       ovntest.MustParseIP(hostIP),
			}
			route, err = util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			return nil
		})
		Expect(err).NotTo(HaveOccurred())
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

func sharedGatewayBypassInterfaceTest(app *cli.App, testNS ns.NetNS, l netlink.Link,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep string, newLayout, netdevChange bool) {
	const (
		clusterCIDR string = "10.1.0.0/16"
		deviceID    string = "0000:01:00.0"
	)

	brName := "br" + eth0Name
	oldNetdev := "dummy0"
	oldRep := "dummy0_0"
	// Add IP, default route and create bridge
	gwIP := ovntest.MustParseIP(eth0GWIP)
	eth0IPNet, err := netlink.ParseAddr(eth0CIDR)
	Expect(err).NotTo(HaveOccurred())

	err = testNS.Do(func(ns.NetNS) error {
		if newLayout {
			err := netlink.AddrAdd(l, eth0IPNet)
			Expect(err).NotTo(HaveOccurred())
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIP,
			}
			return netlink.RouteAdd(defRoute)
		} else {
			defer GinkgoRecover()
			ovntest.AddLink(brName)
			l, err = netlink.LinkByName(brName)
			Expect(err).NotTo(HaveOccurred())
			mac, err := net.ParseMAC(eth0MAC)
			Expect(err).NotTo(HaveOccurred())
			err = netlink.LinkSetHardwareAddr(l, mac)
			Expect(err).NotTo(HaveOccurred())
			if netdevChange {
				ovntest.AddLink(oldNetdev)
				l, err = netlink.LinkByName(oldNetdev)
				Expect(err).NotTo(HaveOccurred())
				mac, err := net.ParseMAC(eth0MAC)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetHardwareAddr(l, mac)
				Expect(err).NotTo(HaveOccurred())
			}
			err = netlink.AddrAdd(l, eth0IPNet)
			Expect(err).NotTo(HaveOccurred())
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIP,
			}
			err = netlink.RouteAdd(defRoute)
			Expect(err).NotTo(HaveOccurred())
			// add masquerade IP and routes
			masqIPNet, err := netlink.ParseAddr(types.V4OVNMasqueradeIP + "/29")
			Expect(err).NotTo(HaveOccurred())
			err = netlink.AddrAdd(l, masqIPNet)
			Expect(err).NotTo(HaveOccurred())
			masqRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				Src:       eth0IPNet.IP,
			}
			return netlink.RouteAdd(masqRoute)
		}
	})
	Expect(err).NotTo(HaveOccurred())

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		// sriovNet mocks
		sriovnetMock := &utilMock.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetMock)
		sriovnetMock.On("GetUplinkRepresentor", deviceID).Return(eth0Name, nil)
		sriovnetMock.On("GetVfIndexByPciAddress", deviceID).Return(0, nil)
		sriovnetMock.On("GetVfRepresentor", eth0Name, 0).Return(bypassRep, nil)

		// fileSystem mocks
		fsMock := &utilMock.FileSystemOps{}
		fsMock.On("Readlink", mock.AnythingOfType("string")).Return("../../../"+deviceID, nil)
		util.SetFileSystemOps(fsMock)

		// exec mocks
		fexec := ovntest.NewLooseCompareFakeExec()
		// bridgeForInterface
		if newLayout {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 port-to-br " + eth0Name,
				Err: fmt.Errorf("not exist"),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 -- --may-exist add-br %[1]s "+
					"-- br-set-external-id %[1]s bridge-id %[1]s "+
					"-- br-set-external-id %[1]s bridge-uplink %[2]s "+
					"-- set bridge %[1]s fail-mode=standalone other_config:hwaddr=%[3]s "+
					"-- --may-exist add-port %[1]s %[2]s "+
					"-- set port %[2]s other-config:transient=true", brName, eth0Name, eth0MAC),
				Action: func() error {
					return testNS.Do(func(ns.NetNS) error {
						defer GinkgoRecover()

						// Create breth0 as a dummy link
						err := netlink.LinkAdd(&netlink.Dummy{
							LinkAttrs: netlink.LinkAttrs{
								Name:         brName,
								HardwareAddr: ovntest.MustParseMAC(eth0MAC),
							},
						})
						Expect(err).NotTo(HaveOccurred())
						_, err = netlink.LinkByName(brName)
						Expect(err).NotTo(HaveOccurred())
						return nil
					})
				},
			})
		} else {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br " + eth0Name,
				Output: brName,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 port-to-br " + bypassRep,
				Err: fmt.Errorf("not exist"),
			})
			if netdevChange {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovs-vsctl --timeout=15 --if-exists get bridge " + brName + " external_ids:ovn-bypass-cfg",
					Output: oldNetdev + "," + oldRep,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists del-port %s %s", brName, oldRep),
				})
			} else {
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovs-vsctl --timeout=15 --if-exists get bridge " + brName + " external_ids:ovn-bypass-cfg",
				})
			}
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-ofctl -O OpenFlow13 --bundle replace-flows " + brName + " -",
			})
		}
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovs-vsctl --timeout=15 -- --may-exist add-port %[1]s %[2]s -- set bridge %[1]s external_ids:ovn-bypass-cfg=%[3]s,%[2]s", brName, bypassRep, bypassNetdev),
		})
		// bridgedGatewayNodeSetup
		// GetOVSPortMACAddress
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + eth0Name + " mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":" + brName,
		})
		// GetNodeChassisID
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		// DetectCheckPktLengthSupport
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features " + brName,
			Output: "Check pkt length action: Yes",
		})
		// IsOvsHwOffloadEnabled
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-" + brName + "_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + eth0Name + " ofport",
			Output: "7",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + bypassRep + " ofport",
			Output: "8",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.10",
			"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
		})
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: eth0IPNet.IP.String()}
		existingNode := v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{existingNode}})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{kubeFakeClient}
		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)
		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			sharedGw, err := newSharedGateway(nc, ovntest.MustParseIPNets(nodeSubnet), nodeAnnotator, &fakeMgmtPortConfig)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init()
			Expect(err).NotTo(HaveOccurred())

			// Verify that IPs had been moved to netdevice link
			netdevLink, err := netlink.LinkByName(bypassNetdev)
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(netdevLink, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found bool
			for _, a := range addrs {
				if a.IP.Equal(eth0IPNet.IP) && bytes.Equal(a.Mask, eth0IPNet.Mask) {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			// Check netdevice link MAC address is eth0 MAC address
			Expect(netdevLink.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))

			// Check that the routes were moved
			routes, err := netlink.RouteList(netdevLink, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(4)) // 2 GW routes + 2 masquerade routes

			// Check masquerade route had been moved
			expRoute := &netlink.Route{
				LinkIndex: netdevLink.Attrs().Index,
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				Src:       eth0IPNet.IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			return nil
		})

		Expect(err).NotTo(HaveOccurred())
		Eventually(fexec.CalledMatchesExpected, 0).Should(BeTrue(), fexec.ErrorDesc)
		return nil
	}

	err = app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--gateway-mode=shared",
		"--gateway-interface=" + bypassNetdev,
	})
	Expect(err).NotTo(HaveOccurred())
}

func sharedGatewayBypassInterfaceRestoreRegularConfigTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep string, isBridge bool) {
	const (
		clusterCIDR string = "10.1.0.0/16"
		deviceID    string = "0000:01:00.0"
	)

	brName := "br" + eth0Name
	gwIface := eth0Name
	if isBridge {
		gwIface = brName
	}

	// Add IP, default route and create bridge
	gwIP := ovntest.MustParseIP(eth0GWIP)
	eth0IPNet, err := netlink.ParseAddr(eth0CIDR)
	Expect(err).NotTo(HaveOccurred())

	err = testNS.Do(func(ns.NetNS) error {
		defer GinkgoRecover()
		ovntest.AddLink(brName)
		l, err := netlink.LinkByName(brName)
		Expect(err).NotTo(HaveOccurred())
		mac, err := net.ParseMAC(eth0MAC)
		Expect(err).NotTo(HaveOccurred())
		err = netlink.LinkSetHardwareAddr(l, mac)
		Expect(err).NotTo(HaveOccurred())
		l, err = netlink.LinkByName(bypassNetdev)
		Expect(err).NotTo(HaveOccurred())
		mac, err = net.ParseMAC(eth0MAC)
		Expect(err).NotTo(HaveOccurred())
		err = netlink.LinkSetHardwareAddr(l, mac)
		Expect(err).NotTo(HaveOccurred())
		err = netlink.AddrAdd(l, eth0IPNet)
		Expect(err).NotTo(HaveOccurred())
		defRoute := &netlink.Route{
			LinkIndex: l.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gwIP,
		}
		err = netlink.RouteAdd(defRoute)
		Expect(err).NotTo(HaveOccurred())
		// add masquerade IP and routes
		masqIPNet, err := netlink.ParseAddr(types.V4OVNMasqueradeIP + "/29")
		Expect(err).NotTo(HaveOccurred())
		err = netlink.AddrAdd(l, masqIPNet)
		Expect(err).NotTo(HaveOccurred())
		masqRoute := &netlink.Route{
			LinkIndex: l.Attrs().Index,
			Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
			Src:       eth0IPNet.IP,
		}
		return netlink.RouteAdd(masqRoute)
	})
	Expect(err).NotTo(HaveOccurred())

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		// sriovNet mocks
		sriovnetMock := &utilMock.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetMock)
		sriovnetMock.On("GetUplinkRepresentor", deviceID).Return("", fmt.Errorf(""))

		// fileSystem mocks
		fsMock := &utilMock.FileSystemOps{}
		fsMock.On("Readlink", mock.AnythingOfType("string")).Return("../../../"+deviceID, nil)
		util.SetFileSystemOps(fsMock)

		// exec mocks
		fexec := ovntest.NewLooseCompareFakeExec()
		// bridgeForInterface
		if isBridge {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovs-vsctl --timeout=15 port-to-br " + brName,
				Err: fmt.Errorf(""),
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 br-exists " + brName,
			})
		} else {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 port-to-br " + eth0Name,
				Output: brName,
			})
		}

		// util.GetNicName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brName,
			Output: eth0Name,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port " + eth0Name + " Interfaces",
			Output: eth0Name,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface " + eth0Name + " Type",
			Output: "system",
		})

		if isBridge {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 get interface " + eth0Name + " ofport",
			})
		}

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get bridge " + brName + " external_ids:ovn-bypass-cfg",
			Output: bypassNetdev + "," + bypassRep,
		})
		// util.RestoreOVSRegularConfig
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists remove bridge " + brName + " external_ids ovn-bypass-cfg",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists del-port " + brName + " " + bypassRep,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows " + brName + " -",
		})
		// bridgedGatewayNodeSetup
		// GetOVSPortMACAddress
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + gwIface + " mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":" + brName,
		})
		// GetNodeChassisID
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		// DetectCheckPktLengthSupport
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features " + brName,
			Output: "Check pkt length action: Yes",
		})
		// IsOvsHwOffloadEnabled
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-" + brName + "_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + eth0Name + " ofport",
			Output: "7",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.10",
			"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
		})
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: eth0IPNet.IP.String()}
		existingNode := v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{existingNode}})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{kubeFakeClient}
		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)
		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			sharedGw, err := newSharedGateway(nc, ovntest.MustParseIPNets(nodeSubnet), nodeAnnotator, &fakeMgmtPortConfig)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init()
			Expect(err).NotTo(HaveOccurred())

			// Verify that IPs had been moved to the bridge
			bridgeLink, err := netlink.LinkByName(brName)
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(bridgeLink, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found bool
			for _, a := range addrs {
				if a.IP.Equal(eth0IPNet.IP) && bytes.Equal(a.Mask, eth0IPNet.Mask) {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
			// Check that the routes were moved
			routes, err := netlink.RouteList(bridgeLink, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(4)) // 2 GW routes + 2 masquerade routes

			// Check masquerade route had been moved
			expRoute := &netlink.Route{
				LinkIndex: bridgeLink.Attrs().Index,
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				Src:       eth0IPNet.IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			return nil
		})

		Expect(err).NotTo(HaveOccurred())
		Eventually(fexec.CalledMatchesExpected, 0).Should(BeTrue(), fexec.ErrorDesc)
		return nil
	}

	err = app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--gateway-mode=shared",
		"--gateway-interface=" + gwIface,
	})
	Expect(err).NotTo(HaveOccurred())
}

func localGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR string, l netlink.Link) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"

	if len(eth0GWIP) > 0 {
		// And a default route
		err := testNS.Do(func(ns.NetNS) error {
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        ovntest.MustParseIP(eth0GWIP),
			}
			return netlink.RouteAdd(defRoute)
		})
		Expect(err).NotTo(HaveOccurred())
	}

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
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
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
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.10",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
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
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter=2",
			Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
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

		expectedAddr, err := netlink.ParseAddr(eth0CIDR)
		Expect(err).NotTo(HaveOccurred())
		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: expectedAddr.IP.String()}
		existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}
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
		endpointSlice := *newEndpointSlice("service1", "namespace1", []discovery.Endpoint{}, []discovery.EndpointPort{})

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
			ifName:    types.K8sMgmtIntfName,
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
			&endpointSlice,
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{KClient: kubeFakeClient}
		iptV4, iptV6 := util.SetFakeIPTablesHelpers()

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			ifAddrs := ovntest.MustParseIPNets(eth0CIDR)
			localGw, err := newLocalGateway(nc, ovntest.MustParseIPNets(nodeSubnet), nodeAnnotator, &fakeMgmtPortConfig)
			Expect(err).NotTo(HaveOccurred())
			err = localGw.Init()
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			localGw.Start()

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

			// check that the masquerade route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", types.V4OVNMasqueradeIP)),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).ToNot(BeNil())

			return nil
		})
		Expect(err).NotTo(HaveOccurred())

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
					"-j OVN-KUBE-EGRESS-SVC",
					"-s 169.254.169.1 -j MASQUERADE",
					"-s 10.1.1.0/24 -j MASQUERADE",
				},
				"OVN-KUBE-SNAT-MGMTPORT": []string{},
				"OVN-KUBE-ETP":           []string{},
				"OVN-KUBE-ITP":           []string{},
				"OVN-KUBE-EGRESS-SVC":    []string{"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN"},
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

func disabledGatewayInterfaceTest(app *cli.App, testNS ns.NetNS, eth0Name, eth0GWIP, eth0CIDR string, l netlink.Link) {
	const clusterCIDR string = "10.1.0.0/16"

	// And a default route
	err := testNS.Do(func(ns.NetNS) error {
		defRoute := &netlink.Route{
			LinkIndex: l.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
			Gw:        ovntest.MustParseIP(eth0GWIP),
		}
		return netlink.RouteAdd(defRoute)
	})
	Expect(err).NotTo(HaveOccurred())

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		fexec := ovntest.NewLooseCompareFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		expectedAddr, err := netlink.ParseAddr(eth0CIDR)
		Expect(err).NotTo(HaveOccurred())
		nodeAddr := v1.NodeAddress{Type: v1.NodeInternalIP, Address: expectedAddr.IP.String()}
		existingNode := v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&v1.NodeList{Items: []v1.Node{existingNode}})
		fakeClient := &util.OVNNodeClientset{
			KubeClient: kubeFakeClient,
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

		k := &kube.Kube{kubeFakeClient}
		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)
		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, wf, nil, nodeName, false)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg)

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			_, err = newDisabledModeGateway(nc, nodeAnnotator)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			return nil
		})

		Expect(err).NotTo(HaveOccurred())
		Eventually(fexec.CalledMatchesExpected, 0).Should(BeTrue(), fexec.ErrorDesc)
		return nil
	}

	err = app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Gateway Init Operations", func() {

	var (
		testNS ns.NetNS
		app    *cli.App
	)

	origSriovNetOps := util.GetSriovnetOps()

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())

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
		util.SetSriovnetOpsInst(origSriovNetOps)
	})

	Context("Setting up the gateway bridge", func() {
		const (
			eth0Name string = "eth0"
			eth0IP   string = "192.168.1.10"
			eth0CIDR string = eth0IP + "/24"
			eth0GWIP string = "192.168.1.1"
		)
		var eth0MAC string
		var link netlink.Link

		BeforeEach(func() {
			// Set up a fake eth0
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(eth0Name)

				var err error
				link, err = netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(eth0CIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(link, addr)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = link.Attrs().HardwareAddr.String()

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up disabled mode gateway", func() {
			disabledGatewayInterfaceTest(app, testNS, eth0Name, eth0GWIP, eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a local gateway with predetermined interface", func() {
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a local gateway with predetermined interface and no default route", func() {
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, "", eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with hw-offloading", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, true, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 3000, link, false, true)
		})

		config.Gateway.Interface = eth0Name
		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN + predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 3000, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with predetermined gateway interface and no default route", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, "", eth0CIDR, 0, link, false, true)
		})

		// don't set the node status internal IP, addMasqueradeRoute will
		// fallback to the provided interface IP
		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with node status internal IPs unset", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, false)
		})
	})

	Context("Bypass configuration", func() {
		const (
			eth0Name     string = "eth0"
			eth0IP       string = "192.168.1.10"
			eth0CIDR     string = eth0IP + "/24"
			eth0GWIP     string = "192.168.1.1"
			bypassNetdev string = "eth0v0"
			bypassRep    string = "eth0_0"
		)
		var eth0MAC string
		var link netlink.Link

		origFileSystemOps := util.GetFileSystemOps()

		BeforeEach(func() {
			// Set up a fake eth0, bypass netdevice and bypass representor
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(eth0Name)
				ovntest.AddLink(bypassNetdev)
				ovntest.AddLink(bypassRep)

				var err error
				link, err = netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = link.Attrs().HardwareAddr.String()

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			util.SetFileSystemOps(origFileSystemOps)
		})

		ovntest.OnSupportedPlatformsIt("sets up bypass bridge layout from scratch", func() {
			sharedGatewayBypassInterfaceTest(app, testNS, link, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep, true, false)
		})

		ovntest.OnSupportedPlatformsIt("sets up bypass bridge layout when regular bridge layout was configured", func() {
			sharedGatewayBypassInterfaceTest(app, testNS, link, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep, false, false)
		})

		ovntest.OnSupportedPlatformsIt("sets up bypass bridge layout when bypass interface changed", func() {
			sharedGatewayBypassInterfaceTest(app, testNS, link, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep, false, true)
		})

		ovntest.OnSupportedPlatformsIt("switches from bypass to regular bridge configuration - gwIface=Uplink", func() {
			sharedGatewayBypassInterfaceRestoreRegularConfigTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep, false)
		})

		ovntest.OnSupportedPlatformsIt("switches from bypass to regular bridge configuration - gwIface=Bridge", func() {
			sharedGatewayBypassInterfaceRestoreRegularConfigTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, bypassNetdev, bypassRep, true)
		})
	})
})

var _ = Describe("Gateway Operations DPU", func() {
	var (
		testNS ns.NetNS
		app    *cli.App
	)

	origSriovNetOps := util.GetSriovnetOps()

	BeforeEach(func() {
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		_, _ = util.SetFakeIPTablesHelpers()
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		util.SetSriovnetOpsInst(origSriovNetOps)
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
			shareGatewayInterfaceDPUTest(app, testNS, brphys, hostMAC, hostCIDR, dpuIP)
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
	gwIface := "eth0"
	gwLink := &linkMock.Link{}
	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
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
			netlinkMock.On("LinkByName", gwIface).Return(gwLink, nil)
			netlinkMock.On("AddrList", gwLink, mock.Anything).Return([]netlink.Addr{{IPNet: dpuSubnet}}, nil)
			gwSubnet, err := getDPUHostPrimaryIPAddresses(gwIface, nodeIP)
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in host subnets", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.1.11")
			netlinkMock.On("LinkByName", gwIface).Return(gwLink, nil)
			netlinkMock.On("AddrList", gwLink, mock.Anything).Return([]netlink.Addr{{IPNet: dpuSubnet}}, nil)
			_, err := getDPUHostPrimaryIPAddresses(gwIface, nodeIP)
			Expect(err).To(HaveOccurred())
		})

		It("returns node IP with config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			nodeIP := net.ParseIP("10.1.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(16, 32)},
			}
			gwSubnet, err := getDPUHostPrimaryIPAddresses(gwIface, nodeIP)
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			nodeIP := net.ParseIP("10.0.0.11")
			_, err := getDPUHostPrimaryIPAddresses(gwIface, nodeIP)
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
