package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	networkattachmentdefinitionapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func (oc *Controller) syncPods(pods []interface{}) {
	var allOps []ovsdb.Operation
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		on, networkMap, err := util.IsNetworkOnPod(pod, oc.nadInfo)
		if err != nil || !on {
			continue
		}
		lsManagerNodeName := pod.Spec.NodeName
		for nadName := range networkMap {
			annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
			if util.PodScheduled(pod) && util.PodWantsNetwork(pod) && err == nil {
				logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name, nadName, !oc.nadInfo.NotDefault)
				expectedLogicalPorts[logicalPort] = true
				if err = oc.waitForNodeLogicalSwitchInCache(lsManagerNodeName); err != nil {
					klog.Errorf("Failed to wait for node %s to be added to cache. IP allocation may fail!",
						lsManagerNodeName)
				}
				if err = oc.lsManager.AllocateIPs(lsManagerNodeName, annotations.IPs); err != nil {
					klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
						" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
						lsManagerNodeName, err)
				}
			}
		}
	}

	// in order to minimize the number of database transactions build a map of all ports keyed by UUID
	portCache := make(map[string]nbdb.LogicalSwitchPort)
	lspList := []nbdb.LogicalSwitchPort{}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	err := oc.mc.nbClient.List(ctx, &lspList)
	if err != nil {
		klog.Errorf("Cannot sync pods, cannot retrieve list of logical switch ports (%+v)", err)
		return
	}
	for _, lsp := range lspList {
		portCache[lsp.UUID] = lsp
	}
	// get all the nodes from the watchFactory
	nodes, err := oc.mc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Failed to get nodes: %v", err)
		return
	}
	for _, n := range nodes {
		stalePorts := []string{}
		// find the logical switch for the node
		ls, err := findLogicalSwitch(oc.mc.nbClient, n.Name)
		if err != nil {
			klog.Errorf("Error getting logical switch for node %s: %v", n.Name, err)
			continue
		}
		for _, port := range ls.Ports {
			if portCache[port].ExternalIDs["pod"] != "true" {
				continue
			}
			netName, ok := portCache[port].ExternalIDs["network_name"]
			if oc.nadInfo.NotDefault {
				if !ok || netName != oc.nadInfo.NetName {
					continue
				}
			} else if ok {
				continue
			}

			if _, ok := expectedLogicalPorts[portCache[port].Name]; ok {
				continue
			}
			stalePorts = append(stalePorts, port)
		}
		if len(stalePorts) > 0 {
			ops, err := oc.mc.nbClient.Where(ls).Mutate(ls, model.Mutation{
				Field:   &ls.Ports,
				Mutator: ovsdb.MutateOperationDelete,
				Value:   stalePorts,
			})
			if err != nil {
				klog.Errorf("Could not generate ops to delete stale ports from logical switch %s (%+v)", n.Name, err)
				continue
			}
			allOps = append(allOps, ops...)
		}
	}
	_, err = libovsdbops.TransactAndCheck(oc.mc.nbClient, allOps)
	if err != nil {
		klog.Errorf("Could not remove stale logicalPorts from switches (%+v)", err)
	}
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if !oc.nadInfo.NotDefault {
		oc.deletePodExternalGW(pod)
	}
	if pod.Spec.HostNetwork {
		return
	}
	if !util.PodScheduled(pod) {
		return
	}

	on, networkMap, err := util.IsNetworkOnPod(pod, oc.nadInfo)
	if err != nil || !on {
		// the Pod is not attached to this specific network
		return
	}

	lsManagerNodeName := pod.Spec.NodeName
	for nadName, network := range networkMap {
		oc.delLogicalPort4Nad(pod, nadName, lsManagerNodeName, network)
	}
}

func (oc *Controller) delLogicalPort4Nad(pod *kapi.Pod, nadName, lsManagerNodeName string,
	network *networkattachmentdefinitionapi.NetworkSelectionElement) {

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod %s on network: %s", podDesc, nadName)

	logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name, nadName, !oc.nadInfo.NotDefault)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		// If ovnkube-master restarts, it is also possible the Pod's logical switch port
		// is not re-added into the cache. Delete logical switch port anyway.
		err = ovnNBLSPDel(oc.mc.nbClient, logicalPort, oc.nadInfo.Prefix+lsManagerNodeName)
		if err != nil {
			klog.Errorf(err.Error())
		}

		// Even if the port is not in the cache, IPs annotated in the Pod annotation may already be allocated,
		// need to release them to avoid leakage.
		annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
		if err == nil {
			podIfAddrs := annotation.IPs
			_ = oc.lsManager.ReleaseIPs(lsManagerNodeName, podIfAddrs)
		}
		return
	}

	// FIXME: if any of these steps fails we need to stop and try again later...

	if err := oc.deletePodFromNamespace(pod.Namespace, portInfo); err != nil {
		klog.Errorf(err.Error())
	}

	err = ovnNBLSPDel(oc.mc.nbClient, logicalPort, oc.nadInfo.Prefix+lsManagerNodeName)
	if err != nil {
		klog.Errorf(err.Error())
	}

	if err := oc.lsManager.ReleaseIPs(lsManagerNodeName, portInfo.ips); err != nil {
		klog.Errorf(err.Error())
	}

	if !oc.nadInfo.NotDefault {
		if config.Gateway.DisableSNATMultipleGWs {
			if err := deletePerPodGRSNAT(oc.mc.nbClient, pod.Spec.NodeName, portInfo.ips); err != nil {
				klog.Errorf(err.Error())
			}
		}
		podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		oc.deleteGWRoutesForPod(podNsName, portInfo.ips)
	}
	oc.logicalPortCache.remove(logicalPort)
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) (*nbdb.LogicalSwitch, error) {
	// Wait for the node logical switch to be created by the ClusterController and be present
	// in libovsdb's cache. The node switch will be created when the node's logical network infrastructure
	// is created by the node watch
	switchName := oc.nadInfo.Prefix + nodeName
	ls := &nbdb.LogicalSwitch{Name: switchName}
	if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
		logicalSwitch, err := findLogicalSwitch(oc.mc.nbClient, switchName)
		if err != nil && err != libovsdbclient.ErrNotFound {
			return false, err
		}
		if err == nil {
			ls = logicalSwitch
			return true, nil
		}
		return false, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for logical switch in libovsdb cache %q subnet: %v", nodeName, err)
	}
	return ls, nil
}

func (oc *Controller) waitForNodeLogicalSwitchInCache(nodeName string) error {
	// Wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	var subnets []*net.IPNet
	if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
		subnets = oc.lsManager.GetSwitchSubnets(nodeName)
		return subnets != nil, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for logical switch %q subnet: %v", nodeName, err)
	}
	return nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet,
	network *networkattachmentdefinitionapi.NetworkSelectionElement) (err error) {

	if oc.nadInfo.NotDefault {
		// non default network, see if its network-attachment's annotation has default-route key.
		// If present, then we need to add default route for it
		podAnnotation.Gateways = append(podAnnotation.Gateways, network.GatewayRequest...)
		for _, podIfAddr := range podAnnotation.IPs {
			isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
			nodeSubnet, err := util.MatchIPNetFamily(isIPv6, nodeSubnets)
			if err != nil {
				return err
			}
			gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)

			for _, clusterSubnet := range oc.clusterSubnets {
				if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
		}
		return nil
	}

	// For default network only: network may be ni for default network
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetK8sPodAllNetworks(pod)
	if err != nil {
		return fmt.Errorf("error while getting all network attachment definitions for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRouteV4 := false
	otherDefaultRouteV6 := false
	for _, n := range networks {
		for _, gatewayRequest := range n.GatewayRequest {
			if utilnet.IsIPv6(gatewayRequest) {
				otherDefaultRouteV6 = true
			} else {
				otherDefaultRouteV4 = true
			}
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchIPNetFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}

		gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)

		otherDefaultRoute := otherDefaultRouteV4
		if isIPv6 {
			otherDefaultRoute = otherDefaultRouteV6
		}
		var gatewayIP net.IP
		if otherDefaultRoute {
			for _, clusterSubnet := range oc.clusterSubnets {
				if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
			for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
				if isIPv6 == utilnet.IsIPv6CIDR(serviceSubnet) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    serviceSubnet,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
		} else {
			gatewayIP = gatewayIPnet.IP
		}

		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			// Add a route for each hybrid overlay subnet via the hybrid
			// overlay port on the pod's logical switch.
			nextHop := util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			for _, clusterSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) == isIPv6 {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: nextHop,
					})
				}
			}
		}
		if gatewayIP != nil {
			podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIP)
		}
	}
	return nil
}

func (oc *Controller) updatePodAnnotationWithRetry(origPod *kapi.Pod, podInfo *util.PodAnnotation) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := oc.mc.kube.GetPod(origPod.Namespace, origPod.Name)
		if err != nil {
			return err
		}

		cpod := pod.DeepCopy()
		err = util.MarshalPodAnnotation(&cpod.Annotations, podInfo, oc.nadInfo.NetName)
		if err != nil {
			return err
		}
		return oc.mc.kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update annotation on pod %s/%s: %v", origPod.Namespace, origPod.Name, resultErr)
	}
	return nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	lsManagerNodeName := pod.Spec.NodeName

	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	if oc.lsManager.IsNonHostSubnetSwitch(lsManagerNodeName) {
		return nil
	}

	on, networkMap, err := util.IsNetworkOnPod(pod, oc.nadInfo)
	if err != nil || !on {
		// the pod is not attached to this specific network
		klog.V(5).Infof("Pod %s/%s is not attached on this network: %s", pod.Namespace, pod.Name, oc.nadInfo.NetName)
		return nil
	}

	klog.V(5).Infof("Pod %s/%s is attached on this network: %s", pod.Namespace, pod.Name, oc.nadInfo.NetName)
	for nadName, network := range networkMap {
		err1 := oc.addLogicalPort4Nad(pod, nadName, lsManagerNodeName, network)
		if err1 != nil {
			err = err1
		}
	}
	return err
}

func (oc *Controller) addLogicalPort4Nad(pod *kapi.Pod, nadName, lsManagerNodeName string,
	network *networkattachmentdefinitionapi.NetworkSelectionElement) (err error) {
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort for network %s took %v", pod.Namespace, pod.Name, oc.nadInfo.NetName, time.Since(start))
	}()

	logicalSwitch := oc.nadInfo.Prefix + lsManagerNodeName
	ls, err := oc.waitForNodeLogicalSwitch(logicalSwitch)
	if err != nil {
		return err
	}

	portName := util.GetLogicalPortName(pod.Namespace, pod.Name, nadName, !oc.nadInfo.NotDefault)
	klog.Infof("[%s/%s] creating logical port for pod on switch %s for nad %s", pod.Namespace, pod.Name, logicalSwitch, nadName)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var allOps []ovsdb.Operation
	var addresses []string
	var releaseIPs bool
	lspExist := false
	needsIP := true

	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	getLSP := &nbdb.LogicalSwitchPort{Name: portName}
	err = oc.mc.nbClient.Get(ctx, getLSP)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
	}
	lsp := &nbdb.LogicalSwitchPort{Name: portName}
	if len(getLSP.UUID) == 0 {
		lsp.UUID = libovsdbops.BuildNamedUUID()
	} else {
		lsp.UUID = getLSP.UUID
		lspExist = true
	}

	lsp.Options = make(map[string]string)
	// Unique identifier to distinguish interfaces for recreated pods, also set by ovnkube-node
	// ovn-controller will claim the OVS interface only if external_ids:iface-id
	// matches with the Port_Binding.logical_port and external_ids:iface-id-ver matches
	// with the Port_Binding.options:iface-id-ver. This is not mandatory.
	// If Port_binding.options:iface-id-ver is not set, then OVS
	// Interface.external_ids:iface-id-ver if set is ignored.
	// Don't set iface-id-ver for already existing LSP if it wasn't set before,
	// because the corresponding OVS port may not have it set
	// (then ovn-controller won't bind the interface).
	// May happen on upgrade, because ovnkube-node doesn't update
	// existing OVS interfaces with new iface-id-ver option.
	if !lspExist || len(getLSP.Options["iface-id-ver"]) != 0 {
		lsp.Options["iface-id-ver"] = string(pod.UID)
	}
	// Bind the port to the node's chassis; prevents ping-ponging between
	// chassis if ovnkube-node isn't running correctly and hasn't cleared
	// out iface-id for an old instance of this pod, and the pod got
	// rescheduled.
	lsp.Options["requested-chassis"] = lsManagerNodeName

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)

	// the IPs we allocate in this function need to be released back to the
	// IPAM pool if there is some error in any step of addLogicalPort past
	// the point the IPs were assigned via the IPAM manager.
	// this needs to be done only when releaseIPs is set to true (the case where
	// we truly have assigned podIPs in this call) AND when there is no error in
	// the rest of the functionality of addLogicalPort. It is important to use a
	// named return variable for defer to work correctly.

	defer func() {
		if releaseIPs && err != nil {
			if relErr := oc.lsManager.ReleaseIPs(lsManagerNodeName, podIfAddrs); relErr != nil {
				klog.Errorf("Error when releasing IPs for node: %s, err: %q",
					lsManagerNodeName, relErr)
			} else {
				klog.Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), lsManagerNodeName)
			}
		}
	}()

	if err == nil {
		podMac = annotation.MAC
		podIfAddrs = annotation.IPs

		// If the pod already has annotations use the existing static
		// IP/MAC from the annotation.
		lsp.DynamicAddresses = nil

		// ensure we have reserved the IPs in the annotation
		if err = oc.lsManager.AllocateIPs(lsManagerNodeName, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			return fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %v",
				pod.Name, util.JoinIPNetIPs(podIfAddrs, " "), err)
		} else {
			needsIP = false
		}
	}

	if needsIP {
		// try to get the IP from existing port in OVN first
		podMac, podIfAddrs, err = oc.getPortAddresses(lsManagerNodeName, portName)
		if err != nil {
			return fmt.Errorf("failed to get pod addresses for pod %s on node: %s, err: %v",
				portName, lsManagerNodeName, err)
		}
		needsNewAllocation := false
		// ensure we have reserved the IPs found in OVN
		if len(podIfAddrs) == 0 {
			needsNewAllocation = true
		} else if err = oc.lsManager.AllocateIPs(lsManagerNodeName, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			klog.Warningf("Unable to allocate IPs found on existing OVN port: %s, for pod %s on node: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), portName, lsManagerNodeName, err)

			needsNewAllocation = true
		}
		if needsNewAllocation {
			// Previous attempts to use already configured IPs failed, need to assign new
			podMac, podIfAddrs, err = oc.assignPodAddresses(lsManagerNodeName)
			if err != nil {
				return fmt.Errorf("failed to assign pod addresses for pod %s on node: %s, err: %v",
					portName, lsManagerNodeName, err)
			}
		}

		releaseIPs = true
		if network != nil && network.MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s for network %s requested custom MAC: %s", pod.Namespace, pod.Name,
				nadName, network.MacRequest)
			podMac, err = net.ParseMAC(network.MacRequest)
			if err != nil {
				return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s on network %s: Error %v",
					network.MacRequest, pod.Name, nadName, err)
			}
		}
		podAnnotation := util.PodAnnotation{
			IPs: podIfAddrs,
			MAC: podMac,
		}
		var nodeSubnets []*net.IPNet
		if nodeSubnets = oc.lsManager.GetSwitchSubnets(lsManagerNodeName); nodeSubnets == nil {
			return fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, node: %s, network %s",
				pod.Name, lsManagerNodeName, oc.nadInfo.NetName)
		}
		err = oc.addRoutesGatewayIP(pod, &podAnnotation, nodeSubnets, network)
		if err != nil {
			return err
		}
		klog.V(5).Infof("Annotation values for network %s: ip=%v ; mac=%s ; gw=%s\n",
			nadName, podIfAddrs, podMac, podAnnotation.Gateways)

		if err = oc.updatePodAnnotationWithRetry(pod, &podAnnotation); err != nil {
			return err
		}
		releaseIPs = false
	}

	// Ensure the namespace/nsInfo exists
	routingExternalGWs, routingPodGWs, err := oc.addPodToNamespace(pod.Namespace, podIfAddrs)
	if err != nil {
		return err
	}

	if !oc.nadInfo.NotDefault {
		// if we have any external or pod Gateways, add routes
		gateways := make([]*gatewayInfo, 0, len(routingExternalGWs.gws)+len(routingPodGWs))

		if len(routingExternalGWs.gws) > 0 {
			gateways = append(gateways, routingExternalGWs)
		}
		for _, gw := range routingPodGWs {
			if len(gw.gws) > 0 {
				if err = validateRoutingPodGWs(routingPodGWs); err != nil {
					klog.Error(err)
				}
				gateways = append(gateways, &gw)
			} else {
				klog.Warningf("Found routingPodGW with no gateways ip set for namespace %s", pod.Namespace)
			}
		}

		if len(gateways) > 0 {
			podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
			err = oc.addGWRoutesForPod(gateways, podIfAddrs, podNsName, lsManagerNodeName)
			if err != nil {
				return err
			}
		} else if config.Gateway.DisableSNATMultipleGWs {
			// Add NAT rules to pods if disable SNAT is set and does not have
			// namespace annotations to go through external egress router
			if err = addPerPodGRSNAT(oc.mc.nbClient, oc.mc.watchFactory, pod, podIfAddrs); err != nil {
				return err
			}
		}

		// check if this pod is serving as an external GW
		err = oc.addPodExternalGW(pod)
		if err != nil {
			return fmt.Errorf("failed to handle external GW check: %v", err)
		}
	}

	// set addresses on the port
	// LSP addresses in OVN are a single space-separated value
	addresses = []string{podMac.String()}
	for _, podIfAddr := range podIfAddrs {
		addresses[0] = addresses[0] + " " + podIfAddr.IP.String()
	}

	lsp.Addresses = addresses

	// add external ids
	lsp.ExternalIDs = map[string]string{"namespace": pod.Namespace, "pod": "true"}
	if oc.nadInfo.NotDefault {
		lsp.ExternalIDs["network_name"] = oc.nadInfo.NetName
	}
	// CNI depends on the flows from port security, delay setting it until end
	lsp.PortSecurity = addresses

	if !lspExist {
		// create new logical switch port
		ops, err := oc.mc.nbClient.Create(lsp)
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)

		//add the logical switch port to the logical switch
		ops, err = oc.mc.nbClient.Where(ls).Mutate(ls, model.Mutation{
			Field:   &ls.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)

	} else {
		//update Existing logical switch port
		ops, err := oc.mc.nbClient.Where(lsp).Update(lsp, &lsp.Addresses, &lsp.ExternalIDs, &lsp.Options, &lsp.PortSecurity)
		if err != nil {
			return fmt.Errorf("could not create commands to update logical switch port %s - %+v", portName, err)
		}
		allOps = append(allOps, ops...)
	}

	results, err := libovsdbops.TransactAndCheck(oc.mc.nbClient, allOps)
	if err != nil {

		return fmt.Errorf("could not perform creation or update of logical switch port %s - %+v", portName, err)
	}
	go oc.mc.metricsRecorder.AddLSPEvent(pod.UID)

	// Add the pod's logical switch port to the port cache
	var lspUUID string
	if len(results) >= 1 && !lspExist {
		// the results may have mutltiple entries but should only be on one UUID
		lspUUID = results[0].UUID.GoUUID
	} else {
		lspUUID = lsp.UUID
	}
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, lspUUID, podMac, podIfAddrs)

	// If multicast is allowed and enabled for the namespace, add the port to the allow policy.
	// FIXME: there's a race here with the Namespace multicastUpdateNamespace() handler, but
	// it's rare and easily worked around for now.
	ns, err := oc.mc.watchFactory.GetNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if oc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
		if err := podAddAllowMulticastPolicy(oc.mc.nbClient, pod.Namespace, portInfo, oc.nadInfo.NetNameInfo); err != nil {
			return err
		}
	}
	// observe the pod creation latency metric, default network for now
	if !oc.nadInfo.NotDefault {
		metrics.RecordPodCreated(pod)
	}
	return nil
}

// Given a node, gets the next set of addresses (from the IPAM) for each of the node's
// subnets to assign to the new pod
func (oc *Controller) assignPodAddresses(nodeName string) (net.HardwareAddr, []*net.IPNet, error) {
	var (
		podMAC   net.HardwareAddr
		podCIDRs []*net.IPNet
		err      error
	)
	podCIDRs, err = oc.lsManager.AllocateNextIPs(nodeName)
	if err != nil {
		return nil, nil, err
	}
	if len(podCIDRs) > 0 {
		podMAC = util.IPAddrToHWAddr(podCIDRs[0].IP)
	}
	return podMAC, podCIDRs, nil
}

// Given a pod and the node on which it is scheduled, get all addresses currently assigned
// to it from the nbdb.
func (oc *Controller) getPortAddresses(nodeName, portName string) (net.HardwareAddr, []*net.IPNet, error) {
	podMac, podIPs, err := util.GetPortAddresses(portName, oc.mc.nbClient)
	if err != nil {
		return nil, nil, err
	}

	if podMac == nil || len(podIPs) == 0 {
		return nil, nil, nil
	}

	var podIPNets []*net.IPNet

	nodeSubnets := oc.lsManager.GetSwitchSubnets(nodeName)

	for _, ip := range podIPs {
		for _, subnet := range nodeSubnets {
			if subnet.Contains(ip) {
				podIPNets = append(podIPNets,
					&net.IPNet{
						IP:   ip,
						Mask: subnet.Mask,
					})
				break
			}
		}
	}
	return podMac, podIPNets, nil
}

// ovnNBLSPDel deletes the given logical switch using the libovsdb library
func ovnNBLSPDel(client libovsdbclient.Client, logicalPort, logicalSwitch string) error {
	var allOps []ovsdb.Operation
	ls, err := findLogicalSwitch(client, logicalSwitch)
	if err != nil {
		return fmt.Errorf("could not find logicalSwitch %s - %v", logicalSwitch, err)
	}

	lsp := &nbdb.LogicalSwitchPort{Name: logicalPort}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	err = client.Get(ctx, lsp)
	if err != nil {
		return fmt.Errorf("cannot delete logical switch port %s failed retrieving the object %v", logicalPort, err)
	}
	ops, err := client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("cannot generate ops delete logical switch port %s: %v", logicalPort, err)
	}
	allOps = append(allOps, ops...)
	//for testing purposes the explicit delete of the logical switch port is required
	ops, err = client.Where(lsp).Delete()
	if err != nil {
		return fmt.Errorf("cannot generate ops delete logical switch port %s: %v", logicalPort, err)
	}
	allOps = append(allOps, ops...)

	_, err = libovsdbops.TransactAndCheck(client, allOps)
	if err != nil {
		return fmt.Errorf("cannot delete logical switch port %s, %v", logicalPort, err)
	}
	return nil
}

func findLogicalSwitch(nbClient libovsdbclient.Client, logicalSwitchName string) (*nbdb.LogicalSwitch, error) {
	logicalSwitches := []nbdb.LogicalSwitch{}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	err := nbClient.WhereCache(
		func(ls *nbdb.LogicalSwitch) bool {
			return ls.Name == logicalSwitchName
		}).List(ctx, &logicalSwitches)

	if err != nil {
		return nil, fmt.Errorf("error finding logical switch %s: %v", logicalSwitchName, err)
	}

	if len(logicalSwitches) == 0 {
		return nil, libovsdbclient.ErrNotFound
	}

	if len(logicalSwitches) > 1 {
		return nil, fmt.Errorf("unexpectedly found multiple logical switches: %v", logicalSwitches)
	}

	return &logicalSwitches[0], nil
}
