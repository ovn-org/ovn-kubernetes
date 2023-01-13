package kvdpa

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	virtioDevDir = "/sys/bus/virtio/devices"
)

// VirtioNet is the virtio-net device information
type VirtioNet interface {
	Name() string
	NetDev() string
}

// virtioNet implements VirtioNet interface
type virtioNet struct {
	name   string
	netDev string
}

// Name returns the virtio device's name (as appears in the virtio bus)
func (v *virtioNet) Name() string {
	return v.name
}

// NetDev returns the virtio-net netdev name
func (v *virtioNet) NetDev() string {
	return v.netDev
}

// GetVirtioNetInPath returns the VirtioNet found in the provided parent device's path
func GetVirtioNetInPath(parentPath string) (VirtioNet, error) {
	fd, err := os.Open(parentPath)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	fileInfos, err := fd.Readdir(-1)
	if err != nil {
		return nil, err
	}
	for _, file := range fileInfos {
		if strings.Contains(file.Name(), "virtio") &&
			file.IsDir() {
			virtioDevPath := filepath.Join(virtioDevDir, file.Name())
			if _, err := os.Stat(virtioDevPath); os.IsNotExist(err) {
				return nil, fmt.Errorf("virtio device %s does not exist", virtioDevPath)
			}
			var netdev string
			// Read the "net" directory in the virtio device path
			netDeviceFiles, err := os.ReadDir(filepath.Join(virtioDevPath, "net"))
			if err == nil && len(netDeviceFiles) == 1 {
				netdev = strings.TrimSpace(netDeviceFiles[0].Name())
			}
			return &virtioNet{
				name:   file.Name(),
				netDev: netdev,
			}, nil
		}
	}
	return nil, fmt.Errorf("no VirtioNet device found in path %s", parentPath)
}
