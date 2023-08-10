package iptables

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	utiliptables "k8s.io/kubernetes/pkg/util/iptables"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo"
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
		c = NewController(true, true)
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
	})

	ginkgo.Context("Ensure rules", func() {
		ruleArg := RuleArg{[]string{"-s", "10.10.10.50", "-j", "MARK", "--set-mark", "1000"}}
		testRuleArgs := []RuleArg{ruleArg,
			{
				[]string{"-s", "10.10.10.5", "-j", "MARK", "--set-mark", "2000"},
			},
		}
		differentRuleArg := RuleArg{[]string{"-s", "10.10.10.50", "-j", "SNAT", "--to", "11.10.10.60"}}

		//TODO: delete does nothing .. need way to detect if rules are present in iptables consider buffer read
		ginkgo.It("ensure rules exist", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				return c.EnsureRules(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4, testRuleArgs)
			})).Should(gomega.Succeed())
			// it's not easy to look up a rule with the existing API so if delete rule succeeds, we know iptables manager must have set the rule
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteRule(utiliptables.TableNAT, testChainName, testRuleArgs[0].Args...)
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
		})
		ginkgo.It("ensure rules recovers following manual removal of one of the two rules", func() {
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				return c.EnsureRules(utiliptables.TableNAT, testChainName, utiliptables.ProtocolIPv4, testRuleArgs)
			})).Should(gomega.Succeed())

			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteRule(utiliptables.TableNAT, testChainName, testRuleArgs[0].Args...)
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
			gomega.Eventually(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteRule(utiliptables.TableNAT, testChainName, testRuleArgs[0].Args...)
			})).WithTimeout(oneSecTimeout).Should(gomega.Succeed())
		})
		ginkgo.It("doesn't remove rules which we don't manage (doesn't own chain)", func() {
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
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				return c.iptV4.DeleteRule(utiliptables.TableNAT, testChainName, differentRuleArg.Args...)
			})).Should(gomega.Succeed())
		})
		ginkgo.It("does remove rules which we manage (own chain)", func() {
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
			time.Sleep(100 * time.Millisecond)
			gomega.Expect(testNS.Do(func(netNS ns.NetNS) error {
				ruleArgs, err := c.GetIPv4ChainRuleArgs(utiliptables.TableNAT, testChainName)
				if err != nil {
					return err
				}
				for _, ruleArg := range ruleArgs {
					if ruleArg.equal(differentRuleArg) {
						return fmt.Errorf("expect not to find rule %v", differentRuleArg.Args)
					}
				}
				return nil
			})).Should(gomega.Succeed())
		})
	})
})

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}
