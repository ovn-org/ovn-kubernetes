// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

func (bsnc *BaseUserDefinedNetworkController) getPortInfoForUserDefinedNetwork(pod *corev1.Pod) map[string]*lpInfo {
	if util.PodWantsHostNetwork(pod) {
		return nil
	}
	portInfoMap, _ := bsnc.logicalPortCache.getAll(pod)
	return portInfoMap
}

// GetInternalCacheEntryForUserDefinedNetwork returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (bsnc *BaseUserDefinedNetworkController) GetInternalCacheEntryForUserDefinedNetwork(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*corev1.Pod)
		return bsnc.getPortInfoForUserDefinedNetwork(pod)
	default:
		return nil
	}
}

// AddUserDefinedNetworkResourceCommon adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. This function is called for User Defined Networks only.
func (bsnc *BaseUserDefinedNetworkController) AddUserDefinedNetworkResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return bsnc.ensurePodForUserDefinedNetwork(pod, true)

	case factory.NamespaceType:
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return bsnc.AddNamespaceForUserDefinedNetwork(ns)

	case factory.MultiNetworkPolicyType:
		mp, ok := obj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", obj)
		}

		if !bsnc.shouldApplyMultiPolicy(mp) {
			return nil
		}

		np, err := bsnc.convertMultiNetPolicyToNetPolicy(mp)
		if err != nil {
			return err
		}
		if err := bsnc.addNetworkPolicy(np); err != nil {
			klog.Infof("MultiNetworkPolicy add failed for %s/%s, will try again later: %v",
				mp.Namespace, mp.Name, err)
			return err
		}

	default:
		return bsnc.AddResourceCommon(objType, obj)
	}
	return nil
}

// UpdateUserDefinedNetworkResourceCommon updates the specified object in the cluster to its version in newObj
// according to its type and returns the error, if any, yielded during the object update. This function is
// called for User Defined Networks only.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (bsnc *BaseUserDefinedNetworkController) UpdateUserDefinedNetworkResourceCommon(objType reflect.Type, oldObj, newObj interface{}, inRetryCache bool) error {
	switch objType {
	case factory.PodType:
		oldPod := oldObj.(*corev1.Pod)
		newPod := newObj.(*corev1.Pod)

		addPort := shouldAddPort(oldPod, newPod, inRetryCache) ||
			bsnc.dhcpPodNetworkUpdated(oldPod, newPod)
		return bsnc.ensurePodForUserDefinedNetwork(newPod, addPort)

	case factory.NamespaceType:
		oldNs, newNs := oldObj.(*corev1.Namespace), newObj.(*corev1.Namespace)
		return bsnc.updateNamespaceForUserDefinedNetwork(oldNs, newNs)

	case factory.MultiNetworkPolicyType:
		oldMp, ok := oldObj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", oldObj)
		}
		newMp, ok := newObj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", newObj)
		}

		oldShouldApply := bsnc.shouldApplyMultiPolicy(oldMp)
		newShouldApply := bsnc.shouldApplyMultiPolicy(newMp)
		if oldShouldApply {
			// this multi-netpol no longer applies to this network controller, delete it
			np, err := bsnc.convertMultiNetPolicyToNetPolicy(oldMp)
			if err != nil {
				return err
			}
			if err := bsnc.deleteNetworkPolicy(np); err != nil {
				klog.Infof("MultiNetworkPolicy delete failed for %s/%s, will try again later: %v",
					oldMp.Namespace, oldMp.Name, err)
				return err
			}
		}
		if newShouldApply {
			// now this multi-netpol applies to this network controller
			np, err := bsnc.convertMultiNetPolicyToNetPolicy(newMp)
			if err != nil {
				return err
			}
			if err := bsnc.addNetworkPolicy(np); err != nil {
				klog.Infof("MultiNetworkPolicy add failed for %s/%s, will try again later: %v",
					newMp.Namespace, newMp.Name, err)
				return err
			}
		}

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
	return nil
}

// DeleteUserDefinedNetworkResourceCommon deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods.
// This function is called for User Defined Networks only.
func (bsnc *BaseUserDefinedNetworkController) DeleteUserDefinedNetworkResourceCommon(objType reflect.Type, obj, cachedObj interface{}) error {
	switch objType {
	case factory.PodType:
		var portInfoMap map[string]*lpInfo
		pod := obj.(*corev1.Pod)

		if cachedObj != nil {
			portInfoMap = cachedObj.(map[string]*lpInfo)
		}
		return bsnc.removePodForUserDefinedNetwork(pod, portInfoMap)

	case factory.NamespaceType:
		ns := obj.(*corev1.Namespace)
		return bsnc.deleteNamespaceForUserDefinedNetwork(ns)

	case factory.MultiNetworkPolicyType:
		mp, ok := obj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", obj)
		}
		np, err := bsnc.convertMultiNetPolicyToNetPolicy(mp)
		if err != nil {
			return err
		}
		// delete this policy regardless it applies to this network controller, in case of missing update event
		if err := bsnc.deleteNetworkPolicy(np); err != nil {
			klog.Infof("MultiNetworkPolicy delete failed for %s/%s, will try again later: %v",
				mp.Namespace, mp.Name, err)
			return err
		}

	default:
		return bsnc.DeleteResourceCommon(objType, obj)
	}
	return nil
}

// ensurePodForUserDefinedNetwork tries to set up the User Defined Network for a pod. It returns nil on success and error
// on failure; failure indicates the pod set up should be retried later.
func (bsnc *BaseUserDefinedNetworkController) ensurePodForUserDefinedNetwork(pod *corev1.Pod, addPort bool) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if util.PodWantsHostNetwork(pod) {
		return nil
	}

	var kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus
	var err error

	if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
		kubevirtLiveMigrationStatus, err = kubevirt.DiscoverLiveMigrationStatus(bsnc.watchFactory.PodCoreInformer().Lister(), pod)
		if err != nil {
			return fmt.Errorf("failed to discover Live-migration status: %w", err)
		}
	}
	updatePort := kubevirtLiveMigrationStatus != nil && pod.Name == kubevirtLiveMigrationStatus.TargetPod.Name

	if !addPort && !updatePort {
		return nil
	}

	// If a node does not have an assigned hostsubnet don't wait for the logical switch to appear
	var switchName string
	switchName, err = bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return err
	}

	var activeNetwork util.NetInfo
	if bsnc.IsPrimaryNetwork() {
		// check to see if the primary NAD is even applicable to our controller
		foundNamespaceNAD, err := bsnc.networkManager.GetPrimaryNADForNamespace(pod.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get primary network namespace NAD: %w", err)
		}
		if foundNamespaceNAD == types.DefaultNetworkName {
			return nil
		}
		networkName := bsnc.networkManager.GetNetworkNameForNADKey(foundNamespaceNAD)
		if networkName != "" && networkName != bsnc.GetNetworkName() {
			return nil
		}
		activeNetwork, err = bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
		if err != nil {
			return fmt.Errorf("failed to find active network for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		if activeNetwork == nil {
			// no active network, pod doesn't belong to our controller
			return nil
		}
	}

	on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
		pod,
		bsnc.GetNetInfo(),
		activeNetwork,
		bsnc.networkManager.GetNetworkNameForNADKey,
		bsnc.networkManager.GetPrimaryNADForNamespace,
	)
	if err != nil {
		bsnc.recordPodErrorEvent(pod, err)
		// configuration error, no need to retry, do not return error
		klog.Errorf("Error getting network-attachment for pod %s/%s network %s: %v",
			pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		return nil
	}

	if !on {
		// the pod is not attached to this specific network
		klog.V(5).Infof("Pod %s/%s is not attached on this network controller %s",
			pod.Namespace, pod.Name, bsnc.GetNetworkName())
		return nil
	}

	if bsnc.isNonHostSubnetSwitch(switchName) {
		klog.V(5).Infof(
			"Pod %s/%s requires IPAM but does not have an assigned IP address", pod.Namespace, pod.Name)
		return nil
	}

	var errs []error
	for nadKey, network := range networkMap {
		if err = bsnc.addLogicalPortToNetworkForNAD(pod, nadKey, switchName, network, kubevirtLiveMigrationStatus); err != nil {
			errs = append(errs, fmt.Errorf("failed to add logical port of Pod %s/%s for NAD key %s: %w", pod.Namespace, pod.Name, nadKey, err))
		}
	}
	if len(errs) != 0 {
		return utilerrors.Join(errs...)
	}
	return nil
}

func (bsnc *BaseUserDefinedNetworkController) addLogicalPortToNetworkForNAD(pod *corev1.Pod, nadKey, switchName string,
	network *nadapi.NetworkSelectionElement, kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus,
) error {
	var libovsdbExecuteTime time.Duration

	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort for NAD key %s took %v, libovsdb time %v",
			pod.Namespace, pod.Name, nadKey, time.Since(start), libovsdbExecuteTime)
	}()

	var err error
	var podAnnotation *util.PodAnnotation
	var ops []ovsdb.Operation
	var lsp *nbdb.LogicalSwitchPort
	var newlyCreated bool

	var lspEnabled *bool
	// actions on the pods' LSP are only triggerred from the target pod
	shouldHandleLiveMigration := kubevirtLiveMigrationStatus != nil && pod.Name == kubevirtLiveMigrationStatus.TargetPod.Name
	if shouldHandleLiveMigration {
		// LSP should be altered inside addLogicalPortToNetwork() before ops are generated because one cannot append
		// multiple ops regarding the same object in the same transact, so passing enabled parameter.
		lspEnabled = ptr.To(kubevirtLiveMigrationStatus.IsTargetDomainReady())
	}

	// we need to create a logical port for all local pods
	// we also need to create a remote logical port for remote pods on layer2
	// topologies with interconnect
	isLocalPod := bsnc.isPodScheduledOnLocalNode(pod)
	requiresLogicalPort := isLocalPod || bsnc.isLayer2WithInterconnectTransport()

	if requiresLogicalPort {
		ops, lsp, podAnnotation, newlyCreated, err = bsnc.addLogicalPortToNetwork(pod, nadKey, network, lspEnabled)
		if err != nil {
			return err
		}
	} else if bsnc.TopologyType() == types.LocalnetTopology {
		// On localnet networks, we might be processing the pod as a result of a
		// node changing zone local -> remote so cleanup the logical port in
		// case it exists and is no longer needed.
		// This should be an idempotent operation.
		// Not needed for layer3 networks as in that case the whole node switch
		// is removed
		// No need to release IPs as those are allocated from cluster manager
		logicalPort := bsnc.GetLogicalPortName(pod, nadKey)
		expectedSwitchName, err := bsnc.getExpectedSwitchName(pod)
		if err != nil {
			return err
		}
		ops, err = bsnc.delLSPOps(logicalPort, expectedSwitchName, "")
		if err != nil {
			return err
		}
		bsnc.logicalPortCache.remove(pod, nadKey)
	}

	if shouldHandleLiveMigration && kubevirtLiveMigrationStatus.IsTargetDomainReady() {
		// We have a source pod LSP at this zone if the source pod and:
		// - layer2 IC There is one at all the zones since we have the remote LSP to implement east/west
		// - localnet only if this is the zone where the source pod is running
		ops, err = bsnc.disableLiveMigrationSourceLSPOps(kubevirtLiveMigrationStatus, nadKey, ops)
		if err != nil {
			return fmt.Errorf("failed to create LSP ops for source pod during Live-migration status: %w", err)
		}
	}

	if podAnnotation == nil {
		podAnnotation, err = util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
		if err != nil {
			return err
		}
	}

	// Add membership to the namespace-owned port group, used by multicast and
	// egress firewall, on networks with known (including DHCP-learned) pod IPs.
	if bsnc.doesNetworkHaveDiscoverablePodIPs() &&
		(util.IsMultiNetworkPoliciesSupportEnabled() || (util.IsNetworkSegmentationSupportEnabled() && bsnc.IsPrimaryNetwork())) {
		portUUID := ""
		if lsp != nil {
			portUUID = lsp.UUID
		}
		ops, err = bsnc.addPodToNamespacePortGroupOps(ops, pod.Namespace, portUUID)
		if err != nil {
			return err
		}
	}

	recordOps, txOkCallBack, _, err := bsnc.AddConfigDurationRecord("pod", pod.Namespace, pod.Name)
	if err != nil {
		klog.Errorf("Config duration recorder: %v", err)
	}
	ops = append(ops, recordOps...)

	transactStart := time.Now()
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(bsnc.nbClient, lsp, ops)
	libovsdbExecuteTime = time.Since(transactStart)
	if err != nil {
		return fmt.Errorf("error transacting operations %+v: %v", ops, err)
	}
	txOkCallBack()

	if lsp != nil {
		_ = bsnc.logicalPortCache.add(pod, switchName, nadKey, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)
		if bsnc.onLogicalPortCacheAdd != nil {
			bsnc.onLogicalPortCacheAdd(pod, nadKey)
		}
		if bsnc.requireDHCP(pod) {
			if err := bsnc.ensureDHCP(pod, podAnnotation, lsp); err != nil {
				return err
			}
		}
	}

	if isLocalPod {
		bsnc.podRecorder.AddLSP(pod.UID, bsnc.GetNetInfo())
		if newlyCreated {
			metrics.RecordPodCreated(pod, bsnc.GetNetInfo())
		}
	}

	return nil
}

// removePodForUserDefinedNetwork tried to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (bsnc *BaseUserDefinedNetworkController) removePodForUserDefinedNetwork(pod *corev1.Pod, portInfoMap map[string]*lpInfo) error {
	if util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) {
		return nil
	}

	podDesc := pod.Namespace + "/" + pod.Name

	// for a specific NAD belongs to this network, Pod's logical port might already be created half-way
	// without its lpInfo cache being created; need to deleted resources created for that NAD as well.
	// So, first get all nadKeys from pod annotation, but handle NADs belong to this network only.
	podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	if portInfoMap == nil {
		portInfoMap = map[string]*lpInfo{}
	}

	var alreadyProcessed bool
	for nadKey, podAnnotation := range podNetworks {
		networkName := bsnc.networkManager.GetNetworkNameForNADKey(nadKey)
		portInfo := portInfoMap[nadKey]
		if networkName == "" && portInfo != nil {
			// The NAD may have been deleted before the Pod delete event is
			// processed. In that case use the cached switch to determine which
			// controller owns the port. The port cache is shared by all network
			// controllers, so the presence of a cached entry alone is not enough.
			var expectedSwitchName string
			expectedSwitchName, err = bsnc.getExpectedSwitchName(pod)
			if err != nil {
				return fmt.Errorf("failed to determine expected switch for pod %s and NAD %s: %w", podDesc, nadKey, err)
			}
			if portInfo.logicalSwitch == expectedSwitchName {
				networkName = bsnc.GetNetworkName()
			}
		}
		if networkName != bsnc.GetNetworkName() {
			continue
		}

		// pod has a network managed by this controller
		klog.Infof("Deleting pod: %s for network %s, NAD key: %s", podDesc, bsnc.GetNetworkName(), nadKey)

		// handle remote pod clean up but only do this one time
		if !bsnc.hasPodLogicalPort(pod) && !alreadyProcessed {
			// except for localnet networks, continue the delete flow in case a node just
			// became remote where we might still need to cleanup. On L3 networks
			// the node switch is removed so there is no need to do this.
			if bsnc.TopologyType() != types.LocalnetTopology {
				return nil
			}
			alreadyProcessed = true
		}

		if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
			if err = bsnc.enableSourceLSPFailedLiveMigration(pod, nadKey, podAnnotation.MAC, podAnnotation.IPs); err != nil {
				return err
			}
		}
		bsnc.logicalPortCache.remove(pod, nadKey)
		var pInfo *lpInfo
		pInfo, err = bsnc.deletePodLogicalPort(pod, portInfo, nadKey)
		if err != nil {
			return err
		}

		// do not release IP address if this controller does not handle IP allocation
		if !bsnc.allocatesPodAnnotation() {
			continue
		}

		if pInfo == nil || len(pInfo.ips) == 0 {
			continue
		}

		// Release only after this attachment's port is gone. LSM preserves
		// any addresses that have since been allocated to a different owner.
		klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s network %s", pod.Namespace, pod.Name,
			util.JoinIPNetIPs(pInfo.ips, " "), bsnc.GetNetworkName())
		if err = bsnc.releasePodIPs(pod, nadKey, pInfo); err != nil {
			return err
		}
	}
	return nil
}

func (bsnc *BaseUserDefinedNetworkController) syncPodsForUserDefinedNetwork(pods []interface{}) error {
	orderedPods, err := logicalswitchmanager.SortPodsForIPAM(pods)
	if err != nil {
		return err
	}
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	expectedLogicalPorts := make(map[string]bool)
	for _, pod := range orderedPods {
		var activeNetwork util.NetInfo
		var err error
		if bsnc.IsPrimaryNetwork() {
			activeNetwork, err = bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// namespace deleted after pod listing - safe to skip
					klog.Infof("%s network controller pod sync: pod %s/%s namespace deleted, skipping",
						bsnc.GetNetworkName(), pod.Namespace, pod.Name)
					continue
				}
				if util.IsInvalidPrimaryNetworkError(err) {
					// If network manager isn't aware of the primary network for this pod's namespace,
					// it can't possibly be already wired to our network unless someone is directly messing
					// with the NADs owned by a CUDN, as network manager syncs all NADs at startup.
					// Skip during initial sync to avoid blocking startup.
					klog.V(5).Infof("%s network controller pod sync: pod %s/%s namespace network not ready, skipping",
						bsnc.GetNetworkName(), pod.Namespace, pod.Name)
					continue
				}
				return fmt.Errorf("failed to find the active network for pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			if activeNetwork == nil || activeNetwork.IsDefault() {
				// no active network for pod, or is a default network pod
				continue
			}
			if activeNetwork.GetNetworkName() != bsnc.GetNetworkName() {
				// network name found but doesn't apply to our controller
				continue
			}
		}

		on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
			pod,
			bsnc.GetNetInfo(),
			activeNetwork,
			bsnc.networkManager.GetNetworkNameForNADKey,
			bsnc.networkManager.GetPrimaryNADForNamespace,
		)
		if err != nil || !on {
			if err != nil {
				bsnc.recordPodErrorEvent(pod, err)
				klog.Errorf("Failed to determine if pod %s/%s needs to be plumb interface on network %s: %v",
					pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
			}
			continue
		}

		isLocalPod := bsnc.isPodScheduledOnLocalNode(pod)
		hasRemotePort := !isLocalPod || bsnc.isLayer2WithInterconnectTransport()

		for nadKey := range networkMap {
			annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
			if err != nil {
				if !util.IsAnnotationNotSetError(err) {
					klog.Errorf("Failed to get pod annotation of pod %s/%s for NAD key %s", pod.Namespace, pod.Name, nadKey)
				}
				continue
			}

			if bsnc.allocatesPodAnnotation() && isLocalPod {
				// only keep track of IPs/ports that have been allocated by this
				// controller
				expectedLogicalPortName, err := bsnc.allocatePodIPs(pod, annotations, nadKey)
				if err != nil {
					return err
				}
				if expectedLogicalPortName != "" {
					expectedLogicalPorts[expectedLogicalPortName] = true
				}
			} else if hasRemotePort {
				// keep also track of remote ports created for layer2 on
				// interconnect
				expectedLogicalPorts[bsnc.GetLogicalPortName(pod, nadKey)] = true
			}
		}
	}

	return bsnc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}

// AddNamespaceForUserDefinedNetwork creates corresponding addressset in ovn db for User Defined Network
func (bsnc *BaseUserDefinedNetworkController) AddNamespaceForUserDefinedNetwork(ns *corev1.Namespace) error {
	klog.Infof("[%s] adding namespace for network %s", ns.Name, bsnc.GetNetworkName())
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v for network %s", ns.Name, time.Since(start), bsnc.GetNetworkName())
	}()

	_, nsUnlock, err := bsnc.ensureNamespaceLockedForUserDefinedNetwork(ns.Name, false, ns)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	nsUnlock()
	return nil
}

// ensureNamespaceLockedForUserDefinedNetwork locks namespacesMutex, gets/creates an entry for ns, configures OVN nsInfo,
// and returns it with its mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
func (bsnc *BaseUserDefinedNetworkController) ensureNamespaceLockedForUserDefinedNetwork(ns string, readOnly bool, namespace *corev1.Namespace) (*namespaceInfo, func(), error) {
	return bsnc.ensureNamespaceLockedCommon(ns, readOnly, namespace, bsnc.configureNamespaceCommon)
}

func (bsnc *BaseUserDefinedNetworkController) updateNamespaceForUserDefinedNetwork(old, newer *corev1.Namespace) error {
	var errors []error
	klog.Infof("[%s] updating namespace for network %s", old.Name, bsnc.GetNetworkName())

	nsInfo, nsUnlock := bsnc.getNamespaceLocked(old.Name, false)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return nil
	}
	defer nsUnlock()

	aclAnnotation := newer.Annotations[util.AclLoggingAnnotation]
	oldACLAnnotation := old.Annotations[util.AclLoggingAnnotation]
	// support for ACL logging update, if new annotation is empty, make sure we propagate new setting
	if aclAnnotation != oldACLAnnotation {
		if err := bsnc.updateNamespaceAclLogging(old.Name, aclAnnotation, nsInfo); err != nil {
			errors = append(errors, err)
		}
	}

	if err := bsnc.multicastUpdateNamespace(newer, nsInfo); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
}

func (bsnc *BaseUserDefinedNetworkController) deleteNamespaceForUserDefinedNetwork(ns *corev1.Namespace) error {
	klog.Infof("[%s] deleting namespace for network %s", ns.Name, bsnc.GetNetworkName())

	nsInfo, err := bsnc.deleteNamespaceLocked(ns.Name)
	if err != nil {
		return err
	}
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := bsnc.multicastDeleteNamespace(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to delete multicast namespace error %v", err)
	}
	return nil
}

// WatchNetworkPolicy starts the watching of networkpolicy resource and calls
// back the appropriate handler logic
func (bsnc *BaseUserDefinedNetworkController) WatchNetworkPolicy() error {
	if bsnc.netPolicyHandler != nil {
		return nil
	}
	handler, err := bsnc.retryNetworkPolicies.WatchResource()
	if err != nil {
		return err
	}
	bsnc.netPolicyHandler = handler
	return nil
}

// WatchMultiNetworkPolicy starts the watching of multinetworkpolicy resource and calls
// back the appropriate handler logic
func (bsnc *BaseUserDefinedNetworkController) WatchMultiNetworkPolicy() error {
	if bsnc.multiNetPolicyHandler != nil {
		return nil
	}
	handler, err := bsnc.retryMultiNetworkPolicies.WatchResource()
	if err != nil {
		return err
	}
	bsnc.multiNetPolicyHandler = handler
	return nil
}

// cleanupGatewayRoutersForNetworkFromDB discovers all gateway routers for the given network from
// the NB DB (by ExternalIDs and GWRouterPrefix) and cleans each one via a dummy GatewayManager.
// Used when gateway managers are empty (e.g. dummy controller or stale cleanup) so cleanup works
// even when nodes are gone.
func cleanupGatewayRoutersForNetworkFromDB(
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	clusterRouterName, joinSwitchName string,
) error {
	var errs []error
	networkName := netInfo.GetNetworkName()
	pred := func(lr *nbdb.LogicalRouter) bool {
		return lr.ExternalIDs[types.NetworkExternalID] == networkName &&
			strings.HasPrefix(lr.Name, types.GWRouterPrefix)
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, pred)
	if err != nil {
		return fmt.Errorf("failed to find gateway routers for network %s: %w", networkName, err)
	}
	layer2UseTransitRouter := netInfo.TopologyType() == types.Layer2Topology && config.Layer2UsesTransitRouter
	for _, lr := range routers {
		nodeName := netInfo.RemoveNetworkScopeFromName(util.GetWorkerFromGatewayRouter(lr.Name))
		gw := NewGatewayManagerForCleanup(nbClient, netInfo, clusterRouterName, joinSwitchName, lr.Name, nodeName, layer2UseTransitRouter)
		if err := gw.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup gateway router %s for network %q (node %s): %w", lr.Name, networkName, nodeName, err))
		}
	}
	return utilerrors.Join(errs...)
}

// cleanupLoadBalancerGroups removes load balancer groups for a user-defined network controller.
// When LB group UUIDs are known (normal controller), they are deleted directly by UUID.
// Otherwise (dummy/stale cleanup controller), the groups are looked up by network-scoped name.
func cleanupLoadBalancerGroups(
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	switchLBGroupUUID, clusterLBGroupUUID, routerLBGroupUUID string,
) {
	networkName := netInfo.GetNetworkName()
	if switchLBGroupUUID != "" || clusterLBGroupUUID != "" || routerLBGroupUUID != "" {
		lbGroups := make([]*nbdb.LoadBalancerGroup, 0, 3)
		for _, lbGroupUUID := range []string{switchLBGroupUUID, clusterLBGroupUUID, routerLBGroupUUID} {
			if lbGroupUUID != "" {
				lbGroups = append(lbGroups, &nbdb.LoadBalancerGroup{UUID: lbGroupUUID})
			}
		}
		if err := libovsdbops.DeleteLoadBalancerGroups(nbClient, lbGroups); err != nil {
			klog.Errorf("Failed to delete load balancer groups on network: %q, error: %v", networkName, err)
		}
		return
	}
	// Dummy controller (e.g. stale UDN cleanup): find LB groups by network-scoped name and delete them
	names := map[string]bool{
		netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName):       true,
		netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName): true,
		netInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName): true,
	}
	staleLBGroups, err := libovsdbops.FindLoadBalancerGroupsWithPredicate(nbClient, func(g *nbdb.LoadBalancerGroup) bool {
		return names[g.Name]
	})
	if err != nil {
		klog.Errorf("Failed to find load balancer groups for stale network %q: %v", networkName, err)
	} else if len(staleLBGroups) > 0 {
		if err := libovsdbops.DeleteLoadBalancerGroups(nbClient, staleLBGroups); err != nil {
			klog.Errorf("Failed to delete load balancer groups on stale network: %q, error: %v", networkName, err)
		}
	}
}

// cleanupPolicyLogicalEntities cleans up all the port groups and address sets that belong to the given controller
func cleanupPolicyLogicalEntities(nbClient libovsdbclient.Client, ops []ovsdb.Operation, controllerName string) ([]ovsdb.Operation, error) {
	var err error
	portGroupPredicate := func(item *nbdb.PortGroup) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName
	}
	ops, err = libovsdbops.DeletePortGroupsWithPredicateOps(nbClient, ops, portGroupPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to get ops to delete port groups owned by controller %s", controllerName)
	}

	// Skip PodSelector address sets - those are owned by the shared AddressSetManager
	// and cleaned up via AddressSetManager.CleanupForController in the controller's cleanup().
	asPredicate := func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName &&
			item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] != libovsdbops.PodSelectorOwnerType
	}
	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(nbClient, ops, asPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to get ops to delete address sets owned by controller %s", controllerName)
	}
	return ops, nil
}

// buildUDNEgressSNAT is used to build the conditional SNAT required on L3 and L2 UDNs to
// steer traffic correctly via mp0 when leaving OVN to the host
func (bsnc *BaseUserDefinedNetworkController) buildUDNEgressSNAT(localPodSubnets []*net.IPNet, outputPort string, isUDNAdvertised bool) ([]*nbdb.NAT, error) {
	if len(localPodSubnets) == 0 {
		return nil, nil // nothing to do
	}
	var snats []*nbdb.NAT
	var masqIP *udn.MasqueradeIPs
	var err error
	networkID := bsnc.GetNetworkID()
	// calculate MAC
	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())

	extIDs := map[string]string{
		types.NetworkExternalID:  bsnc.GetNetworkName(),
		types.TopologyExternalID: bsnc.TopologyType(),
	}

	var nodeIPsAS, svcIPsAS addressset.AddressSet
	if isUDNAdvertised {
		// For advertised networks, we need to SNAT any traffic leaving the
		// pods from these networks towards the node IPs in the cluster. In
		// order to do such a conditional SNAT, we need an address set that
		// contains the node IPs in the cluster. Re-use the shared cluster node
		// IP address set owned by the address set manager.
		nodeIPsASIDs, err := bsnc.addressSetManager.EnsureClusterNodeIPsAddressSet(addresssetmanager.ClusterNodeIPsRouteAdvertisementsBackRef)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure cluster node IP address set for route advertisements: %w", err)
		}
		nodeIPsAS, err = bsnc.addressSetFactory.GetAddressSet(nodeIPsASIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get address set with IDs %v: %w", nodeIPsASIDs, err)
		}

		// We also need to SNAT any traffic leaving the pods from these
		// networks towards the default network service cluster IPs
		// accessible from UDNs: we want the reply traffic to hit the
		// masquerade IP rule rather than the UDN subnet ip rule to allow
		// for overlaps in VRF-Lite configurations
		svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
		svcIPsAS, err = bsnc.addressSetFactory.GetAddressSet(svcIPsASIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get address set with IDs %v: %w", svcIPsASIDs, err)
		}
	}

	for _, localPodSubnet := range localPodSubnets {
		snatMatch := dstMacMatch
		ipFamily := utilnet.IPv4
		masqIP, err = udn.AllocateV4MasqueradeIPs(networkID)
		if utilnet.IsIPv6CIDR(localPodSubnet) {
			masqIP, err = udn.AllocateV6MasqueradeIPs(networkID)
			ipFamily = utilnet.IPv6
		}
		if err != nil {
			return nil, err
		}
		if masqIP == nil {
			return nil, fmt.Errorf("masquerade IP cannot be empty network %s (%d): %v", bsnc.GetNetworkName(), networkID, err)
		}

		if isUDNAdvertised && config.Gateway.Mode == config.GatewayModeShared {
			addedAllowedExtIPsSNAT := false
			// OVN NAT.allowed_ext_ips accepts a single Address_Set, so create one
			// SNAT per allowed destination set.
			for _, allowedExtIPsAS := range []addressset.AddressSet{nodeIPsAS, svcIPsAS} {
				allowedExtIPs := getAddressSetUUIDForIPFamily(ipFamily, allowedExtIPsAS)
				if allowedExtIPs == "" {
					continue
				}
				snats = append(snats, libovsdbops.BuildSNATWithAllowedExtIPs(
					&masqIP.ManagementPort.IP,
					localPodSubnet,
					outputPort,
					extIDs,
					"",
					allowedExtIPs,
				))
				addedAllowedExtIPsSNAT = true
			}
			if !addedAllowedExtIPsSNAT {
				return nil, fmt.Errorf("failed to build allowed_ext_ips SNAT for advertised network %s, subnet %s: no address set UUID for IPv%s",
					bsnc.GetNetworkName(), localPodSubnet, ipFamily)
			}
			continue // move to the next pod subnet
		}

		if isUDNAdvertised {
			additionalSNATMatch := getClusterNodesDestinationBasedSNATMatch(ipFamily, nodeIPsAS, svcIPsAS)
			if additionalSNATMatch != "" {
				snatMatch = fmt.Sprintf("%s && %s", snatMatch, additionalSNATMatch)
			}
		}

		// For noOverlay mode with outboundSNAT enabled in local gateway mode, add exempted_ext_ips
		// to prevent SNATing pod-to-pod traffic within the same CUDN while still SNATing pod-to-external traffic.
		// This SNAT is on ovn_cluster_router, which is used in local gateway mode.
		var snat *nbdb.NAT
		if bsnc.GetNetInfo().Transport() == types.NetworkTransportNoOverlay &&
			bsnc.GetNetInfo().OutboundSNAT() == types.NoOverlaySNATEnabled &&
			config.Gateway.Mode == config.GatewayModeLocal {
			snatMatch = ""
			v4UUID, v6UUID, err := getNoOverlaySNATExemptionAsUUID(bsnc.addressSetFactory, bsnc.GetNetInfo(), bsnc.controllerName)
			if err != nil {
				return nil, fmt.Errorf("failed to get no-overlay SNAT exemption address set UUID: %w", err)
			}
			// Use the appropriate UUID based on IP family
			exemptedExtIPs := v4UUID
			if ipFamily == utilnet.IPv6 {
				exemptedExtIPs = v6UUID
			}
			snat = libovsdbops.BuildSNATWithExemptedExtIPs(
				&masqIP.ManagementPort.IP,
				localPodSubnet,
				outputPort,
				extIDs,
				snatMatch,
				exemptedExtIPs,
			)
		} else {
			snat = libovsdbops.BuildSNATWithMatch(
				&masqIP.ManagementPort.IP,
				localPodSubnet,
				outputPort,
				extIDs,
				snatMatch,
			)
		}
		snats = append(snats, snat)
	}

	return snats, nil
}

func getAddressSetUUIDForIPFamily(ipFamily utilnet.IPFamily, as addressset.AddressSet) string {
	if as == nil {
		return ""
	}
	v4UUID, v6UUID := as.GetASUUID()
	if ipFamily == utilnet.IPv6 {
		return v6UUID
	}
	return v4UUID
}

func getMasqueradeManagementIPSNATMatch(dstMac string) string {
	return fmt.Sprintf("eth.dst == %s", dstMac)
}

// getClusterNodesDestinationBasedSNATMatch creates destination-based SNAT match
// for the specified IP family. Returns an empty string if there is no address
// set for the provided IP family.
func getClusterNodesDestinationBasedSNATMatch(ipFamily utilnet.IPFamily, addressSets ...addressset.AddressSet) string {
	asMatches := make([]string, 0, len(addressSets))
	for _, as := range addressSets {
		asIPv4, asIPv6 := as.GetASHashNames()
		switch {
		case ipFamily == utilnet.IPv4 && asIPv4 != "":
			asMatches = append(asMatches, fmt.Sprintf("ip4.dst == $%s", asIPv4))
		case ipFamily == utilnet.IPv6 && asIPv6 != "":
			asMatches = append(asMatches, fmt.Sprintf("ip6.dst == $%s", asIPv6))
		}
	}
	switch len(asMatches) {
	case 0:
		return ""
	case 1:
		return asMatches[0]
	default:
		return fmt.Sprintf("(%s)", strings.Join(asMatches, " || "))
	}
}

func (bsnc *BaseUserDefinedNetworkController) requireDHCP(pod *corev1.Pod) bool {
	// Configure DHCP only for kubevirt VMs layer2 primary udn with subnets
	return kubevirt.IsPodOwnedByVirtualMachine(pod) &&
		util.IsNetworkSegmentationSupportEnabled() &&
		bsnc.IsPrimaryNetwork() &&
		bsnc.TopologyType() == types.Layer2Topology
}

func (bsnc *BaseUserDefinedNetworkController) setPodLogicalSwitchPortAddressesAndEnabledField(
	pod *corev1.Pod, nadKey string, mac string, ips []string, enabled bool, ops []ovsdb.Operation,
) ([]ovsdb.Operation, *nbdb.LogicalSwitchPort, error) {
	lsp := &nbdb.LogicalSwitchPort{Name: bsnc.GetLogicalPortName(pod, nadKey)}
	lsp.Enabled = ptr.To(enabled)
	customFields := []libovsdbops.ModelUpdateField{
		libovsdbops.LogicalSwitchPortEnabled,
		libovsdbops.LogicalSwitchPortAddresses,
	}
	if !enabled {
		lsp.Addresses = nil
	} else {
		if len(mac) == 0 || len(ips) == 0 {
			return nil, nil, fmt.Errorf("failed to configure addresses for lsp, missing mac and ips for pod %s", pod.Name)
		}

		// Remove length
		for i, ip := range ips {
			ips[i] = strings.Split(ip, "/")[0]
		}

		lsp.Addresses = []string{
			strings.Join(append([]string{mac}, ips...), " "),
		}

		if bsnc.MACSecurityMode() == types.MACSecurityModeDisabled {
			lsp.Addresses = append(lsp.Addresses, "unknown")
		}
	}
	switchName, err := bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch switch name for pod %s: %w", pod.Name, err)
	}
	ops, err = libovsdbops.UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(bsnc.nbClient, ops, &nbdb.LogicalSwitch{Name: switchName}, customFields, lsp)
	if err != nil {
		return nil, nil, fmt.Errorf("failed updating logical switch port %+v on switch %s: %w", *lsp, switchName, err)
	}
	return ops, lsp, nil
}

func (bsnc *BaseUserDefinedNetworkController) disableLiveMigrationSourceLSPOps(
	kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus,
	nadKey string, ops []ovsdb.Operation,
) ([]ovsdb.Operation, error) {

	// Only disable the source LSP if it exists at this zone
	if !bsnc.hasPodLogicalPort(kubevirtLiveMigrationStatus.SourcePod) {
		return ops, nil
	}

	ops, _, err := bsnc.setPodLogicalSwitchPortAddressesAndEnabledField(kubevirtLiveMigrationStatus.SourcePod, nadKey, "", nil, false, ops)
	return ops, err
}

func (bsnc *BaseUserDefinedNetworkController) enableSourceLSPFailedLiveMigration(pod *corev1.Pod, nadKey string, mac string, ips []string) error {
	kubevirtLiveMigrationStatus, err := kubevirt.DiscoverLiveMigrationStatus(bsnc.watchFactory.PodCoreInformer().Lister(), pod)
	if err != nil {
		return fmt.Errorf("failed to discover Live-migration status after pod termination: %w", err)
	}
	if kubevirtLiveMigrationStatus == nil ||
		pod.Name != kubevirtLiveMigrationStatus.TargetPod.Name ||
		kubevirtLiveMigrationStatus.State != kubevirt.LiveMigrationFailed {
		return nil
	}

	// Only re-enable the source LSP if it exists at this zone
	if !bsnc.hasPodLogicalPort(kubevirtLiveMigrationStatus.SourcePod) {
		return nil
	}

	// make sure sourcePod lsp is enabled if migration failed after DomainReady was set.
	ops, sourcePodLsp, err := bsnc.setPodLogicalSwitchPortAddressesAndEnabledField(kubevirtLiveMigrationStatus.SourcePod, nadKey, mac, ips, true, nil)
	if err != nil {
		return fmt.Errorf("failed to set source Pod lsp to enabled after migration failed: %w", err)
	}
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(bsnc.nbClient, sourcePodLsp, ops)
	if err != nil {
		return fmt.Errorf("failed transacting operations %+v: %w", ops, err)
	}

	return nil
}

// hasPodLogicalPort On localnet topologies with interconnect the pod's LSP lives only on the
// node where the pod was scheduled
func (bsnc *BaseUserDefinedNetworkController) hasPodLogicalPort(pod *corev1.Pod) bool {
	return pod != nil && (bsnc.isPodScheduledOnLocalNode(pod) || bsnc.isLayer2WithInterconnectTransport())
}

func shouldAddPort(oldPod, newPod *corev1.Pod, inRetryCache bool) bool {
	return inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod)
}

// dhcpPodNetworkUpdated returns true when this network learns IPs from an
// external DHCP server and this network's entries in the pod-networks
// annotation were added or updated. ovnkube-node patches the DHCP-learned IP
// into the annotation during CNI ADD after the port was first created, and
// may patch it again with a different lease if the sandbox is recreated
// before the pod reaches Running. So the port must be reprocessed to pick up
// the IP.
//
// DHCP IPAM is localnet-secondary only, so the consumers of the
// changed IP are exactly the features supported on such networks: the LSP
// itself (addresses/port security) handled here, plus MultiNetworkPolicy and
// NetworkQoS, which re-evaluate the pod's addresses from their own pod
// UPDATE handlers.
//
// The annotation is a single blob shared by every network the pod attaches
// to, and DHCP pods are multi-homed by construction (localnet is
// secondary-only), so a whole-string comparison would turn every other
// network's annotation write (at least one per network during pod bring-up)
// into a spurious full addLogicalPort pass with a real NBDB transaction.
// Compare only the entries belonging to this network, resolved the same way
// removePodForUserDefinedNetwork does.
//
// A removed entry deliberately does NOT trigger: nothing legitimately removes
// a DHCP entry from a live pod, and reprocessing the port without its
// annotation would only churn (the CNI is the annotation's single writer).
func (bsnc *BaseUserDefinedNetworkController) dhcpPodNetworkUpdated(oldPod, newPod *corev1.Pod) bool {
	if bsnc.IPAMType() != types.IPAMTypeDHCP || oldPod == nil || newPod == nil {
		return false
	}
	if oldPod.Annotations[types.OvnPodAnnotationName] == newPod.Annotations[types.OvnPodAnnotationName] {
		return false
	}
	// If either annotation cannot be parsed, we cannot tell whether this
	// network's entry changed, so return true and reprocess the port. A
	// needless reprocess is harmless (addLogicalPort is idempotent), but
	// skipping a real DHCP IP update would leave the port without its
	// address forever, as no later event retries it.
	oldNetworks, err := util.UnmarshalPodAnnotationAllNetworks(oldPod.Annotations)
	if err != nil {
		klog.Warningf("Failed to unmarshal pod-networks annotation of old pod %s/%s on network %s, reprocessing the port: %v",
			oldPod.Namespace, oldPod.Name, bsnc.GetNetworkName(), err)
		return true
	}
	newNetworks, err := util.UnmarshalPodAnnotationAllNetworks(newPod.Annotations)
	if err != nil {
		klog.Warningf("Failed to unmarshal pod-networks annotation of pod %s/%s on network %s, reprocessing the port: %v",
			newPod.Namespace, newPod.Name, bsnc.GetNetworkName(), err)
		return true
	}
	for nadKey, newEntry := range newNetworks {
		if bsnc.networkManager.GetNetworkNameForNADKey(nadKey) != bsnc.GetNetworkName() {
			continue
		}
		oldEntry, existed := oldNetworks[nadKey]
		if !existed || !reflect.DeepEqual(oldEntry, newEntry) {
			return true
		}
	}
	return false
}

func nodesToInterfaces(nodes []*corev1.Node) []interface{} {
	objs := make([]interface{}, 0, len(nodes))
	for _, node := range nodes {
		objs = append(objs, node)
	}
	return objs
}

func nodeSubnetChangedForUDN(oldNode, newNode *corev1.Node, netName string, oldState, newState *nodecontroller.NodeAnnotationState) bool {
	if !util.NodeSubnetAnnotationChanged(oldNode, newNode) {
		return false
	}
	return nodecontroller.NodeSubnetAnnotationChangedForNetworkWithState(oldState, newState, netName)
}
