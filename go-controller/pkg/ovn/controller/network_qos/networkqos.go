// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	knet "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	nqosapiapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	conditionTypeReady   = "Ready-In-Zone-"
	reasonQoSSetupFailed = "Failed"
	// Limit status growth when many zones fail at once.
	maxConditions = 20
	// Log full errors, but keep status messages short.
	maxConditionMessageLength = 1024
	// Recheck in case the client dropped a switch event when its buffer filled.
	logicalSwitchRecheckInterval = time.Minute
)

func (c *Controller) syncNetworkQoS(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Dropping malformed NetworkQoS key %q: %v", key, err)
		return nil
	}

	c.cache.LockKey(key)
	defer func() {
		c.cache.UnlockKey(key)
		klog.V(5).Infof("Finished reconciling NetworkQoS %s in %v", key, time.Since(startTime))
	}()
	klog.V(5).Infof("Reconciling NetworkQoS %s", key)

	nqos, err := c.nqosLister.NetworkQoSes(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if nqos == nil || !nqos.DeletionTimestamp.IsZero() {
		c.forgetFailure(key)
		return c.clearNetworkQoS(key, namespace, name)
	}

	// Normalization changes CIDRs in place.
	nqos = nqos.DeepCopy()
	if err := validateAndNormalizeSpec(nqos); err != nil {
		return utilerrors.Join(c.clearNetworkQoS(key, namespace, name), c.reportSpecFailure(namespace, name, err))
	}

	desiredNetworks, err := c.resolveNetworks(nqos.Spec.NetworkSelectors)
	if err != nil {
		return utilerrors.Join(err, c.reportFailure(namespace, name, "failed to resolve the selected networks"))
	}

	programmed := map[string]*networkQoSState{}
	entry, loaded := c.cache.Load(key)
	if loaded && entry != nil {
		maps.Copy(programmed, entry.networks)
	} else if err := c.cleanupUnselectedNetworks(ovnObjectName(namespace, name), desiredNetworks); err != nil {
		// Do not populate the cache until old networks are cleaned up.
		return utilerrors.Join(err, c.reportFailure(namespace, name, "failed to clean up the networks it no longer selects"))
	}

	var errs []error
	var failedNetworks, unresolvedNetworks, failedRemovals []string
	for _, networkName := range slices.Sorted(maps.Keys(programmed)) {
		if _, selected := desiredNetworks[networkName]; selected {
			continue
		}
		networkStartTime := time.Now()
		if err := c.clearNetworkQoSOnNetwork(programmed[networkName]); err != nil {
			failedRemovals = append(failedRemovals, networkName)
			errs = append(errs, fmt.Errorf("network %s: %w", networkName, err))
			continue
		}
		recordNetworkQoSDeletionDuration(time.Since(networkStartTime))
		klog.V(4).Infof("Cleared NetworkQoS %s from network %s", key, networkName)
		delete(programmed, networkName)
	}

	waitingForSwitch := false
	for _, networkName := range slices.Sorted(maps.Keys(desiredNetworks)) {
		networkStartTime := time.Now()
		state, err := c.ensureNetworkQoSForNetwork(nqos, c.newNetworkContext(desiredNetworks[networkName]), programmed[networkName])
		// Even a failed attempt may have registered address sets that need releasing later.
		programmed[networkName] = state
		switch {
		case err == nil:
			recordNetworkQoSReconcileDuration(time.Since(networkStartTime))
		case errors.Is(err, errLogicalSwitchMissing):
			// Switch creation requeues this policy.
			klog.V(4).Infof("NetworkQoS %s waits for logical switch %s of network %s", key, state.switchName, networkName)
			waitingForSwitch = true
		case errors.Is(err, errAddressesUnresolved):
			// The address set manager requeues this policy when the error clears.
			unresolvedNetworks = append(unresolvedNetworks, networkName)
			errs = append(errs, fmt.Errorf("network %s: %w", networkName, err))
		default:
			failedNetworks = append(failedNetworks, networkName)
			errs = append(errs, fmt.Errorf("network %s: %w", networkName, err))
		}
	}

	// An empty entry tells the next reconciliation that startup cleanup is done.
	newEntry := &nqosCacheEntry{networks: programmed}
	c.cache.Store(key, newEntry)
	if !appliedNetworks(entry).Equal(appliedNetworks(newEntry)) {
		c.updateNetworkQoSCount()
	}
	if waitingForSwitch {
		c.nqosController.ReconcileAfter(key, logicalSwitchRecheckInterval)
	}

	if len(errs) > 0 {
		return utilerrors.Join(utilerrors.Join(errs...),
			c.reportFailure(namespace, name, failureReason(failedNetworks, unresolvedNetworks, failedRemovals)))
	}
	if err := c.clearStaleZoneStatus(namespace, name); err != nil {
		return fmt.Errorf("successfully reconciled NetworkQoS %s, but failed to patch status: %w", key, err)
	}
	return nil
}

func failureReason(failedNetworks, unresolvedNetworks, failedRemovals []string) string {
	var reasons []string
	if len(failedNetworks) > 0 {
		reasons = append(reasons, "failed to configure network(s) "+strings.Join(failedNetworks, ", "))
	}
	if len(unresolvedNetworks) > 0 {
		reasons = append(reasons, "failed to resolve the addresses of the pods it selects on network(s) "+
			strings.Join(unresolvedNetworks, ", "))
	}
	if len(failedRemovals) > 0 {
		reasons = append(reasons, "failed to remove it from network(s) "+strings.Join(failedRemovals, ", "))
	}
	return strings.Join(reasons, "; ")
}

func validateAndNormalizeSpec(nqos *networkqosapi.NetworkQoS) error {
	if err := validateNetworkSelectors(nqos.Spec.NetworkSelectors); err != nil {
		return err
	}
	if _, err := metav1.LabelSelectorAsSelector(&nqos.Spec.PodSelector); err != nil {
		return fmt.Errorf("invalid source pod selector: %w", err)
	}
	if len(nqos.Spec.Egress) > types.NetworkQoSMaxRules {
		// More rules would overlap the next policy priority band.
		return fmt.Errorf("%d egress rules, at most %d are supported", len(nqos.Spec.Egress), types.NetworkQoSMaxRules)
	}
	for ruleIndex, rule := range nqos.Spec.Egress {
		for portIndex, port := range rule.Classifier.Ports {
			if err := validatePort(port); err != nil {
				return fmt.Errorf("rule %d port %d: %w", ruleIndex, portIndex, err)
			}
		}
		for destIndex, dest := range rule.Classifier.To {
			if dest.IPBlock != nil && (dest.PodSelector != nil || dest.NamespaceSelector != nil) {
				return fmt.Errorf("rule %d destination %d: specifying both ipBlock and podSelector/namespaceSelector is not allowed",
					ruleIndex, destIndex)
			}
			if _, err := metav1.LabelSelectorAsSelector(dest.PodSelector); err != nil {
				return fmt.Errorf("rule %d destination %d: invalid pod selector: %w", ruleIndex, destIndex, err)
			}
			if _, err := metav1.LabelSelectorAsSelector(dest.NamespaceSelector); err != nil {
				return fmt.Errorf("rule %d destination %d: invalid namespace selector: %w", ruleIndex, destIndex, err)
			}
			if dest.IPBlock != nil {
				if err := normalizeIPBlock(dest.IPBlock); err != nil {
					return fmt.Errorf("rule %d destination %d: %w", ruleIndex, destIndex, err)
				}
			}
		}
	}
	return nil
}

// Older CRDs admit any protocol that starts with TCP, contains UDP or ends with SCTP. OVN
// rejects matches on other protocols. They also admit a port without a protocol, which
// matches the port on every protocol. An entry with neither still matches all traffic.
func validatePort(port *networkqosapi.Port) error {
	if port == nil {
		return errors.New("null entry")
	}
	switch port.Protocol {
	case "", "TCP", "UDP", "SCTP":
	default:
		return fmt.Errorf("unsupported protocol %q, must be TCP, UDP or SCTP", port.Protocol)
	}
	return nil
}

func normalizeIPBlock(ipBlock *knet.IPBlock) error {
	if ipBlock.CIDR == "" {
		return nil
	}
	cidr, err := parsePrefix(ipBlock.CIDR)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", ipBlock.CIDR, err)
	}
	ipBlock.CIDR = cidr.String()
	for index, except := range ipBlock.Except {
		exception, err := parsePrefix(except)
		if err != nil {
			return fmt.Errorf("invalid exception %d CIDR %q: %w", index, except, err)
		}
		if exception.Addr().Is4() != cidr.Addr().Is4() {
			return fmt.Errorf("exception %d %q is not of the IP family of CIDR %s", index, except, ipBlock.CIDR)
		}
		ipBlock.Except[index] = exception.String()
	}
	return nil
}

// Accept bare IPs as /32 or /128 prefixes. Clear host bits in CIDRs.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("address %s has a zone", s)
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func (c *Controller) ensureNetworkQoSForNetwork(nqos *networkqosapi.NetworkQoS, nctx *networkContext, previous *networkQoSState) (*networkQoSState, error) {
	state := &networkQoSState{
		name:           nqos.Name,
		namespace:      nqos.Namespace,
		networkName:    nctx.netInfo.GetNetworkName(),
		controllerName: nctx.controllerName,
		switchName:     nctx.logicalSwitchName(c.nodeName),
		addrSetKeys:    sets.New[string](),
		inherited:      previous == nil || previous.inherited,
	}
	previousKeys := sets.New[string]()
	if previous != nil {
		previousKeys = previous.addrSetKeys
	}
	// Old QoS rules may still reference these sets until a retry succeeds.
	fail := func(err error) (*networkQoSState, error) {
		state.addrSetKeys = state.addrSetKeys.Union(previousKeys)
		return state, err
	}
	if state.switchName == "" {
		return fail(fmt.Errorf("unsupported topology %s for network %s", nctx.netInfo.TopologyType(), state.networkName))
	}

	rules := buildGressRules(nqos)
	source, err := c.ensureAddressSets(nqos, state, rules, nctx)
	if err != nil {
		return fail(err)
	}
	existing, err := c.findOwnedQoSes(state.controllerName, state.getObjectNameKey())
	if err != nil {
		return fail(err)
	}
	start := time.Now()
	bound, err := c.bindQoSToLocalSwitch(state, source, rules, existing, nctx)
	if err != nil {
		return fail(err)
	}
	recordOvnOperationDuration("add", time.Since(start))
	start = time.Now()
	checkBindings := state.inherited || (previous != nil && previous.switchName != state.switchName)
	if err := c.cleanupStaleOvnObjects(state, bound, existing, checkBindings); err != nil {
		return fail(fmt.Errorf("failed to delete stale QoSes: %w", err))
	}
	recordOvnOperationDuration("remove", time.Since(start))
	// All remaining rules now use address sets registered by this process.
	state.inherited = false
	// The rewritten rules no longer reference the legacy sets.
	if err := c.deleteLegacyAddressSets(state.getObjectNameKey(), func(ownerController string) bool {
		return ownerController == state.controllerName
	}); err != nil {
		return fail(err)
	}
	if err := c.releaseAddressSets(state, previousKeys.Difference(state.addrSetKeys)); err != nil {
		return fail(err)
	}
	state.applied = true
	// The rules may still use stale addresses if pod address resolution failed.
	if err := c.addressSetErrors(state); err != nil {
		return state, fmt.Errorf("%w: %w", errAddressesUnresolved, err)
	}
	return state, nil
}

// The address set manager requeues affected policies when address resolution recovers.
var errAddressesUnresolved = errors.New("the addresses of the selected pods cannot be resolved")

func (c *Controller) addressSetErrors(state *networkQoSState) error {
	var errs []error
	for _, key := range sets.List(state.addrSetKeys) {
		if err := c.addressSetManager.AddressSetError(key); err != nil {
			errs = append(errs, fmt.Errorf("address set %s: %w", key, err))
		}
	}
	return utilerrors.Join(errs...)
}

func (c *Controller) ensureAddressSets(nqos *networkqosapi.NetworkQoS, qosState *networkQoSState, rules []*GressRule,
	nctx *networkContext) (addrSetRef, error) {
	// Each zone only owns the pod ports on its node, so only those pods can be sources.
	source, err := c.ensureAddressSet(qosState, nctx, &nqos.Spec.PodSelector, nil, nqos.Namespace,
		addresssetmanager.WithNodeName(c.nodeName))
	if err != nil {
		return source, fmt.Errorf("failed to ensure the source address set: %w", err)
	}

	for ruleIndex, rule := range rules {
		for destIndex, dest := range rule.Classifier.Destinations {
			if !dest.hasSelectors() {
				continue
			}
			podSelector, namespaceSelector, namespace := destinationSelectors(dest, nqos.Namespace)
			// Destinations may be on any node.
			dest.addrSet, err = c.ensureAddressSet(qosState, nctx, podSelector, namespaceSelector, namespace)
			if err != nil {
				return source, fmt.Errorf("failed to ensure the address set of rule %d destination %d: %w", ruleIndex, destIndex, err)
			}
		}
	}
	return source, nil
}

func (c *Controller) ensureAddressSet(qosState *networkQoSState, nctx *networkContext,
	podSelector, namespaceSelector *metav1.LabelSelector, namespace string,
	opts ...addresssetmanager.AddressSetOption) (addrSetRef, error) {
	// Host-network pod IPs are node IPs and must not be subject to pod QoS.
	opts = append(opts, addresssetmanager.ExcludeHostNetworkPods())
	key, hashV4, hashV6, err := c.addressSetManager.EnsureAddressSet(podSelector, namespaceSelector, nil,
		namespace, qosState.addrSetBackRef(), nctx.controllerName, nctx.netInfo, false, opts...)
	ref := addrSetRef{key: key, hashV4: hashV4, hashV6: hashV6}
	// EnsureAddressSet can return a key on error. Keep it so we can release the reference.
	if key != "" {
		qosState.holdAddressSet(ref)
	}
	if err != nil {
		return ref, err
	}
	if ipv4Mode, ipv6Mode := nctx.netInfo.IPMode(); ref.matchString(trafficDirSource, ipv4Mode, ipv6Mode) == "" {
		return ref, fmt.Errorf("address set %s holds none of the IP families of network %s", key, qosState.networkName)
	}
	return ref, nil
}

// Without a namespace selector, destinations are in the policy namespace. Without a pod
// selector, all pods in the selected namespaces match.
func destinationSelectors(dest *Destination, nqosNamespace string) (podSelector, namespaceSelector *metav1.LabelSelector, namespace string) {
	podSelector = dest.PodSelector
	if podSelector == nil {
		podSelector = &metav1.LabelSelector{}
	}
	if dest.NamespaceSelector == nil {
		return podSelector, nil, nqosNamespace
	}
	return podSelector, dest.NamespaceSelector, ""
}

func (c *Controller) releaseAddressSets(qosState *networkQoSState, keys sets.Set[string]) error {
	var errs []error
	// Take a copy because releasing a key also removes it from addrSetKeys.
	for _, key := range keys.UnsortedList() {
		if err := c.addressSetManager.DeleteAddressSet(key, qosState.addrSetBackRef()); err != nil {
			errs = append(errs, fmt.Errorf("failed to release address set %s: %w", key, err))
			continue
		}
		qosState.addrSetKeys.Delete(key)
	}
	return utilerrors.Join(errs...)
}

func (c *Controller) clearNetworkQoS(key, namespace, name string) error {
	klog.V(4).Infof("Cleaning up NetworkQoS %s", key)
	startTime := time.Now()
	entry, _ := c.cache.Load(key)
	// OVN does not enforce references to address sets in matches. Keep the sets until rule
	// deletion succeeds.
	if err := c.deleteByObjectName("", ovnObjectName(namespace, name)); err != nil {
		return fmt.Errorf("failed to delete QoS rules for NetworkQoS %s: %w", key, err)
	}
	var errs []error
	if entry != nil {
		for _, state := range entry.networks {
			if err := c.releaseAddressSets(state, state.addrSetKeys); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return utilerrors.Join(errs...)
	}
	if entry != nil && len(entry.networks) > 0 {
		recordNetworkQoSDeletionDuration(time.Since(startTime))
	}
	c.cache.Delete(key)
	if appliedNetworks(entry).Len() > 0 {
		c.updateNetworkQoSCount()
	}
	return nil
}

func appliedNetworks(entry *nqosCacheEntry) sets.Set[string] {
	networks := sets.New[string]()
	if entry == nil {
		return networks
	}
	for networkName, state := range entry.networks {
		if state.applied {
			networks.Insert(networkName)
		}
	}
	return networks
}

func (c *Controller) clearNetworkQoSOnNetwork(state *networkQoSState) error {
	if err := c.deleteByObjectName(state.controllerName, state.getObjectNameKey()); err != nil {
		return err
	}
	return c.releaseAddressSets(state, state.addrSetKeys)
}

// Only report failures to avoid a status write from every node on success.
// No conditions means no reported errors.
func (c *Controller) clearStaleZoneStatus(namespace, name string) error {
	key := nqosKey(namespace, name)
	c.setFailureUnreported(key, false)
	nqos, err := c.nqosLister.NetworkQoSes(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if c.failureReported(key) {
		// The informer may not have seen the failure yet. Retracting does not depend on it.
		return c.retractCondition(namespace, name)
	}
	// The informer may contain a condition written by the previous process.
	condition := meta.FindStatusCondition(nqos.Status.Conditions, conditionTypeReady+c.nodeName)
	if condition == nil {
		return nil
	}
	if condition.Status == metav1.ConditionTrue && nqos.Status.Status != types.NetworkQoSNoErrorsMsg {
		// Older cluster managers wait for every zone to report. Keep our old success
		// condition until an upgraded cluster manager reports no errors.
		return nil
	}
	return c.retractCondition(namespace, name)
}

// Applying an empty status as this node removes the condition owned by its field manager.
func (c *Controller) retractCondition(namespace, name string) error {
	startTime := time.Now()
	applyObj := nqosapiapply.NetworkQoS(name, namespace)
	if _, err := c.nqosClientSet.K8sV1alpha1().NetworkQoSes(namespace).ApplyStatus(context.TODO(), applyObj,
		metav1.ApplyOptions{FieldManager: c.nodeName, Force: true}); err != nil {
		return err
	}
	c.forgetFailure(nqosKey(namespace, name))
	recordStatusPatchDuration(time.Since(startTime))
	return nil
}

func (c *Controller) forgetFailure(key string) {
	c.reportedFailuresMutex.Lock()
	defer c.reportedFailuresMutex.Unlock()
	c.reportedFailures.Delete(key)
	c.unreportedFailures.Delete(key)
}

func (c *Controller) setFailureReported(key string) {
	c.reportedFailuresMutex.Lock()
	defer c.reportedFailuresMutex.Unlock()
	c.reportedFailures.Insert(key)
	c.unreportedFailures.Delete(key)
}

func (c *Controller) setFailureUnreported(key string, unreported bool) {
	c.reportedFailuresMutex.Lock()
	defer c.reportedFailuresMutex.Unlock()
	if unreported {
		c.unreportedFailures.Insert(key)
	} else {
		c.unreportedFailures.Delete(key)
	}
}

func (c *Controller) failureReported(key string) bool {
	c.reportedFailuresMutex.Lock()
	defer c.reportedFailuresMutex.Unlock()
	return c.reportedFailures.Has(key)
}

func (c *Controller) failureUnreported(key string) bool {
	c.reportedFailuresMutex.Lock()
	defer c.reportedFailuresMutex.Unlock()
	return c.unreportedFailures.Has(key)
}

// One zone reports an invalid spec. If its condition disappears, another zone takes over.
func (c *Controller) reportSpecFailure(namespace, name string, err error) error {
	return c.report(namespace, name, specFailureMessage(err), true)
}

func specFailureMessage(err error) string {
	return fmt.Sprintf("%s: invalid spec, not applied on any node: %v", types.NetworkQoSErrorMsg, err)
}

func (c *Controller) reportFailure(namespace, name, reason string) error {
	return c.report(namespace, name, fmt.Sprintf("%s: %s", types.NetworkQoSErrorMsg, reason), false)
}

// Retry failed status writes even when the spec error is permanent.
// For shared failures, only one zone needs to write a condition.
func (c *Controller) report(namespace, name, msg string, sharedFailure bool) error {
	klog.Errorf("NetworkQoS %s/%s: %s", namespace, name, msg)
	cond := metav1.Condition{
		Type:    conditionTypeReady + c.nodeName,
		Status:  metav1.ConditionFalse,
		Reason:  reasonQoSSetupFailed,
		Message: truncateMessage(msg),
	}
	startTime := time.Now()
	result, err := c.updateStatusCondition(cond, namespace, name, sharedFailure)
	if err != nil {
		klog.Warningf("Failed to update the status of NetworkQoS %s/%s: %v", namespace, name, err)
		return fmt.Errorf("failed to update the status of NetworkQoS %s/%s: %w", namespace, name, err)
	}
	key := nqosKey(namespace, name)
	switch result {
	case conditionWritten:
		c.setFailureReported(key)
		recordStatusPatchDuration(time.Since(startTime))
	case conditionUnchanged:
		c.setFailureReported(key)
	case conditionLeftToOtherZones:
		c.setFailureUnreported(key, true)
	}
	return nil
}

func truncateMessage(msg string) string {
	const ellipsis = "..."
	if len(msg) <= maxConditionMessageLength {
		return msg
	}
	cut := maxConditionMessageLength - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + ellipsis
}

type conditionUpdate int

const (
	conditionUnchanged conditionUpdate = iota
	conditionWritten
	// Another zone reports this error, or maxConditions has been reached.
	conditionLeftToOtherZones
)

// Check the resource version so simultaneous writes cannot exceed maxConditions.
// A conflict retries with a fresh object. Skip writes when the condition has not changed.
func (c *Controller) updateStatusCondition(newCondition metav1.Condition, namespace, name string, sharedFailure bool) (conditionUpdate, error) {
	nqos, err := c.nqosLister.NetworkQoSes(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("NetworkQoS %s/%s updating status but not found, ignoring", namespace, name)
			return conditionUnchanged, nil
		}
		return conditionUnchanged, err
	}
	newCondition.ObservedGeneration = nqos.Generation

	existingCondition := meta.FindStatusCondition(nqos.Status.Conditions, newCondition.Type)
	if existingCondition != nil &&
		existingCondition.Status == newCondition.Status &&
		existingCondition.Reason == newCondition.Reason &&
		existingCondition.Message == newCondition.Message &&
		existingCondition.ObservedGeneration == newCondition.ObservedGeneration {
		return conditionUnchanged, nil
	}
	if sharedFailure && reportedByOtherZone(nqos.Status.Conditions, newCondition) {
		if existingCondition != nil {
			// Another zone is reporting this error. Remove any older error from this zone.
			if err := c.retractCondition(namespace, name); err != nil {
				return conditionUnchanged, err
			}
		}
		return conditionLeftToOtherZones, nil
	}
	if existingCondition == nil && countFailures(nqos.Status.Conditions) >= maxConditions {
		klog.Warningf("NetworkQoS %s/%s already holds %d failures, not reporting the failure of zone %s",
			namespace, name, maxConditions, c.nodeName)
		return conditionLeftToOtherZones, nil
	}

	newConditionApply := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &newCondition.Type,
		Status:             &newCondition.Status,
		ObservedGeneration: &newCondition.ObservedGeneration,
		Reason:             &newCondition.Reason,
		Message:            &newCondition.Message,
	}
	if existingCondition == nil || existingCondition.Status != newCondition.Status {
		newConditionApply.LastTransitionTime = ptr.To(metav1.NewTime(time.Now()))
	} else {
		newConditionApply.LastTransitionTime = &existingCondition.LastTransitionTime
	}

	applyObj := nqosapiapply.NetworkQoS(name, namespace)
	if nqos.ResourceVersion != "" {
		applyObj = applyObj.WithResourceVersion(nqos.ResourceVersion)
	}
	applyObj = applyObj.WithStatus(nqosapiapply.Status().WithConditions(newConditionApply))
	if _, err := c.nqosClientSet.K8sV1alpha1().NetworkQoSes(namespace).ApplyStatus(context.TODO(), applyObj,
		metav1.ApplyOptions{FieldManager: c.nodeName, Force: true}); err != nil {
		if apierrors.IsConflict(err) {
			return conditionUnchanged, fmt.Errorf("the conditions changed while being written, retrying with the new ones: %w", err)
		}
		return conditionUnchanged, err
	}
	return conditionWritten, nil
}

func reportedByOtherZone(conditions []metav1.Condition, failure metav1.Condition) bool {
	for _, condition := range conditions {
		if condition.Type != failure.Type &&
			strings.HasPrefix(condition.Type, conditionTypeReady) &&
			condition.Status == metav1.ConditionFalse &&
			condition.Message == failure.Message &&
			condition.ObservedGeneration == failure.ObservedGeneration {
			return true
		}
	}
	return false
}

func countFailures(conditions []metav1.Condition) int {
	failures := 0
	for _, condition := range conditions {
		if strings.HasPrefix(condition.Type, conditionTypeReady) && condition.Status == metav1.ConditionFalse {
			failures++
		}
	}
	return failures
}
