// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	ovn_cooldown_period_ms = int((3.0 / 16) * types.GRMACBindingAgeThresholdInt * 1000)

	addWarnDelayThreshold       = 1000
	refreshWarnDelayThreshold   = ovn_cooldown_period_ms
	warnDelayThrottleIntervalMs = int64(time.Minute / time.Millisecond)
)

type macTimestamp struct {
	mac       string
	timestamp int
}

// syncDynamicMacBinding reads the source's (ip, mac) binding and mirrors it
// onto its followers, unless the ip is a node IP -- those are covered by static
// MAC bindings and must not be mirrored dynamically too. warnDelayThresholdMs
// is the source staleness beyond which the mirror warns about a backlog.
func (c *MACBindingController) syncDynamicMacBinding(source, ip string, warnDelayThresholdMs int) error {
	if !ipFamilyEnabled(ip) {
		// skip disabled IP families, and node IPs which are covered by static
		// MAC bindings and must not be mirrored dynamically too
		return nil
	}
	start := time.Now()
	followers := c.getFollowers(source)
	if len(followers) == 0 {
		return nil
	}

	mb := &sbdb.MACBinding{LogicalPort: source, IP: ip}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	err := c.sbClient.Get(ctx, mb)
	cancel()
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		c.throttleBacklogWarning("MAC_Binding for %s on source port %s missing", ip, source)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get MAC_Binding for port %q IP %q: %w", source, ip, err)
	}

	delayBeforeTxn := int(time.Now().UnixMilli()) - mb.Timestamp
	if err := c.setDynamicMacBindingsOnFollowers(map[string]macTimestamp{mb.IP: {mac: mb.MAC, timestamp: mb.Timestamp}}, followers); err != nil {
		return fmt.Errorf("failed to mirror %s from source %s: %w", ip, source, err)
	}

	if delay := int(time.Now().UnixMilli()) - mb.Timestamp; delay > warnDelayThresholdMs {
		c.throttleBacklogWarning("MAC_Binding for %s on source port %s is being mirrored with %dms delay (over %dms warning threshold) including %dms transaction time",
			ip,
			source,
			delay,
			warnDelayThresholdMs,
			delay-delayBeforeTxn,
		)
	}
	klog.V(5).Infof("Mirroring %s from %s to %d follower(s) took %s", ip, source, len(followers), time.Since(start))
	return nil
}

// syncDynamicMacBindingsToFollower mirrors the dynamic mac bindings from a
// follower's source onto it.
func (c *MACBindingController) syncDynamicMacBindingsToFollower(follower string) error {
	source := c.getValidSourceForFollower(follower)
	if source == "" {
		return nil
	}
	if err := c.syncDynamicMacBindingsFromSourceToFollower(source, follower); err != nil {
		return fmt.Errorf("failed to mirror mac bindings for follower %s: %w", follower, err)
	}
	return nil
}

// syncDynamicMacBindingsFromSourceToFollower mirrors all of the source's
// current MAC bindings onto a single follower. Used when a follower is newly
// allocated to a source and needs to catch up on the bindings already resolved
// there.
func (c *MACBindingController) syncDynamicMacBindingsFromSourceToFollower(source, follower string) error {
	start := time.Now()
	// find all mac bindings for the source port
	mb := &sbdb.MACBinding{LogicalPort: source}
	mbs := []*sbdb.MACBinding{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	err := c.sbClient.Where(mb).List(ctx, &mbs)
	cancel()
	if err != nil {
		return fmt.Errorf("failed to list MAC_Bindings for port %q: %w", source, err)
	}
	if len(mbs) == 0 {
		return nil
	}
	macBindings := map[string]macTimestamp{}
	for _, mb := range mbs {
		if !ipFamilyEnabled(mb.IP) {
			// don't mirror MAC bindings for a disabled IP family
			continue
		}
		macBindings[mb.IP] = macTimestamp{mac: mb.MAC, timestamp: mb.Timestamp}
	}

	if err := c.setDynamicMacBindingsOnFollowers(macBindings, []string{follower}); err != nil {
		return err
	}
	klog.V(5).Infof("Mirroring IPs from %s to follower %s took %s", source, follower, time.Since(start))
	return nil
}

// setDynamicMacBindingsOnFollowers mirrors the given (ip, mac) bindings onto
// every follower port, resolving each follower's datapath.
func (c *MACBindingController) setDynamicMacBindingsOnFollowers(macBindings map[string]macTimestamp, followers []string) error {
	portToDatapath := map[string]string{}
	for _, follower := range followers {
		pb, err := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: follower})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to get port binding for port %q: %w", follower, err)
		}
		if pb.Datapath == "" {
			continue
		}
		portToDatapath[follower] = pb.Datapath
	}
	if len(portToDatapath) == 0 {
		return nil
	}

	return c.setDynamicMacBindings(macBindings, portToDatapath)
}

// setDynamicMacBindings mirrors (ip, mac) as a MAC_Binding row for each target
// port in a single transaction. A row that does not yet exist is created; a row
// whose MAC changed is always updated; an unchanged row is updated only when
// the mirrored timestamp is newer than the row's own, so mirrored writes don't
// regress a row the that may have already been refreshed by OVN.
func (c *MACBindingController) setDynamicMacBindings(macBindings map[string]macTimestamp, portToDatapath map[string]string) error {
	allOps := make([]ovsdb.Operation, 0, len(portToDatapath)*len(macBindings))
	var skips, addCount, updateCount int
	for port, datapath := range portToDatapath {
		for ip, macBinding := range macBindings {
			mb := &sbdb.MACBinding{LogicalPort: port, IP: ip}
			var op []ovsdb.Operation
			ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
			err := c.sbClient.Get(ctx, mb)
			cancel()
			switch {
			case errors.Is(err, libovsdbclient.ErrNotFound):
				addCount++
				op, err = c.sbClient.Create(&sbdb.MACBinding{
					LogicalPort: port,
					IP:          ip,
					MAC:         macBinding.mac,
					Datapath:    datapath,
					Timestamp:   macBinding.timestamp,
				})
			case err == nil:
				mbUpdate := &sbdb.MACBinding{Timestamp: macBinding.timestamp}
				switch {
				case macBinding.mac != mb.MAC:
					mbUpdate.MAC = macBinding.mac
				case mb.Timestamp >= macBinding.timestamp:
					skips++
					continue
				}
				updateCount++
				op, err = c.sbClient.Where(mb).Update(mbUpdate)
			}
			if err != nil {
				return err
			}
			allOps = append(allOps, op...)
		}
	}
	if len(allOps) == 0 {
		return nil
	}
	start := time.Now()
	_, err := ops.TransactAndCheck(c.sbClient, allOps)
	if err != nil {
		return err
	}
	klog.V(5).Infof("Took %s to execute a %d op(s) transaction to set %d mac bindings on %d ports with %d adds, %d updates and %d skipped",
		time.Since(start),
		len(allOps),
		len(macBindings),
		len(portToDatapath),
		addCount,
		updateCount,
		skips,
	)
	return nil
}

// validatePorts returns the subset of ports that currently exist in SBDB (have
// a Port_Binding).
func (c *MACBindingController) validatePorts(ports sets.Set[string]) (sets.Set[string], error) {
	validPorts := sets.New[string]()
	for port := range ports {
		_, err := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: port})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get port binding for port %q: %w", port, err)
		}
		validPorts.Insert(port)
	}
	return validPorts, nil
}

// registerSouthBoundEventHandlers subscribes to the SBDB cache for the two events
// that drive mirroring: a MAC_Binding add/update on a tracked source's GR external
// port enqueues a mirror (a refresh when the MAC is unchanged, a fresh mirror
// otherwise), and a Port_Binding add/delete on a GR external port enqueues its
// network for reconcile.
func (c *MACBindingController) registerSouthBoundEventHandlers() {
	handleMacBinding := func(m, o model.Model) {
		mb := m.(*sbdb.MACBinding)
		if !strings.HasPrefix(mb.LogicalPort, types.GWRouterToExtSwitchPrefix) {
			return
		}
		if !ipFamilyEnabled(mb.IP) {
			return
		}
		if !c.tracksSourceWithFollowers(mb.LogicalPort) {
			return
		}
		var oldMAC string
		if o != nil {
			oldMAC = o.(*sbdb.MACBinding).MAC
		}
		switch mb.MAC {
		case oldMAC:
			c.enqueueDynamicMacBindingRefresh(mb.LogicalPort, mb.IP)
		default:
			c.enqueueDynamicMacBinding(mb.LogicalPort, mb.IP)
		}
	}
	handlePortBinding := func(m model.Model) {
		pb := m.(*sbdb.PortBinding)
		if !strings.HasPrefix(pb.LogicalPort, types.GWRouterToExtSwitchPrefix) {
			return
		}
		network := pb.ExternalIDs[types.NetworkExternalID]
		topology := pb.ExternalIDs[types.TopologyExternalID]
		if network != "" && !shouldTrackTopology(topology) {
			return
		}
		if pb.LogicalPort == c.cdnGatewayPort {
			network = types.DefaultNetworkName
		}
		if network == "" {
			return
		}
		c.enqueueNetwork(network)
	}
	handle := func(op, table string, m, o model.Model) {
		switch {
		case table == sbdb.MACBindingTable && op != "d":
			handleMacBinding(m, o)
		case table == sbdb.PortBindingTable && op != "u":
			handlePortBinding(m)
		}
	}
	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc:    func(table string, model model.Model) { handle("a", table, model, nil) },
		UpdateFunc: func(table string, old, new model.Model) { handle("u", table, new, old) },
		DeleteFunc: func(table string, model model.Model) { handle("d", table, model, nil) },
	})
}

// throttleBacklogWarning emits a mirror-backlog warning, at most one per
// warnDelayThrottleIntervalMs.
func (c *MACBindingController) throttleBacklogWarning(format string, args ...any) {
	if !c.allowBacklogWarning(time.Now().UnixMilli()) {
		return
	}
	klog.Warningf("Excessive delay mirroring MAC bindings (muted for the next %dms): "+format,
		append([]any{warnDelayThrottleIntervalMs}, args...)...,
	)
}

// allowBacklogWarning reports whether enough time has elapsed since the last
// backlog warning to emit another, recording nowMs as the new emission time when
// it returns true. It is safe for concurrent callers.
func (c *MACBindingController) allowBacklogWarning(nowMs int64) bool {
	last := c.lastBacklogWarningMs.Load()
	if nowMs-last < warnDelayThrottleIntervalMs {
		return false
	}
	// If a concurrent caller won the race and already recorded a newer emission,
	// let it own this interval's warning.
	return c.lastBacklogWarningMs.CompareAndSwap(last, nowMs)
}

// ipFamilyEnabled reports whether ip's address family is enabled on this cluster.
// MAC bindings for a disabled family are neither mirrored nor enqueued.
func ipFamilyEnabled(ip string) bool {
	if utilnet.IsIPv6String(ip) {
		return config.IPv6Mode
	}
	return config.IPv4Mode
}
