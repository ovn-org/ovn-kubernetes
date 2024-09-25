package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testMgmtMAC string = "06:05:04:03:02:01"

	thisNodeSubnet string = "1.2.3.0/24"
	thisNodeDRIP   string = "1.2.3.3"
	thisNodeDRMAC  string = "22:33:44:55:66:77"
)

// returns if the two flowCaches are the same
func compareFlowCache(returnedFlowCache, expectedFlowCache map[string]*flowCacheEntry) error {
	if len(returnedFlowCache) != len(expectedFlowCache) {
		return fmt.Errorf("the number of expected flow cache entries (%d) does not equal the number returned (%d)", len(expectedFlowCache), len(returnedFlowCache))
	}

	for key, entry := range returnedFlowCache {
		expectedEntry, ok := expectedFlowCache[key]
		if !ok {
			return fmt.Errorf("unexpected entry %s in nodes flowCache", entry.flows)
		}
		if err := compareFlowCacheEntry(entry, expectedEntry); err != nil {
			return fmt.Errorf("returned flowCacheEntry[%s] does not equal expectedCacheEntry[%s]: %v", key, key, err)
		}
	}
	return nil

}

// compares two entries in the flow cache
func compareFlowCacheEntry(returnedEntry, expectedEntry *flowCacheEntry) error {
	if returnedEntry.learnedFlow != expectedEntry.learnedFlow {
		return fmt.Errorf("the number of flows in the flow cache entry is unexpected")
	}
	if returnedEntry.ignoreLearn != expectedEntry.ignoreLearn {
		return fmt.Errorf("the flowCacheEntry ignoreLearn field is not expected")
	}
	if len(returnedEntry.flows) != len(expectedEntry.flows) {
		return fmt.Errorf("the number of flows is not equal to the number of flows expected")
	}

	for key, returnedEntryFlow := range returnedEntry.flows {
		if returnedEntryFlow != expectedEntry.flows[key] {
			return fmt.Errorf("returnedflowCacheEntry[%d] = %s does not equal expectedFlowCacheEntry[%d] = %s", key, returnedEntryFlow, key, expectedEntry.flows[key])
		}

	}

	return nil
}

func generateInitialFlowCacheEntry(mgmtInterfaceAddr, drIP, drMAC string) *flowCacheEntry {
	_, ipNet, err := net.ParseCIDR(thisNodeSubnet)
	Expect(err).NotTo(HaveOccurred())
	gwIfAddr := util.GetNodeGatewayIfAddr(ipNet)
	gwPortMAC := util.IPAddrToHWAddr(gwIfAddr.IP)
	drMACRaw := strings.Replace(drMAC, ":", "", -1)
	return &flowCacheEntry{
		flows: []string{
			"table=0,priority=0,actions=drop",
			"table=1,priority=0,actions=drop",
			"table=2,priority=0,actions=drop",
			"table=10,priority=0,actions=drop",
			"table=20,priority=0,actions=drop",
			"table=0,priority=100,in_port=ext,arp_op=1,arp,arp_tpa=" + drIP + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + drMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],load:0x" + drMACRaw + "->NXM_NX_ARP_SHA[],load:0x" + getIPAsHexString(net.ParseIP(drIP)) + "->NXM_OF_ARP_SPA[],IN_PORT,resubmit(,1)",
			"table=0,priority=100,in_port=ext-vxlan,ip,nw_dst=" + thisNodeSubnet + ",dl_dst=" + drMAC + ",actions=goto_table:10",
			"table=0,priority=10,arp,in_port=ext-vxlan,arp_op=1,arp_tpa=" + thisNodeSubnet + ",actions=resubmit(,2)",
			"table=2,priority=100,arp,in_port=ext-vxlan,arp_op=1,arp_tpa=" + thisNodeSubnet + ",actions=move:tun_src->tun_dst,load:4097->NXM_NX_TUN_ID[0..31],move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + drMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + drMACRaw + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
			"table=10,priority=100,ip,nw_dst=" + mgmtInterfaceAddr + ",actions=mod_dl_src:" + drMAC + ",mod_dl_dst:" + testMgmtMAC + ",output:ext",
			"table=10,priority=100,ip,nw_dst=" + drIP + ",actions=mod_nw_dst:100.64.0.3,mod_dl_src:" + drMAC + ",mod_dl_dst:" + gwPortMAC.String() + ",output:ext",
		},
	}

}

// returns a fake node IP and DR MAC
func addNodeSetupCmds(fexec *ovntest.FakeExec, nodeName string) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch mynode other-config:subnet",
		Output: thisNodeSubnet,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --may-exist add-br br-ext -- set Bridge br-ext fail_mode=secure -- set Interface br-ext mtu_request=1400",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface br-ext mac_in_use",
		Output: "10:11:12:13:14:15",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set bridge br-ext other-config:hwaddr=10:11:12:13:14:15",
		"ovs-vsctl --timeout=15 --may-exist add-port br-int int -- --may-exist add-port br-ext ext -- set Interface int type=patch options:peer=ext external-ids:iface-id=int-" + nodeName + " -- set Interface ext type=patch options:peer=int",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		`ovs-vsctl --timeout=15 --may-exist add-port br-ext ext-vxlan -- set interface ext-vxlan type=vxlan options:remote_ip="flow" options:key="flow" options:dst_port=4789`,
	})
}

func createNode(name, os, ip string, annotations map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1.LabelOSStable: os,
			},
			Annotations: annotations,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: ip},
			},
		},
	}
}

func addSyncFlows(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-ofctl dump-flows --no-stats br-ext table=20",
		"ovs-ofctl -O OpenFlow13 --bundle replace-flows br-ext -",
	})
}

func createPod(namespace, name, node, podIP, podMAC string) *v1.Pod {
	annotations := map[string]string{}
	if podIP != "" || podMAC != "" {
		ipn := ovntest.MustParseIPNet(podIP)
		gatewayIP := iputils.NextIP(ipn.IP)
		annotations[util.OvnPodAnnotationName] = fmt.Sprintf(`{"default": {"ip_address":"` + podIP + `", "mac_address":"` + podMAC + `", "gateway_ip": "` + gatewayIP.String() + `"}}`)
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: annotations,
		},
		Spec: v1.PodSpec{
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
}

func appRun(app *cli.App) {
	err := app.Run([]string{
		app.Name,
		"-enable-hybrid-overlay",
		"-no-hostsubnet-nodes=" + v1.LabelOSStable + "=windows",
		"-cluster-subnets=10.130.0.0/15/24",
		"-hybrid-overlay-cluster-subnets=10.0.0.1/16/23",
	})
	Expect(err).NotTo(HaveOccurred())
}

func createNodeAnnotationsForSubnet(subnet string) map[string]string {
	subnetAnnotations, err := util.UpdateNodeHostSubnetAnnotation(nil, ovntest.MustParseIPNets(subnet), types.DefaultNetworkName)
	Expect(err).NotTo(HaveOccurred())
	annotations := make(map[string]string)
	for k, v := range subnetAnnotations {
		annotations[k] = fmt.Sprintf("%s", v)
	}
	return annotations
}

type fakeLink struct {
	attrs *netlink.LinkAttrs
}

func (fl *fakeLink) Attrs() *netlink.LinkAttrs {
	return fl.attrs
}

func (fl *fakeLink) Type() string {
	return "fakeLink"
}

func addEnsureHybridOverlayBridgeMocks(nlMock *mocks.NetLinkOps, drIP, oldDRIP string) {
	mockBrExt := &fakeLink{
		attrs: &netlink.LinkAttrs{
			Index: 555,
		},
	}
	mockMp0 := &fakeLink{
		attrs: &netlink.LinkAttrs{
			Index:        777,
			HardwareAddr: ovntest.MustParseMAC(testMgmtMAC),
		},
	}
	mockRoute := &netlink.Route{
		LinkIndex: 777,
		Dst:       ovntest.MustParseIPNet("10.0.0.0/16"),
		Gw:        ovntest.MustParseIP(drIP),
	}

	nlMocks := []ovntest.TestifyMockHelper{
		{
			OnCallMethodName: "LinkByName",
			OnCallMethodArgs: []interface{}{"br-ext"},
			RetArgList:       []interface{}{mockBrExt, nil},
		},
		{
			OnCallMethodName: "LinkSetUp",
			OnCallMethodArgs: []interface{}{mockBrExt},
			RetArgList:       []interface{}{nil},
		},
		{
			OnCallMethodName: "LinkByName",
			OnCallMethodArgs: []interface{}{"ovn-k8s-mp0"},
			RetArgList:       []interface{}{mockMp0, nil},
		},
		{
			OnCallMethodName: "RouteAdd",
			OnCallMethodArgs: []interface{}{mockRoute},
			RetArgList:       []interface{}{nil},
		},
	}

	if len(oldDRIP) > 0 {
		oldMockRoute := &netlink.Route{
			LinkIndex: 777,
			Dst:       ovntest.MustParseIPNet("10.0.0.0/16"),
			Gw:        ovntest.MustParseIP(oldDRIP),
		}
		nlMocks = append(nlMocks, ovntest.TestifyMockHelper{
			OnCallMethodName: "RouteDel",
			OnCallMethodArgs: []interface{}{oldMockRoute},
			RetArgList:       []interface{}{nil},
		})

	}
	ovntest.ProcessMockFnList(&nlMock.Mock, nlMocks)
}

var _ = Describe("Hybrid Overlay Node Linux Operations", func() {
	var (
		app        *cli.App
		fexec      *ovntest.FakeExec
		stopChan   chan struct{}
		wg         *sync.WaitGroup
		mgmtIfAddr *net.IPNet
		nlMock     *mocks.NetLinkOps
	)
	const (
		thisNode   string = "mynode"
		thisNodeIP string = "10.0.0.1"
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		nlMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}

		fexec = ovntest.NewLooseCompareFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		mgmtIfAddr = util.GetNodeManagementIfAddr(ovntest.MustParseIPNet(thisNodeSubnet))
	})

	AfterEach(func() {
		close(stopChan)
		wg.Wait()
		util.ResetNetLinkOpMockInst()
	})

	ovntest.OnSupportedPlatformsIt("does not set up tunnels for non-hybrid-overlay nodes without annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name string = "node1"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(node1Name, "linux", thisNodeIP, nil),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.nodeEventHandler.Run(1, stopChan)
			}()
			// don't add any commands the setup will fail because the master has not set the correct annotations
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}
		appRun(app)
	})

	ovntest.OnSupportedPlatformsIt("does not set up tunnels for non-hybrid-overlay nodes with subnet annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "1.2.4.0/24"
				node1IP     string = "10.11.12.1"
			)

			annotations := createNodeAnnotationsForSubnet(node1Subnet)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*createNode(node1Name, "linux", node1IP, annotations),
				},
			})

			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())
			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.nodeEventHandler.Run(1, stopChan)
			}()

			// similarly to above no ovs commands will be issued to exec because the hybrid overlay setup will fail
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}
		appRun(app)
	})

	ovntest.OnSupportedPlatformsIt("sets up local node hybrid overlay bridge", func() {
		app.Action = func(ctx *cli.Context) error {
			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{*node},
			})

			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())
			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")

			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}
		appRun(app)
	})
	ovntest.OnSupportedPlatformsIt("sets up local linux pod, ignores remote linux pod", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				pod1IP   string = "1.2.3.5"
				pod1CIDR string = pod1IP + "/24"
				pod1MAC  string = "aa:bb:cc:dd:ee:ff"

				remotePodIP      string = "1.2.4.5"
				remotePodCIDR    string = remotePodIP + "/24"
				remotePodMAC     string = "11:22:33:44:55:66"
				remoteNodeName   string = "remoteNode"
				remoteNodeDRMAC  string = "66:55:44:33:22:11"
				remoteNodeSubnet string = "1.2.4.0/24"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)

			remoteNodeAnnotations := createNodeAnnotationsForSubnet(remoteNodeSubnet)
			remoteNodeAnnotations[hotypes.HybridOverlayDRMAC] = remoteNodeDRMAC
			remoteNodeAnnotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.65.0.3/16\"}}"
			remoteNodeAnnotations[hotypes.HybridOverlayDRIP] = "1.2.4.3"
			remoteNode := createNode(remoteNodeName, "linux", "10.20.20.1", remoteNodeAnnotations)

			testPod := createPod("test", "pod1", thisNode, pod1CIDR, pod1MAC)
			remoteTestPod := createPod("test", "remotePod", remoteNodeName, remotePodCIDR, remotePodMAC)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{*node, *remoteNode},
			})

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}

			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// adding a remote pod should do nothing... we only care about directing traffic for pods on this node
			_, err = fakeClient.CoreV1().Pods(remoteTestPod.Namespace).Create(context.TODO(), remoteTestPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			_, err = fakeClient.CoreV1().Pods(testPod.Namespace).Create(context.TODO(), testPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// flowSync after add pods
			addSyncFlows(fexec)

			initialFlowCache[podIPToCookie(net.ParseIP(pod1IP))] = &flowCacheEntry{
				flows:       []string{"table=10,cookie=0x" + podIPToCookie(net.ParseIP(pod1IP)) + ",priority=100,ip,nw_dst=" + pod1IP + ",actions=set_field:" + thisNodeDRMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext"},
				ignoreLearn: true,
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}
		appRun(app)
	})
	ovntest.OnSupportedPlatformsIt("on startup will add a local linux pod that times out on the initial addPod event", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				pod1IP   string = "1.2.3.5"
				pod1CIDR string = pod1IP + "/24"
				pod1MAC  string = "aa:bb:cc:dd:ee:ff"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			testPod := createPod("test", "pod1", thisNode, pod1CIDR, pod1MAC)
			fakeClient := fake.NewSimpleClientset(
				//&v1.NodeList{
				//	Items: []v1.Node{*node},
				//},
				&v1.PodList{
					Items: []v1.Pod{*testPod},
				},
			)

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			time.Sleep(2 * time.Second)
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}

			initialFlowCache[podIPToCookie(net.ParseIP(pod1IP))] = &flowCacheEntry{
				flows:       []string{"table=10,cookie=0x" + podIPToCookie(net.ParseIP(pod1IP)) + ",priority=100,ip,nw_dst=" + pod1IP + ",actions=set_field:" + thisNodeDRMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext"},
				ignoreLearn: true,
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}
		appRun(app)
	})

	ovntest.OnSupportedPlatformsIt("sets up tunnels for Windows nodes", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "10.11.12.0/24"
				node1DRMAC  string = "00:00:00:7f:af:03"
				node1IP     string = "10.11.12.1"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*node,
				},
			})

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			windowsAnnotation := createNodeAnnotationsForSubnet(node1Subnet)
			windowsAnnotation[hotypes.HybridOverlayDRMAC] = node1DRMAC
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), createNode(node1Name, "windows", node1IP, windowsAnnotation), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// flowsync after AddNode
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			node1Cookie := nameToCookie(node1Name)
			initialFlowCache[node1Cookie] = &flowCacheEntry{
				flows: []string{
					"cookie=0x" + node1Cookie + ",table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DRMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + strings.ReplaceAll(node1DRMAC, ":", "") + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
					"cookie=0x" + node1Cookie + ",table=0,priority=100,ip,nw_dst=" + node1Subnet + ",actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
					"cookie=0x" + node1Cookie + ",table=0,priority=101,ip,nw_dst=" + node1Subnet + ",nw_src=100.64.0.3,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + thisNodeDRIP + "->nw_src,set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
				},
			}

			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())
			return nil
		}
		appRun(app)
	})
	ovntest.OnSupportedPlatformsIt("node updates itself, windows tunnel and pod flows when distributed router IP is updated", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "10.11.12.0/24"
				node1DRMAC  string = "00:00:00:7f:af:03"
				node1IP     string = "10.11.12.1"

				pod1IP   string = "1.2.3.5"
				pod1CIDR string = pod1IP + "/24"
				pod1MAC  string = "aa:bb:cc:dd:ee:ff"

				updatedDRIP string = "1.2.3.6"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*node,
				},
			})

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// add the mock commands for the update
			addEnsureHybridOverlayBridgeMocks(nlMock, updatedDRIP, thisNodeDRIP)
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// setup windows node
			windowsAnnotation := createNodeAnnotationsForSubnet(node1Subnet)
			windowsAnnotation[hotypes.HybridOverlayDRMAC] = node1DRMAC
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), createNode(node1Name, "windows", node1IP, windowsAnnotation), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// flowsync after AddNode
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			node1Cookie := nameToCookie(node1Name)
			initialFlowCache[node1Cookie] = &flowCacheEntry{
				flows: []string{
					"cookie=0x" + node1Cookie + ",table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DRMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + strings.ReplaceAll(node1DRMAC, ":", "") + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
					"cookie=0x" + node1Cookie + ",table=0,priority=100,ip,nw_dst=" + node1Subnet + ",actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
					"cookie=0x" + node1Cookie + ",table=0,priority=101,ip,nw_dst=" + node1Subnet + ",nw_src=100.64.0.3,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + thisNodeDRIP + "->nw_src,set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
				},
			}

			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// setup local pod
			testPod := createPod("test", "pod1", thisNode, pod1CIDR, pod1MAC)
			_, err = fakeClient.CoreV1().Pods(testPod.Namespace).Create(context.TODO(), testPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache[podIPToCookie(net.ParseIP(pod1IP))] = &flowCacheEntry{
				flows:       []string{"table=10,cookie=0x" + podIPToCookie(net.ParseIP(pod1IP)) + ",priority=100,ip,nw_dst=" + pod1IP + ",actions=set_field:" + thisNodeDRMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext"},
				ignoreLearn: true,
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			//update Node DRIP
			node.Annotations[hotypes.HybridOverlayDRIP] = updatedDRIP
			_, err = fakeClient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache["0x0"] = generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), updatedDRIP, thisNodeDRMAC)
			initialFlowCache[node1Cookie] = &flowCacheEntry{
				flows: []string{
					"cookie=0x" + node1Cookie + ",table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DRMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + strings.ReplaceAll(node1DRMAC, ":", "") + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
					"cookie=0x" + node1Cookie + ",table=0,priority=100,ip,nw_dst=" + node1Subnet + ",actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
					"cookie=0x" + node1Cookie + ",table=0,priority=101,ip,nw_dst=" + node1Subnet + ",nw_src=100.64.0.3,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + updatedDRIP + "->nw_src,set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
				},
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			return nil
		}
		appRun(app)
	})
	ovntest.OnSupportedPlatformsIt("node updates itself, windows tunnel and pod flows when distributed router MAC is updated", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "10.11.12.0/24"
				node1DRMAC  string = "00:00:00:7f:af:03"
				node1IP     string = "10.11.12.1"

				pod1IP   string = "1.2.3.5"
				pod1CIDR string = pod1IP + "/24"
				pod1MAC  string = "aa:bb:cc:dd:ee:ff"

				updatedDRMAC string = "77:66:55:44:33:22"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*node,
				},
			})

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}, 2).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// setup windows node
			windowsAnnotation := createNodeAnnotationsForSubnet(node1Subnet)
			windowsAnnotation[hotypes.HybridOverlayDRMAC] = node1DRMAC
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), createNode(node1Name, "windows", node1IP, windowsAnnotation), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// flowsync after AddNode
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			node1Cookie := nameToCookie(node1Name)
			initialFlowCache[node1Cookie] = &flowCacheEntry{
				flows: []string{
					"cookie=0x" + node1Cookie + ",table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DRMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + strings.ReplaceAll(node1DRMAC, ":", "") + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
					"cookie=0x" + node1Cookie + ",table=0,priority=100,ip,nw_dst=" + node1Subnet + ",actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
					"cookie=0x" + node1Cookie + ",table=0,priority=101,ip,nw_dst=" + node1Subnet + ",nw_src=100.64.0.3,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + thisNodeDRIP + "->nw_src,set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
				},
			}

			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// setup local pod
			testPod := createPod("test", "pod1", thisNode, pod1CIDR, pod1MAC)
			_, err = fakeClient.CoreV1().Pods(testPod.Namespace).Create(context.TODO(), testPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache[podIPToCookie(net.ParseIP(pod1IP))] = &flowCacheEntry{
				flows:       []string{"table=10,cookie=0x" + podIPToCookie(net.ParseIP(pod1IP)) + ",priority=100,ip,nw_dst=" + pod1IP + ",actions=set_field:" + thisNodeDRMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext"},
				ignoreLearn: true,
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			//update Node DRIP
			annotations[hotypes.HybridOverlayDRMAC] = updatedDRMAC
			_, err = fakeClient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache["0x0"] = generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, updatedDRMAC)
			initialFlowCache[podIPToCookie(net.ParseIP(pod1IP))] = &flowCacheEntry{
				flows:       []string{"table=10,cookie=0x" + podIPToCookie(net.ParseIP(pod1IP)) + ",priority=100,ip,nw_dst=" + pod1IP + ",actions=set_field:" + updatedDRMAC + "->eth_src,set_field:" + pod1MAC + "->eth_dst,output:ext"},
				ignoreLearn: true,
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			return nil
		}
		appRun(app)
	})
	ovntest.OnSupportedPlatformsIt("node updates itself vxlan tunnel when a node is switched to hybrid overlay node", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				node1Name   string = "node1"
				node1Subnet string = "10.11.12.0/24"
				node1DRMAC  string = "00:00:00:7f:af:03"
				node1IP     string = "10.11.12.1"

				pod1IP   string = "1.2.3.5"
				pod1CIDR string = pod1IP + "/24"
				pod1MAC  string = "aa:bb:cc:dd:ee:ff"

				updatedDRMAC string = "77:66:55:44:33:22"
			)

			annotations := createNodeAnnotationsForSubnet(thisNodeSubnet)
			annotations[hotypes.HybridOverlayDRMAC] = thisNodeDRMAC
			annotations[util.OVNNodeGRLRPAddrs] = "{\"default\":{\"ipv4\":\"100.64.0.3/16\"}}"
			annotations[hotypes.HybridOverlayDRIP] = thisNodeDRIP
			node := createNode(thisNode, "linux", thisNodeIP, annotations)
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*node,
				},
			})

			// Node setup from initial node sync
			addNodeSetupCmds(fexec, thisNode)
			_, err := config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				thisNode,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				false,
			)
			Expect(err).NotTo(HaveOccurred())
			linuxNode, okay := n.controller.(*NodeController)
			Expect(okay).To(BeTrue())
			// setting the flowCacheSyncPeriod to 1 hour effectively disabling for testing
			linuxNode.flowCacheSyncPeriod = 1 * time.Hour

			addEnsureHybridOverlayBridgeMocks(nlMock, thisNodeDRIP, "")
			// initial flowSync
			addSyncFlows(fexec)
			// flowsync after EnsureHybridOverlayBridge()
			addSyncFlows(fexec)

			f.Start(stopChan)
			wg.Add(1)
			go func() {
				defer wg.Done()
				n.Run(stopChan)
			}()

			Eventually(func() bool {
				return atomic.LoadUint32(linuxNode.initState) == hotypes.PodsInitialized
			}).Should(BeTrue())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			initialFlowCache := map[string]*flowCacheEntry{
				"0x0": generateInitialFlowCacheEntry(mgmtIfAddr.IP.String(), thisNodeDRIP, thisNodeDRMAC),
			}
			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// setup hybrid overlay node
			windowsAnnotation := createNodeAnnotationsForSubnet(node1Subnet)
			windowsAnnotation[hotypes.HybridOverlayDRMAC] = node1DRMAC
			_, err = fakeClient.CoreV1().Nodes().Create(context.TODO(), createNode(node1Name, "windows", node1IP, windowsAnnotation), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// flowsync after AddNode
			addSyncFlows(fexec)
			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			node1Cookie := nameToCookie(node1Name)
			initialFlowCache[node1Cookie] = &flowCacheEntry{
				flows: []string{
					"cookie=0x" + node1Cookie + ",table=0,priority=100,arp,in_port=ext,arp_tpa=" + node1Subnet + ",actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],mod_dl_src:" + node1DRMAC + ",load:0x2->NXM_OF_ARP_OP[],move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],load:0x" + strings.ReplaceAll(node1DRMAC, ":", "") + "->NXM_NX_ARP_SHA[],move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],IN_PORT",
					"cookie=0x" + node1Cookie + ",table=0,priority=100,ip,nw_dst=" + node1Subnet + ",actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
					"cookie=0x" + node1Cookie + ",table=0,priority=101,ip,nw_dst=" + node1Subnet + ",nw_src=100.64.0.3,actions=load:4097->NXM_NX_TUN_ID[0..31],set_field:" + thisNodeDRIP + "->nw_src,set_field:" + node1IP + "->tun_dst,set_field:" + node1DRMAC + "->eth_dst,output:ext-vxlan",
				},
			}

			Eventually(func() error {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				return compareFlowCache(linuxNode.flowCache, initialFlowCache)
			}, 2).Should(BeNil())

			// node is swiched to ovn node
			_, err = fakeClient.CoreV1().Nodes().Update(context.TODO(), createNode(node1Name, "linux", node1IP, windowsAnnotation), metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				linuxNode.flowMutex.Lock()
				defer linuxNode.flowMutex.Unlock()
				_, ok := linuxNode.flowCache[node1Cookie]
				return ok
			}, 2).Should(BeFalse())
			return nil
		}
		appRun(app)
	})
})
