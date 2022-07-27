package node

import (
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"

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

//
//
//func addIptRules(rules []iptRule) error {
//	var addErrors, err error
//	var ipt util.IPTablesHelper
//	for _, r := range rules {
//		klog.V(5).Infof("Adding rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
//			r.table, r.chain, strings.Join(r.args, " "), r.protocol)
//		if ipt, err = util.GetIPTablesHelper(r.protocol); err != nil {
//			addErrors = errors.Wrapf(addErrors,
//				"Failed to add iptables %s/%s rule %q: %v", r.table, r.chain, strings.Join(r.args, " "), err)
//			continue
//		}
//		if err = ipt.NewChain(r.table, r.chain); err != nil {
//			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v",
//				r.chain, r.table, err)
//		}
//		exists, err := ipt.Exists(r.table, r.chain, r.args...)
//		if !exists && err == nil {
//			err = ipt.Insert(r.table, r.chain, 1, r.args...)
//		}
//		if err != nil {
//			addErrors = errors.Wrapf(addErrors, "failed to add iptables %s/%s rule %q: %v",
//				r.table, r.chain, strings.Join(r.args, " "), err)
//		}
//	}
//	return addErrors
//}
//
//func delIptRules(rules []iptRule) error {
//	var delErrors, err error
//	var ipt util.IPTablesHelper
//	for _, r := range rules {
//		klog.V(5).Infof("Deleting rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
//			r.table, r.chain, strings.Join(r.args, " "), r.protocol)
//		if ipt, err = util.GetIPTablesHelper(r.protocol); err != nil {
//			delErrors = errors.Wrapf(delErrors,
//				"Failed to delete iptables %s/%s rule %q: %v", r.table, r.chain, strings.Join(r.args, " "), err)
//			continue
//		}
//		if exists, err := ipt.Exists(r.table, r.chain, r.args...); err == nil && exists {
//			err := ipt.Delete(r.table, r.chain, r.args...)
//			if err != nil {
//				delErrors = errors.Wrapf(delErrors, "failed to delete iptables %s/%s rule %q: %v",
//					r.table, r.chain, strings.Join(r.args, " "), err)
//			}
//		}
//	}
//	return delErrors
//}

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
