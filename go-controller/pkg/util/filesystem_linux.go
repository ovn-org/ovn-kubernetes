//go:build linux
// +build linux

package util

import (
	"os"
	"path/filepath"
)

var (
	sysClassNetDir = filepath.Join("/", "sys", "class", "net")
)

type FileSystemOps interface {
	Readlink(path string) (string, error)
}

type defaultFileSystemOps struct {
}

var fileSystemOps FileSystemOps = &defaultFileSystemOps{}

func SetFileSystemOps(mockInst FileSystemOps) {
	fileSystemOps = mockInst
}

func GetFileSystemOps() FileSystemOps {
	return fileSystemOps
}

func (defaultFileSystemOps) Readlink(path string) (string, error) {
	return os.Readlink(path)
}

// GetDeviceIDFromNetdevice retrieves device ID for passed netdevice which is PCI address for regular
// netdevice, eg. VF, or Auxiliary Device name for SF netdevice
func GetDeviceIDFromNetdevice(netdev string) (string, error) {
	path := filepath.Join(sysClassNetDir, netdev, "device")
	realPath, err := fileSystemOps.Readlink(path)
	if err != nil {
		return "", err
	}
	return filepath.Base(realPath), nil
}
