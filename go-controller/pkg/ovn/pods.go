package ovn

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

func (oc *DefaultNetworkController) syncPods(pods []interface{}) error {
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	//
	// TBD: Before this succeeds, add Pod handler should not continue to allocate IPs for the new Pods.
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("spurious object in syncPods: %v", podInterface)
		}
		annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, ovntypes.DefaultNetworkName)
		if err != nil {
			continue
		}
		expectedLogicalPortName, err := oc.allocatePodIPs(pod, annotations)
		if err != nil {
			return err
		}
		if expectedLogicalPortName != "" {
			expectedLogicalPorts[expectedLogicalPortName] = true
		}

		// delete the outdated hybrid overlay subnet route if it exists
		newRoutes := []util.PodRoute{}
		switchName := pod.Spec.NodeName
		for _, subnet := range oc.lsManager.GetSwitchSubnets(switchName) {
			hybridOverlayIFAddr := util.GetNodeHybridOverlayIfAddr(subnet).IP
			for _, route := range annotations.Routes {
				if !route.NextHop.Equal(hybridOverlayIFAddr) {
					newRoutes = append(newRoutes, route)
				}
			}
		}
		// checking the length because cannot compare the slices directly and if routes are removed
		// the length will be different
		if len(annotations.Routes) != len(newRoutes) {
			annotations.Routes = newRoutes
			err = oc.updatePodAnnotationWithRetry(pod, annotations, ovntypes.DefaultNetworkName)
			if err != nil {
				return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
			}
		}
	}
	// all pods present before ovn-kube startup have been processed
	atomic.StoreUint32(&oc.allInitialPodsProcessed, 1)

	return oc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}

func (oc *DefaultNetworkController) deleteLogicalPort(pod *kapi.Pod, portInfo *lpInfo) (err error) {
	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)

	if err = oc.deletePodExternalGW(pod); err != nil {
		return fmt.Errorf("unable to delete external gateway routes for pod %s: %w", podDesc, err)
	}
	if pod.Spec.HostNetwork {
		return nil
	}
	if !util.PodScheduled(pod) {
		return nil
	}

	pInfo, err := oc.deletePodLogicalPort(pod, portInfo)
	if err != nil {
		return err
	}

	// do not remove SNATs/GW routes/IPAM for an IP address unless we have validated no other pod is using it
	if pInfo == nil {
		return nil
	}

	if config.Gateway.DisableSNATMultipleGWs {
		if err := deletePodSNAT(oc.nbClient, pInfo.logicalSwitch, []*net.IPNet{}, pInfo.ips); err != nil {
			return fmt.Errorf("cannot delete GR SNAT for pod %s: %w", podDesc, err)
		}
	}
	podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if err := oc.deleteGWRoutesForPod(podNsName, pInfo.ips); err != nil {
		return fmt.Errorf("cannot delete GW Routes for pod %s: %w", podDesc, err)
	}

	// Releasing IPs needs to happen last so that we can deterministically know that if delete failed that
	// the IP of the pod needs to be released. Otherwise we could have a completed pod failed to be removed
	// and we dont know if the IP was released or not, and subsequently could accidentally release the IP
	// while it is now on another pod. Releasing IPs may fail at this point if cache knows nothing about it,
	// which is okay since node may have been deleted.
	klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s", pod.Namespace, pod.Name,
		util.JoinIPNetIPs(pInfo.ips, " "))
	return oc.releasePodIPs(pInfo)
}

func (oc *DefaultNetworkController) addLogicalPort(pod *kapi.Pod) (err error) {
	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	switchName := pod.Spec.NodeName
	if oc.lsManager.IsNonHostSubnetSwitch(switchName) {
		return nil
	}

	network, err := util.GetK8sPodDefaultNetwork(pod)
	if err != nil {
		return fmt.Errorf("error getting default-network's network-attachment: %v", err)
	}

	var libovsdbExecuteTime time.Duration
	var lsp *nbdb.LogicalSwitchPort
	var ops []ovsdb.Operation
	var podAnnotation *util.PodAnnotation
	var newlyCreatedPort bool
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v, libovsdb time %v: %v",
			pod.Namespace, pod.Name, time.Since(start), libovsdbExecuteTime)
	}()

	ops, lsp, podAnnotation, newlyCreatedPort, err = oc.addLogicalPortToNetwork(pod, network)
	if err != nil {
		return err
	}

	// Ensure the namespace/nsInfo exists
	routingExternalGWs, routingPodGWs, addOps, err := oc.addPodToNamespace(pod.Namespace, podAnnotation.IPs)
	if err != nil {
		return err
	}
	ops = append(ops, addOps...)

	// if we have any external or pod Gateways, add routes
	gateways := make([]*gatewayInfo, 0, len(routingExternalGWs.gws)+len(routingPodGWs))

	if len(routingExternalGWs.gws) > 0 {
		gateways = append(gateways, routingExternalGWs)
	}
	for key := range routingPodGWs {
		gw := routingPodGWs[key]
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
		err = oc.addGWRoutesForPod(gateways, podAnnotation.IPs, podNsName, pod.Spec.NodeName)
		if err != nil {
			return err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set and does not have
		// namespace annotations to go through external egress router
		if extIPs, err := getExternalIPsGR(oc.watchFactory, pod.Spec.NodeName); err != nil {
			return err
		} else if ops, err = oc.addOrUpdatePodSNATReturnOps(pod.Spec.NodeName, extIPs, podAnnotation.IPs, ops); err != nil {
			return err
		}
	}

	recordOps, txOkCallBack, _, err := metrics.GetConfigDurationRecorder().AddOVN(oc.nbClient, "pod", pod.Namespace,
		pod.Name)
	if err != nil {
		klog.Errorf("Config duration recorder: %v", err)
	}
	ops = append(ops, recordOps...)

	transactStart := time.Now()
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(oc.nbClient, lsp, ops)
	libovsdbExecuteTime = time.Since(transactStart)
	if err != nil {
		return fmt.Errorf("error transacting operations %+v: %v", ops, err)
	}
	txOkCallBack()
	oc.podRecorder.AddLSP(pod.UID)

	// check if this pod is serving as an external GW
	err = oc.addPodExternalGW(pod)
	if err != nil {
		return fmt.Errorf("failed to handle external GW check: %v", err)
	}

	// if somehow lspUUID is empty, there is a bug here with interpreting OVSDB results
	if len(lsp.UUID) == 0 {
		return fmt.Errorf("UUID is empty from LSP: %+v", *lsp)
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(switchName, lsp.Name, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)

	// If multicast is allowed and enabled for the namespace, add the port to the allow policy.
	// FIXME: there's a race here with the Namespace multicastUpdateNamespace() handler, but
	// it's rare and easily worked around for now.
	ns, err := oc.watchFactory.GetNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if oc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
		if err := podAddAllowMulticastPolicy(oc.nbClient, pod.Namespace, portInfo); err != nil {
			return err
		}
	}
	//observe the pod creation latency metric for newly created pods only
	if newlyCreatedPort {
		metrics.RecordPodCreated(pod)
	}
	return nil
}
