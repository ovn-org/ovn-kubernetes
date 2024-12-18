package node

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func ipEvent(ipStr string, isAdd bool, addrChan chan netlink.AddrUpdate) *net.IPNet {
	ipNet := ovntest.MustParseIPNet(ipStr)
	addrChan <- netlink.AddrUpdate{
		LinkAddress: *ipNet,
		NewAddr:     isAdd,
	}
	return ipNet
}

func nodeHasAddress(fakeClient kubernetes.Interface, nodeName string, ipNet *net.IPNet) bool {
	node, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	addrs, err := util.ParseNodeHostCIDRsDropNetMask(node)
	Expect(err).NotTo(HaveOccurred())
	return addrs.Has(ipNet.IP.String())
}

type testCtx struct {
	ns           ns.NetNS
	ipManager    *addressManager
	watchFactory factory.NodeWatchFactory
	fakeClient   kubernetes.Interface
	doneWg       *sync.WaitGroup
	stopCh       chan struct{}
	addrChan     chan netlink.AddrUpdate
	mgmtPortIP4  *net.IPNet
	mgmtPortIP6  *net.IPNet
	subscribed   uint32
}

var _ = Describe("Node IP Handler event tests", func() {
	// To ensure that variables don't leak between parallel Ginkgo specs,
	// put all test context into a single struct and reference it via
	// a pointer. The pointer will be different for each spec.
	var tc *testCtx

	const (
		nodeName  string = "node1"
		nodeAddr4 string = "10.1.1.10/24"
		nodeAddr6 string = "2001:db8::10/64"
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
			Output: "10.1.1.10",
		})
		Expect(util.SetExec(fexec)).ShouldNot(HaveOccurred())
		useNetlink := false
		tc = configureKubeOVNContext(nodeName, useNetlink)
		// We need to wait until the ipManager's goroutine runs the subscribe
		// function at least once. We can't use a WaitGroup because we have
		// no way to Add(1) to it, and WaitGroups must have matched Add/Done
		// calls.
		subscribe := func() (bool, chan netlink.AddrUpdate, error) {
			defer atomic.StoreUint32(&tc.subscribed, 1)
			tc.addrChan = make(chan netlink.AddrUpdate)
			tc.ipManager.sync()
			return true, tc.addrChan, nil
		}
		tc.doneWg.Add(1)
		go func() {
			tc.ipManager.runInternal(tc.stopCh, subscribe)
			tc.doneWg.Done()
		}()
		Eventually(func() bool {
			return atomic.LoadUint32(&tc.subscribed) == 1
		}, 5).Should(BeTrue())
	})

	AfterEach(func() {
		close(tc.stopCh)
		tc.doneWg.Wait()
		tc.watchFactory.Shutdown()
		close(tc.addrChan)
		util.ResetRunner()
	})

	Describe("Changing node addresses", func() {
		Context("by adding and deleting a valid IP", func() {
			It("should update node annotations", func() {
				for _, addr := range []string{nodeAddr4, nodeAddr6} {
					ipNet := ipEvent(addr, true, tc.addrChan)
					Eventually(func() bool {
						return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
					}, 5).Should(BeTrue())

					ipNet = ipEvent(addr, false, tc.addrChan)
					Eventually(func() bool {
						return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
					}, 5).Should(BeFalse())
				}
			})
		})

		Context("by adding and deleting an invalid IP", func() {
			It("should not update node annotations", func() {
				for _, addr := range []string{tc.mgmtPortIP4.String(), tc.mgmtPortIP6.String(), config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String() + "/29", config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String() + "/125"} {
					ipNet := ipEvent(addr, true, tc.addrChan)
					Consistently(func() bool {
						return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
					}, 3).Should(BeFalse())

					ipNet = ipEvent(addr, false, tc.addrChan)
					Consistently(func() bool {
						return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
					}, 3).Should(BeFalse())
				}
			})
		})
	})

	Describe("Subscription errors", func() {
		It("should resubscribe and continue processing address events", func() {
			// Reset our subscription tracker, close the channel to force
			// the ipManager to resubscribe, and wait until it does
			atomic.StoreUint32(&tc.subscribed, 0)
			close(tc.addrChan)
			Eventually(func() bool {
				return atomic.LoadUint32(&tc.subscribed) == 1
			}, 5).Should(BeTrue())

			ipNet := ipEvent(nodeAddr4, true, tc.addrChan)
			Eventually(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
			}, 5).Should(BeTrue())

			ipNet = ipEvent(nodeAddr6, false, tc.addrChan)
			Eventually(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ipNet)
			}, 5).Should(BeFalse())
		})
	})
})

var _ = Describe("Node IP Handler tests", func() {
	// To ensure that variables don't leak between parallel Ginkgo specs,
	// put all test context into a single struct and reference it via
	// a pointer. The pointer will be different for each spec.
	var tc *testCtx

	const (
		nodeName                 = "node1"
		dummyBrName              = "breth0"
		dummyBrInternalIPv4      = "10.1.1.10"
		dummyAdditionalIPv4CIDR  = "192.168.2.2/24"
		dummyAdditionalIPv4CIDR2 = "192.168.3.2/24"
		dummyBrUniqLocalIPv6CIDR = "fd53:6043:6000:e0e0:1::6001/80"
		dummyMasqIPv4            = "169.254.169.2"
		dummyMasqIPv4CIDR        = dummyMasqIPv4 + "/29"
		dummyMasqIPv6            = "fd69::2"
		dummyMasqIPv6CIDR        = dummyMasqIPv6 + "/125"
	)

	BeforeEach(func() {
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
			Output: dummyBrInternalIPv4,
		})
		Expect(util.SetExec(fexec)).ShouldNot(HaveOccurred())
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = true
		tc = configureKubeOVNContextWithNs(nodeName)
		tc.ipManager.syncPeriod = 10 * time.Millisecond
		tc.doneWg.Add(1)
		go tc.ns.Do(func(netNS ns.NetNS) error {
			tc.ipManager.runInternal(tc.stopCh, tc.ipManager.getNetlinkAddrSubFunc(tc.stopCh))
			tc.doneWg.Done()
			return nil
		})
	})

	AfterEach(func() {
		close(tc.stopCh)
		tc.doneWg.Wait()
		tc.watchFactory.Shutdown()
		Expect(tc.ns.Close()).ShouldNot(HaveOccurred())
		util.ResetRunner()
	})

	Context("valid addresses", func() {
		ovntest.OnSupportedPlatformsIt("allows keepalived VIP", func() {
			Expect(tc.ns.Do(func(netNS ns.NetNS) error {
				link, err := netlink.LinkByName(dummyBrName)
				if err != nil {
					return err
				}
				return netlink.AddrAdd(link, &netlink.Addr{
					LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, Label: dummyBrName + ":vip", IPNet: ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR),
				})
			})).ShouldNot(HaveOccurred())
			Eventually(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR))
			}, 5).Should(BeTrue())
			// ensure a sync doesnt remove it
			Consistently(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR))
			}, 3).Should(BeTrue())
		})

		ovntest.OnSupportedPlatformsIt("allows unique local address", func() {
			Expect(tc.ns.Do(func(netNS ns.NetNS) error {
				link, err := netlink.LinkByName(dummyBrName)
				if err != nil {
					return err
				}
				return netlink.AddrAdd(link, &netlink.Addr{
					LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, IPNet: ovntest.MustParseIPNet(dummyBrUniqLocalIPv6CIDR),
				})
			})).ShouldNot(HaveOccurred())
			Eventually(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyBrUniqLocalIPv6CIDR))
			}, 5).Should(BeTrue())
			// ensure a sync doesnt remove it
			Consistently(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyBrUniqLocalIPv6CIDR))
			}, 3).Should(BeTrue())
		})

		ovntest.OnSupportedPlatformsIt("allow secondary IP", func() {
			primaryIPNet := ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR)
			// create an additional IP which resides within the primary subnet aka secondary IP
			secondaryIP := make(net.IP, len(primaryIPNet.IP))
			copy(secondaryIP, primaryIPNet.IP)
			secondaryIP[len(secondaryIP)-1]++
			secondaryIPNet := &net.IPNet{IP: secondaryIP, Mask: primaryIPNet.Mask}

			Expect(tc.ns.Do(func(netNS ns.NetNS) error {
				link, err := netlink.LinkByName(dummyBrName)
				if err != nil {
					return err
				}
				err = netlink.AddrAdd(link, &netlink.Addr{
					LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, IPNet: primaryIPNet})
				if err != nil {
					return err
				}
				return netlink.AddrAdd(link, &netlink.Addr{
					LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, IPNet: secondaryIPNet})
			})).ShouldNot(HaveOccurred())
			Eventually(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, primaryIPNet) && nodeHasAddress(tc.fakeClient, nodeName, secondaryIPNet)
			}, 5).Should(BeTrue())
			// ensure a sync doesnt remove it
			Consistently(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, primaryIPNet) && nodeHasAddress(tc.fakeClient, nodeName, secondaryIPNet)
			}, 3).Should(BeTrue())
		})

		ovntest.OnSupportedPlatformsIt("doesn't allow OVN reserved IPs", func() {
			config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP = ovntest.MustParseIP(dummyMasqIPv4)
			config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP = ovntest.MustParseIP(dummyMasqIPv6)

			Expect(tc.ns.Do(func(netNS ns.NetNS) error {
				link, err := netlink.LinkByName(dummyBrName)
				if err != nil {
					return err
				}
				err = netlink.AddrAdd(link, &netlink.Addr{LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, IPNet: ovntest.MustParseIPNet(dummyMasqIPv4CIDR)})
				if err != nil {
					return err
				}
				return netlink.AddrAdd(link, &netlink.Addr{
					LinkIndex: link.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE, IPNet: ovntest.MustParseIPNet(dummyMasqIPv6CIDR)})
			})).ShouldNot(HaveOccurred())

			Consistently(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyMasqIPv4CIDR)) &&
					nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyMasqIPv6CIDR))
			}, 3).Should(BeFalse())
		})

		ovntest.OnSupportedPlatformsIt("doesn't allow OVN management port IPs", func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			Expect(tc.ns.Do(func(netNS ns.NetNS) error {
				mpLink := ovntest.AddLink(fmt.Sprintf("%s1234", ovntypes.K8sMgmtIntfNamePrefix))
				return netlink.AddrAdd(mpLink, &netlink.Addr{LinkIndex: mpLink.Attrs().Index, Scope: unix.RT_SCOPE_UNIVERSE,
					IPNet: ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR)})
			})).ShouldNot(HaveOccurred())
			Consistently(func() bool {
				return nodeHasAddress(tc.fakeClient, nodeName, ovntest.MustParseIPNet(dummyAdditionalIPv4CIDR))
			}, 2).Should(BeFalse())
		})
	})
})

func configureKubeOVNContextWithNs(nodeName string) *testCtx {
	testNs, err := testutils.NewNS()
	Expect(err).NotTo(HaveOccurred())
	setupPrimaryInfFn := func() error {
		link := ovntest.AddLink("breth0")
		if err = netlink.AddrAdd(link, &netlink.Addr{IPNet: ovntest.MustParseIPNet("10.1.1.10/24")}); err != nil {
			return err
		}
		return netlink.AddrAdd(link, &netlink.Addr{IPNet: ovntest.MustParseIPNet("2001:db8::10/64")})
	}
	Expect(testNs.Do(func(netNS ns.NetNS) error {
		return setupPrimaryInfFn()
	}))
	useNetlink := true
	var tc *testCtx
	testNs.Do(func(netNS ns.NetNS) error {
		tc = configureKubeOVNContext(nodeName, useNetlink)
		return nil
	})
	tc.ns = testNs
	return tc
}

func configureKubeOVNContext(nodeName string, useNetlink bool) *testCtx {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.OVNNodeHostCIDRs:           `["10.1.1.10/24", "2001:db8::10/64"]`,
				"k8s.ovn.org/l3-gateway-config": `{"default":{"mac-address":"52:54:00:e2:ed:d0","ip-addresses":["10.1.1.10/24"],"ip-address":"10.1.1.10/24","next-hops":["10.1.1.1"],"next-hop":"10.1.1.1"}}`,
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Address: "10.1.1.10", Type: corev1.NodeInternalIP}, {Address: "2001:db8::10", Type: corev1.NodeInternalIP}},
		},
	}

	tc := &testCtx{
		doneWg:      &sync.WaitGroup{},
		stopCh:      make(chan struct{}),
		fakeClient:  fake.NewSimpleClientset(node),
		mgmtPortIP4: ovntest.MustParseIPNet("10.1.1.2/24"),
		mgmtPortIP6: ovntest.MustParseIPNet("2001:db8::1/64"),
	}

	var err error
	fakeClientset := &util.OVNNodeClientset{
		KubeClient: tc.fakeClient,
	}
	tc.watchFactory, err = factory.NewNodeWatchFactory(fakeClientset, nodeName)
	Expect(err).NotTo(HaveOccurred())
	err = tc.watchFactory.Start()
	Expect(err).NotTo(HaveOccurred())

	_ = nodenft.SetFakeNFTablesHelper()

	fakeMgmtPortConfig := &managementPortConfig{
		ifName:    nodeName,
		link:      nil,
		routerMAC: nil,
		ipv4: &managementPortIPFamilyConfig{
			allSubnets: nil,
			ifAddr:     tc.mgmtPortIP4,
			gwIP:       tc.mgmtPortIP4.IP,
		},
		ipv6: &managementPortIPFamilyConfig{
			allSubnets: nil,
			ifAddr:     tc.mgmtPortIP6,
			gwIP:       tc.mgmtPortIP6.IP,
		},
	}
	err = setupManagementPortNFTables(fakeMgmtPortConfig)
	Expect(err).NotTo(HaveOccurred())

	fakeBridgeConfiguration := &bridgeConfiguration{bridgeName: "breth0"}

	k := &kube.Kube{KClient: tc.fakeClient}
	tc.ipManager = newAddressManagerInternal(nodeName, k, fakeMgmtPortConfig, tc.watchFactory, fakeBridgeConfiguration, useNetlink)
	return tc
}
