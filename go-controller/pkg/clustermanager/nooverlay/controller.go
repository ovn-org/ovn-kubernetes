// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package nooverlay

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
)

// validationErrorType represents different types of validation failures
type validationErrorType int

const (
	errTypeNotAccepted validationErrorType = iota
	errTypeNoRouteAdvertise
)

// eventReason represents Kubernetes event reasons
type eventReason string

const (
	eventReasonRANotAccepted eventReason = "RouteAdvertisementsNotAccepted"
	eventReasonNoRA          eventReason = "NoRouteAdvertisements"
	eventReasonConfigError   eventReason = "NoOverlayConfigurationError"
	eventReasonConfigReady   eventReason = "NoOverlayConfigurationReady"
)

// conditionTypeAccepted is the Accepted condition type for RouteAdvertisements
const conditionTypeAccepted = "Accepted"

// validationError represents different types of validation failures
type validationError struct {
	errorType validationErrorType
	message   string
	raNames   []string // Names of RAs that exist but aren't accepted (for notAccepted scenario)
}

func (e *validationError) Error() string {
	return e.message
}

// Controller validates no-overlay configuration with RouteAdvertisements.
// It watches RouteAdvertisements CRs, triggering validation when relevant changes occur.
type Controller struct {
	wf       *factory.WatchFactory
	recorder record.EventRecorder

	// raController watches RouteAdvertisements resources
	raController controllerutil.Controller

	// validationLock protects validation state
	validationLock sync.Mutex
	// lastValidationError tracks the last validation error to avoid spamming events
	lastValidationError string
}

// NewController creates a new no-overlay validation controller.
func NewController(wf *factory.WatchFactory, recorder record.EventRecorder) *Controller {
	klog.Infof("Creating no-overlay validation controller")

	c := &Controller{
		wf:       wf,
		recorder: recorder,
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
	if c == nil {
		return
	}

	klog.Infof("Stopping no-overlay validation controller")

	controllerutil.Stop(c.raController)
}

// reconcileRA is called whenever a RouteAdvertisements resource changes
func (c *Controller) reconcileRA(_ context.Context, key string) error {
	klog.V(5).Infof("No-overlay controller reconciling RouteAdvertisements %q", key)
	c.runValidation()
	return nil
}

// selectsDefaultNetwork returns true if the RouteAdvertisements selects the default network
func selectsDefaultNetwork(ra *ratypes.RouteAdvertisements) bool {
	for _, networkSelector := range ra.Spec.NetworkSelectors {
		if networkSelector.NetworkSelectionType == apitypes.DefaultNetwork {
			return true
		}
	}
	return false
}

// raNeedsValidation checks if the RouteAdvertisements update requires validation
func (c *Controller) raNeedsValidation(oldRA, newRA *ratypes.RouteAdvertisements) bool {
	// If either object is nil, we need to validate, e.g., on deletion or addition
	if oldRA == nil || newRA == nil {
		return true
	}

	// If the RA started or stopped advertising default network, validate
	if selectsDefaultNetwork(oldRA) != selectsDefaultNetwork(newRA) {
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
	return isRAAccepted(oldRA.Status.Conditions) != isRAAccepted(newRA.Status.Conditions)
}

// runValidation runs validation and emits events if the state changed
func (c *Controller) runValidation() {
	c.validationLock.Lock()
	defer c.validationLock.Unlock()

	err := c.validate()
	currentError := ""
	if err != nil {
		currentError = err.Error()
	}

	// Only emit event if error state changed
	if currentError != c.lastValidationError {
		if err != nil {
			klog.Errorf("No-overlay validation failed: %v", err)
			c.emitValidationEvent(err)
		} else {
			klog.Infof("No-overlay validation passed: RouteAdvertisements configuration is now valid")
			c.emitReadyEvent()
		}
		c.lastValidationError = currentError
	}
}

// validate checks if the no-overlay configuration is valid
func (c *Controller) validate() error {
	// Get all RouteAdvertisements CRs
	ras, err := c.wf.RouteAdvertisementsInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	// Track if we found RAs advertising default network that are not accepted
	foundDefaultNetworkRA := false
	notAcceptedRANames := []string{}

	// Check if any RouteAdvertisements CR is configured for the default network
	for _, ra := range ras {
		// Check if this RouteAdvertisements selects the default network
		for _, networkSelector := range ra.Spec.NetworkSelectors {
			if networkSelector.NetworkSelectionType == apitypes.DefaultNetwork {
				// Found a RouteAdvertisements for default network
				// Check if it advertises pod networks
				if !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
					continue
				}

				// We found at least one RA advertising default network
				foundDefaultNetworkRA = true

				if isRAAccepted(ra.Status.Conditions) {
					// Valid configuration found
					klog.V(5).Infof("Found valid RouteAdvertisements %q for default network with no-overlay transport", ra.Name)
					return nil
				} else {
					klog.Warningf("RouteAdvertisements %q selects default network but status is not Accepted", ra.Name)
					notAcceptedRANames = append(notAcceptedRANames, ra.Name)
				}
			}
		}
	}

	// Return specific error based on what we found
	if !foundDefaultNetworkRA {
		return &validationError{
			errorType: errTypeNoRouteAdvertise,
			message:   "no RouteAdvertisements CR is advertising the default network pod networks",
		}
	}

	// Found RAs advertising default network, but none are accepted
	// Sort names to ensure deterministic error messages for event deduplication
	slices.Sort(notAcceptedRANames)
	return &validationError{
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
func (c *Controller) emitReadyEvent() {
	c.emitEvent(
		corev1.EventTypeNormal,
		string(eventReasonConfigReady),
		"No-overlay transport is properly configured with RouteAdvertisements CR advertising the default network pod networks with status Accepted=True",
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

func isRAAccepted(conditions []metav1.Condition) bool {
	condition := meta.FindStatusCondition(conditions, conditionTypeAccepted)
	return condition != nil && condition.Status == metav1.ConditionTrue
}
