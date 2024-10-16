package networkqos

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// networkQoSState is the cache that keeps the state of a single
// network qos in the cluster with namespace+name being unique
type networkQoSState struct {
	sync.RWMutex
	// name of the network qos
	name      string
	namespace string

	networkAttachmentName string

	SrcAddrSet  addressset.AddressSet
	Pods        sync.Map // pods name -> ips in the srcAddrSet
	SwitchRefs  sync.Map // switch name -> list of source pods
	PodSelector labels.Selector

	// egressRules stores the objects needed to track .Spec.Egress changes
	EgressRules []*GressRule
}

func (nqosState *networkQoSState) getObjectNameKey() string {
	return joinMetaNamespaceAndName(nqosState.namespace, nqosState.name, ":")
}

func (nqosState *networkQoSState) getDbObjectIDs(controller string, ruleIndex int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.NetworkQoS, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: nqosState.getObjectNameKey(),
		libovsdbops.RuleIndex:     fmt.Sprintf("%d", ruleIndex),
	})
}

func (nqosState *networkQoSState) emptyPodSelector() bool {
	return nqosState.PodSelector == nil || nqosState.PodSelector.Empty()
}

func (nqosState *networkQoSState) initAddressSets(addressSetFactory addressset.AddressSetFactory, controllerName string) error {
	var err error
	// init source address set
	if nqosState.emptyPodSelector() {
		nqosState.SrcAddrSet, err = getNamespaceAddressSet(addressSetFactory, controllerName, nqosState.namespace)
	} else {
		nqosState.SrcAddrSet, err = addressSetFactory.EnsureAddressSet(GetNetworkQoSAddrSetDbIDs(nqosState.namespace, nqosState.name, "src", "0", controllerName))
	}
	if err != nil {
		return fmt.Errorf("failed to init source address set for %s/%s: %w", nqosState.namespace, nqosState.name, err)
	}

	// ensure destination address sets
	for ruleIndex, rule := range nqosState.EgressRules {
		for destIndex, dest := range rule.Classifier.Destinations {
			if dest.NamespaceSelector == nil && dest.PodSelector == nil {
				continue
			}
			dest.DestAddrSet, err = addressSetFactory.EnsureAddressSet(GetNetworkQoSAddrSetDbIDs(nqosState.namespace, nqosState.name, strconv.Itoa(ruleIndex), strconv.Itoa(destIndex), controllerName))
			if err != nil {
				return fmt.Errorf("failed to init destination address set for %s/%s: %w", nqosState.namespace, nqosState.name, err)
			}
		}
	}
	return nil
}

func (nqosState *networkQoSState) matchSourceSelector(pod *v1.Pod) bool {
	if pod.Namespace != nqosState.namespace {
		return false
	}
	if nqosState.PodSelector == nil {
		return true
	}
	return nqosState.PodSelector.Matches(labels.Set(pod.Labels))
}

func (nqosState *networkQoSState) configureSourcePod(ctrl *Controller, pod *v1.Pod, addresses []string) error {
	fullPodName := joinMetaNamespaceAndName(pod.Namespace, pod.Name)
	if nqosState.PodSelector != nil {
		// if PodSelector is nil, use namespace's address set, so unnecessary to add ip here
		if err := nqosState.SrcAddrSet.AddAddresses(addresses); err != nil {
			return fmt.Errorf("failed to add addresses {%s} to address set %s for NetworkQoS %s/%s: %v", strings.Join(addresses, ","), nqosState.SrcAddrSet.GetName(), nqosState.namespace, nqosState.name, err)
		}
		nqosState.Pods.Store(fullPodName, addresses)
		klog.V(4).Infof("Successfully added address (%s) of pod %s to address set %s", strings.Join(addresses, ","), fullPodName, nqosState.SrcAddrSet.GetName())
	}
	// get switch name
	switchName := ctrl.getLogicalSwitchName(pod.Spec.NodeName)
	if switchName == "" {
		return fmt.Errorf("failed to get logical switch name for node %s, topology %s", pod.Spec.NodeName, ctrl.TopologyType())
	}

	podList := []string{}
	val, loaded := nqosState.SwitchRefs.Load(switchName)
	if loaded {
		podList = val.([]string)
	}

	if !loaded {
		klog.V(4).Infof("Adding NetworkQoS %s/%s to logical switch %s", nqosState.namespace, nqosState.name, switchName)
		start := time.Now()
		if err := ctrl.addQoSToLogicalSwitch(nqosState, switchName); err != nil {
			return err
		}
		recordOvnOperationDuration("add", time.Since(start).Milliseconds())
	}

	podList = append(podList, fullPodName)
	nqosState.SwitchRefs.Store(switchName, podList)
	return nil
}

func (nqosState *networkQoSState) removePodFromSource(ctrl *Controller, fullPodName string, addresses []string) error {
	if len(addresses) == 0 {
		// if no addresses is provided, try lookup in cache
		if val, ok := nqosState.Pods.Load(fullPodName); ok {
			addresses = val.([]string)
		}
	}
	if len(addresses) > 0 && nqosState.PodSelector != nil {
		// remove pod from non-namespace-scope source address set
		if err := nqosState.SrcAddrSet.DeleteAddresses(addresses); err != nil {
			return fmt.Errorf("failed to delete addresses (%s) from address set %s: %v", strings.Join(addresses, ","), nqosState.SrcAddrSet.GetName(), err)
		}
	}
	nqosState.Pods.Delete(fullPodName)
	return nqosState.removeZeroQoSNodes(ctrl, fullPodName)
}

func (nqosState *networkQoSState) removeZeroQoSNodes(ctrl *Controller, fullPodName string) error {
	zeroQoSSwitches := []string{}
	// since node is unknown when pod is delete, iterate the SwitchRefs to remove the pod
	nqosState.SwitchRefs.Range(func(key, val any) bool {
		switchName := key.(string)
		podList := val.([]string)
		podList = slices.DeleteFunc(podList, func(s string) bool {
			return s == fullPodName
		})
		if len(podList) == 0 {
			zeroQoSSwitches = append(zeroQoSSwitches, switchName)
		} else {
			nqosState.SwitchRefs.Store(switchName, podList)
		}
		return true
	})
	// unbind qos from L3 logical switches where doesn't have source pods any more
	if len(zeroQoSSwitches) > 0 && ctrl.TopologyType() == types.Layer3Topology {
		start := time.Now()
		if err := ctrl.removeQoSFromLogicalSwitches(nqosState, zeroQoSSwitches); err != nil {
			return err
		}
		recordOvnOperationDuration("remove", time.Since(start).Milliseconds())
		for _, lsw := range zeroQoSSwitches {
			nqosState.SwitchRefs.Delete(lsw)
		}
	}
	return nil
}

func (nqosState *networkQoSState) getAddressSetHashNames() []string {
	addrsetNames := []string{}
	if nqosState.SrcAddrSet != nil {
		v4Hash, v6Hash := nqosState.SrcAddrSet.GetASHashNames()
		addrsetNames = append(addrsetNames, v4Hash, v6Hash)
	}
	for _, rule := range nqosState.EgressRules {
		for _, dest := range rule.Classifier.Destinations {
			if dest.DestAddrSet != nil {
				v4Hash, v6Hash := dest.DestAddrSet.GetASHashNames()
				addrsetNames = append(addrsetNames, v4Hash, v6Hash)
			}
		}
	}
	return addrsetNames
}

type GressRule struct {
	Priority   int
	Dscp       int
	Classifier *Classifier

	// bandwitdh
	Rate  *int
	Burst *int
}

type protocol string

func (p protocol) IsValid() bool {
	switch p.String() {
	case "tcp", "udp", "sctp":
		return true
	default:
		return false
	}
}

func (p protocol) String() string {
	return strings.ToLower(string(p))
}

type trafficDirection string

const (
	trafficDirSource trafficDirection = "src"
	trafficDirDest   trafficDirection = "dst"
)

type Classifier struct {
	Destinations []*Destination

	// port
	Protocol protocol
	Port     *int
}

// ToQosMatchString generates dest and protocol/port part of QoS match string, based on
// Classifier's destinations, protocol and port fields, example:
// (ip4.dst == $addr_set_name || (ip4.dst == 128.116.0.0/17 && ip4.dst != {128.116.0.0,128.116.0.255})) && tcp && tcp.dst == 8080
// Multiple destinations will be connected by "||".
// See https://github.com/ovn-org/ovn/blob/2bdf1129c19d5bd2cd58a3ddcb6e2e7254b05054/ovn-nb.xml#L2942-L3025 for details
func (c *Classifier) ToQosMatchString(ipv4Enabled, ipv6Enabled bool) string {
	if c == nil {
		return ""
	}
	destMatchStrings := []string{}
	for _, dest := range c.Destinations {
		match := "ip4.dst == 0.0.0.0/0 || ip6.dst == ::/0"
		if dest.DestAddrSet != nil {
			match = addressSetToMatchString(dest.DestAddrSet, trafficDirDest, ipv4Enabled, ipv6Enabled)
		} else if dest.IpBlock != nil && dest.IpBlock.CIDR != "" {
			ipVersion := "ip4"
			if utilnet.IsIPv6CIDRString(dest.IpBlock.CIDR) {
				ipVersion = "ip6"
			}
			if len(dest.IpBlock.Except) == 0 {
				match = fmt.Sprintf("%s.%s == %s", ipVersion, trafficDirDest, dest.IpBlock.CIDR)
			} else {
				match = fmt.Sprintf("%s.%s == %s && %s.%s != {%s}", ipVersion, trafficDirDest, dest.IpBlock.CIDR, ipVersion, trafficDirDest, strings.Join(dest.IpBlock.Except, ","))
			}
		}
		destMatchStrings = append(destMatchStrings, match)
	}

	output := ""
	if len(destMatchStrings) == 1 {
		output = destMatchStrings[0]
	} else {
		for index, str := range destMatchStrings {
			if index > 0 {
				output += " || "
			}
			if strings.Contains(str, "||") || strings.Contains(str, "&&") {
				output = output + fmt.Sprintf("(%s)", str)
			} else {
				output = output + str
			}
		}
	}
	if strings.Contains(output, "||") {
		output = fmt.Sprintf("(%s)", output)
	}
	if c.Protocol != "" {
		if c.Port != nil && *c.Port > 0 {
			match := fmt.Sprintf("%s && %s.dst == %d", c.Protocol.String(), c.Protocol.String(), *c.Port)
			if output != "" {
				output = fmt.Sprintf("%s && %s", output, match)
			} else {
				output = match
			}
		} else {
			if output != "" {
				output = fmt.Sprintf("%s && %s", output, c.Protocol.String())
			} else {
				output = c.Protocol.String()
			}
		}
	}
	return output
}

type Destination struct {
	IpBlock *knet.IPBlock

	DestAddrSet       addressset.AddressSet
	PodSelector       labels.Selector
	Pods              sync.Map // pods name -> ips in the destAddrSet
	NamespaceSelector labels.Selector
}

func (dest *Destination) matchNamespace(podNs *v1.Namespace, qosNamespace string) bool {
	if dest.NamespaceSelector == nil {
		return podNs.Name == qosNamespace
	}
	return dest.NamespaceSelector.Matches(labels.Set(podNs.Labels))
}

func (dest *Destination) matchPod(podNs *v1.Namespace, pod *v1.Pod, qosNamespace string) bool {
	switch {
	case dest.NamespaceSelector != nil && dest.PodSelector != nil:
		return dest.NamespaceSelector.Matches(labels.Set(podNs.Labels)) && dest.PodSelector.Matches(labels.Set(pod.Labels))
	case dest.NamespaceSelector == nil && dest.PodSelector != nil:
		return pod.Namespace == qosNamespace && dest.PodSelector.Matches(labels.Set(pod.Labels))
	case dest.NamespaceSelector != nil && dest.PodSelector == nil:
		return dest.NamespaceSelector.Matches(labels.Set(podNs.Labels))
	default: //dest.NamespaceSelector == nil && dest.PodSelector == nil:
		return false
	}
}

func (dest *Destination) addPod(podNamespace, podName string, addresses []string) error {
	if err := dest.DestAddrSet.AddAddresses(addresses); err != nil {
		return err
	}
	// add pod to map
	dest.Pods.Store(joinMetaNamespaceAndName(podNamespace, podName), addresses)
	return nil
}

func (dest *Destination) removePod(fullPodName string, addresses []string) error {
	if len(addresses) == 0 {
		val, ok := dest.Pods.Load(fullPodName)
		if ok && val != nil {
			addresses = val.([]string)
		}
	}
	if err := dest.DestAddrSet.DeleteAddresses(addresses); err != nil {
		return fmt.Errorf("failed to remove addresses (%s): %v", strings.Join(addresses, ","), err)
	}
	dest.Pods.Delete(fullPodName)
	return nil
}

func (dest *Destination) removePodsInNamespace(namespace string) error {
	var err error
	// check for pods in the namespace being cleared
	dest.Pods.Range(func(key, value any) bool {
		fullPodName := key.(string)
		nameParts := strings.Split(fullPodName, "/")
		if nameParts[0] != namespace {
			// pod's namespace doesn't match
			return true
		}
		err = dest.removePod(fullPodName, nil)
		return err == nil
	})
	return err
}

func (dest *Destination) addPodsInNamespace(ctrl *Controller, namespace string) error {
	podSelector := labels.Everything()
	if dest.PodSelector != nil {
		podSelector = dest.PodSelector
	}
	pods, err := ctrl.nqosPodLister.Pods(namespace).List(podSelector)
	if err != nil {
		if errors.IsNotFound(err) || len(pods) == 0 {
			return nil
		}
		return fmt.Errorf("failed to look up pods in ns %s: %v", namespace, err)
	}
	klog.V(5).Infof("Found %d pods in namespace %s by selector %s", len(pods), namespace, podSelector.String())
	for _, pod := range pods {
		podAddresses, err := getPodAddresses(pod, ctrl.NetInfo)
		if err != nil {
			return fmt.Errorf("failed to parse IPs for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		if err := dest.addPod(pod.Namespace, pod.Name, podAddresses); err != nil {
			return fmt.Errorf("failed to add addresses {%s} to address set %s: %v", strings.Join(podAddresses, ","), dest.DestAddrSet.GetName(), err)
		}
	}
	return nil
}
