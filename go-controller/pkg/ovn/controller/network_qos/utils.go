package networkqos

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovnkutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func joinMetaNamespaceAndName(namespace, name string, separator ...string) string {
	if namespace == "" {
		return name
	}
	sep := "/"
	if len(separator) > 0 {
		sep = separator[0]
	}
	return namespace + sep + name
}

func GetNetworkQoSAddrSetDbIDs(nqosNamespace, nqosName, ruleIndex, ipBlockIndex, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkQoS, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: joinMetaNamespaceAndName(nqosNamespace, nqosName, ":"),
			// rule index is the unique id for address set within given objectName
			libovsdbops.RuleIndex:       ruleIndex,
			libovsdbops.IpBlockIndexKey: ipBlockIndex,
		})
}

func getPodAddresses(pod *corev1.Pod, networkInfo ovnkutil.NetInfo) ([]string, error) {
	// check annotation "k8s.ovn.org/pod-networks" before calling GetPodIPsOfNetwork,
	// as it's no easy to check if the error is caused by missing annotation, while
	// we don't want to return error for such case as it will trigger retry
	_, ok := pod.Annotations[ovnkutil.OvnPodAnnotationName]
	if !ok {
		// pod hasn't been annotated yet, return nil to avoid retry
		return nil, nil
	}
	ips, err := ovnkutil.GetPodIPsOfNetwork(pod, networkInfo)
	if err != nil {
		return nil, err
	}
	addresses := []string{}
	for _, ip := range ips {
		addresses = append(addresses, ip.String())
	}
	return addresses, nil
}

func generateNetworkQoSMatch(qosState *networkQoSState, rule *GressRule, ipv4Enabled, ipv6Enabled bool) string {
	match := addressSetToMatchString(qosState.SrcAddrSet, trafficDirSource, ipv4Enabled, ipv6Enabled)

	classiferMatchString := rule.Classifier.ToQosMatchString(ipv4Enabled, ipv6Enabled)
	if classiferMatchString != "" {
		match = match + " && " + classiferMatchString
	}

	return match
}

func addressSetToMatchString(addrset addressset.AddressSet, dir trafficDirection, ipv4Enabled, ipv6Enabled bool) string {
	ipv4AddrSetHashName, ipv6AddrSetHashName := addrset.GetASHashNames()
	output := ""
	switch {
	case ipv4Enabled && ipv6Enabled:
		output = fmt.Sprintf("(ip4.%s == {$%s} || ip6.%s == {$%s})", dir, ipv4AddrSetHashName, dir, ipv6AddrSetHashName)
	case ipv4Enabled:
		output = fmt.Sprintf("ip4.%s == {$%s}", dir, ipv4AddrSetHashName)
	case ipv6Enabled:
		output = fmt.Sprintf("ip6.%s == {$%s}", dir, ipv6AddrSetHashName)
	}
	return output
}

func getNamespaceAddressSet(addressSetFactory addressset.AddressSetFactory, controllerName, namespace string) (addressset.AddressSet, error) {
	dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: namespace,
	})
	return addressSetFactory.EnsureAddressSet(dbIDs)
}
