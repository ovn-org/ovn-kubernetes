package ovn

import (
	"context"
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/dhcp"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func (oc *DefaultNetworkController) addDHCPOptions(pod *kapi.Pod, lsp *nbdb.LogicalSwitchPort) error {
	ipPoolName, err := oc.getIPPoolName(pod)
	if err != nil {
		return err
	}
	var switchSubnets []*net.IPNet
	if switchSubnets = oc.lsManager.GetSwitchSubnets(ipPoolName); switchSubnets == nil {
		return fmt.Errorf("cannot retrieve subnet for assigning gateway routes switch: %s", ipPoolName)
	}
	// Fake router to delegate on proxy arp mechanism
	router := arpProxyAddress
	cidr := switchSubnets[0].String()
	hostname := pod.Labels[kubevirt.VMLabel]
	dhcpOptions, err := dhcp.ComposeOptionsWithKubeDNS(oc.client, hostname, cidr, router)
	if err != nil {
		return fmt.Errorf("failed composing DHCP options: %v", err)
	}
	dhcpOptions.ExternalIDs = map[string]string{
		"namespace":      pod.Namespace,
		kubevirt.VMLabel: pod.Labels[kubevirt.VMLabel],
	}
	err = libovsdbops.CreateOrUpdateDhcpv4Options(oc.nbClient, lsp, dhcpOptions)
	if err != nil {
		return fmt.Errorf("failed adding ovn operations to add DHCP v4 options: %v", err)
	}
	return nil
}

func matchesKubevirtPod(pod *kapi.Pod, externalIDs map[string]string) bool {
	return len(externalIDs) > 1 && externalIDs["namespace"] == pod.Namespace && externalIDs[kubevirt.VMLabel] == pod.Labels[kubevirt.VMLabel]
}

func (oc *DefaultNetworkController) deleteDHCPOptions(pod *kapi.Pod) error {
	predicate := func(item *nbdb.DHCPOptions) bool {
		return matchesKubevirtPod(pod, item.ExternalIDs)
	}
	return libovsdbops.DeleteDHCPOptionsWithPredicate(oc.nbClient, predicate)
}

func (oc *DefaultNetworkController) deletePodEnrouting(pod *kapi.Pod) error {
	routePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return matchesKubevirtPod(pod, item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(oc.nbClient, types.OVNClusterRouter, routePredicate); err != nil {
		return err
	}
	policyPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return matchesKubevirtPod(pod, item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(oc.nbClient, types.OVNClusterRouter, policyPredicate); err != nil {
		return err
	}
	return nil
}

func (oc *DefaultNetworkController) enroutePodAddressesToNode(pod *kapi.Pod) error {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, "default")
	if err != nil {
		return err
	}

	nodeGwAddress, err := oc.lrpAddress(types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + pod.Spec.NodeName)
	if err != nil {
		return err
	}
	for _, podIP := range podAnnotation.IPs {
		podAddress := podIP.IP.String()
		// Add a reroute policy to route VM n/s traffic to the node where the VM
		// is running
		egressPolicy := nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip4.src == %s", podAddress),
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			Nexthops: []string{nodeGwAddress},
			Priority: 1,
			ExternalIDs: map[string]string{
				"namespace":      pod.Namespace,
				kubevirt.VMLabel: pod.Labels[kubevirt.VMLabel],
			},
		}
		if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(oc.nbClient, types.OVNClusterRouter, &egressPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == egressPolicy.Priority && item.Match == egressPolicy.Match && item.Action == egressPolicy.Action
		}); err != nil {
			return err
		}

		// Add a policy to force send an ARP to discover VMs MAC and send
		// directly to it since there is no more routers in the middle
		outputPort := types.RouterToSwitchPrefix + pod.Spec.NodeName
		ingressRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   podAddress,
			Nexthop:    podAddress,
			Policy:     &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			OutputPort: &outputPort,
			ExternalIDs: map[string]string{
				"namespace":      pod.Namespace,
				kubevirt.VMLabel: pod.Labels[kubevirt.VMLabel],
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(oc.nbClient, types.OVNClusterRouter, &ingressRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == ingressRoute.IPPrefix && item.Nexthop == ingressRoute.Nexthop && item.Policy != nil && *item.Policy == *ingressRoute.Policy
			return matches
		}); err != nil {
			return err
		}
	}
	return nil
}

func (oc *DefaultNetworkController) enrouteVirtualMachine(pod *kapi.Pod) error {
	targetNode := pod.Labels[kubevirt.NodeNameLabel]
	targetStartTimestamp := pod.Annotations[kubevirt.MigrationTargetStartTimestampAnnotation]
	klog.Infof("deleteme, enrouteVirtualMachine: %s", targetStartTimestamp)
	// No live migration or target node was reached || qemu is already ready
	if targetNode == pod.Spec.NodeName || targetStartTimestamp != "" {
		if err := oc.enroutePodAddressesToNode(pod); err != nil {
			return fmt.Errorf("failed enroutePodAddressesToNode for  %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

func (oc *DefaultNetworkController) lrpAddress(lrpName string) (string, error) {
	lrp := &nbdb.LogicalRouterPort{
		Name: lrpName,
	}

	lrp, err := libovsdbops.GetLogicalRouterPort(oc.nbClient, lrp)
	if err != nil {
		return "", err
	}
	lrpIP, _, err := net.ParseCIDR(lrp.Networks[0])
	if err != nil {
		return "", err
	}
	address := lrpIP.String()
	if address == "" {
		return "", fmt.Errorf("missing logical router port address")
	}
	return address, nil
}

func (bnc *BaseNetworkController) syncKubevirtPodIPConfig(pod *kapi.Pod) error {
	vmIPConfig, err := kubevirt.FindIPConfigByVMLabel(bnc.client, pod)
	if err != nil {
		return err
	}
	pod.Annotations[kubevirt.IPPoolNameAnnotation] = vmIPConfig.PoolName
	if vmIPConfig.Annotation != "" {
		pod.Annotations[util.OvnPodAnnotationName] = vmIPConfig.Annotation
	}
	if _, err := bnc.client.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (bnc *BaseNetworkController) getIPPoolName(pod *kapi.Pod) (string, error) {
	ipPoolName, ok := pod.Annotations[kubevirt.IPPoolNameAnnotation]
	if !ok {
		return bnc.getExpectedSwitchName(pod)
	}
	return ipPoolName, nil
}
