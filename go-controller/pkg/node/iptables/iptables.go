package iptables

import (
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// Rule represents an iptables rule.
type Rule struct {
	Table    string
	Chain    string
	Args     []string
	Protocol iptables.Protocol
}

// AddRules adds the given rules to iptables.
func AddRules(rules []Rule, append bool) error {
	addErrors := errors.New("")
	var err error
	var ipt util.IPTablesHelper
	var exists bool
	for _, r := range rules {
		klog.V(5).Infof("Adding rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
			r.Table, r.Chain, strings.Join(r.Args, " "), r.Protocol)
		if ipt, err = util.GetIPTablesHelper(r.Protocol); err != nil {
			addErrors = errors.Wrapf(addErrors,
				"Failed to add iptables %s/%s rule %q: %v", r.Table, r.Chain, strings.Join(r.Args, " "), err)
			continue
		}
		if err = ipt.NewChain(r.Table, r.Chain); err != nil {
			klog.V(5).Infof("Chain: \"%s\" in table: \"%s\" already exists, skipping creation: %v",
				r.Chain, r.Table, err)
		}
		exists, err = ipt.Exists(r.Table, r.Chain, r.Args...)
		if !exists && err == nil {
			if append {
				err = ipt.Append(r.Table, r.Chain, r.Args...)
			} else {
				err = ipt.Insert(r.Table, r.Chain, 1, r.Args...)
			}
		}
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "failed to add iptables %s/%s rule %q: %v",
				r.Table, r.Chain, strings.Join(r.Args, " "), err)
		}
	}
	if addErrors.Error() == "" {
		addErrors = nil
	}
	return addErrors
}

// DelRules deletes the given rules from iptables.
func DelRules(rules []Rule) error {
	delErrors := errors.New("")
	var err error
	var ipt util.IPTablesHelper
	for _, r := range rules {
		klog.V(5).Infof("Deleting rule in table: %s, chain: %s with args: \"%s\" for protocol: %v ",
			r.Table, r.Chain, strings.Join(r.Args, " "), r.Protocol)
		if ipt, err = util.GetIPTablesHelper(r.Protocol); err != nil {
			delErrors = errors.Wrapf(delErrors,
				"Failed to delete iptables %s/%s rule %q: %v", r.Table, r.Chain, strings.Join(r.Args, " "), err)
			continue
		}
		if exists, err := ipt.Exists(r.Table, r.Chain, r.Args...); err == nil && exists {
			err := ipt.Delete(r.Table, r.Chain, r.Args...)
			if err != nil {
				delErrors = errors.Wrapf(delErrors, "failed to delete iptables %s/%s rule %q: %v",
					r.Table, r.Chain, strings.Join(r.Args, " "), err)
			}
		}
	}
	if delErrors.Error() == "" {
		delErrors = nil
	}
	return delErrors
}
