package egressip

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	ovnconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovnkube "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovniptables "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/util/iptables"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	kexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

// testPodConfig holds all the information needed to validate a config is applied for a pod
type testPodConfig struct {
	name        string // pod name
	ipTableRule ovniptables.RuleArg
	ipRule      *netlink.Rule
}

// eipConfig contains all the information needed to validate an EIP. Max one EIP IP maybe configured for a given
// EgressIP
type eipConfig struct {
	eIP        *egressipv1.EgressIP
	routes     []netlink.Route
	addr       *netlink.Addr
	inf        string
	podConfigs []testPodConfig
}

// nodeConfig holds networking configuration for a single kubernetes nodes
type nodeConfig struct {
	routes       []netlink.Route
	iptableRules []ovniptables.RuleArg
	ipRules      []netlink.Rule
	linkConfigs  []linkConfig
}

type linkConfig struct {
	linkName string
	address  []address
}

type address struct {
	cidr string
	eip  bool
}

const (
	namespace1                      = "ns1"
	namespace2                      = "ns2"
	namespace3                      = "ns3"
	namespace4                      = "ns4"
	node1Name                       = "node1"
	egressIP1Name                   = "egressip-1"
	egressIP2Name                   = "egressip-2"
	egressIP1IPV4                   = "5.5.5.50"
	egressIP1IPV4CIDR               = egressIP1IPV4 + "/32"
	egressIP1IPV6Compressed         = "2001::1:0:8a2e:370:2"
	egressIP1IPV6Uncompressed       = "2001:0000:0000:0001:0000:8a2e:0370:0002"
	egressIP2IPV4                   = "5.5.10.55"
	egressIP2IPV4CIDR               = egressIP2IPV4 + "/32"
	egressIP2IPV6Compressed         = "2001::2:0:8a2e:370:2"
	egressIP3IP                     = "10.10.10.10"
	egressIP3IPCIDR                 = egressIP3IP + "/32"
	egressIPv4Mask                  = "32"
	egressIPv6Mask                  = "128"
	oneSec                          = time.Second
	dummyLink1Name                  = "dummy1"
	dummyLink2Name                  = "dummy2"
	dummyLink3Name                  = "dummy3"
	dummyLink4Name                  = "dummy4"
	dummy1IPv4                      = "5.5.5.10"
	dummy1IPv4CIDR                  = dummy1IPv4 + "/24"
	dummy1IPv6Compressed            = "2001:0:0:1::1"
	dummy1IPv6CIDRCompressed        = dummy1IPv6Compressed + "/64"
	dummy1IPv4CIDRNetwork           = "5.5.5.0/24"
	dummy1IPv6CIDRNetworkCompressed = "2001:0:0:1::/64"
	dummy2IPv4                      = "5.5.10.15"
	dummy2IPv4CIDR                  = dummy2IPv4 + "/24"
	dummy2IPv6Compressed            = "2001:0:0:2::1"
	dummy2IPv6CIDRCompressed        = dummy2IPv6Compressed + "/64"
	dummy2IPv4CIDRNetwork           = "5.5.10.0/24"
	dummy2IPv6CIDRNetworkCompressed = "2001:0:0:2::/64"
	dummy3IPv4CIDR                  = "8.8.10.3/16"
	dummy3IPv6Compressed            = "2001:0:0:3::1"
	dummy3IPv6CIDRCompressed        = dummy3IPv6Compressed + "/64"
	dummy4IPv4CIDR                  = "9.8.10.3/16"
	dummy4IPv6Compressed            = "2001:0:0:4::1"
	dummy4IPv6CIDRCompressed        = dummy4IPv6Compressed + "/64"
	pod1Name                        = "testpod1"
	pod1IPv4                        = "192.168.100.2"
	pod1IPv4CIDR                    = pod1IPv4 + "/32"
	pod1IPv6Compressed              = "fd46::1"
	pod1IPv6CIDRCompressed          = pod1IPv6Compressed + "/128"
	pod2Name                        = "testpod2"
	pod2IPv4                        = "192.168.100.7"
	pod2IPv4CIDR                    = "192.168.100.7/32"
	pod2IPv6Compressed              = "fd46::2"
	pod2IPv6CIDRCompressed          = pod2IPv6Compressed + "/128"
	pod3Name                        = "testpod3"
	pod3IPv4                        = "192.168.100.9"
	pod3IPv4CIDR                    = "192.168.100.9/32"
	pod3IPv6Compressed              = "fd46::3"
	pod3IPv6CIDRCompressed          = pod3IPv6Compressed + "/128"
	pod4Name                        = "testpod4"
	pod4IPv4                        = "192.168.100.67"
	pod4IPv4CIDR                    = "192.168.100.67/32"
	pod4IPv6Compressed              = "fd46::4"
	pod4IPv6CIDRCompressed          = pod4IPv6Compressed + "/128"
	node1OVNNetworkV4               = "11.11.0.0/16"
)

var (
	egressPodLabel  = map[string]string{"egress": "needed"}
	namespace1Label = map[string]string{"prod1": ""}
	namespace2Label = map[string]string{"prod2": ""}
)

type cleanupFn func() error

func setupFakeNode(nodeInitialConfig nodeConfig) (ns.NetNS, error) {
	testNS, err := testutils.NewNS()
	if err != nil {
		return nil, fmt.Errorf("failed to create new network namespace: %v", err)
	}
	err = testNS.Do(func(netNS ns.NetNS) error {
		// adding links
		for _, link := range nodeInitialConfig.linkConfigs {
			if err = addLinkAndAddresses(link.linkName, link.address); err != nil {
				return fmt.Errorf("failed to add interface %s with addresses %v: %v", link.linkName, link.address, err)
			}
		}
		// adding routes
		for _, newRoute := range nodeInitialConfig.routes {
			if err = netlink.RouteAdd(&newRoute); err != nil {
				return fmt.Errorf("failed to add route (%s): %v", newRoute.String(), err)
			}
		}
		// adding IPTable rules
		ipTableV4Client := iptables.New(kexec.New(), iptables.ProtocolIPv4)
		ipTableV6Client := iptables.New(kexec.New(), iptables.ProtocolIPv6)
		var ipTableClient iptables.Interface
		for _, iptableRule := range nodeInitialConfig.iptableRules {
			if len(iptableRule.Args) != 0 {
				if isIPTableRuleArgIPV6(iptableRule.Args) {
					ipTableClient = ipTableV6Client
				} else {
					ipTableClient = ipTableV4Client
				}
				if err = ensureIPTableChainAndRule(ipTableClient, iptableRule.Args); err != nil {
					return err
				}
			}
		}
		// adding IP rules
		for _, ipRule := range nodeInitialConfig.ipRules {
			if err = netlink.RuleAdd(&ipRule); err != nil {
				return fmt.Errorf("failed to add IP rule (%v): %v", ipRule.String(), err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add node config: %v", err)
	}
	return testNS, nil
}

func ensureIPTableChainAndRule(ipt iptables.Interface, ruleArgs []string) error {
	_, err := ipt.EnsureChain(iptables.TableNAT, iptChainName)
	if err != nil {
		return fmt.Errorf("failed to create chain %s in NAT table: %v", iptChainName, err)
	}
	_, err = ipt.EnsureRule(iptables.Prepend, iptables.TableNAT, iptChainName, ruleArgs...)
	if err != nil {
		return fmt.Errorf("failed to ensure IPTables rule (%v): %v", ruleArgs, err)
	}
	return nil
}

func setupFakeTestNode(nodeInitialState nodeConfig) (ns.NetNS, cleanupFn, error) {
	testNS, err := setupFakeNode(nodeInitialState)
	if err != nil {
		return nil, nil, err
	}
	cleanupFn := func() error {
		if err := testNS.Close(); err != nil {
			return err
		}
		if err = testutils.UnmountNS(testNS); err != nil {
			return err
		}
		return nil
	}
	return testNS, cleanupFn, nil
}

func createVRFAndEnslaveLink(testNS ns.NetNS, linkName, vrfName string, vrfTable uint32) error {
	vrfLink := &netlink.Vrf{
		LinkAttrs: netlink.LinkAttrs{Name: vrfName},
		Table:     vrfTable,
	}
	return testNS.Do(func(netNS ns.NetNS) error {
		if err := netlink.LinkAdd(vrfLink); err != nil {
			return fmt.Errorf("failed to create VRF link %s and table ID %d: %v", vrfName, vrfTable, err)
		}
		if err := netlink.LinkSetUp(vrfLink); err != nil {
			return fmt.Errorf("failed to set VRF link %s up: %v", vrfName, err)
		}
		slaveLink, err := netlink.LinkByName(linkName)
		if err != nil {
			return fmt.Errorf("failed to get link %s: %v", linkName, err)
		}
		if err = netlink.LinkSetMaster(slaveLink, vrfLink); err != nil {
			return fmt.Errorf("failed to set link %s master to link %s: %v", linkName, vrfName, err)
		}
		return nil
	})
}

func initController(namespaces []corev1.Namespace, pods []corev1.Pod, egressIPs []egressipv1.EgressIP, node nodeConfig, v4, v6, createEIPAnnot bool) (*Controller, *egressipfake.Clientset, error) {

	kubeClient := fake.NewSimpleClientset(&corev1.NodeList{Items: []corev1.Node{getNodeObj(node, createEIPAnnot)}},
		&corev1.NamespaceList{Items: namespaces}, &corev1.PodList{Items: pods})
	egressIPClient := egressipfake.NewSimpleClientset(&egressipv1.EgressIPList{Items: egressIPs})
	ovnNodeClient := &util.OVNNodeClientset{KubeClient: kubeClient, EgressIPClient: egressIPClient}
	rm := routemanager.NewController()
	ovnconfig.OVNKubernetesFeature.EnableEgressIP = true
	watchFactory, err := factory.NewNodeWatchFactory(ovnNodeClient, node1Name)
	if err != nil {
		return nil, nil, err
	}
	if err := watchFactory.Start(); err != nil {
		return nil, nil, err
	}
	linkManager := linkmanager.NewController(node1Name, v4, v6, nil)
	c, err := NewController(&ovnkube.Kube{KClient: kubeClient}, watchFactory.EgressIPInformer(), watchFactory.NodeInformer(), watchFactory.NamespaceInformer(),
		watchFactory.PodCoreInformer(), rm, v4, v6, node1Name, linkManager)
	if err != nil {
		return nil, nil, err
	}
	_, err = c.namespaceInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return nil, nil, err
	}
	_, err = c.podInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return nil, nil, err
	}
	_, err = c.eIPInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onEIPAdd,
			UpdateFunc: c.onEIPUpdate,
			DeleteFunc: c.onEIPDelete,
		}))
	if err != nil {
		return nil, nil, err
	}
	return c, egressIPClient, nil
}

func runController(testNS ns.NetNS, c *Controller) (cleanupFn, error) {
	stopCh := make(chan struct{})
	for _, se := range []struct {
		resourceName string
		syncFn       cache.InformerSynced
	}{
		{"eipeip", c.eIPInformer.HasSynced},
		{"eipnamespace", c.namespaceInformer.HasSynced},
		{"eippod", c.podInformer.HasSynced},
	} {
		func(resourceName string, syncFn cache.InformerSynced) {
			if !util.WaitForInformerCacheSyncWithTimeout(resourceName, stopCh, syncFn) {
				gomega.PanicWith(fmt.Sprintf("timed out waiting for %q caches to sync", resourceName))
			}
		}(se.resourceName, se.syncFn)
	}

	wg := &sync.WaitGroup{}
	runSubControllers(testNS, c, wg, stopCh)

	err := testNS.Do(func(netNS ns.NetNS) error {
		return c.ruleManager.OwnPriority(rulePriority)
	})
	if err != nil {
		return nil, err
	}

	for i := 0; i < 1; i++ {
		for _, workerFn := range []func(*sync.WaitGroup){
			c.runEIPWorker,
			c.runPodWorker,
			c.runNamespaceWorker,
		} {
			wg.Add(1)
			go func(fn func(*sync.WaitGroup)) {
				defer wg.Done()
				_ = testNS.Do(func(netNS ns.NetNS) error {
					wait.Until(func() {
						fn(wg)
					}, 10*time.Millisecond, stopCh)
					return nil
				})

			}(workerFn)
		}
	}
	err = testNS.Do(func(netNS ns.NetNS) error {
		if err = c.ruleManager.OwnPriority(rulePriority); err != nil {
			return err
		}
		if c.v4 {
			if err = c.iptablesManager.OwnChain(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv4); err != nil {
				return err
			}
			if err = c.iptablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv4, iptJumpRule); err != nil {
				return err
			}
		}
		if c.v6 {
			if err = c.iptablesManager.OwnChain(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv6); err != nil {
				return err
			}
			if err = c.iptablesManager.EnsureRule(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv6, iptJumpRule); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	cleanupFn := func() error {
		close(stopCh)
		c.eIPQueue.ShutDown()
		c.podQueue.ShutDown()
		c.namespaceQueue.ShutDown()
		wg.Wait()
		return nil
	}
	return cleanupFn, err
}

func runSubControllers(testNS ns.NetNS, c *Controller, wg *sync.WaitGroup, stopCh chan struct{}) {
	// we do not call start for our controller because the newly created goroutines will not be set to the correct network namespace,
	// so we invoke them manually here and call reconcile manually
	// normally executed during Run but we call it manually here because run spawns a go routine that we cannot control its netns during test
	c.linkManager.Run(stopCh, wg)
	wg.Add(1)
	go testNS.Do(func(netNS ns.NetNS) error {
		c.iptablesManager.Run(stopCh, 10*time.Millisecond)
		wg.Done()
		return nil
	})
	wg.Add(1)
	go testNS.Do(func(netNS ns.NetNS) error {
		c.routeManager.Run(stopCh, 10*time.Millisecond)
		wg.Done()
		return nil
	})
	wg.Add(1)
	go testNS.Do(func(netNS ns.NetNS) error {
		c.ruleManager.Run(stopCh, 10*time.Millisecond)
		wg.Done()
		return nil
	})
}

// FIXME(mk) - Within GH VM, if I need to create a new NetNs. I see the following error:
// "failed to create new network namespace: mount --make-rshared /run/user/1001/netns failed: "operation not permitted""
var _ = ginkgo.DescribeTable("EgressIP selectors",
	func(expectedEIPConfigs []eipConfig, pods []corev1.Pod, namespaces []corev1.Namespace, nodeConfig nodeConfig) {
		defer ginkgo.GinkgoRecover()
		if os.Getenv("NOROOT") == "TRUE" {
			ginkgo.Skip("Test requires root privileges")
		}
		if !commandExists("iptables") {
			ginkgo.Skip("Test requires iptables tools to be available in PATH")
		}
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		var err error
		egressIPList := make([]egressipv1.EgressIP, 0)
		for _, expectedEIPConfig := range expectedEIPConfigs {
			if expectedEIPConfig.eIP == nil {
				continue
			}
			egressIPList = append(egressIPList, *expectedEIPConfig.eIP)
		}
		ginkgo.By("setting up test environment")
		// determine which IP versions we must support from the Egress IPs defined
		v4, v6 := getEIPsIPVersions(expectedEIPConfigs)
		// setup "node" environment before controller is started
		testNS, cleanupNodeFn, err := setupFakeTestNode(nodeConfig)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		c, eIPClient, err := initController(namespaces, pods, egressIPList, nodeConfig, v4, v6, true) //TODO: test for IPV6
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		cleanupControllerFn, err := runController(testNS, c)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		ginkgo.By("verify expected IPTable rules match what was found")
		// Ensure only the iptables rules we expect are present on the chain
		gomega.Eventually(func() error {
			foundIPTRules, err := getIPTableRules(testNS, c.iptablesManager, v4, v6)
			if err != nil {
				return err
			}
			// refator all this to account for new egress selected refactor
			// since iptables rules found must be unique and since each expected rule is also unique, it is sufficient
			// to say they are equal by comparing their length and ensuring we find all expected rules
			var ruleCount int
			for _, expectedEIPConfig := range expectedEIPConfigs {
				ruleCount += len(expectedEIPConfig.podConfigs)
			}
			if ruleCount != len(foundIPTRules) {
				return fmt.Errorf("expected and found IPTable rule(s) count do not match: expected %d IPtable rule(s) but got %d:\n"+
					"expected IPtable rule(s) '%v'\nfound IPTable rule(s) '%v'", ruleCount, len(foundIPTRules), expectedEIPConfigs, foundIPTRules)
			}
			for _, expectedEIPConfig := range expectedEIPConfigs {
				for _, expectedPodConfig := range expectedEIPConfig.podConfigs {
					var found bool
					for _, foundIPTRule := range foundIPTRules {
						if strings.Join(expectedPodConfig.ipTableRule.Args, " ") == strings.Join(foundIPTRule.Args, " ") {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("failed to find expected IPtable rule %+v", strings.Join(expectedPodConfig.ipTableRule.Args, " "))
					}
				}
			}
			return nil
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify expected IP routes match what was found")
		// Ensure only the routes we expect are present in a specific route table
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				// loop over egress IPs
				for _, expectedEIPConfig := range expectedEIPConfigs {
					if len(expectedEIPConfig.podConfigs) == 0 {
						// no pod configs expected therefore no IP routes
						continue
					}
					for _, status := range expectedEIPConfig.eIP.Status.Items {
						if status.Node != node1Name {
							continue
						}
						if status.EgressIP == "" {
							continue
						}
						// FIXME: we assume all EIPs are hosted by secondary host network. Skip OVN network EIPs.
						link, err := netlink.LinkByName(expectedEIPConfig.inf)
						if err != nil {
							return err
						}
						filter, mask := filterRouteByLinkTable(link.Attrs().Index, util.CalculateRouteTableID(link.Attrs().Index))
						existingRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, filter, mask)
						if err != nil {
							return fmt.Errorf("failed to list routes for link %q: %v", expectedEIPConfig.inf, err)
						}
						// only check if routes are present in the custom routing table and not equal because theres a race.
						// When we add an IP to a link (EIP), the linux kernal adds a route for that IP. We dont specify that
						// route in the expected routes.
						if !containsRoutes(expectedEIPConfig.routes, existingRoutes) {
							return fmt.Errorf("expected routes to be present in custom routing table for link %s but"+
								" they are not: \nExpected routes: %+v\nExisting routes: %+v", expectedEIPConfig.inf,
								expectedEIPConfig.routes, existingRoutes)
						}
					}
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify expected IP rules match what was found")
		// verify the correct rules are present and also no rule entries we do not expect
		expectedIPRules := make(map[string][]netlink.Rule, 0)
		for _, expectedEIPConfig := range expectedEIPConfigs {
			if len(expectedEIPConfig.podConfigs) == 0 {
				// no pod configs expected therefore no IP routes
				continue
			}
			var expectedRules []netlink.Rule
			for _, status := range expectedEIPConfig.eIP.Status.Items {
				if status.Node != node1Name {
					continue
				}
				for _, expectedPodConfig := range expectedEIPConfig.podConfigs {
					pod := getPod(pods, expectedPodConfig.name)
					ips, err := util.DefaultNetworkPodIPs(pod)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					for _, ip := range ips {
						expectedRules = append(expectedRules, generateIPRule(ip, utilnet.IsIPv6(ip), getLinkIndex(expectedEIPConfig.inf)))
					}
				}
			}
			expectedIPRules[expectedEIPConfig.eIP.Name] = expectedRules
		}
		// verify expected IP rules versus what was found
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				for _, expectedEIPConfig := range expectedEIPConfigs {
					if len(expectedEIPConfig.podConfigs) == 0 {
						// no pod configs expected therefore no IP routes
						continue
					}
					for _, status := range expectedEIPConfig.eIP.Status.Items {
						if status.Node != node1Name {
							continue
						}
						foundRules, err := netlink.RuleList(netlink.FAMILY_ALL)
						if err != nil {
							return err
						}
						temp := foundRules[:0]
						for _, rule := range foundRules {
							if rule.Priority == rulePriority {
								temp = append(temp, rule)
							}
						}
						foundRules = temp
						expectedRules := expectedIPRules[expectedEIPConfig.eIP.Name]
						var found bool
						for _, expectedRule := range expectedRules {
							found = false
							for _, foundRule := range foundRules {
								if expectedRule.Src.IP.Equal(foundRule.Src.IP) {
									if expectedRule.Table == foundRule.Table {
										found = true
										break
									}
								}
							}
							if !found {
								return fmt.Errorf("failed to find rule %s", expectedRule)
							}
						}
					}
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify expected egress IPs are assigned to interface")
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				for _, expectedEIPConfig := range expectedEIPConfigs {
					if expectedEIPConfig.addr == nil {
						return nil
					}
					link, err := netlink.LinkByName(expectedEIPConfig.inf)
					if err != nil {
						return err
					}
					addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					for _, addr := range addrs {
						if addr.IP.Equal(expectedEIPConfig.addr.IP) {
							return nil
						}
					}
					return fmt.Errorf("failed to find expected EIP IP %q from link %q addresses (%v)", expectedEIPConfig.addr.String(), expectedEIPConfig.inf, addrs)
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		// save annotations before removing EIP in-order to validate the addresses aren't assigned to a link anymore
		var annotationEIPs sets.Set[string]
		err = testNS.Do(func(netNS ns.NetNS) error {
			annotationEIPs, err = c.getAnnotation()
			return err
		})
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		ginkgo.By("remove EIPs in-order to validate cleanup is performed")
		for _, eIP := range egressIPList {
			gomega.Expect(eIPClient.K8sV1().EgressIPs().Delete(context.TODO(), eIP.Name, metav1.DeleteOptions{})).Should(gomega.Succeed())
		}
		ginkgo.By("verify IPTable rules are removed")
		gomega.Eventually(func() error {
			foundIPTRules, err := getIPTableRules(testNS, c.iptablesManager, v4, v6)
			if err != nil {
				return err
			}
			if len(foundIPTRules) != 0 {
				return fmt.Errorf("expected zero IPTable rules but found %d", len(foundIPTRules))
			}
			return nil
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify IP routes are removed")
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				// loop over all interfaces and ensure no EIP configured routes associated with links
				for _, linkConfig := range nodeConfig.linkConfigs {
					link, err := netlink.LinkByName(linkConfig.linkName)
					if err != nil {
						return err
					}
					filter, mask := filterRouteByLinkTable(link.Attrs().Index, util.CalculateRouteTableID(link.Attrs().Index))
					existingRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, filter, mask)
					if err != nil {
						return err
					}
					if len(existingRoutes) > 0 {
						return fmt.Errorf("expected zero routes for link %s but found %d: %v", link.Attrs().Name, len(existingRoutes), existingRoutes)
					}
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify IP rules are removed")
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				filter, mask := filterRuleByPriority(rulePriority)
				foundRules, err := netlink.RuleListFiltered(netlink.FAMILY_ALL, filter, mask)
				if err != nil {
					return err
				}
				if len(foundRules) > 0 {
					return fmt.Errorf("expected zero IP rules but found %d: %v", len(foundRules), foundRules)
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("verify assigned IP addresses are removed from annotation")
		gomega.Eventually(func() error {
			eipIPs, err := c.getAnnotation()
			if err != nil {
				return err
			}
			if eipIPs.Len() != 0 {
				return fmt.Errorf("expected 0 EIPs in annotation but found %d", eipIPs.Len())
			}
			return nil
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		ginkgo.By("ensure no Egress IP is still assigned to a link")
		gomega.Eventually(func() error {
			return testNS.Do(func(netNS ns.NetNS) error {
				for _, linkConfig := range nodeConfig.linkConfigs {
					link, err := netlink.LinkByName(linkConfig.linkName)
					if err != nil {
						return fmt.Errorf("failed to find link %q: %v", linkConfig.linkName, err)
					}
					addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
					if err != nil {
						return fmt.Errorf("failed to list addresses for link %q: %v", linkConfig.linkName, err)
					}
					for _, address := range addresses {
						if annotationEIPs.Has(address.IP.String()) {
							ginkgo.Fail(fmt.Sprintf("Egress IP %s found on link %s but should have been removed",
								address.IP.String(), linkConfig.linkName))
						}
					}
				}
				return nil
			})
		}).WithTimeout(oneSec).Should(gomega.Succeed())
		gomega.Expect(cleanupControllerFn()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(cleanupNodeFn()).ShouldNot(gomega.HaveOccurred())
	},
	ginkgo.Entry("configures nothing when EIPs dont select anything",
		[]eipConfig{
			{
				eIP: newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{dummy2IPv4CIDR, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv4 EIP and one Pod",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv4CIDRNetwork)},
				getNetlinkAddr(egressIP1IPV4, egressIPv4Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod1IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{dummy2IPv4CIDR, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv6 EIP and one Pod",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV6Compressed, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink1Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP1IPV6Compressed, egressIPv6Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod1IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv6Compressed, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv6CIDRCompressed, false}}},
				{dummyLink2Name, []address{{dummy2IPv6CIDRCompressed, false}}}},
		},
	),
	ginkgo.Entry("configures one uncompressed IPv6 EIP and one Pod",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV6Uncompressed, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink1Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP1IPV6Compressed, egressIPv6Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod1IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv6Compressed, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv6CIDRCompressed, false}}},
				{dummyLink2Name, []address{{dummy2IPv6CIDRCompressed, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv4 EIP and multiple pods",
		// Test pod and namespace selection -
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv4CIDRNetwork)},
				getNetlinkAddr(egressIP1IPV4, egressIPv4Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod1IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod2IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel),
			newPodWithLabels(namespace1, pod2Name, node1Name, pod2IPv4, egressPodLabel),
			newPodWithLabels(namespace1, pod3Name, node1Name, pod3IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, map[string]string{})},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{dummy2IPv4CIDR, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv6 EIP and multiple pods",
		// Test pod and namespace selection -
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV6Compressed, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink1Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP1IPV6Compressed, egressIPv6Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod1IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod2IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace1, pod2Name, node1Name, pod2IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace1, pod3Name, node1Name, pod3IPv6Compressed, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, map[string]string{})},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv6CIDRNetworkCompressed, false}}},
				{dummyLink2Name, []address{{dummy2IPv6CIDRCompressed, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv4 EIP and multiple namespaces and multiple pods",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv4CIDRNetwork)},
				getNetlinkAddr(egressIP1IPV4, egressIPv4Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod1IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod2IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel),
			newPodWithLabels(namespace2, pod2Name, node1Name, pod2IPv4, egressPodLabel),
			newPodWithLabels(namespace2, pod3Name, node1Name, pod3IPv4, map[string]string{}),
			newPodWithLabels(namespace3, pod4Name, node1Name, pod4IPv4, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, namespace1Label),
			newNamespaceWithLabels(namespace3, map[string]string{})},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{dummy2IPv4CIDR, false}}}},
		},
	),
	ginkgo.Entry("configures one IPv6 EIP and multiple namespaces and multiple pods",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV6Compressed, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink1Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP1IPV6Compressed, egressIPv6Mask),
				dummyLink1Name,
				[]testPodConfig{
					{pod1Name,
						getIPTableMasqRule(pod1IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod1IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod2IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace2, pod2Name, node1Name, pod2IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace2, pod3Name, node1Name, pod3IPv6Compressed, map[string]string{}),
			newPodWithLabels(namespace3, pod4Name, node1Name, pod4IPv6Compressed, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, namespace1Label),
			newNamespaceWithLabels(namespace3, map[string]string{})},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv6CIDRCompressed, false}}},
				{dummyLink2Name, []address{{dummy2IPv6CIDRCompressed, false}}}},
		},
	),
	ginkgo.Entry("configures multiple IPv4 EIPs on different links, multiple namespaces and multiple pods",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv4CIDRNetwork)},
				getNetlinkAddr(egressIP1IPV4, egressIPv4Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod1IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IPV4),
						getRule(pod2IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
			{
				newEgressIP(egressIP2Name, egressIP2IPV4, node1Name, namespace2Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink2Name)),
					getDstRoute(getLinkIndex(dummyLink2Name), dummy2IPv4CIDRNetwork)},
				getNetlinkAddr(egressIP2IPV4, egressIPv4Mask),
				dummyLink2Name,
				[]testPodConfig{
					{
						pod3Name,
						getIPTableMasqRule(pod3IPv4CIDR, dummyLink2Name, egressIP2IPV4),
						getRule(pod3IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink2Name))),
					},
					{
						pod4Name,
						getIPTableMasqRule(pod4IPv4CIDR, dummyLink2Name, egressIP2IPV4),
						getRule(pod4IPv4, util.CalculateRouteTableID(getLinkIndex(dummyLink2Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel),
			newPodWithLabels(namespace2, pod2Name, node1Name, pod2IPv4, egressPodLabel),
			newPodWithLabels(namespace3, pod3Name, node1Name, pod3IPv4, egressPodLabel),
			newPodWithLabels(namespace4, pod4Name, node1Name, pod4IPv4, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, namespace1Label),
			newNamespaceWithLabels(namespace3, namespace2Label), newNamespaceWithLabels(namespace4, namespace2Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{dummy2IPv4CIDR, false}}},
				{dummyLink3Name, []address{{dummy3IPv4CIDR, false}}},
				{dummyLink4Name, []address{{dummy4IPv4CIDR, false}}}},
		},
	),
	ginkgo.Entry("configures multiple IPv6 EIPs on different links, multiple namespaces and multiple pods",
		[]eipConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IPV6Compressed, node1Name, namespace1Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink1Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink1Name)),
					getDstRoute(getLinkIndex(dummyLink1Name), dummy1IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP1IPV6Compressed, egressIPv6Mask),
				dummyLink1Name,
				[]testPodConfig{
					{
						pod1Name,
						getIPTableMasqRule(pod1IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod1IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
					{
						pod2Name,
						getIPTableMasqRule(pod2IPv6CIDRCompressed, dummyLink1Name, egressIP1IPV6Compressed),
						getRule(pod2IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))),
					},
				},
			},
			{
				newEgressIP(egressIP2Name, egressIP2IPV6Compressed, node1Name, namespace2Label, egressPodLabel),
				[]netlink.Route{getDefaultIPv6Route(getLinkIndex(dummyLink2Name)),
					getLinkLocalRoute(getLinkIndex(dummyLink2Name)),
					getDstRoute(getLinkIndex(dummyLink2Name), dummy2IPv6CIDRNetworkCompressed)},
				getNetlinkAddr(egressIP2IPV6Compressed, egressIPv6Mask),
				dummyLink2Name,
				[]testPodConfig{
					{
						pod3Name,
						getIPTableMasqRule(pod3IPv6CIDRCompressed, dummyLink2Name, egressIP2IPV6Compressed),
						getRule(pod3IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink2Name))),
					},
					{
						pod4Name,
						getIPTableMasqRule(pod4IPv6CIDRCompressed, dummyLink2Name, egressIP2IPV6Compressed),
						getRule(pod4IPv6Compressed, util.CalculateRouteTableID(getLinkIndex(dummyLink2Name))),
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace2, pod2Name, node1Name, pod2IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace3, pod3Name, node1Name, pod3IPv6Compressed, egressPodLabel),
			newPodWithLabels(namespace4, pod4Name, node1Name, pod4IPv6Compressed, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, namespace1Label),
			newNamespaceWithLabels(namespace3, namespace2Label), newNamespaceWithLabels(namespace4, namespace2Label)},
		nodeConfig{
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv6CIDRCompressed, false}}},
				{dummyLink2Name, []address{{dummy2IPv6CIDRCompressed, false}}},
				{dummyLink3Name, []address{{dummy3IPv6CIDRCompressed, false}}},
				{dummyLink4Name, []address{{dummy4IPv6CIDRCompressed, false}}}},
		},
	),
)

var _ = ginkgo.Describe("label to annotations migration", func() {
	// Test using root and a test netns because we want to test between netlink lib
	// and the egress IP components (link manager, route manager)
	defer ginkgo.GinkgoRecover()
	if os.Getenv("NOROOT") == "TRUE" {
		ginkgo.Skip("Test requires root privileges")
	}
	if !commandExists("iptables") {
		ginkgo.Skip("Test requires iptables tools to be available in PATH")
	}
	var testNS ns.NetNS
	var c *Controller
	var cleanupFn cleanupFn
	var err error

	ginkgo.BeforeEach(func() {
		runtime.LockOSThread()
		initLinkConfig := nodeConfig{
			linkConfigs: []linkConfig{
				{
					dummyLink1Name,
					[]address{{cidr: dummy1IPv4CIDR}, {cidr: egressIP1IPV4CIDR, eip: true}},
				},
				{
					dummyLink2Name,
					[]address{{cidr: dummy2IPv4CIDR}, {cidr: egressIP2IPV4CIDR, eip: true}, {cidr: egressIP3IPCIDR, eip: true}},
				},
			},
		}
		// setup fake test node will create the links, addresses specified on said link and will also create the associated annotation
		// entries for any EIPs.
		testNS, cleanupFn, err = setupFakeTestNode(initLinkConfig)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		// set labels on each eip address
		gomega.Expect(setDepreciatedManagedAddressLabel(testNS, egressIP1IPV4CIDR, egressIP2IPV4CIDR, egressIP3IPCIDR)).ShouldNot(gomega.HaveOccurred())
		// init controller and set createEIPAnnot to false, therfore no annotation created
		c, _, err = initController(nil, nil, nil, initLinkConfig, true, false, false)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	})

	ginkgo.It("creates annotation", func() {
		gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
			return c.migrateFromAddrLabelToAnnotation()
		})).Should(gomega.Succeed())
		gomega.Eventually(func() bool {
			assignedEIPs, err := c.getAnnotation()
			if err != nil {
				panic(err.Error())
			}
			return assignedEIPs.Has(egressIP1IPV4) &&
				assignedEIPs.Has(egressIP2IPV4) &&
				assignedEIPs.Has(egressIP3IP)
		}).Should(gomega.BeTrue())
	})

	ginkgo.AfterEach(func() {
		defer runtime.UnlockOSThread()
		gomega.Expect(cleanupFn()).Should(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("VRF", func() {
	ginkgo.It("copies routes from the VRF routing table for a link enslaved by VRF device", func() {
		defer ginkgo.GinkgoRecover()
		if os.Getenv("NOROOT") == "TRUE" {
			ginkgo.Skip("Test requires root privileges")
		}
		if !commandExists("iptables") {
			ginkgo.Skip("Test requires iptables tools to be available in PATH")
		}
		vrfName := "vrf-dummy"
		var vrfTable uint32 = 55555
		ginkgo.By("setup link")
		nodeConfig := nodeConfig{routes: []netlink.Route{}, linkConfigs: []linkConfig{
			{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
		}}
		testNS, cleanupNodeFn, err := setupFakeTestNode(nodeConfig)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), "fake node setup should succeed")
		ginkgo.By("create VRF and add link")
		gomega.Expect(createVRFAndEnslaveLink(testNS, dummyLink1Name, vrfName, vrfTable)).Should(gomega.Succeed())
		ginkgo.By("add route to routing table associated with VRF")
		vrfRoute := getDstRouteForTable(getLinkIndex(dummyLink1Name), int(vrfTable), dummy3IPv4CIDR)
		gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
			return netlink.RouteAdd(&vrfRoute)
		})).Should(gomega.Succeed())
		egressIPList := []egressipv1.EgressIP{*newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel)}
		pods := []corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel)}
		namespaces := []corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)}
		ginkgo.By("start controller")
		c, _, err := initController(namespaces, pods, egressIPList, nodeConfig, true, false, true)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		cleanupControllerFn, err := runController(testNS, c)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		ginkgo.By("ensure route previously added is copied to new routing table")
		eipTable := util.CalculateRouteTableID(getLinkIndex(dummyLink1Name))
		gomega.Eventually(func() bool {
			eipRoutes, err := getRoutesFromTable(testNS, eipTable, netlink.FAMILY_V4)
			gomega.Expect(err).Should(gomega.Succeed())
			for _, eipRoute := range eipRoutes {
				if eipRoute.Dst.String() == vrfRoute.Dst.String() {
					return true
				}
			}
			return false
		}).Should(gomega.BeTrue(), "route should be copied to new routing table")
		gomega.Expect(cleanupControllerFn()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(cleanupNodeFn()).ShouldNot(gomega.HaveOccurred())
	})
})

var _ = ginkgo.DescribeTable("repair node", func(expectedStateFollowingClean []eipConfig,
	nodeConfigsBeforeRepair nodeConfig, pods []corev1.Pod, namespaces []corev1.Namespace) {
	// Test using root and a test netns because we want to test between netlink lib
	// and the egress IP components (link manager, route manager)
	defer ginkgo.GinkgoRecover()
	if os.Getenv("NOROOT") == "TRUE" {
		ginkgo.Skip("Test requires root privileges")
	}
	if !commandExists("iptables") {
		ginkgo.Skip("Test requires iptables tools to be available in PATH")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ginkgo.By("setting up test environment and controller")
	// if no EIPs, enable v4, otherwise enable IP versions based on what IP versions the EIP IPs contain
	var v4, v6 bool
	if len(expectedStateFollowingClean) == 0 {
		v4 = true
	} else {
		v4, v6 = getEIPsIPVersions(expectedStateFollowingClean)
	}
	egressIPList := make([]egressipv1.EgressIP, 0)
	for _, testEIPConfig := range expectedStateFollowingClean {
		if testEIPConfig.eIP == nil {
			continue
		}
		egressIPList = append(egressIPList, *testEIPConfig.eIP)
	}
	testNS, cleanupNodeFn, err := setupFakeTestNode(nodeConfigsBeforeRepair)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	c, _, err := initController(namespaces, pods, egressIPList, nodeConfigsBeforeRepair, v4, v6, true)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	wg := &sync.WaitGroup{}
	stopCh := make(chan struct{}, 0)
	runSubControllers(testNS, c, wg, stopCh)
	// for now, we expect it to always succeed
	err = testNS.Do(func(netNS ns.NetNS) error {
		return c.repairNode()
	})
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	// check node
	ginkgo.By("ensure no stale IPTable rules")
	foundIPTRules, err := getIPTableRules(testNS, c.iptablesManager, v4, v6)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	var expectedIPTableRuleCount int
	for _, expectedConfig := range expectedStateFollowingClean {
		if len(expectedConfig.podConfigs) == 0 {
			continue
		}
		for _, podConfig := range expectedConfig.podConfigs {
			expectedIPTableRuleCount += 1
			gomega.Expect(isIPTableRuleFound(foundIPTRules, podConfig.ipTableRule)).Should(gomega.BeTrue())
		}
	}
	gomega.Expect(expectedIPTableRuleCount).Should(gomega.Equal(len(foundIPTRules)))
	ginkgo.By("ensure no stale Egress IP addresses assigned to interface(s)")
	linkNames := make([]string, 0)
	for _, expectedState := range expectedStateFollowingClean {
		if expectedState.inf == "" {
			continue
		}
		linkNames = append(linkNames, expectedState.inf)
	}
	linkAddresses, err := getLinkAddresses(testNS, linkNames...)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	eipIPs, err := c.getAnnotation()
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	for _, existingAddresses := range linkAddresses {
		for _, existingAddress := range existingAddresses {
			gomega.Expect(eipIPs.Has(existingAddress.IP.String())).Should(gomega.BeFalse())
		}
	}
	ginkgo.By("ensure no stale routes")
	expectedRoutes := sets.New[string]()
	for _, expectedState := range expectedStateFollowingClean {
		for _, expectedRoute := range expectedState.routes {
			expectedRoutes.Insert(expectedRoute.String())
		}
	}
	foundRoutes := sets.New[string]()
	for _, expectedState := range expectedStateFollowingClean {
		if expectedState.inf == "" {
			continue
		}
		existingRoutes, err := getRoutesForLinkFromTable(testNS, expectedState.inf)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		for _, existingRoute := range existingRoutes {
			foundRoutes.Insert(existingRoute.String())
		}
	}
	diff := foundRoutes.Difference(expectedRoutes)
	gomega.Expect(diff.Len()).Should(gomega.BeZero())
	close(stopCh)
	wg.Wait()
	gomega.Expect(cleanupNodeFn()).ShouldNot(gomega.HaveOccurred())
}, ginkgo.Entry("should not fail when node is clean and nothing to apply",
	[]eipConfig{},
	nodeConfig{
		linkConfigs: []linkConfig{
			{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
		},
	},
	[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
	[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)}),
	ginkgo.Entry("should remove stale route with no assigned IP",
		[]eipConfig{},
		nodeConfig{ // node state before repair
			routes: []netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink1Name))},
			linkConfigs: []linkConfig{
				{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
			},
		},
		[]corev1.Pod{},
		[]corev1.Namespace{}),
	ginkgo.Entry("should remove stale address",
		[]eipConfig{},
		nodeConfig{ // node state before repair
			linkConfigs: []linkConfig{
				{dummyLink1Name, []address{{egressIP1IPV4CIDR, true}}},
			},
		},
		[]corev1.Pod{},
		[]corev1.Namespace{}),
	ginkgo.Entry("should remove stale route and EIP address on wrong link",
		[]eipConfig{
			{
				eIP: newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
			},
		},
		nodeConfig{ // node state before repair
			linkConfigs: []linkConfig{
				{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}},
				{dummyLink2Name, []address{{egressIP1IPV4CIDR, true}}},
			},
			routes: []netlink.Route{getDefaultIPv4Route(getLinkIndex(dummyLink2Name))},
		},
		[]corev1.Pod{},
		[]corev1.Namespace{}),
	ginkgo.Entry("should remove stale iptables rules",
		[]eipConfig{
			{
				eIP: newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
			},
		},
		nodeConfig{ // node state before repair
			linkConfigs:  []linkConfig{{dummyLink2Name, nil}},
			iptableRules: []ovniptables.RuleArg{generateIPTablesSNATRuleArg(net.ParseIP(pod1IPv4), false, dummyLink1Name, egressIP1IPV4)},
		},
		[]corev1.Pod{},
		[]corev1.Namespace{}),
	ginkgo.Entry("should remove stale iptables rules but not valid rules",
		[]eipConfig{
			{
				eIP: newEgressIP(egressIP1Name, egressIP1IPV4, node1Name, namespace1Label, egressPodLabel),
				podConfigs: []testPodConfig{
					{
						ipTableRule: generateIPTablesSNATRuleArg(net.ParseIP(pod1IPv4), false, dummyLink1Name, egressIP1IPV4),
					},
				},
			},
		},
		nodeConfig{ // node state before repair
			iptableRules: []ovniptables.RuleArg{generateIPTablesSNATRuleArg(net.ParseIP(pod1IPv4), false, dummyLink1Name, egressIP1IPV4), // valid
				generateIPTablesSNATRuleArg(net.ParseIP(pod2IPv4), false, dummyLink1Name, egressIP1IPV4), // invalid
			},
			linkConfigs: []linkConfig{{dummyLink1Name, []address{{dummy1IPv4CIDR, false}}}},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
	))

func isIPTableRuleFound(rules []ovniptables.RuleArg, candidateRule ovniptables.RuleArg) bool {
	for _, rule := range rules {
		if reflect.DeepEqual(rule.Args, candidateRule.Args) {
			return true
		}
	}
	return false
}

func newPodWithLabels(namespace, name, node, podIP string, additionalLabels map[string]string) corev1.Pod {
	podIPs := []corev1.PodIP{}
	if podIP != "" {
		podIPs = append(podIPs, corev1.PodIP{IP: podIP})
	}
	return corev1.Pod{
		ObjectMeta: newPodMeta(namespace, name, additionalLabels),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  podIP,
			PodIPs: podIPs,
		},
	}
}

func newPodMeta(namespace, name string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": name,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels:    labels,
	}
}

func newNamespaceWithLabels(namespace string, additionalLabels map[string]string) corev1.Namespace {
	return corev1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, additionalLabels),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}

func newNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         types.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func getNodeObj(testNode nodeConfig, createEIPAnnot bool) corev1.Node {
	hostCIDRs := make([]string, 0)
	assignedEIPs := sets.New[string]()
	hostCIDRs = append(hostCIDRs, fmt.Sprintf("\"%s\"", node1OVNNetworkV4))
	// we assume all extra interfaces and their addresses are valid for host addr
	for _, linkAddrs := range testNode.linkConfigs {
		for _, addrs := range linkAddrs.address {
			if addrs.eip {
				addrSplit := strings.Split(addrs.cidr, "/")
				if len(addrSplit) != 2 {
					panic(fmt.Sprintf("invalid CIDR %s", addrs.cidr))
				}
				assignedEIPs.Insert(addrSplit[0])
			}
			hostCIDRs = append(hostCIDRs, fmt.Sprintf("\"%s\"", addrs.cidr))
		}
	}
	annots := map[string]string{
		"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
			node1OVNNetworkV4, ""),
		util.OVNNodeHostCIDRs: fmt.Sprintf("[%s]", strings.Join(hostCIDRs, ",")),
	}
	if createEIPAnnot && assignedEIPs.Len() > 0 {
		bytes, err := json.Marshal(assignedEIPs.UnsortedList())
		if err != nil {
			panic(err.Error())
		}
		annots[util.OVNNodeSecondaryHostEgressIPs] = string(bytes)
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node1Name,
			Annotations: annots,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func getLinkAddresses(testNS ns.NetNS, infs ...string) (map[string][]netlink.Addr, error) {
	var err error
	linkAddresses := make(map[string][]netlink.Addr)
	err = testNS.Do(func(netNS ns.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		for _, link := range links {
			// if infs isnt specified, get all links
			if !isStrInArray(infs, link.Attrs().Name) {
				continue
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				return err
			}
			linkAddresses[link.Attrs().Name] = addresses
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return linkAddresses, nil
}

func getIPTableRules(testNS ns.NetNS, iptablesManager *ovniptables.Controller, v4, v6 bool) ([]ovniptables.RuleArg, error) {
	var foundIPTRules []ovniptables.RuleArg
	var err error
	err = testNS.Do(func(netNS ns.NetNS) error {
		if v4 {
			foundIPTRulesV4, err := iptablesManager.GetIPv4ChainRuleArgs(utiliptables.TableNAT, iptChainName)
			if err != nil {
				return err
			}
			foundIPTRules = append(foundIPTRules, foundIPTRulesV4...)
		}
		if v6 {
			foundIPTRulesV6, err := iptablesManager.GetIPv6ChainRuleArgs(utiliptables.TableNAT, iptChainName)
			if err != nil {
				return err
			}
			foundIPTRules = append(foundIPTRules, foundIPTRulesV6...)
		}
		return nil
	})
	return foundIPTRules, err
}

func getRoutesForLinkFromTable(testNS ns.NetNS, linkName string) ([]netlink.Route, error) {
	var err error
	var routes []netlink.Route
	err = testNS.Do(func(netNS ns.NetNS) error {
		link, err := netlink.LinkByName(linkName)
		if err != nil {
			return err
		}
		filterRoute, filterMask := filterRouteByLinkTable(link.Attrs().Index, util.CalculateRouteTableID(link.Attrs().Index))
		routes, err = netlink.RouteListFiltered(netlink.FAMILY_ALL, filterRoute, filterMask)
		if err != nil {
			return err
		}
		return nil
	})
	return routes, err
}

func getRoutesFromTable(testNS ns.NetNS, table, family int) ([]netlink.Route, error) {
	var err error
	var routes []netlink.Route
	err = testNS.Do(func(netNS ns.NetNS) error {
		filterRoute, filterMask := filterRouteByTable(table)
		routes, err = netlink.RouteListFiltered(family, filterRoute, filterMask)
		if err != nil {
			return err
		}
		return nil
	})
	return routes, err
}

func filterRouteByTable(table int) (*netlink.Route, uint64) {
	return &netlink.Route{
			Table: table,
		},
		netlink.RT_FILTER_TABLE
}

func newEgressIPMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  types.UID(name),
		Name: name,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newEgressIP(name, ip, node string, namespaceLabels, podLabels map[string]string) *egressipv1.EgressIP {
	return &egressipv1.EgressIP{
		ObjectMeta: newEgressIPMeta(name),
		Spec: egressipv1.EgressIPSpec{
			EgressIPs: []string{ip},
			PodSelector: metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: namespaceLabels,
			},
		},
		Status: egressipv1.EgressIPStatus{
			Items: []egressipv1.EgressIPStatusItem{{
				node,
				ip,
			}},
		},
	}
}

var index = 5

func addLinkAndAddresses(name string, addresses []address) error {
	if name == "" {
		return fmt.Errorf("must define valid interface name")
	}
	index += 1
	mac, _ := net.ParseMAC("00:00:5e:00:53:" + fmt.Sprintf("%02d", index))
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{
		Index:        getLinkIndex(name),
		MTU:          1500,
		Name:         name,
		HardwareAddr: mac,
	}}
	if err := netlink.LinkAdd(dummy); err != nil {
		return fmt.Errorf("failed to add dummy link %q: %v", dummy.Name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if err = netlink.LinkSetUp(link); err != nil {
		return err
	}
	for _, address := range addresses {
		if address.cidr == "" {
			continue
		}
		nlAddress, err := netlink.ParseAddr(address.cidr)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(dummy, nlAddress); err != nil {
			return err
		}
	}
	return nil
}

func getIPTableMasqRule(podIP, infName, snatIP string) ovniptables.RuleArg {
	return ovniptables.RuleArg{Args: []string{"-s", podIP, "-o", infName, "-j", "SNAT", "--to-source", snatIP}}
}

// isIPTableRuleArgIPV6 iterates through rule args starting at the end and attempts to find an IP to determine the version.
// If an IP is found to be a specific IP version, we can conclude the entire RuleArg is that IP version.
func isIPTableRuleArgIPV6(ruleArgs []string) bool {
	if len(ruleArgs) == 0 {
		panic("unable to determine IP version because IPTable rule is empty or nil")
	}
	for i := len(ruleArgs) - 1; i >= 0; i-- {
		// try IP first
		ip := net.ParseIP(ruleArgs[i])
		if len(ip) > 0 {
			if ip.To4() == nil {
				return true
			} else {
				return false
			}
		} else {
			// try CIDR
			ip, _, err := net.ParseCIDR(ruleArgs[i])
			if err != nil {
				continue
			}
			if ip.To4() == nil {
				return true
			} else {
				return false
			}
		}
	}
	panic("unable to determine IP version of IPTable rule")
}

func getDefaultIPv4Route(linkIndex int) netlink.Route {
	// dst is nil because netlink represents a default route as nil
	return netlink.Route{LinkIndex: linkIndex, Table: util.CalculateRouteTableID(linkIndex), Dst: defaultV4AnyCIDR}
}

func getDefaultIPv6Route(linkIndex int) netlink.Route {
	// dst is nil because netlink represents a default route as nil
	return netlink.Route{LinkIndex: linkIndex, Table: util.CalculateRouteTableID(linkIndex), Dst: defaultV6AnyCIDR}
}

func getDstRoute(linkIndex int, dst string) netlink.Route {
	return getDstRouteForTable(linkIndex, util.CalculateRouteTableID(linkIndex), dst)

}

func getDstRouteForTable(linkIndex, table int, dst string) netlink.Route {
	_, dstIPNet, err := net.ParseCIDR(dst)
	if err != nil {
		panic(err.Error())
	}
	return netlink.Route{LinkIndex: linkIndex, Dst: dstIPNet, Table: table}
}

func getDstWithSrcRoute(linkIndex int, dst, src string) netlink.Route {
	_, dstIPNet, err := net.ParseCIDR(dst)
	if err != nil {
		panic(err.Error())
	}
	ip := net.ParseIP(src)
	if len(ip) == 0 {
		panic("invalid src IP")
	}
	return netlink.Route{LinkIndex: linkIndex, Dst: dstIPNet, Src: ip, Table: util.CalculateRouteTableID(linkIndex)}
}

func getLinkLocalRoute(linkIndex int) netlink.Route {
	return netlink.Route{LinkIndex: linkIndex, Dst: linkLocalCIDR, Table: util.CalculateRouteTableID(linkIndex)}
}

func getNetlinkAddr(ip, netmask string) *netlink.Addr {
	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%s", ip, netmask))
	if err != nil {
		panic(fmt.Sprintf("failed to parse %q: %v", ip, err))
	}
	return addr
}

// areRoutesEqual turns true if routes are partially equal. A limited set of fields within a route are checked to ensure
// they are equal. The reason a limit set is checked is that a user may define a limited subset of fields but when we retrieve
// this route from the system, other fields are populated by default.
// Duplicate routes aren't tolerated.
func areRoutesEqual(routes1 []netlink.Route, routes2 []netlink.Route) bool {
	if len(routes1) != len(routes2) {
		return false
	}
	var found bool
	for _, route1 := range routes1 {
		found = false
		for _, route2 := range routes2 {
			if routemanager.RoutePartiallyEqual(route1, route2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, route2 := range routes2 {
		found = false
		for _, route1 := range routes1 {
			if routemanager.RoutePartiallyEqual(route2, route1) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// containsRoutes returns true if routes in routes1 are presents in routes routes2
func containsRoutes(routes1 []netlink.Route, routes2 []netlink.Route) bool {
	var found bool
	for _, route1 := range routes1 {
		found = false
		for _, route2 := range routes2 {
			if routemanager.RoutePartiallyEqual(route1, route2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func getRule(podIP string, tableID int) *netlink.Rule {
	ipNet, err := util.GetIPNetFullMask(podIP)
	if err != nil {
		panic(err.Error())
	}
	return &netlink.Rule{
		Priority: rulePriority,
		Src:      ipNet,
		Table:    tableID,
	}
}

// getLinkIndex is used to map interface name to a link index. When creating dummy type interfaces, we set the
// link index using this function which can be used later to lookup a link index
func getLinkIndex(linkName string) int {
	return hash(linkName)%200 + 5
}

func hash(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32())
}

func getPod(pods []corev1.Pod, name string) *corev1.Pod {
	for _, pod := range pods {
		if pod.Name == name {
			return &pod
		}
	}
	panic("failed to find a pod")
}

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func isStrInArray(elements []string, candidate string) bool {
	for _, element := range elements {
		if element == candidate {
			return true
		}
	}
	return false
}

func setDepreciatedManagedAddressLabel(testNS ns.NetNS, cidrs ...string) error {
	return testNS.Do(func(netNS ns.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			return nil
		}
		for _, link := range links {
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				return err
			}
			for _, address := range addresses {
				if isStrInArray(cidrs, address.IPNet.String()) {
					// cannot use netlink.AddrReplace b/c it doesn't update labels, so we must delete address and add it
					if err = netlink.AddrDel(link, &address); err != nil {
						return err
					}
					address.Label = linkmanager.DeprecatedGetAssignedAddressLabel(link.Attrs().Name)
					if err = netlink.AddrAdd(link, &address); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func getEIPsIPVersions(configs []eipConfig) (bool, bool) {
	var v4, v6 bool
	for _, config := range configs {
		tempV4, tempV6 := getEIPIPVersions(config.eIP)
		if tempV4 {
			v4 = true
		}
		if tempV6 {
			v6 = true
		}
	}
	if !v4 && !v6 {
		panic("unable to determine an IP version from EIPs")
	}
	return v4, v6
}

func getEIPIPVersions(eip *egressipv1.EgressIP) (bool, bool) {
	var v4, v6 bool
	for _, ipStr := range eip.Spec.EgressIPs {
		ip := net.ParseIP(ipStr)
		if len(ip) == 0 {
			panic("unable to deterine IP version because invalid IP")
		}
		if ip.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	if !v4 && !v6 {
		panic("unable to determine an IP version")
	}
	return v4, v6
}
