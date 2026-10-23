// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// repairStaticMacBindings deletes stale static mac bindings owned by the
// controller for non-existant networks.
func (c *MACBindingController) repairStaticMacBindings(knownPorts sets.Set[string]) {
	start := time.Now()
	stale := func(smb *nbdb.StaticMACBinding) bool {
		return !knownPorts.Has(smb.LogicalPort) &&
			ops.IsMacBindingControllerOwned(smb.LogicalPort, smb.IP)
	}
	if err := ops.DeleteStaticMACBindingWithPredicate(c.nbClient, stale); err != nil {
		klog.Errorf("Failed to delete stale static MAC bindings: %v", err)
		return
	}
	klog.V(5).Infof("Deleted stale static MAC bindings, took %s", time.Since(start))
}

// syncStaticMacBinding (re)applies or removes a single node IP's static MAC
// binding across the uplink's non-CDN ports: ensured (add+update) on followers,
// update-only on the source, which keeps the bindings it might have inherited
// as a follower current but is never given new ones (so a source that was never
// a follower stays untouched and resolves dynamically, and we don't remove mac
// bindings of source that promoted from follower to prevent network blips). The
// CDN is never a target.
func (c *MACBindingController) syncStaticMacBinding(uplink, ip string) error {
	source := c.getMacBindingSourceForUplinks()[uplink]
	followers := c.getFollowers(source)
	var updateOnly []string
	if source != "" && source != c.cdnGatewayPort {
		updateOnly = []string{source}
	}
	if len(followers) == 0 && len(updateOnly) == 0 {
		return nil
	}
	start := time.Now()
	mac, ok := c.getNodeIPMAC(uplink, ip)
	if ok {
		if err := c.ensureStaticMacBindings(map[string]string{ip: mac}, followers); err != nil {
			return err
		}
		if err := c.updateStaticMacBindings(map[string]string{ip: mac}, updateOnly); err != nil {
			return err
		}
	} else {
		if err := c.deleteStaticMacBindings([]string{ip}, append(followers, updateOnly...)); err != nil {
			return err
		}
	}
	klog.V(5).Infof("Syncing node IP %s static MAC binding (present=%t) on %d follower(s) for uplink %q took %s",
		ip, ok, len(followers), uplink, time.Since(start))
	return nil
}

// syncFollowerStaticMacBindings ensures or removes a port's node-IP static
// bindings. A non-default uplink source keeps the bindings it might have
// inherited as a follower but is never given new ones (so a source that was
// never a follower stays untouched and resolves dynamically, and we don't
// remove mac bindings of source that promoted from follower to prevent network
// blips). The CDN is never a target.
func (c *MACBindingController) syncFollowerStaticMacBindings(follower string) error {
	source := c.getSourceForPort(follower)
	if source == follower || source == unknownSource {
		return nil
	}
	if source == "" {
		// not a known follower: drop any static bindings it may have had. The
		// delete no-ops if it is still tracked.
		return c.deleteStaticMacBindingsForFollower(follower)
	}
	return c.syncStaticMacBindingsToFollower(source, follower)
}

// syncStaticMacBindingsToFollower applies all of the source uplink's node-IP
// static bindings on a single follower.
func (c *MACBindingController) syncStaticMacBindingsToFollower(source, follower string) error {
	start := time.Now()

	uplink, ok := c.getUplinkForSource(source)
	if !ok {
		return nil
	}
	macBindings := c.getNodeIPMACs(uplink)

	// cleanup unknown mac bindings first
	if err := c.reapStaticMacBindings(follower, macBindings); err != nil {
		return err
	}

	if len(macBindings) == 0 {
		return nil
	}
	if err := c.ensureStaticMacBindings(macBindings, []string{follower}); err != nil {
		return err
	}
	klog.V(5).Infof("Setting %d node IP static MAC binding(s) on follower %s took %s",
		len(macBindings), follower, time.Since(start))

	return nil
}

// deleteStaticMacBindingsForFollower removes the node-IP static MAC bindings on
// a port that is no longer a follower: the controller owns none of them now, so
// every binding it did not leave to the gateway is reaped.
func (c *MACBindingController) deleteStaticMacBindingsForFollower(follower string) error {
	return c.reapStaticMacBindings(follower, nil)
}

// reapStaticMacBindings deletes every static MAC binding on port that the
// controller owns but no longer recognizes as current (IP not in cache). The
// Ownership is based on a a whitelist (ops.IsMacBindingControllerOwned) due to
// the lack of externalIDs column in the StaticMACBinding table.
func (c *MACBindingController) reapStaticMacBindings(port string, owned map[string]string) error {
	start := time.Now()
	// fetch all static MAC bindings on the port via the logical_port secondary
	// index, then keep only those the controller neither owns nor left to the
	// gateway.
	smbs := []*nbdb.StaticMACBinding{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	if err := c.nbClient.Where(&nbdb.StaticMACBinding{LogicalPort: port}).List(ctx, &smbs); err != nil {
		return fmt.Errorf("failed to list static MAC bindings on port %s: %w", port, err)
	}
	stale := smbs[:0]
	for _, smb := range smbs {
		if _, current := owned[smb.IP]; current {
			continue
		}
		if ops.IsMacBindingControllerOwned(smb.LogicalPort, smb.IP) {
			stale = append(stale, smb)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	if err := ops.DeleteStaticMacBindings(c.nbClient, stale...); err != nil {
		return fmt.Errorf("failed to reap static MAC bindings on port %s: %w", port, err)
	}
	klog.V(5).Infof("Reaped %d static MAC binding(s) on port %s took %s",
		len(stale), port, time.Since(start))
	return nil
}

// ensureStaticMacBindings creates or updates the static MAC binding for each
// (ip, mac) on each port.
func (c *MACBindingController) ensureStaticMacBindings(macBindings map[string]string, ports []string) error {
	return c.setStaticMacBindings(macBindings, ports, false)
}

// updateStaticMacBindings updates the static MAC binding for each (ip, mac) on
// each port only if it already exists.
func (c *MACBindingController) updateStaticMacBindings(macBindings map[string]string, ports []string) error {
	return c.setStaticMacBindings(macBindings, ports, true)
}

// setStaticMacBindings writes a static MAC binding for each (ip, mac) on each
// port in a single transaction, skipping unchanged rows. With inhibitAdd set it
// only updates rows that already exist.
func (c *MACBindingController) setStaticMacBindings(macBindings map[string]string, ports []string, inhibitAdd bool) error {
	allOps := make([]ovsdb.Operation, 0, len(ports)*len(macBindings))
	var addCount, updateCount, skips int
	for _, port := range ports {
		for ip, mac := range macBindings {
			smb := &nbdb.StaticMACBinding{LogicalPort: port, IP: ip}
			var op []ovsdb.Operation
			ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
			err := c.nbClient.Get(ctx, smb)
			cancel()
			switch {
			case errors.Is(err, libovsdbclient.ErrNotFound):
				if inhibitAdd {
					skips++
					continue
				}
				addCount++
				op, err = c.nbClient.Create(&nbdb.StaticMACBinding{
					LogicalPort:        port,
					IP:                 ip,
					MAC:                mac,
					OverrideDynamicMAC: true,
				})
			case err == nil:
				if smb.MAC == mac && smb.OverrideDynamicMAC {
					skips++
					continue
				}
				updateCount++
				update := &nbdb.StaticMACBinding{OverrideDynamicMAC: true}
				if smb.MAC != mac {
					update.MAC = mac
				}
				op, err = c.nbClient.Where(smb).Update(update)
			}
			if err != nil {
				return fmt.Errorf("failed to build static MAC binding op for %s on %s: %w", ip, port, err)
			}
			allOps = append(allOps, op...)
		}
	}
	if len(allOps) == 0 {
		return nil
	}
	if _, err := ops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to set static MAC bindings on ports: %w", err)
	}
	klog.V(5).Infof("Set static MAC bindings on %d port(s): %d added, %d updated, %d unchanged",
		len(ports), addCount, updateCount, skips)
	return nil
}

// deleteStaticMacBindings removes the static MAC binding for each ip from each
// port.
func (c *MACBindingController) deleteStaticMacBindings(ips []string, ports []string) error {
	smbs := make([]*nbdb.StaticMACBinding, 0, len(ports)*len(ips))
	for _, port := range ports {
		for _, ip := range ips {
			smbs = append(smbs, &nbdb.StaticMACBinding{
				LogicalPort: port,
				IP:          ip,
			})
		}
	}
	if len(smbs) == 0 {
		return nil
	}
	if err := ops.DeleteStaticMacBindings(c.nbClient, smbs...); err != nil {
		return fmt.Errorf("failed to delete static MAC bindings on ports: %w", err)
	}
	return nil
}
