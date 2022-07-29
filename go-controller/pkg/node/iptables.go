package node

import (
	"regexp"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type iptRule struct {
	table    string
	chain    string
	args     []string
	protocol iptables.Protocol
}

// IsEqual checks if 2 rules are equal. This func is useful to check if a
// certain rule is already provisioned.
func (r iptRule) IsEqual(rule string) bool {

	leftBucket := sets.NewString(parseRule(rule)...)
	rightBucket := sets.NewString(r.args...)
	return leftBucket.Equal(rightBucket)
}

func shouldSkipRule(ruleSpec ...string) bool {
	return len(ruleSpec) == 0
}

func parseRule(rule string) []string {
	re := regexp.MustCompile(`"[^"]+"`)

	ruleCopy := rule
	var params []string
	quotedParams := re.FindAllString(rule, -1)
	for _, s := range quotedParams {
		ruleCopy = strings.Replace(ruleCopy, s, "", -1)
		params = append(params, s[1:len(s)-1])
	}

	space := regexp.MustCompile(`\s+`)
	ruleCopy = space.ReplaceAllString(ruleCopy, " ")

	return append(ruleSpec(ruleCopy), params...)
}

func ruleSpec(rule string) []string {
	return strings.Split(rule, " ")[2:]
}

func addIptRules(rules []iptRule) error {
	var addErrors error
	for _, r := range rules {
		klog.V(5).Infof("Adding rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ", r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		if err := ipt.NewChain(r.table, r.chain); err != nil {
			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v", r.chain, r.table, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Insert(r.table, r.chain, 1, r.args...)
		}
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}
	return addErrors
}

// ensureIptRules adds the rules passed to it *in the order* defined in the
// slice - i.e. unlike it `addIptRules` it *appends* the rules to the chain
// instead of inserting at the beginning.
func ensureIptRules(rules []iptRule) error {
	var addErrors error
	for _, r := range rules {
		klog.V(5).Infof("Appending rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ", r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		if err := ipt.NewChain(r.table, r.chain); err != nil {
			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v", r.chain, r.table, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Append(r.table, r.chain, r.args...)
		}
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
		klog.V(5).Infof("Rule \"%s\" *already* found in table: %s, chain: %s for protocol: %v. Skipped adding it.", strings.Join(r.args, " "), r.table, r.chain, r.protocol)
	}
	return addErrors
}

func delIptRules(rules []iptRule) error {
	var delErrors error
	for _, r := range rules {
		klog.V(5).Infof("Deleting rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ", r.table, r.chain, strings.Join(r.args, " "), r.protocol)
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		if exists, err := ipt.Exists(r.table, r.chain, r.args...); err == nil && exists {
			err := ipt.Delete(r.table, r.chain, r.args...)
			if err != nil {
				delErrors = errors.Wrapf(delErrors, "failed to delete iptables %s/%s rule %q: %v",
					r.table, r.chain, strings.Join(r.args, " "), err)
			}
		}
	}
	return delErrors
}

func clusterIPTablesProtocols() []iptables.Protocol {
	var protocols []iptables.Protocol
	if config.IPv4Mode {
		protocols = append(protocols, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		protocols = append(protocols, iptables.ProtocolIPv6)
	}
	return protocols
}

// getIPTablesProtocol returns the IPTables protocol matching the protocol (v4/v6) of provided IP string
func getIPTablesProtocol(ip string) iptables.Protocol {
	if utilnet.IsIPv6String(ip) {
		return iptables.ProtocolIPv6
	}
	return iptables.ProtocolIPv4
}
