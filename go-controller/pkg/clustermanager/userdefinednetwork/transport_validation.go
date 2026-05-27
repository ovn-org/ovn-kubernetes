// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

const (
	ConditionTypeTransportAccepted = "TransportAccepted"

	ReasonVTEPNotFound                            = "VTEPNotFound"
	ReasonVTEPNotAccepted                         = "VTEPNotAccepted"
	ReasonEVPNConfigError                         = "EVPNConfigError"
	ReasonEVPNTransportAccepted                   = "EVPNTransportAccepted"
	ReasonEVPNRouteAdvertisementsIsMissing        = "EVPNRouteAdvertisementsIsMissing"
	ReasonEVPNRouteAdvertisementsNotAccepted      = "EVPNRouteAdvertisementsNotAccepted"
	ReasonNoOverlayTransportAccepted              = "NoOverlayTransportAccepted"
	ReasonNoOverlayRouteAdvertisementsIsMissing   = "NoOverlayRouteAdvertisementsIsMissing"
	ReasonNoOverlayRouteAdvertisementsNotAccepted = "NoOverlayRouteAdvertisementsNotAccepted"
	ReasonNoOverlayRouteAdvertisementsNotRequired = "NoOverlayRouteAdvertisementsNotRequired"
	ReasonRouteAdvertisementsNotEnabled           = "RouteAdvertisementsNotEnabled"

	MessageNoOverlayRouteAdvertisementsNotRequired = "Transport has been configured as no-overlay with unmanaged routing. RouteAdvertisements are not required; external routing is responsible for returning traffic to node pod subnets."
)

// setTransportStatusCondition validates the transport configuration for a CUDN and sets its TransportAccepted status condition.
// This function is called during CUDN reconciliation to ensure the transport is properly configured.
//
// For empty/default Geneve transport, no status condition is set (status is not updated).
// For EVPN, if evpnErr is non-nil (from the single validateEVPN call in reconcileCUDN),
// the error is surfaced as a TransportAccepted=False condition (VTEP or config issue).
// For no-overlay and EVPN transports (when EVPN validation passes), it validates that a
// RouteAdvertisements CR exists and is accepted.
// No-overlay CUDNs with unmanaged routing can be accepted without
// RouteAdvertisements only when no RouteAdvertisements object selects the CUDN
// and advertises PodNetwork.
//
// This function only SETS the condition on the provided CUDN object; it does NOT apply status.
// The actual status update is handled by updateClusterUDNStatus() to ensure a single status update.
//
// Per OKEP, CUDN validation failures should only update status, not emit events.
//
// Returns true if the TransportAccepted condition was updated (changed from previous value).
func (c *Controller) setTransportStatusCondition(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, evpnErr error) (bool, error) {
	if cudn == nil {
		return false, nil
	}

	transport := cudn.Spec.Network.GetTransport()

	// Skip setting transport condition for default Geneve transport (empty string)
	if transport == "" {
		return false, nil
	}

	// For EVPN, surface any validation error (VTEP or config) from the
	// single validateEVPN call performed by reconcileCUDN.
	if evpnErr != nil {
		var vtepNotFound *vtepNotFoundError
		var vtepNotAccepted *vtepNotAcceptedError
		var updated bool
		if errors.As(evpnErr, &vtepNotFound) {
			updated = c.setTransportCondition(cudn, metav1.ConditionFalse, ReasonVTEPNotFound,
				fmt.Sprintf("VTEP %q referenced by EVPN configuration does not exist.", vtepNotFound.vtepName))
		} else if errors.As(evpnErr, &vtepNotAccepted) {
			updated = c.setTransportCondition(cudn, metav1.ConditionFalse, ReasonVTEPNotAccepted,
				fmt.Sprintf("VTEP %q exists but is not accepted.", vtepNotAccepted.vtepName))
		} else {
			updated = c.setTransportCondition(cudn, metav1.ConditionFalse, ReasonEVPNConfigError, evpnErr.Error())
		}
		return updated, nil
	}

	// Validate RouteAdvertisements (for both NoOverlay and EVPN)
	if transport == userdefinednetworkv1.TransportOptionNoOverlay || transport == userdefinednetworkv1.TransportOptionEVPN {
		return c.validateTransportWithRouteAdvertisements(cudn, transport)
	}

	// Unknown transport - no validation
	return false, nil
}

// allowsNoOverlayWithoutRouteAdvertisements returns true for no-overlay CUDNs
// that do not require RouteAdvertisements because routing is unmanaged. In
// this mode, the external network is responsible for returning traffic to
// node pod subnets.
// CRD CEL validation already restricts no-overlay transport to primary layer 3
// networks with a noOverlay config, so only the routing mode needs checking
// here (the nil checks are panic guards).
func allowsNoOverlayWithoutRouteAdvertisements(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) bool {
	return cudn != nil &&
		cudn.Spec.Network.GetTransport() == userdefinednetworkv1.TransportOptionNoOverlay &&
		cudn.Spec.Network.NoOverlay != nil &&
		cudn.Spec.Network.NoOverlay.Routing == userdefinednetworkv1.RoutingUnmanaged
}

// validateTransportWithRouteAdvertisements validates that a CUDN with no-overlay or EVPN transport has proper RouteAdvertisements configuration.
// This function is used for both NoOverlay and EVPN transports as they both require RouteAdvertisements validation.
// This function updates the cudn object status then reconcileCUDN() goes ahead and updates the CR to reflect actual CUDN transport status.
// Returns true if the TransportAccepted condition was updated (changed from previous value).
func (c *Controller) validateTransportWithRouteAdvertisements(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, transport userdefinednetworkv1.TransportOption) (bool, error) {
	allowsNoRAAcceptance := allowsNoOverlayWithoutRouteAdvertisements(cudn)

	if c.raLister == nil {
		if allowsNoRAAcceptance {
			return c.setTransportCondition(cudn, metav1.ConditionTrue, ReasonNoOverlayRouteAdvertisementsNotRequired, MessageNoOverlayRouteAdvertisementsNotRequired), nil
		}
		// RouteAdvertisements feature not enabled
		updated := c.setTransportCondition(cudn, metav1.ConditionFalse, ReasonRouteAdvertisementsNotEnabled,
			fmt.Sprintf("RouteAdvertisements feature is not enabled but required for %s transport.", transport))
		return updated, nil
	}

	klog.V(5).Infof("Validating %s ClusterUserDefinedNetwork %q", transport, cudn.Name)

	// Get all RouteAdvertisements CRs
	ras, err := c.raLister.List(labels.Everything())
	if err != nil {
		return false, fmt.Errorf("failed to list RouteAdvertisements: %w", err)
	}

	// Track if we found RAs advertising this CUDN (but not accepted)
	foundCUDNRA := false
	var notAcceptedRANames []string
	var acceptedRAName string

	// Check if any RouteAdvertisements CR is configured for this CUDN
	for _, ra := range ras {
		selects, err := isRASelectingCUDN(ra, cudn)
		if err != nil {
			return false, fmt.Errorf("failed to check if RouteAdvertisements %q selects CUDN %q: %w", ra.Name, cudn.Name, err)
		}
		if selects {
			// Check if it advertises pod networks
			if !slices.Contains(ra.Spec.Advertisements, ratypes.PodNetwork) {
				continue
			}

			foundCUDNRA = true

			if notifier.IsRAAccepted(ra.Status.Conditions) {
				// Valid configuration found for this CUDN
				klog.V(5).Infof("Found valid RouteAdvertisements %q for ClusterUserDefinedNetwork %q with %s transport", ra.Name, cudn.Name, transport)
				acceptedRAName = ra.Name
				break
			} else {
				klog.Warningf("RouteAdvertisements %q selects ClusterUserDefinedNetwork %q but status is not Accepted", ra.Name, cudn.Name)
				notAcceptedRANames = append(notAcceptedRANames, ra.Name)
			}
		}
	}

	// Generate reason and message strings based on transport type
	var acceptedReason, missingReason, notAcceptedReason string
	var acceptedMessage string

	if transport == userdefinednetworkv1.TransportOptionNoOverlay {
		acceptedReason = ReasonNoOverlayTransportAccepted
		acceptedMessage = "Transport has been configured as 'no-overlay'."
		missingReason = ReasonNoOverlayRouteAdvertisementsIsMissing
		notAcceptedReason = ReasonNoOverlayRouteAdvertisementsNotAccepted
	} else { // EVPN
		acceptedReason = ReasonEVPNTransportAccepted
		acceptedMessage = "Transport has been configured as 'EVPN'."
		missingReason = ReasonEVPNRouteAdvertisementsIsMissing
		notAcceptedReason = ReasonEVPNRouteAdvertisementsNotAccepted
	}

	// Set status condition based on what we found and track if it changed
	var updated bool
	if acceptedRAName != "" {
		// Found an accepted RouteAdvertisements CR
		updated = c.setTransportCondition(cudn, metav1.ConditionTrue, acceptedReason, acceptedMessage)
	} else if !foundCUDNRA {
		// No RouteAdvertisements CR advertising this CUDN
		if allowsNoRAAcceptance {
			updated = c.setTransportCondition(cudn, metav1.ConditionTrue, ReasonNoOverlayRouteAdvertisementsNotRequired, MessageNoOverlayRouteAdvertisementsNotRequired)
		} else {
			updated = c.setTransportCondition(cudn, metav1.ConditionFalse, missingReason, "No RouteAdvertisements CR is advertising the pod networks.")
		}
	} else {
		// Found RAs advertising this CUDN, but none are accepted
		raNamesList := strings.Join(notAcceptedRANames, ", ")
		message := fmt.Sprintf("RouteAdvertisements CR %s advertises the pod subnets, but its status is not accepted.", raNamesList)
		updated = c.setTransportCondition(cudn, metav1.ConditionFalse, notAcceptedReason, message)
	}

	return updated, nil
}

// isRASelectingCUDN checks if a RouteAdvertisements CR selects a specific ClusterUserDefinedNetwork
func isRASelectingCUDN(ra *ratypes.RouteAdvertisements, cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) (bool, error) {
	for _, networkSelector := range ra.Spec.NetworkSelectors {
		if networkSelector.NetworkSelectionType == apitypes.ClusterUserDefinedNetworks {
			// Check if the label selector matches this CUDN
			if networkSelector.ClusterUserDefinedNetworkSelector != nil {
				selector, err := metav1.LabelSelectorAsSelector(&networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
				if err != nil {
					return false, fmt.Errorf("failed to convert label selector to selector: %w", err)
				}
				if selector.Matches(labels.Set(cudn.Labels)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// validateVTEP checks that the VTEP referenced by an EVPN CUDN has
// Accepted=True. Returns a vtepNotFoundError or vtepNotAcceptedError if the
// VTEP is missing or not accepted.
// Note: the not-found case only occurs for brand-new CUDNs referencing a
// VTEP that was never created. Once NADs exist, the VTEP finalizer prevents
// deletion while CUDNs reference it.
func (c *Controller) validateVTEP(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork) error {
	if cudn.Spec.Network.EVPN == nil || cudn.Spec.Network.EVPN.VTEP == "" {
		return nil
	}

	vtepName := cudn.Spec.Network.EVPN.VTEP
	vtep, err := c.vtepLister.Get(vtepName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &vtepNotFoundError{vtepName: vtepName}
		}
		return fmt.Errorf("failed to get VTEP %q: %w", vtepName, err)
	}

	if !notifier.IsVTEPAccepted(vtep.Status.Conditions) {
		return &vtepNotAcceptedError{vtepName: vtepName}
	}

	return nil
}

// setTransportCondition sets the TransportAccepted status condition on a ClusterUserDefinedNetwork.
// This function only SETS the condition on the provided CUDN object; it does NOT apply status to the API server.
// The actual status update is handled by updateClusterUDNStatus() to ensure a single status update with all conditions.
// Returns true if the condition was updated (changed from previous value).
func (c *Controller) setTransportCondition(cudn *userdefinednetworkv1.ClusterUserDefinedNetwork, status metav1.ConditionStatus, reason, message string) bool {
	if cudn == nil {
		return false
	}

	// Create and set the condition using meta.SetStatusCondition
	// This will automatically handle updates vs creates and transition times
	now := metav1.Now()
	condition := metav1.Condition{
		Type:               ConditionTypeTransportAccepted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}

	updated := meta.SetStatusCondition(&cudn.Status.Conditions, condition)
	klog.V(5).Infof("Set TransportAccepted condition for ClusterUserDefinedNetwork %q: %s (reason: %s)", cudn.Name, status, reason)
	return updated
}
