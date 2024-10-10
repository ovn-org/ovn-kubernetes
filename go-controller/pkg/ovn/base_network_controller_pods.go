package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	ipallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	subnetipallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	logicalswitchmanager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func (bnc *BaseNetworkController) allocatePodIPs(pod *kapi.Pod,
	annotations *util.PodAnnotation, nadName string) (expectedLogicalPortName string, err error) {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return "", err
	}
	return bnc.allocatePodIPsOnSwitch(pod, annotations, nadName, switchName)
}

var nodeNotFoundError = errors.New("node not found")

// allocatePodIPsForSwitch will allocate the the ip from pod annotation at
// a specified switch, this switch can be different than the one the pod is
// attachted to, for example hypershift kubevirt provider live migration.
func (bnc *BaseNetworkController) allocatePodIPsOnSwitch(pod *kapi.Pod,
	annotations *util.PodAnnotation, nadName string, switchName string) (expectedLogicalPortName string, err error) {

	// Completed pods will be allocated as well to avoid having their IPs
	// allocated to other pods before we make sure that we have released them
	// first.
	// See trackReleasedPodsBeforeStartup for additional tracking that
	// happens to avoid relasing IPs for completed pods that were already
	// released before startup.
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
		return "", nil
	}
	// skip nodes that are not running ovnk (inferred from host subnets)
	if bnc.lsManager.IsNonHostSubnetSwitch(switchName) {
		return "", nil
	}

	expectedLogicalPortName = bnc.GetLogicalPortName(pod, nadName)
	// it is possible to try to add a pod here that has no node. For example if a pod was deleted with
	// a finalizer, and then the node was removed. In this case the pod will still exist in a running state.
	// Terminating pods should still have network connectivity for pre-stop hooks or termination grace period
	if _, err := bnc.watchFactory.GetNode(pod.Spec.NodeName); kerrors.IsNotFound(err) &&
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
	var switchNames []string

	// get all switches that Pod logical port would be reside on.
	topoType := bnc.TopologyType()
	if !bnc.IsSecondary() || topoType == ovntypes.Layer3Topology {
		// for default network and layer3 topology type networks, get all local zone node switches
		nodes, err := bnc.GetLocalZoneNodes()
		if err != nil {
			return fmt.Errorf("failed to get nodes: %v", err)
		}

		switchNames = make([]string, 0, len(nodes))
		for _, n := range nodes {
			// skip nodes that are not running ovnk (inferred from host subnets)
			switchName := bnc.GetNetworkScopedName(n.Name)
			if bnc.lsManager.IsNonHostSubnetSwitch(switchName) {
				continue
			}
			switchNames = append(switchNames, switchName)
		}
	} else if topoType == ovntypes.Layer2Topology {
		switchNames = []string{bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)}
	} else if topoType == ovntypes.LocalnetTopology {
		switchNames = []string{bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)}
	} else {
		return fmt.Errorf("topology type %s not supported", topoType)
	}

	return bnc.deleteStaleLogicalSwitchPortsOnSwitches(switchNames, expectedLogicalPorts)
}

func (bnc *BaseNetworkController) deleteStaleLogicalSwitchPortsOnSwitches(switchNames []string,
	expectedLogicalPorts map[string]bool) error {
	var ops []ovsdb.Operation
	var err error
	for _, switchName := range switchNames {
		p := func(item *nbdb.LogicalSwitchPort) bool {
			return item.ExternalIDs["pod"] == "true" && !expectedLogicalPorts[item.Name]
		}
		sw := nbdb.LogicalSwitch{
			Name: switchName,
		}

		ops, err = libovsdbops.DeleteLogicalSwitchPortsWithPredicateOps(bnc.nbClient, ops, &sw, p)
		if err != nil {
			return fmt.Errorf("could not generate ops to delete stale ports from logical switch %s (%+v)", switchName, err)
		}
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("could not remove stale logicalPorts from switches for network %s (%+v)", bnc.GetNetworkName(), err)
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

func (bnc *BaseNetworkController) deletePodLogicalPort(pod *kapi.Pod, portInfo *lpInfo,
	nadName string) (*lpInfo, error) {
	var portUUID, switchName, logicalPort string
	var podIfAddrs []*net.IPNet

	expectedSwitchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, err
	}

	podDesc := fmt.Sprintf("pod %s/%s/%s", nadName, pod.Namespace, pod.Name)
	logicalPort = bnc.GetLogicalPortName(pod, nadName)
	if portInfo == nil {
		// If ovnkube-master restarts, it is also possible the Pod's logical switch port
		// is not re-added into the cache. Delete logical switch port anyway.
		annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
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

	shouldRelease, err := bnc.shouldReleaseDeletedPod(pod, switchName, nadName, podIfAddrs)
	if err != nil {
		return nil, fmt.Errorf("unable to determine if ip should be released: %v", err)
	}

	var allOps, ops []ovsdb.Operation

	// if the ip is in use by another pod we should not try to remove it from the address set
	if shouldRelease {
		if ops, err = bnc.deletePodFromNamespace(pod.Namespace,
			podIfAddrs, portUUID); err != nil {
			return nil, fmt.Errorf("unable to delete pod %s from namespace: %w", podDesc, err)
		}
		allOps = append(allOps, ops...)
	}
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
func (bnc *BaseNetworkController) findPodWithIPAddresses(needleIPs []net.IP, nodeName string) (*kapi.Pod, error) {
	allPods, err := bnc.watchFactory.GetAllPods()
	if err != nil {
		return nil, fmt.Errorf("unable to get pods: %w", err)
	}

	// iterate through all pods
	for _, p := range allPods {
		if util.PodCompleted(p) || util.PodWantsHostNetwork(p) || !util.PodScheduled(p) {
			continue
		}

		// If the network type is Layer 3, then IP allocation is per node, so we can filter pods by node name.
		// If pod is not applicable to live migration, restrict the search to only pods on the filtered node
		// This specifically speeds up a case where a pod may have been annotated by ovnkube-controller, but has not yet
		// returned from CNI ADD. In that case the GetPodIPsOfNetwork would unmarshal the annotation and take a perf
		// hit for no reason (since the IP cannot be in the same subnet as what we are looking for).
		if bnc.TopologyType() == ovntypes.Layer3Topology && !kubevirt.IsPodLiveMigratable(p) && len(nodeName) > 0 && nodeName != p.Spec.NodeName {
			continue
		}

		// check if the pod addresses match in the OVN annotation
		haystackPodAddrs, err := util.GetPodIPsOfNetwork(p, bnc.NetInfo)
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
func (bnc *BaseNetworkController) canReleasePodIPs(podIfAddrs []*net.IPNet, nodeName string) (bool, error) {
	var needleIPs []net.IP
	for _, podIPNet := range podIfAddrs {
		needleIPs = append(needleIPs, podIPNet.IP)
	}

	collidingPod, err := bnc.findPodWithIPAddresses(needleIPs, nodeName)
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

func (bnc *BaseNetworkController) waitForNodeLogicalSwitch(switchName string) (*nbdb.LogicalSwitch, error) {
	// Wait for the node logical switch to be created by the ClusterController and be present
	// in libovsdb's cache. The node switch will be created when the node's logical network infrastructure
	// is created by the node watch
	ls := &nbdb.LogicalSwitch{Name: switchName}
	if err := wait.PollUntilContextTimeout(context.Background(), 30*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if subnets := bnc.lsManager.GetSwitchSubnets(switchName); subnets == nil {
			return false, fmt.Errorf("error getting logical switch %s: %s", switchName, "switch not in logical switch cache")
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
	if err := wait.PollUntilContextTimeout(context.Background(), 30*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		subnets = bnc.lsManager.GetSwitchSubnets(switchName)
		return subnets != nil, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for logical switch %q subnet: %w", switchName, nodeNotFoundError)
	}
	return nil
}

// podExpectedInLogicalCache returns true if pod should be added to oc.logicalPortCache.
// For some pods, like hostNetwork pods, overlay node pods, or completed pods waiting for them to be added
// to oc.logicalPortCache will never succeed.
func (bnc *BaseNetworkController) podExpectedInLogicalCache(pod *kapi.Pod) bool {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return false
	}
	return !util.PodWantsHostNetwork(pod) &&
		!(bnc.lsManager.IsNonHostSubnetSwitch(switchName) &&
			bnc.doesNetworkRequireIPAM()) &&
		!util.PodCompleted(pod)
}

func (bnc *BaseNetworkController) getExpectedSwitchName(pod *kapi.Pod) (string, error) {
	switchName := pod.Spec.NodeName
	if bnc.IsSecondary() {
		topoType := bnc.TopologyType()
		switch topoType {
		case ovntypes.Layer3Topology:
			switchName = bnc.GetNetworkScopedName(pod.Spec.NodeName)
		case ovntypes.Layer2Topology:
			switchName = bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
		case ovntypes.LocalnetTopology:
			switchName = bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)
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
func (bnc *BaseNetworkController) ensurePodAnnotation(pod *kapi.Pod, nadName string) (*util.PodAnnotation, bool, error) {
	if kubevirt.IsPodLiveMigratable(pod) {
		podAnnotation, err := kubevirt.EnsurePodAnnotationForVM(bnc.watchFactory, bnc.kube, pod, bnc.NetInfo, nadName)
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
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
	if err != nil {
		return nil, true, nil
	}
	return podAnnotation, true, nil
}

func (bnc *BaseNetworkController) addLogicalPortToNetwork(pod *kapi.Pod, nadName string,
	network *nadapi.NetworkSelectionElement) (ops []ovsdb.Operation,
	lsp *nbdb.LogicalSwitchPort, podAnnotation *util.PodAnnotation, newlyCreatedPort bool, err error) {
	var ls *nbdb.LogicalSwitch

	podDesc := fmt.Sprintf("%s/%s/%s", nadName, pod.Namespace, pod.Name)
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("[%s] failed geting expected switch name when adding logical switch port: %v", podDesc, err)
	}

	// it is possible to try to add a pod here that has no node. For example if a pod was deleted with
	// a finalizer, and then the node was removed. In this case the pod will still exist in a running state.
	// Terminating pods should still have network connectivity for pre-stop hooks or termination grace period
	// We cannot wire a pod that has no node/switch, so retry again later
	if _, err := bnc.watchFactory.GetNode(pod.Spec.NodeName); kerrors.IsNotFound(err) &&
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

	portName := bnc.GetLogicalPortName(pod, nadName)
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
	// Bind the port to the node's chassis; prevents ping-ponging between
	// chassis if ovnkube-node isn't running correctly and hasn't cleared
	// out iface-id for an old instance of this pod, and the pod got
	// rescheduled.

	if !config.Kubernetes.DisableRequestedChassis {
		lsp.Options["requested-chassis"] = pod.Spec.NodeName
	}

	// Although we have different code to allocate the pod annotation for the
	// default network and secondary networks, at the time of this writing they
	// are functionally equivalent and the only reason to keep them separated is
	// to make sure the secondary network code has no bugs before we switch to
	// it for the default network as well. If at all possible, keep them
	// functionally equivalent going forward.
	var annotationUpdated bool
	if bnc.IsSecondary() {
		podAnnotation, annotationUpdated, err = bnc.allocatePodAnnotationForSecondaryNetwork(pod, existingLSP, nadName, network)
	} else {
		podAnnotation, annotationUpdated, err = bnc.allocatePodAnnotation(pod, existingLSP, podDesc, nadName, network)
	}

	if err != nil {
		return nil, nil, nil, false, err
	}

	// set addresses on the port
	// LSP addresses in OVN are a single space-separated value
	addresses = []string{podAnnotation.MAC.String()}
	for _, podIfAddr := range podAnnotation.IPs {
		addresses[0] = addresses[0] + " " + podIfAddr.IP.String()
	}

	lsp.Addresses = addresses

	// add external ids
	lsp.ExternalIDs = map[string]string{"namespace": pod.Namespace, "pod": "true"}
	if bnc.IsSecondary() {
		lsp.ExternalIDs[ovntypes.NetworkExternalID] = bnc.GetNetworkName()
		lsp.ExternalIDs[ovntypes.NADExternalID] = nadName
		lsp.ExternalIDs[ovntypes.TopologyExternalID] = bnc.TopologyType()
	}

	// CNI depends on the flows from port security, delay setting it until end
	lsp.PortSecurity = addresses

	// On layer2 topology with interconnect, we need to add specific port config
	if bnc.isLayer2Interconnect() {
		isRemotePort := !bnc.isPodScheduledinLocalZone(pod)
		err = bnc.zoneICHandler.AddTransitPortConfig(isRemotePort, podAnnotation, lsp)
		if err != nil {
			return nil, nil, nil, false, err
		}
	}

	ops, err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitchOps(bnc.nbClient, nil, ls, lsp)
	if err != nil {
		return nil, nil, nil, false,
			fmt.Errorf("error creating logical switch port %+v on switch %+v: %+v", *lsp, *ls, err)
	}

	return ops, lsp, podAnnotation, annotationUpdated && !lspExist, nil
}

func (bnc *BaseNetworkController) updatePodAnnotationWithRetry(origPod *kapi.Pod, podInfo *util.PodAnnotation, nadName string) error {
	return util.UpdatePodAnnotationWithRetry(
		bnc.watchFactory.PodCoreInformer().Lister(),
		bnc.kube,
		origPod,
		podInfo,
		nadName,
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

func (bnc *BaseNetworkController) deletePodFromNamespace(ns string, podIfAddrs []*net.IPNet, portUUID string) ([]ovsdb.Operation, error) {
	// for secondary network, namespace may be not managed
	nsInfo, nsUnlock := bnc.getNamespaceLocked(ns, true)
	if nsInfo == nil {
		return nil, nil
	}
	defer nsUnlock()
	var ops []ovsdb.Operation
	var err error
	if nsInfo.addressSet != nil {
		if ops, err = nsInfo.addressSet.DeleteAddressesReturnOps(util.IPNetsIPToStringSlice(podIfAddrs)); err != nil {
			return nil, err
		}
	}

	if nsInfo.portGroupName != "" && len(portUUID) > 0 {
		if ops, err = libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, ops, nsInfo.portGroupName, portUUID); err != nil {
			return nil, err
		}
	}

	return ops, nil
}

// isPodScheduledinLocalZone returns true if
//   - bnc.localZoneNodes map is nil or
//   - if the pod.Spec.NodeName is in the bnc.localZoneNodes map
//
// false otherwise.
func (bnc *BaseNetworkController) isPodScheduledinLocalZone(pod *kapi.Pod) bool {
	isLocalZonePod := true

	if bnc.localZoneNodes != nil {
		if util.PodScheduled(pod) {
			_, isLocalZonePod = bnc.localZoneNodes.Load(pod.Spec.NodeName)
		} else {
			isLocalZonePod = false
		}
	}

	return isLocalZonePod
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
func (bnc *BaseNetworkController) allocatePodAnnotation(pod *kapi.Pod, existingLSP *nbdb.LogicalSwitchPort, podDesc, nadName string, network *nadapi.NetworkSelectionElement) (*util.PodAnnotation, bool, error) {
	var releaseIPs bool
	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet

	switchName := pod.Spec.NodeName

	podAnnotation, zoneContainsPodSubnet, err := bnc.ensurePodAnnotation(pod, nadName)
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
				// ensure we have reserved the IPs in the annotation
				if err = bnc.lsManager.AllocateIPs(switchName, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
					return nil, false, fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %v",
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
		if err = bnc.lsManager.AllocateIPs(switchName, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			klog.Warningf("Unable to allocate IPs %s found on existing OVN port: %s, for pod %s on switch: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), bnc.GetLogicalPortName(pod, nadName), podDesc, switchName, err)

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
		IPs: podIfAddrs,
		MAC: podMac,
	}
	var nodeSubnets []*net.IPNet
	if nodeSubnets = bnc.lsManager.GetSwitchSubnets(switchName); nodeSubnets == nil && bnc.doesNetworkRequireIPAM() {
		return nil, false, fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, switch: %s",
			podDesc, switchName)
	}
	err = util.AddRoutesGatewayIP(bnc.NetInfo, pod, podAnnotation, network)
	if err != nil {
		return nil, false, err
	}

	klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s",
		podIfAddrs, podMac, podAnnotation.Gateways)
	annoStart := time.Now()
	err = bnc.updatePodAnnotationWithRetry(pod, podAnnotation, nadName)
	podAnnoTime := time.Since(annoStart)
	klog.Infof("[%s] addLogicalPort annotation time took %v", podDesc, podAnnoTime)
	if err != nil {
		return nil, false, err
	}
	releaseIPs = false

	return podAnnotation, true, nil
}

// allocatePodAnnotationForSecondaryNetwork and update the corresponding pod
// annotation.
func (bnc *BaseNetworkController) allocatePodAnnotationForSecondaryNetwork(pod *kapi.Pod, lsp *nbdb.LogicalSwitchPort, nadName string, network *nadapi.NetworkSelectionElement) (*util.PodAnnotation, bool, error) {
	switchName, err := bnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, false, err
	}

	// In certain configurations, pod IP allocation is handled from cluster
	// manager so wait for it to allocate the IPs
	if !bnc.allocatesPodAnnotation() {
		podAnnotation, _ := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
		if !util.IsValidPodAnnotation(podAnnotation) {
			return nil, false, fmt.Errorf("failed to get PodAnnotation for %s/%s/%s, cluster manager might have not allocated it yet",
				nadName, pod.Namespace, pod.Name)
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
				nadName, pod.Namespace, pod.Name, switchName, err)
		}
		network.MacRequest = mac.String()
		network.IPRequest = util.StringSlice(ips)
		reallocate = true

		klog.V(5).Infof("Will attempt to use LSP IP addresses %v and mac %s for pod %s/%s/%s",
			network.IPRequest, network.MacRequest, nadName, pod.Namespace, pod.Name)
	}

	var ipAllocator subnetipallocator.NamedAllocator
	if bnc.doesNetworkRequireIPAM() {
		ipAllocator = bnc.lsManager.ForSwitch(switchName)
	}

	updatedPod, podAnnotation, err := bnc.podAnnotationAllocator.AllocatePodAnnotation(
		ipAllocator,
		pod,
		network,
		reallocate,
	)

	if err != nil {
		return nil, false, err
	}

	if updatedPod != nil {
		klog.V(5).Infof("Allocated IP addresses %v, mac address %s, gateways %v and routes %s for pod %s/%s on nad %s",
			util.StringSlice(podAnnotation.IPs),
			podAnnotation.MAC,
			util.StringSlice(podAnnotation.Gateways),
			util.StringSlice(podAnnotation.Routes),
			pod.Namespace, pod.Name, nadName,
		)

		return podAnnotation, true, nil
	}

	return podAnnotation, false, nil
}

func (bnc *BaseNetworkController) allocatesPodAnnotation() bool {
	switch bnc.NetInfo.TopologyType() {
	case ovntypes.Layer2Topology:
		// on layer2 topologies, cluster manager allocates tunnel IDs, allocates
		// IPs if the network has IPAM, and sets the PodAnnotation
		return !config.OVNKubernetesFeature.EnableInterconnect
	case ovntypes.LocalnetTopology:
		// on localnet topologies with IPAM, cluster manager allocates IPs and
		// sets the PodAnnotation
		return !config.OVNKubernetesFeature.EnableInterconnect || !bnc.doesNetworkRequireIPAM()
	}
	return true
}

func (bnc *BaseNetworkController) shouldReleaseDeletedPod(pod *kapi.Pod, switchName, nad string, podIfAddrs []*net.IPNet) (bool, error) {
	var err error
	var isMigratedSourcePodStale bool
	if !bnc.IsSecondary() {
		isMigratedSourcePodStale, err = kubevirt.IsMigratedSourcePodStale(bnc.watchFactory, pod)
		if err != nil {
			return false, err
		}
	}

	// Removing the the kubevirt stale pods should not de allocate the IPs
	// to ensure that new pods do not take them
	if isMigratedSourcePodStale {
		return false, nil
	}

	if !util.PodCompleted(pod) {
		return true, nil
	}

	if bnc.wasPodReleasedBeforeStartup(string(pod.UID), nad) {
		klog.Infof("Completed pod %s/%s was already released for nad %s before startup",
			pod.Namespace,
			pod.Name,
			nad,
		)
		return false, nil
	}

	shouldReleasePodIPs := func() (bool, error) {
		// If this pod applies to live migration it could have migrated so get the
		// correct node name corresponding with the subnet. If the subnet is not
		// tracked within the zone, nodeName will be empty which will force
		// canReleasePodIPs to lookup all nodes.
		nodeName := pod.Spec.NodeName
		if !bnc.IsSecondary() && kubevirt.IsPodLiveMigratable(pod) {
			nodeName, _ = bnc.lsManager.GetSubnetName(podIfAddrs)
		}

		shouldRelease, err := bnc.canReleasePodIPs(podIfAddrs, nodeName)
		if err != nil {
			return false, err
		}

		return shouldRelease, nil
	}

	var shouldRelease bool
	// for secondary network IPs allocated from cluster manager, we will check
	// if other pods are using the same IPs just in case we are processing
	// events in different order than cluster manager did (best effort, there
	// can still be issues with this)
	if !bnc.allocatesPodAnnotation() {
		shouldRelease, err = shouldReleasePodIPs()
	} else {
		shouldRelease, err = bnc.lsManager.ConditionalIPRelease(switchName, podIfAddrs, shouldReleasePodIPs)
	}

	if err != nil {
		return false, fmt.Errorf("cannot determine if IPs are safe to release for completed pod %s/%s on nad %s: %w",
			pod.Namespace,
			pod.Name,
			nad,
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
func (bnc *BaseNetworkController) trackPodsReleasedBeforeStartup(podAnnotations map[*kapi.Pod]map[string]*util.PodAnnotation) {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()

	// we will order the pods by order of initialization, by that time pods
	// should have been already allocated exclusive IPs
	getInitializedConditionTime := func(pod *kapi.Pod) time.Time {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == kapi.PodInitialized {
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
		for nad, annotation := range podAnnotations[pod] {
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
				klog.Errorf("Non completed pod %s/%s shares ips %s from NAD %s with some other non completed pod",
					pod.Namespace,
					pod.Name,
					util.StringSlice(annotation.IPs),
					nad,
				)
				continue
			}
			// otherwise consider the IPs of this NAD already released for the pod
			if bnc.releasedPodsBeforeStartup[nad] == nil {
				bnc.releasedPodsBeforeStartup[nad] = sets.New(uid)
			} else {
				bnc.releasedPodsBeforeStartup[nad].Insert(uid)
			}
		}
	}
}

// forgetPodReleasedBeforeStartup stops tracking a released pod on the specified NAD
func (bnc *BaseNetworkController) forgetPodReleasedBeforeStartup(uid, nad string) {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()
	if bnc.releasedPodsBeforeStartup[nad] == nil {
		return
	}
	bnc.releasedPodsBeforeStartup[nad].Delete(uid)
	if bnc.releasedPodsBeforeStartup[nad].Len() == 0 {
		delete(bnc.releasedPodsBeforeStartup, nad)
	}
}

// wasPodReleasedBeforeStartup returns whether a pod has been considered released on
// startup for the specific NAD
func (bnc *BaseNetworkController) wasPodReleasedBeforeStartup(uid, nad string) bool {
	bnc.releasedPodsOnStartupMutex.Lock()
	defer bnc.releasedPodsOnStartupMutex.Unlock()
	if bnc.releasedPodsBeforeStartup[nad] == nil {
		return false
	}
	return bnc.releasedPodsBeforeStartup[nad].Has(uid)
}
