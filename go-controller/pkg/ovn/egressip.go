package ovn

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	defaultNoRereoutePriority = "101"
	egressIPRereoutePriority  = "100"
)

type modeEgressIP interface {
	addPodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error
	deletePodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error
	needsRetry(pod *kapi.Pod) bool
}

func (oc *Controller) addEgressIP(eIP *egressipv1.EgressIP) error {
	// If the status is set at this point, then we know it's valid from syncEgressIP and we have no assignment to do.
	// Just initialize all watchers (which should not re-create any already existing items in the OVN DB)
	if len(eIP.Status.Items) == 0 {
		if err := oc.assignEgressIPs(eIP); err != nil {
			return fmt.Errorf("unable to assign egress IP: %s, error: %v", eIP.Name, err)
		}
	}

	oc.egressIPNamespaceHandlerMutex.Lock()
	defer oc.egressIPNamespaceHandlerMutex.Unlock()

	sel, err := metav1.LabelSelectorAsSelector(&eIP.Spec.NamespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid namespaceSelector on EgressIP %s: %v", eIP.Name, err)
	}
	h := oc.watchFactory.AddFilteredNamespaceHandler("", sel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				klog.V(5).Infof("EgressIP: %s has matched on namespace: %s", eIP.Name, namespace.Name)
				if err := oc.addNamespaceEgressIP(eIP, namespace); err != nil {
					klog.Errorf("error: unable to add namespace handler for EgressIP: %s, err: %v", eIP.Name, err)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {},
			DeleteFunc: func(obj interface{}) {
				namespace := obj.(*kapi.Namespace)
				klog.V(5).Infof("EgressIP: %s stopped matching on namespace: %s", eIP.Name, namespace.Name)
				if err := oc.deleteNamespaceEgressIP(eIP, namespace); err != nil {
					klog.Errorf("error: unable to delete namespace handler for EgressIP: %s, err: %v", eIP.Name, err)
				}
			},
		}, nil)
	oc.egressIPNamespaceHandlerCache[getEgressIPKey(eIP)] = *h
	return nil
}

func (oc *Controller) deleteEgressIP(eIP *egressipv1.EgressIP) error {
	oc.releaseEgressIPs(eIP)

	oc.egressIPNamespaceHandlerMutex.Lock()
	defer oc.egressIPNamespaceHandlerMutex.Unlock()
	delete(oc.egressIPNamespaceHandlerCache, getEgressIPKey(eIP))

	oc.egressIPPodHandlerMutex.Lock()
	defer oc.egressIPPodHandlerMutex.Unlock()
	delete(oc.egressIPPodHandlerCache, getEgressIPKey(eIP))

	namespaces, err := oc.kube.GetNamespaces(eIP.Spec.NamespaceSelector)
	if err != nil {
		return err
	}
	for _, namespace := range namespaces.Items {
		if err := oc.deleteNamespacePodsEgressIP(eIP, &namespace); err != nil {
			return err
		}
	}
	return nil
}

func (oc *Controller) syncEgressIPs(eIPs []interface{}) {
	oc.eIPAllocatorMutex.Lock()
	defer oc.eIPAllocatorMutex.Unlock()
	for _, eIP := range eIPs {
		eIP, ok := eIP.(*egressipv1.EgressIP)
		if !ok {
			klog.Errorf("Spurious object in syncEgressIPs: %v", eIP)
			continue
		}
		var validAssignment bool
		for _, eIPStatus := range eIP.Status.Items {
			validAssignment = false
			eNode, exists := oc.eIPAllocator[eIPStatus.Node]
			if !exists {
				klog.Errorf("Allocator error: EgressIP: %s claims to have an allocation on a node which is unassignable for egress IP: %s", eIP.Name, eIPStatus.Node)
				break
			}
			if eNode.tainted {
				klog.Errorf("Allocator error: EgressIP: %s claims multiple egress IPs on same node: %s, will attempt rebalancing", eIP.Name, eIPStatus.Node)
				break
			}
			ip := net.ParseIP(eIPStatus.EgressIP)
			if ip == nil {
				klog.Errorf("Allocator error: EgressIP allocation contains unparsable IP address: %s", eIPStatus.EgressIP)
				break
			}
			if utilnet.IsIPv6(ip) && eNode.v6Subnet != nil {
				if !eNode.v6Subnet.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.v6Subnet.String())
					break
				}
			} else if !utilnet.IsIPv6(ip) && eNode.v4Subnet != nil {
				if !eNode.v4Subnet.Contains(ip) {
					klog.Errorf("Allocator error: EgressIP allocation: %s on subnet: %s which cannot host it", ip.String(), eNode.v4Subnet.String())
					break
				}
			} else {
				klog.Errorf("Allocator error: EgressIP allocation on node: %s which does not support its IP protocol version", eIPStatus.Node)
				break
			}
			validAssignment = true
			eNode.tainted = true
		}
		// In the unlikely event that any status has been misallocated previously:
		// unassign that by updating the entire status and re-allocate properly in addEgressIP
		if !validAssignment {
			eIP.Status = egressipv1.EgressIPStatus{
				Items: []egressipv1.EgressIPStatusItem{},
			}
			if err := oc.updateEgressIPWithRetry(eIP); err != nil {
				klog.Error(err)
				continue
			}
			namespaces, err := oc.kube.GetNamespaces(eIP.Spec.NamespaceSelector)
			if err != nil {
				klog.Errorf("Unable to list namespaces matched by EgressIP: %s, err: %v", getEgressIPKey(eIP), err)
				continue
			}
			for _, namespace := range namespaces.Items {
				if err := oc.deleteNamespacePodsEgressIP(eIP, &namespace); err != nil {
					klog.Error(err)
				}
			}
		}
		for _, eNode := range oc.eIPAllocator {
			eNode.tainted = false
		}
	}
}

func (oc *Controller) updateEgressIPWithRetry(eIP *egressipv1.EgressIP) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return oc.kube.UpdateEgressIP(eIP)
	})
}

func (oc *Controller) addNamespaceEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	oc.egressIPPodHandlerMutex.Lock()
	defer oc.egressIPPodHandlerMutex.Unlock()

	sel, err := metav1.LabelSelectorAsSelector(&eIP.Spec.PodSelector)
	if err != nil {
		return fmt.Errorf("invalid podSelector on EgressIP %s: %v", eIP.Name, err)
	}
	h := oc.watchFactory.AddFilteredPodHandler(namespace.Name, sel,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*kapi.Pod)
				klog.V(5).Infof("EgressIP: %s has matched on pod: %s in namespace: %s", eIP.Name, pod.Name, namespace.Name)
				if err := oc.modeEgressIP.addPodEgressIP(eIP, pod); err != nil {
					klog.Errorf("Unable to add pod: %s/%s to EgressIP: %s, err: %v", pod.Namespace, pod.Name, eIP.Name, err)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				newPod := newObj.(*kapi.Pod)
				// FYI: the only pod update we care about here is the pod being assigned an IP, which
				// it didn't have when we received the ADD. If the label is changed and it stops matching:
				// this watcher receives a delete.
				if oc.modeEgressIP.needsRetry(newPod) {
					klog.V(5).Infof("EgressIP: %s update for pod: %s in namespace: %s", eIP.Name, newPod.Name, namespace.Name)
					if err := oc.modeEgressIP.addPodEgressIP(eIP, newPod); err != nil {
						klog.Errorf("Unable to add pod: %s/%s to EgressIP: %s, err: %v", newPod.Namespace, newPod.Name, eIP.Name, err)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*kapi.Pod)
				// FYI: we can be in a situation where we processed a pod ADD for which there was no IP
				// address assigned. If the pod is deleted before the IP address is assigned,
				// we should not process that delete (as nothing exists in OVN for it)
				klog.V(5).Infof("EgressIP: %s has stopped matching on pod: %s in namespace: %s, needs delete: %v", eIP.Name, pod.Name, namespace.Name, !oc.modeEgressIP.needsRetry(pod))
				if !oc.modeEgressIP.needsRetry(pod) {
					if err := oc.modeEgressIP.deletePodEgressIP(eIP, pod); err != nil {
						klog.Errorf("Unable to delete pod: %s/%s to EgressIP: %s, err: %v", pod.Namespace, pod.Name, eIP.Name, err)
					}
				}
			},
		}, nil)
	oc.egressIPPodHandlerCache[getEgressIPKey(eIP)] = *h
	return nil
}

func (oc *Controller) deleteNamespaceEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	oc.egressIPPodHandlerMutex.Lock()
	defer oc.egressIPPodHandlerMutex.Unlock()
	delete(oc.egressIPPodHandlerCache, getEgressIPKey(eIP))
	if err := oc.deleteNamespacePodsEgressIP(eIP, namespace); err != nil {
		return err
	}
	return nil
}

func (oc *Controller) deleteNamespacePodsEgressIP(eIP *egressipv1.EgressIP, namespace *kapi.Namespace) error {
	pods, err := oc.kube.GetPods(namespace.Name, eIP.Spec.PodSelector)
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if !oc.modeEgressIP.needsRetry(&pod) {
			if err := oc.modeEgressIP.deletePodEgressIP(eIP, &pod); err != nil {
				return err
			}
		}
	}
	return nil
}

func (oc *Controller) assignEgressIPs(eIP *egressipv1.EgressIP) error {
	assignments := []egressipv1.EgressIPStatusItem{}
	oc.eIPAllocatorMutex.Lock()
	defer func() {
		eIP.Status.Items = assignments
		oc.eIPAllocatorMutex.Unlock()
	}()
	if len(oc.eIPAllocator) == 0 {
		oc.egressAssignmentRetry.Store(eIP.Name, true)
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: eIP.Name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "no assignable nodes for EgressIP: %s, please tag at least one node with label: %s", eIP.Name, util.GetNodeEgressLabel())
		return fmt.Errorf("no assignable nodes")
	}
	eNodes, existingAllocations := oc.getSortedEgressData()
	klog.V(5).Infof("Current assignments are: %+v", existingAllocations)
	for _, egressIP := range eIP.Spec.EgressIPs {
		klog.V(5).Infof("Will attempt assignment for egress IP: %s", egressIP)
		eIPC := net.ParseIP(egressIP)
		if eIPC == nil {
			eIPRef := kapi.ObjectReference{
				Kind: "EgressIP",
				Name: eIP.Name,
			}
			oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "InvalidEgressIP", "egress IP: %s for object EgressIP: %s is not a valid IP address", egressIP, eIP.Name)
			klog.Errorf("Unable to parse provided EgressIP: %s, invalid", egressIP)
			continue
		}
		for i := 0; i < len(eNodes); i++ {
			klog.V(5).Infof("Attempting assignment on egress node: %+v", eNodes[i])
			if _, exists := existingAllocations[eIPC.String()]; exists {
				klog.V(5).Infof("EgressIP: %s is already allocated, skipping", eIPC.String())
				break
			}
			if eNodes[i].tainted {
				klog.V(5).Infof("Node: %s is already in use by another egress IP for this EgressIP: %s, trying another node", eNodes[i].name, eIP.Name)
				continue
			}
			if (utilnet.IsIPv6(eIPC) && eNodes[i].v6Subnet != nil && eNodes[i].v6Subnet.Contains(eIPC)) ||
				(!utilnet.IsIPv6(eIPC) && eNodes[i].v4Subnet != nil && eNodes[i].v4Subnet.Contains(eIPC)) {
				eNodes[i].tainted, oc.eIPAllocator[eNodes[i].name].allocations[eIPC.String()] = true, true
				assignments = append(assignments, egressipv1.EgressIPStatusItem{
					EgressIP: eIPC.String(),
					Node:     eNodes[i].name,
				})
				klog.V(5).Infof("Successful assignment of egress IP: %s on node: %+v", egressIP, eNodes[i])
				break
			}
		}
	}
	if len(assignments) == 0 {
		oc.egressAssignmentRetry.Store(eIP.Name, true)
		eIPRef := kapi.ObjectReference{
			Kind: "EgressIP",
			Name: eIP.Name,
		}
		oc.recorder.Eventf(&eIPRef, kapi.EventTypeWarning, "NoMatchingNodeFound", "No matching nodes found, which can host any of the egress IPs: %v for object EgressIP: %s", eIP.Spec.EgressIPs, eIP.Name)
		return fmt.Errorf("no matching host found")
	}
	return nil
}

func (oc *Controller) releaseEgressIPs(eIP *egressipv1.EgressIP) {
	oc.eIPAllocatorMutex.Lock()
	defer oc.eIPAllocatorMutex.Unlock()
	for _, status := range eIP.Status.Items {
		klog.V(5).Infof("Releasing egress IP assignment: %s", status.EgressIP)
		if node, exists := oc.eIPAllocator[status.Node]; exists {
			delete(node.allocations, status.EgressIP)
		}
		klog.V(5).Infof("Remaining allocations on node are: %+v", oc.eIPAllocator[status.Node])
	}
}

func (oc *Controller) getSortedEgressData() ([]eNode, map[string]bool) {
	nodes := []eNode{}
	allAllocations := make(map[string]bool)
	for _, eNode := range oc.eIPAllocator {
		nodes = append(nodes, *eNode)
		for ip := range eNode.allocations {
			allAllocations[ip] = true
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		return len(nodes[i].allocations) < len(nodes[j].allocations)
	})
	return nodes, allAllocations
}

func getEgressIPKey(eIP *egressipv1.EgressIP) string {
	return eIP.Name
}

func getPodKey(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}

type egressIPMode struct {
	ovnNBClient    goovn.Client
	podRetry       sync.Map
	gatewayIPCache sync.Map
}

type gatewayRouter struct {
	ipV4 net.IP
	ipV6 net.IP
}

func (e *egressIPMode) getGatewayRouterJoinIP(node string, wantsIPv6 bool) (net.IP, error) {
	if item, exists := e.gatewayIPCache.Load(node); exists {
		if gatewayRouter, ok := item.(gatewayRouter); ok {
			if wantsIPv6 && gatewayRouter.ipV6 != nil {
				return gatewayRouter.ipV6, nil
			} else if !wantsIPv6 && gatewayRouter.ipV4 != nil {
				return gatewayRouter.ipV4, nil
			}
			return nil, fmt.Errorf("node does not have a gateway router IP corresponding to the IP version requested")
		}
		return nil, fmt.Errorf("unable to cast node: %s gatewayIP cache item to correct type", node)
	}
	gatewayRouter := gatewayRouter{}
	err := utilwait.ExponentialBackoff(retry.DefaultRetry, func() (bool, error) {
		grIPv4, grIPv6, err := util.GetNodeLogicalRouterIP(node)
		if err != nil {
			return false, nil
		}
		if grIPv4 != nil {
			gatewayRouter.ipV4 = grIPv4
		}
		if grIPv6 != nil {
			gatewayRouter.ipV6 = grIPv6
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	e.gatewayIPCache.Store(node, gatewayRouter)
	if wantsIPv6 && gatewayRouter.ipV6 != nil {
		return gatewayRouter.ipV6, nil
	} else if !wantsIPv6 && gatewayRouter.ipV4 != nil {
		return gatewayRouter.ipV4, nil
	}
	return nil, fmt.Errorf("node does not have a gateway router IP corresponding to the IP version requested")
}

func (e *egressIPMode) getPodIPs(pod *kapi.Pod) ([]net.IP, error) {
	_, podIps, err := util.GetPortAddresses(getPodKey(pod), e.ovnNBClient)
	if err != nil || len(podIps) == 0 {
		e.podRetry.Store(getPodKey(pod), true)
		return nil, fmt.Errorf("pod does not have IP assigned, err: %v", err)
	}
	if e.needsRetry(pod) {
		e.podRetry.Delete(getPodKey(pod))
	}
	return podIps, nil
}

func (e *egressIPMode) needsRetry(pod *kapi.Pod) bool {
	_, retry := e.podRetry.Load(getPodKey(pod))
	return retry
}

// createEgressPolicy uses logical router policies to force egress traffic to the egress node, for that we need
// to retrive the internal gateway router IP attached to the egress node. This method handles both the shared and
// local gateway mode case, and in the local case (i.e: packetMark != 0) uses the optional packetMark field
// on the logical router policy to properly L3 SNAT on the node side.
func (e *egressIPMode) createEgressPolicy(podIps []net.IP, status egressipv1.EgressIPStatusItem, packetMark uint32) error {
	isEgressIPv6 := utilnet.IsIPv6String(status.EgressIP)
	gatewayRouterIP, err := e.getGatewayRouterJoinIP(status.Node, isEgressIPv6)
	if err != nil {
		return fmt.Errorf("unable to retrieve node's: %s gateway IP, err: %v", status.Node, err)
	}
	for _, podIP := range podIps {
		if isEgressIPv6 && utilnet.IsIPv6(podIP) {
			var stderr string
			var err error
			if packetMark == 0 {
				_, stderr, err = util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, egressIPRereoutePriority,
					fmt.Sprintf("ip6.src == %s && ip6.dst == ::/0", podIP.String()), "reroute", gatewayRouterIP.String())
			} else {
				_, stderr, err = util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, egressIPRereoutePriority,
					fmt.Sprintf("ip6.src == %s && ip6.dst == ::/0", podIP.String()), "reroute", gatewayRouterIP.String(), fmt.Sprintf("pkt_mark=%v", packetMark))
			}
			if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
				return fmt.Errorf("unable to create logical router policy for egress IP: %s, stderr: %s, err: %v", status.EgressIP, stderr, err)
			}
		} else if !isEgressIPv6 && !utilnet.IsIPv6(podIP) {
			var stderr string
			var err error
			if packetMark == 0 {
				_, stderr, err = util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, egressIPRereoutePriority,
					fmt.Sprintf("ip4.src == %s && ip4.dst == 0.0.0.0/0", podIP.String()), "reroute", gatewayRouterIP.String())
			} else {
				_, stderr, err = util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, egressIPRereoutePriority,
					fmt.Sprintf("ip4.src == %s && ip4.dst == 0.0.0.0/0", podIP.String()), "reroute", gatewayRouterIP.String(), fmt.Sprintf("pkt_mark=%v", packetMark))
			}
			if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
				return fmt.Errorf("unable to create logical router policy for egress IP: %s, stderr: %s, err: %v", status.EgressIP, stderr, err)
			}
		}
	}
	return nil
}

func (e *egressIPMode) deleteEgressPolicy(podIps []net.IP, status egressipv1.EgressIPStatusItem) error {
	for _, podIP := range podIps {
		if utilnet.IsIPv6(podIP) && utilnet.IsIPv6String(status.EgressIP) {
			_, _, err := util.RunOVNNbctl("lr-policy-del", ovnClusterRouter, egressIPRereoutePriority, fmt.Sprintf("ip6.src == %s && ip6.dst == ::/0", podIP.String()))
			if err != nil {
				return fmt.Errorf("unable to delete logical router policy for pod IP: %s, err: %v", podIP.String(), err)
			}
		} else if !utilnet.IsIPv6(podIP) && !utilnet.IsIPv6String(status.EgressIP) {
			_, _, err := util.RunOVNNbctl("lr-policy-del", ovnClusterRouter, egressIPRereoutePriority, fmt.Sprintf("ip4.src == %s && ip4.dst == 0.0.0.0/0", podIP.String()))
			if err != nil {
				return fmt.Errorf("unable to delete logical router policy for pod IP: %s, err: %v", podIP.String(), err)
			}
		}
	}
	return nil
}

func (oc *Controller) addEgressNode(egressNode *kapi.Node) error {
	klog.V(5).Infof("Egress node: %s about to be initialized", egressNode.Name)
	if err := oc.initEgressIPAllocator(egressNode); err != nil {
		return fmt.Errorf("egress node initialization error: %v", err)
	}
	oc.egressAssignmentRetry.Range(func(key, value interface{}) bool {
		eIPName := key.(string)
		klog.V(5).Infof("Re-assignment for EgressIP: %s attempted by new node: %s", eIPName, egressNode.Name)
		eIP, err := oc.kube.GetEgressIP(eIPName)
		if err != nil {
			klog.Errorf("Re-assignment for EgressIP: unable to retrieve EgressIP: %s from the api-server, err: %v", eIPName, err)
			return true
		}
		if err := oc.addEgressIP(eIP); err != nil {
			klog.Errorf("Re-assignment for EgressIP: unable to assign EgressIP: %s, err: %v", eIP.Name, err)
			return true
		}
		oc.egressAssignmentRetry.Delete(eIP.Name)
		if err := oc.updateEgressIPWithRetry(eIP); err != nil {
			klog.Error(err)
		}
		return true
	})
	return nil
}

func (oc *Controller) deleteEgressNode(egressNode *kapi.Node) error {
	oc.eIPAllocatorMutex.Lock()
	delete(oc.eIPAllocator, egressNode.Name)
	oc.eIPAllocatorMutex.Unlock()

	klog.V(5).Infof("Egress node: %s about to be removed", egressNode.Name)
	egressIPs, err := oc.kube.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list egressIPs, err: %v", err)
	}
	for _, eIP := range egressIPs.Items {
		needsReassignment := false
		for _, status := range eIP.Status.Items {
			if status.Node == egressNode.Name {
				needsReassignment = true
				break
			}
		}
		if needsReassignment {
			klog.V(5).Infof("EgressIP: %s about to be re-assigned", eIP.Name)
			if err := oc.deleteEgressIP(&eIP); err != nil {
				klog.Errorf("EgressIP: %s re-assignmnent error: old egress IP deletion failed, err: %v", eIP.Name, err)
			}
			eIP.Status = egressipv1.EgressIPStatus{
				Items: []egressipv1.EgressIPStatusItem{},
			}
			if err := oc.addEgressIP(&eIP); err != nil {
				klog.Errorf("EgressIP: %s re-assignmnent error: new egress IP assignment failed, err: %v", eIP.Name, err)
			}
			if err := oc.updateEgressIPWithRetry(&eIP); err != nil {
				klog.Errorf("EgressIP: %s re-assignmnent error: update of new egress IP failed, err: %v", eIP.Name, err)
			}
		}
	}
	return nil
}

func (oc *Controller) initEgressIPAllocator(node *kapi.Node) (err error) {
	oc.eIPAllocatorMutex.Lock()
	defer oc.eIPAllocatorMutex.Unlock()
	var v4Subnet, v6Subnet *net.IPNet
	v4IfAddr, v6IfAddr, err := util.ParseNodePrimaryIfAddr(node)
	if err != nil {
		return fmt.Errorf("unable to use node for egress assignment, err: %v", err)
	}
	if v4IfAddr != "" {
		_, v4Subnet, err = net.ParseCIDR(v4IfAddr)
		if err != nil {
			return err
		}
	}
	if v6IfAddr != "" {
		_, v6Subnet, err = net.ParseCIDR(v6IfAddr)
		if err != nil {
			return err
		}
	}
	oc.eIPAllocator[node.Name] = &eNode{
		name:        node.Name,
		v4Subnet:    v4Subnet,
		v6Subnet:    v6Subnet,
		allocations: make(map[string]bool),
	}
	return nil
}

func (oc *Controller) addNodeForEgress(node *v1.Node) error {
	v4Addr, v6Addr := oc.getNodeInternalAddrs(node)
	v4ClusterSubnet, v6ClusterSubnet := oc.getClusterSubnets()
	if err := oc.createDefaultNoRerouteNodePolicies(v4Addr, v6Addr, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	return nil
}

func (oc *Controller) deleteNodeForEgress(node *v1.Node) error {
	v4Addr, v6Addr := oc.getNodeInternalAddrs(node)
	v4ClusterSubnet, v6ClusterSubnet := oc.getClusterSubnets()
	if err := oc.deleteDefaultNoRerouteNodePolicies(v4Addr, v6Addr, v4ClusterSubnet, v6ClusterSubnet); err != nil {
		return err
	}
	return nil
}

func (oc *Controller) initClusterEgressPolicies(nodes []interface{}) {
	v4ClusterSubnet, v6ClusterSubnet := oc.getClusterSubnets()
	oc.createDefaultNoReroutePodPolicies(v4ClusterSubnet, v6ClusterSubnet)
}

func (oc *Controller) getClusterSubnets() (*net.IPNet, *net.IPNet) {
	var v4ClusterSubnet, v6ClusterSubnet *net.IPNet
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if !utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			v4ClusterSubnet = clusterSubnet.CIDR
		} else {
			v6ClusterSubnet = clusterSubnet.CIDR
		}
	}
	return v4ClusterSubnet, v6ClusterSubnet
}

func (oc *Controller) getNodeInternalAddrs(node *v1.Node) (net.IP, net.IP) {
	var v4Addr, v6Addr net.IP
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type == v1.NodeInternalIP {
			ip := net.ParseIP(nodeAddr.Address)
			if !utilnet.IsIPv6(ip) {
				v4Addr = ip
			} else {
				v6Addr = ip
			}
		}
	}
	return v4Addr, v6Addr
}

// createDefaultNoReroutePodPolicies ensures egress pods east<->west traffic with regular pods,
// i.e: ensuring that an egress pod can still communicate with a regular pod / service backed by regular pods
func (oc *Controller) createDefaultNoReroutePodPolicies(v4ClusterSubnet, v6ClusterSubnet *net.IPNet) {
	if v4ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4ClusterSubnet.String(), v4ClusterSubnet.String()), "allow")
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			klog.Errorf("Unable to create IPv4 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
	if v6ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6ClusterSubnet.String(), v6ClusterSubnet.String()), "allow")
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			klog.Errorf("Unable to create IPv6 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
}

// createDefaultNoRerouteNodePolicies ensures egress pods east<->west traffic with hostNetwork pods,
// i.e: ensuring that an egress pod can still communicate with a hostNetwork pod / service backed by hostNetwork pods
func (oc *Controller) createDefaultNoRerouteNodePolicies(v4NodeAddr, v6NodeAddr net.IP, v4ClusterSubnet, v6ClusterSubnet *net.IPNet) error {
	if v4NodeAddr != nil && v4ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip4.src == %s && ip4.dst == %s/32", v4ClusterSubnet.String(), v4NodeAddr.String()), "allow")
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			return fmt.Errorf("unable to create IPv4 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
	if v6NodeAddr != nil && v6ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-add", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip6.src == %s && ip6.dst == %s/128", v6ClusterSubnet.String(), v6NodeAddr.String()), "allow")
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			return fmt.Errorf("unable to create IPv6 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
	return nil
}

func (oc *Controller) deleteDefaultNoRerouteNodePolicies(v4NodeAddr, v6NodeAddr net.IP, v4ClusterSubnet, v6ClusterSubnet *net.IPNet) error {
	if v4NodeAddr != nil && v4ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-del", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip4.src == %s && ip4.dst == %s/32", v4ClusterSubnet.String(), v4NodeAddr.String()))
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			return fmt.Errorf("unable to create IPv4 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
	if v6NodeAddr != nil && v6ClusterSubnet != nil {
		_, stderr, err := util.RunOVNNbctl("lr-policy-del", ovnClusterRouter, defaultNoRereoutePriority,
			fmt.Sprintf("ip6.src == %s && ip6.dst == %s/128", v6ClusterSubnet.String(), v6NodeAddr.String()))
		if err != nil && !strings.Contains(stderr, policyAlreadyExistsMsg) {
			return fmt.Errorf("unable to create IPv6 default no-reroute logical router policy, stderr: %s, err: %v", stderr, err)
		}
	}
	return nil
}
