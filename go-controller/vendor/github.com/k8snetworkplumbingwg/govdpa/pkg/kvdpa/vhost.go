package kvdpa

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VhostVdpa is the vhost-vdpa device information
type VhostVdpa interface {
	Name() string
	Path() string
}

// vhostVdpa implements VhostVdpa interface
type vhostVdpa struct {
	name string
	path string
}

// Name returns the vhost device's name
func (v *vhostVdpa) Name() string {
	return v.name
}

// Name returns the vhost device's path
func (v *vhostVdpa) Path() string {
	return v.path
}

// GetVhostVdpaDevInPath returns the VhostVdpa found in the provided parent device's path
func GetVhostVdpaDevInPath(parentPath string) (VhostVdpa, error) {
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
		if strings.Contains(file.Name(), "vhost-vdpa") &&
			file.IsDir() {
			devicePath := filepath.Join(vdpaVhostDevDir, file.Name())
			info, err := os.Stat(devicePath)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeDevice == 0 {
				return nil, fmt.Errorf("vhost device %s is not a valid device", devicePath)
			}
			return &vhostVdpa{
				name: file.Name(),
				path: devicePath,
			}, nil
		}
	}
	return nil, fmt.Errorf("no VhostVdpa device foiund in path  %s", parentPath)
}
