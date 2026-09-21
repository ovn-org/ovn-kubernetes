// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// networkRefReconcilerFunc adapts a function to the
// networkmanager.NetworkRefReconciler interface.
type networkRefReconcilerFunc func(node, networkName string)

func (f networkRefReconcilerFunc) Reconcile(node, networkName string) { f(node, networkName) }

// syncNetwork syncs a single network, falling back to a full network sync when a
// source may need to be (re)allocated.
func (c *MACBindingController) syncNetwork(key string) error {
	// the key can be a NAD namespaced name or a network name
	var netInfo util.NetInfo
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %w", key, err)
	}
	switch namespace {
	case "":
		netInfo = c.networkManager.GetNetwork(key)
	default:
		netInfo = c.networkManager.GetNetInfoForNADKey(key)
	}

	if netInfo == nil && namespace != "" {
		// deletes are handled with port binding events queueing networks, so
		// ignore NAD delete events
		return nil
	}

	// we can assume the key is the network name for our own cache lookups if it
	// doesn't exist
	networkName := key
	if netInfo != nil {
		networkName = netInfo.GetNetworkName()
	}
	portName := util.GetNetworkScopedGWRouterExtPortName(networkName, c.nodeName)

	// this might be a new source for our unknown source ports, full reconcile (enqueued
	// to dedup)
	if netInfo != nil && c.hasUnknownSource() {
		c.enqueueAllNetworks()
		return nil
	}

	// a port acting as source for followers might have been deleted (even if
	// network manager isn't aware), full reconcile to relocate its followers
	// (enqueued to dedup)
	if c.tracksSourceWithFollowers(portName) {
		c.enqueueAllNetworks()
		return nil
	}

	return c.processNetworks(networkName)
}

// syncAllNetworks reconciles every tracked network.
func (c *MACBindingController) syncAllNetworks() error {
	return c.processNetworks()
}

// processNetworks reconciles the given networks, or all of them when none are
// given, reconciling the follower map against the networks' ports currently in
// SBDB and enqueuing follower syncs and cleanups for the resulting changes.
func (c *MACBindingController) processNetworks(networks ...string) error {
	knownPorts := sets.New[string]()
	portToUplink := map[string]string{}
	fullReconcile := len(networks) == 0
	var err error
	switch {
	case fullReconcile:
		// no network means reconcile all networks
		networks, err = c.getAllNetworkInfo(knownPorts, portToUplink)
	default:
		err = c.getNetworksInfo(networks, knownPorts, portToUplink)
	}
	if err != nil {
		return fmt.Errorf("failed to gather network info: %w", err)
	}

	// filter out ports that don't exist in SBDB.
	validPorts, err := c.validatePorts(knownPorts)
	if err != nil {
		return err
	}

	addPorts := sets.New[string]()
	removePorts := sets.New[string]()
	trackedPorts := c.getAllPorts()
	if fullReconcile {
		removePorts = trackedPorts.Difference(validPorts)
	}
	for _, network := range networks {
		port := util.GetNetworkScopedGWRouterExtPortName(network, c.nodeName)
		switch {
		case trackedPorts.Has(port) && !validPorts.Has(port):
			removePorts.Insert(port)
		case !trackedPorts.Has(port) && validPorts.Has(port):
			addPorts.Insert(port)
		}
	}

	// skip on no additions or deletions and no unknown sources
	if addPorts.Len() == 0 && removePorts.Len() == 0 && !c.hasUnknownSource() {
		return nil
	}

	uplinkToSource := c.getMacBindingSourceForUplinks()
	newFollowers := c.updateFollowers(addPorts, removePorts, portToUplink, uplinkToSource)

	// sync the dynamic mirror and static bindings of new followers. The static
	// side goes to the static reconciler so it serializes with removals.
	for follower := range newFollowers {
		c.enqueueFollowerDynamicMacBindings(follower)
		c.enqueueFollowerStaticMacBindings(follower)
	}

	// clean up node-IP static mac bindings of removed ports. A promoted source is
	// not cleaned: it keeps the bindings it inherited as a follower, maintained
	// update-only (see syncStaticMacBinding).
	for port := range removePorts {
		c.enqueueFollowerStaticMacBindings(port)
	}

	// repair after first full reconcile
	if fullReconcile {
		repair.Do(func() { c.repair(validPorts) })
	}

	return nil
}

// getAllNetworkInfo returns every tracked network and, into ports and
// portToUplink, each one's external GR port and the uplink it sits on.
func (c *MACBindingController) getAllNetworkInfo(ports sets.Set[string], portToUplink map[string]string) ([]string, error) {
	// pre-fill default network info, network manager won't iterate through it
	networks := []string{types.DefaultNetworkName}
	ports.Insert(c.cdnGatewayPort)
	portToUplink[c.cdnGatewayPort] = ""

	err := c.networkManager.DoWithLock(func(network util.NetInfo) error {
		if !shouldTrackNetwork(network) {
			return nil
		}
		networks = append(networks, network.GetNetworkName())
		port := types.GWRouterToExtSwitchPrefix + network.GetNetworkScopedGWRouterName(c.nodeName)
		ports.Insert(port)
		portToUplink[port] = network.Uplink()
		return nil
	})

	return networks, err
}

// getNetworksInfo fills ports and portToUplink with the external GR port and
// uplink of each named network that is tracked, skipping the rest.
func (c *MACBindingController) getNetworksInfo(networks []string, ports sets.Set[string], portToUplink map[string]string) error {
	for _, network := range networks {
		netInfo := c.networkManager.GetNetwork(network)
		if netInfo == nil || !shouldTrackNetwork(netInfo) {
			continue
		}
		port := util.GetNetworkScopedGWRouterExtPortName(network, c.nodeName)
		ports.Insert(port)
		portToUplink[port] = netInfo.Uplink()
	}
	return nil
}

// updateFollowers applies the added and removed ports to the follower map,
// assigning each port to the source on its uplink and parking those with no
// known source under unknownSource. It returns the ports newly made followers.
func (c *MACBindingController) updateFollowers(
	addPorts, removePorts sets.Set[string],
	portToUplink, uplinkToSource map[string]string,
) (newFollowers sets.Set[string]) {
	// prepare the inverse of uplinkToSource for convenience
	sourceToUplink := make(map[string]string, len(uplinkToSource))
	for uplink, source := range uplinkToSource {
		sourceToUplink[source] = uplink
	}

	// gather new ports by uplink, while also adding them to the ports map
	addPortsByUplink := map[string][]string{}
	for port := range addPorts {
		uplink := portToUplink[port]
		addPortsByUplink[uplink] = append(addPortsByUplink[uplink], port)
	}

	// track where ports land this round, for logging
	var logAddedBySource map[string][]string
	if klog.V(5).Enabled() {
		logAddedBySource = map[string][]string{}
	}

	c.Lock()
	defer c.Unlock()
	start := time.Now()

	newFollowers = sets.New[string]()
	removePortsList := removePorts.UnsortedList()
	followersWithNoSource := sets.New[string]()

	// big task: update followers map
	for source, followers := range c.followers {
		if source == unknownSource {
			// these are followers for which we don't know a source yet, handle
			// later
			continue
		}

		// remove followers
		followers.Delete(removePortsList...)

		// add followers on the same uplink as source
		uplink, isSource := sourceToUplink[source]
		if !isSource || removePorts.Has(source) {
			// source changed or removed, handle later
			followersWithNoSource.Insert(followers.UnsortedList()...)
			followersWithNoSource.Insert(source)
			delete(c.followers, source)
			continue
		}

		added := addPortsByUplink[uplink]
		followers.Insert(added...)
		followers.Delete(source)
		newFollowers.Insert(added...)
		newFollowers.Delete(source)
		delete(addPortsByUplink, uplink)

		if logAddedBySource != nil && len(added) > 0 {
			logAddedBySource[source] = sets.New(added...).Delete(source).UnsortedList()
		}
	}

	// Before handling new ports, treat followers with unknown source as if they
	// were new ports too. For the most part, the controller is only able to
	// relocate followers on full reconciliations where portToUplink has a
	// complete map of all networks.
	followersWithNoSource.Insert(c.followers[unknownSource].UnsortedList()...)
	followersWithNoSource.Delete(removePortsList...)
	delete(c.followers, unknownSource)
	for follower := range followersWithNoSource {
		uplink, knownUplink := portToUplink[follower]
		if !knownUplink {
			continue
		}
		addPortsByUplink[uplink] = append(addPortsByUplink[uplink], follower)
		delete(followersWithNoSource, follower)
	}

	// add new ports with known sources as followers
	for uplink, ports := range addPortsByUplink {
		source := uplinkToSource[uplink]
		portSet := sets.New(ports...)
		if source == "" || !portSet.Has(source) {
			// we don't have a source on the same uplink as this port
			followersWithNoSource.Insert(ports...)
			continue
		}

		c.followers[source] = portSet
		newFollowers.Insert(ports...)
		c.followers[source].Delete(source)
		newFollowers.Delete(source)

		if logAddedBySource != nil && len(ports) > 0 {
			logAddedBySource[source] = append(logAddedBySource[source], c.followers[source].UnsortedList()...)
		}
	}

	// if we still have ports with unknown sources, set them back as such
	if followersWithNoSource.Len() > 0 {
		c.followers[unknownSource] = followersWithNoSource
		if logAddedBySource != nil {
			logAddedBySource[unknownSource] = followersWithNoSource.UnsortedList()
		}
	}

	if logAddedBySource != nil {
		klog.V(5).Infof("Changing follower allocation took %s: added ports: %v, removed ports: %v",
			time.Since(start),
			logAddedBySource,
			removePorts.UnsortedList(),
		)
	}

	return newFollowers
}

// --- network predicates ------------------------------------------------

// shouldTrackNetwork reports whether a network takes part in MAC binding mirroring:
// a primary L2/L3 network present on this node.
func shouldTrackNetwork(netInfo util.NetInfo) bool {
	if netInfo == nil {
		return false
	}
	if !netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
		return false
	}
	if !shouldTrackTopology(netInfo.TopologyType()) {
		return false
	}
	return true
}

// shouldTrackTopology reports whether a network topology takes part in MAC
// binding mirroring.
func shouldTrackTopology(topology string) bool {
	return topology == types.Layer2Topology || topology == types.Layer3Topology
}
