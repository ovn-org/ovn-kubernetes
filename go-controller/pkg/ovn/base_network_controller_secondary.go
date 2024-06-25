package ovn

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

func (bsnc *BaseSecondaryNetworkController) getPortInfoForSecondaryNetwork(pod *kapi.Pod) map[string]*lpInfo {
	if util.PodWantsHostNetwork(pod) {
		return nil
	}
	portInfoMap, _ := bsnc.logicalPortCache.getAll(pod)
	return portInfoMap
}

// GetInternalCacheEntryForSecondaryNetwork returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (bsnc *BaseSecondaryNetworkController) GetInternalCacheEntryForSecondaryNetwork(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return bsnc.getPortInfoForSecondaryNetwork(pod)
	default:
		return nil
	}
}

// AddSecondaryNetworkResourceCommon adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. This function is called for secondary network only.
func (bsnc *BaseSecondaryNetworkController) AddSecondaryNetworkResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return bsnc.ensurePodForSecondaryNetwork(pod, true)

	case factory.NamespaceType:
		ns, ok := obj.(*kapi.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return bsnc.AddNamespaceForSecondaryNetwork(ns)

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
	case factory.IPAMClaimsType:
		return nil

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
	return nil
}

// UpdateSecondaryNetworkResourceCommon updates the specified object in the cluster to its version in newObj
// according to its type and returns the error, if any, yielded during the object update. This function is
// called for secondary network only.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (bsnc *BaseSecondaryNetworkController) UpdateSecondaryNetworkResourceCommon(objType reflect.Type, oldObj, newObj interface{}, inRetryCache bool) error {
	switch objType {
	case factory.PodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)

		return bsnc.ensurePodForSecondaryNetwork(newPod, inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod))

	case factory.NamespaceType:
		oldNs, newNs := oldObj.(*kapi.Namespace), newObj.(*kapi.Namespace)
		return bsnc.updateNamespaceForSecondaryNetwork(oldNs, newNs)

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
	case factory.IPAMClaimsType:
		return nil

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
	return nil
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods.
// This function is called for secondary network only.
func (bsnc *BaseSecondaryNetworkController) DeleteSecondaryNetworkResourceCommon(objType reflect.Type, obj, cachedObj interface{}) error {
	switch objType {
	case factory.PodType:
		var portInfoMap map[string]*lpInfo
		pod := obj.(*kapi.Pod)

		if cachedObj != nil {
			portInfoMap = cachedObj.(map[string]*lpInfo)
		}
		return bsnc.removePodForSecondaryNetwork(pod, portInfoMap)

	case factory.NamespaceType:
		ns := obj.(*kapi.Namespace)
		return bsnc.deleteNamespace4SecondaryNetwork(ns)

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

	case factory.IPAMClaimsType:
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *ipamclaimsapi.IPAMClaim", obj)
		}

		switchName, err := bsnc.getExpectedSwitchName(dummyPod())
		if err != nil {
			return err
		}
		ipAllocator := bsnc.lsManager.ForSwitch(switchName)
		err = bsnc.ipamClaimsReconciler.Reconcile(ipamClaim, nil, ipAllocator)
		if err != nil && !errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return fmt.Errorf("error deleting IPAMClaim: %w", err)
		} else if errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return nil // let's avoid the log below, since nothing was released.
		}
		klog.Infof("Released IPs %q for network %q", ipamClaim.Status.IPs, ipamClaim.Spec.Network)

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
	return nil
}

// ensurePodForSecondaryNetwork tries to set up secondary network for a pod. It returns nil on success and error
// on failure; failure indicates the pod set up should be retried later.
func (bsnc *BaseSecondaryNetworkController) ensurePodForSecondaryNetwork(pod *kapi.Pod, addPort bool) error {

	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if util.PodWantsHostNetwork(pod) && !addPort {
		return nil
	}

	// If a node does not have an assigned hostsubnet don't wait for the logical switch to appear
	switchName, err := bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return err
	}

	if bsnc.IsPrimaryNetwork() && (bsnc.TopologyType() == "layer2" || bsnc.TopologyType() == "layer3") {
		network := &nadapi.NetworkSelectionElement{
			// TODO: we need to get the NAD name here, not the network name.
			Name:      bsnc.GetNetworkName(),
			Namespace: pod.Namespace,
		}
		return bsnc.ensurePodForUserDefinedPrimaryNet(pod, network)
	}

	on, networkMap, err := util.GetPodNADToNetworkMapping(pod, bsnc.NetInfo)
	if err != nil {
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

	if bsnc.doesNetworkRequireIPAM() && bsnc.lsManager.IsNonHostSubnetSwitch(switchName) {
		klog.V(5).Infof(
			"Pod %s/%s requires IPAM but does not have an assigned IP address", pod.Namespace, pod.Name)
		return nil
	}

	var errs []error
	for nadName, network := range networkMap {
		if err = bsnc.addLogicalPortToNetworkForNAD(pod, nadName, switchName, network); err != nil {
			errs = append(errs, fmt.Errorf("failed to add logical port of Pod %s/%s for NAD %s: %w", pod.Namespace, pod.Name, nadName, err))
		}
	}
	if len(errs) != 0 {
		return utilerrors.Join(errs...)
	}
	return nil
}

func (bsnc *BaseSecondaryNetworkController) addLogicalPortToNetworkForNAD(pod *kapi.Pod, nadName, switchName string,
	network *nadapi.NetworkSelectionElement) error {
	var libovsdbExecuteTime time.Duration

	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort for NAD %s took %v, libovsdb time %v",
			pod.Namespace, pod.Name, nadName, time.Since(start), libovsdbExecuteTime)
	}()

	var err error
	var podAnnotation *util.PodAnnotation
	var ops []ovsdb.Operation
	var lsp *nbdb.LogicalSwitchPort
	var newlyCreated bool

	// we need to create a logical port for all local pods
	// we also need to create a remote logical port for remote pods on layer2
	// topologies with interconnect
	isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
	requiresLogicalPort := isLocalPod || bsnc.isLayer2Interconnect()

	if requiresLogicalPort {
		ops, lsp, podAnnotation, newlyCreated, err = bsnc.addLogicalPortToNetwork(pod, nadName, network)
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
		logicalPort := bsnc.GetLogicalPortName(pod, nadName)
		expectedSwitchName, err := bsnc.getExpectedSwitchName(pod)
		if err != nil {
			return err
		}
		ops, err = bsnc.delLSPOps(logicalPort, expectedSwitchName, "")
		if err != nil {
			return err
		}
		bsnc.logicalPortCache.remove(pod, nadName)
	}

	if podAnnotation == nil {
		podAnnotation, err = util.UnmarshalPodAnnotation(pod.Annotations, nadName)
		if err != nil {
			return err
		}
	}

	if bsnc.doesNetworkRequireIPAM() && util.IsMultiNetworkPoliciesSupportEnabled() {
		// Ensure the namespace/nsInfo exists
		addOps, err := bsnc.addPodToNamespaceForSecondaryNetwork(pod.Namespace, podAnnotation.IPs)
		if err != nil {
			return err
		}
		ops = append(ops, addOps...)
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
		_ = bsnc.logicalPortCache.add(pod, switchName, nadName, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)
	}

	// we need to create the binding ourselves for the remote ports we create on
	// layer2 topologies with interconnect
	isRemotePort := !isLocalPod && bsnc.isLayer2Interconnect()
	if isRemotePort {
		err := bsnc.zoneICHandler.BindTransitRemotePort(pod.Spec.NodeName, lsp.Name)
		if err != nil {
			return fmt.Errorf("failed to bind remote transit port: %w", err)
		}
	}

	if isLocalPod {
		bsnc.podRecorder.AddLSP(pod.UID, bsnc.NetInfo)
		if newlyCreated {
			metrics.RecordPodCreated(pod, bsnc.NetInfo)
		}
	}

	return nil
}

// removePodForSecondaryNetwork tried to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (bsnc *BaseSecondaryNetworkController) removePodForSecondaryNetwork(pod *kapi.Pod, portInfoMap map[string]*lpInfo) error {
	if util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) {
		return nil
	}

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s for network %s", podDesc, bsnc.GetNetworkName())

	// there is only a logical port for local pods or remote pods of layer2
	// networks on interconnect, so only delete in these cases
	isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
	hasLogicalPort := isLocalPod || bsnc.isLayer2Interconnect()

	// otherwise just delete pod IPs from the namespace address set
	if !hasLogicalPort {
		if bsnc.doesNetworkRequireIPAM() && util.IsMultiNetworkPoliciesSupportEnabled() {
			return bsnc.removeRemoteZonePodFromNamespaceAddressSet(pod)
		}

		// except for localnet networks, continue the delete flow in case a node just
		// became remote where we might still need to cleanup. On L3 networks
		// the node switch is removed so there is no need to do this.
		if bsnc.TopologyType() != types.LocalnetTopology {
			return nil
		}
	}

	// for a specific NAD belongs to this network, Pod's logical port might already be created half-way
	// without its lpInfo cache being created; need to deleted resources created for that NAD as well.
	// So, first get all nadNames from pod annotation, but handle NADs belong to this network only.
	podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	if portInfoMap == nil {
		portInfoMap = map[string]*lpInfo{}
	}
	for nadName := range podNetworks {
		if !bsnc.HasNAD(nadName) {
			continue
		}

		_, networkMap, err := util.GetPodNADToNetworkMapping(pod, bsnc.NetInfo)
		if err != nil {
			return err
		}

		bsnc.logicalPortCache.remove(pod, nadName)
		pInfo, err := bsnc.deletePodLogicalPort(pod, portInfoMap[nadName], nadName)
		if err != nil {
			return err
		}

		// do not release IP address if this controller does not handle IP allocation
		if !bsnc.allocatesPodAnnotation() {
			continue
		}

		// do not release IP address unless we have validated no other pod is using it
		if pInfo == nil || len(pInfo.ips) == 0 {
			bsnc.forgetPodReleasedBeforeStartup(string(pod.UID), nadName)
			continue
		}

		network := networkMap[nadName]

		hasPersistentIPs := bsnc.allowPersistentIPs()
		hasIPAMClaim := network != nil && network.IPAMClaimReference != ""
		if hasIPAMClaim && !hasPersistentIPs {
			klog.Errorf(
				"Pod %s/%s referencing an IPAMClaim on network %q which does not honor it",
				pod.GetNamespace(),
				pod.GetName(),
				bsnc.NetInfo.GetNetworkName(),
			)
			hasIPAMClaim = false
		}
		if hasIPAMClaim {
			ipamClaim, err := bsnc.ipamClaimsReconciler.FindIPAMClaim(network.IPAMClaimReference, network.Namespace)
			hasIPAMClaim = ipamClaim != nil && len(ipamClaim.Status.IPs) > 0
			if apierrors.IsNotFound(err) {
				klog.Errorf("Failed to retrieve IPAMClaim %q but will release IPs: %v", network.IPAMClaimReference, err)
			} else if err != nil {
				return fmt.Errorf("failed to get IPAMClaim %s/%s: %w", network.Namespace, network.IPAMClaimReference, err)
			}
		}

		if hasIPAMClaim {
			continue
		}
		// Releasing IPs needs to happen last so that we can deterministically know that if delete failed that
		// the IP of the pod needs to be released. Otherwise we could have a completed pod failed to be removed
		// and we dont know if the IP was released or not, and subsequently could accidentally release the IP
		// while it is now on another pod
		klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s network %s", pod.Namespace, pod.Name,
			util.JoinIPNetIPs(pInfo.ips, " "), bsnc.GetNetworkName())
		if err = bsnc.releasePodIPs(pInfo); err != nil {
			return err
		}

		bsnc.forgetPodReleasedBeforeStartup(string(pod.UID), nadName)
	}
	return nil
}

func (bsnc *BaseSecondaryNetworkController) syncPodsForSecondaryNetwork(pods []interface{}) error {
	annotatedLocalPods := map[*kapi.Pod]map[string]*util.PodAnnotation{}
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("spurious object in syncPods: %v", podInterface)
		}
		on, networkMap, err := util.GetPodNADToNetworkMapping(pod, bsnc.NetInfo)
		if err != nil || !on {
			if err != nil {
				klog.Warningf("Failed to determine if pod %s/%s needs to be plumb interface on network %s: %v",
					pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
			}
			continue
		}

		isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
		hasRemotePort := !isLocalPod || bsnc.isLayer2Interconnect()

		for nadName := range networkMap {
			annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
			if err != nil {
				if !util.IsAnnotationNotSetError(err) {
					klog.Errorf("Failed to get pod annotation of pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
				}
				continue
			}

			if bsnc.allocatesPodAnnotation() && isLocalPod {
				// only keep track of IPs/ports that have been allocated by this
				// controller
				expectedLogicalPortName, err := bsnc.allocatePodIPs(pod, annotations, nadName)
				if err != nil {
					return err
				}
				if expectedLogicalPortName != "" {
					expectedLogicalPorts[expectedLogicalPortName] = true
				}

				if annotatedLocalPods[pod] == nil {
					annotatedLocalPods[pod] = map[string]*util.PodAnnotation{}
				}
				annotatedLocalPods[pod][nadName] = annotations
			} else if hasRemotePort {
				// keep also track of remote ports created for layer2 on
				// interconnect
				expectedLogicalPorts[bsnc.GetLogicalPortName(pod, nadName)] = true
			}
		}
	}

	// keep track of which pods might have already been released
	bsnc.trackPodsReleasedBeforeStartup(annotatedLocalPods)

	return bsnc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}

// addPodToNamespaceForSecondaryNetwork returns the ops needed to add pod's IP to the namespace's address set.
func (bsnc *BaseSecondaryNetworkController) addPodToNamespaceForSecondaryNetwork(ns string, ips []*net.IPNet) ([]ovsdb.Operation, error) {
	var ops []ovsdb.Operation
	var err error
	nsInfo, nsUnlock, err := bsnc.ensureNamespaceLockedForSecondaryNetwork(ns, true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure namespace locked: %v", err)
	}

	defer nsUnlock()

	if ops, err = nsInfo.addressSet.AddAddressesReturnOps(util.IPNetsIPToStringSlice(ips)); err != nil {
		return nil, err
	}

	return ops, nil
}

// AddNamespaceForSecondaryNetwork creates corresponding addressset in ovn db for secondary network
func (bsnc *BaseSecondaryNetworkController) AddNamespaceForSecondaryNetwork(ns *kapi.Namespace) error {
	klog.Infof("[%s] adding namespace for network %s", ns.Name, bsnc.GetNetworkName())
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v for network %s", ns.Name, time.Since(start), bsnc.GetNetworkName())
	}()

	_, nsUnlock, err := bsnc.ensureNamespaceLockedForSecondaryNetwork(ns.Name, false, ns)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	defer nsUnlock()
	return nil
}

// ensureNamespaceLockedForSecondaryNetwork locks namespacesMutex, gets/creates an entry for ns, configures OVN nsInfo,
// and returns it with its mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
func (bsnc *BaseSecondaryNetworkController) ensureNamespaceLockedForSecondaryNetwork(ns string, readOnly bool, namespace *kapi.Namespace) (*namespaceInfo, func(), error) {
	return bsnc.ensureNamespaceLockedCommon(ns, readOnly, namespace, bsnc.getAllNamespacePodAddresses, bsnc.configureNamespaceCommon)
}

func (bsnc *BaseSecondaryNetworkController) updateNamespaceForSecondaryNetwork(old, newer *kapi.Namespace) error {
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

func (bsnc *BaseSecondaryNetworkController) deleteNamespace4SecondaryNetwork(ns *kapi.Namespace) error {
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

// WatchMultiNetworkPolicy starts the watching of multinetworkpolicy resource and calls
// back the appropriate handler logic
func (bsnc *BaseSecondaryNetworkController) WatchMultiNetworkPolicy() error {
	if !util.IsMultiNetworkPoliciesSupportEnabled() {
		return nil
	}

	if bsnc.policyHandler != nil {
		return nil
	}
	handler, err := bsnc.retryNetworkPolicies.WatchResource()
	if err != nil {
		bsnc.policyHandler = handler
	}
	return err
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

	asPredicate := func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName
	}
	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(nbClient, ops, asPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to get ops to delete address sets owned by controller %s", controllerName)
	}
	return ops, nil
}

// WatchIPAMClaims starts the watching of IPAMClaim resources and calls
// back the appropriate handler logic
func (bsnc *BaseSecondaryNetworkController) WatchIPAMClaims() error {
	if bsnc.ipamClaimsHandler != nil {
		return nil
	}
	handler, err := bsnc.retryIPAMClaims.WatchResource()
	if err != nil {
		bsnc.ipamClaimsHandler = handler
	}
	return err
}

func (oc *BaseSecondaryNetworkController) allowPersistentIPs() bool {
	return config.OVNKubernetesFeature.EnablePersistentIPs &&
		oc.NetInfo.AllowsPersistentIPs() &&
		util.DoesNetworkRequireIPAM(oc.NetInfo) &&
		(oc.NetInfo.TopologyType() == types.Layer2Topology || oc.NetInfo.TopologyType() == types.LocalnetTopology)
}

func (bsnc *BaseSecondaryNetworkController) ensurePodForUserDefinedPrimaryNet(pod *kapi.Pod, network *nadapi.NetworkSelectionElement) error {
	klog.Infof("DEBUG| invoking the port builder for the defined network")
	switchName, err := bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return err
	}

	nadName := fmt.Sprintf("%s/%s", pod.Namespace, network.Name)
	var errs []error
	if err := bsnc.addLogicalPortToNetworkForNAD(pod, nadName, switchName, network); err != nil {
		errs = append(
			errs,
			fmt.Errorf(
				"failed to add logical port of Pod %s/%s for NAD %s: %w",
				pod.Namespace,
				pod.Name,
				nadName,
				err,
			),
		)
	}

	return nil
}
