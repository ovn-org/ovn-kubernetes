package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

// Builds the logical switch port name for a given pod.
func podLogicalPortName(pod *kapi.Pod) string {
	return pod.Namespace + "_" + pod.Name
}

func (oc *Controller) syncPods(pods []interface{}) {
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		annotations, err := util.UnmarshalPodAnnotation(pod.Annotations)
		if podScheduled(pod) && podWantsNetwork(pod) && err == nil {
			logicalPort := podLogicalPortName(pod)
			expectedLogicalPorts[logicalPort] = true
			if err = oc.lsManager.AllocateIPs(pod.Spec.NodeName, annotations.IPs); err != nil {
				klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
					" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
					pod.Spec.NodeName, err)
			}
		}
	}

	// get the list of logical ports from OVN
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "logical_switch_port", "external_ids:pod=true")
	if err != nil {
		klog.Errorf("Error in obtaining list of logical ports, "+
			"stderr: %q, err: %v",
			stderr, err)
		return
	}
	existingLogicalPorts := strings.Fields(output)
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			klog.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
				existingPort)
			if err != nil {
				klog.Errorf("Error in deleting pod's logical port "+
					"stdout: %q, stderr: %q err: %v",
					out, stderr, err)
			}
		}
	}
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)

	logicalPort := podLogicalPortName(pod)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	// FIXME: if any of these steps fails we need to stop and try again later...

	// Remove the port from the default deny multicast policy
	if oc.multicastSupport {
		if err := podDeleteDefaultDenyMulticastPolicy(portInfo); err != nil {
			klog.Errorf(err.Error())
		}
	}

	if err := oc.deletePodFromNamespace(pod.Namespace, portInfo); err != nil {
		klog.Errorf(err.Error())
	}

	out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del", logicalPort)
	if err != nil {
		klog.Errorf("Error in deleting pod %s logical port "+
			"stdout: %q, stderr: %q, (%v)",
			podDesc, out, stderr, err)
	}

	if err := oc.lsManager.ReleaseIPs(portInfo.logicalSwitch, portInfo.ips); err != nil {
		klog.Errorf(err.Error())
	}

	for _, podIPNet := range portInfo.ips {
		// delete src-ip cached route to GR
		nsInfo, err := oc.waitForNamespaceLocked(pod.Namespace)
		if err != nil {
			klog.Errorf("Unable to check port: %s, ip: %s for external gw route deletion: %v", portInfo.name,
				podIPNet.IP, err)
			continue
		}
		podIP := podIPNet.IP.String()
		if gwToGr, ok := nsInfo.podExternalRoutes[podIP]; ok {
			if len(gwToGr) == 0 {
				delete(nsInfo.podExternalRoutes, pod.Status.PodIP)
				nsInfo.Unlock()
				continue
			}
			mask := GetIPFullMask(podIP)
			for gw, gr := range gwToGr {
				_, stderr, err = util.RunOVNNbctl("--", "--if-exists", "--policy=src-ip",
					"lr-route-del", gr, podIP+mask, gw)
				if err != nil {
					klog.Errorf("Unable to delete external gw ecmp route to GR router, stderr:%q, err:%v", stderr, err)
				} else {
					delete(nsInfo.podExternalRoutes, pod.Status.PodIP)
				}
			}
		}
		if config.Gateway.DisableSNATMultipleGWs && nsInfo.routingExternalGWs == nil {
			nodeName := pod.Spec.NodeName
			gr := "GR_" + nodeName
			stdout, stderr, err := util.RunOVNNbctl("--", "--if-exists", "lr-nat-del",
				gr, "snat", podIP)
			if err != nil {
				klog.Errorf("Failed to delete SNAT rule for pod on gateway router %s, "+
					"stdout: %q, stderr: %q, error: %v", gr, stdout, stderr, err)
			}
		}
		nsInfo.Unlock()
	}
	oc.deletePodExternalGW(pod)
	oc.logicalPortCache.remove(logicalPort)
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) error {
	// Wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
		return oc.lsManager.GetSwitchSubnets(nodeName) != nil, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for logical switch %q subnet: %v", nodeName, err)
	}
	return nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet) error {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetPodNetSelAnnotation(pod, util.NetworkAttachmentAnnotation)
	if err != nil {
		return fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRouteV4 := false
	otherDefaultRouteV6 := false
	for _, network := range networks {
		for _, gatewayRequest := range network.GatewayRequest {
			if utilnet.IsIPv6(gatewayRequest) {
				otherDefaultRouteV6 = true
			} else {
				otherDefaultRouteV4 = true
			}
		}
	}
	// DUALSTACK FIXME: hybridOverlayExternalGW is not Dualstack
	var hybridOverlayExternalGW net.IP
	if config.HybridOverlay.Enabled {
		hybridOverlayExternalGW, err = oc.getHybridOverlayExternalGwAnnotation(pod.Namespace)
		if err != nil {
			return err
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchIPFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}
		// DUALSTACK FIXME: hybridOverlayExternalGW is not Dualstack
		// When oc.getHybridOverlayExternalGwAnnotation() supports dualstack, return error if no match.
		// If external gateway mode is configured, need to use it for all outgoing traffic, so don't want
		// to fall back to the default gateway here
		if hybridOverlayExternalGW != nil && utilnet.IsIPv6(hybridOverlayExternalGW) != isIPv6 {
			klog.Warningf("Pod %s/%s has no external gateway for %s", pod.Namespace, pod.Name, util.IPFamilyName(isIPv6))
			continue
		}

		gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)

		otherDefaultRoute := otherDefaultRouteV4
		if isIPv6 {
			otherDefaultRoute = otherDefaultRouteV6
		}
		var gatewayIP net.IP
		if otherDefaultRoute || hybridOverlayExternalGW != nil {
			for _, clusterSubnet := range config.Default.ClusterSubnets {
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
			if hybridOverlayExternalGW != nil {
				gatewayIP = util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
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

func (oc *Controller) getRoutingExternalGWs(ns string) ([]net.IP, error) {
	nsInfo, err := oc.waitForNamespaceLocked(ns)
	if err != nil {
		return nil, err
	}
	defer nsInfo.Unlock()
	return nsInfo.routingExternalGWs, nil
}

func (oc *Controller) getHybridOverlayExternalGwAnnotation(ns string) (net.IP, error) {
	nsInfo, err := oc.waitForNamespaceLocked(ns)
	if err != nil {
		return nil, err
	}
	defer nsInfo.Unlock()
	return nsInfo.hybridOverlayExternalGW, nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	if oc.lsManager.IsNonHostSubnetSwitch(pod.Spec.NodeName) {
		return nil
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	logicalSwitch := pod.Spec.NodeName
	err = oc.waitForNodeLogicalSwitch(logicalSwitch)
	if err != nil {
		return err
	}

	portName := podLogicalPortName(pod)
	klog.V(5).Infof("Creating logical port for %s on switch %s", portName, logicalSwitch)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var cmds []*goovn.OvnCommand
	var addresses []string
	var cmd *goovn.OvnCommand
	var releaseIPs bool
	needsIP := true

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	lsp, err := oc.ovnNBClient.LSPGet(portName)
	if err != nil && err != goovn.ErrorNotFound && err != goovn.ErrorSchema {
		return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
	}

	if lsp == nil {
		cmd, err = oc.ovnNBClient.LSPAdd(logicalSwitch, portName)
		if err != nil {
			return fmt.Errorf("unable to create the LSPAdd command for port: %s from the nbdb", portName)
		}
		cmds = append(cmds, cmd)
	}

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)

	// the IPs we allocate in this function need to be released back to the
	// IPAM pool if there is some error in any step of addLogicalPort past
	// the point the IPs were assigned via the IPAM manager.
	// this needs to be done only when releaseIPs is set to true (the case where
	// we truly have assigned podIPs in this call) AND when there is no error in
	// the rest of the functionality of addLogicalPort. It is important to use a
	// named return variable for defer to work correctly.

	defer func() {
		if releaseIPs && err != nil {
			if relErr := oc.lsManager.ReleaseIPs(logicalSwitch, podIfAddrs); relErr != nil {
				klog.Errorf("Error when releasing IPs for node: %s, err: %q",
					logicalSwitch, relErr)
			} else {
				klog.Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), logicalSwitch)
			}
		}
	}()

	if err == nil {
		podMac = annotation.MAC
		podIfAddrs = annotation.IPs

		// If the pod already has annotations use the existing static
		// IP/MAC from the annotation.
		cmd, err = oc.ovnNBClient.LSPSetDynamicAddresses(portName, "")
		if err != nil {
			return fmt.Errorf("unable to create LSPSetDynamicAddresses command for port: %s", portName)
		}
		cmds = append(cmds, cmd)

		// ensure we have reserved the IPs in the annotation
		if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			return fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %v",
				pod.Name, util.JoinIPNetIPs(podIfAddrs, " "), err)
		} else {
			needsIP = false
		}
	}

	if needsIP {
		// try to get the IP from existing port in OVN first
		podMac, podIfAddrs, err = oc.getPortAddresses(logicalSwitch, portName)
		if err != nil {
			return fmt.Errorf("failed to get pod addresses for pod %s on node: %s, err: %v",
				portName, logicalSwitch, err)
		}
		needsNewAllocation := false
		// ensure we have reserved the IPs found in OVN
		if len(podIfAddrs) == 0 {
			needsNewAllocation = true
		} else if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			klog.Warningf("Unable to allocate IPs found on existing OVN port: %s, for pod %s on node: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), portName, logicalSwitch, err)

			needsNewAllocation = true
		}
		if needsNewAllocation {
			// Previous attempts to use already configured IPs failed, need to assign new
			podMac, podIfAddrs, err = oc.assignPodAddresses(logicalSwitch)
			if err != nil {
				return fmt.Errorf("failed to assign pod addresses for pod %s on node: %s, err: %v",
					portName, logicalSwitch, err)
			}
		}

		var networks []*types.NetworkSelectionElement

		networks, err = util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
		// handle error cases separately first to ensure binding to err, otherwise the
		// defer will fail
		if err != nil {
			return fmt.Errorf("error while getting custom MAC config for port %q from "+
				"default-network's network-attachment: %v", portName, err)
		} else if networks != nil && len(networks) != 1 {
			err = fmt.Errorf("invalid network annotation size while getting custom MAC config"+
				" for port %q", portName)
			return err
		}

		if networks != nil && networks[0].MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
			podMac, err = net.ParseMAC(networks[0].MacRequest)
			if err != nil {
				return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
					networks[0].MacRequest, pod.Name, err)
			}
		}
		podAnnotation := util.PodAnnotation{
			IPs: podIfAddrs,
			MAC: podMac,
		}
		var nodeSubnets []*net.IPNet
		if nodeSubnets = oc.lsManager.GetSwitchSubnets(logicalSwitch); nodeSubnets == nil {
			return fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, node: %s",
				pod.Name, logicalSwitch)
		}
		err = oc.addRoutesGatewayIP(pod, &podAnnotation, nodeSubnets)
		if err != nil {
			return err
		}
		var marshalledAnnotation map[string]string
		marshalledAnnotation, err = util.MarshalPodAnnotation(&podAnnotation)
		if err != nil {
			return fmt.Errorf("error creating pod network annotation: %v", err)
		}

		klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s\nAnnotation=%s",
			podIfAddrs, podMac, podAnnotation.Gateways, marshalledAnnotation)
		if err = oc.kube.SetAnnotationsOnPod(pod, marshalledAnnotation); err != nil {
			releaseIPs = true
			return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
		}
	}

	// set addresses on the port
	addresses = make([]string, len(podIfAddrs)+1)
	addresses[0] = podMac.String()
	for idx, podIfAddr := range podIfAddrs {
		addresses[idx+1] = podIfAddr.IP.String()
	}
	// LSP addresses in OVN are a single space-separated value
	cmd, err = oc.ovnNBClient.LSPSetAddress(portName, strings.Join(addresses, " "))
	if err != nil {
		return fmt.Errorf("unable to create LSPSetAddress command for port: %s", portName)
	}
	cmds = append(cmds, cmd)

	// add external ids
	extIds := map[string]string{"namespace": pod.Namespace, "pod": "true"}
	cmd, err = oc.ovnNBClient.LSPSetExternalIds(portName, extIds)
	if err != nil {
		return fmt.Errorf("unable to create LSPSetExternalIds command for port: %s", portName)
	}
	cmds = append(cmds, cmd)

	// execute all the commands together.
	err = oc.ovnNBClient.Execute(cmds...)
	if err != nil {
		return fmt.Errorf("error while creating logical port %s error: %v",
			portName, err)
	}

	lsp, err = oc.ovnNBClient.LSPGet(portName)
	if err != nil || lsp == nil {
		return fmt.Errorf("failed to get the logical switch port: %s from the ovn client, error: %s", portName, err)
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, lsp.UUID, podMac, podIfAddrs)

	// Wait for namespace to exist, no calls after this should ever use waitForNamespaceLocked
	if err = oc.addPodToNamespace(pod.Namespace, portInfo); err != nil {
		return err
	}

	// Enforce the default deny multicast policy
	if oc.multicastSupport {
		if err = podAddDefaultDenyMulticastPolicy(portInfo); err != nil {
			return err
		}
	}

	// add src-ip routes to GR if external gw annotation is set
	routingExternalGWs, err := oc.getRoutingExternalGWs(pod.Namespace)
	if err != nil {
		return err
	}
	if routingExternalGWs != nil {
		gr := "GR_" + pod.Spec.NodeName
		for _, v := range routingExternalGWs {
			gw := v.String()
			for _, podIPNet := range podIfAddrs {
				podIP := podIPNet.IP.String()
				mask := GetIPFullMask(podIP)
				_, stderr, err := util.RunOVNNbctl("--may-exist", "--policy=src-ip", "--ecmp",
					"lr-route-add", gr, podIP+mask, gw)
				if err != nil {
					return fmt.Errorf("unable to add external gw src-ip route to GR router, stderr:%q, err:%v", stderr, err)
				}
				nsInfo, err := oc.waitForNamespaceLocked(pod.Namespace)
				if err != nil {
					return err
				}
				if nsInfo.podExternalRoutes[podIP] == nil {
					nsInfo.podExternalRoutes[podIP] = make(map[string]string)
				}
				nsInfo.podExternalRoutes[podIP][gw] = gr
				nsInfo.Unlock()
			}
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set and does not have
		// namespace annotations to go thru external egress router
		nodeName := pod.Spec.NodeName
		node, err := oc.watchFactory.GetNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to get node %s: %v", nodeName, err)
		}
		l3GWConfig, err := util.ParseNodeL3GatewayAnnotation(node)
		if err != nil {
			return fmt.Errorf("unable to parse node L3 gw annotation: %v", err)
		}
		gr := "GR_" + nodeName
		for _, gwIPNet := range l3GWConfig.IPAddresses {
			for _, podIPNet := range podIfAddrs {
				gwIP := gwIPNet.IP.String()
				podIP := podIPNet.IP.String()
				mask := GetIPFullMask(podIP)
				stdout, stderr, err := util.RunOVNNbctl("--may-exist", "lr-nat-add",
					gr, "snat", gwIP, podIP+mask)
				if err != nil {
					return fmt.Errorf("failed to create SNAT rule for pod on gateway router %s, "+
						"stdout: %q, stderr: %q, error: %v", gr, stdout, stderr, err)
				}
			}
		}
	}

	// check if this pod is serving as an external GW
	err = oc.addPodExternalGW(pod)
	if err != nil {
		return fmt.Errorf("failed to handle external GW check: %v", err)
	}

	// CNI depends on the flows from port security, delay setting it until end
	cmd, err = oc.ovnNBClient.LSPSetPortSecurity(portName, strings.Join(addresses, " "))
	if err != nil {
		return fmt.Errorf("unable to create LSPSetPortSecurity command for port: %s", portName)
	}

	err = oc.ovnNBClient.Execute(cmd)
	if err != nil {
		return fmt.Errorf("error while setting port security on port: %s error: %v",
			portName, err)
	}

	// observe the pod creation latency metric.
	metrics.RecordPodCreated(pod)
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
	podMac, podIPs, err := util.GetPortAddresses(portName, oc.ovnNBClient)
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

func (oc *Controller) addPodExternalGW(pod *kapi.Pod) error {
	type Network struct {
		Name string
		Ips  []string
	}
	routingNamespaceAnnotation := pod.Annotations[routingNamespaceAnnotation]
	if routingNamespaceAnnotation == "" {
		return nil
	}
	klog.Infof("External gateway pod: %s, detected for namespace(s) %s", pod.Name, routingNamespaceAnnotation)
	var foundGws []net.IP
	if routingNetworkAnnotation == "" && pod.Spec.HostNetwork {
		for _, podIP := range pod.Status.PodIPs {
			ip := net.ParseIP(podIP.IP)
			if ip != nil {
				foundGws = append(foundGws, ip)
			}
		}
	} else if routingNetworkAnnotation != "" {
		var multusNetworks []Network
		err := json.Unmarshal([]byte(pod.ObjectMeta.Annotations["k8s.v1.cni.cncf.io/network-status"]), &multusNetworks)
		if err != nil {
			return fmt.Errorf("unable to unmarshall annotation k8s.v1.cni.cncf.io/network-status on pod %s: %v", pod.Name, err)
		}
		for _, multusNetwork := range multusNetworks {
			if multusNetwork.Name == routingNetworkAnnotation {
				for _, gwIP := range multusNetwork.Ips {
					ip := net.ParseIP(gwIP)
					if ip != nil {
						foundGws = append(foundGws, ip)
					}
				}
			}
		}
	} else {
		klog.Errorf("Ignoring pod %s as an external gateway candidate. Invalid combination "+
			"of host network: %t and routing-network annotation: %s", pod.Name, pod.Spec.HostNetwork, routingNetworkAnnotation)
		return nil
	}

	// if we found any gateways then we need to update current pods routing in the relevant namespace
	if len(foundGws) == 0 {
		klog.Warningf("No valid gateway IPs found for requested external gateway pod: %s", pod.Name)
		return nil
	}

	for _, namespace := range strings.Split(routingNamespaceAnnotation, ",") {
		nsInfo, err := oc.waitForNamespaceLocked(namespace)
		if err != nil {
			return err
		}
		defer nsInfo.Unlock()
		nsInfo.routingExternalPodGWs[pod.Name] = foundGws
		existingPods, err := oc.watchFactory.GetPods(namespace)
		if err != nil {
			return fmt.Errorf("failed to get all the pods for namespace %s, error: %v", namespace, err)
		}
		for _, gwIP := range foundGws {
			for _, pod := range existingPods {
				for _, podIP := range pod.Status.PodIPs {
					mask := GetIPFullMask(podIP.IP)
					gr := "GR_" + pod.Spec.NodeName
					// TODO (trozet): use the go bindings here and batch commands
					_, stderr, err := util.RunOVNNbctl("--", "--may-exist", "--policy=src-ip", "--ecmp",
						"lr-route-add", gr, podIP.IP+mask, gwIP.String())
					if err != nil {
						klog.Errorf("Unable to add pod ecmp src-ip route to GR router, stderr:%q, err:%v", stderr, err)
					} else {
						klog.V(5).Infof("ECMP route added for pod: %s, on gr: %s, to gw: %s", pod.Name,
							gr, gwIP.String())
						if nsInfo.podExternalRoutes[podIP.IP] == nil {
							nsInfo.podExternalRoutes[podIP.IP] = make(map[string]string)
						}
						nsInfo.podExternalRoutes[podIP.IP][gwIP.String()] = gr
					}
				}
			}
		}
	}
	return nil
}

func (oc *Controller) deletePodExternalGW(pod *kapi.Pod) {
	routingNamespaceAnnotation := pod.Annotations[routingNamespaceAnnotation]
	if routingNamespaceAnnotation == "" {
		return
	}
	klog.Infof("External gateway pod: %s, detected for namespace(s) %s", pod.Name, routingNamespaceAnnotation)
	for _, namespace := range strings.Split(routingNamespaceAnnotation, ",") {
		nsInfo, err := oc.waitForNamespaceLocked(namespace)
		if err != nil {
			klog.Error(err)
			continue
		}
		// check if any gateways were stored for this pod
		foundGws := nsInfo.routingExternalPodGWs[pod.Name]
		if foundGws == nil {
			klog.Infof("No gateways found to remove for annotated gateway pod: %s on namespace: %s",
				pod.Name, namespace)
			nsInfo.Unlock()
			continue
		}

		for _, gwIP := range foundGws {
			// check for previously configured pod routes
			for podIP, gwInfo := range nsInfo.podExternalRoutes {
				if len(gwInfo) == 0 {
					continue
				}
				gr := gwInfo[gwIP.String()]
				if gr == "" {
					continue
				}
				mask := GetIPFullMask(podIP)
				// TODO (trozet): use the go bindings here and batch commands
				_, stderr, err := util.RunOVNNbctl("--", "--if-exists", "--policy=src-ip",
					"lr-route-del", gr, podIP+mask, gwIP.String())
				if err != nil {
					klog.Errorf("Unable to delete pod %s route to GR %s, GW: %s, stderr:%q, err:%v",
						pod.Name, gr, gwIP.String(), stderr, err)
				} else {
					klog.V(5).Infof("ECMP route deleted for pod: %s, on gr: %s, to gw: %s", pod.Name,
						gr, gwIP.String())
					delete(nsInfo.podExternalRoutes[podIP], gwIP.String())
					// clean up if there are no more routes for this podIP
					if entry := nsInfo.podExternalRoutes[podIP]; len(entry) == 0 {
						delete(nsInfo.podExternalRoutes, podIP)
					}
				}
			}
		}
		delete(nsInfo.routingExternalPodGWs, pod.Name)
		nsInfo.Unlock()
		klog.Infof("pod: %s, removed as external gateway for namespace %s", pod.Name, namespace)
	}
}
