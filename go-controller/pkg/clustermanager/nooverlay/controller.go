// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package nooverlay

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// validationErrorType represents different types of validation failures
type validationErrorType int

const (
	errTypeNotAccepted validationErrorType = iota
	errTypeNoRouteAdvertise
)

type validationMode string

const (
	validationModeRouteAdvertisements   validationMode = "RouteAdvertisements"
	validationModeNoRouteAdvertisements validationMode = "NoRouteAdvertisements"
)

// eventReason represents Kubernetes event reasons
type eventReason string

const (
	eventReasonRANotAccepted eventReason = "RouteAdvertisementsNotAccepted"
	eventReasonNoRA          eventReason = "NoRouteAdvertisements"
	eventReasonConfigError   eventReason = "NoOverlayConfigurationError"
	eventReasonConfigReady   eventReason = "NoOverlayConfigurationReady"
)

// validationError represents different types of validation failures
type validationError struct {
	errorType validationErrorType
	message   string
	raNames   []string // Names of RAs that exist but aren't accepted (for notAccepted scenario)
}

func (e *validationError) Error() string {
	return e.message
}

// validationState is the outcome of a validation run, used to avoid emitting
// duplicate events: a mode when validation passed, an error text otherwise.
// The zero value means no validation has run yet.
type validationState struct {
	mode    validationMode
	errText string
}

// Controller validates no-overlay configuration with RouteAdvertisements.
// It watches RouteAdvertisements CRs, triggering validation when relevant changes occur.
// When RouteAdvertisements are disabled, it validates once: the only valid
// no-overlay configuration without them is unmanaged routing.
type Controller struct {
	wf       *factory.WatchFactory
	recorder record.EventRecorder

	// raController watches RouteAdvertisements resources; nil when
	// RouteAdvertisements are disabled
	raController controllerutil.Controller

	// validationLock protects validation state
	validationLock sync.Mutex
	// lastValidationState tracks the last validation outcome to avoid spamming
	// events; the zero value means no validation has run yet
	lastValidationState validationState
}

// NewController creates a new no-overlay validation controller.
func NewController(wf *factory.WatchFactory, recorder record.EventRecorder) *Controller {
	klog.Infof("Creating no-overlay validation controller")

	c := &Controller{
		wf:       wf,
		recorder: recorder,
	}

	if !util.IsRouteAdvertisementsEnabled() {
		return c
	}

	// Create controller config with RouteAdvertisements informer
	raConfig := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileRA,
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsValidation,
	}
	c.raController = controllerutil.NewController("no-overlay-ra-watcher", raConfig)

	return c
}

// Start starts the no-overlay validation controller
func (c *Controller) Start() error {
	if c.raController == nil {
		// RouteAdvertisements are disabled: the configuration is static, so a
		// single validation is enough.
		c.runValidation()
		klog.Infof("no-overlay validation controller started without RouteAdvertisements")
		return nil
	}
	// Start controller with initial validation after cache sync.
	// This ensures the informer cache is populated before validation runs,
	// preventing false errors from reading an empty cache.
	if err := controllerutil.StartWithInitialSync(func() error {
		c.runValidation()
		return nil
	}, c.raController); err != nil {
		return err
	}
	klog.Infof("no-overlay validation controller started")
	return nil
}

// Stop stops the no-overlay validation controller
func (c *Controller) Stop() {
	if c == nil || c.raController == nil {
		return
	}

	klog.Infof("Stopping no-overlay validation controller")

	controllerutil.Stop(c.raController)
}

// reconcileRA is called whenever a RouteAdvertisements resource changes
func (c *Controller) reconcileRA(key string) error {
	klog.V(5).Infof("No-overlay controller reconciling RouteAdvertisements %q", key)
	c.runValidation()
	return nil
}

// raNeedsValidation checks if the RouteAdvertisements update requires validation
func (c *Controller) raNeedsValidation(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	// If either object is nil, we need to validate, e.g., on deletion or addition
	if oldRA == nil || newRA == nil {
		return true
	}

	// If the RA started or stopped advertising default network, validate
	if util.RASelectsDefaultNetwork(oldRA) != util.RASelectsDefaultNetwork(newRA) {
		return true
	}

	// Check if NetworkSelectors changed
	if !reflect.DeepEqual(oldRA.Spec.NetworkSelectors, newRA.Spec.NetworkSelectors) {
		return true
	}

	// Check if Advertisements changed
	if !reflect.DeepEqual(oldRA.Spec.Advertisements, newRA.Spec.Advertisements) {
		return true
	}

	// Check if Accepted condition changed
	return notifier.IsRAAccepted(oldRA.Status.Conditions) != notifier.IsRAAccepted(newRA.Status.Conditions)
}

// runValidation runs validation and emits events if the state changed
func (c *Controller) runValidation() {
	c.validationLock.Lock()
	defer c.validationLock.Unlock()

	mode, err := c.validate()
	currentState := validationState{mode: mode}
	if err != nil {
		currentState = validationState{errText: err.Error()}
	}

	// Only emit an event if the validation outcome changed.
	if c.lastValidationState != currentState {
		if err != nil {
			klog.Errorf("No-overlay validation failed: %v", err)
			c.emitValidationEvent(err)
		} else {
			klog.Infof("No-overlay validation passed")
			c.emitReadyEvent(mode)
		}
		c.lastValidationState = currentState
	}
}

// validate selects the active default-network no-overlay state: accepted RA,
// unmanaged no-overlay without RAs, or invalid when matching RAs exist but none
// are accepted.
func (c *Controller) validate() (validationMode, error) {
	if !util.IsRouteAdvertisementsEnabled() {
		if config.IsDefaultNetworkUnmanagedNoOverlay() {
			return validationModeNoRouteAdvertisements, nil
		}
		// not reachable with a validated config: config validation requires
		// RouteAdvertisements for no-overlay unless routing is unmanaged
		return "", fmt.Errorf("RouteAdvertisements are disabled: transport=no-overlay requires RouteAdvertisements unless routing is unmanaged")
	}

	// Get all RouteAdvertisements CRs
	ras, err := c.wf.RouteAdvertisementsInformer().Lister().List(labels.Everything())
	if err != nil {
		return "", fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	// Track matching RAs that are not accepted. Accepted matches return early.
	notAcceptedRANames := []string{}

	for _, ra := range ras {
		if !util.RAAdvertisesDefaultNetwork(ra) {
			continue
		}

		if notifier.IsRAAccepted(ra.Status.Conditions) {
			klog.V(5).Infof("Found valid RouteAdvertisements %q for default network with no-overlay transport", ra.Name)
			return validationModeRouteAdvertisements, nil
		}
		klog.Warningf("RouteAdvertisements %q selects default network but status is not Accepted", ra.Name)
		notAcceptedRANames = append(notAcceptedRANames, ra.Name)
	}

	if len(notAcceptedRANames) == 0 {
		if config.IsDefaultNetworkUnmanagedNoOverlay() {
			return validationModeNoRouteAdvertisements, nil
		}
		return "", &validationError{
			errorType: errTypeNoRouteAdvertise,
			message:   "no RouteAdvertisements CR is advertising the default network pod networks",
		}
	}

	// Found RAs advertising default network, but none are accepted
	// Sort names to ensure deterministic error messages for event deduplication
	slices.Sort(notAcceptedRANames)
	return "", &validationError{
		errorType: errTypeNotAccepted,
		message:   fmt.Sprintf("RouteAdvertisements CRs %q are advertising the default network pod networks but none have status Accepted=True", notAcceptedRANames),
		raNames:   notAcceptedRANames,
	}
}

// emitValidationEvent emits a Kubernetes event for validation failures
func (c *Controller) emitValidationEvent(err error) {
	var reason eventReason
	var eventMessage string

	// Check if this is our custom validation error type
	if valErr, ok := err.(*validationError); ok {
		switch valErr.errorType {
		case errTypeNotAccepted:
			// Scenario: RAs exist but none are accepted
			reason = eventReasonRANotAccepted
			if len(valErr.raNames) > 0 {
				eventMessage = fmt.Sprintf("RouteAdvertisements CR(s) %v exist for the default network but none have status Accepted=True. "+
					"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks.",
					strings.Join(valErr.raNames, ", "))
			} else {
				eventMessage = "RouteAdvertisements CR(s) exist for the default network but none have status Accepted=True. " +
					"When transport=no-overlay, at least one RouteAdvertisements CR must be accepted to advertise pod networks."
			}
		case errTypeNoRouteAdvertise:
			// Scenario: No RAs advertising default network
			reason = eventReasonNoRA
			eventMessage = "No RouteAdvertisements CR is advertising the default network. " +
				"RouteAdvertisements configuration is required when transport=no-overlay."
		default:
			// Unknown validation error type
			reason = eventReasonConfigError
			eventMessage = fmt.Sprintf("No-overlay transport configuration error: %v", err)
		}
	} else {
		// Generic error
		reason = eventReasonConfigError
		eventMessage = fmt.Sprintf("No-overlay transport configuration error: %v", err)
	}

	c.emitEvent(corev1.EventTypeWarning, string(reason), eventMessage)
}

// emitReadyEvent emits a Normal event when validation passes
func (c *Controller) emitReadyEvent(mode validationMode) {
	message := "No-overlay transport is properly configured with RouteAdvertisements CR advertising the default network pod networks with status Accepted=True"
	if mode == validationModeNoRouteAdvertisements {
		message = "No-overlay transport is configured with unmanaged routing and no RouteAdvertisements. Routes to pod subnets are not installed on nodes; external routing is responsible for returning traffic to node pod subnets."
	}
	c.emitEvent(
		corev1.EventTypeNormal,
		string(eventReasonConfigReady),
		message,
	)
}

// emitEvent emits a Kubernetes event on the default network NAD.
func (c *Controller) emitEvent(eventType, reason, message string) {
	c.recorder.Eventf(
		&corev1.ObjectReference{
			Kind:      "NetworkAttachmentDefinition",
			Name:      config.Default.ClusterDefaultNetworkNAD.Name,
			Namespace: config.Default.ClusterDefaultNetworkNAD.Namespace,
		},
		eventType,
		reason,
		"%s",
		message,
	)
}
