// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

func (oc *DefaultNetworkController) syncPods(pods []interface{}) error {
	annotatedLocalPods := map[*corev1.Pod]map[string]*util.PodAnnotation{}
	var allHostSubnets []*net.IPNet

	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	//
	// TBD: Before this succeeds, add Pod handler should not continue to allocate IPs for the new Pods.
	expectedLogicalPorts := make(map[string]bool)
	vms := make(map[ktypes.NamespacedName]bool)
	var err error
	switchesNotFound := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("spurious object in syncPods: %v", podInterface)
		}

		expectedLogicalPortName := ""
		var annotations *util.PodAnnotation
		if kubevirt.IsPodLiveMigratable(pod) {
			vms, expectedLogicalPortName, annotations, err = oc.allocateSyncMigratablePodIPsOnZone(vms, pod)
			if err != nil {
				return err
			}
		} else if oc.isPodScheduledOnLocalNode(pod) {
			if switchesNotFound[pod.Spec.NodeName] {
				klog.Warningf("Cannot allocate IPs for %s/%s, node was not found after 30 seconds", pod.Namespace, pod.Name)
				continue
			}
			expectedLogicalPortName, annotations, err = oc.allocateSyncPodsIPs(pod)

			if errors.Is(err, errNodeNotFound) {
				klog.Warningf("Cannot allocate IPs for %s/%s, node was not found after 30 seconds", pod.Namespace, pod.Name)
				switchesNotFound[pod.Spec.NodeName] = true
				continue
			}
			if err != nil {
				return err
			}
		} else {
			continue
		}

		if annotations == nil {
			continue
		}

		// We track which pods are allocated on startup to check which might
		// have already been released. We track local non-migratable pods and
		// live migratable pods that have an IP allocation from a local subnet
		// (assigned to a node belonging to this zone) or from an unassigned
		// subnet (previously assigned to a node that has been since removed)
		var zoneContainsPodSubnetOrUntracked bool
		if kubevirt.IsPodLiveMigratable(pod) {
			allHostSubnets, zoneContainsPodSubnetOrUntracked, err = kubevirt.ZoneContainsPodSubnetOrUntracked(
				oc.watchFactory,
				oc.lsManager,
				allHostSubnets,
				annotations)
			if err != nil {
				return err
			}
		}
		if kubevirt.IsPodLiveMigratable(pod) && !zoneContainsPodSubnetOrUntracked {
			continue
		}
		annotatedLocalPods[pod] = map[string]*util.PodAnnotation{types.DefaultNetworkName: annotations}

		if expectedLogicalPortName != "" {
			expectedLogicalPorts[expectedLogicalPortName] = true
		}

		// only update annotations for pods belonging to my zone
		if oc.isPodScheduledOnLocalNode(pod) {
			// delete the outdated hybrid overlay subnet route if it exists
			newRoutes := []util.PodRoute{}
			// HO is IPv4 only
			ipv4Subnets := util.MatchAllIPNetFamily(false, oc.lsManager.GetSwitchSubnets(pod.Spec.NodeName))
			for _, route := range annotations.Routes {
				if !util.IsNodeHybridOverlayIfAddr(route.NextHop, ipv4Subnets) {
					newRoutes = append(newRoutes, route)
				}
			}

			syncPodAnnotations := false

			// checking the length because cannot compare the slices directly and if routes are removed
			// the length will be different
			if len(annotations.Routes) != len(newRoutes) {
				annotations.Routes = newRoutes
				syncPodAnnotations = true
			}
			// the ovn pod annotation role field is mandatory for default pod network, update old pods
			// it it's missing
			if util.IsNetworkSegmentationSupportEnabled() {
				if annotations.Role == "" {
					// Hardcode directly to primary since we don't support primary role networks at namespaces with
					// pods in it, do not make sense to call GetNetworkRole here.
					annotations.Role = types.NetworkRolePrimary
					syncPodAnnotations = true
				}
			}
			if syncPodAnnotations {
				err = oc.updatePodAnnotationWithRetry(pod, annotations, types.DefaultNetworkName)
				if err != nil {
					return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
				}
			}
		}
	}
	// all pods present before ovn-kube startup have been processed
	atomic.StoreUint32(&oc.allInitialPodsProcessed, 1)

	if config.HybridOverlay.Enabled {
		// allocate all previously annoted hybridOverlay Distributed Router IP addresses. Allocation needs to happen here
		// before a Pod Add event can be processed and be allocated a previously assigned hybridOverlay Distributed Router IP address.
		// we do not support manually setting the hybrid overlay DRIP address
		node, err := oc.GetLocalNode()
		if err != nil {
			return err
		}
		// Allocation also happens during Add/Update Node events. Allocate any address
		// retained from a previous run before pod events can claim it.
		if _, ok := node.Annotations[hotypes.HybridOverlayDRIP]; ok {
			if err := oc.allocateHybridOverlayDRIP(node); err != nil {
				return fmt.Errorf("cannot allocate hybridOverlay DRIP on node %s (%v)", node.Name, err)
			}
		}
	}
	if err := kubevirt.SyncVirtualMachines(oc.nbClient, vms, types.DefaultNetworkControllerName); err != nil {
		return fmt.Errorf("failed syncing running virtual machines: %v", err)
	}

	// keep track of which pods might have already been released
	oc.trackPodsReleasedBeforeStartup(annotatedLocalPods)

	return oc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}

func (oc *DefaultNetworkController) deleteLogicalPort(pod *corev1.Pod, portInfo *lpInfo) (err error) {
	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)

	if pod.Spec.HostNetwork {
		return nil
	}

	pInfo, err := oc.deletePodLogicalPort(pod, portInfo, types.DefaultNetworkName)
	if err != nil {
		return err
	}

	// delete open port ACLs for UDN pods
	if util.IsNetworkSegmentationSupportEnabled() {
		// safe to call for non-UDN pods
		err = oc.setUDNPodOpenPorts(pod.Namespace+"/"+pod.Name, pod.Annotations, "")
		if err != nil {
			return fmt.Errorf("failed to cleanup UDN pod %s/%s open ports: %w", pod.Namespace, pod.Name, err)
		}
	}

	// do not remove SNATs/GW routes/IPAM for an IP address unless we have validated no other pod is using it
	if pInfo == nil {
		return nil
	}

	if config.Gateway.DisableSNATMultipleGWs {
		if err := oc.deletePodSNAT(pInfo.logicalSwitch, []*net.IPNet{}, pInfo.ips); err != nil {
			return fmt.Errorf("cannot delete GR SNAT for pod %s: %w", podDesc, err)
		}
	}
	podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if err := oc.deleteGWRoutesForPod(podNsName, pInfo.ips); err != nil {
		return fmt.Errorf("cannot delete GW Routes for pod %s: %w", podDesc, err)
	}

	if kubevirt.IsPodLiveMigratable(pod) {
		switchName, hasLocalIPs := oc.lsManager.GetSubnetName(pInfo.ips)
		// don't attempt to release IPs that are not managed by this zone which can
		// happen with live migratable pods, otherwise we would get distracting
		// error logs on release
		if !hasLocalIPs {
			klog.V(5).Infof("Inhibiting release of live migratable pod %s/%s IPs %s not managed by this zone",
				pod.Namespace, pod.Name,
				util.JoinIPNetIPs(pInfo.ips, " "),
			)
			return nil
		}
		// a pod might have migrated from one node to another node in the same
		// zone so fix the switch for which the release needs to happen
		pInfo.logicalSwitch = switchName
	}

	// Releasing IPs needs to happen last so that we can deterministically know that if delete failed that
	// the IP of the pod needs to be released. Otherwise we could have a completed pod failed to be removed
	// and we dont know if the IP was released or not, and subsequently could accidentally release the IP
	// while it is now on another pod. Releasing IPs may fail at this point if cache knows nothing about it,
	// which is okay since node may have been deleted.
	klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s", pod.Namespace, pod.Name,
		util.JoinIPNetIPs(pInfo.ips, " "))
	return oc.releasePodIPsOnce(pod, types.DefaultNetworkName, pInfo)
}

func (oc *DefaultNetworkController) addLogicalPort(pod *corev1.Pod) (err error) {
	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	switchName := pod.Spec.NodeName
	if oc.lsManager.IsNonHostSubnetSwitch(switchName) {
		return nil
	}

	_, networkMap, err := util.GetDefaultPodNADToNetworkMapping(pod)
	if err != nil {
		// multus won't add this Pod if this fails, should never happen
		return fmt.Errorf("error getting default-network's network-attachment for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	// for default network, networkMap either is empty or contains only one network selection element
	if len(networkMap) > 1 {
		return fmt.Errorf("more than one NAD requested on default network for pod %s/%s", pod.Namespace, pod.Name)
	}

	var network *nadapi.NetworkSelectionElement
	for _, network = range networkMap {
		break
	}

	var libovsdbExecuteTime time.Duration
	var lsp *nbdb.LogicalSwitchPort
	var ops []ovsdb.Operation
	var podAnnotation *util.PodAnnotation
	var newlyCreatedPort bool
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v, libovsdb time %v",
			pod.Namespace, pod.Name, time.Since(start), libovsdbExecuteTime)
	}()

	nadKey := types.DefaultNetworkName
	ops, lsp, podAnnotation, newlyCreatedPort, err = oc.addLogicalPortToNetwork(pod, nadKey, network, nil)
	if err != nil {
		return err
	}

	// If default network is not primary, update secondaryPods port group to isolate default network
	networkRole, err := oc.GetNetworkRole(pod)
	if err != nil {
		return err
	}
	if networkRole == types.NetworkRoleInfrastructure && util.IsNetworkSegmentationSupportEnabled() {
		pgName := libovsdbutil.GetPortGroupName(oc.getSecondaryPodsPortGroupDbIDs())
		if ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, pgName, lsp.UUID); err != nil {
			return err
		}
		// set open ports for UDN pods, use function without transact, since lsp is not created yet.
		var parseErr error
		ops, parseErr, err = oc.setUDNPodOpenPortsOps(pod.Namespace+"/"+pod.Name, pod.Annotations, lsp.Name, ops)
		err = utilerrors.Join(parseErr, err)
		if err != nil {
			return fmt.Errorf("failed to set UDN pod %s/%s open ports: %w", pod.Namespace, pod.Name, err)
		}
	}

	// Ensure namespace port group membership before pod setup succeeds.
	ops, err = oc.addPodToNamespacePortGroupOps(ops, pod.Namespace, lsp.UUID)
	if err != nil {
		return err
	}

	if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set. External gateway routes
		// (and the associated per-pod SNAT removal) are programmed by the
		// AdminPolicyBasedExternalRoute controller.
		snatOps, err := oc.AddPodSNATOps(pod.Spec.NodeName, podAnnotation.IPs)
		if err != nil {
			return err
		}
		ops = append(ops, snatOps...)
	}

	recordOps, txOkCallBack, _, err := oc.AddConfigDurationRecord("pod", pod.Namespace, pod.Name)
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
	oc.podRecorder.AddLSP(pod.UID, oc.GetNetInfo())

	// if somehow lspUUID is empty, there is a bug here with interpreting OVSDB results
	if len(lsp.UUID) == 0 {
		return fmt.Errorf("UUID is empty from LSP: %+v", *lsp)
	}

	// Add the pod's logical switch port to the port cache
	_ = oc.logicalPortCache.add(pod, switchName, types.DefaultNetworkName, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)
	if oc.onLogicalPortCacheAdd != nil {
		oc.onLogicalPortCacheAdd(pod, types.DefaultNetworkName)
	}

	if kubevirt.IsPodLiveMigratable(pod) {
		if err := oc.ensureDHCP(pod, podAnnotation, lsp); err != nil {
			return fmt.Errorf("failed configuring DHCP for default network at pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// observe the pod creation latency metric for newly created pods only
	if newlyCreatedPort {
		metrics.RecordPodCreated(pod, oc.GetNetInfo())
	}
	return nil
}

func (oc *DefaultNetworkController) allocateSyncPodsIPs(pod *corev1.Pod) (string, *util.PodAnnotation, error) {
	annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return "", nil, nil
	}
	expectedLogicalPortName, err := oc.allocatePodIPsOnSwitch(pod, annotations, oc.GetNetworkName(), oc.GetNetworkScopedSwitchName(pod.Spec.NodeName))
	if err != nil {
		return "", nil, err
	}
	return expectedLogicalPortName, annotations, nil
}

func (oc *DefaultNetworkController) allocateSyncMigratablePodIPsOnZone(vms map[ktypes.NamespacedName]bool, pod *corev1.Pod) (map[ktypes.NamespacedName]bool, string, *util.PodAnnotation, error) {
	allocatePodIPsOnSwitchWrapFn := func(liveMigratablePod *corev1.Pod, liveMigratablePodAnnotation *util.PodAnnotation, switchName, nadKey string) (string, error) {
		return oc.allocatePodIPsOnSwitch(liveMigratablePod, liveMigratablePodAnnotation, switchName, nadKey)
	}
	vmKey, expectedLogicalPortName, podAnnotation, err := kubevirt.AllocateSyncMigratablePodIPsOnZone(oc.watchFactory, oc.lsManager, oc.GetNetworkName(), pod, allocatePodIPsOnSwitchWrapFn)
	if err != nil {
		return nil, "", nil, err
	}

	// If there is a vmKey this VM is not stale so it should be in sync
	isLocalPod := oc.isPodScheduledOnLocalNode(pod)
	if vmKey != nil {
		vms[*vmKey] = isLocalPod
	}

	// For remote pods we the logical switch port is not present so
	// empty expectedLogicalPortName is returned
	if !isLocalPod {
		expectedLogicalPortName = ""
	}

	return vms, expectedLogicalPortName, podAnnotation, nil
}
