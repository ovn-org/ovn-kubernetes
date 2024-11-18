package model

import (
	"fmt"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

type NetworkEvent interface {
	String() string
}

type ACLEvent struct {
	NetworkEvent
	Action    string
	Actor     string
	Name      string
	Namespace string
	Direction string
}

func (e *ACLEvent) String() string {
	var action string
	switch e.Action {
	case nbdb.ACLActionAllow, nbdb.ACLActionAllowRelated, nbdb.ACLActionAllowStateless:
		action = "Allowed"
	case nbdb.ACLActionDrop:
		action = "Dropped"
	case nbdb.ACLActionPass:
		action = "Delegated to network policy"
	default:
		action = "Action " + e.Action
	}
	var msg string
	switch e.Actor {
	case libovsdbops.AdminNetworkPolicyOwnerType:
		msg = fmt.Sprintf("admin network policy %s, direction %s", e.Name, e.Direction)
	case libovsdbops.BaselineAdminNetworkPolicyOwnerType:
		msg = fmt.Sprintf("baseline admin network policy %s, direction %s", e.Name, e.Direction)
	case libovsdbops.MulticastNamespaceOwnerType:
		msg = fmt.Sprintf("multicast in namespace %s, direction %s", e.Namespace, e.Direction)
	case libovsdbops.MulticastClusterOwnerType:
		msg = fmt.Sprintf("cluster multicast policy, direction %s", e.Direction)
	case libovsdbops.NetpolNodeOwnerType:
		msg = fmt.Sprintf("default allow from local node policy, direction %s", e.Direction)
	case libovsdbops.NetworkPolicyOwnerType:
		if e.Namespace != "" {
			msg = fmt.Sprintf("network policy %s in namespace %s, direction %s", e.Name, e.Namespace, e.Direction)
		} else {
			msg = fmt.Sprintf("network policy %s, direction %s", e.Name, e.Direction)
		}
	case libovsdbops.NetpolNamespaceOwnerType:
		msg = fmt.Sprintf("network policies isolation in namespace %s, direction %s", e.Namespace, e.Direction)
	case libovsdbops.EgressFirewallOwnerType:
		msg = fmt.Sprintf("egress firewall in namespace %s", e.Namespace)
	case libovsdbops.UDNIsolationOwnerType:
		msg = fmt.Sprintf("UDN isolation of type %s", e.Name)
	}
	return fmt.Sprintf("%s by %s", action, msg)
}
