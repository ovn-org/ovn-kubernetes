// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package managementport

import (
	"errors"
	"fmt"
	"net"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var errMgmtPortDeviceNotFound = errors.New("management port PCI device not found")

type managementPortRepresentor struct {
	cfg        *managementPortConfig
	ifName     string
	repDevName string
	link       netlink.Link
	ovsClient  libovsdbclient.Client
}

// newManagementPortRepresentor creates a new managementPort representor
// For management port representor only.
// name is types.K8sMgmtIntfName (on dpu mode node) or types.K8sMgmtIntfName+"_0" (on full mode)
// repDevName is the representor VF device name
func newManagementPortRepresentor(name, repDevName string, cfg *managementPortConfig, ovsClient libovsdbclient.Client) *managementPortRepresentor {
	return &managementPortRepresentor{
		cfg:        cfg,
		ifName:     name,
		repDevName: repDevName,
		ovsClient:  ovsClient,
	}
}

func (mp *managementPortRepresentor) create() error {
	klog.V(5).Infof("Lookup representor link and existing management port for '%v'", mp.repDevName)
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repDevName)
	if err != nil {
		return err
	}

	if link.Attrs().Name != mp.ifName {
		if err := syncMgmtPortInterface(mp.ovsClient, mp.ifName, false); err != nil {
			return fmt.Errorf("failed to check existing management port: %v", err)
		}
	}

	klog.V(5).Infof("Setup representor management port: %s", link.Attrs().Name)
	// configure management port: rename representor device to specified management port name, set MTU and bring the link up
	err = bringupManagementPortLink(types.DefaultNetworkName, link, nil, mp.ifName, config.Default.MTU)
	if err != nil {
		return fmt.Errorf("update management port for default network failed: %v", err)
	}
	// connect representor port to br-int, set OvnManagementPortNameExternalID external-id to indicate its
	// associated network name and management port device name
	externalIDs := []string{fmt.Sprintf("%s=%s", types.OvnManagementPortNameExternalID, types.K8sMgmtIntfName)}
	if mp.repDevName != mp.ifName {
		externalIDs = append(externalIDs, fmt.Sprintf("ovn-orig-mgmt-port-rep-name=%s", mp.repDevName))
	}
	err = createManagementPortOVSRepresentor(mp.ovsClient, types.DefaultNetworkName, mp.ifName, types.K8sPrefix+mp.cfg.nodeName, config.Default.MTU, externalIDs)
	if err != nil {
		return err
	}

	mp.link = link
	return nil
}

func (mp *managementPortRepresentor) checkRepresentorPortHealth() error {
	// After host reboot, management port link name changes back to default name.
	link, err := util.GetNetLinkOps().LinkByName(mp.ifName)
	if err != nil {
		klog.Warningf("Failed to get link device %s: %v", mp.ifName, err)
		// Get management port representor by name
		link, err := util.GetNetLinkOps().LinkByName(mp.repDevName)
		if err != nil {
			klog.Fatalf("Can't find neither renamed management port %s nor original representor device %s: %v, inconsistent state, crashing to recover with fresh start", mp.ifName, mp.repDevName, err)
		}
		err = bringupManagementPortLink(types.DefaultNetworkName, link, nil, mp.ifName, config.Default.MTU)
		if err != nil {
			return err
		}
		mp.link = link
	} else if (link.Attrs().Flags & net.FlagUp) != net.FlagUp {
		if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set link up for device %s: %w", mp.ifName, err)
		}
	}
	return nil
}

func (mp *managementPortRepresentor) reconcilePeriod() time.Duration {
	return 5 * time.Second
}

func (mp *managementPortRepresentor) doReconcile() error {
	return mp.checkRepresentorPortHealth()
}

type managementPortNetdev struct {
	ifName       string
	deviceID     string
	cfg          *managementPortConfig
	routeManager *routemanager.Controller
	ovsClient    libovsdbclient.Client
}

// newManagementPortNetdev creates a new managementPortNetdev.
// deviceID is the PCI device ID (e.g., "0000:03:00.2") used to identify the VF.
func newManagementPortNetdev(deviceID string, cfg *managementPortConfig, routeManager *routemanager.Controller, ovsClient libovsdbclient.Client) *managementPortNetdev {
	return &managementPortNetdev{
		ifName:       types.K8sMgmtIntfName,
		deviceID:     deviceID,
		cfg:          cfg,
		routeManager: routeManager,
		ovsClient:    ovsClient,
	}
}

// findNetdevByDeviceID resolves the current interface name from the PCI device ID.
func (mp *managementPortNetdev) findNetdevByDeviceID() (netlink.Link, error) {
	if mp.deviceID == "" {
		return nil, fmt.Errorf("no device ID available")
	}

	netdevName, err := util.GetNetdevNameFromDeviceId(mp.deviceID, nadapi.DeviceInfo{})
	if err != nil {
		return nil, fmt.Errorf("%w: device ID %s lookup failed: %v", errMgmtPortDeviceNotFound, mp.deviceID, err)
	}
	if netdevName == "" {
		return nil, fmt.Errorf("%w: device ID %s resolved to empty netdev name", errMgmtPortDeviceNotFound, mp.deviceID)
	}

	link, err := util.LinkByNameOrAlias(netdevName)
	if err != nil {
		return nil, fmt.Errorf("device ID %s resolved to %s but the link lookup failed: %w", mp.deviceID, netdevName, err)
	}

	return link, nil
}

func (mp *managementPortNetdev) create() error {
	klog.Infof("Management port netdev create: deviceID=%s, ifName=%s", mp.deviceID, mp.ifName)
	link, err := mp.findNetdevByDeviceID()
	if err != nil {
		return err
	}

	if link.Attrs().Name != mp.ifName {
		err = syncMgmtPortInterface(mp.ovsClient, mp.ifName, false)
		if err != nil {
			return fmt.Errorf("failed to sync management port: %v", err)
		}
	}

	// configure management port: name, mac, MTU, nftables
	// mac addr, derived from the first entry in host subnets using the .2 address as mac with a fixed prefix.
	klog.V(5).Infof("Setup netdevice management port: %s (deviceID: %s)", link.Attrs().Name, mp.deviceID)
	mgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(mp.cfg.hostSubnets[0]).IP)
	err = bringupManagementPortLink(types.DefaultNetworkName, link, &mgmtPortMac, mp.ifName, config.Default.MTU)
	if err != nil {
		return err
	}

	if link.Attrs().Name != mp.ifName && (config.IsModeDPU() || config.IsModeFull()) {
		if err := ovsops.UpdateOpenvSwitchExternalIDs(mp.ovsClient, map[string]string{
			"ovn-orig-mgmt-port-netdev-name": link.Attrs().Name,
		}); err != nil {
			return fmt.Errorf("failed to store original mgmt port interface name: %w", err)
		}
	}

	err = createPlatformManagementPort(mp.ifName, mp.cfg, mp.routeManager)
	if err != nil {
		return err
	}
	return nil
}

func (mp *managementPortNetdev) reconcilePeriod() time.Duration {
	return 30 * time.Second
}

func (mp *managementPortNetdev) doReconcile() error {
	if err := createPlatformManagementPort(mp.ifName, mp.cfg, mp.routeManager); err != nil {
		klog.Warningf("Failed to reconcile management port netdev, attempting to recreate: %v", err)
		if err := mp.create(); err != nil {
			if errors.Is(err, errMgmtPortDeviceNotFound) {
				// The VF's PCI device is no longer present on the bus.
				// For example, a DPU reboot while the host container is
				// still running destroys all VFs and recreates them from
				// scratch — potentially under different PCI addresses
				// (e.g., after a firmware settings change).
				// We cannot safely pick a replacement VF at runtime; only a
				// container restart allows the device plugin to re-allocate
				// the correct device.
				klog.Fatalf("Failed to recreate management port netdev, terminating so device plugin can re-allocate the correct VF on restart: %v", err)
			}
			return fmt.Errorf("failed to recreate management port: %w", err)
		}
	}
	return nil
}
