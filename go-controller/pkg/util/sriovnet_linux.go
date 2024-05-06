//go:build linux
// +build linux

package util

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/k8snetworkplumbingwg/govdpa/pkg/kvdpa"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/k8snetworkplumbingwg/sriovnet"
	"k8s.io/klog/v2"
)

const (
	PcidevPrefix = "device"
	NetSysDir    = "/sys/class/net"
)

type SriovnetOps interface {
	GetNetDevicesFromPci(pciAddress string) ([]string, error)
	GetNetDevicesFromAux(auxDev string) ([]string, error)
	GetPciFromNetDevice(name string) (string, error)
	GetUplinkRepresentor(vfPciAddress string) (string, error)
	GetUplinkRepresentorFromAux(auxDev string) (string, error)
	GetVfIndexByPciAddress(vfPciAddress string) (int, error)
	GetPfIndexByVfPciAddress(vfPciAddress string) (int, error)
	GetSfIndexByAuxDev(auxDev string) (int, error)
	GetVfRepresentor(uplink string, vfIndex int) (string, error)
	GetSfRepresentor(uplink string, sfIndex int) (string, error)
	GetPfPciFromVfPci(vfPciAddress string) (string, error)
	GetPfPciFromAux(auxDev string) (string, error)
	GetVfRepresentorDPU(pfID, vfIndex string) (string, error)
	IsVfPciVfioBound(pciAddr string) bool
	GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error)
	GetRepresentorPortFlavour(netdev string) (sriovnet.PortFlavour, error)
	GetPCIFromDeviceName(netdevName string) (string, error)
	GetPortIndexFromRepresentor(name string) (int, error)
}

type defaultSriovnetOps struct {
}

var sriovnetOps SriovnetOps = &defaultSriovnetOps{}

// SetSriovnetOpsInst method would be used by unit tests in other packages
func SetSriovnetOpsInst(mockInst SriovnetOps) {
	sriovnetOps = mockInst
}

// GetSriovnetOps will be invoked by functions in other packages that would need access to the sriovnet library methods.
func GetSriovnetOps() SriovnetOps {
	return sriovnetOps
}

func (defaultSriovnetOps) GetNetDevicesFromPci(pciAddress string) ([]string, error) {
	return sriovnet.GetNetDevicesFromPci(pciAddress)
}

func (defaultSriovnetOps) GetNetDevicesFromAux(auxDev string) ([]string, error) {
	return sriovnet.GetNetDevicesFromAux(auxDev)
}

func (defaultSriovnetOps) GetPciFromNetDevice(name string) (string, error) {
	return sriovnet.GetPciFromNetDevice(name)
}

func (defaultSriovnetOps) GetUplinkRepresentor(vfPciAddress string) (string, error) {
	return sriovnet.GetUplinkRepresentor(vfPciAddress)
}

func (defaultSriovnetOps) GetUplinkRepresentorFromAux(auxDev string) (string, error) {
	return sriovnet.GetUplinkRepresentorFromAux(auxDev)
}

func (defaultSriovnetOps) GetVfIndexByPciAddress(vfPciAddress string) (int, error) {
	return sriovnet.GetVfIndexByPciAddress(vfPciAddress)
}

func (defaultSriovnetOps) GetPfIndexByVfPciAddress(vfPciAddress string) (int, error) {
	return sriovnet.GetPfIndexByVfPciAddress(vfPciAddress)
}

func (defaultSriovnetOps) GetSfIndexByAuxDev(auxDev string) (int, error) {
	return sriovnet.GetSfIndexByAuxDev(auxDev)
}

func (defaultSriovnetOps) GetVfRepresentor(uplink string, vfIndex int) (string, error) {
	return sriovnet.GetVfRepresentor(uplink, vfIndex)
}

func (defaultSriovnetOps) GetSfRepresentor(uplink string, sfIndex int) (string, error) {
	return sriovnet.GetSfRepresentor(uplink, sfIndex)
}

func (defaultSriovnetOps) GetPfPciFromVfPci(vfPciAddress string) (string, error) {
	return sriovnet.GetPfPciFromVfPci(vfPciAddress)
}

func (defaultSriovnetOps) GetPfPciFromAux(auxDev string) (string, error) {
	return sriovnet.GetPfPciFromAux(auxDev)
}

func (defaultSriovnetOps) GetVfRepresentorDPU(pfID, vfIndex string) (string, error) {
	return sriovnet.GetVfRepresentorDPU(pfID, vfIndex)
}

func (defaultSriovnetOps) GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error) {
	return sriovnet.GetRepresentorPeerMacAddress(netdev)
}

func (defaultSriovnetOps) GetRepresentorPortFlavour(netdev string) (sriovnet.PortFlavour, error) {
	return sriovnet.GetRepresentorPortFlavour(netdev)
}

func (defaultSriovnetOps) GetPortIndexFromRepresentor(name string) (int, error) {
	return sriovnet.GetPortIndexFromRepresentor(name)
}

// GetFunctionRepresentorName returns representor name for passed device ID. Supported devices are Virtual Function
// or Scalable Function
func GetFunctionRepresentorName(deviceID string) (string, error) {
	var rep, uplink string
	var err error
	var index int

	if IsPCIDeviceName(deviceID) { // PCI device
		uplink, err = GetSriovnetOps().GetUplinkRepresentor(deviceID)
		if err != nil {
			return "", err
		}
		index, err = GetSriovnetOps().GetVfIndexByPciAddress(deviceID)
		if err != nil {
			return "", err
		}
		rep, err = GetSriovnetOps().GetVfRepresentor(uplink, index)
	} else if IsAuxDeviceName(deviceID) { // Auxiliary device
		uplink, err = GetSriovnetOps().GetUplinkRepresentorFromAux(deviceID)
		if err != nil {
			return "", err
		}
		index, err = GetSriovnetOps().GetSfIndexByAuxDev(deviceID)
		if err != nil {
			return "", err
		}
		rep, err = GetSriovnetOps().GetSfRepresentor(uplink, index)
	} else {
		return "", fmt.Errorf("cannot determine device type for id '%s'", deviceID)
	}
	if err != nil {
		return "", err
	}
	return rep, nil
}

// GetNetdevNameFromDeviceId returns the netdevice name from the passed device ID.
func GetNetdevNameFromDeviceId(deviceId string, deviceInfo nadapi.DeviceInfo) (string, error) {
	var netdevices []string
	var err error

	if IsPCIDeviceName(deviceId) {
		if deviceInfo.Vdpa != nil {
			if deviceInfo.Vdpa.Driver == "vhost" {
				klog.V(2).Info("deviceInfo.Vdpa.Driver is vhost, returning empty netdev")
				return "", nil
			}
		}

		// If a virtio/vDPA device exists, it takes preference over the vendor device, steering-wize
		var vdpaDevice kvdpa.VdpaDevice
		vdpaDevice, err = GetVdpaOps().GetVdpaDeviceByPci(deviceId)
		if err == nil && vdpaDevice != nil && vdpaDevice.Driver() == kvdpa.VirtioVdpaDriver {
			klog.V(2).Infof("deviceInfo.Vdpa.Driver is virtio, returning netdev %s", vdpaDevice.VirtioNet().NetDev())
			return vdpaDevice.VirtioNet().NetDev(), nil
		}
		if err != nil {
			klog.Warningf("Error when searching for the virtio/vdpa netdev: %v", err)
		}

		netdevices, err = GetSriovnetOps().GetNetDevicesFromPci(deviceId)
	} else { // Auxiliary network device
		netdevices, err = GetSriovnetOps().GetNetDevicesFromAux(deviceId)
	}
	if err != nil {
		return "", err
	}

	// Make sure we have 1 netdevice per pci address
	numNetDevices := len(netdevices)
	if numNetDevices != 1 {
		return "", fmt.Errorf("failed to get one netdevice interface (count %d) per Device ID %s", numNetDevices, deviceId)
	}
	return netdevices[0], nil
}

func (defaultSriovnetOps) IsVfPciVfioBound(pciAddr string) bool {
	return sriovnet.IsVfPciVfioBound(pciAddr)
}

// SetVFHardwreAddress sets mac address for a VF interface
func SetVFHardwreAddress(deviceID string, mac net.HardwareAddr) error {
	// get uplink netdevice name and its netlink object
	uplink, err := GetSriovnetOps().GetUplinkRepresentor(deviceID)
	if err != nil {
		return err
	}
	uplinkObj, err := GetNetLinkOps().LinkByName(uplink)
	if err != nil {
		return err
	}
	// get VF index from PCI
	vfIndex, err := GetSriovnetOps().GetVfIndexByPciAddress(deviceID)
	if err != nil {
		return err
	}
	// set MAC address through VF representor
	if err := GetNetLinkOps().LinkSetVfHardwareAddr(uplinkObj, vfIndex, mac); err != nil {
		return err
	}
	return nil
}

// From sriovnet, ideally should export from the lib and use it here.
func readPCIsymbolicLink(symbolicLink string) (string, error) {
	pciDevDir, err := os.Readlink(symbolicLink)
	//nolint:gomnd
	if len(pciDevDir) <= 3 {
		return "", fmt.Errorf("could not find PCI Address")
	}

	return pciDevDir[9:], err
}

func (defaultSriovnetOps) GetPCIFromDeviceName(netdevName string) (string, error) {
	symbolicLink := filepath.Join(NetSysDir, netdevName, PcidevPrefix)
	pciAddress, err := readPCIsymbolicLink(symbolicLink)
	if err != nil {
		err = fmt.Errorf("%v for netdevice %s", err, netdevName)
	}
	return pciAddress, err
}

// GetUplinkRepresentorName returns uplink representor name for passed device ID.
// Supported devices are Virtual Function or Scalable Function
func GetUplinkRepresentorName(deviceID string) (string, error) {
	var uplink string
	var err error

	if IsPCIDeviceName(deviceID) { // PCI device
		uplink, err = GetSriovnetOps().GetUplinkRepresentor(deviceID)
	} else if IsAuxDeviceName(deviceID) { // Auxiliary device
		uplink, err = GetSriovnetOps().GetUplinkRepresentorFromAux(deviceID)
	}

	return uplink, err
}
