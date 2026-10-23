// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
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
	if len(portInfoMap) == 0 {
		return nil
	}

	networkPortInfoMap := map[string]*lpInfo{}
	for nadKey, portInfo := range portInfoMap {
		if portInfo != nil && portInfo.appliedNetworkName == bsnc.GetNetworkName() {
			networkPortInfoMap[nadKey] = portInfo
		}
	}
	if len(networkPortInfoMap) == 0 {
		return nil
	}
	return networkPortInfoMap
}

// getReplacementPod returns the current pod when the namespaced name has been
// reused by a different pod incarnation.
func (bsnc *BaseUserDefinedNetworkController) getReplacementPod(pod *corev1.Pod) (*corev1.Pod, error) {
	if bsnc.watchFactory == nil {
		return nil, nil
	}
	currentPod, err := bsnc.watchFactory.GetPod(pod.Namespace, pod.Name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get current pod %s/%s while deleting UID %s: %w",
			pod.Namespace, pod.Name, pod.UID, err)
	}
	if currentPod.UID == pod.UID {
		return nil, nil
	}
	return currentPod, nil
}

// reconcileNetworkPolicyPortGroupsAfterDelete removes membership owned by
// the deleted identity, or reconciles the latest same-name replacement. Policy
// membership is keyed by namespace/name rather than UID, so blindly deleting
// it would race a replacement while blindly preserving it would leak the old
// pod's membership for ineligible replacements such as HostNetwork or pending
// pods.
func (bsnc *BaseUserDefinedNetworkController) reconcileNetworkPolicyPortGroupsAfterDelete(pod, replacementPod *corev1.Pod) error {
	if replacementPod != nil {
		return bsnc.reconcileNetworkPolicyPortGroupsWithPortCheck(replacementPod, false)
	}
	return bsnc.deletePodNetworkPolicyMembership(pod)
}

// getCurrentPodLogicalPortNADs returns the NADs for which the current pod
// should have an LSP in this zone. If the current pod's attachment cannot be
// evaluated, desiredKnown is false and legacy ports must be preserved. The
// returned error lets callers retain a delete retry when that ambiguity
// prevents an applied legacy port from being classified.
func (bsnc *BaseUserDefinedNetworkController) getCurrentPodLogicalPortNADs(pod *corev1.Pod) (nadKeys sets.Set[string], desiredKnown bool, err error) {
	nadKeys = sets.New[string]()
	if pod == nil || util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) || !bsnc.hasPodLogicalPort(pod) {
		return nadKeys, true, nil
	}

	on, networkMap, err := bsnc.podNetworkSelectionForUserDefinedNetwork(pod)
	if err != nil {
		resolutionErr := fmt.Errorf("cannot determine current network attachments for replacement pod %s/%s UID %s on network %s: %w",
			pod.Namespace, pod.Name, pod.UID, bsnc.GetNetworkName(), err)
		klog.Warningf("%v; preserving legacy OVN ports", resolutionErr)
		return nadKeys, false, resolutionErr
	}
	if !on {
		return nadKeys, true, nil
	}
	for nadKey := range networkMap {
		nadKeys.Insert(nadKey)
	}
	return nadKeys, true, nil
}

// getPodPortInfoFromNBDB discovers applied pod ports owned by this network. A
// replacement pod makes the namespaced name ambiguous, so iface-id-ver and the
// current pod's desired NADs are used to separate stale ports from current
// ports. The returned port map contains only ports that are safe to delete.
// A failure to query NBDB returns nil maps. Errors resolving individual rows
// are aggregated with the non-nil partial maps so callers can clean up safely
// classified ports while retaining a retry for the unresolved rows.
func (bsnc *BaseUserDefinedNetworkController) getPodPortInfoFromNBDB(
	pod, replacementPod *corev1.Pod,
	replacementNADKeys sets.Set[string],
	replacementNADKeysKnown bool,
) (map[string]*lpInfo, map[string]*nbdb.LogicalSwitchPort, error) {

	podNameSuffix := "_" + util.GetLogicalPortName(pod.Namespace, pod.Name)
	lsps, err := libovsdbops.FindLogicalSwitchPortsByNamespaceAndNetwork(bsnc.nbClient, pod.Namespace, bsnc.GetNetworkName())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find OVN pod ports for pod %s/%s on network %s: %w", pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
	}

	portInfoMap := map[string]*lpInfo{}
	protectedLSPs := map[string]*nbdb.LogicalSwitchPort{}
	var rowErrors []error
	for _, lsp := range lsps {
		if lsp.ExternalIDs["pod"] != "true" || !strings.HasSuffix(lsp.Name, podNameSuffix) {
			continue
		}
		nadKey := lsp.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			rowErrors = append(rowErrors, fmt.Errorf("OVN pod port %s for pod %s/%s on network %s has no NAD external ID",
				lsp.Name, pod.Namespace, pod.Name, bsnc.GetNetworkName()))
			continue
		}
		expectedPortName := bsnc.GetLogicalPortName(pod, nadKey)
		if lsp.Name != expectedPortName {
			rowErrors = append(rowErrors, fmt.Errorf("OVN pod port %s for pod %s/%s on network %s does not match expected port name %s for NAD %s",
				lsp.Name, pod.Namespace, pod.Name, bsnc.GetNetworkName(), expectedPortName, nadKey))
			continue
		}

		ifaceIDVer := lsp.Options["iface-id-ver"]
		if replacementPod != nil {
			currentPodUID := string(replacementPod.UID)
			currentOwned := currentPodUID != "" && ifaceIDVer == currentPodUID
			legacyDesired := ifaceIDVer == "" &&
				(!replacementNADKeysKnown || replacementNADKeys.Has(nadKey))
			if currentOwned || legacyDesired {
				if staleLSP, found := portInfoMap[nadKey]; found {
					delete(portInfoMap, nadKey)
					rowErrors = append(rowErrors, fmt.Errorf("found both replacement-owned OVN pod port %s and stale OVN pod port %s for pod %s/%s on NAD %s",
						lsp.Name, staleLSP.name, pod.Namespace, pod.Name, nadKey))
				}
				if protectedLSP, found := protectedLSPs[nadKey]; found {
					rowErrors = append(rowErrors, fmt.Errorf("found multiple replacement-owned OVN pod ports %s and %s for pod %s/%s on NAD %s",
						protectedLSP.Name, lsp.Name, pod.Namespace, pod.Name, nadKey))
					continue
				}
				protectedLSPs[nadKey] = lsp
				continue
			}
		}

		portUUID, switchName, err := bsnc.lookupPortUUIDAndSwitchName(lsp.Name)
		if err != nil {
			rowErrors = append(rowErrors, fmt.Errorf("failed to locate logical switch for OVN pod port %s: %w", lsp.Name, err))
			continue
		}
		if portUUID != lsp.UUID {
			rowErrors = append(rowErrors, fmt.Errorf("OVN pod port %s changed UUID while discovering applied state: found %s, expected %s", lsp.Name, portUUID, lsp.UUID))
			continue
		}

		mac, ips, err := bsnc.getPortAddresses(switchName, lsp)
		if err != nil {
			rowErrors = append(rowErrors, fmt.Errorf("failed to restore applied addresses from OVN pod port %s: %w", lsp.Name, err))
			continue
		}
		if bsnc.allocatesPodAnnotation() && bsnc.doesNetworkRequireIPAM() {
			_, rawIPs, err := libovsdbutil.ExtractPortAddresses(lsp)
			if err != nil {
				rowErrors = append(rowErrors, fmt.Errorf("failed to extract addresses from OVN pod port %s: %w", lsp.Name, err))
				continue
			}
			if len(ips) != len(rawIPs) {
				// Node or zone cleanup can remove the logical-switch allocator before
				// this stale LSP is discovered. Keep every durable address for the
				// collision check and delete recovery; IPAM lookup/release is keyed by
				// the IP itself, so a host mask is a safe fallback when the original
				// subnet is no longer available.
				maskedIPs := make(map[string]*net.IPNet, len(ips))
				for _, ipNet := range ips {
					if ipNet != nil {
						maskedIPs[ipNet.IP.String()] = ipNet
					}
				}
				restoredIPs := make([]*net.IPNet, 0, len(rawIPs))
				for _, rawIP := range rawIPs {
					if ipNet, ok := maskedIPs[rawIP.String()]; ok {
						restoredIPs = append(restoredIPs, ipNet)
						continue
					}
					restoredIPs = append(restoredIPs, &net.IPNet{
						IP:   append(net.IP(nil), rawIP...),
						Mask: util.GetIPFullMask(rawIP),
					})
				}
				klog.Warningf("Restored %d of %d subnet masks for stale OVN pod port %s; using host masks for missing allocator state",
					len(ips), len(rawIPs), lsp.Name)
				ips = restoredIPs
			}
		}
		if protectedLSP, found := protectedLSPs[nadKey]; found {
			rowErrors = append(rowErrors, fmt.Errorf("found both stale OVN pod port %s and replacement-owned OVN pod port %s for pod %s/%s on NAD %s",
				lsp.Name, protectedLSP.Name, pod.Namespace, pod.Name, nadKey))
			continue
		}
		if _, found := portInfoMap[nadKey]; found {
			rowErrors = append(rowErrors, fmt.Errorf("found multiple OVN pod ports for pod %s/%s on NAD %s", pod.Namespace, pod.Name, nadKey))
			continue
		}
		portInfoMap[nadKey] = &lpInfo{
			name:               lsp.Name,
			uuid:               lsp.UUID,
			appliedNetworkName: bsnc.GetNetworkName(),
			logicalSwitch:      switchName,
			ips:                ips,
			mac:                mac,
		}
	}

	return portInfoMap, protectedLSPs, utilerrors.Join(rowErrors...)
}

// releaseStaleCachedPodIPsForReplacement releases IPAM state retained only by
// an old delete snapshot while preserving every address used by the protected
// replacement LSP. It never mutates or deletes replacement-owned OVN/cache
// state. LSM ownership checks also preserve addresses acquired by any other pod.
func (bsnc *BaseUserDefinedNetworkController) releaseStaleCachedPodIPsForReplacement(
	deletedPod *corev1.Pod,
	nadKey string,
	stalePortInfo *lpInfo,
	protectedLSP *nbdb.LogicalSwitchPort,
) error {
	if !bsnc.allocatesPodAnnotation() || !bsnc.doesNetworkRequireIPAM() || stalePortInfo == nil || len(stalePortInfo.ips) == 0 {
		return nil
	}

	_, protectedIPs, err := libovsdbutil.ExtractPortAddresses(protectedLSP)
	if err != nil {
		return fmt.Errorf("failed to extract addresses from replacement-owned OVN pod port %s: %w", protectedLSP.Name, err)
	}
	if len(protectedIPs) == 0 {
		return fmt.Errorf("replacement-owned OVN pod port %s has no addresses while stale IPAM state remains for NAD %s",
			protectedLSP.Name, nadKey)
	}

	protectedIPStrings := sets.New[string]()
	for _, protectedIP := range protectedIPs {
		protectedIPStrings.Insert(protectedIP.String())
	}
	staleIPs := make([]*net.IPNet, 0, len(stalePortInfo.ips))
	for _, staleIP := range stalePortInfo.ips {
		if staleIP == nil || protectedIPStrings.Has(staleIP.IP.String()) {
			continue
		}
		staleIPs = append(staleIPs, staleIP)
	}
	if len(staleIPs) == 0 {
		return nil
	}

	staleIPInfo := cloneLPInfo(stalePortInfo)
	staleIPInfo.ips = staleIPs
	if err := bsnc.releasePodIPs(deletedPod, nadKey, staleIPInfo); err != nil {
		return fmt.Errorf("failed to release stale replacement IPs for NAD %s: %w", nadKey, err)
	}
	return nil
}

// GetInternalCacheEntryForUserDefinedNetwork returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (bsnc *BaseUserDefinedNetworkController) GetInternalCacheEntryForUserDefinedNetwork(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*corev1.Pod)
		return bsnc.GetPodState(pod)
	default:
		return nil
	}
}

func (bsnc *BaseUserDefinedNetworkController) SyncPods(pods []*corev1.Pod) (sets.Set[apitypes.UID], error) {
	if err := bsnc.syncPodsForUserDefinedNetwork(podsToInterfaces(pods)); err != nil {
		return nil, err
	}
	// Discover durable state once per network, rather than making every unrelated
	// bootstrap pod run the full-table fallback in getPodPortInfoFromNBDB.
	ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(bsnc.nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
		return lsp.ExternalIDs["pod"] == "true" && lsp.ExternalIDs[types.NetworkExternalID] == bsnc.GetNetworkName()
	})
	if err != nil {
		return nil, err
	}
	portNamespaces := sets.New[string]()
	for _, port := range ports {
		portNamespaces.Insert(port.ExternalIDs["namespace"])
	}
	emptyPods := sets.New[apitypes.UID]()
	for _, pod := range pods {
		// Namespace-level evidence is deliberately conservative: it also covers
		// legacy ports without a UID or NAD, without parsing logical port names.
		if portNamespaces.Has(pod.Namespace) || portNamespaces.Has("") ||
			len(bsnc.podPortsForReconcile(pod, nil)) != 0 {
			continue
		}
		annotations, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			continue
		}
		hasAttachment := false
		for nadKey := range annotations {
			if nadKey == types.DefaultNetworkName {
				continue
			}
			networkName := bsnc.networkManager.GetNetworkNameForNADKey(nadKey)
			if networkName == "" || networkName == bsnc.GetNetworkName() {
				// An annotation can reserve IPAM during sync even without an LSP.
				hasAttachment = true
				break
			}
		}
		if !hasAttachment && pod.UID != "" {
			emptyPods.Insert(pod.UID)
		}
	}
	return emptyPods, nil
}

func (bsnc *BaseUserDefinedNetworkController) GetPodState(pod *corev1.Pod) interface{} {
	// Avoid returning a typed-nil map as applied state.
	if portInfoMap := bsnc.getPortInfoForUserDefinedNetwork(pod); portInfoMap != nil {
		return portInfoMap
	}
	return nil
}

func (bsnc *BaseUserDefinedNetworkController) PodExpectedOnNetwork(pod *corev1.Pod) (bool, error) {
	if util.PodCompleted(pod) {
		return false, nil
	}
	on, _, err := bsnc.podNetworkSelectionForUserDefinedNetwork(pod)
	if err != nil {
		var configErr *podNetworkConfigError
		if errors.As(err, &configErr) {
			return true, nil
		}
		return false, err
	}
	return on, nil
}

// podPortsForReconcile includes side effects of failed attempts without
// advancing the shared controller's last successfully applied pod snapshot.
func (bsnc *BaseUserDefinedNetworkController) podPortsForReconcile(pod *corev1.Pod, applied map[string]*lpInfo) map[string]*lpInfo {
	ports := make(map[string]*lpInfo)
	merge := func(entries map[string]*lpInfo, requireUID bool) {
		for nadKey, info := range entries {
			if info == nil || info.appliedNetworkName != bsnc.GetNetworkName() {
				continue
			}
			// Expiring cache entries may be left by an already successful
			// cleanup. Only a retained applied/pending snapshot can prove that
			// their teardown still needs retrying.
			if requireUID && !info.expires.IsZero() {
				continue
			}
			if (requireUID || info.podUID != "") && info.podUID != pod.UID {
				continue
			}
			ports[nadKey] = cloneLPInfo(info)
		}
	}
	merge(applied, false)
	if pending, ok := bsnc.incompletePodPorts.Load(podReconcileKey(pod)); ok {
		merge(pending.(map[string]*lpInfo), false)
	}
	// The live cache is name-keyed and may belong to a replacement. Only
	// merge entries written for this UID, even when no replacement is visible
	// in the informer yet. Current cache entries supersede older snapshots.
	cached, _ := bsnc.logicalPortCache.getAll(pod)
	merge(cached, true)
	return ports
}

func (bsnc *BaseUserDefinedNetworkController) finishPodReconcile(pod *corev1.Pod) {
	bsnc.incompletePodPorts.Delete(podReconcileKey(pod))
}

func (bsnc *BaseUserDefinedNetworkController) rememberIncompletePodPorts(pod *corev1.Pod, applied map[string]*lpInfo) {
	ports := bsnc.podPortsForReconcile(pod, applied)
	if len(ports) != 0 {
		bsnc.incompletePodPorts.Store(podReconcileKey(pod), ports)
	}
}

func (bsnc *BaseUserDefinedNetworkController) ReconcilePod(oldPod, newPod *corev1.Pod, lastState interface{}) (state interface{}, reconcileErr error) {
	var appliedPortInfoMap map[string]*lpInfo
	if lastState != nil {
		var ok bool
		appliedPortInfoMap, ok = lastState.(map[string]*lpInfo)
		if !ok {
			return nil, fmt.Errorf("pod reconcile for network %s expected map[string]*lpInfo cache state but got %T", bsnc.GetNetworkName(), lastState)
		}
	}
	pod := newPod
	if pod == nil {
		pod = oldPod
	}
	if pod == nil {
		return nil, fmt.Errorf("pod reconcile for network %s is missing pod", bsnc.GetNetworkName())
	}
	appliedPortInfoMap = bsnc.podPortsForReconcile(pod, appliedPortInfoMap)
	defer func() {
		if reconcileErr != nil {
			bsnc.rememberIncompletePodPorts(pod, appliedPortInfoMap)
		}
	}()
	if newPod == nil {
		if oldPod == nil {
			return nil, fmt.Errorf("pod delete reconcile for network %s is missing pod", bsnc.GetNetworkName())
		}
		if err := bsnc.removePodForUserDefinedNetwork(oldPod, appliedPortInfoMap); err != nil {
			return nil, err
		}
		bsnc.finishPodReconcile(oldPod)
		return nil, nil
	}

	desiredPortNADs, err := bsnc.desiredPodPortNADs(newPod)
	if err != nil {
		var configErr *podNetworkConfigError
		if !errors.As(err, &configErr) {
			return nil, err
		}
		// Preserve applied state while the desired attachment is ambiguous.
		// The ensure path records this as a pod configuration event without
		// turning it into an endlessly retried controller error.
		eventReason := "ErrorAddingResource"
		if oldPod != nil {
			eventReason = "ErrorUpdatingResource"
		}
		if err := bsnc.ensurePodForUserDefinedNetwork(newPod, true, eventReason); err != nil {
			return nil, err
		}
		return appliedPortInfoMap, nil
	}

	appliedPod := oldPod
	if appliedPod == nil {
		appliedPod = newPod
	}
	if err := bsnc.removeStalePodPortsForUserDefinedNetwork(appliedPod, appliedPortInfoMap, desiredPortNADs); err != nil {
		return nil, err
	}

	// Until the first successful reconcile, keep retrying the add-style path
	// even if a failed attempt partially populated local caches.
	addPort := oldPod == nil || bsnc.shouldEnsurePodForUserDefinedNetwork(newPod)
	eventReason := "ErrorAddingResource"
	if oldPod != nil {
		eventReason = "ErrorUpdatingResource"
	}
	err = bsnc.ensurePodForUserDefinedNetwork(newPod, addPort, eventReason)
	if err == nil {
		err = bsnc.reconcileNetworkPolicyPortGroups(newPod)
	}
	if err != nil {
		if addPort {
			bsnc.logicalPortCache.markPodForReconcile(newPod, bsnc.GetNetworkName())
		}
		return nil, err
	}
	bsnc.finishPodReconcile(newPod)
	return bsnc.getDesiredPortInfoForUserDefinedNetwork(newPod, desiredPortNADs), nil
}

// desiredPodPortNADs returns the desired LSP keys in this zone. Network
// relevance is intentionally resolved first: when attachment state is
// malformed, applied ports must be preserved rather than mistaken for stale
// state. A relevant remote or no-host-subnet pod has an empty desired LSP set
// but still proceeds through the ensure path for policy and KubeVirt state.
func (bsnc *BaseUserDefinedNetworkController) desiredPodPortNADs(pod *corev1.Pod) (sets.Set[string], error) {
	desiredPortNADs := sets.New[string]()
	on, networkMap, err := bsnc.podNetworkSelectionForUserDefinedNetwork(pod)
	if err != nil || !on {
		return desiredPortNADs, err
	}
	if !bsnc.hasPodLogicalPort(pod) || !bsnc.podExpectedInLogicalCache(pod) {
		return desiredPortNADs, nil
	}
	for nadKey := range networkMap {
		desiredPortNADs.Insert(nadKey)
	}
	return desiredPortNADs, nil
}

func (bsnc *BaseUserDefinedNetworkController) getDesiredPortInfoForUserDefinedNetwork(
	pod *corev1.Pod, desiredPortNADs sets.Set[string],
) map[string]*lpInfo {
	portInfoMap := map[string]*lpInfo{}
	for nadKey := range desiredPortNADs {
		portInfo, err := bsnc.logicalPortCache.get(pod, nadKey)
		if err != nil || portInfo == nil || !portInfo.expires.IsZero() || portInfo.appliedNetworkName != bsnc.GetNetworkName() {
			continue
		}
		portInfoMap[nadKey] = portInfo
	}
	if len(portInfoMap) == 0 {
		return nil
	}
	return portInfoMap
}

// removeStalePodPortsForUserDefinedNetwork retires only applied NADs that are
// absent from the current desired LSP set. Whole-pod policy and replacement
// cleanup remains in removePodForUserDefinedNetwork; updates reconcile policy
// membership once the desired ports have been ensured.
func (bsnc *BaseUserDefinedNetworkController) removeStalePodPortsForUserDefinedNetwork(
	pod *corev1.Pod,
	appliedPortInfoMap map[string]*lpInfo,
	desiredPortNADs sets.Set[string],
) error {
	if len(appliedPortInfoMap) == 0 {
		return nil
	}

	podNetworks, annotationErr := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if annotationErr != nil {
		// Applied port state is sufficient for teardown. Keep malformed immutable
		// state from blocking convergence when a usable snapshot exists.
		klog.Errorf("Failed to unmarshal applied pod annotation for %s/%s on network %s while removing stale ports: %v",
			pod.Namespace, pod.Name, bsnc.GetNetworkName(), annotationErr)
	}

	var errs []error
	for _, nadKey := range sets.List(sets.KeySet(appliedPortInfoMap).Difference(desiredPortNADs)) {
		portInfo := appliedPortInfoMap[nadKey]
		if portInfo != nil && portInfo.appliedNetworkName != bsnc.GetNetworkName() {
			continue
		}

		if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
			var mac string
			var ips []string
			if podAnnotation, ok := podNetworks[nadKey]; ok {
				mac = podAnnotation.MAC
				ips = podAnnotation.IPs
			} else if portInfo != nil {
				if len(portInfo.mac) > 0 {
					mac = portInfo.mac.String()
				}
				ips = util.IPNetsToStringSlice(portInfo.ips)
			}
			if err := bsnc.enableSourceLSPFailedLiveMigration(pod, nadKey, mac, ips); err != nil {
				errs = append(errs, err)
				continue
			}
		}

		bsnc.logicalPortCache.remove(pod, nadKey)
		pInfo, err := bsnc.deletePodLogicalPort(pod, portInfo, nadKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete stale logical port of pod %s/%s for NAD key %s: %w",
				pod.Namespace, pod.Name, nadKey, err))
			continue
		}
		if !bsnc.allocatesPodAnnotation() {
			continue
		}
		if pInfo == nil || len(pInfo.ips) == 0 {
			continue
		}
		if err := bsnc.releasePodIPs(pod, nadKey, pInfo); err != nil {
			errs = append(errs, fmt.Errorf("failed to release stale pod IPs for %s/%s NAD key %s: %w",
				pod.Namespace, pod.Name, nadKey, err))
			continue
		}
	}
	return utilerrors.Join(errs...)
}

// AddUserDefinedNetworkResourceCommon adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. This function is called for User Defined Networks only.
func (bsnc *BaseUserDefinedNetworkController) AddUserDefinedNetworkResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
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
func (bsnc *BaseUserDefinedNetworkController) UpdateUserDefinedNetworkResourceCommon(objType reflect.Type, oldObj, newObj interface{}, _ bool) error {
	switch objType {
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
func (bsnc *BaseUserDefinedNetworkController) DeleteUserDefinedNetworkResourceCommon(objType reflect.Type, obj, _ interface{}) error {
	switch objType {
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

func (bsnc *BaseUserDefinedNetworkController) shouldEnsurePodForUserDefinedNetwork(pod *corev1.Pod) bool {
	if !util.PodScheduled(pod) || !bsnc.podExpectedInLogicalCache(pod) {
		return false
	}

	// Remote-zone pods only need forced transition cleanup.
	if !bsnc.hasPodLogicalPort(pod) {
		return false
	}

	nadKeys, err := bsnc.getPodNADKeys(pod)
	if err != nil {
		// Surface malformed attachment state through the ensure path.
		return true
	}
	for _, nadKey := range nadKeys {
		portInfo, err := bsnc.logicalPortCache.get(pod, nadKey)
		// Ignore cache entries written by other network controllers.
		if err != nil || !portInfo.expires.IsZero() || portInfo.needsReconcile || portInfo.appliedNetworkName != bsnc.GetNetworkName() {
			return true
		}
		if bsnc.dhcpPodNetworkOutOfSync(pod, nadKey, portInfo) {
			return true
		}
	}

	if len(nadKeys) > 0 || !bsnc.IsPrimaryNetwork() {
		return false
	}

	activeNetwork, err := bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
	if err != nil {
		return true
	}
	return activeNetwork != nil && activeNetwork.GetNetworkName() == bsnc.GetNetworkName()
}

// dhcpPodNetworkOutOfSync reports whether the current DHCP-owned pod
// annotation differs from the state recorded when the logical port was last
// programmed. This keeps DHCP updates level driven: retries make the same
// decision from current desired and applied state without relying on an old
// informer object.
func (bsnc *BaseUserDefinedNetworkController) dhcpPodNetworkOutOfSync(pod *corev1.Pod, nadKey string, portInfo *lpInfo) bool {
	if bsnc.IPAMType() != types.IPAMTypeDHCP {
		return false
	}
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		// A removed entry should not churn a live port. Any malformed entry is
		// reprocessed so the ensure path surfaces the configuration error.
		return !util.IsAnnotationNotSetError(err)
	}
	return !reflect.DeepEqual(podAnnotation.MAC, portInfo.mac) ||
		!reflect.DeepEqual(podAnnotation.IPs, portInfo.ips)
}

type podNetworkConfigError struct {
	err error
}

func (e *podNetworkConfigError) Error() string {
	return e.err.Error()
}

func (e *podNetworkConfigError) Unwrap() error {
	return e.err
}

func (bsnc *BaseUserDefinedNetworkController) podActiveNetworkForUserDefinedNetwork(pod *corev1.Pod) (bool, util.NetInfo, error) {
	if pod == nil || util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) {
		return false, nil, nil
	}

	var activeNetwork util.NetInfo
	if bsnc.IsPrimaryNetwork() {
		// check to see if the primary NAD is even applicable to our controller
		foundNamespaceNAD, err := bsnc.networkManager.GetPrimaryNADForNamespace(pod.Namespace)
		if err != nil {
			return false, nil, fmt.Errorf("failed to get primary network namespace NAD: %w", err)
		}
		if foundNamespaceNAD == types.DefaultNetworkName {
			return false, nil, nil
		}
		networkName := bsnc.networkManager.GetNetworkNameForNADKey(foundNamespaceNAD)
		if networkName != "" && networkName != bsnc.GetNetworkName() {
			return false, nil, nil
		}
		activeNetwork, err = bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
		if err != nil {
			return false, nil, fmt.Errorf("failed to find active network for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		if activeNetwork == nil {
			// no active network, pod doesn't belong to our controller
			return false, nil, nil
		}
	}

	return true, activeNetwork, nil
}

func (bsnc *BaseUserDefinedNetworkController) podNetworkSelectionForUserDefinedNetwork(pod *corev1.Pod) (bool, map[string]*nadapi.NetworkSelectionElement, error) {
	on, activeNetwork, err := bsnc.podActiveNetworkForUserDefinedNetwork(pod)
	if !on || err != nil {
		return on, nil, err
	}

	on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
		pod,
		bsnc.GetNetInfo(),
		activeNetwork,
		bsnc.networkManager.GetNetworkNameForNADKey,
		bsnc.networkManager.GetPrimaryNADForNamespace,
	)
	if err != nil {
		return false, nil, &podNetworkConfigError{err: err}
	}

	return on, networkMap, nil
}

// ensurePodForUserDefinedNetwork tries to set up the User Defined Network for
// a pod. Errors are retried by the caller.
func (bsnc *BaseUserDefinedNetworkController) ensurePodForUserDefinedNetwork(pod *corev1.Pod, addPort bool, eventReason string) error {
	if !util.PodScheduled(pod) || util.PodWantsHostNetwork(pod) {
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

	on, networkMap, err := bsnc.podNetworkSelectionForUserDefinedNetwork(pod)
	if err != nil {
		var configErr *podNetworkConfigError
		if errors.As(err, &configErr) {
			bsnc.recordPodErrorEventWithReason(pod, eventReason, configErr.err)
			// configuration error, no need to retry, do not return error
			klog.Errorf("Error getting network-attachment for pod %s/%s network %s: %v",
				pod.Namespace, pod.Name, bsnc.GetNetworkName(), configErr.err)
			return nil
		}
		return err
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
		_ = bsnc.logicalPortCache.add(pod, switchName, nadKey, bsnc.GetNetworkName(), lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)
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
	replacementPod, err := bsnc.getReplacementPod(pod)
	if err != nil {
		return err
	}
	// A successful snapshot may omit ports created by a later failed update.
	// Include partial state while excluding the replacement's live cache.
	portInfoMap = bsnc.podPortsForReconcile(pod, portInfoMap)
	if util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) {
		if err := bsnc.reconcileNetworkPolicyPortGroupsAfterDelete(pod, replacementPod); err != nil {
			return fmt.Errorf("failed to reconcile network policy membership while deleting ineligible pod %s/%s network %s: %w",
				pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		}
		return nil
	}

	podDesc := pod.Namespace + "/" + pod.Name
	var errs []error

	// Delete uses annotation state plus applied cache state.
	podNetworks, annotationErr := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if annotationErr != nil {
		klog.Errorf("Failed to unmarshal pod annotation for deleted pod %s on network %s: %v",
			podDesc, bsnc.GetNetworkName(), annotationErr)
	}

	if portInfoMap == nil {
		portInfoMap = map[string]*lpInfo{}
	}
	// Delete retries receive an immutable applied-state snapshot. Keep it
	// separate because replacement handling makes NBDB authoritative for rows
	// that still exist, while cache-only orphan state must still be expired and
	// run through IPAM cleanup.
	cachedPortInfoMap := make(map[string]*lpInfo, len(portInfoMap))
	for nadKey, portInfo := range portInfoMap {
		cachedPortInfoMap[nadKey] = portInfo
	}

	annotationNADKeys := sets.New[string]()
	for nadKey := range podNetworks {
		networkName := bsnc.networkManager.GetNetworkNameForNADKey(nadKey)
		if networkName == bsnc.GetNetworkName() {
			annotationNADKeys.Insert(nadKey)
		}
	}

	nadKeys := sets.New[string]()
	if replacementPod != nil {
		// The annotation and applied cache are keyed by namespaced name and may
		// both describe the replacement. Only NBDB state with stale ownership is
		// safe to use for teardown in this case.
		replacementNADKeys, replacementNADKeysKnown, replacementNADResolutionErr := bsnc.getCurrentPodLogicalPortNADs(replacementPod)
		discoveredPortInfo, protectedLSPs, discoveryErr := bsnc.getPodPortInfoFromNBDB(
			pod, replacementPod, replacementNADKeys, replacementNADKeysKnown)
		if discoveryErr != nil {
			if discoveredPortInfo == nil || protectedLSPs == nil {
				// The NBDB query itself failed, so there is no trustworthy partial
				// result from which cleanup can proceed.
				return discoveryErr
			}
			errs = append(errs, discoveryErr)
		}
		if replacementNADResolutionErr != nil {
			for _, protectedLSP := range protectedLSPs {
				if protectedLSP.Options["iface-id-ver"] == "" {
					// This legacy row cannot be assigned safely to either pod until
					// the replacement's desired NADs can be evaluated. Preserve it,
					// but retain the old pod's delete retry so it can be classified
					// and removed if it proves stale.
					errs = append(errs, replacementNADResolutionErr)
					break
				}
			}
		}
		portInfoMap = discoveredPortInfo
		for nadKey, cachedPortInfo := range cachedPortInfoMap {
			if cachedPortInfo == nil || cachedPortInfo.appliedNetworkName != bsnc.GetNetworkName() {
				continue
			}
			if protectedLSP, protected := protectedLSPs[nadKey]; protected {
				if err := bsnc.releaseStaleCachedPodIPsForReplacement(pod, nadKey, cachedPortInfo, protectedLSP); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if _, foundInNBDB := portInfoMap[nadKey]; foundInNBDB {
				// Durable NBDB state wins over a potentially stale cache snapshot.
				continue
			}
			if discoveryErr != nil {
				// A malformed row may be the durable state for this cache key.
				// Without a complete discovery result, absence from both partial
				// maps does not prove this cache-only state is stale.
				continue
			}
			// No LSP remains for this cached applied state. Process it through the
			// normal idempotent delete path so the cache is expired and IPAM state
			// is released only after the replacement collision check.
			portInfoMap[nadKey] = cachedPortInfo
		}
		for nadKey := range portInfoMap {
			nadKeys.Insert(nadKey)
		}
		for nadKey := range protectedLSPs {
			klog.Infof("Preserving OVN pod port for replacement pod %s/%s UID %s on NAD %s",
				replacementPod.Namespace, replacementPod.Name, replacementPod.UID, nadKey)
		}
	} else {
		// A valid annotation for this controller is authoritative and keeps the
		// common delete path free of an NBDB scan. A cache snapshot is still
		// merged below, but cannot prove it contains every applied NAD.
		nadKeys.Insert(annotationNADKeys.UnsortedList()...)
		for nadKey := range portInfoMap {
			nadKeys.Insert(nadKey)
		}
		// A cache snapshot cannot prove completeness. If this controller has no
		// usable NAD in the immutable annotation (missing, malformed, empty, or
		// detached), merge durable NBDB state even when part of the pod is cached.
		if annotationNADKeys.Len() == 0 {
			discoveredPortInfo, _, discoveryErr := bsnc.getPodPortInfoFromNBDB(pod, nil, nil, true)
			if discoveryErr != nil && discoveredPortInfo == nil {
				if annotationErr != nil {
					return utilerrors.Join(
						fmt.Errorf("failed to parse pod network annotation for deleted pod %s on network %s: %w", podDesc, bsnc.GetNetworkName(), annotationErr),
						discoveryErr,
					)
				}
				return discoveryErr
			}
			if discoveryErr != nil {
				if annotationErr != nil {
					errs = append(errs, fmt.Errorf("failed to parse pod network annotation for deleted pod %s on network %s: %w",
						podDesc, bsnc.GetNetworkName(), annotationErr))
				}
				errs = append(errs, discoveryErr)
			}
			for nadKey, portInfo := range discoveredPortInfo {
				if cachedPortInfo, found := portInfoMap[nadKey]; found && cachedPortInfo != nil && cachedPortInfo.uuid != portInfo.uuid {
					klog.Warningf("Cached OVN pod port UUID %s differs from durable NBDB UUID %s for pod %s NAD %s; using NBDB state",
						cachedPortInfo.uuid, portInfo.uuid, podDesc, nadKey)
				}
				portInfoMap[nadKey] = portInfo
				nadKeys.Insert(nadKey)
			}
		}
	}

	cleanupNetworkPolicyMembership := func() error {
		if err := bsnc.reconcileNetworkPolicyPortGroupsAfterDelete(pod, replacementPod); err != nil {
			return fmt.Errorf("failed to delete network policy membership for pod %s/%s network %s: %w", pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		}
		return nil
	}

	removePortInfoFromCache := func(nadKey string, deletedPortInfo *lpInfo) {
		if replacementPod == nil {
			bsnc.logicalPortCache.remove(pod, nadKey)
			return
		}
		// Do not remove name-keyed cache state that has already been replaced.
		// Pod events for one namespaced name share a retry lock, so comparing the
		// UUID before removal is sufficient to protect a converged replacement.
		if deletedPortInfo == nil {
			return
		}
		currentPortInfo, err := bsnc.logicalPortCache.get(replacementPod, nadKey)
		if err == nil && currentPortInfo.uuid == deletedPortInfo.uuid {
			bsnc.logicalPortCache.remove(replacementPod, nadKey)
		}
	}

	var alreadyProcessed bool
	for _, nadKey := range sets.List(nadKeys) {
		// pod has a network managed by this controller
		klog.Infof("Deleting pod: %s for network %s, NAD key: %s", podDesc, bsnc.GetNetworkName(), nadKey)

		// Handle remote pod cleanup only once. Concrete cached or NBDB-applied
		// state may outlive a zone transition; when present, continue through the
		// normal idempotent teardown instead of taking the remote-zone shortcut.
		if !bsnc.hasPodLogicalPort(pod) && !alreadyProcessed && len(portInfoMap) == 0 {
			// except for localnet networks, continue the delete flow in case a node just
			// became remote where we might still need to cleanup. On L3 networks
			// the node switch is removed so there is no need to do this.
			if bsnc.TopologyType() != types.LocalnetTopology {
				// Clear applied state so recreated pods do not look configured.
				for _, cachedNADKey := range sets.List(nadKeys) {
					removePortInfoFromCache(cachedNADKey, portInfoMap[cachedNADKey])
				}
				if err := cleanupNetworkPolicyMembership(); err != nil {
					errs = append(errs, err)
				}
				return utilerrors.Join(errs...)
			}
			alreadyProcessed = true
		}

		if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
			var mac string
			var ips []string
			if podAnnotation, ok := podNetworks[nadKey]; ok {
				mac = podAnnotation.MAC
				ips = podAnnotation.IPs
			} else if portInfo := portInfoMap[nadKey]; portInfo != nil {
				if len(portInfo.mac) > 0 {
					mac = portInfo.mac.String()
				}
				ips = util.IPNetsToStringSlice(portInfo.ips)
			}
			if err = bsnc.enableSourceLSPFailedLiveMigration(pod, nadKey, mac, ips); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		removePortInfoFromCache(nadKey, portInfoMap[nadKey])
		// LSM ownership checks protect replacement allocations when IPs are released.
		pInfo, err := bsnc.deletePodLogicalPort(pod, portInfoMap[nadKey], nadKey)
		if err != nil {
			errs = append(errs, err)
			continue
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
			errs = append(errs, err)
			continue
		}
	}
	// Always clean recorded membership, even when per-NAD teardown failed.
	if err := cleanupNetworkPolicyMembership(); err != nil {
		errs = append(errs, err)
	}
	return utilerrors.Join(errs...)
}

func (bsnc *BaseUserDefinedNetworkController) syncPodsForUserDefinedNetwork(pods []interface{}) error {
	orderedPods, err := logicalswitchmanager.SortPodsForIPAM(pods)
	if err != nil {
		return err
	}
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	expectedLogicalPorts, err := bsnc.restoreAppliedPodIPs(orderedPods)
	if err != nil {
		return err
	}
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

// restoreAppliedPodIPs reserves durable attachments before considering desired
// annotations, which can still contain addresses released by a live detach.
func (bsnc *BaseUserDefinedNetworkController) restoreAppliedPodIPs(pods []*corev1.Pod) (map[string]bool, error) {
	expectedPorts := make(map[string]bool)
	if !bsnc.allocatesPodAnnotation() || !bsnc.doesNetworkRequireIPAM() {
		return expectedPorts, nil
	}
	for _, pod := range pods {
		if !bsnc.isPodScheduledOnLocalNode(pod) || util.PodWantsHostNetwork(pod) {
			continue
		}
		annotations, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			continue
		}
		ports := map[string]*lpInfo{}
		for nadKey := range annotations {
			if bsnc.networkManager.GetNetworkNameForNADKey(nadKey) != bsnc.GetNetworkName() {
				continue
			}
			lsp, err := libovsdbops.GetLogicalSwitchPort(bsnc.nbClient, &nbdb.LogicalSwitchPort{Name: bsnc.GetLogicalPortName(pod, nadKey)})
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if uid := lsp.Options["iface-id-ver"]; uid != "" && uid != string(pod.UID) {
				continue
			}
			_, switchName, err := bsnc.lookupPortUUIDAndSwitchName(lsp.Name)
			if err != nil {
				return nil, err
			}
			mac, ips, err := bsnc.getPortAddresses(switchName, lsp)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				continue
			}
			if err := bsnc.lsManager.AllocateIPs(switchName, bsnc.podIPOwner(pod, nadKey), ips); err != nil &&
				(!ipallocator.IsErrAllocated(err) || errors.Is(err, ipallocator.ErrAllocatedByOther)) {
				return nil, fmt.Errorf("failed to restore IP ownership for pod port %s: %w", lsp.Name, err)
			}
			expectedPorts[lsp.Name] = true
			ports[nadKey] = &lpInfo{
				name: lsp.Name, uuid: lsp.UUID, podUID: pod.UID,
				appliedNetworkName: bsnc.GetNetworkName(), logicalSwitch: switchName,
				mac: mac, ips: ips, needsReconcile: true,
			}
		}
		// Detached ports must remain available for per-pod teardown, even when
		// the first live reconcile only selects another NAD on this network.
		bsnc.rememberIncompletePodPorts(pod, ports)
	}
	return expectedPorts, nil
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
