// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"golang.org/x/exp/maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sretry "k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	subnetipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	podallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/pod"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (bnc *BaseNetworkController) allocatePodIPs(pod *corev1.Pod,
	annotations *util.PodAnnotation, nadKey string) (expectedLogicalPortName string, err error) {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return "", err
	}
	return bnc.allocatePodIPsOnSwitch(pod, annotations, nadKey, switchName)
}

var errNodeNotFound = errors.New("node not found")

// allocatePodIPsForSwitch will allocate the the ip from pod annotation at
// a specified switch, this switch can be different than the one the pod is
// attachted to, for example hypershift kubevirt provider live migration.
func (bnc *BaseNetworkController) allocatePodIPsOnSwitch(pod *corev1.Pod,
	annotations *util.PodAnnotation, nadKey string, switchName string) (expectedLogicalPortName string, err error) {

	// Completed pods will be allocated as well to avoid having their IPs
	// allocated to other pods before we make sure that we have released them
	// first.
	// See trackReleasedPodsBeforeStartup for additional tracking that
	// happens to avoid relasing IPs for completed pods that were already
	// released before startup.
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
		return "", nil
	}

	// skip nodes that are not running ovnk (inferred from host subnets and
	// topology type)
	if bnc.isNonHostSubnetSwitch(switchName) {
		return "", nil
	}

	expectedLogicalPortName = bnc.GetLogicalPortName(pod, nadKey)

	// For IPAM-less networks (e.g., localnet without subnet ), skip IP allocation.
	// The logical port should still be tracked to prevent deletion during sync.
	if !bnc.doesNetworkRequireIPAM() {
		return expectedLogicalPortName, nil
	}

	// it is possible to try to add a pod here that has no node. For example if a pod was deleted with
	// a finalizer, and then the node was removed. In this case the pod will still exist in a running state.
	// Terminating pods should still have network connectivity for pre-stop hooks or termination grace period
	if _, err := bnc.watchFactory.GetNode(pod.Spec.NodeName); apierrors.IsNotFound(err) &&
		bnc.lsManager.GetSwitchSubnets(switchName) == nil {
		if util.PodTerminating(pod) {
			klog.Infof("Ignoring IP allocation for terminating pod: %s/%s, on deleted "+
				"node: %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
			return expectedLogicalPortName, nil
		} else if bnc.doesNetworkRequireIPAM() {
			// unknown condition how we are getting a non-terminating pod without a node here
			klog.Errorf("Pod IP allocation found for a non-existent node in API with unknown "+
				"condition. Pod: %s/%s, node: %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		}
	}
	if err := bnc.waitForNodeLogicalSwitchSubnetsInCache(switchName); err != nil {
		return expectedLogicalPortName, err
	}
	if err = bnc.lsManager.AllocateIPs(switchName, annotations.IPs); err != nil {
		if err == ipallocator.ErrAllocated {
			// already allocated: log a warning but not stop syncPod from continuing
			klog.Warningf("Already allocated IPs: %s for pod: %s in phase: %v on switch: %s",
				util.JoinIPNetIPs(annotations.IPs, " "), expectedLogicalPortName,
				&pod.Status.Phase, switchName)
		} else {
			return expectedLogicalPortName, fmt.Errorf("couldn't allocate IPs: %s for pod: %s on switch: %s"+
				" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), expectedLogicalPortName,
				switchName, err)
		}
	}
	return expectedLogicalPortName, nil
}

func (bnc *BaseNetworkController) deleteStaleLogicalSwitchPorts(expectedLogicalPorts map[string]bool) error {
	var switchName string

	// Get the switch that Pod logical ports reside on.
	topoType := bnc.TopologyType()
	if !bnc.IsUserDefinedNetwork() || topoType == ovntypes.Layer3Topology {
		// For default network and layer3 topology type networks, get the local node switch.
		node, err := bnc.GetLocalNode()
		if err != nil {
			return err
		}
		// Skip nodes that are not running ovnk (inferred from host subnets).
		switchName = bnc.GetNetworkScopedSwitchName(node.Name)
		if bnc.isNonHostSubnetSwitch(switchName) {
			return nil
		}
	} else if topoType == ovntypes.Layer2Topology {
		switchName = bnc.GetNetworkScopedSwitchName(ovntypes.OVNLayer2Switch)
	} else if topoType == ovntypes.LocalnetTopology {
		switchName = bnc.GetNetworkScopedSwitchName(ovntypes.OVNLocalnetSwitch)
	} else {
		return fmt.Errorf("topology type %s not supported", topoType)
	}

	return bnc.deleteStaleLogicalSwitchPortsOnSwitch(switchName, expectedLogicalPorts)
}

func (bnc *BaseNetworkController) deleteStaleLogicalSwitchPortsOnSwitch(switchName string,
	expectedLogicalPorts map[string]bool) error {
	p := func(item *nbdb.LogicalSwitchPort) bool {
		return item.ExternalIDs["pod"] == "true" && !expectedLogicalPorts[item.Name]
	}
	sw := nbdb.LogicalSwitch{
		Name: switchName,
	}

	ops, err := libovsdbops.DeleteLogicalSwitchPortsWithPredicateOps(bnc.nbClient, nil, &sw, p)
	if err != nil {
		return fmt.Errorf("could not generate ops to delete stale ports from logical switch %s (%+v)", switchName, err)
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("could not remove stale logicalPorts from switch %s for network %s (%+v)", switchName, bnc.GetNetworkName(), err)
	}
	return nil
}

// lookupPortUUIDAndSwitchName will use libovsdb to locate the logical switch port uuid as well as the logical switch
// that owns such port (aka nodeName), based on the logical port name.
func (bnc *BaseNetworkController) lookupPortUUIDAndSwitchName(logicalPort string) (portUUID string, logicalSwitch string, err error) {
	lsp := &nbdb.LogicalSwitchPort{Name: logicalPort}
	lsp, err = libovsdbops.GetLogicalSwitchPort(bnc.nbClient, lsp)
	if err != nil {
		return "", "", err
	}
	p := func(item *nbdb.LogicalSwitch) bool {
		for _, currPortUUID := range item.Ports {
			if currPortUUID == lsp.UUID {
				return true
			}
		}
		return false
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(bnc.nbClient, p)
	if err != nil {
		return "", "", fmt.Errorf("failed to get node logical switch for logical port %s (%s): %w", logicalPort, lsp.UUID, err)
	}
	if len(nodeSwitches) != 1 {
		return "", "", fmt.Errorf("found %d node logical switch for logical port %s (%s)", len(nodeSwitches), logicalPort, lsp.UUID)
	}
	return lsp.UUID, nodeSwitches[0].Name, nil
}

func (bnc *BaseNetworkController) deletePodLogicalPort(pod *corev1.Pod, portInfo *lpInfo,
	nadKey string) (*lpInfo, error) {
	var portUUID, switchName, logicalPort string
	var podIfAddrs []*net.IPNet

	expectedSwitchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, err
	}

	podDesc := fmt.Sprintf("pod %s/%s/%s", nadKey, pod.Namespace, pod.Name)
	logicalPort = bnc.GetLogicalPortName(pod, nadKey)
	if portInfo == nil {
		// If ovnkube-controller restarts, it is also possible the Pod's logical switch port
		// is not re-added into the cache. Delete logical switch port anyway.
		annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				// if the annotation doesn’t exist, that’s not an error. It means logical port does not need to be deleted.
				klog.V(5).Infof("No annotations on %s, no need to delete its logical port: %s", podDesc, logicalPort)
				return nil, nil
			}
			return nil, fmt.Errorf("unable to unmarshal pod annotations for %s: %w", podDesc, err)
		}

		// Since portInfo is not available, use ovn to locate the logical switch (named after the node name) for the logical port.
		portUUID, switchName, err = bnc.lookupPortUUIDAndSwitchName(logicalPort)
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return nil, fmt.Errorf("unable to locate portUUID+switchName for %s: %w", podDesc, err)
			}
			// The logical port no longer exists in OVN. The caller expects this function to be idem-potent,
			// so the proper action to take is to use an empty uuid and extract the node name from the pod spec.
			portUUID = ""
			switchName = expectedSwitchName
		}
		podIfAddrs = annotation.IPs

		klog.Warningf("No cached port info for deleting %s. Using logical switch %s port uuid %s and addrs %v",
			podDesc, switchName, portUUID, podIfAddrs)
	} else {
		portUUID = portInfo.uuid
		switchName = portInfo.logicalSwitch
		podIfAddrs = portInfo.ips
	}

	// Sanity check
	if switchName != expectedSwitchName {
		klog.Errorf("Deleting %s expecting switch name: %s, OVN DB has switch name %s for port uuid %s",
			podDesc, expectedSwitchName, switchName, portUUID)
	}

	// IP collision check only needed when this controller owns IPAM.
	// For L2/localnet, cluster-manager allocates IPs centrally.
	shouldRelease := false
	if bnc.allocatesPodAnnotation() {
		shouldRelease, err = bnc.shouldReleaseDeletedPod(pod, switchName, nadKey, podIfAddrs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine if ip should be released: %v", err)
		}
	}

	var allOps, ops []ovsdb.Operation

	if ops, err = bnc.deletePodFromNamespace(pod.Namespace,
		portUUID); err != nil {
		return nil, fmt.Errorf("unable to delete pod %s from namespace: %w", podDesc, err)
	}
	allOps = append(allOps, ops...)

	ops, err = bnc.delLSPOps(logicalPort, switchName, portUUID)
	// Tolerate cases where logical switch of the logical port no longer exist in OVN.
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, fmt.Errorf("failed to create delete ops for the lsp: %s: %s", logicalPort, err)
	}
	allOps = append(allOps, ops...)

	recordOps, txOkCallBack, _, err := bnc.AddConfigDurationRecord("pod", pod.Namespace, pod.Name)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	allOps = append(allOps, recordOps...)

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, allOps)
	if err != nil {
		return nil, fmt.Errorf("cannot delete logical switch port %s, %v", logicalPort, err)
	}
	txOkCallBack()

	// do not remove SNATs/GW routes/IPAM for an IP address unless we have validated no other pod is using it
	if !shouldRelease {
		return nil, nil
	}

	pInfo := lpInfo{
		name:          logicalPort,
		uuid:          portUUID,
		logicalSwitch: switchName,
		ips:           podIfAddrs,
	}
	return &pInfo, nil
}

// findPodWithIPAddresses finds any pods with the same IPs in a running state on the cluster
// If nodeName is provided, pods only belonging to the same node will be checked, unless this pod has
// potentially live migrated.
func findPodWithIPAddresses(watchFactory *factory.WatchFactory, netInfo util.NetInfo, needleIPs []net.IP, nodeName string, getNetworkNameForNADKey func(nadKey string) string, ignoredUID ktypes.UID) (*corev1.Pod, error) {
	allPods, err := watchFactory.GetAllPods()
	if err != nil {
		return nil, fmt.Errorf("unable to get pods: %w", err)
	}

	// iterate through all pods
	for _, p := range allPods {
		if ignoredUID != "" && p.UID == ignoredUID {
			continue
		}
		if util.PodCompleted(p) || util.PodWantsHostNetwork(p) || !util.PodScheduled(p) {
			continue
		}

		// If the network type is Layer 3, then IP allocation is per node, so we can filter pods by node name.
		// If pod is not applicable to live migration, restrict the search to only pods on the filtered node
		// This specifically speeds up a case where a pod may have been annotated by ovnkube-controller, but has not yet
		// returned from CNI ADD. In that case the GetPodIPsOfNetwork would unmarshal the annotation and take a perf
		// hit for no reason (since the IP cannot be in the same subnet as what we are looking for).
		if netInfo.TopologyType() == ovntypes.Layer3Topology && !kubevirt.IsPodLiveMigratable(p) && len(nodeName) > 0 && nodeName != p.Spec.NodeName {
			continue
		}

		// check if the pod addresses match in the OVN annotation
		haystackPodAddrs, err := util.GetPodIPsOfNetwork(p, netInfo, getNetworkNameForNADKey)
		if err != nil {
			continue
		}

		for _, haystackPodAddr := range haystackPodAddrs {
			for _, needleIP := range needleIPs {
				if haystackPodAddr.Equal(needleIP) {
					return p, nil
				}
			}
		}
	}

	return nil, nil
}

// canReleasePodIPs checks if the podIPs can be released or not.
func (bnc *BaseNetworkController) canReleasePodIPs(pod *corev1.Pod, podIfAddrs []*net.IPNet, nodeName string) (bool, error) {
	var needleIPs []net.IP
	for _, podIPNet := range podIfAddrs {
		needleIPs = append(needleIPs, podIPNet.IP)
	}

	collidingPod, err := findPodWithIPAddresses(bnc.watchFactory, bnc.GetNetInfo(), needleIPs, nodeName, bnc.getNetworkNameForNADKeyFunc(), pod.UID)
	if err != nil {
		return false, fmt.Errorf("unable to determine if pod IPs: %#v are in use by another pod :%w", podIfAddrs, err)

	}

	if collidingPod != nil {
		klog.Infof("Should not release IP address: %s. Detected another pod"+
			" using this IP: %s/%s", util.JoinIPNetIPs(podIfAddrs, " "), collidingPod.Namespace, collidingPod.Name)
		return false, nil
	}

	return true, nil
}

func (bnc *BaseNetworkController) releasePodIPs(pInfo *lpInfo) error {
	if err := bnc.lsManager.ReleaseIPs(pInfo.logicalSwitch, pInfo.ips); err != nil {
		if !errors.Is(err, logicalswitchmanager.SwitchNotFound) {
			return fmt.Errorf("cannot release IPs of port %s on switch %s: %w", pInfo.name, pInfo.logicalSwitch, err)
		}
		klog.Warningf("Ignoring release IPs failure of port %s on switch %s: %v", pInfo.name, pInfo.logicalSwitch, err)
	}
	return nil
}

// releasePodIPsOnce records progress independently of the informer and port
// cache. Later policy/NAD cleanup can fail after IPAM has already released the
// addresses; a retry must not release a different pod's new reservation.
func (bnc *BaseNetworkController) releasePodIPsOnce(pod *corev1.Pod, nadKey string, pInfo *lpInfo) error {
	bnc.podIPReleasesMutex.Lock()
	defer bnc.podIPReleasesMutex.Unlock()
	if bnc.podIPReleases[pod.UID].Has(nadKey) {
		return nil
	}
	if err := bnc.releasePodIPs(pInfo); err != nil {
		return err
	}
	if bnc.podIPReleases == nil {
		bnc.podIPReleases = make(map[ktypes.UID]sets.Set[string])
	}
	if bnc.podIPReleases[pod.UID] == nil {
		bnc.podIPReleases[pod.UID] = sets.New[string]()
	}
	bnc.podIPReleases[pod.UID].Insert(nadKey)
	return nil
}

func (bnc *BaseNetworkController) wasPodIPReleased(pod *corev1.Pod, nadKey string) bool {
	bnc.podIPReleasesMutex.Lock()
	defer bnc.podIPReleasesMutex.Unlock()
	return bnc.podIPReleases[pod.UID].Has(nadKey)
}

// forgetPodIPReleases retires progress after successful reconciliation. Passing
// a NAD only retires that receipt when a new allocation is acquired for it.
func (bnc *BaseNetworkController) forgetPodIPReleases(pod *corev1.Pod, nadKeys ...string) {
	bnc.podIPReleasesMutex.Lock()
	defer bnc.podIPReleasesMutex.Unlock()
	if len(nadKeys) == 0 {
		delete(bnc.podIPReleases, pod.UID)
		return
	}
	bnc.podIPReleases[pod.UID].Delete(nadKeys...)
	if len(bnc.podIPReleases[pod.UID]) == 0 {
		delete(bnc.podIPReleases, pod.UID)
	}
}

func (bnc *BaseNetworkController) waitForNodeLogicalSwitch(switchName string) (*nbdb.LogicalSwitch, error) {
	// Wait for the node logical switch to be created by the ClusterController and be present
	// in libovsdb's cache. The node switch will be created when the node's logical network infrastructure
	// is created by the node watch
	ls := &nbdb.LogicalSwitch{Name: switchName}
	if err := wait.PollUntilContextTimeout(context.Background(), 30*time.Millisecond, 30*time.Second, true, func(_ context.Context) (bool, error) {
		if subnets := bnc.lsManager.GetSwitchSubnets(switchName); subnets == nil {
			// A cache miss is expected while node topology reconciliation is still creating the switch.
			// Keep polling until it appears or the overall wait times out.
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for logical switch in logical switch cache %q subnet: %v", switchName, err)
	}
	return ls, nil
}

func (bnc *BaseNetworkController) waitForNodeLogicalSwitchSubnetsInCache(switchName string) error {
	// Wait for the node logical switch with IPAM to be created by the ClusterController
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	// This function is only invoked when IPAM is required.
	var subnets []*net.IPNet
	if err := wait.PollUntilContextTimeout(context.Background(), 30*time.Millisecond, 30*time.Second, true, func(_ context.Context) (bool, error) {
		subnets = bnc.lsManager.GetSwitchSubnets(switchName)
		return subnets != nil, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for logical switch %q subnet: %w", switchName, errNodeNotFound)
	}
	return nil
}

// podExpectedInLogicalCache returns true if pod should be added to oc.logicalPortCache.
// For some pods, like hostNetwork pods, overlay node pods, or completed pods waiting for them to be added
// to oc.logicalPortCache will never succeed.
func (bnc *BaseNetworkController) podExpectedInLogicalCache(pod *corev1.Pod) bool {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return false
	}
	return !util.PodWantsHostNetwork(pod) &&
		!bnc.isNonHostSubnetSwitch(switchName) &&
		!util.PodCompleted(pod)
}

func (bnc *BaseNetworkController) getExpectedSwitchName(pod *corev1.Pod) (string, error) {
	switchName := pod.Spec.NodeName
	if bnc.IsUserDefinedNetwork() {
		topoType := bnc.TopologyType()
		switch topoType {
		case ovntypes.Layer3Topology:
			switchName = bnc.GetNetworkScopedSwitchName(pod.Spec.NodeName)
		case ovntypes.Layer2Topology:
			switchName = bnc.GetNetworkScopedSwitchName(ovntypes.OVNLayer2Switch)
		case ovntypes.LocalnetTopology:
			switchName = bnc.GetNetworkScopedSwitchName(ovntypes.OVNLocalnetSwitch)
		default:
			return "", fmt.Errorf("topology type %s not supported", topoType)
		}
	}
	return switchName, nil
}

// ensurePodAnnotation will copy pod annotations at migratable pods related
// to the same virtual machine, for normal pods it will unmarshal and return
// it, also there returned boolean will be true if the pod subnet belong to
// controller's zone.
func (bnc *BaseNetworkController) ensurePodAnnotation(pod *corev1.Pod, nadKey string) (*util.PodAnnotation, bool, error) {
	if kubevirt.IsPodLiveMigratable(pod) {
		podAnnotation, err := kubevirt.EnsurePodAnnotationForVM(bnc.watchFactory, bnc.kube, pod, nadKey)
		if err != nil {
			return nil, false, err
		}
		if podAnnotation == nil {
			return nil, true, nil
		}
		// The live migrated pods will have the pod annotation but the switch manager running
		// at the node will not contain the switch as expected
		_, zoneContainsPodSubnet := kubevirt.ZoneContainsPodSubnet(bnc.lsManager, podAnnotation.IPs)
		return podAnnotation, zoneContainsPodSubnet, nil
	}
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return nil, true, nil
	}
	return podAnnotation, true, nil
}

func (bnc *BaseNetworkController) addLogicalPortToNetwork(pod *corev1.Pod, nadKey string,
	network *nadapi.NetworkSelectionElement, enable *bool) (ops []ovsdb.Operation,
	lsp *nbdb.LogicalSwitchPort, podAnnotation *util.PodAnnotation, newlyCreatedPort bool, err error) {
	var ls *nbdb.LogicalSwitch

	podDesc := fmt.Sprintf("%s/%s/%s", nadKey, pod.Namespace, pod.Name)
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("[%s] failed geting expected switch name when adding logical switch port: %v", podDesc, err)
	}

	// it is possible to try to add a pod here that has no node. For example if a pod was deleted with
	// a finalizer, and then the node was removed. In this case the pod will still exist in a running state.
	// Terminating pods should still have network connectivity for pre-stop hooks or termination grace period
	// We cannot wire a pod that has no node/switch, so retry again later
	if _, err := bnc.watchFactory.GetNode(pod.Spec.NodeName); apierrors.IsNotFound(err) &&
		bnc.lsManager.GetSwitchSubnets(switchName) == nil && bnc.doesNetworkRequireIPAM() {
		podState := "unknown"
		if util.PodTerminating(pod) {
			podState = "terminating"
		}
		return nil, nil, nil, false, fmt.Errorf("[%s/%s] Non-existent node: %s in API for pod with %s state",
			pod.Namespace, pod.Name, pod.Spec.NodeName, podState)
	}

	ls, err = bnc.waitForNodeLogicalSwitch(switchName)
	if err != nil {
		return nil, nil, nil, false, err
	}

	portName := bnc.GetLogicalPortName(pod, nadKey)
	klog.Infof("[%s] creating logical port %s for pod on switch %s", podDesc, portName, switchName)

	var addresses []string
	lspExist := false

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	lsp = &nbdb.LogicalSwitchPort{Name: portName}
	existingLSP, err := libovsdbops.GetLogicalSwitchPort(bnc.nbClient, lsp)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, nil, nil, false, fmt.Errorf("unable to get the lsp %s from the nbdb: %s", portName, err)
	}
	lspExist = !errors.Is(err, libovsdbclient.ErrNotFound)

	// Sanity check. If port exists, it should be in the logical switch obtained from the pod spec.
	if lspExist {
		portFound := false
		ls, err = libovsdbops.GetLogicalSwitch(bnc.nbClient, ls)
		if err != nil {
			return nil, nil, nil, false, fmt.Errorf("[%s] unable to find logical switch %s in NBDB",
				podDesc, switchName)
		}
		for _, currPortUUID := range ls.Ports {
			if currPortUUID == existingLSP.UUID {
				portFound = true
				break
			}
		}
		if !portFound {
			// This should never happen and indicates we failed to clean up an LSP for a pod that was recreated
			return nil, nil, nil, false, fmt.Errorf("[%s] failed to locate existing logical port %s (%s) in logical switch %s",
				podDesc, existingLSP.Name, existingLSP.UUID, switchName)
		}
	}

	var customFields []libovsdbops.ModelUpdateField

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
	if !lspExist || len(existingLSP.Options["iface-id-ver"]) != 0 {
		lsp.Options["iface-id-ver"] = string(pod.UID)
	}
	// let's calculate if this network controller's role for this pod
	// and pass that information while determining the podAnnotations
	networkRole, err := bnc.GetNetworkRole(pod)
	if err != nil {
		return nil, nil, nil, false, err
	}

	if networkRole == ovntypes.NetworkRoleNone {
		// pod not on this controller, nothing to do
		return nil, nil, nil, false, nil
	}

	// Bind the port to the node's chassis.
	// This is required for Layer 2 networks with remote ports. It also prevents
	// chassis ping-ponging if ovnkube-node isn't running correctly, hasn't
	// cleared iface-id for an old instance of this pod, and the pod got
	// rescheduled.
	var node *corev1.Node
	if !config.Kubernetes.DisableRequestedChassis {
		node, err = bnc.watchFactory.GetNode(pod.Spec.NodeName)
		if err != nil {
			return nil, nil, nil, false, err
		}
		chassisID, err := util.ParseNodeChassisIDAnnotation(node)
		if err != nil {
			if util.IsAnnotationNotSetError(err) {
				return nil, nil, nil, false, ovntypes.NewSuppressedError(err)
			}
			return nil, nil, nil, false, err
		}
		lsp.Options[libovsdbops.RequestedChassis] = chassisID
	}

	// Although we have different code to allocate the pod annotation for the
	// default network and user-defined networks, at the time of this writing they
	// are functionally equivalent and the only reason to keep them separated is
	// to make sure the UDN code has no bugs before we switch to
	// it for the default network as well. If at all possible, keep them
	// functionally equivalent going forward.
	var annotationUpdated bool
	// A successful release receipt means this attachment no longer owns the
	// annotation's old IPs. Require IPAM to reserve them again before the receipt
	// can be retired; ErrAllocated may now refer to a different pod.
	requireIPAMReservation := bnc.wasPodIPReleased(pod, nadKey)
	if bnc.IsUserDefinedNetwork() {
		podAnnotation, annotationUpdated, err = bnc.allocatePodAnnotationForUserDefinedNetwork(
			pod, existingLSP, nadKey, network, networkRole, requireIPAMReservation)
	} else {
		podAnnotation, annotationUpdated, err = bnc.allocatePodAnnotation(
			pod, existingLSP, podDesc, nadKey, network, networkRole, requireIPAMReservation)
	}

	if err != nil {
		return nil, nil, nil, false, err
	}

	// This NAD now owns an allocation again, including reattachment after a
	// partially successful teardown of the same pod UID.
	bnc.forgetPodIPReleases(pod, nadKey)

	lsp.Enabled = enable
	if lsp.Enabled != nil {
		customFields = append(customFields, libovsdbops.LogicalSwitchPortEnabled)
	}

	addresses = []string{podAnnotation.MAC.String()}
	for _, podIfAddr := range podAnnotation.IPs {
		addresses[0] = addresses[0] + " " + podIfAddr.IP.String()
	}

	macSecurityDisabled := bnc.MACSecurityMode() == ovntypes.MACSecurityModeDisabled
	if macSecurityDisabled {
		// steer frames destined to unknown MAC addresses to this port (e.g. nested VMs behind this port).
		addresses = append(addresses, "unknown")
	}

	// Skip address configuration if LSP is disabled since it will install
	// l2 look up flows that harms some topologies
	if lsp.Enabled == nil || *lsp.Enabled {
		// set addresses on the port
		// LSP addresses in OVN are a single space-separated value

		lsp.Addresses = addresses
		customFields = append(customFields, libovsdbops.LogicalSwitchPortAddresses)
	}

	// add external ids
	lsp.ExternalIDs = map[string]string{"namespace": pod.Namespace, "pod": "true"}
	if bnc.IsUserDefinedNetwork() {
		lsp.ExternalIDs[ovntypes.NetworkExternalID] = bnc.GetNetworkName()
		lsp.ExternalIDs[ovntypes.NADExternalID] = nadKey
		lsp.ExternalIDs[ovntypes.TopologyExternalID] = bnc.TopologyType()
	}

	// CNI depends on the flows from port security, delay setting it until end
	lsp.PortSecurity = addresses
	if macSecurityDisabled {
		// drop MAC/IP addresses restriction on this port.
		lsp.PortSecurity = nil
	}
	customFields = append(customFields, libovsdbops.LogicalSwitchPortPortSecurity)

	// On layer2 topology with interconnect, we need to add specific port config
	if bnc.isLayer2WithInterconnectTransport() {
		isRemotePort := !bnc.isPodScheduledOnLocalNode(pod)
		err = bnc.zoneICHandler.AddTransitPortConfig(isRemotePort, podAnnotation, lsp)
		if err != nil {
			return nil, nil, nil, false, err
		}
		if isRemotePort {
			customFields = append(customFields, libovsdbops.LogicalSwitchPortType)
		}
	}
	if len(lsp.Options) != 0 {
		customFields = append(customFields, libovsdbops.LogicalSwitchPortOptions)
	}
	ops, err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(bnc.nbClient, nil, ls, customFields, lsp)
	if err != nil {
		return nil, nil, nil, false,
			fmt.Errorf("error creating logical switch port %+v on switch %+v: %+v", *lsp, *ls, err)
	}

	return ops, lsp, podAnnotation, annotationUpdated && !lspExist, nil
}

func (bnc *BaseNetworkController) updatePodAnnotationWithRetry(origPod *corev1.Pod, podInfo *util.PodAnnotation, nadKey string) error {
	return util.UpdatePodAnnotationWithRetry(
		bnc.watchFactory.PodCoreInformer().Lister(),
		bnc.kube,
		origPod,
		podInfo,
		nadKey,
	)
}

// Given a switch, gets the next set of addresses (from the IPAM) for each of the node's
// subnets to assign to the new pod
func (bnc *BaseNetworkController) assignPodAddresses(switchName string) (net.HardwareAddr, []*net.IPNet, error) {
	var (
		podMAC   net.HardwareAddr
		podCIDRs []*net.IPNet
		err      error
	)

	if !bnc.doesNetworkRequireIPAM() {
		klog.V(5).Infof("layer2 topology without subnet; will only generate the MAC address for the pod NIC")
		mac, err := util.GenerateRandMAC()
		if err != nil {
			return nil, nil, err
		}
		return mac, nil, nil
	}
	podCIDRs, err = bnc.lsManager.AllocateNextIPs(switchName)
	if err != nil {
		return nil, nil, err
	}
	if len(podCIDRs) > 0 {
		podMAC = util.IPAddrToHWAddr(podCIDRs[0].IP)
	}
	return podMAC, podCIDRs, nil
}

// Given a logical switch port and the switch on which it is scheduled, get all
// addresses currently assigned to it including subnet masks.
func (bnc *BaseNetworkController) getPortAddresses(switchName string, existingLSP *nbdb.LogicalSwitchPort) (net.HardwareAddr, []*net.IPNet, error) {
	podMac, podIPs, err := libovsdbutil.ExtractPortAddresses(existingLSP)
	if err != nil {
		return nil, nil, err
	} else if podMac == nil || len(podIPs) == 0 {
		return nil, nil, nil
	}

	var podIPNets []*net.IPNet

	nodeSubnets := bnc.lsManager.GetSwitchSubnets(switchName)

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

// delLSPOps returns the ovsdb operations required to delete the given logical switch port (LSP)
func (bnc *BaseNetworkController) delLSPOps(logicalPort, switchName,
	lspUUID string) ([]ovsdb.Operation, error) {
	lsw := nbdb.LogicalSwitch{
		Name: switchName,
	}
	lsp := nbdb.LogicalSwitchPort{
		UUID: lspUUID,
		Name: logicalPort,
	}
	ops, err := libovsdbops.DeleteLogicalSwitchPortsOps(bnc.nbClient, nil, &lsw, &lsp)
	if err != nil {
		return nil, fmt.Errorf("error deleting logical switch port %+v from switch %+v: %w", lsp, lsw, err)
	}

	return ops, nil
}

func (bnc *BaseNetworkController) addPodToNamespacePortGroupOps(ops []ovsdb.Operation, ns, portUUID string) ([]ovsdb.Operation, error) {
	if !bnc.needNamespacedPortGroup() || portUUID == "" {
		return ops, nil
	}

	// Namespace handling owns group lifetime. A pod that outlives namespace
	// teardown must not recreate the group; missing dependencies are retried.
	pgName := bnc.getNamespacePortGroupName(ns)
	// The namespace handler may be only milliseconds behind pod setup. Retry
	// just this cache lookup briefly before falling back to a full pod retry.
	var membershipOps []ovsdb.Operation
	err := k8sretry.OnError(k8sretry.DefaultRetry, func(err error) bool {
		return errors.Is(err, libovsdbclient.ErrNotFound)
	}, func() error {
		var err error
		membershipOps, err = libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, ops, pgName, portUUID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add pod port %s to namespace port group %s: %w", portUUID, pgName, err)
	}
	return membershipOps, nil
}

func (bnc *BaseNetworkController) deletePodFromNamespace(ns string, portUUID string) ([]ovsdb.Operation, error) {
	if !bnc.needNamespacedPortGroup() || portUUID == "" {
		return nil, nil
	}

	pgName := bnc.getNamespacePortGroupName(ns)
	ops, err := libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, nil, pgName, portUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to delete pod port %s from namespace port group %s: %w", portUUID, pgName, err)
	}
	return ops, nil
}

// isPodScheduledOnLocalNode returns true when the pod is scheduled on the node
// managed by this controller.
func (bnc *BaseNetworkController) isPodScheduledOnLocalNode(pod *corev1.Pod) bool {
	return util.PodScheduled(pod) && pod.Spec.NodeName == bnc.nodeName
}

// WatchPods starts the watching of the Pod resource and calls back the appropriate handler logic
func (bnc *BaseNetworkController) WatchPods() error {
	if bnc.podHandler != nil {
		return nil
	}

	handler, err := bnc.retryPods.WatchResource()
	if err == nil {
		bnc.podHandler = handler
	}
	return err
}

func calculateStaticIPs(podDesc string, ips []string) ([]*net.IPNet, error) {
	var staticIPs []*net.IPNet
	klog.V(5).Infof("Pod %s requested static IPs: %s", podDesc, strings.Join(ips, ";"))
	for _, ip := range ips {
		ipAddr, ipNet, err := net.ParseCIDR(ip)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IP %s requested in annotation for pod %s: Error %v",
				ip, podDesc, err)
		}
		ipNet.IP = ipAddr
		staticIPs = append(staticIPs, ipNet)
	}

	return staticIPs, nil
}

func calculateStaticMAC(podDesc string, mac string) (net.HardwareAddr, error) {
	var err error
	var podMac net.HardwareAddr
	klog.V(5).Infof("Pod %s requested custom MAC: %s", podDesc, mac)
	podMac, err = net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
			mac, podDesc, err)
	}
	return podMac, nil
}

// allocatePodAnnotation and update the corresponding pod annotation.
func (bnc *BaseNetworkController) allocatePodAnnotation(pod *corev1.Pod, existingLSP *nbdb.LogicalSwitchPort, podDesc,
	nadKey string, network *nadapi.NetworkSelectionElement, networkRole string, requireIPAMReservation bool) (*util.PodAnnotation, bool, error) {
	var releaseIPs bool
	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet

	switchName := pod.Spec.NodeName

	podAnnotation, zoneContainsPodSubnet, err := bnc.ensurePodAnnotation(pod, nadKey)
	if err != nil {
		return nil, false, fmt.Errorf("unable to ensure pod annotation: %v", err)
	}

	// the IPs we allocate in this function need to be released back to the
	// IPAM pool if there is some error in any step of addLogicalPort past
	// the point the IPs were assigned via the IPAM manager.
	// this needs to be done only when releaseIPs is set to true (the case where
	// we truly have assigned podIPs in this call) AND when there is no error in
	// the rest of the functionality of addLogicalPort. It is important to use a
	// named return variable for defer to work correctly.
	defer func() {
		if releaseIPs && err != nil {
			if relErr := bnc.lsManager.ReleaseIPs(switchName, podIfAddrs); relErr != nil {
				klog.Errorf("Error when releasing IPs %s for switch: %s, err: %q",
					util.JoinIPNetIPs(podIfAddrs, " "), switchName, relErr)
			} else {
				klog.Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), switchName)
			}
		}
	}()

	if podAnnotation != nil {
		podMac = podAnnotation.MAC
		podIfAddrs = podAnnotation.IPs

		if bnc.doesNetworkRequireIPAM() {
			if zoneContainsPodSubnet {
				// ensure we have reserved the IPs in the annotation.
				// For live migratable pods, the IPs may belong to a different
				// node's subnet (the source node) during migration. In that case
				// skip allocation here.
				if kubevirt.IsPodLiveMigratable(pod) {
					if sn, ok := bnc.lsManager.GetSubnetName(podIfAddrs); !ok || sn != switchName {
						klog.V(5).Infof("Skipping IP allocation for live migratable pod %s: IPs %s belong to switch %s, not %s",
							podDesc, util.JoinIPNetIPs(podIfAddrs, " "), sn, switchName)
						return podAnnotation, false, nil
					}
				}
				if err = bnc.lsManager.AllocateIPs(switchName, podIfAddrs); err != nil &&
					(!ipallocator.IsErrAllocated(err) || requireIPAMReservation) {
					return nil, false, fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %w",
						podDesc, util.JoinIPNetIPs(podIfAddrs, " "), err)
				}
			}
			return podAnnotation, false, nil

		} else if len(podIfAddrs) > 0 {
			return nil, false, fmt.Errorf("IPAMless network with IPs present in the annotations; rejecting to handle this request")
		}
	}

	// It is possible that IPs have already been allocated for this pod and annotation has been updated, then the last
	// addLogicalPortToNetwork() failed afterwards. In the current retry attempt, if the input pod argument got from
	// the informer cache still lags behind, we would fail to get the updated pod annotation. Just continue to allocate
	// new IPs and this function will eventually fail in updatePodAnnotationWithRetry() with ErrOverridePodIPs
	// when it tries to override the pod IP annotation. Newly allocated IPs will be released then.
	if existingLSP != nil {
		// try to get the MAC and IPs from existing OVN port first
		podMac, podIfAddrs, err = bnc.getPortAddresses(switchName, existingLSP)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get pod addresses for pod %s on node: %s, err: %v",
				podDesc, switchName, err)
		}
	}
	needsNewMacOrIPAllocation := false

	// ensure we have reserved the IPs found in OVN
	if len(podIfAddrs) == 0 {
		needsNewMacOrIPAllocation = true
	} else if bnc.doesNetworkRequireIPAM() {
		if err = bnc.lsManager.AllocateIPs(switchName, podIfAddrs); err != nil &&
			(!ipallocator.IsErrAllocated(err) || requireIPAMReservation) {
			klog.Warningf("Unable to allocate IPs %s found on existing OVN port: %s, for pod %s on switch: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), bnc.GetLogicalPortName(pod, nadKey), podDesc, switchName, err)

			needsNewMacOrIPAllocation = true
		}
	}
	if needsNewMacOrIPAllocation {
		if network != nil && network.IPRequest != nil && !bnc.doesNetworkRequireIPAM() {
			klog.V(5).Infof("Will use static IP addresses for pod %s on a flatL2 topology without subnet defined", podDesc)
			podIfAddrs, err = calculateStaticIPs(podDesc, network.IPRequest)
			if err != nil {
				return nil, false, err
			}
			podMac = util.IPAddrToHWAddr(podIfAddrs[0].IP)
		} else {
			// Previous attempts to use already configured IPs failed, need to assign new
			generatedPodMac, generatedPodIfAddrs, err := bnc.assignPodAddresses(switchName)
			if err != nil {
				return nil, false, fmt.Errorf("failed to assign pod addresses for pod %s on switch: %s, err: %v",
					podDesc, switchName, err)
			}
			if podMac == nil {
				podMac = generatedPodMac
			}
			if len(generatedPodIfAddrs) > 0 {
				podIfAddrs = generatedPodIfAddrs
				podMac = generatedPodMac
			}
		}
	}

	releaseIPs = true
	// handle error cases separately first to ensure binding to err, otherwise the
	// defer will fail
	if network != nil && network.MacRequest != "" {
		podMac, err = calculateStaticMAC(podDesc, network.MacRequest)
		if err != nil {
			return nil, false, err
		}
	}
	podAnnotation = &util.PodAnnotation{
		IPs:  podIfAddrs,
		MAC:  podMac,
		Role: networkRole,
	}
	var nodeSubnets []*net.IPNet
	if nodeSubnets = bnc.lsManager.GetSwitchSubnets(switchName); nodeSubnets == nil && bnc.doesNetworkRequireIPAM() {
		return nil, false, fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, switch: %s",
			podDesc, switchName)
	}

	node, err := bnc.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return nil, false, err
	}

	err = podallocator.AddRoutesGatewayIP(bnc.GetNetInfo(), node, pod, podAnnotation, network)
	if err != nil {
		return nil, false, err
	}

	klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s",
		podIfAddrs, podMac, podAnnotation.Gateways)
	annoStart := time.Now()
	err = bnc.updatePodAnnotationWithRetry(pod, podAnnotation, nadKey)
	podAnnoTime := time.Since(annoStart)
	klog.Infof("[%s] addLogicalPort annotation time took %v", podDesc, podAnnoTime)
	if err != nil {
		return nil, false, err
	}
	releaseIPs = false

	return podAnnotation, true, nil
}

// allocatePodAnnotationForUserDefinedNetwork and update the corresponding pod
// annotation.
func (bnc *BaseNetworkController) allocatePodAnnotationForUserDefinedNetwork(pod *corev1.Pod, lsp *nbdb.LogicalSwitchPort,
	nadKey string, network *nadapi.NetworkSelectionElement, networkRole string, requireIPAMReservation bool) (*util.PodAnnotation, bool, error) {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, false, err
	}

	// In certain configurations, pod IP allocation is handled from cluster
	// manager so wait for it to allocate the IPs
	if !bnc.allocatesPodAnnotation() {
		podAnnotation, _ := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
		if !util.IsValidPodAnnotation(podAnnotation) {
			allocator := "cluster manager"
			if bnc.IPAMType() == ovntypes.IPAMTypeDHCP {
				allocator = "the CNI"
			}
			return nil, false, ovntypes.NewSuppressedError(fmt.Errorf(
				"failed to get PodAnnotation for %s/%s/%s, %s might have not allocated it yet",
				nadKey, pod.Namespace, pod.Name, allocator))
		}

		return podAnnotation, false, nil
	}

	if network == nil {
		network = &nadapi.NetworkSelectionElement{}
	}

	var reallocate bool
	if lsp != nil && len(network.IPRequest) == 0 {
		mac, ips, err := bnc.getPortAddresses(switchName, lsp)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get pod addresses for pod %s/%s/%s on node %s, err: %v",
				nadKey, pod.Namespace, pod.Name, switchName, err)
		}
		network.MacRequest = mac.String()
		network.IPRequest = util.StringSlice(ips)
		reallocate = true

		klog.V(5).Infof("Will attempt to use LSP IP addresses %v and mac %s for pod %s/%s/%s",
			network.IPRequest, network.MacRequest, nadKey, pod.Namespace, pod.Name)
	}

	var ipAllocator subnetipallocator.NamedAllocator
	if bnc.doesNetworkRequireIPAM() {
		ipAllocator = bnc.lsManager.ForSwitch(switchName)
	}
	node, err := bnc.watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get pod %s/%s/%s node %q: %w",
			nadKey, pod.Namespace, pod.Name, pod.Spec.NodeName, err)
	}
	updatedPod, podAnnotation, err := bnc.podAnnotationAllocator.AllocatePodAnnotation(
		ipAllocator,
		node,
		pod,
		nadKey,
		network,
		reallocate,
		requireIPAMReservation,
		networkRole,
	)

	if err != nil {
		if errors.Is(err, ipallocator.ErrFull) {
			bnc.recordPodErrorEvent(pod, err)
		}
		return nil, false, err
	}

	if updatedPod != nil {
		klog.V(5).Infof("Allocated IP addresses %v, mac address %s, gateways %v and routes %s for pod %s/%s on NAD key %s",
			util.StringSlice(podAnnotation.IPs),
			podAnnotation.MAC,
			util.StringSlice(podAnnotation.Gateways),
			util.StringSlice(podAnnotation.Routes),
			pod.Namespace, pod.Name, nadKey,
		)

		return podAnnotation, true, nil
	}

	return podAnnotation, false, nil
}

func (bnc *BaseNetworkController) allocatesPodAnnotation() bool {
	switch bnc.TopologyType() {
	case ovntypes.Layer2Topology:
		// on layer2 topologies, cluster manager allocates tunnel IDs, allocates
		// IPs if the network has IPAM, and sets the PodAnnotation
		return false
	case ovntypes.LocalnetTopology:
		// on localnet topologies with IPAM, cluster manager allocates IPs and
		// sets the PodAnnotation. On DHCP IPAM the CNI writes it (the MAC at
		// CNI ADD, the DHCP-learned IPs after the exchange).
		return !bnc.doesNetworkRequireIPAM() && bnc.IPAMType() != ovntypes.IPAMTypeDHCP
	}
	return true
}

func (bnc *BaseNetworkController) shouldReleaseDeletedPod(pod *corev1.Pod, switchName, nadKey string, podIfAddrs []*net.IPNet) (bool, error) {
	if bnc.wasPodIPReleased(pod, nadKey) {
		return false, nil
	}
	var err error
	if !bnc.IsUserDefinedNetwork() && kubevirt.IsPodLiveMigratable(pod) {
		allVMPodsAreCompleted, err := kubevirt.AllVMPodsAreCompleted(bnc.watchFactory.PodCoreInformer().Lister(), pod)
		if err != nil {
			return false, err
		}

		// Removing the the kubevirt stale pods should not de allocate the IPs
		// to ensure that new pods do not take them
		if !allVMPodsAreCompleted {
			return false, nil
		}
	}

	if len(podIfAddrs) == 0 {
		return true, nil
	}
	// Until its first release, an ordinary running pod still owns its IPs.
	// Retries are protected by the receipt above, without a cluster-wide scan.
	// Completed pods still need the existing collision guards.
	if !util.PodCompleted(pod) {
		return true, nil
	}

	if bnc.wasPodReleasedBeforeStartup(string(pod.UID), nadKey) {
		klog.Infof("Completed pod %s/%s was already released for NAD key %s before startup",
			pod.Namespace,
			pod.Name,
			nadKey,
		)
		return false, nil
	}

	// If this pod applies to live migration it could have migrated within the
	// zone so get the correct node name corresponding with the subnet. If the
	// subnet is not tracked within the zone, nodeName will be empty which
	// will force a lookup for all nodes.
	zoneOwnsSubnet := true
	nodeName := pod.Spec.NodeName
	if !bnc.IsUserDefinedNetwork() && kubevirt.IsPodLiveMigratable(pod) {
		switchName, zoneOwnsSubnet = bnc.lsManager.GetSubnetName(podIfAddrs)
	}
	if !zoneOwnsSubnet {
		// force shouldReleasePodIPs to search all nodes
		nodeName = ""
	}

	shouldReleasePodIPs := func() (bool, error) {
		shouldRelease, err := bnc.canReleasePodIPs(pod, podIfAddrs, nodeName)
		if err != nil {
			return false, err
		}

		return shouldRelease, nil
	}

	var shouldRelease bool
	if !zoneOwnsSubnet {
		shouldRelease, err = shouldReleasePodIPs()
	} else {
		shouldRelease, err = bnc.lsManager.ConditionalIPRelease(switchName, podIfAddrs, shouldReleasePodIPs)
	}

	if err != nil {
		return false, fmt.Errorf("cannot determine if IPs are safe to release for completed pod %s/%s on NAD key %s: %w",
			pod.Namespace,
			pod.Name,
			nadKey,
			err)
	}

	return shouldRelease, nil
}

// trackPodsReleasedBeforeStartup tracks from all of the annotated pods on startup,
// which ones might have had their IPs previously released. This information is
// used to prevent those IPs to be released while they might be still used by
// other running pods and is tracked per NAD, as it is possible for a pod to
// have been released for one of its NADs but not all in an unexpected error
// condition. It uses the following rules:
//   - One or more completed pods sharing an IP with a running pod are
//     considered released.
//   - One or more completed pods sharing an IP are considered released except
//     the last one to be initialized.
func (bnc *BaseNetworkController) trackPodsReleasedBeforeStartup(podAnnotations map[*corev1.Pod]map[string]*util.PodAnnotation) {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()

	// we will order the pods by order of initialization, by that time pods
	// should have been already allocated exclusive IPs
	getInitializedConditionTime := func(pod *corev1.Pod) time.Time {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodInitialized {
				return condition.LastTransitionTime.Time
			}
		}
		return time.Time{}
	}

	// order the pods, running pods first, then completed pods ordered by
	// initialization time
	pods := maps.Keys(podAnnotations)
	sort.Slice(pods, func(i, j int) bool {
		if !util.PodCompleted(pods[i]) {
			return true
		}
		return getInitializedConditionTime(pods[i]).After(getInitializedConditionTime(pods[j]))
	})

	// consider dual-stack IPs individually but only track at the pod level based
	// on a couple of assumptions:
	// - while the same pair of IPs released for a pod will most likely be
	//   assigned to a different pod, assume that one of those IPs might be
	//   assigned to a pod and the other IP to a different pod. This is easy to
	//   handle so better take a safe approach
	// - assume that there is no error path leading to one of the IPs of the
	//   pair to be released while the other is not. This is based on the fact
	//   that both IPs are released in block.
	visitedIPs := sets.Set[string]{}
	bnc.releasedPodsBeforeStartup = map[string]sets.Set[string]{}

	for _, pod := range pods {
		uid := string(pod.UID)
		for nadKey, annotation := range podAnnotations[pod] {
			ips := []string{}
			for _, ipnet := range annotation.IPs {
				// normalize ips to their 16 octect string representation
				ips = append(ips, string(ipnet.IP.To16()))
			}
			// if we haven't seen these IPs before, consider this pod the rightful
			// owner with which these IPs will be released
			if !visitedIPs.HasAny(ips...) {
				visitedIPs.Insert(ips...)
				continue
			}
			if !util.PodCompleted(pod) {
				// this should not happen, but let's log it just in case
				klog.Errorf("Non completed pod %s/%s shares ips %s from NAD key %s with some other non completed pod",
					pod.Namespace,
					pod.Name,
					util.StringSlice(annotation.IPs),
					nadKey,
				)
				continue
			}
			// otherwise consider the IPs of this NAD already released for the pod
			if bnc.releasedPodsBeforeStartup[nadKey] == nil {
				bnc.releasedPodsBeforeStartup[nadKey] = sets.New(uid)
			} else {
				bnc.releasedPodsBeforeStartup[nadKey].Insert(uid)
			}
		}
	}
}

// forgetPodReleasedBeforeStartup stops tracking a released pod on the specified NAD
func (bnc *BaseNetworkController) forgetPodReleasedBeforeStartup(uid, nadKey string) {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()
	if bnc.releasedPodsBeforeStartup[nadKey] == nil {
		return
	}
	bnc.releasedPodsBeforeStartup[nadKey].Delete(uid)
	if bnc.releasedPodsBeforeStartup[nadKey].Len() == 0 {
		delete(bnc.releasedPodsBeforeStartup, nadKey)
	}
}

// wasPodReleasedBeforeStartup returns whether a pod has been considered released on
// startup for the specific NAD
func (bnc *BaseNetworkController) wasPodReleasedBeforeStartup(uid, nadKey string) bool {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()
	if bnc.releasedPodsBeforeStartup[nadKey] == nil {
		return false
	}
	return bnc.releasedPodsBeforeStartup[nadKey].Has(uid)
}

func (bnc *BaseNetworkController) isNonHostSubnetSwitch(switchName string) bool {
	return bnc.doesNetworkRequireIPAM() && bnc.lsManager.IsNonHostSubnetSwitch(switchName)
}
