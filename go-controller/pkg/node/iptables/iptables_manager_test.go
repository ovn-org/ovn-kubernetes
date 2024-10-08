package iptables

import (
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	utiliptables "k8s.io/kubernetes/pkg/util/iptables"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	testChainName = "testchainiptablemanagertest"
	oneSecTimeout = 1 * time.Second
)

var _ = ginkgo.Describe("IPTables Manager", func() {
	var stopCh chan struct{}
	var wg *sync.WaitGroup
	var testNS ns.NetNS
	var c *Controller

	ginkgo.BeforeEach(func() {
		defer ginkgo.GinkgoRecover()
		if os.Getenv("NOROOT") == "TRUE" {
			ginkgo.Skip("Test requires root privileges")
		}
		if !commandExists("iptables") {
			ginkgo.Skip("Test requires iptables tools to be available in PATH")
		}
		var err error
		runtime.LockOSThread()
		testNS, err = testutils.NewNS()
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		wg.Add(1)
		c = NewController()
		go testNS.Do(func(netNS ns.NetNS) error {
			c.Run(stopCh, 50*time.Millisecond)
			wg.Done()
			return nil
		})
	})

	ginkgo.AfterEach(func() {
		defer runtime.UnlockOSThread()
		close(stopCh)
		wg.Wait()
		gomega.Expect(testNS.Close()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed())
	})

	ginkgo.Context("Own chain", func() {
		ginkgo.It("ensure chain exist", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				return c.OwnChain(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4)
			})).ShouldNot(gomega.HaveOccurred())

			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				_, err := c.iptV4.ChainExists(utiliptables.TableNAT, testChainName)
				return err
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
		})

		ginkgo.It("ensure chain recovers following manual removal", func() {
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				if err := c.OwnChain(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4); err != nil {
					return err
				}
				_, err := c.iptV4.ChainExists(utiliptables.TableNAT, testChainName)
				return err
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteChain(utiliptables.TableNAT, testChainName)
			})).Should(gomega.Succeed())

			time.Sleep(100 * time.Millisecond)
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				_, err := c.iptV4.ChainExists(utiliptables.TableNAT, testChainName)
				return err
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
		})
		ginkgo.It("owning a chain create a chain", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				if err := c.OwnChain(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv6); err != nil {
					return err
				}
				return nil
			})).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return chainExists(testNS, c.iptV6, utiliptables.TableNAT, testChainName)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("Ensure IPv4 rules", func() {
		testRuleArgs := []RuleArg{
			{
				[]string{"-s", "192.168.1.2/32", "-o", "eth0", "-j", "SNAT", "--to-source", "1.1.1.1"},
			},
			{
				[]string{"-s", "192.168.1.3/32", "-o", "eth0", "-j", "SNAT", "--to-source", "1.1.1.2"},
			},
		}
		differentRuleArg := RuleArg{[]string{"-s", "192.168.1.4/32", "-j", "SNAT", "--to-source", "1.1.1.2"}}

		ginkgo.It("ensure rules exist", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				for _, rule := range testRuleArgs {
					err := c.EnsureRule(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4, rule)
					if err != nil {
						return err
					}
				}
				return nil
			})).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return containsRuleArgs(testNS, c.iptV4, utiliptables.TableNAT, testChainName, testRuleArgs)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
		ginkgo.It("ensure rules recovers following manual removal of one of the two rules", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				for _, rule := range testRuleArgs {
					err := c.EnsureRule(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4, rule)
					if err != nil {
						return err
					}
				}
				return nil
			})).Should(gomega.Succeed())
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteRule(utiliptables.TableNAT, testChainName, testRuleArgs[0].Args...)
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return containsRuleArgs(testNS, c.iptV4, utiliptables.TableNAT, testChainName, testRuleArgs)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
		ginkgo.It("doesn't remove unknown rules in un-owned chain", func() {
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				if _, err := c.iptV4.EnsureChain(utiliptables.TableNAT, testChainName); err != nil {
					return err
				}
				if _, err := c.iptV4.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, testChainName, differentRuleArg.Args...); err != nil {
					return err
				}
				return nil
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			time.Sleep(100 * time.Millisecond)
			gomega.Eventually(func() bool {
				return containsRuleArg(testNS, c.iptV4, utiliptables.TableNAT, testChainName, differentRuleArg)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
		ginkgo.It("does remove unknown rules in owned chain", func() {
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				if err := c.OwnChain(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4); err != nil {
					return err
				}
				if _, err := c.iptV4.EnsureChain(utiliptables.TableNAT, testChainName); err != nil {
					return err
				}
				if _, err := c.iptV4.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, testChainName, differentRuleArg.Args...); err != nil {
					return err
				}
				return nil
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return containsRuleArg(testNS, c.iptV4, utiliptables.TableNAT, testChainName, differentRuleArg)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeFalse())
		})
	})

	ginkgo.Context("Ensure IPv6 rules", func() {
		testRuleArgs := []RuleArg{
			{
				[]string{"-s", "2001:0:0:2::1/128", "-o", "eth0", "-j", "SNAT", "--to-source", "2001::1"},
			},
			{
				[]string{"-s", "2001:0:0:2::2/128", "-o", "eth0", "-j", "SNAT", "--to-source", "2001::1"},
			},
		}
		differentRuleArg := RuleArg{[]string{"-s", "2001:0:0:2::3/128", "-o", "eth0", "-j", "SNAT", "--to-source", "2001::1"}}

		ginkgo.It("ensure rules exist", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				for _, rule := range testRuleArgs {
					err := c.EnsureRule(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv6, rule)
					if err != nil {
						return err
					}
				}
				return nil
			})).Should(gomega.Succeed())
			// it's not easy to look up a rule with the existing API so if delete rule succeeds, we know iptables manager must have set the rule
			gomega.Eventually(func() bool {
				return containsRuleArgs(testNS, c.iptV6, utiliptables.TableNAT, testChainName, testRuleArgs)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})

		ginkgo.It("ensure rules recovers following manual removal of one of the two rules", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				for _, rule := range testRuleArgs {
					err := c.EnsureRule(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv6, rule)
					if err != nil {
						return err
					}
				}
				return nil
			})).Should(gomega.Succeed())
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV6.DeleteRule(utiliptables.TableNAT, testChainName, testRuleArgs[0].Args...)
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return containsRuleArgs(testNS, c.iptV6, utiliptables.TableNAT, testChainName, testRuleArgs)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
		ginkgo.It("doesn't remove unknown rules in un-owned chain", func() {
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				if _, err := c.iptV6.EnsureChain(utiliptables.TableNAT, testChainName); err != nil {
					return err
				}
				if _, err := c.iptV6.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, testChainName, differentRuleArg.Args...); err != nil {
					return err
				}
				return nil
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			time.Sleep(100 * time.Millisecond)
			gomega.Eventually(func() bool {
				return containsRuleArg(testNS, c.iptV6, utiliptables.TableNAT, testChainName, differentRuleArg)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeTrue())
		})
		ginkgo.It("removes unknown rules in owned chain", func() {
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				if err := c.OwnChain(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv6); err != nil {
					return err
				}
				if _, err := c.iptV6.EnsureChain(utiliptables.TableNAT, testChainName); err != nil {
					return err
				}
				if _, err := c.iptV6.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT, testChainName, differentRuleArg.Args...); err != nil {
					return err
				}
				return nil
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Eventually(func() bool {
				return containsRuleArg(testNS, c.iptV6, utiliptables.TableNAT, testChainName, differentRuleArg)
			}).WithTimeout(oneSecTimeout).Should(gomega.BeFalse())
		})
	})
})

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func chainExists(testNS ns.NetNS, ipt utiliptables.Interface, table utiliptables.Table, chain utiliptables.Chain) bool {
	var exists bool
	var err error
	err = testNS.Do(func(netNS ns.NetNS) error {
		exists, err = ipt.ChainExists(table, chain)
		return err
	})
	if err != nil {
		panic(err.Error())
	}
	return exists
}

func containsRuleArgs(testNS ns.NetNS, ipt utiliptables.Interface, table utiliptables.Table, chain utiliptables.Chain, ruleArgs []RuleArg) bool {
	if len(ruleArgs) == 0 {
		panic("no rule args specified")
	}
	for _, ruleArg := range ruleArgs {
		if !containsRuleArg(testNS, ipt, table, chain, ruleArg) {
			return false
		}
	}
	return true
}

func containsRuleArg(testNS ns.NetNS, ipt utiliptables.Interface, table utiliptables.Table, chain utiliptables.Chain, ruleArg RuleArg) bool {
	var found bool
	err := testNS.Do(func(netNS ns.NetNS) error {
		existingRuleArgs, err := getChainRuleArgs(ipt, table, chain)
		if err != nil {
			return err
		}
		for _, existingRuleArg := range existingRuleArgs {
			if existingRuleArg.equal(ruleArg) {
				found = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		panic(err.Error())
	}
	return found
}
