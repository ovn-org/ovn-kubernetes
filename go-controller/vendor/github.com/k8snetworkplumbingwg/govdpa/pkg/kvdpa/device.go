package kvdpa

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Exported constants
const (
	VhostVdpaDriver  = "vhost_vdpa"
	VirtioVdpaDriver = "virtio_vdpa"
)

// Private constants
const (
	vdpaBusDevDir   = "/sys/bus/vdpa/devices"
	vdpaVhostDevDir = "/dev"
	rootDevDir      = "/sys/devices"
)

// VdpaDevice contains information about a Vdpa Device
type VdpaDevice interface {
	Driver() string
	Name() string
	MgmtDev() MgmtDev
	VirtioNet() VirtioNet
	VhostVdpa() VhostVdpa
	ParentDevicePath() (string, error)
}

// vdpaDev implements VdpaDevice interface
type vdpaDev struct {
	name      string
	driver    string
	mgmtDev   *mgmtDev
	virtioNet VirtioNet
	vhostVdpa VhostVdpa
}

// Driver resturns de device's driver name
func (vd *vdpaDev) Driver() string {
	return vd.driver
}

// Driver resturns de device's name
func (vd *vdpaDev) Name() string {
	return vd.name
}

// MgmtDev returns the device's management device
func (vd *vdpaDev) MgmtDev() MgmtDev {
	return vd.mgmtDev
}

// VhostVdpa returns the VhostVdpa device information associated
// or nil if the device is not bound to the vhost_vdpa driver
func (vd *vdpaDev) VhostVdpa() VhostVdpa {
	return vd.vhostVdpa
}

// Virtionet returns the VirtioNet device information associated
// or nil if the device is not bound to the virtio_vdpa driver
func (vd *vdpaDev) VirtioNet() VirtioNet {
	return vd.virtioNet
}

// getBusInfo populates the vdpa bus information
// the vdpa device must have at least the name prepopulated
func (vd *vdpaDev) getBusInfo() error {
	driverLink, err := os.Readlink(filepath.Join(vdpaBusDevDir, vd.name, "driver"))
	if err != nil {
		// No error if driver is not present. The device is simply not bound to any.
		return nil
	}

	vd.driver = filepath.Base(driverLink)

	switch vd.driver {
	case VhostVdpaDriver:
		vd.vhostVdpa, err = vd.getVhostVdpaDev()
		if err != nil {
			return err
		}
	case VirtioVdpaDriver:
		vd.virtioNet, err = vd.getVirtioVdpaDev()
		if err != nil {
			return err
		}
	}

	return nil
}

// parseAttributes populates the vdpa device information from netlink attributes
func (vd *vdpaDev) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	mgmtDev := &mgmtDev{}
	for _, a := range attrs {
		switch a.Attr.Type {
		case VdpaAttrDevName:
			vd.name = string(a.Value[:len(a.Value)-1])
		case VdpaAttrMgmtDevBusName:
			mgmtDev.busName = string(a.Value[:len(a.Value)-1])
		case VdpaAttrMgmtDevDevName:
			mgmtDev.devName = string(a.Value[:len(a.Value)-1])
		}
	}
	vd.mgmtDev = mgmtDev
	return nil
}

/* Finds the vhost vdpa device of a vdpa device and returns it's path */
func (vd *vdpaDev) getVhostVdpaDev() (VhostVdpa, error) {
	// vhost vdpa devices live in the vdpa device's path
	path := filepath.Join(vdpaBusDevDir, vd.name)
	return GetVhostVdpaDevInPath(path)
}

/* ParentDevice returns the path of the parent device (e.g: PCI) of the device */
func (vd *vdpaDev) ParentDevicePath() (string, error) {
	vdpaDevicePath := filepath.Join(vdpaBusDevDir, vd.name)

	/* For pci devices we have:
	/sys/bud/vdpa/devices/vdpaX ->
	    ../../../devices/pci0000:00/.../0000:05:00:1/vdpaX

	Resolving the symlinks should give us the parent PCI device.
	*/
	devicePath, err := filepath.EvalSymlinks(vdpaDevicePath)
	if err != nil {
		return "", err
	}

	/* If the parent device is the root device /sys/devices, there is
	no parent (e.g: vdpasim).
	*/
	parent := filepath.Dir(devicePath)
	if parent == rootDevDir {
		return devicePath, nil
	}

	return parent, nil
}

/*
	Finds the virtio vdpa device of a vdpa device and returns its path

Currently, PCI-based devices have the following sysfs structure:
/sys/bus/vdpa/devices/

	vdpa1 -> ../../../devices/pci0000:00/0000:00:03.2/0000:05:00.2/vdpa1

In order to find the virtio device we look for virtio* devices inside the parent device:

	sys/devices/pci0000:00/0000:00:03.2/0000:05:00.2/virtio{N}

We also check the virtio device exists in the virtio bus:
/sys/bus/virtio/devices

	virtio{N} -> ../../../devices/pci0000:00/0000:00:03.2/0000:05:00.2/virtio{N}
*/
func (vd *vdpaDev) getVirtioVdpaDev() (VirtioNet, error) {
	parentPath, err := vd.ParentDevicePath()
	if err != nil {
		return nil, err
	}
	return GetVirtioNetInPath(parentPath)
}

/*GetVdpaDevice returns the vdpa device information by a vdpa device name */
func GetVdpaDevice(name string) (VdpaDevice, error) {
	nameAttr, err := GetNetlinkOps().NewAttribute(VdpaAttrDevName, name)
	if err != nil {
		return nil, err
	}

	msgs, err := GetNetlinkOps().
		RunVdpaNetlinkCmd(VdpaCmdDevGet, 0, []*nl.RtAttr{nameAttr})
	if err != nil {
		return nil, err
	}

	vdpaDevs, err := parseDevLinkVdpaDevList(msgs)
	if err != nil {
		return nil, err
	}
	return vdpaDevs[0], nil
}

/*
GetVdpaDevicesByMgmtDev returns the VdpaDevice objects whose MgmtDev
has the given bus and device names.
*/
func GetVdpaDevicesByMgmtDev(busName, devName string) ([]VdpaDevice, error) {
	result := []VdpaDevice{}
	devices, err := ListVdpaDevices()
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.MgmtDev() != nil &&
			device.MgmtDev().BusName() == busName &&
			device.MgmtDev().DevName() == devName {
			result = append(result, device)
		}
	}
	if len(result) == 0 {
		return nil, syscall.ENODEV
	}
	return result, nil
}

/*ListVdpaDevices returns a list of all available vdpa devices */
func ListVdpaDevices() ([]VdpaDevice, error) {
	msgs, err := GetNetlinkOps().RunVdpaNetlinkCmd(VdpaCmdDevGet, syscall.NLM_F_DUMP, nil)
	if err != nil {
		return nil, err
	}

	vdpaDevs, err := parseDevLinkVdpaDevList(msgs)
	if err != nil {
		return nil, err
	}
	return vdpaDevs, nil
}

func extractBusNameAndMgmtDeviceName(fullMgmtDeviceName string) (busName string, mgmtDeviceName string, err error) {
	numSlashes := strings.Count(fullMgmtDeviceName, "/")
	if numSlashes > 1 {
		return "", "", errors.New("expected mgmtDeviceName to be either in the format <mgmtBusName>/<mgmtDeviceName> or <mgmtDeviceName>")
	} else if numSlashes == 0 {
		return "", fullMgmtDeviceName, nil
	} else {
		values := strings.Split(fullMgmtDeviceName, "/")
		return values[0], values[1], nil
	}
}

/*
GetVdpaDevicesByPciAddress returns the VdpaDevice objects for the given pciAddress

	The pciAddress must have one of the following formats:
	- MgmtBusName/MgmtDevName
	- MgmtDevName
*/
func GetVdpaDevicesByPciAddress(pciAddress string) ([]VdpaDevice, error) {
	busName, mgmtDeviceName, err := extractBusNameAndMgmtDeviceName(pciAddress)
	if err != nil {
		return nil, unix.EINVAL
	}

	return GetVdpaDevicesByMgmtDev(busName, mgmtDeviceName)
}

/*AddVdpaDevice adds a new vdpa device to the given management device */
func AddVdpaDevice(mgmtDeviceName string, vdpaDeviceName string) error {
	if mgmtDeviceName == "" || vdpaDeviceName == "" {
		return unix.EINVAL
	}

	busName, mgmtDeviceName, err := extractBusNameAndMgmtDeviceName(mgmtDeviceName)
	if err != nil {
		return unix.EINVAL
	}

	var attributes []*nl.RtAttr
	var busNameAttr *nl.RtAttr
	if busName != "" {
		busNameAttr, err = GetNetlinkOps().NewAttribute(VdpaAttrMgmtDevBusName, busName)
		if err != nil {
			return err
		}
		attributes = append(attributes, busNameAttr)
	}

	mgmtAttr, err := GetNetlinkOps().NewAttribute(VdpaAttrMgmtDevDevName, mgmtDeviceName)
	if err != nil {
		return err
	}
	attributes = append(attributes, mgmtAttr)

	nameAttr, err := GetNetlinkOps().NewAttribute(VdpaAttrDevName, vdpaDeviceName)
	if err != nil {
		return err
	}
	attributes = append(attributes, nameAttr)

	_, err = GetNetlinkOps().RunVdpaNetlinkCmd(VdpaCmdDevNew, unix.NLM_F_ACK|unix.NLM_F_REQUEST, attributes)
	if err != nil {
		return err
	}

	return nil
}

/*DeleteVdpaDevice deletes a vdpa device */
func DeleteVdpaDevice(vdpaDeviceName string) error {
	if vdpaDeviceName == "" {
		return unix.EINVAL
	}

	nameAttr, err := GetNetlinkOps().NewAttribute(VdpaAttrDevName, vdpaDeviceName)
	if err != nil {
		return err
	}

	_, err = GetNetlinkOps().RunVdpaNetlinkCmd(VdpaCmdDevDel, unix.NLM_F_ACK|unix.NLM_F_REQUEST, []*nl.RtAttr{nameAttr})
	if err != nil {
		return err
	}

	return nil
}

func parseDevLinkVdpaDevList(msgs [][]byte) ([]VdpaDevice, error) {
	devices := make([]VdpaDevice, 0, len(msgs))

	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		dev := &vdpaDev{}
		if err = dev.parseAttributes(attrs); err != nil {
			return nil, err
		}
		if err = dev.getBusInfo(); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, nil
}
