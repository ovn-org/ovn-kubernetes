// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package managementport

import (
	"context"
	"fmt"
	"net"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	coreinformers "k8s.io/client-go/informers/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/controllers/mgmtportdevice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type udnManagementPort interface {
	create() error
	delete() error
}

// UDNManagementPortController plumbs the management port of one network. Port
// operations are serialized by lifecycle instead of by a lock: Create starts
// the device watch only once the ports are plumbed, Delete drains it before
// tearing them down, and the watch runs one reconcile at a time. This holds
// because the network lifecycle never runs Create and Delete concurrently,
// and because the controller is not reused after Delete.
type UDNManagementPortController struct {
	cfg   *udnManagementPortConfig
	ports map[string]udnManagementPort
	// deviceController follows the device published for this network. Only set
	// where the device is not resolved locally, which today is a DPU.
	deviceController *mgmtportdevice.Controller
	nodeLister       listers.NodeLister
}

type udnManagementPortConfig struct {
	util.NetInfo
	nodeName  string
	subnets   []*net.IPNet
	mpMAC     net.HardwareAddr
	ovsClient libovsdbclient.Client
}

func newUDNManagementPortConfig(nodeName string, networkLocalSubnets []*net.IPNet, netInfo util.NetInfo) (*udnManagementPortConfig, error) {
	if len(networkLocalSubnets) == 0 {
		return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", netInfo.GetNetworkName())
	}

	return &udnManagementPortConfig{
		NetInfo:  netInfo,
		subnets:  networkLocalSubnets,
		nodeName: nodeName,
		mpMAC:    util.IPAddrToHWAddr(netInfo.GetNodeManagementIP(networkLocalSubnets[0]).IP),
	}, nil
}

// Create plumbs the ports of this network's management port and then starts
// following the device published for it, so that the first update reconciles
// against plumbed state.
func (c *UDNManagementPortController) Create() error {
	for _, port := range c.ports {
		err := port.create()
		if err != nil {
			return err
		}
	}
	return c.deviceController.Start()
}

// Stop stops following the device published for this network and leaves its
// ports plumbed. This is the shutdown counterpart of Delete, which stops the
// same watch but also tears the ports down: ovnkube-node keeps the management
// port of a network it still owns across a restart.
func (c *UDNManagementPortController) Stop() {
	// c is nil when the network never got as far as plumbing its management
	// port, and shutdown stops whatever a network reached.
	if c == nil {
		return
	}
	c.deviceController.Stop()
}

// Delete stops following the device published for this network and then
// removes the ports of its management port. Stopping waits for a device
// update already in flight, so none can re-plumb behind the teardown.
func (c *UDNManagementPortController) Delete() error {
	c.deviceController.Stop()
	for _, port := range c.ports {
		err := port.delete()
		if err != nil {
			return err
		}
	}
	return nil
}

// Reconcile re-plumbs the representor when the device published for this
// network no longer matches the one in use. The representor is derived from
// that device on DPUs, and it can change while the port is plumbed, which
// otherwise leaves the DPU attached to a device the host has moved off.
func (c *UDNManagementPortController) Reconcile() error {
	rep, ok := c.ports[representorPort].(*udnManagementPortRep)
	if !ok {
		return nil
	}

	node, err := c.nodeLister.Get(c.cfg.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %v", c.cfg.nodeName, err)
	}
	mpdevs, err := util.ParseNodeManagementPortAnnotation(node)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return fmt.Errorf("failed to get management port details for network %s: %v", c.cfg.GetNetworkName(), err)
	}
	mpdev := mpdevs[c.cfg.GetNetworkName()]
	if mpdev == nil {
		// Nothing published to converge on. Tearing down here would disrupt a
		// working port, and a deleted network is torn down through Delete.
		return nil
	}
	repDevice, err := util.GetDPUOps().GetPortRepresentor(fmt.Sprintf("%d", mpdev.PfId), fmt.Sprintf("%d", mpdev.FuncId))
	if err != nil {
		return fmt.Errorf("failed to get management port representor for pfID %v vfID %v network %s: %v",
			mpdev.PfId, mpdev.FuncId, c.cfg.GetNetworkName(), err)
	}
	newRep := rep
	if repDevice != rep.repDevice {
		klog.Infof("Management port representor for network %s changed from %s to %s, re-plumbing",
			c.cfg.GetNetworkName(), rep.repDevice, repDevice)
		if err := rep.delete(); err != nil {
			return fmt.Errorf("failed to delete stale management port representor %s for network %s: %v",
				rep.repDevice, c.cfg.GetNetworkName(), err)
		}
		// Track the replacement before plumbing it, so that a teardown landing
		// while the create below fails still tears it down.
		newRep = newUDNManagementPortRep(c.cfg, repDevice)
		c.ports[representorPort] = newRep
	}

	// Plumbing the representor is idempotent, so a reconcile that finds the
	// device unchanged re-asserts it, and one retried after a failure above
	// completes it.
	if err := newRep.create(); err != nil {
		return fmt.Errorf("failed to create management port representor %s for network %s: %v",
			repDevice, c.cfg.GetNetworkName(), err)
	}

	return nil
}

// NewUDNManagementPortController creates a new management port controller for a primary UDN
func NewUDNManagementPortController(
	ovsClient libovsdbclient.Client,
	nodeInformer coreinformers.NodeInformer,
	nodeName string,
	networkLocalSubnets []*net.IPNet,
	netInfo util.NetInfo,
) (*UDNManagementPortController, error) {
	var mpdev *util.NetworkDeviceDetails

	mgmtIfName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(netInfo.GetNetworkID()))

	cfg, err := newUDNManagementPortConfig(nodeName, networkLocalSubnets, netInfo)
	if err != nil {
		return nil, err
	}
	cfg.ovsClient = ovsClient

	nodeLister := nodeInformer.Lister()
	node, err := nodeLister.Get(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %v", nodeName, err)
	}
	mpdevs, err := util.ParseNodeManagementPortAnnotation(node)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return nil, fmt.Errorf("failed to get management port details for network %s: %v", netInfo.GetNetworkName(), err)
		}
	} else {
		mpdev = mpdevs[netInfo.GetNetworkName()]
	}

	// in full mode and MgmtPortDPResourceName is empty, OVS internal management port can be used, otherwise, management
	// port must has been allocated for this network
	if ((config.IsModeDPU() || config.IsModeDPUHost()) || config.OvnKubeNode.MgmtPortDPResourceName != "") && mpdev == nil {
		return nil, fmt.Errorf("management port resource not allocated for network %s (annotation missing or invalid)", cfg.GetNetworkName())
	}

	// cleanup stale OVS management port entities created in different mode/configuration
	err = syncUDNManagementPort(cfg, mgmtIfName, mpdev)
	if err != nil {
		return nil, err
	}

	c := &UDNManagementPortController{
		cfg:        cfg,
		nodeLister: nodeLister,
		ports:      map[string]udnManagementPort{},
	}

	if config.IsModeFull() && config.OvnKubeNode.MgmtPortDPResourceName == "" {
		c.ports[ovsPort] = newUDNManagementPortOVS(cfg, mgmtIfName)
		return c, nil
	}

	switch config.OvnKubeNode.Mode {
	case types.NodeModeFull:
		repDeviceName, err := util.GetFunctionRepresentorName(mpdev.DeviceId)
		if err != nil {
			return nil, fmt.Errorf("failed to get management port representor name for device %s network %s: %v",
				mpdev.DeviceId, netInfo.GetNetworkName(), err)
		}
		c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpdev.DeviceId)
		c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
	case types.NodeModeDPU:
		repDeviceName, err := util.GetDPUOps().GetPortRepresentor(fmt.Sprintf("%d", mpdev.PfId), fmt.Sprintf("%d", mpdev.FuncId))
		if err != nil {
			return nil, fmt.Errorf("failed to get management port representor for pfID %v vfID %v network %s: %v",
				mpdev.PfId, mpdev.FuncId, netInfo.GetNetworkName(), err)
		}
		c.ports[representorPort] = newUDNManagementPortRep(cfg, repDeviceName)
		// the host owns this device and can move it, so follow it
		c.deviceController = mgmtportdevice.NewController(nodeName, netInfo.GetNetworkName(), nodeInformer, c)
	case types.NodeModeDPUHost:
		c.ports[netdevPort] = newUDNManagementPortNetdev(cfg, mgmtIfName, mpdev.DeviceId)
	}

	return c, nil
}

type udnManagementPortOVS struct {
	udnManagementPortConfig
	ifName string
}

type udnManagementPortNetdev struct {
	udnManagementPortConfig
	ifName   string
	deviceID string
}

type udnManagementPortRep struct {
	udnManagementPortConfig
	repDevice string
}

// newUDNManagementPortOVS creates a new udnManagementPortOVS
func newUDNManagementPortOVS(cfg *udnManagementPortConfig, ifName string) *udnManagementPortOVS {
	return &udnManagementPortOVS{
		udnManagementPortConfig: *cfg,
		ifName:                  ifName,
	}
}

// newUDNManagementPortNetdev creates a new udnManagementPortNetdev
func newUDNManagementPortNetdev(cfg *udnManagementPortConfig, ifName, deviceID string) *udnManagementPortNetdev {
	return &udnManagementPortNetdev{
		udnManagementPortConfig: *cfg,
		ifName:                  ifName,
		deviceID:                deviceID,
	}
}

// newUdnManagementPortRep creates a new udnManagementPortRep
func newUDNManagementPortRep(cfg *udnManagementPortConfig, repDeviceName string) *udnManagementPortRep {
	return &udnManagementPortRep{
		udnManagementPortConfig: *cfg,
		repDevice:               repDeviceName,
	}
}

// syncUDNManagementPort is to delete stale UDN management port entities created when the node was in different configuration/mode
func syncUDNManagementPort(cfg *udnManagementPortConfig, mgmtIfName string, mpdev *util.NetworkDeviceDetails) error {
	if cfg.ovsClient == nil {
		if !config.IsModeDPUHost() {
			return fmt.Errorf("OVS client is required to sync management port %s in node mode %s",
				mgmtIfName, config.OvnKubeNode.Mode)
		}
		// DPU-host has no local OVS. Preserve its stale netdevice cleanup, but
		// do not issue OVS queries that cannot succeed in this mode.
		return syncUDNManagementPortEntities(cfg, mgmtIfName, mpdev, "", "")
	}

	var ovsRepIfName, ovsInternalIfName string
	representors, findErr := ovsops.FindInterfacesWithPredicate(cfg.ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.ExternalIDs[types.OvnManagementPortNameExternalID] == mgmtIfName
	})
	if findErr != nil {
		return fmt.Errorf("failed to find management port representor for %s: %w", mgmtIfName, findErr)
	}
	if len(representors) > 0 {
		ovsRepIfName = representors[0].Name
	}
	internal, findErr := ovsops.FindInterfacesWithPredicate(cfg.ovsClient, func(iface *vswitchd.Interface) bool {
		return iface.Name == mgmtIfName && iface.Type == "internal"
	})
	if findErr != nil {
		return fmt.Errorf("failed to find internal management port for %s: %w", mgmtIfName, findErr)
	}
	if len(internal) > 0 {
		ovsInternalIfName = internal[0].Name
	}
	return syncUDNManagementPortEntities(cfg, mgmtIfName, mpdev, ovsRepIfName, ovsInternalIfName)
}

func syncUDNManagementPortEntities(cfg *udnManagementPortConfig, mgmtIfName string, mpdev *util.NetworkDeviceDetails, ovsRepIfName, ovsInternalIfName string) error {
	var err error
	if config.OvnKubeNode.MgmtPortDPResourceName == "" && config.IsModeFull() {
		// expect internal OVS management port interface
		if ovsRepIfName != "" {
			klog.V(5).Infof("Expected management port OVS internal interface, delete stale management port representor %s for network %s",
				ovsRepIfName, cfg.GetNetworkName())
			err = DeleteManagementPortRepInterface(cfg.ovsClient, cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
			if err != nil {
				klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
			}
		}
		// In gateway_udn unit testing, it pre-create the netdev link for management port OVS internal interface.
		// In order for unit testing to work, we can not tear-down/rename the link. Remove below for now.
		//
		//if ovsInternalIfName == "" {
		//	klog.V(5).Infof("Expected management port OVS internal interface, bring down stale management port netdev interface %s for network %s",
		//		mgmtIfName, cfg.GetNetworkName())
		//	link, _ := util.GetNetLinkOps().LinkByName(mgmtIfName)
		//	if link != nil {
		//		err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
		//		if err != nil {
		//			klog.Errorf("Failed to bring down OVS netdev interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
		//		}
		//	}
		//}
		return nil
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if ovsInternalIfName != "" {
			klog.V(5).Infof("Expected management port OVS netdev interface, bring down stale management port internal interface %s for network %s",
				mgmtIfName, cfg.GetNetworkName())
			err = DeleteManagementPortInternalOVSInterface(cfg.ovsClient, cfg.GetNetworkName(), mgmtIfName)
			if err != nil {
				klog.Errorf("Failed to delete OVS internal interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
			}
		}
		// then check the current mgmtIfName to see if it is the same netdevice
		link, _ := util.GetNetLinkOps().LinkByName(mgmtIfName)
		if link == nil {
			return nil
		}
		deviceId, err := util.GetDeviceIDFromNetdevice(link.Attrs().Name)
		if err != nil {
			return nil
		}
		if deviceId != mpdev.DeviceId {
			klog.Infof("Management port device %s for network %s deviceID mismatch: expected %s but is %s",
				mgmtIfName, cfg.GetNetworkName(), mpdev.DeviceId, deviceId)
			err = TearDownManagementPortLink(cfg.GetNetworkName(), link, "")
			if err != nil {
				klog.Errorf("Failed to bring down stale OVS netdev interface %s for network %s: %v", mgmtIfName, cfg.GetNetworkName(), err)
			}
			// in that case, representor could also be associated with stale netdev and needs to be deleted
			if ovsRepIfName != "" {
				err = DeleteManagementPortRepInterface(cfg.ovsClient, cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
				if err != nil {
					klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
				}
			}
		}
	} else if config.IsModeDPU() && ovsRepIfName != "" {
		repDeviceName, _ := util.GetDPUOps().GetPortRepresentor(fmt.Sprintf("%d", mpdev.PfId), fmt.Sprintf("%d", mpdev.FuncId))
		if repDeviceName != ovsRepIfName {
			err = DeleteManagementPortRepInterface(cfg.ovsClient, cfg.GetNetworkName(), ovsRepIfName, ovsRepIfName)
			if err != nil {
				klog.Errorf("Failed to delete stale OVS representor port interface %s for network %s: %v", ovsRepIfName, cfg.GetNetworkName(), err)
			}
		}
	}

	return nil
}

// udnManagementPortOVS.create does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: sets up the management port link on the host
// STEP3: enables IPv4 forwarding on the interface if the network has a v4 subnet
func (mp *udnManagementPortOVS) create() error {
	// STEP1
	if mp.ovsClient == nil {
		return fmt.Errorf("OVS client is required to create management port for network %s", mp.GetNetworkName())
	}
	mac := mp.mpMAC.String()
	iface := &vswitchd.Interface{
		Type:       "internal",
		MAC:        &mac,
		MTURequest: ptr.To(mp.MTU()),
		ExternalIDs: map[string]string{
			"iface-id":              mp.GetNetworkScopedK8sMgmtIntfName(mp.nodeName),
			types.NetworkExternalID: mp.GetNetworkName(),
		},
	}
	ovsdbOps, err := ovsops.CreateOrUpdatePodPortOps(mp.ovsClient, nil, "br-int", mp.ifName, &vswitchd.Port{}, iface)
	if err != nil {
		return fmt.Errorf("failed to create operations to add port to br-int for network %s: %w", mp.GetNetworkName(), err)
	}
	if err := ovsops.TransactAndCheckAndWaitForVSwitchd(context.Background(), mp.ovsClient, ovsdbOps); err != nil {
		return fmt.Errorf("failed to add port to br-int for network %s: %w", mp.GetNetworkName(), err)
	}
	return mp.configureLink()
}

func (mp *udnManagementPortOVS) configureLink() error {
	klog.V(3).Infof("Added OVS management port interface %s for network %s", mp.ifName, mp.GetNetworkName())
	// STEP2
	_, err := util.LinkSetUp(mp.ifName)
	if err != nil {
		return fmt.Errorf("failed to set the link up for interface %s while plumbing network %s, err: %v",
			mp.ifName, mp.GetNetworkName(), err)
	}
	klog.V(3).Infof("Setup management port link %s for network %s succeeded", mp.ifName, mp.GetNetworkName())

	// STEP3
	err = util.SetForwardingModeForInterface(mp.ifName)
	if err != nil {
		return err
	}

	// add loose mode for rp filter on management port
	if err = util.SetRPFilterLooseModeForInterface(mp.ifName); err != nil {
		return err
	}
	return nil
}

func (mp *udnManagementPortOVS) delete() error {
	return DeleteManagementPortInternalOVSInterface(mp.ovsClient, mp.GetNetworkName(), mp.ifName)
}

// Create management port representor. Note that the representor device is not renamed. One can determine its associated
// UDN network and its management netdev interface from its external-ids.
func (mp *udnManagementPortRep) create() error {
	klog.V(5).Infof("Lookup representor link and existing management port for '%v'", mp.repDevice)
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repDevice)
	if err != nil {
		return fmt.Errorf("failed to lookup management port representor interface %s for network %s: %v", mp.repDevice, mp.GetNetworkName(), err)
	}

	// configure management port: rename, set MTU and set link up and connect representor port to br-int
	klog.V(5).Infof("Setup representor management port %s for network %s", link.Attrs().Name, mp.GetNetworkName())
	err = bringupManagementPortLink(mp.GetNetworkName(), link, nil, mp.repDevice, mp.MTU())
	if err != nil {
		return fmt.Errorf("bring up management port %s for network %s failed: %v", mp.repDevice, mp.GetNetworkName(), err)
	}

	externalIds := []string{
		fmt.Sprintf("%s=%s", types.NetworkExternalID, mp.GetNetworkName()),
		fmt.Sprintf("%s=%s", types.OvnManagementPortNameExternalID, util.GetNetworkScopedK8sMgmtHostIntfName(uint(mp.GetNetworkID()))),
	}
	return createManagementPortOVSRepresentor(mp.ovsClient, mp.GetNetworkName(), mp.repDevice, mp.GetNetworkScopedK8sMgmtIntfName(mp.nodeName), mp.MTU(), externalIds)
}

func (mp *udnManagementPortRep) delete() error {
	return DeleteManagementPortRepInterface(mp.ovsClient, mp.GetNetworkName(), mp.repDevice, mp.repDevice)
}

func (mp *udnManagementPortNetdev) create() error {
	netdevice, err := util.GetNetdevNameFromDeviceId(mp.deviceID, v1.DeviceInfo{})
	if err != nil {
		return fmt.Errorf("failed to get netdev name for device %s allocated for %s network: %v", mp.deviceID, mp.GetNetworkName(), err)
	}

	link, err := util.LinkByNameOrAlias(netdevice)
	if err != nil {
		return fmt.Errorf("failed to get management port link %s for network %s: %v", netdevice, mp.GetNetworkName(), err)
	}

	klog.V(5).Infof("Setup netdevice management port %s for network %s: netdevice %s, MAC %v MTU: %v",
		mp.ifName, mp.GetNetworkName(), netdevice, mp.mpMAC, mp.MTU())
	err = bringupManagementPortLink(mp.GetNetworkName(), link, &mp.mpMAC, mp.ifName, mp.MTU())
	if err != nil {
		return fmt.Errorf("bring up management port %s for network %s failed: %v", mp.ifName, mp.GetNetworkName(), err)
	}

	err = util.SetForwardingModeForInterface(mp.ifName)
	if err != nil {
		return err
	}

	// add loose mode for rp filter on management port
	if err = util.SetRPFilterLooseModeForInterface(mp.ifName); err != nil {
		return err
	}
	return err
}

func (mp *udnManagementPortNetdev) delete() error {
	link, err := util.GetNetLinkOps().LinkByName(mp.ifName)
	if err != nil {
		klog.Warningf("Failed to lookup management port interface %s for network %s: %v", mp.ifName, mp.GetNetworkName(), err)
		return nil
	}

	// original management port interface name can be found from link alias
	err = TearDownManagementPortLink(mp.GetNetworkName(), link, "")
	if err != nil {
		return fmt.Errorf("tearing down management port %s for network %s failed: %v", mp.ifName, mp.GetNetworkName(), err)
	}
	return nil
}
