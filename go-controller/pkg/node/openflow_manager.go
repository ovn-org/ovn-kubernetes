// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	libovsdbmodel "github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type openflowManager struct {
	defaultBridge         *openflowBridge
	externalGatewayBridge *openflowBridge
	uplinkBridgesMu       sync.Mutex
	uplinkBridges         map[string]*openflowBridge
	staticFlowsMu         sync.Mutex
	staticFlowsSet        bool
	staticFlowHostIPs     []net.IP
	staticFlowHostSubnets []*net.IPNet
	// localnetPortChan coalesces OVSDB events that may change whether a managed
	// bridge has a localnet topology patch port.
	localnetPortChan chan struct{}
	// channel to indicate we need to update flows immediately
	flowChan  chan struct{}
	ovsClient libovsdbclient.Client
	// portCheckFailed lists the uplink bridges whose last port check failed;
	// their flows are not synced until a check passes.
	portCheckFailed map[string]struct{}
}

type openflowBridge struct {
	*bridgeconfig.BridgeConfiguration
	// flow cache, use map instead of array for readability when debugging
	flowCache       map[string][]string
	flowMutex       sync.Mutex
	groupCache      map[string][]string
	groupMutex      sync.Mutex
	installedGroups map[string]struct{}
	syncMutex       sync.Mutex
}

// defaultOpenFlowBridgeSetName is an internal target name, not an OVS bridge.
// It means the default bridge and, when configured, the external gateway bridge.
// Its length prevents it from colliding with a Linux interface name.
const defaultOpenFlowBridgeSetName = "<default-bridge-set>"

func openflowBridgeTargetDisplayName(targetName string) string {
	if targetName == defaultOpenFlowBridgeSetName {
		return "default bridge set"
	}
	return targetName
}

func newOpenflowBridge(bridge *bridgeconfig.BridgeConfiguration) *openflowBridge {
	return &openflowBridge{
		BridgeConfiguration: bridge,
		flowCache:           make(map[string][]string),
		groupCache:          make(map[string][]string),
		installedGroups:     make(map[string]struct{}),
	}
}

var openFlowGroupIDRegexp = regexp.MustCompile(`(?:^|,)group_id=([^,]+)`)

const localnetPortEventDebounce = 100 * time.Millisecond

// UTILs Needed for UDN (also leveraged for default netInfo) in openflowmanager

func (c *openflowManager) getDefaultBridgePortConfigurations() ([]*bridgeconfig.BridgeUDNConfiguration, string, string) {
	return c.defaultBridge.GetPortConfigurations()
}

func (c *openflowManager) getExGwBridgePortConfigurations() ([]*bridgeconfig.BridgeUDNConfiguration, string, string) {
	return c.externalGatewayBridge.GetPortConfigurations()
}

type bridgePortConfigurations struct {
	netConfigs []*bridgeconfig.BridgeUDNConfiguration
	physIntf   string
	ofPortPhys string
}

func (c *openflowManager) forEachUplinkBridge(fn func(bridgeName string, bridge *openflowBridge) error) error {
	for bridgeName, bridge := range c.uplinkBridgeSnapshot() {
		if err := fn(bridgeName, bridge); err != nil {
			return err
		}
	}
	return nil
}

func (c *openflowManager) uplinkBridgeSnapshot() map[string]*openflowBridge {
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()

	bridges := make(map[string]*openflowBridge, len(c.uplinkBridges))
	for bridgeName, bridge := range c.uplinkBridges {
		bridges[bridgeName] = bridge
	}
	return bridges
}

func (c *openflowManager) getUplinkBridgePortConfigurations() map[string]bridgePortConfigurations {
	configs := make(map[string]bridgePortConfigurations)
	_ = c.forEachUplinkBridge(func(bridgeName string, bridge *openflowBridge) error {
		netConfigs, physIntf, ofPortPhys := bridge.GetPortConfigurations()
		configs[bridgeName] = bridgePortConfigurations{
			netConfigs: netConfigs,
			physIntf:   physIntf,
			ofPortPhys: ofPortPhys,
		}
		return nil
	})
	return configs
}

func (c *openflowManager) addNetwork(bridgeName string, bridge *bridgeconfig.BridgeConfiguration, nInfo util.NetInfo,
	nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint, v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	if bridge != nil {
		return c.addNetworkToUplinkBridge(bridgeName, bridge, nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark,
			v6MasqIPs, v4MasqIPs)
	}
	return c.addNetworkToDefaultBridgeSet(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs)
}

func (c *openflowManager) addNetworkToDefaultBridgeSet(nInfo util.NetInfo, nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint, v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	if err := c.defaultBridge.AddNetworkConfig(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs); err != nil {
		return err
	}
	if c.externalGatewayBridge != nil {
		if err := c.externalGatewayBridge.AddNetworkConfig(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs); err != nil {
			return err
		}
	}
	return nil
}

func (c *openflowManager) addNetworkToUplinkBridge(bridgeName string, bridge *bridgeconfig.BridgeConfiguration,
	nInfo util.NetInfo, nodeSubnets, mgmtIPs []*net.IPNet, masqCTMark, pktMark uint,
	v6MasqIPs, v4MasqIPs *udn.MasqueradeIPs) error {
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()

	uplinkBridge, found := c.uplinkBridges[bridgeName]
	if !found {
		uplinkBridge = newOpenflowBridge(bridge)
		c.uplinkBridges[bridgeName] = uplinkBridge
	}
	return uplinkBridge.AddNetworkConfig(nInfo, nodeSubnets, mgmtIPs, masqCTMark, pktMark, v6MasqIPs, v4MasqIPs)
}

func (c *openflowManager) delNetwork(nInfo util.NetInfo, targetName string) error {
	if targetName != defaultOpenFlowBridgeSetName {
		return c.delNetworkFromUplinkBridge(nInfo, targetName)
	}
	c.delNetworkFromDefaultBridgeSet(nInfo)
	return nil
}

func (c *openflowManager) delNetworkFromDefaultBridgeSet(nInfo util.NetInfo) {
	c.defaultBridge.DelNetworkConfig(nInfo)
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.DelNetworkConfig(nInfo)
	}
}

func (c *openflowManager) delNetworkFromUplinkBridge(nInfo util.NetInfo, bridgeName string) error {
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()

	bridge, found := c.uplinkBridges[bridgeName]
	if !found {
		return nil
	}
	bridge.DelNetworkConfig(nInfo)
	if bridge.HasNetworkConfigs() {
		return nil
	}

	bridge.resetFlowCacheToNormal()
	if err := bridge.syncFlows(); err != nil {
		// An admin may delete the Uplink bridge before its last network is
		// torn down: nothing is left to clean up, stop tracking it.
		if _, lookupErr := ovsops.GetBridge(c.ovsClient, bridgeName); errors.Is(lookupErr, libovsdbclient.ErrNotFound) {
			klog.Infof("Uplink bridge %s no longer exists, treating its flows as cleaned up", bridgeName)
			delete(c.uplinkBridges, bridgeName)
			return nil
		}
		return fmt.Errorf("failed to clean up unused uplink bridge %s flows: %w", bridgeName, err)
	}
	delete(c.uplinkBridges, bridgeName)
	return nil
}

func (c *openflowManager) setNetworkOfPatchPort(targetName, networkName string) error {
	if targetName == defaultOpenFlowBridgeSetName {
		if err := c.defaultBridge.SetNetworkOfPatchPort(networkName); err != nil {
			return fmt.Errorf("failed to set default bridge patch port: %w", err)
		}
		if c.externalGatewayBridge != nil {
			if err := c.externalGatewayBridge.SetNetworkOfPatchPort(networkName); err != nil {
				return fmt.Errorf("failed to set external gateway bridge patch port: %w", err)
			}
		}
		return nil
	}

	bridge, found := c.getUplinkBridge(targetName)
	if !found {
		return fmt.Errorf("uplink bridge %s not found", targetName)
	}
	if err := bridge.SetNetworkOfPatchPort(networkName); err != nil {
		return fmt.Errorf("failed to set uplink bridge %s patch port: %w", targetName, err)
	}
	return nil
}

func (c *openflowManager) getUplinkBridge(bridgeName string) (*openflowBridge, bool) {
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()
	bridge, found := c.uplinkBridges[bridgeName]
	return bridge, found
}

func (c *openflowManager) syncUplinkBridgeFlows(bridgeName string) (bool, error) {
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()

	bridge, found := c.uplinkBridges[bridgeName]
	if !found {
		return false, nil
	}
	if _, failed := c.portCheckFailed[bridgeName]; failed {
		// Reported as not found: a gateway programming the bridge retries,
		// while a network leaving it only needs its configuration dropped.
		klog.Errorf("Skipping flow sync for bridge %s because its port check failed", bridgeName)
		return false, nil
	}
	if err := bridge.syncFlows(); err != nil {
		return true, fmt.Errorf("failed to sync flows for uplink bridge %s: %w", bridgeName, err)
	}
	return true, nil
}

func (c *openflowManager) doWithUplinkBridgeForNetwork(nInfo util.NetInfo, fn func(*openflowBridge) error) (bool, error) {
	foundBridge := false
	err := c.forEachUplinkBridge(func(_ string, bridge *openflowBridge) error {
		if foundBridge {
			return nil
		}
		if bridge.GetNetworkConfig(nInfo.GetNetworkName()) != nil {
			foundBridge = true
			return fn(bridge)
		}
		return nil
	})
	return foundBridge, err
}

func (c *openflowManager) getNetworkConfig(nInfo util.NetInfo) *bridgeconfig.BridgeUDNConfiguration {
	if netConfig := c.defaultBridge.GetNetworkConfig(nInfo.GetNetworkName()); netConfig != nil {
		return netConfig
	}
	var netConfig *bridgeconfig.BridgeUDNConfiguration
	_, _ = c.doWithUplinkBridgeForNetwork(nInfo, func(bridge *openflowBridge) error {
		netConfig = bridge.GetNetworkConfig(nInfo.GetNetworkName())
		return nil
	})
	return netConfig
}

func (c *openflowManager) getBridgeForNetwork(nInfo util.NetInfo) *bridgeconfig.BridgeConfiguration {
	var bridgeConfig *bridgeconfig.BridgeConfiguration
	bridgeFound, _ := c.doWithUplinkBridgeForNetwork(nInfo, func(bridge *openflowBridge) error {
		bridgeConfig = bridge.BridgeConfiguration
		return nil
	})
	if bridgeFound {
		return bridgeConfig
	}
	return c.defaultBridge.BridgeConfiguration
}

func (c *openflowManager) getActiveNetwork(nInfo util.NetInfo) *bridgeconfig.BridgeUDNConfiguration {
	if netConfig := c.defaultBridge.GetActiveNetworkBridgeConfigCopy(nInfo.GetNetworkName()); netConfig != nil {
		return netConfig
	}
	var netConfig *bridgeconfig.BridgeUDNConfiguration
	_, _ = c.doWithUplinkBridgeForNetwork(nInfo, func(bridge *openflowBridge) error {
		netConfig = bridge.GetActiveNetworkBridgeConfigCopy(nInfo.GetNetworkName())
		return nil
	})
	return netConfig
}

// END UDN UTILs

func (c *openflowManager) getDefaultBridgeName() string {
	return c.defaultBridge.GetBridgeName()
}

func (c *openflowManager) getDefaultBridgeMAC() net.HardwareAddr {
	return c.defaultBridge.GetMAC()
}

func (c *openflowManager) setDefaultBridgeMAC(macAddr net.HardwareAddr) {
	c.defaultBridge.SetMAC(macAddr)
}

// setDefaultBridgeGARPDrop is used to enable or disable whether openflow manager generates ovs flows and adds them to
// the default ext bridge to drop GARP
func (c *openflowManager) setDefaultBridgeGARPDrop(isDropped bool) {
	c.defaultBridge.SetDropGARP(isDropped)
}

func (b *openflowBridge) updateFlowCacheEntry(key string, flows []string) {
	b.flowMutex.Lock()
	defer b.flowMutex.Unlock()
	if b.flowCache == nil {
		b.flowCache = make(map[string][]string)
	}
	b.flowCache[key] = flows
}

func (b *openflowBridge) deleteFlowsByKey(key string) {
	b.flowMutex.Lock()
	delete(b.flowCache, key)
	b.flowMutex.Unlock()
	b.deleteGroupsByKey(key)
}

func (b *openflowBridge) getFlowsByKey(key string) []string {
	b.flowMutex.Lock()
	defer b.flowMutex.Unlock()
	return b.flowCache[key]
}

func (b *openflowBridge) updateGroupCacheEntry(key string, groups []string) {
	b.groupMutex.Lock()
	defer b.groupMutex.Unlock()
	if len(groups) == 0 {
		delete(b.groupCache, key)
		return
	}
	if b.groupCache == nil {
		b.groupCache = make(map[string][]string)
	}
	b.groupCache[key] = groups
}

func (b *openflowBridge) deleteGroupsByKey(key string) {
	b.groupMutex.Lock()
	defer b.groupMutex.Unlock()
	delete(b.groupCache, key)
}

func (b *openflowBridge) getGroupsByKey(key string) []string {
	b.groupMutex.Lock()
	defer b.groupMutex.Unlock()
	return b.groupCache[key]
}

func (b *openflowBridge) resetFlowCacheToNormal() {
	b.flowMutex.Lock()
	b.flowCache = map[string][]string{
		"NORMAL": {fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)},
	}
	b.flowMutex.Unlock()

	b.groupMutex.Lock()
	b.groupCache = map[string][]string{}
	b.groupMutex.Unlock()
}

func (b *openflowBridge) flows() []string {
	b.flowMutex.Lock()
	defer b.flowMutex.Unlock()
	return flattenFlowCacheEntries(b.flowCache)
}

func (c *openflowManager) updateFlowCacheEntry(key string, flows []string) {
	c.defaultBridge.updateFlowCacheEntry(key, flows)
}

func (c *openflowManager) deleteFlowsByKey(key string) {
	c.defaultBridge.deleteFlowsByKey(key)
}

func (c *openflowManager) getFlowsByKey(key string) []string {
	return c.defaultBridge.getFlowsByKey(key)
}

func (c *openflowManager) updateGroupCacheEntry(key string, groups []string) {
	c.defaultBridge.updateGroupCacheEntry(key, groups)
}

func (c *openflowManager) getGroupsByKey(key string) []string {
	return c.defaultBridge.getGroupsByKey(key)
}

func (c *openflowManager) updateExBridgeFlowCacheEntry(key string, flows []string) {
	c.externalGatewayBridge.updateFlowCacheEntry(key, flows)
}

func (c *openflowManager) updateNetworkFlowCacheEntry(nInfo util.NetInfo, key string, flows []string) {
	updated, _ := c.doWithUplinkBridgeForNetwork(nInfo, func(bridge *openflowBridge) error {
		bridge.updateFlowCacheEntry(key, flows)
		return nil
	})
	if updated {
		return
	}
	c.updateFlowCacheEntry(key, flows)
}

func (c *openflowManager) updateNetworkGroupCacheEntry(nInfo util.NetInfo, key string, groups []string) {
	updated, _ := c.doWithUplinkBridgeForNetwork(nInfo, func(bridge *openflowBridge) error {
		bridge.updateGroupCacheEntry(key, groups)
		return nil
	})
	if updated {
		return
	}
	c.updateGroupCacheEntry(key, groups)
}

func (c *openflowManager) deleteNetworkFlowsByKey(key string) {
	c.deleteFlowsByKey(key)
	_ = c.forEachUplinkBridge(func(_ string, bridge *openflowBridge) error {
		bridge.deleteFlowsByKey(key)
		return nil
	})
}

func (c *openflowManager) requestFlowSync() {
	select {
	case c.flowChan <- struct{}{}:
		klog.V(5).Infof("Gateway OpenFlow sync requested")
	default:
		klog.V(5).Infof("Gateway OpenFlow sync already requested")
	}
}

// syncFlows syncs the flows of every managed bridge, skipping the uplink
// bridges whose last port check failed.
func (c *openflowManager) syncFlows() {
	if err := c.defaultBridge.syncFlows(); err != nil {
		klog.Errorf("Failed to sync flows for bridge %s: %v",
			c.defaultBridge.GetBridgeName(), err)
	}

	if c.externalGatewayBridge != nil {
		if err := c.externalGatewayBridge.syncFlows(); err != nil {
			klog.Errorf("Failed to sync flows for bridge %s: %v",
				c.externalGatewayBridge.GetBridgeName(), err)
		}
	}

	c.uplinkBridgesMu.Lock()
	failed := c.portCheckFailed
	c.uplinkBridgesMu.Unlock()
	_ = c.forEachUplinkBridge(func(bridgeName string, bridge *openflowBridge) error {
		if _, skip := failed[bridgeName]; skip {
			klog.Errorf("Skipping flow sync for bridge %s because its port check failed", bridgeName)
			return nil
		}
		if err := bridge.syncFlows(); err != nil {
			klog.Errorf("Failed to sync flows for bridge %s: %v",
				bridge.GetBridgeName(), err)
		}
		return nil
	})
}

func (b *openflowBridge) syncFlows() error {
	b.syncMutex.Lock()
	defer b.syncMutex.Unlock()

	if !b.syncGroups() {
		return fmt.Errorf("openflow group sync failed")
	}
	flows := b.flows()
	_, stderr, err := util.ReplaceOFFlows(b.GetBridgeName(), flows)
	if err != nil {
		return fmt.Errorf("failed to replace OpenFlow flows, stderr: %s, flow count: %d: %w",
			stderr, len(flows), err)
	}
	b.deleteStaleGroups()
	if err := b.SyncNoFlood(); err != nil {
		return fmt.Errorf("failed to sync no-flood port config: %w", err)
	}
	return nil
}

func (b *openflowBridge) syncGroups() bool {
	b.groupMutex.Lock()
	groups := flattenFlowCacheEntries(b.groupCache)
	b.groupMutex.Unlock()

	success := true
	successfulGroupIDs := make([]string, 0, len(groups))
	for _, group := range groups {
		_, stderr, err := util.AddOrModOFGroup(b.GetBridgeName(), group)
		if err != nil {
			klog.Errorf("Failed to add or modify OpenFlow group for bridge %s, error: %v, stderr: %s, group: %s",
				b.GetBridgeName(), err, stderr, group)
			success = false
			continue
		}
		if groupID, ok := parseOpenFlowGroupID(group); ok {
			successfulGroupIDs = append(successfulGroupIDs, groupID)
		}
	}
	if len(successfulGroupIDs) > 0 {
		b.groupMutex.Lock()
		if b.installedGroups == nil {
			b.installedGroups = make(map[string]struct{})
		}
		for _, groupID := range successfulGroupIDs {
			b.installedGroups[groupID] = struct{}{}
		}
		b.groupMutex.Unlock()
	}
	return success
}

func (b *openflowBridge) deleteStaleGroups() {
	b.groupMutex.Lock()
	groups := flattenFlowCacheEntries(b.groupCache)
	desiredGroupIDs := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if groupID, ok := parseOpenFlowGroupID(group); ok {
			desiredGroupIDs[groupID] = struct{}{}
		}
	}
	if b.installedGroups == nil {
		b.installedGroups = make(map[string]struct{})
	}
	staleGroupIDs := make([]string, 0, len(b.installedGroups))
	for groupID := range b.installedGroups {
		if _, ok := desiredGroupIDs[groupID]; !ok {
			staleGroupIDs = append(staleGroupIDs, groupID)
		}
	}
	b.groupMutex.Unlock()

	failedGroupIDs := make([]string, 0)
	for _, groupID := range staleGroupIDs {
		_, stderr, err := util.DeleteOFGroup(b.GetBridgeName(), groupID)
		if err != nil {
			klog.Errorf("Failed to delete stale OpenFlow group %s for bridge %s, error: %v, stderr: %s",
				groupID, b.GetBridgeName(), err, stderr)
			failedGroupIDs = append(failedGroupIDs, groupID)
		}
	}

	b.groupMutex.Lock()
	b.installedGroups = desiredGroupIDs
	for _, groupID := range failedGroupIDs {
		b.installedGroups[groupID] = struct{}{}
	}
	b.groupMutex.Unlock()
}

func parseOpenFlowGroupID(group string) (string, bool) {
	match := openFlowGroupIDRegexp.FindStringSubmatch(group)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func flattenFlowCacheEntries(flowCache map[string][]string) []string {
	flowCount := 0
	for _, entry := range flowCache {
		flowCount += len(entry)
	}
	flows := make([]string, 0, flowCount)
	for _, entry := range flowCache {
		flows = append(flows, entry...)
	}
	return flows
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//
//	the return traffic can be steered back to OVN logical topology
//
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeconfig.BridgeConfiguration, ovsClient libovsdbclient.Client) (*openflowManager, error) {
	if ovsClient == nil {
		return nil, fmt.Errorf("newGatewayOpenFlowManager: ovsClient must not be nil")
	}
	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge:    newOpenflowBridge(gwBridge),
		uplinkBridges:    map[string]*openflowBridge{},
		localnetPortChan: make(chan struct{}, 1),
		flowChan:         make(chan struct{}, 1),
		ovsClient:        ovsClient,
	}
	if exGWBridge != nil {
		ofm.externalGatewayBridge = newOpenflowBridge(exGWBridge)
	}
	ofm.registerLocalnetPortEventHandler()

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

// Localnet patch ports can appear after their network is created because
// ovn-controller applies ovn-bridge-mappings asynchronously. Watch the
// resulting OVSDB Port and Bridge changes so static flows are refreshed when
// the patch port is actually attached to or removed from a managed bridge.
// Bridge events do not identify the changed port type, so unrelated port
// membership changes on managed bridges may also trigger reconciliation. The
// flow cache is compared before OVS flows are updated.
func (c *openflowManager) registerLocalnetPortEventHandler() {
	c.ovsClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, row libovsdbmodel.Model) {
			c.handleLocalnetPortEvent(table, nil, row)
		},
		UpdateFunc: func(table string, old, new libovsdbmodel.Model) {
			c.handleLocalnetPortEvent(table, old, new)
		},
		DeleteFunc: func(table string, row libovsdbmodel.Model) {
			c.handleLocalnetPortEvent(table, row, nil)
		},
	})
}

func (c *openflowManager) handleLocalnetPortEvent(table string, old, new libovsdbmodel.Model) {
	switch table {
	case vswitchd.PortTable:
		// Ignore statistics and other updates to existing localnet ports. Bridge
		// membership changes are handled through Bridge table events below.
		if isLocalnetTopologyPort(old) == isLocalnetTopologyPort(new) {
			return
		}
	case vswitchd.BridgeTable:
		oldBridge, _ := old.(*vswitchd.Bridge)
		newBridge, _ := new.(*vswitchd.Bridge)
		if !c.isManagedBridge(oldBridge) && !c.isManagedBridge(newBridge) {
			return
		}
		if oldBridge != nil && newBridge != nil && oldBridge.Name == newBridge.Name &&
			stringListsEqual(oldBridge.Ports, newBridge.Ports) {
			return
		}
	default:
		return
	}

	c.notifyLocalnetPortChange()
}

func isLocalnetTopologyPort(row libovsdbmodel.Model) bool {
	port, ok := row.(*vswitchd.Port)
	return ok && bridgeconfig.IsLocalnetTopologyPort(port)
}

func (c *openflowManager) isManagedBridge(bridge *vswitchd.Bridge) bool {
	if bridge == nil {
		return false
	}
	if bridge.Name == c.defaultBridge.GetBridgeName() ||
		(c.externalGatewayBridge != nil && bridge.Name == c.externalGatewayBridge.GetBridgeName()) {
		return true
	}
	c.uplinkBridgesMu.Lock()
	defer c.uplinkBridgesMu.Unlock()
	_, found := c.uplinkBridges[bridge.Name]
	return found
}

func (c *openflowManager) notifyLocalnetPortChange() {
	select {
	case c.localnetPortChan <- struct{}{}:
	default:
	}
}

// Run starts OpenFlow Manager which will constantly sync flows for managed OVS bridges
func (c *openflowManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		syncPeriod := 15 * time.Second
		timer := time.NewTicker(syncPeriod)
		defer timer.Stop()
		var localnetDebounce <-chan time.Time
		for {
			select {
			case <-timer.C:

				netConfigs, physIntf, ofPortPhys := c.getDefaultBridgePortConfigurations()
				if err := checkPorts(c.ovsClient, netConfigs, physIntf, ofPortPhys); err != nil {
					klog.Errorf("Checkports failed %v", err)
					continue
				}

				if c.externalGatewayBridge != nil {
					netConfigs, physIntf, ofPortPhys = c.getExGwBridgePortConfigurations()
					if err := checkPorts(c.ovsClient, netConfigs, physIntf, ofPortPhys); err != nil {
						klog.Errorf("Checkports failed %v", err)
						continue
					}
				}
				c.checkUplinkBridgePorts()
				// Localnet topology patch ports are created and removed asynchronously by
				// ovn-controller. Re-render static flows before each periodic sync so
				// priority-102 NORMAL flows follow the current bridge membership.
				if _, err := c.refreshBridgeFlowCache(); err != nil {
					klog.Errorf("Failed to refresh gateway bridge flows: %v", err)
					continue
				}
				c.syncFlows()
			case <-c.localnetPortChan:
				// Port events identify localnet ports but not their bridge membership,
				// while Bridge events identify managed bridge membership changes but
				// not the port type. Notifications are therefore intentionally broad.
				// Debounce related events before checking authoritative OVSDB state.
				if localnetDebounce == nil {
					localnetDebounce = time.After(localnetPortEventDebounce)
				}
			case <-localnetDebounce:
				localnetDebounce = nil
				// A notification that races with the timer remains buffered and starts
				// another debounce cycle. An unchanged cache does not trigger a sync.
				changed, err := c.refreshBridgeFlowCache()
				if err != nil {
					klog.Errorf("Failed to refresh gateway bridge flows after an OVSDB port change: %v", err)
					continue
				}
				if changed {
					c.syncFlows()
					timer.Reset(syncPeriod)
				}
			case <-c.flowChan:
				// Requests reset the periodic timer: check the uplink bridge
				// ports here too, or a bridge whose check failed stays skipped.
				c.checkUplinkBridgePorts()
				c.syncFlows()
				timer.Reset(syncPeriod)
			case <-stopChan:
				// sync before shutting down because flows maybe added, and theres a race between flow channel (req sync)
				// and stop chan on shutdown. ensure flows are sync before shut down
				c.syncFlows()
				return
			}
		}
	}()
}

func (c *openflowManager) updateBridgePMTUDFlowCache(key string, ipAddrs []string) {
	dftFlows := c.defaultBridge.PMTUDDropFlows(ipAddrs)
	c.updateFlowCacheEntry(key, dftFlows)
	if c.externalGatewayBridge != nil {
		exGWBridgeDftFlows := c.externalGatewayBridge.PMTUDDropFlows(ipAddrs)
		c.updateExBridgeFlowCacheEntry(key, exGWBridgeDftFlows)
	}
	_ = c.forEachUplinkBridge(func(_ string, bridge *openflowBridge) error {
		bridge.updateFlowCacheEntry(key, bridge.PMTUDDropFlows(ipAddrs))
		return nil
	})
}

// updateBridgeFlowCache generates the "static" per-bridge flows
// note: this is shared between shared and local gateway modes
func (c *openflowManager) updateBridgeFlowCache(hostIPs []net.IP, hostSubnets []*net.IPNet) error {
	c.staticFlowsMu.Lock()
	defer c.staticFlowsMu.Unlock()

	if _, err := c.updateBridgeFlowCacheLocked(hostIPs, hostSubnets); err != nil {
		return err
	}
	c.staticFlowHostIPs = make([]net.IP, len(hostIPs))
	for i := range hostIPs {
		c.staticFlowHostIPs[i] = append(net.IP(nil), hostIPs[i]...)
	}
	c.staticFlowHostSubnets = util.CopyIPNets(hostSubnets)
	c.staticFlowsSet = true
	return nil
}

// refreshBridgeFlowCache re-renders static flows with the most recently
// supplied node addresses. It is serialized with address-driven updates so a
// periodic refresh cannot overwrite newer address information. The return
// value reports whether the static flow cache changed.
func (c *openflowManager) refreshBridgeFlowCache() (bool, error) {
	c.staticFlowsMu.Lock()
	defer c.staticFlowsMu.Unlock()
	if !c.staticFlowsSet {
		return false, nil
	}
	return c.updateBridgeFlowCacheLocked(c.staticFlowHostIPs, c.staticFlowHostSubnets)
}

func (c *openflowManager) updateBridgeFlowCacheLocked(hostIPs []net.IP, hostSubnets []*net.IPNet) (bool, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	dftFlows, err := c.defaultBridge.DefaultBridgeFlows(hostSubnets, hostIPs)
	if err != nil {
		return false, err
	}
	changed := !stringListsEqual(c.getFlowsByKey("DEFAULT"), dftFlows)

	c.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	c.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if c.externalGatewayBridge != nil {
		exGWBridgeDftFlows, err := c.externalGatewayBridge.ExternalBridgeFlows(hostSubnets)
		if err != nil {
			return false, err
		}
		changed = changed || !stringListsEqual(c.externalGatewayBridge.getFlowsByKey("DEFAULT"), exGWBridgeDftFlows)

		c.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		c.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}
	err = c.forEachUplinkBridge(func(_ string, bridge *openflowBridge) error {
		uplinkBridgeDftFlows, err := bridge.UplinkBridgeFlows(hostSubnets)
		if err != nil {
			return err
		}
		changed = changed || !stringListsEqual(bridge.getFlowsByKey("DEFAULT"), uplinkBridgeDftFlows)
		bridge.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		bridge.updateFlowCacheEntry("DEFAULT", uplinkBridgeDftFlows)
		return nil
	})
	return changed, err
}

func stringListsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, item := range a {
		counts[item]++
	}
	for _, item := range b {
		if counts[item] == 0 {
			return false
		}
		counts[item]--
	}
	return true
}

// getOfport returns the current ofport of the given OVS interface as a string,
// or "" if the interface does not exist. It errors if the interface exists but
// has no valid ofport assigned (unset or -1).
func getOfport(ovsClient libovsdbclient.Client, name string) (string, error) {
	iface, err := ovsops.GetOVSInterface(ovsClient, name)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get ofport of %s: %w", name, err)
	}
	if iface.Ofport == nil || *iface.Ofport == -1 {
		return "", fmt.Errorf("interface %s has invalid ofport", name)
	}
	return fmt.Sprintf("%d", *iface.Ofport), nil
}

// checkUplinkBridgePorts runs checkPorts on every uplink bridge and records
// the bridges whose flows must not be synced until a check passes. An admin
// may delete an Uplink bridge at any time: its ports vanish from OVSDB with
// it and checkPorts would exit, so a bridge gone from OVSDB only skips its
// sync. Its last network's cleanup deregisters it.
func (c *openflowManager) checkUplinkBridgePorts() {
	failed := map[string]struct{}{}
	for bridgeName, config := range c.getUplinkBridgePortConfigurations() {
		if _, err := ovsops.GetBridge(c.ovsClient, bridgeName); errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Warningf("Uplink bridge %s no longer exists, skipping its port check", bridgeName)
			failed[bridgeName] = struct{}{}
			continue
		}
		if err := checkPorts(c.ovsClient, config.netConfigs, config.physIntf, config.ofPortPhys); err != nil {
			klog.Errorf("Checkports failed for bridge %s: %v", bridgeName, err)
			failed[bridgeName] = struct{}{}
		}
	}
	c.uplinkBridgesMu.Lock()
	c.portCheckFailed = failed
	c.uplinkBridgesMu.Unlock()
}

func checkPorts(ovsClient libovsdbclient.Client, netConfigs []*bridgeconfig.BridgeUDNConfiguration, physIntf, ofPortPhys string) error {
	// it could be that the ovn-controller recreated the patch between the host OVS bridge and
	// the integration bridge, as a result the ofport number changed for that patch interface
	for _, netConfig := range netConfigs {
		if netConfig.OfPortPatch == "" {
			continue
		}
		curOfportPatch, err := getOfport(ovsClient, netConfig.PatchPort)
		if err != nil {
			return err
		}
		if netConfig.OfPortPatch != curOfportPatch {
			if netConfig.IsDefaultNetwork() {
				klog.Errorf("Fatal error: patch port %s ofport changed from %s to %s",
					netConfig.PatchPort, netConfig.OfPortPatch, curOfportPatch)
				os.Exit(1)
			} else {
				klog.Warningf("UDN patch port %s changed for existing network from %v to %v. Expecting bridge config update.", netConfig.PatchPort, netConfig.OfPortPatch, curOfportPatch)
			}
		}
	}

	// With --allow-no-uplink, NewBridgeConfiguration may leave physIntf empty.
	// There is then no physical ofport to track.
	if config.Gateway.AllowNoUplink && physIntf == "" {
		return nil
	}

	// it could be that someone removed the physical interface and added it back on the OVS host
	// bridge, as a result the ofport number changed for that physical interface
	curOfportPhys, err := getOfport(ovsClient, physIntf)
	if err != nil {
		return err
	}
	if ofPortPhys != curOfportPhys {
		klog.Errorf("Fatal error: phys port %s ofport changed from %s to %s",
			physIntf, ofPortPhys, curOfportPhys)
		os.Exit(1)
	}
	return nil
}

// bootstrapOVSFlows handles ensuring basic, required flows are in place. This is done before OpenFlow manager has
// been created/started, and only done when there is just a NORMAL flow programmed and OVN/OVS is already setup
func bootstrapOVSFlows(ovsClient libovsdbclient.Client, nodeName string) error {
	// see if patch port exists already
	var portsOutput string
	var stderr string
	var err error
	if portsOutput, stderr, err = util.RunOVSVsctl("--no-heading", "--data=bare", "--format=csv", "--columns",
		"name", "list", "interface"); err != nil {
		// bridge exists, but could not list ports
		return fmt.Errorf("failed to list ports on existing bridge br-int: %s, %w", stderr, err)
	}

	bridge, patchPort := localnetPortInfo(nodeName, portsOutput)

	if len(bridge) == 0 {
		// bridge exists but no patch port was found
		return nil
	}

	// get the current flows and if there is more than just default flow, we dont need to bootstrap as we already
	// have flows
	flows, err := util.GetOFFlows(bridge)
	if err != nil {
		return err
	}
	if len(flows) > 1 {
		// more than 1 flow, assume the OVS has retained previous flows from previous running OVNK instance
		return nil
	}

	// only have 1 flow, need to install required flows
	klog.Infof("Default NORMAL flow installed on OVS bridge: %s, will bootstrap with required port security flows", bridge)

	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	var bridgeMACAddress net.HardwareAddr
	if config.IsModeDPU() {
		bridgeMACAddress, err = util.GetDPUOps().GetHostGatewayMACAddress(ovsClient, bridge, nodeName)
		if err != nil {
			return err
		}
	} else {
		bridgeMACAddress, err = util.GetOVSPortMACAddress(bridge)
		if err != nil {
			return fmt.Errorf("failed to get MAC address for ovs port %s: %w", bridge, err)
		}
	}

	var dftFlows []string
	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
			nodetypes.DefaultOpenFlowCookie, ofportPatch, bridgeMACAddress))
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
			nodetypes.DefaultOpenFlowCookie, ofportPatch))
	dftFlows = append(dftFlows, "priority=0, table=0, actions=output:NORMAL")

	_, stderr, err = util.ReplaceOFFlows(bridge, dftFlows)
	if err != nil {
		return fmt.Errorf("failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, dftFlows)
	}

	return nil
}

// localnetPortInfo returns the name of the bridge and the patch port name for the default cluster network
func localnetPortInfo(nodeName string, portsOutput string) (string, string) {
	// This needs to work with:
	// - default network: patch-<bridge name>_<node>-to-br-int
	// but not with:
	// - user defined primary network: patch-<bridge name>_<network-name>_<node>-to-br-int
	// - user defined secondary localnet network: patch-<bridge name>_<network-name>_ovn_localnet_port-to-br-int
	// TODO: going forward, maybe it would preferable to just read the bridge name from the config.
	r := regexp.MustCompile(fmt.Sprintf("^patch-([^_]*)_%s-to-br-int$", nodeName))
	for _, line := range strings.Split(portsOutput, "\n") {
		matches := r.FindStringSubmatch(line)
		if len(matches) == 2 {
			return matches[1], matches[0]
		}
	}
	return "", ""
}
