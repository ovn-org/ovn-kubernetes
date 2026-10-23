// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// dpuConnectionState is the DPU connection state of one NAD of a pod. The VF
// representor name is stored already resolved, so that the state can be rebuilt
// from OVS external_ids on restart.
type dpuConnectionState struct {
	vfRepName string
	sandboxId string
	// revalidate is set on a port rebuilt from OVS that the pod does not
	// already report ready, so it exists but may not be fully configured.
	revalidate bool
}

// podDPUState holds the per-NAD state of a single pod, along with the UID of the
// pod incarnation it was created for. Access is serialized by the podStates key
// lock.
type podDPUState struct {
	uid       k8stypes.UID
	nadStates map[string]*dpuConnectionState
}

func (c *Controller) addDPUPodForNAD(pod *corev1.Pod, state *dpuConnectionState,
	netInfo util.NetInfo, nadKey string, getter cni.PodInfoGetter, isRevalidation bool) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Adding %s on DPU", podDesc)
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, nil,
		string(pod.UID), "", nadKey, netInfo.GetNetworkName(), netInfo.MTU())
	if err != nil {
		return fmt.Errorf("failed to get pod interface information of %s: %w", podDesc, err)
	}
	err = c.addRepPort(pod, state, podInterfaceInfo, getter, isRevalidation)
	if err != nil {
		return fmt.Errorf("failed to add rep port for %s: %w", podDesc, err)
	}
	return nil
}

func dpuConnectionStateChanged(oldState *dpuConnectionState, newState *dpuConnectionState) bool {
	if oldState == nil && newState == nil {
		return false
	}
	if (oldState != nil && newState == nil) || (oldState == nil && newState != nil) {
		return true
	}
	return oldState.vfRepName != newState.vfRepName || oldState.sandboxId != newState.sandboxId
}

// getNetInfoForNADKey returns the NetInfo for a NAD key, or nil when the NAD it
// names no longer exists. An indexed key ("ns/nad/1", from a pod referencing the
// same NAD more than once) is stripped to the base key networkMgr tracks, and
// the default network is looked up with GetNetwork since it has no NAD resource.
// A key that cannot be parsed is a malformed annotation rather than a deleted
// NAD, so it is reported as an error.
func (c *Controller) getNetInfoForNADKey(nadKey string) (util.NetInfo, error) {
	if nadKey == types.DefaultNetworkName {
		return c.networkMgr.GetNetwork(nadKey), nil
	}
	nadName, _, err := util.GetNadFromIndexedNADKey(nadKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse NAD key %s: %w", nadKey, err)
	}
	return c.networkMgr.GetNetInfoForNADKey(nadName), nil
}

// handleAddOrUpdateDPUPod reconciles DPU state for a pod.
// The podStates lock for podKey must be held by the caller.
func (c *Controller) handleAddOrUpdateDPUPod(podKey string, pod *corev1.Pod, clientSet cni.PodInfoGetter) error {
	allDPUCDs, err := util.UnmarshalPodDPUConnDetailsAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	// Index the pod by the NADs it asks for, before the lookups below, so that
	// a NAD becoming known between the two still requeues the pod. The default
	// network has no NAD to wait for.
	nadNames := sets.New[string]()
	for nadKey := range allDPUCDs {
		if nadKey == types.DefaultNetworkName {
			continue
		}
		if nadName, _, err := util.GetNadFromIndexedNADKey(nadKey); err == nil {
			nadNames.Insert(nadName)
		}
	}
	c.nadPods.set(podKey, nadNames)

	// Desired NADs: those in the connection details annotation whose network
	// still exists. Each one keeps the NetInfo the add below configures from.
	type desiredNAD struct {
		state   *dpuConnectionState
		netInfo util.NetInfo
	}
	desiredNADs := make(map[string]desiredNAD)
	for nadKey, dpuCD := range allDPUCDs {
		netInfo, err := c.getNetInfoForNADKey(nadKey)
		if err != nil {
			return err
		}
		if netInfo == nil {
			klog.V(5).Infof("Network of NAD %s not found, skipping it for pod %s", nadKey, podKey)
			continue
		}
		vfRepName, err := util.GetDPUOps().GetPortRepresentor(dpuCD.PfId, dpuCD.VfId)
		if err != nil {
			return fmt.Errorf("failed to get VF representor for pod %s/%s NAD %s (PfId=%s, VfId=%s): %w",
				pod.Namespace, pod.Name, nadKey, dpuCD.PfId, dpuCD.VfId, err)
		}
		desiredNADs[nadKey] = desiredNAD{
			state: &dpuConnectionState{
				vfRepName: vfRepName,
				sandboxId: dpuCD.SandboxId,
			},
			netInfo: netInfo,
		}
	}
	if len(desiredNADs) == 0 {
		return c.handleDeleteDPUPod(podKey, pod)
	}

	ps, _ := c.podStates.LoadOrStore(podKey, &podDPUState{
		uid:       pod.UID,
		nadStates: make(map[string]*dpuConnectionState),
	})

	klog.V(5).Infof("Reconcile for Pod: %s (UID %s)", podKey, pod.UID)

	var errs []error

	// First clean up NADs that existed before but are either not desired or updated.
	var validPortsToDelete []string
	var nadsToUntrack []string
	revalidating := map[string]*dpuConnectionState{}
	nadsToKeep := make(map[string]*dpuConnectionState, len(ps.nadStates))
	for nadKey, state := range ps.nadStates {
		desired, exists := desiredNADs[nadKey]
		if !exists || dpuConnectionStateChanged(state, desired.state) {
			klog.Infof("Deleting stale VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
			valid, err := validateRepPort(c.ovsClient, state.vfRepName, state.sandboxId, nadKey)
			if err != nil {
				return fmt.Errorf("failed to validate representor %s for pod %s NAD %s: %w", state.vfRepName, podKey, nadKey, err)
			}
			if valid {
				validPortsToDelete = append(validPortsToDelete, state.vfRepName)
			}
			nadsToUntrack = append(nadsToUntrack, nadKey)
			continue
		}
		if state.revalidate {
			// ConfigureOVS adds the port before it sets the representor up, so
			// a port recovered from OVS may have been left half configured.
			// Untracking it runs ConfigureOVS again, which is idempotent, while
			// keeping the status the pod already carries. The port predates this
			// reconcile and may well be carrying traffic, so it is left in place
			// if that run fails instead of being deleted and built again.
			klog.Infof("Revalidating VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
			nadsToKeep[nadKey] = state
			nadsToUntrack = append(nadsToUntrack, nadKey)
			revalidating[nadKey] = state
			continue
		}
		if _, err := libovsdbops.GetOVSInterface(c.ovsClient, state.vfRepName); err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to look up representor %s for pod %s NAD %s: %w",
					state.vfRepName, podKey, nadKey, err)
			}
			// Untracking it is what makes the loop below add it back.
			klog.Infof("VF representor %s for pod %s NAD %s is gone from OVS, re-adding",
				state.vfRepName, podKey, nadKey)
			nadsToUntrack = append(nadsToUntrack, nadKey)
			continue
		}
		nadsToKeep[nadKey] = state
	}

	// Make the status match the NADs that stay configured, before the deletions
	// below: the host side waits on this status to plumb a new sandbox, and must
	// not be told a representor is ready once it has been deleted. NADs added
	// below get their status from addRepPort instead.
	if err := c.reconcileDPUConnStatus(pod, podKey, nadsToKeep); err != nil {
		if len(validPortsToDelete) > 0 {
			// Nothing is untracked yet, so the next retry finds these ports.
			return err
		}
		errs = append(errs, err)
	}

	if len(validPortsToDelete) > 0 {
		setRepPortInterfacesDown(validPortsToDelete)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", validPortsToDelete...); err != nil {
			return fmt.Errorf("failed to delete stale representor ports %v for pod %s: %w", validPortsToDelete, podKey, err)
		}
	}

	// Only untrack once the ports are gone, so that a failed deletion is picked
	// up by the next retry.
	for _, nadKey := range nadsToUntrack {
		delete(ps.nadStates, nadKey)
	}

	// Then setup NADs that are new or updated.
	for nadKey, desired := range desiredNADs {
		if _, ok := ps.nadStates[nadKey]; ok {
			continue
		}
		recovered, isRevalidation := revalidating[nadKey]
		if err := c.addDPUPodForNAD(pod, desired.state, desired.netInfo, nadKey, clientSet,
			isRevalidation); err != nil {
			klog.Errorf("Error adding pod %s NAD %s: %v", podKey, nadKey, err)
			if isRevalidation {
				// Keep the recovered state tracked, since the port was left in
				// place: the retry revalidates it again, and a teardown in
				// between still finds the port to delete.
				ps.nadStates[nadKey] = recovered
			}
			errs = append(errs, err)
			continue
		}
		ps.nadStates[nadKey] = desired.state
	}

	return utilerrors.Join(errs...)
}

// reconcileDPUConnStatus makes the pod's connection status annotation match the
// NADs tracked for it: every tracked NAD whose port is confirmed is reported
// ready, every other entry is removed.
func (c *Controller) reconcileDPUConnStatus(pod *corev1.Pod, podKey string, tracked map[string]*dpuConnectionState) error {
	currentStatus, err := util.UnmarshalPodDPUConnStatusAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	statusMap := map[string]*util.DPUConnectionStatus{}
	for nadKey, state := range tracked {
		if state.revalidate {
			// Nothing confirms this port yet, so leave the status as it is
			// rather than claim a representor that may still be down.
			// addRepPort reports it ready once the configuration completes.
			continue
		}
		if status, ok := currentStatus[nadKey]; !ok || status.Status != util.DPUConnectionStatusReady {
			statusMap[nadKey] = &util.DPUConnectionStatus{Status: util.DPUConnectionStatusReady}
		}
	}
	for nadKey := range currentStatus {
		if _, ok := tracked[nadKey]; !ok {
			statusMap[nadKey] = nil
		}
	}
	if len(statusMap) == 0 {
		return nil
	}

	klog.V(5).Infof("Updating DPU connection status of pod %s for %d NADs", podKey, len(statusMap))
	err = util.UpdatePodDPUConnStatusWithRetry(c.podLister, c.kube, pod, statusMap)
	if err != nil && !util.IsAnnotationAlreadySetError(err) {
		return fmt.Errorf("failed to update DPU connection status of pod %s: %w", podKey, err)
	}
	return nil
}

// handleDeleteDPUPod cleans up all DPU state tracked for podKey, batching the
// port deletions into a single OVSDB transaction. pod must be passed when it
// still exists, so that the connection status it carries can be cleared; a UID
// mismatch means the state belongs to an earlier pod of the same name.
// The state entry is dropped last, so that a retry still finds the ports.
// The podStates lock for podKey must be held by the caller.
func (c *Controller) handleDeleteDPUPod(podKey string, pod *corev1.Pod) error {
	ps, ok := c.podStates.Load(podKey)
	if !ok {
		// Nothing is tracked, but the pod can still carry status that this
		// controller never got to clean up.
		if pod != nil {
			return c.reconcileDPUConnStatus(pod, podKey, nil)
		}
		return nil
	}

	klog.V(5).Infof("Delete for Pod: %s (UID %s)", podKey, ps.uid)
	var portsToDelete []string
	for nadKey, state := range ps.nadStates {
		klog.Infof("Deleting VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
		valid, err := validateRepPort(c.ovsClient, state.vfRepName, state.sandboxId, nadKey)
		if err != nil {
			return fmt.Errorf("failed to validate representor %s for pod %s NAD %s: %w", state.vfRepName, podKey, nadKey, err)
		}
		if valid {
			portsToDelete = append(portsToDelete, state.vfRepName)
		}
	}

	// Clear the status before the ports go away, so that the host side is never
	// told a representor is ready after it has been deleted.
	if pod != nil && pod.UID == ps.uid {
		if err := c.reconcileDPUConnStatus(pod, podKey, nil); err != nil {
			return err
		}
	}

	if len(portsToDelete) > 0 {
		setRepPortInterfacesDown(portsToDelete)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", portsToDelete...); err != nil {
			return fmt.Errorf("failed to delete representor ports %v for pod %s: %w", portsToDelete, podKey, err)
		}
		klog.Infof("Deleted %d representor ports from br-int for pod %s", len(portsToDelete), podKey)
	}

	c.podStates.Delete(podKey)
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge. isRevalidation
// says the port was recovered after a restart rather than created here, and is
// kept rather than deleted again if the configuration fails.
func (c *Controller) addRepPort(pod *corev1.Pod, state *dpuConnectionState, ifInfo *cni.PodInterfaceInfo,
	getter cni.PodInfoGetter, isRevalidation bool) error {

	nadKey := ifInfo.NADKey
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)

	// set netdevName so OVS interface can be added with external_ids:vf-netdev-name, and is able to
	// be part of healthcheck.
	ifInfo.NetdevName = state.vfRepName
	deviceID, err := util.GetDPUOps().GetDeviceAddress(state.vfRepName)
	if err != nil {
		return fmt.Errorf("failed to get PCI address of VF rep %s for pod %s: %w", state.vfRepName, podDesc, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	klog.Infof("Adding VF representor %s for %s", state.vfRepName, podDesc)
	err = cni.ConfigureOVS(ctx, c.ovsClient, pod.Namespace, pod.Name, "", state.vfRepName, ifInfo, state.sandboxId,
		deviceID, false, getter)
	if err != nil {
		if !isRevalidation {
			// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
			_ = c.delRepPort(pod, state, nadKey)
		}
		return err
	}
	klog.Infof("Port %s added to bridge br-int", state.vfRepName)

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	statusMap := map[string]*util.DPUConnectionStatus{nadKey: {Status: util.DPUConnectionStatusReady, Reason: ""}}
	err = util.UpdatePodDPUConnStatusWithRetry(c.podLister, c.kube, pod, statusMap)
	if err != nil && !util.IsAnnotationAlreadySetError(err) {
		if !isRevalidation {
			_ = c.delRepPort(pod, state, nadKey)
		}
		return fmt.Errorf("failed to update connection status annotation for %s: %w", podDesc, err)
	}
	return nil
}

// delRepPort validates and deletes a single representor port from br-int.
// TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
func (c *Controller) delRepPort(pod *corev1.Pod, state *dpuConnectionState, nadKey string) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Deleting VF representor %s for %s", state.vfRepName, podDesc)
	valid, err := validateRepPort(c.ovsClient, state.vfRepName, state.sandboxId, nadKey)
	if err != nil {
		return err
	}
	if !valid {
		return nil
	}
	setRepPortInterfacesDown([]string{state.vfRepName})
	if err = libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", state.vfRepName); err != nil {
		return fmt.Errorf("failed to delete representor port %s from br-int for %s: %w", state.vfRepName, podDesc, err)
	}
	klog.Infof("Port %s deleted from bridge br-int", state.vfRepName)
	return nil
}

// validateRepPort checks that the OVS port matches the expected sandbox and NAD
// key. Returns (true, nil) if valid; (false, nil) if the port doesn't exist or
// belongs to a different pod/NAD, which no retry can fix and is logged as an
// error; (false, err) only on transient OVS lookup failures.
func validateRepPort(ovsClient libovsdbclient.Client, vfRepName, sandboxId, nadKey string) (bool, error) {
	if ovsClient == nil {
		return false, fmt.Errorf("cannot validate VF representor %s without an OVS client", vfRepName)
	}
	iface, err := libovsdbops.GetOVSInterface(ovsClient, vfRepName)
	ifExists := err == nil
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return false, err
	}
	if !ifExists {
		klog.Infof("VF representor %s is not an OVS interface, nothing to do", vfRepName)
		return false, nil
	}
	sandbox := iface.ExternalIDs["sandbox"]
	expectedNADKey := iface.ExternalIDs[types.NADExternalID]
	if expectedNADKey == "" {
		expectedNADKey = types.DefaultNetworkName
	}
	if sandbox != sandboxId {
		klog.Errorf("OVS port %s belongs to sandbox %s, not the expected %s; leaving it in place",
			vfRepName, sandbox, sandboxId)
		return false, nil
	}
	if expectedNADKey != nadKey {
		klog.Errorf("OVS port %s belongs to NAD %s, not the expected %s; leaving it in place",
			vfRepName, expectedNADKey, nadKey)
		return false, nil
	}
	return true, nil
}

// setRepPortInterfacesDown sets the link down for each representor interface.
// Failures are logged as warnings but do not block deletion.
func setRepPortInterfacesDown(interfaces []string) {
	for _, iface := range interfaces {
		link, err := util.GetNetLinkOps().LinkByName(iface)
		if err != nil {
			klog.Warningf("Failed to get link device for representor port %s: %v", iface, err)
			continue
		}
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to set link down for representor port %s: %v", iface, err)
		}
	}
}

// podKeyFromIfaceID extracts the namespace/name pod key from an OVS iface-id.
// For the default network, iface-id is "namespace_podName".
// For UDN, iface-id is "<udnPrefix>namespace_podName" where udnPrefix is derived from the NAD key.
func podKeyFromIfaceID(ifaceID, nadKey string) string {
	namespacePod := ifaceID
	if nadKey != types.DefaultNetworkName {
		prefix := util.GetUserDefinedNetworkPrefix(nadKey)
		namespacePod = strings.TrimPrefix(ifaceID, prefix)
	}
	parts := strings.SplitN(namespacePod, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
