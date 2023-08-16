package egressip

import (
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

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	ovnconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovniptables "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/util/iptables"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	kexec "k8s.io/utils/exec"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

// testInfConfig contains information to setup a linux interface and its associated address
type testInfConfig struct {
	name string
	addr string
}

// testNode holds all the information needed to configure a test node which maps to one Node object
type testNode struct {
	infs []testInfConfig
}

// testPodConfig holds all the information needed to validate a config is applied for a pod
type testPodConfig struct {
	name        string // pod name
	ipTableRule ovniptables.RuleArg
	ipRule      *netlink.Rule
}

// testConfig holds all the information needs to validate a single EIP IP is correctly applied
type testConfig struct {
	route      *netlink.Route
	addr       *netlink.Addr
	inf        string
	podConfigs []testPodConfig
}

// testEIPConfig contains all the information needed to validate an EIP. Max one EIP IP maybe configured for a given
// EgressIP
type testEIPConfig struct {
	eIP egressipv1.EgressIP
	testConfig
}

const (
	namespace1               = "ns1"
	namespace2               = "ns2"
	namespace3               = "ns3"
	namespace4               = "ns4"
	node1Name                = "node1"
	node2Name                = "node2"
	egressIP1Name            = "egressip-1"
	egressIP2Name            = "egressip-2"
	egressIP1IP              = "5.5.5.50"
	egressIP2IP              = "5.5.10.55"
	oneSec                   = time.Second
	dummyLink1Name           = "dummy1"
	dummyLink2Name           = "dummy2"
	dummyLink3Name           = "dummy3"
	dummyLink4Name           = "dummy4"
	dummy1IPv4CIDR           = "5.5.5.10/24"
	dummy1IPv4CIDRNetwork    = "5.5.5.0/24"
	dummy2IPv4CIDR           = "5.5.10.15/24"
	dummy2IPv4CIDRNetwork    = "5.5.10.0/24"
	dummy3IPv4CIDR           = "8.8.10.3/16"
	dummy4IPv4CIDR           = "9.8.10.3/16"
	pod1Name                 = "testpod1"
	pod1IPv4                 = "192.168.100.2"
	pod1IPv4CIDR             = "192.168.100.2/32"
	pod2Name                 = "testpod2"
	pod2IPv4                 = "192.168.100.7"
	pod2IPv4CIDR             = "192.168.100.7/32"
	pod3Name                 = "testpod3"
	pod3IPv4                 = "192.168.100.9"
	pod3IPv4CIDR             = "192.168.100.9/32"
	pod4Name                 = "testpod4"
	pod4IPv4                 = "192.168.100.67"
	pod4IPv4CIDR             = "192.168.100.67/32"
	node1OVNManagedNetworkV4 = "11.11.0.0/16"
)

var (
	egressPodLabel  = map[string]string{"egress": "needed"}
	namespace1Label = map[string]string{"prod1": ""}
	namespace2Label = map[string]string{"prod2": ""}
)

type cleanupFn func() error

func setupFakeNode(node testNode, nodeInitialConfigs []testConfig) (ns.NetNS, error) {
	testNS, err := testutils.NewNS()
	if err != nil {
		return nil, fmt.Errorf("failed to create new network namespace: %v", err)
	}
	err = testNS.Do(func(netNS ns.NetNS) error {
		// adding links
		for _, i := range node.infs {
			if i.name == "" || i.addr == "" {
				continue
			}
			if err = addInfAndAddr(i.name, i.addr); err != nil {
				return fmt.Errorf("failed to add interface %s with address %s: %v", i.name, i.addr, err)
			}
		}
		ipTableV4Client := iptables.New(kexec.New(), iptables.ProtocolIPv4)
		for _, nodeConfig := range nodeInitialConfigs {
			if nodeConfig.route != nil {
				if err = netlink.RouteAdd(nodeConfig.route); err != nil {
					return fmt.Errorf("failed to add route (%s): %v", nodeConfig.route.String(), err)
				}
			}
			if nodeConfig.addr != nil {
				link, err := netlink.LinkByName(nodeConfig.inf)
				if err != nil {
					return fmt.Errorf("failed to link %q and therefore failed to add address (%s) to link: %v",
						nodeConfig.inf, nodeConfig.addr.String(), err)
				}
				if err = netlink.AddrAdd(link, nodeConfig.addr); err != nil {
					return fmt.Errorf("failed to add address (%s) to link %s: %v", nodeConfig.addr.String(), nodeConfig.inf, err)
				}
			}
			for _, podConfig := range nodeConfig.podConfigs {
				if len(podConfig.ipTableRule.Args) == 0 || podConfig.ipRule == nil {
					continue
				}
				_, err = ipTableV4Client.EnsureRule(iptables.Prepend, iptables.TableNAT, iptChainName, podConfig.ipTableRule.Args...)
				if err != nil {
					return fmt.Errorf("failed to ensure IPTables rule (%v): %v", podConfig.ipTableRule.Args, err)
				}
				if err = netlink.RuleAdd(podConfig.ipRule); err != nil {
					return fmt.Errorf("failed to add IP rule (%v): %v", podConfig.ipRule.String(), err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add node config: %v", err)
	}
	return testNS, nil
}

func setupFakeTestNode(node testNode, nodeInitialState []testConfig) (ns.NetNS, cleanupFn, error) {
	testNS, err := setupFakeNode(node, nodeInitialState)
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

func initController(namespaces []corev1.Namespace, pods []corev1.Pod, egressIPs []egressipv1.EgressIP, node testNode, v4, v6 bool) (*Controller, error) {

	kubeClient := fake.NewSimpleClientset(&corev1.NodeList{Items: []corev1.Node{getNodeObj(node)}},
		&corev1.NamespaceList{Items: namespaces}, &corev1.PodList{Items: pods})
	egressIPClient := egressipfake.NewSimpleClientset(&egressipv1.EgressIPList{Items: egressIPs})
	ovnNodeClient := &util.OVNNodeClientset{KubeClient: kubeClient, EgressIPClient: egressIPClient}
	rm := routemanager.NewController()
	ovnconfig.OVNKubernetesFeature.EnableEgressIP = true
	watchFactory, err := factory.NewNodeWatchFactory(ovnNodeClient, node1Name)
	if err != nil {
		return nil, err
	}
	if err := watchFactory.Start(); err != nil {
		return nil, err
	}
	c, err := NewController(watchFactory.EgressIPInformer(), watchFactory.NodeInformer(), watchFactory.NamespaceInformer(),
		watchFactory.PodCoreInformer(), rm, v4, v6, node1Name)
	if err != nil {
		return nil, err
	}
	_, err = c.namespaceInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return nil, err
	}
	_, err = c.podInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return nil, err
	}
	_, err = c.eIPInformer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onEIPAdd,
			UpdateFunc: c.onEIPUpdate,
			DeleteFunc: c.onEIPDelete,
		}))
	if err != nil {
		return nil, err
	}
	return c, nil
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
			if !util.WaitForNamedCacheSyncWithTimeout(resourceName, stopCh, syncFn) {
				gomega.PanicWith(fmt.Sprintf("timed out waiting for %q caches to sync", resourceName))
			}
		}(se.resourceName, se.syncFn)
	}

	wg := &sync.WaitGroup{}
	// we do not call start for our controller because the newly created goroutines will not be set to the correct network namespace,
	// so we invoke them manually here and call reconcile manually
	// normally executed during Run but we call it manually here because run spawns a go routine that we cannot control its netns during test
	wg.Add(1)
	go testNS.Do(func(netNS ns.NetNS) error {
		c.linkManager.Run(stopCh, 10*time.Millisecond)
		wg.Done()
		return nil
	})
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
			if err = c.iptablesManager.EnsureRules(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv4, iptJumpRule); err != nil {
				return err
			}
		}
		if c.v6 {
			if err = c.iptablesManager.OwnChain(utiliptables.TableNAT, iptChainName, utiliptables.ProtocolIPv6); err != nil {
				return err
			}
			if err = c.iptablesManager.EnsureRules(utiliptables.TableNAT, utiliptables.ChainPostrouting, utiliptables.ProtocolIPv6, iptJumpRule); err != nil {
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

// FIXME(mk) - Within GH VM, if I need to create a new NetNs. I see the following error:
// "failed to create new network namespace: mount --make-rshared /run/user/1001/netns failed: "operation not permitted""
var _ = table.XDescribeTable("EgressIP selectors",
	func(expectedEIPConfigs []testEIPConfig, pods []corev1.Pod, namespaces []corev1.Namespace, nodeConfig testNode) {
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
			egressIPList = append(egressIPList, expectedEIPConfig.eIP)
		}
		ginkgo.By("setting up test environment")
		testNS, cleanupNodeFn, err := setupFakeTestNode(nodeConfig, nil)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		c, err := initController(namespaces, pods, egressIPList, nodeConfig, true, false) //TODO: test for IPV6
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		cleanupControllerFn, err := runController(testNS, c)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		ginkgo.By("verify expected IPTable rules match what was found")
		// Ensure only the iptables rules we expect are present on the chain
		gomega.Eventually(func() error {
			foundIPTRules, err := getIPTableRules(testNS, c.iptablesManager)
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
						if status.Network == "" || status.EgressIP == "" {
							continue
						}
						// FIXME: we assume all EIPs are hosted by non-OVN managed network. Skip OVN managed EIPs.
						link, err := netlink.LinkByName(expectedEIPConfig.inf)
						if err != nil {
							return err
						}
						filter, mask := filterRouteByLinkTable(link.Attrs().Index, getRouteTableID(link.Attrs().Index))
						existingRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, filter, mask)
						if err != nil {
							return fmt.Errorf("failed to list routes for link %q: %v", expectedEIPConfig.inf, err)
						}
						// ensure at least one route if there are any pods configured for this EIP
						if len(expectedEIPConfig.podConfigs) > 0 && len(existingRoutes) != 1 {
							return fmt.Errorf("expected exactly one route but found %d: %+v", len(existingRoutes), existingRoutes)
						}
						if !areRoutesEqual(expectedEIPConfig.route, &existingRoutes[0]) {
							return fmt.Errorf("expected routes to be equal but they are not: Route 1: %q\tRoute 2: %q",
								expectedEIPConfig.route.String(), existingRoutes[0].String())
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
						expectedRules = append(expectedRules, generateIPRule(ip, getLinkIndex(expectedEIPConfig.inf)))
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
						if len(foundRules) != len(expectedRules) {
							return fmt.Errorf("failed to the correct number of rules (expected %d, but got %d):\nexpected: %+v\nfound: %+v",
								len(expectedRules), len(foundRules), expectedRules, foundRules)
						}
						var found bool
						for _, foundRule := range foundRules {
							found = false
							for _, expectedRule := range expectedRules {
								if expectedRule.Src.IP.Equal(foundRule.Src.IP) {
									if expectedRule.Table == foundRule.Table {
										found = true
									}
								}
							}
							if !found {
								return fmt.Errorf("unexpected rule found: %v", foundRule)
							}
						}
					}

				}
				return nil
			})
		})
		gomega.Expect(cleanupControllerFn()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(cleanupNodeFn()).ShouldNot(gomega.HaveOccurred())
	},
	table.Entry("configures nothing when EIPs dont select anything",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		},
	),
	table.Entry("configures one EIP and one Pod",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink1Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink1Name,
					[]testPodConfig{
						{
							pod1Name,
							getExpectedIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod1IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel)},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		},
	),
	table.Entry("configures one EIP and multiple pods",
		// Test pod and namespace selection -
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink1Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink1Name,
					[]testPodConfig{
						{
							pod1Name,
							getExpectedIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod1IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
						{
							pod2Name,
							getExpectedIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod2IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
					},
				},
			},
		},
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, egressPodLabel),
			newPodWithLabels(namespace1, pod2Name, node1Name, pod2IPv4, egressPodLabel),
			newPodWithLabels(namespace1, pod3Name, node1Name, pod3IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label), newNamespaceWithLabels(namespace2, map[string]string{})},
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		},
	),
	table.Entry("configures one EIP and multiple namespaces and multiple pods",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink1Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink1Name,
					[]testPodConfig{
						{
							pod1Name,
							getExpectedIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod1IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
						{
							pod2Name,
							getExpectedIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod2IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
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
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		},
	),
	table.Entry("configures one EIP and multiple namespaces and multiple pods",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink1Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink1Name,
					[]testPodConfig{
						{pod1Name,
							getExpectedIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod1IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
						{
							pod2Name,
							getExpectedIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod2IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
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
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		},
	),
	table.Entry("configures multiple EIPs and multiple namespaces and multiple pods",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink1Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink1Name,
					[]testPodConfig{
						{
							pod1Name,
							getExpectedIPTableMasqRule(pod1IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod1IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
						{
							pod2Name,
							getExpectedIPTableMasqRule(pod2IPv4CIDR, dummyLink1Name, egressIP1IP),
							getExpectedRule(pod2IPv4, getRouteTableID(getLinkIndex(dummyLink1Name))),
						},
					},
				},
			},
			{
				newEgressIP(egressIP2Name, egressIP2IP, dummy2IPv4CIDRNetwork, node1Name, namespace2Label, egressPodLabel),
				testConfig{ // expected config for EIP
					getExpectedDefaultRoute(getLinkIndex(dummyLink2Name)),
					getExpectedLinkAddr(egressIP1IP),
					dummyLink2Name,
					[]testPodConfig{
						{
							pod3Name,
							getExpectedIPTableMasqRule(pod3IPv4CIDR, dummyLink2Name, egressIP2IP),
							getExpectedRule(pod3IPv4, getRouteTableID(getLinkIndex(dummyLink2Name))),
						},
						{
							pod4Name,
							getExpectedIPTableMasqRule(pod4IPv4CIDR, dummyLink2Name, egressIP2IP),
							getExpectedRule(pod4IPv4, getRouteTableID(getLinkIndex(dummyLink2Name))),
						},
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
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR},
				{dummyLink3Name, dummy3IPv4CIDR}, {dummyLink4Name, dummy4IPv4CIDR}},
		},
	),
)

var _ = table.XDescribeTable("repair node", func(expectedStateFollowingClean []testEIPConfig,
	nodeConfigsBeforeRepair []testConfig, pods []corev1.Pod, namespaces []corev1.Namespace, nodeConfig testNode) {
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
	egressIPList := make([]egressipv1.EgressIP, 0)
	for _, testEIPConfig := range expectedStateFollowingClean {
		egressIPList = append(egressIPList, testEIPConfig.eIP)
	}
	testNS, cleanupNodeFn, err := setupFakeTestNode(nodeConfig, nodeConfigsBeforeRepair)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	c, err := initController(namespaces, pods, egressIPList, nodeConfig, true, false)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	// for now, we expect it to always succeed
	err = testNS.Do(func(netNS ns.NetNS) error {
		return c.RepairNode()
	})
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	// check node
	ginkgo.By("ensure no stale IPTable rules")
	foundIPTRules, err := getIPTableRules(testNS, c.iptablesManager)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	var expectedIPTableRuleCount int
	for _, expectedConfig := range expectedStateFollowingClean {
		if len(expectedConfig.podConfigs) == 0 {
			continue
		}
		for _, podConfig := range expectedConfig.podConfigs {
			expectedIPTableRuleCount += 1
			gomega.Expect(isIPTableRuleFound(foundIPTRules, podConfig.ipTableRule)).Should(gomega.Succeed())
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
	// current test implementation ensures only one link address should be present
	for linkName, addresses := range linkAddresses {
		ginkgo.By(fmt.Sprintf("checking interface %s for stale address(es)", linkName))
		gomega.Expect(addresses).Should(gomega.HaveLen(1))
	}
	gomega.Expect(cleanupNodeFn()).ShouldNot(gomega.HaveOccurred())
}, table.Entry("should not fail when node is clean and nothing to apply",
	[]testEIPConfig{
		{
			newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
			testConfig{}, // no expected config
		},
	},
	[]testConfig{}, // // node state before repair - no initial config for the node
	[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
	[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
	testNode{
		[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
	}),
	table.Entry("should remove stale route",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{
					inf: dummyLink2Name,
				}, // no expected config
			},
		},
		[]testConfig{ // node state before repair
			{
				getExpectedDefaultRoute(getLinkIndex(dummyLink2Name)),
				nil,
				dummyLink2Name,
				[]testPodConfig{},
			},
		}, // no initial config for the node
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		}),
	// skip for now - test in dev
	table.XEntry("should remove stale address",
		[]testEIPConfig{
			{
				newEgressIP(egressIP1Name, egressIP1IP, dummy1IPv4CIDRNetwork, node1Name, namespace1Label, egressPodLabel),
				testConfig{
					inf: dummyLink2Name,
				}, // no expected config
			},
		},
		[]testConfig{ // node state before repair
			{
				nil,
				getExpectedLinkAddr(egressIP1IP),
				dummyLink2Name,
				[]testPodConfig{},
			},
		}, // no initial config for the node
		[]corev1.Pod{newPodWithLabels(namespace1, pod1Name, node1Name, pod1IPv4, map[string]string{})},
		[]corev1.Namespace{newNamespaceWithLabels(namespace1, namespace1Label)},
		testNode{
			[]testInfConfig{{dummyLink1Name, dummy1IPv4CIDR}, {dummyLink2Name, dummy2IPv4CIDR}},
		}))

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

func getNodeObj(testNode testNode) corev1.Node {
	hostAddrs := make([]string, 0)
	hostAddrs = append(hostAddrs, fmt.Sprintf("\"%s\"", node1OVNManagedNetworkV4))
	// we assume all extra interfaces and their addresses are valid for host addr
	for _, infs := range testNode.infs {
		hostAddrs = append(hostAddrs, fmt.Sprintf("\"%s\"", infs.addr))
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: node1Name,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
					node1OVNManagedNetworkV4, ""),
				"k8s.ovn.org/host-addresses": fmt.Sprintf("[%s]", strings.Join(hostAddrs, ",")),
			}},
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

func getIPTableRules(testNS ns.NetNS, iptablesManager *ovniptables.Controller) ([]ovniptables.RuleArg, error) {
	var foundIPTRules []ovniptables.RuleArg
	var err error
	err = testNS.Do(func(netNS ns.NetNS) error {
		foundIPTRules, err = iptablesManager.GetIPv4ChainRuleArgs(utiliptables.TableNAT, iptChainName)
		if err != nil {
			return err
		}
		return nil
	})
	return foundIPTRules, err
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

func newEgressIP(name, ip, network, node string, namespaceLabels, podLabels map[string]string) egressipv1.EgressIP {
	_, ipnet, err := net.ParseCIDR(network)
	if err != nil {
		panic(err.Error())
	}
	return egressipv1.EgressIP{
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
				ipnet.String(),
			}},
		},
	}
}

var index = 5

func addInfAndAddr(name string, address string) error {
	index += 1
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{
		Index:        getLinkIndex(name),
		MTU:          1500,
		Name:         name,
		HardwareAddr: util.IPAddrToHWAddr(net.ParseIP(address)),
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
	ip, ipNet, err := net.ParseCIDR(address)
	if err != nil {
		return err
	}
	ipNet.IP = ip
	addr := &netlink.Addr{IPNet: ipNet, LinkIndex: dummy.Index, Scope: int(netlink.SCOPE_UNIVERSE)}
	return netlink.AddrAdd(dummy, addr)
}

func getExpectedIPTableMasqRule(podIP, infName, snatIP string) ovniptables.RuleArg {
	return ovniptables.RuleArg{Args: []string{"-s", podIP, "-o", infName, "-j", "SNAT", "--to-source", snatIP}}
}

func getExpectedDefaultRoute(linkIndex int) *netlink.Route {
	// dst is nil because netlink represents a default route as nil
	return &netlink.Route{LinkIndex: linkIndex, Table: getRouteTableID(linkIndex), Dst: nil}
}

func getExpectedLinkAddr(ip string) *netlink.Addr {
	if !strings.Contains(ip, "/") {
		ip = ip + "/32"
	}
	addr, err := netlink.ParseAddr(ip)
	if err != nil {
		panic(fmt.Sprintf("failed to parse %q: %v", ip, err))
	}
	return addr
}

func areRoutesEqual(r1 *netlink.Route, r2 *netlink.Route) bool {
	if r1.String() == r2.String() {
		return true
	}
	return false
}

func getExpectedRule(podIP string, tableID int) *netlink.Rule {
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
