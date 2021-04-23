package kvdpa

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

/*Exported constants */
const (
	VhostVdpaDriver  = "vhost_vdpa"
	VirtioVdpaDriver = "virtio_vdpa"
)

/*Private constants */
const (
	vdpaBusDevDir   = "/sys/bus/vdpa/devices"
	pciBusDevDir    = "/sys/bus/pci/devices"
	vdpaVhostDevDir = "/dev"
	virtioDevDir    = "/sys/bus/virtio/devices"
	rootDevDir      = "/sys/devices"
)

/*VdpaDevice contains information about a Vdpa Device*/
type VdpaDevice interface {
	GetDriver() string
	GetParent() string
	GetPath() string
	GetNetDev() string
}

/*vdpaDevimplements VdpaDevice interface */
type vdpaDev struct {
	name   string
	driver string
	path   string // Path of the vhost or virtio device
	netdev string // VirtioNet netdev (only for virtio-vdpa devices)
}

func (vd *vdpaDev) GetDriver() string {
	return vd.driver
}

func (vd *vdpaDev) GetParent() string {
	return vd.name
}

func (vd *vdpaDev) GetPath() string {
	return vd.path
}

func (vd *vdpaDev) GetNetDev() string {
	return vd.netdev
}

/*GetVdpaDeviceList returns a list of all available vdpa devices */
func GetVdpaDeviceList() ([]VdpaDevice, error) {
	vdpaDevList := make([]VdpaDevice, 0)
	fd, err := os.Open(vdpaBusDevDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	fileInfos, err := fd.Readdir(-1)
	if err != nil {
		return nil, err
	}
	var errors []string
	for _, file := range fileInfos {
		if vdpaDev, err := GetVdpaDeviceByName(file.Name()); err != nil {
			errors = append(errors, err.Error())
		} else {
			vdpaDevList = append(vdpaDevList, vdpaDev)
		}
	}

	if len(errors) > 0 {
		return vdpaDevList, fmt.Errorf(strings.Join(errors, ";"))
	}
	return vdpaDevList, nil
}

/*GetVdpaDeviceByName returns the vdpa device information by a vdpa device name */
func GetVdpaDeviceByName(name string) (VdpaDevice, error) {
	var err error
	var path string
	var netdev string

	driverLink, err := os.Readlink(filepath.Join(vdpaBusDevDir, name, "driver"))
	if err != nil {
		return nil, err
	}

	driver := filepath.Base(driverLink)
	switch driver {
	case VhostVdpaDriver:
		path, err = getVhostVdpaDev(name)
		if err != nil {
			return nil, err
		}
	case VirtioVdpaDriver:
		path, err = getVirtioVdpaDev(name)
		if err != nil {
			return nil, err
		}
		virtioNetDir := filepath.Join(path, "net")
		netDeviceFiles, err := ioutil.ReadDir(virtioNetDir)
		if err != nil || len(netDeviceFiles) != 1 {
			return nil, fmt.Errorf("failed to get network device name from vdpa device in %v %v", name, err)
		}
		netdev = strings.TrimSpace(netDeviceFiles[0].Name())
	default:
		return nil, fmt.Errorf("Unknown vdpa bus driver %s", driver)
	}

	return &vdpaDev{
		name:   name,
		driver: driver,
		path:   path,
		netdev: netdev,
	}, nil
}

/* Finds the vhost vdpa device of a vdpa device and returns it's path */
func getVhostVdpaDev(name string) (string, error) {
	file := filepath.Join(vdpaBusDevDir, name)
	fd, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	fileInfos, err := fd.Readdir(-1)
	for _, file := range fileInfos {
		if strings.Contains(file.Name(), "vhost-vdpa") &&
			file.IsDir() {
			devicePath := filepath.Join(vdpaVhostDevDir, file.Name())
			info, err := os.Stat(devicePath)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeDevice == 0 {
				return "", fmt.Errorf("vhost device %s is not a valid device", devicePath)
			}
			return devicePath, nil
		}
	}
	return "", fmt.Errorf("vhost device not found for vdpa device %s", name)
}

/*GetVdpaDeviceByPci returns the vdpa device information corresponding to a PCI device*/
/* Based on the following directory hiearchy:
/sys/bus/pci/devices/{PCIDev}/
    /vdpa{N}/

/sys/bus/vdpa/devices/vdpa{N} -> ../../../devices/pci.../{PCIDev}/vdpa{N}
*/
func GetVdpaDeviceByPci(pciAddr string) (VdpaDevice, error) {
	path, err := filepath.EvalSymlinks(filepath.Join(pciBusDevDir, pciAddr))
	if err != nil {
		return nil, err
	}
	fd, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	fileInfos, err := fd.Readdir(-1)
	for _, file := range fileInfos {
		if strings.Contains(file.Name(), "vdpa") {
			parent, err := getParentDevice(filepath.Join(vdpaBusDevDir, file.Name()))
			if err != nil {
				return nil, err
			}
			if parent != path {
				return nil, fmt.Errorf("vdpa device %s parent (%s) does not match containing dir (%s)",
					file.Name(), parent, path)
			}
			return GetVdpaDeviceByName(file.Name())
		}
	}
	return nil, fmt.Errorf("PCI address %s does not contain a vdpa device", pciAddr)
}

/* Finds the virtio vdpa device of a vdpa device and returns it's path
Currently, PCI-based devices have the following sysfs structure:
/sys/bus/vdpa/devices/
    vdpa1 -> ../../../devices/pci0000:00/0000:00:03.2/0000:05:00.2/vdpa1

In order to find the virtio device we look for virtio* devices inside the parent device:
	sys/devices/pci0000:00/0000:00:03.2/0000:05:00.2/virtio{N}

We also check the virtio device exists in the virtio bus:
/sys/bus/virtio/devices
    virtio{N} -> ../../../devices/pci0000:00/0000:00:03.2/0000:05:00.2/virtio{N}
*/
func getVirtioVdpaDev(name string) (string, error) {
	vdpaDevicePath := filepath.Join(vdpaBusDevDir, name)
	parentPath, err := getParentDevice(vdpaDevicePath)
	if err != nil {
		return "", err
	}

	fd, err := os.Open(parentPath)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	fileInfos, err := fd.Readdir(-1)
	for _, file := range fileInfos {
		if strings.Contains(file.Name(), "virtio") &&
			file.IsDir() {
			virtioDevPath := filepath.Join(virtioDevDir, file.Name())
			if _, err := os.Stat(virtioDevPath); os.IsNotExist(err) {
				return "", fmt.Errorf("virtio device %s does not exist", virtioDevPath)
			}
			return virtioDevPath, nil
		}
	}

	return "", fmt.Errorf("virtio device not found for vdpa device %s", name)
}

/* getParentDevice returns the parent's path of a vdpa device path */
func getParentDevice(path string) (string, error) {
	devicePath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	parent := filepath.Dir(devicePath)
	// if the "parent" is sys/devices, we have reached the "root" device
	if parent == rootDevDir {
		return devicePath, nil
	}
	return parent, nil
}
