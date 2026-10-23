// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/applyconfiguration/uplink/v1alpha1"
	uplinkclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned"
	uplinkinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	uplinkGatewayFieldManager      = "ovnkube-node-uplink-gateway-controller"
	uplinkHostGatewayFieldManager  = "ovnkube-node-uplink-host-gateway-controller"
	maxGatewayConditionExamples    = 3
	maxGatewayConditionErrorLength = 160
	uplinkGatewayAPITimeout        = 30 * time.Second
	uplinkGatewayStatusBatchDelay  = 100 * time.Millisecond
	uplinkGatewayStatusWorkers     = 2
)

type uplinkGatewayNetworkPhase string

const (
	uplinkGatewayNetworkReady  uplinkGatewayNetworkPhase = "ready"
	uplinkGatewayNetworkFailed uplinkGatewayNetworkPhase = "failed"
)

type uplinkGatewayNetworkState struct {
	phase   uplinkGatewayNetworkPhase
	reason  string
	message string
}

// uplinkGatewayState contains only the state needed to aggregate readiness.
// Dataplane lifecycle and serialization are owned by each UDN gateway.
type uplinkGatewayState struct {
	// conditionMutex serializes the readiness read, merge, and publication
	// sequence for this Uplink. lastAppliedCondition suppresses duplicate
	// writes while the informer cache catches up with a successful Apply.
	conditionMutex       sync.Mutex
	networks             map[string]*uplinkGatewayNetworkState
	lastCondition        *metav1.Condition
	lastAppliedCondition *metav1.Condition
	forcePublish         bool
	// stateUID binds this aggregate epoch to one UplinkState object. An empty
	// value means the object is currently absent or has not yet been observed.
	stateUID k8stypes.UID
	// reportedFingerprint identifies the UplinkState input epoch for the
	// network results in networks. Reports from a different UID or discovery
	// configuration start a new aggregate epoch without publishing an
	// intermediate Pending state.
	reportedFingerprint *uplinkGatewayFingerprint
	// publicationInvalid prevents queued work from restoring readiness while
	// its UplinkState object is absent. Only observing an add clears it.
	publicationInvalid bool
}

// UplinkStateGatewayStatusController aggregates results reported by active
// CUDNs using an Uplink on the local node. A shared level-driven controller
// observes UplinkState lifecycle, so discovery does not need gateway-specific
// callbacks. It deliberately does not own CUDN lifecycle callbacks or
// serialize dataplane operations: each UDN watches its UplinkState and
// serializes its own reconciliation.
type UplinkStateGatewayStatusController struct {
	nodeName          string
	uplinkClient      uplinkclientset.Interface
	uplinkStateLister uplinklisters.UplinkStateLister
	// uplinkStateController tracks object lifecycles without performing slow
	// API writes. statusReconciler coalesces and retries those writes
	// independently, so deletion can invalidate an in-flight publication.
	uplinkStateController controllerutil.Controller
	statusReconciler      controllerutil.Reconciler
	// The DPU owns GatewayReady in split-DPU deployments. The DPU-host reports
	// its part of gateway programming through HostGatewayReady.
	conditionType string
	fieldManager  string

	// mutex protects short-lived aggregate readiness bookkeeping. Slow
	// dataplane operations are never performed while it is held.
	mutex               sync.Mutex
	uplinks             map[string]*uplinkGatewayState
	uplinkByNetworkName map[string]string
	// UplinkState names are opaque and cannot be parsed back into Uplink
	// names. This index lets deletion reconciliation retire the right
	// aggregate after the object has disappeared from the informer cache.
	uplinkByStateName map[string]string
}

// NewUplinkStateGatewayStatusController creates the node-local readiness
// aggregator.
func NewUplinkStateGatewayStatusController(
	nodeName string,
	uplinkClient uplinkclientset.Interface,
	uplinkStateInformer uplinkinformers.UplinkStateInformer,
) *UplinkStateGatewayStatusController {
	conditionType := uplinkv1alpha1.UplinkStateConditionGatewayReady
	fieldManager := uplinkGatewayFieldManager
	if config.IsModeDPUHost() {
		conditionType = uplinkv1alpha1.UplinkStateConditionHostGatewayReady
		fieldManager = uplinkHostGatewayFieldManager
	}
	sharedInformer := uplinkStateInformer.Informer()
	c := &UplinkStateGatewayStatusController{
		nodeName:            nodeName,
		uplinkClient:        uplinkClient,
		uplinkStateLister:   uplinkStateInformer.Lister(),
		conditionType:       conditionType,
		fieldManager:        fieldManager,
		uplinks:             map[string]*uplinkGatewayState{},
		uplinkByNetworkName: map[string]string{},
		uplinkByStateName:   map[string]string{},
	}
	c.uplinkStateController = controllerutil.NewController(
		"uplink-state-gateway-status-controller",
		&controllerutil.ControllerConfig[uplinkv1alpha1.UplinkState]{
			MaxAttempts: controllerutil.InfiniteAttempts,
			Informer:    sharedInformer,
			Lister:      c.uplinkStateLister.List,
			Reconcile: func(_ context.Context, stateName string) error {
				return c.reconcileUplinkState(stateName)
			},
			ObjNeedsUpdate: c.uplinkStateNeedsUpdate,
			Threadiness:    1,
		},
	)
	c.statusReconciler = controllerutil.NewReconciler(
		"uplink-state-gateway-status-publisher",
		&controllerutil.ReconcilerConfig{
			MaxAttempts: controllerutil.InfiniteAttempts,
			Reconcile: func(_ context.Context, stateName string) error {
				return c.reconcileGatewayCondition(stateName)
			},
			Threadiness: uplinkGatewayStatusWorkers,
		},
	)
	return c
}

// Start runs the level-driven UplinkState status controller.
func (c *UplinkStateGatewayStatusController) Start() error {
	return controllerutil.Start(c.uplinkStateController, c.statusReconciler)
}

// Stop prevents new publications and waits for an in-flight API request.
func (c *UplinkStateGatewayStatusController) Stop() {
	controllerutil.Stop(c.uplinkStateController, c.statusReconciler)
}

func (c *UplinkStateGatewayStatusController) uplinkStateNeedsUpdate(
	oldState, newState *uplinkv1alpha1.UplinkState,
) bool {
	if newState == nil {
		return false
	}
	newUplinkName, newNodeName := uplinkutil.StateIdentity(newState)
	if oldState == nil {
		return newUplinkName != "" && newNodeName == c.nodeName
	}
	oldUplinkName, oldNodeName := uplinkutil.StateIdentity(oldState)
	if oldNodeName != c.nodeName && newNodeName != c.nodeName {
		return false
	}
	if oldState.UID != newState.UID || oldUplinkName != newUplinkName ||
		oldNodeName != newNodeName {
		return true
	}
	return !uplinkGatewayInputsEqual(oldState, newState) ||
		meta.FindStatusCondition(newState.Status.Conditions, c.conditionType) == nil
}

// reconcileUplinkState observes the latest object lifecycle without waiting
// for status API calls. CUDN reports feed a separate level-driven reconciler
// so deletion can invalidate an aggregate while a publication is in flight.
func (c *UplinkStateGatewayStatusController) reconcileUplinkState(stateName string) error {
	state, err := c.uplinkStateLister.Get(stateName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.retireUplinkState(stateName)
			return nil
		}
		return fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}

	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if nodeName != c.nodeName {
		c.retireUplinkState(stateName)
		return nil
	}
	if expectedStateName := uplinkutil.StateName(uplinkName, nodeName); expectedStateName != stateName {
		c.retireUplinkState(stateName)
		return fmt.Errorf(
			"UplinkState %s reports uplinkName %q and nodeName %q, whose expected object name is %s",
			stateName, uplinkName, nodeName, expectedStateName)
	}
	c.observeUplinkState(state)
	return nil
}

// reconcileGatewayCondition publishes the aggregate derived from the latest
// informer state. API failures are retried independently of CUDN dataplane
// reconciliation, and multiple CUDN reports for one UplinkState are coalesced.
func (c *UplinkStateGatewayStatusController) reconcileGatewayCondition(stateName string) error {
	state, err := c.uplinkStateLister.Get(stateName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.retireUplinkState(stateName)
			return nil
		}
		return fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if nodeName != c.nodeName || uplinkutil.StateName(uplinkName, nodeName) != stateName {
		return nil
	}
	if err := c.publishGatewayCondition(uplinkName); err != nil {
		if isUplinkStateNotFound(err) {
			c.retireUplinkState(stateName)
			return nil
		}
		return err
	}
	return nil
}

// observeUplinkState starts publication for the current object lifecycle. A
// different UID is a recreation whose old in-flight CUDN completions must not
// update the new object's readiness.
func (c *UplinkStateGatewayStatusController) observeUplinkState(
	state *uplinkv1alpha1.UplinkState,
) {
	uplinkName, nodeName := uplinkutil.StateIdentity(state)
	if uplinkName == "" || nodeName != c.nodeName {
		return
	}

	c.mutex.Lock()
	uplinkState := c.ensureUplinkStateLocked(uplinkName)
	if uplinkState.stateUID != "" && uplinkState.stateUID != state.UID {
		uplinkState = newUplinkGatewayState(false)
		c.uplinks[uplinkName] = uplinkState
	}
	uplinkState.stateUID = state.UID
	uplinkState.publicationInvalid = false
	c.uplinkByStateName[state.Name] = uplinkName
	c.mutex.Unlock()

	if meta.FindStatusCondition(state.Status.Conditions, c.conditionType) == nil {
		uplinkState.conditionMutex.Lock()
		uplinkState.forcePublish = true
		uplinkState.conditionMutex.Unlock()
		c.enqueueGatewayCondition(uplinkName)
	}
}

// retireUplinkState ends the lifecycle represented by an absent or no-longer
// local object key. Replacing the state pointer prevents queued publication
// for the old object from reaching a later object with the same name.
func (c *UplinkStateGatewayStatusController) retireUplinkState(
	stateName string,
) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	uplinkName := c.uplinkByStateName[stateName]
	delete(c.uplinkByStateName, stateName)
	if uplinkName == "" {
		return
	}
	retired := c.uplinks[uplinkName]
	if retired == nil {
		return
	}
	replacement := newUplinkGatewayState(true)
	replacement.stateUID = retired.stateUID
	c.uplinks[uplinkName] = replacement
}

func newUplinkGatewayState(publicationInvalid bool) *uplinkGatewayState {
	return &uplinkGatewayState{
		networks:           map[string]*uplinkGatewayNetworkState{},
		publicationInvalid: publicationInvalid,
	}
}

// SyncNetworks removes results for networks that are no longer active and
// seeds UplinkState publication before individual network controllers report
// their results. Missing UplinkStates are tolerated because discovery may still
// be completing during node startup.
func (c *UplinkStateGatewayStatusController) SyncNetworks(networks ...util.NetInfo) error {
	desired := make(map[string]string)
	for _, network := range networks {
		if network.Uplink() != "" {
			desired[network.GetNetworkName()] = network.Uplink()
		}
	}

	states, err := c.uplinkStateLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UplinkStates during gateway sync: %w", err)
	}

	affectedUplinks := map[string]struct{}{}
	c.mutex.Lock()
	for _, state := range states {
		uplinkName, nodeName := uplinkutil.StateIdentity(state)
		if uplinkName == "" || nodeName != c.nodeName {
			continue
		}
		uplinkState := c.ensureUplinkStateLocked(uplinkName)
		if uplinkState.stateUID != "" && uplinkState.stateUID != state.UID {
			uplinkState = newUplinkGatewayState(false)
			c.uplinks[uplinkName] = uplinkState
		}
		uplinkState.stateUID = state.UID
		uplinkState.publicationInvalid = false
		c.uplinkByStateName[state.Name] = uplinkName
		affectedUplinks[uplinkName] = struct{}{}
	}
	for networkName, uplinkName := range c.uplinkByNetworkName {
		if desiredUplink, found := desired[networkName]; found && desiredUplink == uplinkName {
			continue
		}
		if state := c.uplinks[uplinkName]; state != nil {
			delete(state.networks, networkName)
		}
		delete(c.uplinkByNetworkName, networkName)
		affectedUplinks[uplinkName] = struct{}{}
	}
	for _, uplinkName := range desired {
		affectedUplinks[uplinkName] = struct{}{}
	}
	c.mutex.Unlock()

	for uplinkName := range affectedUplinks {
		c.enqueueGatewayCondition(uplinkName)
	}
	return nil
}

func (c *UplinkStateGatewayStatusController) ensureNetworkStatusLocked(
	networkName, uplinkName string,
) (*uplinkGatewayState, *uplinkGatewayNetworkState, []string) {
	previousUplink := c.uplinkByNetworkName[networkName]
	if previousUplink != "" && previousUplink != uplinkName {
		if previousState := c.uplinks[previousUplink]; previousState != nil {
			delete(previousState.networks, networkName)
		}
	}

	uplinkState := c.ensureUplinkStateLocked(uplinkName)
	networkState := uplinkState.networks[networkName]
	if networkState == nil {
		networkState = &uplinkGatewayNetworkState{}
		uplinkState.networks[networkName] = networkState
	}
	c.uplinkByNetworkName[networkName] = uplinkName

	affectedUplinks := []string{uplinkName}
	if previousUplink != "" && previousUplink != uplinkName {
		affectedUplinks = append(affectedUplinks, previousUplink)
	}
	return uplinkState, networkState, affectedUplinks
}

func (c *UplinkStateGatewayStatusController) ensureUplinkStateLocked(uplinkName string) *uplinkGatewayState {
	uplinkState := c.uplinks[uplinkName]
	if uplinkState == nil {
		uplinkState = newUplinkGatewayState(false)
		c.uplinks[uplinkName] = uplinkState
	}
	return uplinkState
}

// ReportNetworkResult records one UDN-owned dataplane result and asynchronously
// publishes aggregate readiness. observedUID and observedFingerprint must
// identify the UplinkState inputs used by the operation. A report is discarded
// if the current object has a different UID or gateway configuration
// fingerprint.
func (c *UplinkStateGatewayStatusController) ReportNetworkResult(
	network util.NetInfo,
	observedUID k8stypes.UID,
	observedFingerprint uplinkGatewayFingerprint,
	reconcileErr error,
) {
	if network.Uplink() == "" || observedUID == "" {
		return
	}
	stateName := uplinkutil.StateName(network.Uplink(), c.nodeName)
	currentState, err := uplinkutil.GetState(
		c.uplinkStateLister, network.Uplink(), c.nodeName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf(
				"failed to validate gateway result for UplinkState %s: %w",
				stateName, err))
		}
		return
	}
	currentFingerprint := uplinkGatewayFingerprintFromState(currentState)
	if observedUID != currentState.UID ||
		observedFingerprint != currentFingerprint {
		return
	}

	c.mutex.Lock()
	uplinkName := network.Uplink()
	uplinkState := c.ensureUplinkStateLocked(uplinkName)
	if uplinkState.publicationInvalid && uplinkState.stateUID == currentState.UID {
		c.mutex.Unlock()
		return
	}
	if uplinkState.stateUID != currentState.UID {
		uplinkState = newUplinkGatewayState(false)
		c.uplinks[uplinkName] = uplinkState
		uplinkState.stateUID = currentState.UID
	}
	uplinkState.publicationInvalid = false
	c.uplinkByStateName[stateName] = uplinkName
	if uplinkState.reportedFingerprint == nil ||
		*uplinkState.reportedFingerprint != currentFingerprint {
		uplinkState.networks = map[string]*uplinkGatewayNetworkState{}
		uplinkState.reportedFingerprint = &observedFingerprint
	}
	_, networkState, affectedUplinks := c.ensureNetworkStatusLocked(
		network.GetNetworkName(), uplinkName)
	if reconcileErr == nil {
		networkState.phase = uplinkGatewayNetworkReady
		networkState.reason = uplinkv1alpha1.UplinkStateReasonGatewayConfigured
		networkState.message = ""
	} else {
		networkState.phase = uplinkGatewayNetworkFailed
		networkState.reason = uplinkGatewayFailureReason(reconcileErr)
		networkState.message = reconcileErr.Error()
	}
	c.mutex.Unlock()
	c.enqueueGatewayConditions(affectedUplinks)
}

// ReportNetworkDeleted removes a CUDN from aggregate readiness only
// after its UDN-owned dataplane cleanup succeeds.
func (c *UplinkStateGatewayStatusController) ReportNetworkDeleted(
	network util.NetInfo,
	observedUID k8stypes.UID,
	observedFingerprint uplinkGatewayFingerprint,
	reconcileErr error,
) {
	if network.Uplink() == "" {
		return
	}
	if reconcileErr != nil {
		c.ReportNetworkResult(
			network, observedUID, observedFingerprint, reconcileErr)
		return
	}

	c.mutex.Lock()
	uplinkName := network.Uplink()
	networkName := network.GetNetworkName()
	if c.uplinkByNetworkName[networkName] == uplinkName {
		if uplinkState := c.uplinks[uplinkName]; uplinkState != nil {
			delete(uplinkState.networks, networkName)
		}
		delete(c.uplinkByNetworkName, networkName)
	}
	c.mutex.Unlock()
	c.enqueueGatewayCondition(uplinkName)
}

func (c *UplinkStateGatewayStatusController) enqueueGatewayConditions(uplinkNames []string) {
	for _, uplinkName := range uplinkNames {
		c.enqueueGatewayCondition(uplinkName)
	}
}

func (c *UplinkStateGatewayStatusController) enqueueGatewayCondition(uplinkName string) {
	c.statusReconciler.ReconcileAfter(
		uplinkutil.StateName(uplinkName, c.nodeName),
		uplinkGatewayStatusBatchDelay,
	)
}

// publishGatewayCondition writes aggregate readiness for the CUDNs that have
// reported a result for this Uplink's current input configuration.
func (c *UplinkStateGatewayStatusController) publishGatewayCondition(uplinkName string) error {
	c.mutex.Lock()
	uplinkState := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if uplinkState == nil {
		return nil
	}

	// Serialize the read, merge, and apply sequence and access to
	// lastCondition for this Uplink.
	uplinkState.conditionMutex.Lock()
	defer uplinkState.conditionMutex.Unlock()

	// An UplinkState delete or recreation may have retired the entry after this
	// publisher read it but before it acquired conditionMutex. Do not publish
	// readiness from the superseded object lifecycle.
	c.mutex.Lock()
	current := c.uplinks[uplinkName]
	c.mutex.Unlock()
	if current != uplinkState {
		return nil
	}

	desiredCondition, expectedUID, reportedFingerprint, found := c.gatewayCondition(
		uplinkName, uplinkState)
	if !found {
		return nil
	}
	stateName := uplinkutil.StateName(uplinkName, c.nodeName)
	state, err := uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			existing := []metav1.Condition(nil)
			if uplinkState.lastCondition != nil {
				existing = []metav1.Condition{*uplinkState.lastCondition}
			}
			condition, _ := util.MergeStatusCondition(existing, desiredCondition)
			uplinkState.lastCondition = condition.DeepCopy()
		}
		return fmt.Errorf("failed to get UplinkState %s from cache: %w", stateName, err)
	}
	if !c.reportMatchesCurrentState(
		uplinkName, uplinkState, expectedUID, reportedFingerprint, state) {
		return nil
	}

	if !uplinkState.forcePublish &&
		(conditionsEqual(uplinkState.lastAppliedCondition, desiredCondition) ||
			conditionsEqual(meta.FindStatusCondition(
				state.Status.Conditions, c.conditionType), desiredCondition)) {
		return nil
	}

	existingConditions := state.Status.Conditions
	if uplinkState.lastCondition != nil {
		existingConditions = []metav1.Condition{*uplinkState.lastCondition}
	}
	condition, _ := util.MergeStatusCondition(existingConditions, desiredCondition)
	// Recheck immediately before applying because an informer update can make
	// the report stale while it waits behind another status publication.
	state, err = uplinkutil.GetState(c.uplinkStateLister, uplinkName, c.nodeName)
	if err != nil {
		return fmt.Errorf("failed to recheck UplinkState %s before status apply: %w", stateName, err)
	}
	if !c.reportMatchesCurrentState(
		uplinkName, uplinkState, expectedUID, reportedFingerprint, state) {
		return nil
	}
	// Keep the merge result as the retry base while the informer cache catches
	// up, preserving LastTransitionTime across duplicate publications.
	uplinkState.lastCondition = condition.DeepCopy()
	ctx, cancel := context.WithTimeout(context.Background(), uplinkGatewayAPITimeout)
	defer cancel()
	applyState := uplinkapply.UplinkState(stateName).WithStatus(
		uplinkapply.UplinkStateStatus().WithConditions(util.ConditionToApply(condition)),
	)
	if state.UID != "" {
		applyState = applyState.WithUID(state.UID)
	}
	_, err = c.uplinkClient.K8sV1alpha1().UplinkStates().Apply(
		ctx,
		applyState,
		metav1.ApplyOptions{FieldManager: c.fieldManager, Force: true},
	)
	if err != nil {
		return fmt.Errorf("failed to apply UplinkState %s status: %w", stateName, err)
	}
	uplinkState.lastAppliedCondition = condition.DeepCopy()
	uplinkState.forcePublish = false
	return nil
}

func (c *UplinkStateGatewayStatusController) reportMatchesCurrentState(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
	expectedUID k8stypes.UID,
	reportedFingerprint *uplinkGatewayFingerprint,
	currentState *uplinkv1alpha1.UplinkState,
) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	currentFingerprint := uplinkGatewayFingerprintFromState(currentState)
	return c.uplinks[uplinkName] == expectedUplinkState &&
		expectedUplinkState.stateUID == expectedUID &&
		currentState.UID == expectedUID &&
		(reportedFingerprint == nil ||
			*reportedFingerprint == currentFingerprint)
}

func (c *UplinkStateGatewayStatusController) gatewayCondition(
	uplinkName string,
	expectedUplinkState *uplinkGatewayState,
) (metav1.Condition, k8stypes.UID, *uplinkGatewayFingerprint, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	uplinkState := c.uplinks[uplinkName]
	if uplinkState != expectedUplinkState {
		return metav1.Condition{}, "", nil, false
	}
	if uplinkState.publicationInvalid {
		return metav1.Condition{}, "", nil, false
	}
	if len(uplinkState.networks) == 0 {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonNoActiveCUDNs,
			Message: "No active CUDN gateways have reported programming",
		}, uplinkState.stateUID, nil, true
	}

	networkNames := make([]string, 0, len(uplinkState.networks))
	for networkName := range uplinkState.networks {
		networkNames = append(networkNames, networkName)
	}
	sort.Strings(networkNames)

	failureReasons := map[string]struct{}{}
	examples := make([]string, 0, maxGatewayConditionExamples)
	failed := 0
	for _, networkName := range networkNames {
		networkState := uplinkState.networks[networkName]
		if networkState.phase == uplinkGatewayNetworkReady {
			continue
		}
		failed++
		failureReasons[networkState.reason] = struct{}{}
		if len(examples) < maxGatewayConditionExamples {
			example := fmt.Sprintf("%s=%s", networkName, networkState.reason)
			if networkState.message != "" {
				example += ": " + truncateGatewayConditionError(networkState.message)
			}
			examples = append(examples, example)
		}
	}
	if failed == 0 {
		return metav1.Condition{
			Type:    c.conditionType,
			Status:  metav1.ConditionTrue,
			Reason:  uplinkv1alpha1.UplinkStateReasonGatewayConfigured,
			Message: fmt.Sprintf("Uplink gateway programming succeeded for %d reported active CUDN(s)", len(networkNames)),
		}, uplinkState.stateUID, uplinkState.reportedFingerprint, true
	}

	return metav1.Condition{
		Type:   c.conditionType,
		Status: metav1.ConditionFalse,
		Reason: aggregateGatewayFailureReason(failureReasons),
		Message: fmt.Sprintf(
			"%d of %d reported active CUDN(s) have failed Uplink gateway configuration; examples: %s",
			failed, len(networkNames), strings.Join(examples, ", ")),
	}, uplinkState.stateUID, uplinkState.reportedFingerprint, true
}

func aggregateGatewayFailureReason(reasons map[string]struct{}) string {
	for _, reason := range []string{
		uplinkv1alpha1.UplinkStateReasonConfigurationConflict,
		uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
		uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
		uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
	} {
		if _, found := reasons[reason]; found {
			return reason
		}
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
}

func conditionsEqual(existing *metav1.Condition, desired metav1.Condition) bool {
	return existing != nil && existing.Status == desired.Status && existing.Reason == desired.Reason &&
		existing.Message == desired.Message
}

func truncateGatewayConditionError(message string) string {
	if len(message) <= maxGatewayConditionErrorLength {
		return message
	}
	return message[:maxGatewayConditionErrorLength]
}

func isUplinkStateNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		unwrapped := joined.Unwrap()
		if len(unwrapped) == 0 {
			return false
		}
		for _, nested := range unwrapped {
			if !isUplinkStateNotFound(nested) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isUplinkStateNotFound(wrapped.Unwrap())
	}
	return false
}

type uplinkGatewayError struct {
	reason string
	err    error
}

func (e *uplinkGatewayError) Error() string { return e.err.Error() }
func (e *uplinkGatewayError) Unwrap() error { return e.err }

func newUplinkGatewayError(reason string, err error) error {
	var gatewayErr *uplinkGatewayError
	if errors.As(err, &gatewayErr) {
		return err
	}
	return &uplinkGatewayError{reason: reason, err: err}
}

func uplinkGatewayFailureReason(err error) string {
	var gatewayErr *uplinkGatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr.reason
	}
	return uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed
}
