package ovn

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	knet "k8s.io/api/networking/v1"
	utilnet "k8s.io/utils/net"
)

// WatchNetworkPolicy starts the watching of network policy resource and calls
// back the appropriate handler logic
func (oc *DefaultNetworkController) WatchNetworkPolicy() error {
	_, err := oc.retryNetworkPolicies.WatchResource()
	return err
}

func (oc *DefaultNetworkController) getNetpolDefaultACLDbIDs(direction string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolDefault, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      allowHairpinningACLID,
			libovsdbops.PolicyDirectionKey: direction,
		})
}

func (oc *DefaultNetworkController) addHairpinAllowACL() error {
	var v4Match, v6Match, match string

	if config.IPv4Mode {
		v4Match = fmt.Sprintf("%s.src == %s", "ip4", types.V4OVNServiceHairpinMasqueradeIP)
		match = v4Match
	}
	if config.IPv6Mode {
		v6Match = fmt.Sprintf("%s.src == %s", "ip6", types.V6OVNServiceHairpinMasqueradeIP)
		match = v6Match
	}
	if config.IPv4Mode && config.IPv6Mode {
		match = fmt.Sprintf("(%s || %s)", v4Match, v6Match)
	}

	ingressACLIDs := oc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeIngress))
	ingressACL := BuildACL(ingressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportIngress)

	egressACLIDs := oc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeEgress))
	egressACL := BuildACL(egressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportEgressAfterLB)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to create or update hairpin allow ACL %v", err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, oc.getClusterPortGroupName(types.ClusterPortGroupNameBase),
		ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL hairpin allow acl to port group: %v", err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (oc *DefaultNetworkController) syncNetworkPolicies(networkPolicies []interface{}) error {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncNetworkPolicies: %v", npInterface)
		}
		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}
	err := oc.syncNetworkPoliciesCommon(expectedPolicies)
	if err != nil {
		return err
	}

	// add default hairpin allow acl
	err = oc.addHairpinAllowACL()
	if err != nil {
		return fmt.Errorf("failed to create allow hairping acl: %w", err)
	}

	return nil
}

func getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNode, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.IpKey:         mgmtPortIP,
		})
}

// There is no delete function for this ACL type, because the ACL is applied on a node switch.
// When the node is deleted, switch will be deleted by the node sync, and the dependent ACLs will be
// garbage-collected.
func (oc *DefaultNetworkController) addAllowACLFromNode(nodeName string, mgmtPortIP net.IP) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())
	dbIDs := getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP.String(), oc.controllerName)
	nodeACL := BuildACL(dbIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportIngress)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to create or update ACL %v: %v", nodeACL, err)
	}

	ops, err = libovsdbops.AddACLsToLogicalSwitchOps(oc.nbClient, ops, nodeName, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL %v to switch %s: %v", nodeACL, nodeName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}
