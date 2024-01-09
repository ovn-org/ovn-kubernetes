//go:build linux
// +build linux

package util

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/onsi/ginkgo"
)

const (
	sysClassNetDir          = "/sys/class/net"
	PortRangeKernelFilePath = "/proc/sys/net/ipv4/ip_local_port_range"
	portRangeMin            = 1
	portRangeMax            = 65535
)

type FileSystemOps interface {
	Readlink(path string) (string, error)
	ReadFile(path string) (string, error)
}

type defaultFileSystemOps struct {
}

var fileSystemOps FileSystemOps = &defaultFileSystemOps{}

func SetFileSystemOps(mockInst FileSystemOps) {
	fileSystemOps = mockInst
}

func GetDefaultFileSystemOps() FileSystemOps {
	return &defaultFileSystemOps{}
}

func MockPortRangeFileSystemOps(t ginkgo.GinkgoTInterface, startRange, endRange int) {
	m := mocks.NewFileSystemOps(t)
	m.Mock.On("ReadFile", PortRangeKernelFilePath).Return(fmt.Sprintf("%d\t%d", startRange, endRange), nil)
	SetFileSystemOps(m)
}

func (defaultFileSystemOps) Readlink(path string) (string, error) {
	return os.Readlink(path)
}

func (defaultFileSystemOps) ReadFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
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

// GetExternalPortRange attempts to get external port range from the TCP/UDP local port range (net.ipv4.ip_local_port_range)
// and converts it into a format consumable by NB DB NAT field external_port_range.
func GetExternalPortRange() (string, error) {
	portRange, err := fileSystemOps.ReadFile(PortRangeKernelFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	var start, end int
	n, err := fmt.Sscanf(portRange, "%d\t%d", &start, &end)
	if err != nil {
		return "", fmt.Errorf("failed to parse contents: %v", err)
	}
	if n != 2 {
		return "", fmt.Errorf("unexpected number of items parsed. Expected 2 but found %d", n)
	}
	if start > end || start > portRangeMax || end < portRangeMin {
		return "", fmt.Errorf("invalid range found: %d-%d", start, end)
	}
	return fmt.Sprintf("%d-%d", start, end), nil
}
