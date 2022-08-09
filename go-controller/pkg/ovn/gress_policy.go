package ovn

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type gressPolicy struct {
	policyNamespace string
	policyName      string
	policyType      knet.PolicyType
	idx             int

	// peerAddressSet points to the addressSet that holds all peer pod
	// IP addresess.
	peerAddressSet addressset.AddressSet

	// peerV4AddressSets has Address sets for all namespaces and pod selectors for IPv4
	peerV4AddressSets sets.String
	// peerV6AddressSets has Address sets for all namespaces and pod selectors for IPv6
	peerV6AddressSets sets.String

	// portPolicies represents all the ports to which traffic is allowed for
	// the rule in question.
	portPolicies []*portPolicy

	ipBlock []*knet.IPBlock
}

type portPolicy struct {
	protocol string
	port     int32
	endPort  int32
}

// for a given ingress/egress rule, captures all the provided port ranges and
// individual ports
type gressPolicyPorts struct {
	portList  []string // list of provided ports as string
	portRange []string // list of provided port ranges in OVN ACL format
}

func (gp *gressPolicy) getProtocolPortsMap() map[string]*gressPolicyPorts {
	var supportedProtocols = []string{TCP, UDP, SCTP}
	gressProtoPortsMap := make(map[string]*gressPolicyPorts)
	for _, pp := range gp.portPolicies {
		var found bool
		for _, protocol := range supportedProtocols {
			if protocol == pp.protocol {
				found = true
				break
			}
		}
		if !found {
			klog.Warningf("Unknown protocol %v, while processing network policy %s/%s",
				pp.protocol, gp.policyNamespace, gp.policyName)
			continue
		}
		protocol := strings.ToLower(pp.protocol)
		gpp, ok := gressProtoPortsMap[protocol]
		if !ok {
			gpp = &gressPolicyPorts{portList: []string{}, portRange: []string{}}
			gressProtoPortsMap[protocol] = gpp
		}
		if pp.endPort != 0 && pp.endPort != pp.port {
			gpp.portRange = append(gpp.portRange, fmt.Sprintf("%d<=%s.dst<=%d", pp.port, protocol, pp.endPort))
		} else if pp.port != 0 {
			gpp.portList = append(gpp.portList, fmt.Sprintf("%d", pp.port))
		}
	}
	return gressProtoPortsMap
}

func getL4Match(protocol string, ports *gressPolicyPorts) string {
	allL4Matches := []string{}
	if len(ports.portList) > 0 {
		// if there is just one port, then don't use `{}`
		template := "%s.dst==%s"
		if len(ports.portList) > 1 {
			template = "%s.dst=={%s}"
		}
		allL4Matches = append(allL4Matches, fmt.Sprintf(template, protocol, strings.Join(ports.portList, ",")))
	}
	allL4Matches = append(allL4Matches, ports.portRange...)
	l4Match := protocol
	if len(allL4Matches) > 0 {
		template := "%s && %s"
		if len(allL4Matches) > 1 {
			template = "%s && (%s)"
		}
		l4Match = fmt.Sprintf(template, protocol, strings.Join(allL4Matches, " || "))
	}
	return l4Match
}

func newGressPolicy(policyType knet.PolicyType, idx int, namespace, name string) *gressPolicy {
	return &gressPolicy{
		policyNamespace:   namespace,
		policyName:        name,
		policyType:        policyType,
		idx:               idx,
		peerV4AddressSets: sets.String{},
		peerV6AddressSets: sets.String{},
		portPolicies:      make([]*portPolicy, 0),
	}
}

func (gp *gressPolicy) ensurePeerAddressSet(factory addressset.AddressSetFactory) error {
	if gp.peerAddressSet != nil {
		return nil
	}

	direction := strings.ToLower(string(gp.policyType))
	asName := fmt.Sprintf("%s.%s.%s.%d", gp.policyNamespace, gp.policyName, direction, gp.idx)
	as, err := factory.NewAddressSet(asName, nil)
	if err != nil {
		return err
	}

	gp.peerAddressSet = as
	ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()
	if ipv4HashedAS != "" {
		gp.peerV4AddressSets.Insert("$" + ipv4HashedAS)
	}
	if ipv6HashedAS != "" {
		gp.peerV6AddressSets.Insert("$" + ipv6HashedAS)
	}
	return nil
}

func (gp *gressPolicy) addPeerSvcVip(nbClient client.Client, service *v1.Service) error {
	if gp.peerAddressSet == nil {
		return fmt.Errorf("peer AddressSet is nil, cannot add peer Service: %s for gressPolicy: %s",
			service.ObjectMeta.Name, gp.policyName)
	}

	klog.V(5).Infof("Service %s is applied to same namespace as network Policy, finding Service VIPs", service.Name)
	ips := getSvcVips(nbClient, service)

	klog.V(5).Infof("Adding SVC clusterIP to gressPolicy's Address Set: %v", ips)
	return gp.peerAddressSet.AddIPs(ips)
}

func (gp *gressPolicy) deletePeerSvcVip(nbClient client.Client, service *v1.Service) error {
	if gp.peerAddressSet == nil {
		return fmt.Errorf("peer AddressSet is nil, cannot add peer Service: %s for gressPolicy: %s",
			service.ObjectMeta.Name, gp.policyName)
	}

	klog.V(5).Infof("Service %s is applied to same namespace as network Policy, finding cluster IPs", service.Name)
	ips := getSvcVips(nbClient, service)

	klog.Infof("Deleting service %s, possible VIPs: %v from gressPolicy's %s Address Set", service.Name, ips, gp.policyName)
	return gp.peerAddressSet.DeleteIPs(ips)
}

func (gp *gressPolicy) addPeerPods(pods ...*v1.Pod) error {
	if gp.peerAddressSet == nil {
		return fmt.Errorf("peer AddressSet is nil, cannot add peer pod(s): for gressPolicy: %s",
			gp.policyName)
	}

	podIPFactor := 1
	if config.IPv4Mode && config.IPv6Mode {
		podIPFactor = 2
	}
	ips := make([]net.IP, 0, len(pods)*podIPFactor)
	for _, pod := range pods {
		podIPs, err := util.GetAllPodIPs(pod)
		if err != nil {
			return err
		}
		ips = append(ips, podIPs...)
	}

	return gp.peerAddressSet.AddIPs(ips)
}

func (gp *gressPolicy) deletePeerPod(pod *v1.Pod) error {
	ips, err := util.GetAllPodIPs(pod)
	if err != nil {
		return err
	}
	return gp.peerAddressSet.DeleteIPs(ips)
}

// If the port is not specified, it implies all ports for that protocol
func (gp *gressPolicy) addPortPolicy(portJSON *knet.NetworkPolicyPort) {
	pp := &portPolicy{protocol: string(*portJSON.Protocol),
		port:    0,
		endPort: 0,
	}
	if portJSON.Port != nil {
		pp.port = portJSON.Port.IntVal
	}
	if portJSON.EndPort != nil {
		pp.endPort = *portJSON.EndPort
	}
	gp.portPolicies = append(gp.portPolicies, pp)
}

func (gp *gressPolicy) addIPBlock(ipblockJSON *knet.IPBlock) {
	gp.ipBlock = append(gp.ipBlock, ipblockJSON)
}

func (gp *gressPolicy) getL3MatchFromAddressSet() string {
	var l3Match string
	if len(gp.peerV4AddressSets) == 0 && len(gp.peerV6AddressSets) == 0 {
		l3Match = constructEmptyMatchString()
	} else {
		// List() method on the set will return the sorted strings
		// Hence we'll be constructing the sorted adress set string here
		l3Match = gp.constructMatchString(gp.peerV4AddressSets.List(), gp.peerV6AddressSets.List())
	}
	return l3Match
}

func constructEmptyMatchString() string {
	switch {
	case config.IPv4Mode && config.IPv6Mode:
		return "(ip4 || ip6)"
	case config.IPv6Mode:
		return "ip6"
	default:
		return "ip4"
	}
}

func (gp *gressPolicy) constructMatchString(v4AddressSets, v6AddressSets []string) string {
	var direction, v4MatchStr, v6MatchStr, matchStr string
	if gp.policyType == knet.PolicyTypeIngress {
		direction = "src"
	} else {
		direction = "dst"
	}

	//  At this point there will be address sets in one or both of them.
	//  Contents in both address sets mean dual stack, else one will be empty because we will only populate
	//  entries for enabled stacks
	if len(v4AddressSets) > 0 {
		v4AddressSetStr := strings.Join(v4AddressSets, ", ")
		v4MatchStr = fmt.Sprintf("%s.%s == {%s}", "ip4", direction, v4AddressSetStr)
		matchStr = v4MatchStr
	}
	if len(v6AddressSets) > 0 {
		v6AddressSetStr := strings.Join(v6AddressSets, ", ")
		v6MatchStr = fmt.Sprintf("%s.%s == {%s}", "ip6", direction, v6AddressSetStr)
		matchStr = v6MatchStr
	}
	if len(v4AddressSets) > 0 && len(v6AddressSets) > 0 {
		matchStr = fmt.Sprintf("(%s || %s)", v4MatchStr, v6MatchStr)
	}
	return matchStr
}

func (gp *gressPolicy) sizeOfAddressSet() int {
	return gp.peerV4AddressSets.Len() + gp.peerV6AddressSets.Len()
}

func (gp *gressPolicy) getMatchFromIPBlock(lportMatch, l4Match string) []string {
	var ipBlockMatches []string
	if gp.policyType == knet.PolicyTypeIngress {
		ipBlockMatches = constructIPBlockStringsForACL("src", gp.ipBlock, lportMatch, l4Match)
	} else {
		ipBlockMatches = constructIPBlockStringsForACL("dst", gp.ipBlock, lportMatch, l4Match)
	}
	return ipBlockMatches
}

// addNamespaceAddressSet adds a namespace address set to the gress policy
// if the address set does not exist and returns `true`;  if the address set already exists,
// it returns `false`.
func (gp *gressPolicy) addNamespaceAddressSet(name string) bool {
	v4HashName, v6HashName := addressset.MakeAddressSetHashNames(name)
	v4HashName = "$" + v4HashName
	v6HashName = "$" + v6HashName

	if gp.peerV4AddressSets.Has(v4HashName) || gp.peerV6AddressSets.Has(v6HashName) {
		return false
	}
	if config.IPv4Mode {
		gp.peerV4AddressSets.Insert(v4HashName)
	}
	if config.IPv6Mode {
		gp.peerV6AddressSets.Insert(v6HashName)
	}

	return true
}

// addNamespaceAddressSets adds namespace address sets to the gress policy.
func (gp *gressPolicy) addNamespaceAddressSets(namespaces []interface{}) {
	if len(namespaces) <= 0 {
		return
	}
	for _, nsInterface := range namespaces {
		namespace, ok := nsInterface.(*v1.Namespace)
		if !ok {
			klog.Errorf("Spurious object in addNamespaceAddressSets: %v", nsInterface)
			continue
		}
		gp.addNamespaceAddressSet(namespace.Name)
	}
}

// delNamespaceAddressSet removes a namespace address set from the gress policy
// and returns whether the address set was in the policy or not.
func (gp *gressPolicy) delNamespaceAddressSet(name string) bool {
	v4HashName, v6HashName := addressset.MakeAddressSetHashNames(name)
	v4HashName = "$" + v4HashName
	v6HashName = "$" + v6HashName

	if !gp.peerV4AddressSets.Has(v4HashName) && !gp.peerV6AddressSets.Has(v6HashName) {
		return false
	}
	if config.IPv4Mode {
		gp.peerV4AddressSets.Delete(v4HashName)
	}
	if config.IPv6Mode {
		gp.peerV6AddressSets.Delete(v6HashName)
	}

	return true
}

// buildLocalPodACLs builds the ACLs that implement the gress policy's rules to the
// given Port Group (which should contain all pod logical switch ports selected
// by the parent NetworkPolicy)
func (gp *gressPolicy) buildLocalPodACLs(portGroupName, aclLogging string) []*nbdb.ACL {
	l3Match := gp.getL3MatchFromAddressSet()
	var lportMatch string
	var cidrMatches []string
	if gp.policyType == knet.PolicyTypeIngress {
		lportMatch = fmt.Sprintf("outport == @%s", portGroupName)
	} else {
		lportMatch = fmt.Sprintf("inport == @%s", portGroupName)
	}

	acls := []*nbdb.ACL{}
	if len(gp.portPolicies) == 0 {
		match := fmt.Sprintf("%s && %s", l3Match, lportMatch)
		l4Match := noneMatch

		if len(gp.ipBlock) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatches = gp.getMatchFromIPBlock(lportMatch, l4Match)
			for i, cidrMatch := range cidrMatches {
				acl := gp.buildACLAllow(cidrMatch, l4Match, i+1, aclLogging)
				acls = append(acls, acl)
			}
		}
		// if there are pod/namespace selector, then allow packets from/to that address_set or
		// if the NetworkPolicyPeer is empty, then allow from all sources or to all destinations.
		if gp.sizeOfAddressSet() > 0 || len(gp.ipBlock) == 0 {
			acl := gp.buildACLAllow(match, l4Match, 0, aclLogging)
			acls = append(acls, acl)
		}
	}
	for protocol, ports := range gp.getProtocolPortsMap() {
		l4Match := getL4Match(protocol, ports)
		match := fmt.Sprintf("%s && %s && %s", l3Match, l4Match, lportMatch)
		if len(gp.ipBlock) > 0 {
			// Add ACL allow rule for IPBlock CIDR
			cidrMatches = gp.getMatchFromIPBlock(lportMatch, l4Match)
			for i, cidrMatch := range cidrMatches {
				acl := gp.buildACLAllow(cidrMatch, l4Match, i+1, aclLogging)
				acls = append(acls, acl)
			}
		}
		if gp.sizeOfAddressSet() > 0 || len(gp.ipBlock) == 0 {
			acl := gp.buildACLAllow(match, l4Match, 0, aclLogging)
			acls = append(acls, acl)
		}
	}

	return acls
}

// buildACLAllow builds an allow-related ACL for a given given match
func (gp *gressPolicy) buildACLAllow(match, l4Match string, ipBlockCIDR int, aclLogging string) *nbdb.ACL {
	var direction string
	var options map[string]string
	if gp.policyType == knet.PolicyTypeIngress {
		direction = nbdb.ACLDirectionToLport
	} else {
		direction = nbdb.ACLDirectionFromLport
		options = map[string]string{"apply-after-lb": "true"}
	}
	priority := types.DefaultAllowPriority
	action := nbdb.ACLActionAllowRelated
	aclName := fmt.Sprintf("%s_%s_%v", gp.policyNamespace, gp.policyName, gp.idx)

	// For backward compatibility with existing ACLs, we use "ipblock_cidr=false" for
	// non-ipblock ACLs and "ipblock_cidr=true" for the first ipblock ACL in a policy,
	// but then number them after that.
	var ipBlockCIDRString string
	switch ipBlockCIDR {
	case 0:
		ipBlockCIDRString = "false"
	case 1:
		ipBlockCIDRString = "true"
	default:
		ipBlockCIDRString = strconv.FormatInt(int64(ipBlockCIDR), 10)
	}

	policyTypeNum := fmt.Sprintf(policyTypeNumACLExtIdKey, gp.policyType)
	policyTypeIndex := strconv.FormatInt(int64(gp.idx), 10)

	externalIds := map[string]string{
		l4MatchACLExtIdKey:     l4Match,
		ipBlockCIDRACLExtIdKey: ipBlockCIDRString,
		namespaceACLExtIdKey:   gp.policyNamespace,
		policyACLExtIdKey:      gp.policyName,
		policyTypeACLExtIdKey:  string(gp.policyType),
		policyTypeNum:          policyTypeIndex,
	}
	if l4Match != noneMatch {
		externalIds[l4MatchFusedExtIdKey] = "true"
	}

	acl := libovsdbops.BuildACL(aclName, direction, priority, match, action, types.OvnACLLoggingMeter, getACLLoggingSeverity(aclLogging), aclLogging != "", externalIds, options)
	return acl
}

func constructIPBlockStringsForACL(direction string, ipBlocks []*knet.IPBlock, lportMatch, l4Match string) []string {
	var matchStrings []string
	var matchStr, ipVersion string
	for _, ipBlock := range ipBlocks {
		if utilnet.IsIPv6CIDRString(ipBlock.CIDR) {
			ipVersion = "ip6"
		} else {
			ipVersion = "ip4"
		}
		if len(ipBlock.Except) == 0 {
			matchStr = fmt.Sprintf("%s.%s == %s", ipVersion, direction, ipBlock.CIDR)

		} else {
			matchStr = fmt.Sprintf("%s.%s == %s && %s.%s != {%s}", ipVersion, direction, ipBlock.CIDR,
				ipVersion, direction, strings.Join(ipBlock.Except, ", "))
		}
		if l4Match == noneMatch {
			matchStr = fmt.Sprintf("%s && %s", matchStr, lportMatch)
		} else {
			matchStr = fmt.Sprintf("%s && %s && %s", matchStr, l4Match, lportMatch)
		}
		matchStrings = append(matchStrings, matchStr)
	}
	return matchStrings
}

func (gp *gressPolicy) destroy() error {
	if gp.peerAddressSet != nil {
		if err := gp.peerAddressSet.Destroy(); err != nil {
			return err
		}
	}
	return nil
}

// SVC can be of types 1. clusterIP, 2. NodePort, 3. LoadBalancer,
// or 4.ExternalIP
// TODO adjust for upstream patch when it lands:
// https://bugzilla.redhat.com/show_bug.cgi?id=1908540
func getSvcVips(nbClient client.Client, service *v1.Service) []net.IP {
	ips := make([]net.IP, 0)

	if util.ServiceTypeHasNodePort(service) {
		gatewayRouters, err := gateway.GetOvnGateways(nbClient)
		if err != nil {
			klog.Errorf("Cannot get gateways: %s", err)
		}
		for _, gatewayRouter := range gatewayRouters {
			// VIPs would be the physical IPS of the GRs(IPs of the node) in this case
			physicalIPs, err := gateway.GetGatewayPhysicalIPs(nbClient, gatewayRouter)
			if err != nil {
				klog.Errorf("Unable to get gateway router %s physical ip, error: %v", gatewayRouter, err)
				continue
			}

			for _, physicalIP := range physicalIPs {
				ip := net.ParseIP(physicalIP)
				if ip == nil {
					klog.Errorf("Failed to parse physical IP %q", physicalIP)
					continue
				}
				ips = append(ips, ip)
			}
		}
	}
	if util.ServiceTypeHasClusterIP(service) {
		ipStrs := util.GetClusterIPs(service)
		for _, ipStr := range ipStrs {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				klog.Errorf("Failed to parse cluster IP %q", service.Spec.ClusterIP)
				continue
			}
			ips = append(ips, ip)
		}

		for _, ing := range service.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				klog.V(5).Infof("Adding ingress IPs: %s from Service: %s to VIP set", ing.IP, service.Name)
				ips = append(ips, net.ParseIP(ing.IP))
			}
		}

		if len(service.Spec.ExternalIPs) > 0 {
			for _, extIP := range service.Spec.ExternalIPs {
				ip := net.ParseIP(extIP)
				if ip == nil {
					klog.Errorf("Failed to parse external IP %q", extIP)
					continue
				}
				klog.V(5).Infof("Adding external IP: %s, from Service: %s to VIP set",
					ip, service.Name)
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		klog.V(5).Infof("Service has no VIPs")
		return nil
	}

	return ips
}
