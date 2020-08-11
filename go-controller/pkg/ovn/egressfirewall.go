package ovn

import (
	"fmt"
	"net"
	"strings"

	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	defaultStartPriority           = 2000
	egressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	egressFirewallAddError         = "EgressFirewall Rules not correctly added"
	egressFirewallUpdateError      = "EgressFirewall Rules did not update correctly"
)

type egressFirewall struct {
	name        string
	namespace   string
	egressRules []*egressFirewallRule
}

type egressFirewallRule struct {
	id     int
	access egressfirewallapi.EgressFirewallRuleType
	ports  []egressfirewallapi.EgressFirewallPort
	to     destination
}

type destination struct {
	cidrSelector string
}

func newEgressFirewall(egressFirewallPolicy *egressfirewallapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        egressFirewallPolicy.Name,
		namespace:   egressFirewallPolicy.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

func newEgressFirewallRule(rawEgressFirewallRule egressfirewallapi.EgressFirewallRule, id int) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	_, _, err := net.ParseCIDR(rawEgressFirewallRule.To.CIDRSelector)
	if err != nil {
		return nil, err
	}
	efr.to.cidrSelector = rawEgressFirewallRule.To.CIDRSelector

	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

func (oc *Controller) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) []error {
	klog.Infof("Adding egress Firewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)
	nsInfo, err := oc.waitForNamespaceLocked(egressFirewall.Namespace)
	if err != nil {
		return []error{fmt.Errorf("failed to wait for namespace %s event (%v)",
			egressFirewall.Namespace, err)}
	}
	defer nsInfo.Unlock()

	if nsInfo.egressFirewallPolicy != nil {
		return []error{fmt.Errorf("error attempting to add egressFirewall %s to namespace %s when it already has an egressFirewall",
			egressFirewall.Name, egressFirewall.Namespace)}
	}

	ef := newEgressFirewall(egressFirewall)
	nsInfo.egressFirewallPolicy = ef
	var errList []error
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		//process Rules into egressFirewallRules for egressFirewall struct
		efr, err := newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			errList = append(errList, fmt.Errorf("error: cannot create EgressFirewall Rule for destination %s to namespace %s - %v",
				egressFirewallRule.To.CIDRSelector, egressFirewall.Namespace, err))
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if len(errList) > 0 {
		return errList
	}

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		return []error{fmt.Errorf("error unable to add egressfirewall %s, cannot list nodes: %s", egressFirewall.Name, err)}
	}
	var joinSwitches []string
	for _, node := range existingNodes.Items {
		joinSwitches = append(joinSwitches, joinSwitch(node.Name))
	}
	err = ef.addACLToJoinSwitch(joinSwitches, nsInfo.addressSet.GetIPv4HashName(), nsInfo.addressSet.GetIPv6HashName())
	if err != nil {
		errList = append(errList, err)
	}
	if len(errList) > 0 {
		return errList
	}

	return nil

}

func (oc *Controller) updateEgressFirewall(oldEgressFirewall, newEgressFirewall *egressfirewallapi.EgressFirewall) []error {
	errList := oc.deleteEgressFirewall(oldEgressFirewall)
	errList = append(errList, oc.addEgressFirewall(newEgressFirewall)...)
	return errList
}

func (oc *Controller) deleteEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall) []error {
	klog.Infof("Deleting egress Firewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	nsInfo := oc.getNamespaceLocked(egressFirewall.Namespace)
	if nsInfo != nil {
		// clear it so an error does not prevent future egressFirewalls
		nsInfo.egressFirewallPolicy = nil
		nsInfo.Unlock()
	}

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		return []error{fmt.Errorf("error deleting egressFirewall for namespace %s, cannot get nodes to delete ACLS %v",
			egressFirewall.Namespace, err)}
	}

	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:egressFirewall=%s", egressFirewall.Namespace))
	if err != nil {
		return []error{fmt.Errorf("error executing find ACL command, stderr: %q, %+v", stderr, err)}
	}
	var errList []error
	uuids := strings.Fields(stdout)
	for _, uuid := range uuids {
		for _, node := range existingNodes.Items {
			_, stderr, err := util.RunOVNNbctl("remove", "logical_switch",
				joinSwitch(node.Name), "acls", uuid)
			if err != nil {
				errList = append(errList, fmt.Errorf("failed to delete the rules for "+
					"egressFirewall in namespace %s on node %s, stderr: %q (%v)", egressFirewall.Namespace, node.Name, stderr, err))
			}
		}
	}
	return errList
}

func (oc *Controller) updateEgressFirewallWithRetry(egressfirewall *egressfirewallapi.EgressFirewall) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressFirewall(egressfirewall)
	})
}

func (ef *egressFirewall) addACLToJoinSwitch(joinSwitches []string, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string) error {
	for _, rule := range ef.egressRules {
		var match string
		var action string
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = "allow"
		} else {
			action = "drop"
		}

		ipAddress, _, err := net.ParseCIDR(rule.to.cidrSelector)
		if err != nil {
			return fmt.Errorf("error rule.to.cidrSelector %s is not a valid CIDR (%+v)", rule.to.cidrSelector, err)
		}
		if !utilnet.IsIPv6(ipAddress) {
			match = fmt.Sprintf("match=\"ip4.dst == %s && ip4.src == $%s\"", rule.to.cidrSelector, hashedAddressSetNameIPv4)
		} else {
			match = fmt.Sprintf("match=\"ip6.dst == %s && ip6.src == $%s\"", rule.to.cidrSelector, hashedAddressSetNameIPv6)
		}

		if len(rule.ports) > 0 {
			match = fmt.Sprintf("%s && ( %s )\"", match[:len(match)-1], egressGetL4Match(rule.ports))
		}
		uuid, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
			"--columns=_uuid", "find", "ACL", match, "action="+action,
			fmt.Sprintf("external-ids:egressFirewall=%s", ef.namespace))

		if err != nil {
			return fmt.Errorf("error executing find ACL command, stderr: %q, %+v", stderr, err)
		}
		for _, joinSwitch := range joinSwitches {
			if uuid == "" {
				_, stderr, err := util.RunOVNNbctl("--id=@acl", "create", "acl",
					fmt.Sprintf("priority=%d", defaultStartPriority-rule.id),
					fmt.Sprintf("direction=%s", fromLport), match, "action="+action,
					fmt.Sprintf("external-ids:egressFirewall=%s", ef.namespace),
					"--", "add", "logical_switch", joinSwitch,
					"acls", "@acl")
				if err != nil {
					return fmt.Errorf("error executing create ACL command, stderr: %q, %+v", stderr, err)
				}
			} else {
				_, stderr, err := util.RunOVNNbctl("add", "logical_switch", joinSwitch, "acls", uuid)
				if err != nil {
					return fmt.Errorf("error adding ACL to joinsSwitch %s failed, stderr: %q, %+v", joinSwitch, stderr, err)

				}
			}
		}
	}
	return nil
}

func egressGetL4Match(ports []egressfirewallapi.EgressFirewallPort) string {
	var udpString string
	var tcpString string
	var sctpString string
	for _, port := range ports {
		if kapi.Protocol(port.Protocol) == kapi.ProtocolUDP && udpString != "udp" {
			if port.Port == 0 {
				udpString = "udp"
			} else {
				udpString = fmt.Sprintf("%s udp.dst == %d ||", udpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolTCP && tcpString != "tcp" {
			if port.Port == 0 {
				tcpString = "tcp"
			} else {
				tcpString = fmt.Sprintf("%s tcp.dst == %d ||", tcpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolSCTP && sctpString != "sctp" {
			if port.Port == 0 {
				sctpString = "sctp"
			} else {
				sctpString = fmt.Sprintf("%s sctp.dst == %d ||", sctpString, port.Port)
			}
		}
	}
	// build the l4 match
	var l4Match string
	type tuple struct {
		protocolName     string
		protocolFormated string
	}
	list := []tuple{
		{
			protocolName:     "udp",
			protocolFormated: udpString,
		},
		{
			protocolName:     "tcp",
			protocolFormated: tcpString,
		},
		{
			protocolName:     "sctp",
			protocolFormated: sctpString,
		},
	}
	for _, entry := range list {
		if entry.protocolName == entry.protocolFormated {
			if l4Match == "" {
				l4Match = fmt.Sprintf("(%s)", entry.protocolName)
			} else {
				l4Match = fmt.Sprintf("%s || (%s)", l4Match, entry.protocolName)
			}
		} else {
			if l4Match == "" && entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("(%s && (%s))", entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			} else if entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("%s || (%s && (%s))", l4Match, entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			}
		}
	}
	return l4Match
}

func joinSwitch(nodeName string) string {
	return fmt.Sprintf("join_%s", nodeName)
}
