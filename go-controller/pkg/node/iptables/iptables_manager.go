package iptables

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/util/iptables"
	kexec "k8s.io/utils/exec"
)

// rulesIndex structure is used as a golang map key to point to a set of IPTable rules. It holds all the necessary info
// to retrieve IPTable rules
type rulesIndex struct {
	Table iptables.Table
	Chain iptables.Chain
	Proto iptables.Protocol
}

// rules represents one or more rules within a chain of a specific IP version
type rules struct {
	// exclusive represents whether the set of rules specified in ruleArgs are the exclusive set of rules allowed in a chain or not.
	// if exclusive is true, only the ruleArgs are allowed in a chain. If exclusive is false, other rules are allowed to co-exist.
	exclusive bool
	ruleArgs  []RuleArg
}

func (r rules) has(candidateRuleArg RuleArg) bool {
	for _, ruleArg := range r.ruleArgs {
		if ruleArg.equal(candidateRuleArg) {
			return true
		}
	}
	return false
}

func newRules() rules {
	return rules{ruleArgs: make([]RuleArg, 0)}
}

// RuleArg represents a single iptables rule entry
type RuleArg struct {
	Args []string
}

func (r RuleArg) equal(r2 RuleArg) bool {
	return reflect.DeepEqual(r, r2)
}

// Controller manages iptables for clients
type Controller struct {
	mu    *sync.Mutex // used to sync interaction with iptables or rules map
	store map[rulesIndex]rules
	iptV4 iptables.Interface
	iptV6 iptables.Interface
}

// NewController creates a controller to manage chains and rules. Provides functionality to "own" a chain which
// allows consumers to ensure only the rules submitted to the controller persist and unmanaged rules are removed.
// If a chain is unowned, then only the rules that are submitted persist.
func NewController() *Controller {
	return &Controller{
		store: make(map[rulesIndex]rules, 0),
		mu:    &sync.Mutex{},
		iptV4: iptables.New(kexec.New(), iptables.ProtocolIPv4),
		iptV6: iptables.New(kexec.New(), iptables.ProtocolIPv6),
	}
}

func (c *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	var err error
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			if err = c.reconcile(); err != nil {
				klog.Errorf("IPTables manager failed to reconcile (will be retried in %s): %v", syncPeriod.String(), err)
			}
			c.mu.Unlock()
		}
	}
}

// OwnChain ensures this chain exists and any rules within it this component exclusively owns. Any rules that we do not
// manage for this chain will be removed.
func (c *Controller) OwnChain(table iptables.Table, chain iptables.Chain, proto iptables.Protocol) error {
	klog.Infof("IPTables manager: own chain: table %s, chain %s, protocol %s", table, chain, proto)
	c.mu.Lock()
	defer c.mu.Unlock()
	ruleIndex := rulesIndex{
		Table: table,
		Chain: chain,
		Proto: proto,
	}
	rules, found := c.store[ruleIndex]
	if !found {
		rules = newRules()
		rules.exclusive = true
	}
	c.store[ruleIndex] = rules
	return c.reconcile()
}

// DeleteRule deletes an iptable rule
func (c *Controller) DeleteRule(table iptables.Table, chain iptables.Chain, proto iptables.Protocol, ruleArg RuleArg) error {
	klog.Infof("IPTables manager: delete rule - table %s, chain %s, protocol %s, rule %v", table, chain, proto, ruleArg)
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if proto == iptables.ProtocolIPv4 {
		err = execIPTablesWithRetry(func() error {
			err = c.iptV4.DeleteRule(table, chain, ruleArg.Args...)
			if err != nil {
				klog.Errorf("IPTables Manager: err while deleting rule %v chain %v", ruleArg.Args, err)
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to delete IPv4 rule %v on table %s and chain %s: %v", ruleArg.Args, table, chain, err)
		}
	} else {
		err = execIPTablesWithRetry(func() error {
			err = c.iptV6.DeleteRule(table, chain, ruleArg.Args...)
			if err != nil {
				klog.Errorf("IPTables Manager: err while deleting rule %v chain %v", ruleArg.Args, err)
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to IPv6 delete rule %v on table %s and chain %s: %v", ruleArg.Args, table, chain, err)
		}
	}
	ruleIndex := rulesIndex{
		Table: table,
		Chain: chain,
		Proto: proto,
	}
	savedRules, alreadyExists := c.store[ruleIndex]
	if !alreadyExists {
		return nil
	}
	tempRules := newRules()
	tempRules.exclusive = savedRules.exclusive
	for _, existingRuleArg := range savedRules.ruleArgs {
		if !existingRuleArg.equal(ruleArg) {
			tempRules.ruleArgs = append(tempRules.ruleArgs, existingRuleArg)
		}
	}
	c.store[ruleIndex] = tempRules
	return nil
}

// EnsureRule adds an iptable rule that will persist until deleted
func (c *Controller) EnsureRule(table iptables.Table, chain iptables.Chain, proto iptables.Protocol, ruleArg RuleArg) error {
	klog.Infof("IPTables manager: ensure rule - table %s, chain %s, protocol %s, rule: %v", table, chain, proto, ruleArg)
	c.mu.Lock()
	defer c.mu.Unlock()
	ruleIndex := rulesIndex{
		Table: table,
		Chain: chain,
		Proto: proto,
	}
	existingRuleArgs, exists := c.store[ruleIndex]
	if !exists {
		rules := newRules()
		rules.ruleArgs = []RuleArg{ruleArg}
		c.store[ruleIndex] = rules
	} else {
		exists = false
		for _, existingRuleArg := range existingRuleArgs.ruleArgs {
			if existingRuleArg.equal(ruleArg) {
				exists = true
				break
			}
		}
		if !exists {
			existingRuleArgs.ruleArgs = append(existingRuleArgs.ruleArgs, ruleArg)
		}
		c.store[ruleIndex] = existingRuleArgs
	}
	return c.reconcile()
}

func (c *Controller) GetChainRuleArgs(table iptables.Table, chain iptables.Chain, proto iptables.Protocol) ([]RuleArg, error) {
	if proto == iptables.ProtocolIPv4 {
		return c.GetIPv4ChainRuleArgs(table, chain)
	}
	return c.GetIPv6ChainRuleArgs(table, chain)
}

// GetIPv4ChainRuleArgs returns IPv4 RuleArgs
func (c *Controller) GetIPv4ChainRuleArgs(table iptables.Table, chain iptables.Chain) ([]RuleArg, error) {
	return getChainRuleArgs(c.iptV4, table, chain)
}

// GetIPv6ChainRuleArgs returns IPv6 RuleArgs
func (c *Controller) GetIPv6ChainRuleArgs(table iptables.Table, chain iptables.Chain) ([]RuleArg, error) {
	return getChainRuleArgs(c.iptV6, table, chain)
}

func getChainRuleArgs(ipt iptables.Interface, table iptables.Table, chain iptables.Chain) ([]RuleArg, error) {
	buf := bytes.NewBuffer(nil)
	err := execIPTablesWithRetry(func() error {
		if err := ipt.SaveInto(table, buf); err != nil {
			return fmt.Errorf("failed to retrieve iptables table %s: %v: %s", table, err, buf)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rules := make([]RuleArg, 0)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, fmt.Sprintf("-A %s", string(chain))) {
			rules = append(rules, RuleArg{
				// cleave off the -A ${chain_name}
				Args: strings.Split(line, " ")[2:],
			})
		}
	}
	return rules, nil
}

func processRules(ipt iptables.Interface, wantedChainRules map[rulesIndex]rules, existingChainRules map[rulesIndex]rules) error {
	var err error
	// cleanup any rules we see that we do not expect if the chain is 'owned'
	for rulesIndex, existingRules := range existingChainRules {
		if rulesIndex.Proto != ipt.Protocol() {
			continue
		}
		wantedRules, found := wantedChainRules[rulesIndex]
		// owned chains means we exclusively control all rules in the chain and therefore remove any rules which we do not want
		if found && wantedRules.exclusive {
			for _, existingRuleArg := range existingRules.ruleArgs {
				found = false
				for _, wantedRuleArg := range wantedRules.ruleArgs {
					if wantedRuleArg.equal(existingRuleArg) {
						found = true
						break
					}
				}
				if !found {
					err = execIPTablesWithRetry(func() error {
						if err = ipt.DeleteRule(rulesIndex.Table, rulesIndex.Chain, existingRuleArg.Args...); err != nil {
							return fmt.Errorf("failed to delete stale rule (%s) in table %s and chain %s: %v",
								strings.Join(existingRuleArg.Args, " "), rulesIndex.Table, rulesIndex.Chain, err)
						}
						return nil
					})
					if err != nil {
						klog.Errorf("IPTables manager: failed to process iptables rule: %v", err)
					}
				}
			}
		}
	}
	// add the rules we want that do not exist
	for rulesIndex, wantedRules := range wantedChainRules {
		if rulesIndex.Proto != ipt.Protocol() {
			continue
		}
		for _, wantedRuleArg := range wantedRules.ruleArgs {
			if existingChainRules[rulesIndex].has(wantedRuleArg) {
				continue
			}
			err = execIPTablesWithRetry(func() error {
				_, err = ipt.EnsureRule(iptables.Prepend, rulesIndex.Table, rulesIndex.Chain, wantedRuleArg.Args...)
				if err != nil {
					return fmt.Errorf("failed to ensure rule (%v) in chain %s and table %s: %v", wantedRuleArg.Args,
						rulesIndex.Chain, rulesIndex.Table, err)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// reconcile configures IPTables to ensure the correct chains and rules.
// CPU starvation or iptables lock held by an external entity may cause this function to take some time to execute.
// callers must hold the lock for mutex mu.
func (c *Controller) reconcile() error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Reconciling IPTables rules took %v", time.Since(start))
	}()

	existingChainRules := make(map[rulesIndex]rules)
	// Gather existing rules from tables and chains that we care about
	for ruleIndex := range c.store {
		if err := ensureChainExistsWithRetry(getIPTableClient(c.iptV4, c.iptV6, ruleIndex.Proto), ruleIndex.Table, ruleIndex.Chain); err != nil {
			return fmt.Errorf("failed to ensure chain %s: %v", ruleIndex.Chain, err)
		}
		ruleArgs, err := c.GetChainRuleArgs(ruleIndex.Table, ruleIndex.Chain, ruleIndex.Proto)
		if err != nil {
			return fmt.Errorf("failed to get %s rules for chain %s in table %s: %v", ruleIndex.Proto, ruleIndex.Chain, ruleIndex.Table, err)
		}
		existingChainRules[ruleIndex] = rules{ruleArgs: ruleArgs}
	}
	if err := processRules(c.iptV4, c.store, existingChainRules); err != nil {
		return fmt.Errorf("failed to process IPv4 rules: %v", err)
	}
	if err := processRules(c.iptV6, c.store, existingChainRules); err != nil {
		return fmt.Errorf("failed to process IPv6 rules: %v", err)
	}
	return nil
}

// iptablesBackoff will retry 10 times over a period of 13 seconds
var iptablesBackoff = wait.Backoff{
	Duration: 500 * time.Millisecond,
	Factor:   1.25,
	Steps:    10,
}

// execIPTablesWithRetry allows a simple way to retry IpTables commands if they fail the first time
func execIPTablesWithRetry(f func() error) error {
	return wait.ExponentialBackoff(iptablesBackoff, func() (bool, error) {
		if err := f(); err != nil {
			if isResourceError(err) {
				klog.V(5).Infof("Call to iptables failed with transient failure: %v", err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

const iptablesStatusResourceProblem = 4

// isResourceError returns true if the error indicates that iptables ran into a "resource
// problem" and was unable to attempt the request. In particular, this will be true if it
// times out trying to get the iptables lock.
func isResourceError(err error) bool {
	if ee, isExitError := err.(kexec.ExitError); isExitError {
		return ee.ExitStatus() == iptablesStatusResourceProblem
	}
	return false
}

func ensureChainExistsWithRetry(ipt iptables.Interface, table iptables.Table, chain iptables.Chain) error {
	return execIPTablesWithRetry(func() error {
		_, err := ipt.EnsureChain(table, chain)
		return err
	})
}

func getIPTableClient(ipv4, ipv6 iptables.Interface, proto iptables.Protocol) iptables.Interface {
	if proto == iptables.ProtocolIPv4 {
		return ipv4
	}
	return ipv6
}
