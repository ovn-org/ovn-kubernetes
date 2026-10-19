// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/batching"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	// UDN ACL names, should be unique across all controllers
	// Default network-only ACLs:
	allowHostARPACL        = "AllowHostARPPrimaryUDN"
	allowHostPrimaryUDNACL = "AllowHostPrimaryUDN"
	denyPrimaryUDNACL      = "DenyPrimaryUDN"
	// OpenPortACLPrefix is used to build per-pod ACLs, pod name should be added to the prefix to build a unique name
	OpenPortACLPrefix = "OpenPort-"
	// the same tier is used for all UDN isolation ACLs
	isolationTier = types.PrimaryACLTier

	// Port Group ID for pods with primary UDN
	// Note, this is left with wording "Secondary" because we do not currently allow
	// mutating a port group's name. ACL match criteria may reference this name, so it
	// is unsafe to update. Therefore we keep the legacy name for now.
	legacySecondaryPodPGName = "SecondaryPods"

	// deprecated Legacy versions
	allowHostSecondaryACL = "AllowHostSecondary"
	denySecondaryACL      = "DenySecondary"
	legacyAllowHostARPACL = "AllowHostARPSecondary"
)

// setupUDNACLs should be called after the node's management port was configured
// Only used on default network switches.
func (oc *DefaultNetworkController) setupUDNACLs(mgmtPortIPs []net.IP) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}
	// add port group to track UDN primary pods
	pgIDs := oc.getSecondaryPodsPortGroupDbIDs()
	pg := &nbdb.PortGroup{
		Name: libovsdbutil.GetPortGroupName(pgIDs),
	}
	_, err := libovsdbops.GetPortGroup(oc.nbClient, pg)
	if err != nil {
		if !errors.Is(err, libovsdbclient.ErrNotFound) {
			return err
		}
		// we didn't find an existing secondaryPodsPG, let's create a new empty PG
		pg = libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
		err = libovsdbops.CreateOrUpdatePortGroups(oc.nbClient, pg)
		if err != nil {
			klog.Errorf("Failed to create secondary pods port group: %v", err)
			return err
		}
	}
	// Now add ACLs to limit non-primary pods traffic to only allow kubelet probes
	// - egress+ingress -> allow ARP to/from mgmtPort
	// - ingress -> allow-related all from mgmtPort
	// - egress+ingress -> deny everything else
	pgName := libovsdbutil.GetPortGroupName(pgIDs)
	egressDenyIDs := oc.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLEgress)
	match := libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLEgress)
	egressDenyACL := libovsdbutil.BuildACL(egressDenyIDs, types.PrimaryUDNDenyPriority, match, nbdb.ACLActionDrop,
		nil, libovsdbutil.LportEgress, isolationTier)

	getARPMatch := func(direction libovsdbutil.ACLDirection) string {
		match := "("
		for i, mgmtPortIP := range mgmtPortIPs {
			var protoMatch string
			if utilnet.IsIPv6(mgmtPortIP) {
				protoMatch = "( nd && nd.target == " + mgmtPortIP.String() + " )"
			} else {
				dir := "t"
				if direction == libovsdbutil.ACLIngress {
					dir = "s"
				}
				protoMatch = fmt.Sprintf("( arp && arp.%spa == %s )", dir, mgmtPortIP.String())
			}
			if i > 0 {
				match += " || "
			}
			match += protoMatch
		}
		match += ")"
		return match
	}

	egressARPIDs := oc.getUDNACLDbIDs(allowHostARPACL, libovsdbutil.ACLEgress)
	match = libovsdbutil.GetACLMatch(pgName, getARPMatch(libovsdbutil.ACLEgress), libovsdbutil.ACLEgress)
	egressARPACL := libovsdbutil.BuildACL(egressARPIDs, types.PrimaryUDNAllowPriority, match, nbdb.ACLActionAllow,
		nil, libovsdbutil.LportEgress, isolationTier)

	ingressDenyIDs := oc.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLIngress)
	match = libovsdbutil.GetACLMatch(pgName, "", libovsdbutil.ACLIngress)
	ingressDenyACL := libovsdbutil.BuildACL(ingressDenyIDs, types.PrimaryUDNDenyPriority, match, nbdb.ACLActionDrop,
		nil, libovsdbutil.LportIngress, isolationTier)

	ingressARPIDs := oc.getUDNACLDbIDs(allowHostARPACL, libovsdbutil.ACLIngress)
	match = libovsdbutil.GetACLMatch(pgName, getARPMatch(libovsdbutil.ACLIngress), libovsdbutil.ACLIngress)
	ingressARPACL := libovsdbutil.BuildACL(ingressARPIDs, types.PrimaryUDNAllowPriority, match, nbdb.ACLActionAllow,
		nil, libovsdbutil.LportIngress, isolationTier)

	ingressAllowIDs := oc.getUDNACLDbIDs(allowHostPrimaryUDNACL, libovsdbutil.ACLIngress)
	match = "("
	for i, mgmtPortIP := range mgmtPortIPs {
		ipFamily := "ip4"
		if utilnet.IsIPv6(mgmtPortIP) {
			ipFamily = "ip6"
		}
		ipMatch := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())
		if i > 0 {
			match += " || "
		}
		match += ipMatch
	}
	match += ")"
	match = libovsdbutil.GetACLMatch(pgName, match, libovsdbutil.ACLIngress)
	ingressAllowACL := libovsdbutil.BuildACL(ingressAllowIDs, types.PrimaryUDNAllowPriority, match, nbdb.ACLActionAllowRelated,
		nil, libovsdbutil.LportIngress, isolationTier)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, oc.GetSamplingConfig("", libovsdbops.UDNIsolationSample), egressDenyACL, egressARPACL, ingressARPACL, ingressDenyACL, ingressAllowACL)
	if err != nil {
		return fmt.Errorf("failed to create or update UDN ACLs: %v", err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, egressDenyACL, egressARPACL, ingressARPACL, ingressDenyACL, ingressAllowACL)
	if err != nil {
		return fmt.Errorf("failed to add UDN ACLs to portGroup %s: %v", pgName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
}

func (oc *DefaultNetworkController) getSecondaryPodsPortGroupDbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupUDN, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: legacySecondaryPodPGName,
		})
}

func (oc *DefaultNetworkController) getUDNACLDbIDs(name string, aclDir libovsdbutil.ACLDirection) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLUDN, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      name,
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

func getPortsMatches(podAnnotations map[string]string, lspName string) (string, string, error) {
	if lspName == "" {
		return "", "", nil
	}
	ports, err := util.UnmarshalUDNOpenPortsAnnotation(podAnnotations)
	if err != nil {
		return "", "", err
	}
	if len(ports) == 0 {
		return "", "", nil
	}
	// protocol match is only used for ingress rules, use dst match
	portMatches := []string{}
	for _, portDef := range ports {
		if portDef.Protocol == "icmp" {
			// from the ovn docs:
			// "icmp expands to icmp4 || icmp6"
			portMatches = append(portMatches, "icmp")
		} else {
			portMatches = append(portMatches, fmt.Sprintf("%s.dst == %d", portDef.Protocol, *portDef.Port))
		}
	}
	protoMatch := strings.Join(portMatches, " || ")
	// allow ingress for ARP or ND and open ports
	// allow egress for ARP or ND
	ingressMatch := fmt.Sprintf(`outport == "%s" && (arp || nd || (%s))`, lspName, protoMatch)
	egressMatch := fmt.Sprintf(`inport == "%s" && (arp || nd)`, lspName)

	return ingressMatch, egressMatch, nil
}

// setUDNPodOpenPorts should be called after the pod's lsp is created to add ACLs that allow ingress on required ports.
// When lspName="", ACLs are removed. If annotation can't be parsed correctly, ACLs will be deleted.
func (oc *DefaultNetworkController) setUDNPodOpenPorts(podNamespacedName string, podAnnotations map[string]string, lspName string) error {
	ops, parseErr, err := oc.setUDNPodOpenPortsOps(podNamespacedName, podAnnotations, lspName, nil)
	if err != nil {
		return errors.Join(parseErr, err)
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return utilerrors.Join(parseErr, fmt.Errorf("failed to transact open ports UDN ACLs: %v", err))
	}
	return parseErr
}

// setUDNPodOpenPortsOps returns the operations to add or remove ACLs that allow ingress on required ports.
// first returned error is parse error, second is db ops error
func (oc *DefaultNetworkController) setUDNPodOpenPortsOps(podNamespacedName string, podAnnotations map[string]string, lspName string,
	ops []ovsdb.Operation) ([]ovsdb.Operation, error, error) {
	udnPGName := libovsdbutil.GetPortGroupName(oc.getSecondaryPodsPortGroupDbIDs())

	ingressMatch, egressMatch, parseErr := getPortsMatches(podAnnotations, lspName)
	// don't return on parseErr, as we need to cleanup potentially present ACLs from the previous config
	ingressIDs := oc.getUDNOpenPortDbIDs(podNamespacedName, libovsdbutil.ACLIngress)
	ingressACL := libovsdbutil.BuildACL(ingressIDs, types.PrimaryUDNAllowPriority,
		ingressMatch, nbdb.ACLActionAllowRelated, nil, libovsdbutil.LportIngress, isolationTier)

	egressIDs := oc.getUDNOpenPortDbIDs(podNamespacedName, libovsdbutil.ACLEgress)
	egressACL := libovsdbutil.BuildACL(egressIDs, types.PrimaryUDNAllowPriority,
		egressMatch, nbdb.ACLActionAllow, nil, libovsdbutil.LportEgress, isolationTier)

	var err error
	if ingressMatch == "" && egressMatch == "" || parseErr != nil {
		// no open ports or error parsing annotations, remove ACLs
		foundACLs, err := libovsdbops.FindACLs(oc.nbClient, []*nbdb.ACL{ingressACL, egressACL})
		if err != nil {
			return ops, parseErr, fmt.Errorf("failed to find open ports UDN ACLs: %v", err)
		}
		ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, udnPGName, foundACLs...)
		if err != nil {
			return ops, parseErr, fmt.Errorf("failed to remove open ports ACLs from portGroup %s: %v", udnPGName, err)
		}
	} else {
		// update ACLs
		ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, oc.GetSamplingConfig("", libovsdbops.UDNIsolationSample), ingressACL, egressACL)
		if err != nil {
			return ops, parseErr, fmt.Errorf("failed to create or update open ports UDN ACLs: %v", err)
		}

		ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, udnPGName, ingressACL, egressACL)
		if err != nil {
			return ops, parseErr, fmt.Errorf("failed to add open ports ACLs to portGroup %s: %v", udnPGName, err)
		}
	}
	return ops, parseErr, nil
}

func (oc *DefaultNetworkController) getUDNOpenPortDbIDs(podNamespacedName string, aclDir libovsdbutil.ACLDirection) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLUDN, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      OpenPortACLPrefix + podNamespacedName,
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

// advertisedNetworkSubnetsKey is the object name key for the global advertised
// networks addressset, the global deny ACL and the port group hosting the deny
// ACL.
const advertisedNetworkSubnetsKey = "advertised-network-subnets"

// GetAdvertisedNetworkSubnetsAddressSetDBIDs returns the DB IDs for the advertised network subnets addressset
func GetAdvertisedNetworkSubnetsAddressSetDBIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetAdvertisedNetwork, types.DefaultNetworkControllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: advertisedNetworkSubnetsKey,
	})
}

// GetAdvertisedNetworkSubnetsDropPGdbIDs returns the DB IDs for the advertised network subnets drop port group
func GetAdvertisedNetworkSubnetsDropPGdbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupAdvertisedNetwork, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: advertisedNetworkSubnetsKey,
		})
}

// GetAdvertisedNetworkSubnetsDropPGName returns the hashed name for the advertised network subnets drop port group
func GetAdvertisedNetworkSubnetsDropPGName() string {
	return libovsdbutil.GetPortGroupName(GetAdvertisedNetworkSubnetsDropPGdbIDs())
}

// GetAdvertisedNetworkSubnetsDropACLdbIDs returns the DB IDs for the advertised network subnets drop ACL
func GetAdvertisedNetworkSubnetsDropACLdbIDs() *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLAdvertisedNetwork, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: advertisedNetworkSubnetsKey,
			libovsdbops.NetworkKey:    "",
		})
}

// GetAdvertisedNetworkSubnetsPassACLdbIDs returns the DB IDs for the advertised network subnets pass ACL
func GetAdvertisedNetworkSubnetsPassACLdbIDs(controller, networkName string, networkID int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLAdvertisedNetwork, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: networkName,
			libovsdbops.NetworkKey:    strconv.Itoa(networkID),
		})
}

// ConfigureAdvertisedNetworkIsolation ensures the global resources for advertised network isolation exist.
func ConfigureAdvertisedNetworkIsolation(nbClient libovsdbclient.Client) error {
	addressSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
	addrSet, ops, err := addressSetFactory.EnsureAddressSetOps(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
	if err != nil {
		return fmt.Errorf("failed to ensure advertised subnets address set: %w", err)
	}

	dropACL := BuildAdvertisedNetworkSubnetsDropACL(addrSet)
	pg := libovsdbutil.BuildPortGroup(GetAdvertisedNetworkSubnetsDropPGdbIDs(), nil, nil)

	ops, err = migrateAdvertisedNetworkDropACLToPortGroup(nbClient, pg, dropACL, ops)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.CreateOrUpdateACLsOps(nbClient, ops, nil, dropACL)
	if err != nil {
		return fmt.Errorf("failed to create or update advertised network isolation drop ACL: %w", err)
	}

	pg.ACLs = []string{dropACL.UUID}

	ops, err = libovsdbops.CreatePortGroupOps(nbClient, ops, pg)
	if err != nil {
		return fmt.Errorf("failed to create advertised network isolation port group: %w", err)
	}

	if _, err = libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
		return fmt.Errorf("failed to configure advertised network isolation: %w", err)
	}
	return nil
}

// migrateAdvertisedNetworkDropACLToPortGroup handles upgrade from switch-based drop ACL.
// If the port group already exists, migration was already done. Otherwise, it looks up
// the existing drop ACL by index. If found, it removes it from switches and populates the
// port group with the stor ports of affected switches. If the index lookup fails with
// multiple results (the race condition this migration fixes), it falls back to a predicate
// scan. It sets dropACL.UUID to the first existing ACL's UUID so the caller can update it
// in place.
func migrateAdvertisedNetworkDropACLToPortGroup(nbClient libovsdbclient.Client, pg *nbdb.PortGroup, dropACL *nbdb.ACL, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	_, err := libovsdbops.GetPortGroup(nbClient, &nbdb.PortGroup{Name: pg.Name})
	if err == nil {
		return ops, nil
	}
	if !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, fmt.Errorf("failed to get advertised network isolation port group: %w", err)
	}

	var existingDropACLs []*nbdb.ACL
	existingACL, err := libovsdbops.GetACL(nbClient, &nbdb.ACL{ExternalIDs: GetAdvertisedNetworkSubnetsDropACLdbIDs().GetExternalIDs()})
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return ops, nil
	}
	if err == nil {
		existingDropACLs = []*nbdb.ACL{existingACL}
	} else {
		// multiple ACLs found (the race condition this migration fixes), fall back to predicate scan
		existingDropACLs, err = libovsdbops.FindACLsWithPredicate(nbClient,
			libovsdbops.GetPredicate[*nbdb.ACL](GetAdvertisedNetworkSubnetsDropACLdbIDs(), nil))
		if err != nil {
			return nil, fmt.Errorf("failed to find advertised network drop ACLs: %w", err)
		}
	}
	// shouldn't happen but safe-guard it
	if len(existingDropACLs) == 0 {
		return ops, nil
	}

	dropACL.UUID = existingDropACLs[0].UUID

	allDropACLUUIDs := sets.New[string]()
	for _, acl := range existingDropACLs {
		allDropACLUUIDs.Insert(acl.UUID)
	}

	var affectedSwitches []string
	p := func(sw *nbdb.LogicalSwitch) bool {
		if allDropACLUUIDs.HasAny(sw.ACLs...) {
			affectedSwitches = append(affectedSwitches, sw.Name)
			return true
		}
		return false
	}
	ops, err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicateOps(nbClient, ops, p, existingDropACLs...)
	if err != nil {
		return nil, fmt.Errorf("failed to remove stale drop ACLs from switches: %w", err)
	}

	for _, swName := range affectedSwitches {
		storPortName := util.GetNetworkScopedSwitchToRouterPortNameFromSwitchName(swName)
		storLSP, err := libovsdbops.GetLogicalSwitchPort(nbClient, &nbdb.LogicalSwitchPort{Name: storPortName})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Warningf("Skipping advertised network isolation port group population for switch %s: stor port %s not found", swName, storPortName)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get stor port %s: %w", storPortName, err)
		}
		pg.Ports = append(pg.Ports, storLSP.UUID)
	}

	return ops, nil
}

// CleanupStaleAdvertisedNetworkSubnets removes subnets from the advertised network subnets address set
// that are not in validSubnets.
func CleanupStaleAdvertisedNetworkSubnets(nbClient libovsdbclient.Client, validSubnets sets.Set[string]) error {
	addressSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

	addrSetDBIDs := GetAdvertisedNetworkSubnetsAddressSetDBIDs()
	addrSet, err := addressSetFactory.GetAddressSet(addrSetDBIDs)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to get advertised subnets address set %s: %w", addrSetDBIDs, err)
	}
	if addrSet == nil {
		return nil
	}

	v4Addrs, v6Addrs := addrSet.GetAddresses()
	var stale []string
	for _, addr := range append(v4Addrs, v6Addrs...) {
		if !validSubnets.Has(addr) {
			stale = append(stale, addr)
		}
	}
	if len(stale) > 0 {
		if err := addrSet.DeleteAddresses(stale); err != nil {
			klog.Errorf("Failed to delete stale addresses %q from advertised network subnets address set: %v", stale, err)
		}
		klog.Infof("Cleaned up stale advertised addresses %q from advertised network subnets address set", stale)
	}

	return nil
}

// BuildAdvertisedNetworkSubnetsDropACL builds the advertised network subnets drop ACL:
// action match                                                                       priority
// ------ --------------------------------------------------------------------------- --------
// drop   "(ip[4|6].src == $<ALL_ADV_SUBNETS> && ip[4|6].dst == $<ALL_ADV_SUBNETS>)"    1050
func BuildAdvertisedNetworkSubnetsDropACL(advertisedNetworkSubnetsAddressSet addressset.AddressSet) *nbdb.ACL {
	var dropMatches []string
	v4AddrSet, v6AddrSet := advertisedNetworkSubnetsAddressSet.GetASHashNames()
	if v4AddrSet != "" {
		dropMatches = append(dropMatches, fmt.Sprintf("(ip4.src == $%s && ip4.dst == $%s)", v4AddrSet, v4AddrSet))
	}
	if v6AddrSet != "" {
		dropMatches = append(dropMatches, fmt.Sprintf("(ip6.src == $%s && ip6.dst == $%s)", v6AddrSet, v6AddrSet))
	}

	dropACL := libovsdbutil.BuildACL(
		GetAdvertisedNetworkSubnetsDropACLdbIDs(),
		types.AdvertisedNetworkDenyPriority,
		strings.Join(dropMatches, " || "),
		nbdb.ACLActionDrop,
		nil,
		libovsdbutil.LportEgressAfterLB,
		isolationTier)
	return dropACL
}

// addAdvertisedNetworkIsolation adds advertised network isolation rules to the given node.
// We end up with the following ACLs:
// action match                                                                       priority
// ------ --------------------------------------------------------------------------- --------
// pass   "(ip[4|6].src == <UDN_SUBNET> && ip[4|6].dst == <UDN_SUBNET>)"                1100
// drop   "(ip[4|6].src == $<ALL_ADV_SUBNETS> && ip[4|6].dst == $<ALL_ADV_SUBNETS>)"    1050
// The pass ACL is added to the node switch. The drop ACL is on a port group shared by all
// advertised networks. The stor port of this network's switch is added to that port group
// so the drop ACL applies to the whole switch.
// On upgrade from switch-based drop ACL, stale switch references are removed in the same transaction.
func (bnc *BaseNetworkController) addAdvertisedNetworkIsolation(nodeName string) error {
	var passMatches, cidrs []string
	var ops []ovsdb.Operation

	addrSetDBIDs := GetAdvertisedNetworkSubnetsAddressSetDBIDs()
	addrSet, err := bnc.addressSetFactory.GetAddressSet(addrSetDBIDs)
	if err != nil {
		return fmt.Errorf("failed to get advertised subnets address set %s for network %s: %w", addrSetDBIDs, bnc.GetNetworkName(), err)
	}
	var ipv4Subnets, ipv6Subnets []*net.IPNet
	for _, subnet := range bnc.Subnets() {
		if utilnet.IsIPv6CIDR(subnet.CIDR) {
			ipv6Subnets = append(ipv6Subnets, subnet.CIDR)
		} else {
			ipv4Subnets = append(ipv4Subnets, subnet.CIDR)
		}
		cidrs = append(cidrs, subnet.CIDR.String())
	}
	if len(ipv4Subnets) > 0 {
		var srcMatches, dstMatches []string
		for _, subnet := range ipv4Subnets {
			srcMatches = append(srcMatches, fmt.Sprintf("ip4.src == %s", subnet))
			dstMatches = append(dstMatches, fmt.Sprintf("ip4.dst == %s", subnet))
		}
		// build match ((ip4.src == subnet1 || ip4.src == subnet2 ...) && (ip4.dst == subnet1 || ip4.dst == subnet2 ...))
		passMatches = append(passMatches, fmt.Sprintf("((%s) && (%s))", strings.Join(srcMatches, " || "), strings.Join(dstMatches, " || ")))
	}
	if len(ipv6Subnets) > 0 {
		var srcMatches, dstMatches []string
		for _, subnet := range ipv6Subnets {
			srcMatches = append(srcMatches, fmt.Sprintf("ip6.src == %s", subnet))
			dstMatches = append(dstMatches, fmt.Sprintf("ip6.dst == %s", subnet))
		}
		// build match ((ip6.src == subnet1 || ip6.src == subnet2 ...) && (ip6.dst == subnet1 || ip6.dst == subnet2 ...))
		passMatches = append(passMatches, fmt.Sprintf("((%s) && (%s))", strings.Join(srcMatches, " || "), strings.Join(dstMatches, " || ")))
	}
	addrOps, err := addrSet.AddAddressesReturnOps(cidrs)
	if err != nil {
		return fmt.Errorf("failed to add addresses %q to the %s address set for network %s: %w", cidrs, GetAdvertisedNetworkSubnetsAddressSetDBIDs(), bnc.GetNetworkName(), err)
	}
	ops = append(ops, addrOps...)

	switchName := bnc.GetNetworkScopedSwitchName(nodeName)

	if len(passMatches) > 0 {
		passACL := libovsdbutil.BuildACL(
			GetAdvertisedNetworkSubnetsPassACLdbIDs(bnc.controllerName, bnc.GetNetworkName(), bnc.GetNetworkID()),
			types.AdvertisedNetworkPassPriority,
			strings.Join(passMatches, " || "),
			nbdb.ACLActionPass,
			nil,
			libovsdbutil.LportEgressAfterLB,
			isolationTier)

		ops, err = libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, ops, nil, passACL)
		if err != nil {
			return fmt.Errorf("failed to create or update network isolation pass ACL for network %s: %w", bnc.GetNetworkName(), err)
		}
		ops, err = libovsdbops.AddACLsToLogicalSwitchOps(bnc.nbClient, ops, switchName, passACL)
		if err != nil {
			return fmt.Errorf("failed to add network isolation pass ACL to switch %s for network %s: %w", switchName, bnc.GetNetworkName(), err)
		}
	}

	// Add the stor port to the port group
	pgName := GetAdvertisedNetworkSubnetsDropPGName()
	storPortName := bnc.GetNetworkScopedSwitchToRouterPortName(nodeName)
	storLSP, err := libovsdbops.GetLogicalSwitchPort(bnc.nbClient, &nbdb.LogicalSwitchPort{Name: storPortName})
	if err != nil {
		return fmt.Errorf("failed to get stor port %s: %w", storPortName, err)
	}
	ops, err = libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, ops, pgName, storLSP.UUID)
	if err != nil {
		return fmt.Errorf("failed to add stor port %s to advertised network isolation port group: %w", storPortName, err)
	}

	if _, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops); err != nil {
		return fmt.Errorf("failed to configure network isolation OVN rules for network %s: %w", bnc.GetNetworkName(), err)
	}
	return nil
}

// deleteAdvertisedNetworkIsolation deletes advertised network isolation rules from the given node switch.
// It removes the network CIDRs from the global advertised networks address set, removes the pass ACL from
// the node switch, and removes the stor port from the advertised network drop port group.
func (bnc *BaseNetworkController) deleteAdvertisedNetworkIsolation(nodeName string) error {
	addrSet, err := bnc.addressSetFactory.GetAddressSet(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to get advertised subnets addresset %s for network %s: %w", GetAdvertisedNetworkSubnetsAddressSetDBIDs(), bnc.GetNetworkName(), err)
	}

	var ops []ovsdb.Operation
	if addrSet != nil {
		var cidrs []string
		for _, subnet := range bnc.Subnets() {
			cidrs = append(cidrs, subnet.CIDR.String())
		}
		ops, err = addrSet.DeleteAddressesReturnOps(cidrs)
		if err != nil {
			return fmt.Errorf("failed to create ovsdb ops for deleting the addresses from %s addresset for network %s: %w", GetAdvertisedNetworkSubnetsAddressSetDBIDs(), bnc.GetNetworkName(), err)
		}
	}

	switchName := bnc.GetNetworkScopedSwitchName(nodeName)

	// Remove the pass ACL from the switch
	passACLIDs := GetAdvertisedNetworkSubnetsPassACLdbIDs(bnc.controllerName, bnc.GetNetworkName(), bnc.GetNetworkID())
	passACL, err := libovsdbops.GetACL(bnc.nbClient, &nbdb.ACL{ExternalIDs: passACLIDs.GetExternalIDs()})
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("unable to find pass ACL for advertised network %s: %w", bnc.GetNetworkName(), err)
	}
	if passACL != nil {
		ops, err = libovsdbops.RemoveACLsFromLogicalSwitchOps(bnc.nbClient, ops, switchName, passACL)
		if err != nil {
			return fmt.Errorf("failed to remove pass ACL from switch %s for network %s: %w", switchName, bnc.GetNetworkName(), err)
		}
	}

	// Remove the stor port from the port group
	storPortName := bnc.GetNetworkScopedSwitchToRouterPortName(nodeName)
	storLSP, err := libovsdbops.GetLogicalSwitchPort(bnc.nbClient, &nbdb.LogicalSwitchPort{Name: storPortName})
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to get stor port %s: %w", storPortName, err)
	}
	if storLSP != nil {
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, ops, GetAdvertisedNetworkSubnetsDropPGName(), storLSP.UUID)
		if err != nil {
			return fmt.Errorf("failed to remove stor port %s from advertised network isolation port group: %w", storPortName, err)
		}
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	return err
}

func (oc *DefaultNetworkController) syncUDNIsolation() error {
	// Find ACLs with old "secondary" naming IDs, update them
	type aclUpdate struct {
		old *libovsdbops.DbObjectIDs
		new *libovsdbops.DbObjectIDs
	}
	updates := []*aclUpdate{
		{oc.getUDNACLDbIDs(denySecondaryACL, libovsdbutil.ACLEgress), oc.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLEgress)},
		{oc.getUDNACLDbIDs(legacyAllowHostARPACL, libovsdbutil.ACLEgress), oc.getUDNACLDbIDs(allowHostARPACL, libovsdbutil.ACLEgress)},
		{oc.getUDNACLDbIDs(denySecondaryACL, libovsdbutil.ACLIngress), oc.getUDNACLDbIDs(denyPrimaryUDNACL, libovsdbutil.ACLIngress)},
		{oc.getUDNACLDbIDs(legacyAllowHostARPACL, libovsdbutil.ACLIngress), oc.getUDNACLDbIDs(allowHostARPACL, libovsdbutil.ACLIngress)},
		{oc.getUDNACLDbIDs(allowHostSecondaryACL, libovsdbutil.ACLIngress), oc.getUDNACLDbIDs(allowHostPrimaryUDNACL, libovsdbutil.ACLIngress)},
	}

	var aclsToUpdate []*nbdb.ACL
	_, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, func(acl *nbdb.ACL) bool {
		for _, update := range updates {
			p := libovsdbops.GetPredicate[*nbdb.ACL](update.old, nil)
			if p(acl) {
				copy := *acl
				copy.ExternalIDs = update.new.GetExternalIDs()
				aclName := libovsdbutil.GetACLName(update.new)
				copy.Name = &aclName
				aclsToUpdate = append(aclsToUpdate, &copy)
				return false
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("unable to find ACLs for UDN Isolation sync: %w", err)
	}

	if len(aclsToUpdate) > 0 {
		err := batching.Batch(20000, aclsToUpdate, func(batchACLs []*nbdb.ACL) error {
			return libovsdbops.CreateOrUpdateACLs(oc.nbClient, oc.GetSamplingConfig("", libovsdbops.UDNIsolationSample), batchACLs...)
		})
		if err != nil {
			return fmt.Errorf("failed to create or update UDN ACLs: %w", err)
		}
	}

	return nil
}
