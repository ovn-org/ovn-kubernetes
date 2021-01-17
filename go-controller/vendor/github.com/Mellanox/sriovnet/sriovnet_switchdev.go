package sriovnet

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	utilfs "github.com/Mellanox/sriovnet/pkg/utils/filesystem"
)

const (
	netdevPhysSwitchID = "phys_switch_id"
	netdevPhysPortName = "phys_port_name"
)

var physPortRe = regexp.MustCompile(`pf(\d+)vf(\d+)`)
var pfPhysPortNameRe = regexp.MustCompile(`p\d+`)

func parsePortName(physPortName string) (pfRepIndex, vfRepIndex int, err error) {
	pfRepIndex = -1
	vfRepIndex = -1

	// old kernel syntax of phys_port_name is vf index
	physPortName = strings.TrimSpace(physPortName)
	physPortNameInt, err := strconv.Atoi(physPortName)
	if err == nil {
		vfRepIndex = physPortNameInt
	} else {
		// new kernel syntax of phys_port_name pfXVfY
		matches := physPortRe.FindStringSubmatch(physPortName)
		//nolint:gomnd
		if len(matches) != 3 {
			err = fmt.Errorf("failed to parse physPortName %s", physPortName)
		} else {
			pfRepIndex, err = strconv.Atoi(matches[1])
			if err == nil {
				vfRepIndex, err = strconv.Atoi(matches[2])
			}
		}
	}
	return pfRepIndex, vfRepIndex, err
}

func isSwitchdev(netdevice string) bool {
	swIDFile := filepath.Join(NetSysDir, netdevice, netdevPhysSwitchID)
	physSwitchID, err := utilfs.Fs.ReadFile(swIDFile)
	if err != nil {
		return false
	}
	if physSwitchID != nil && string(physSwitchID) != "" {
		return true
	}
	return false
}

// GetUplinkRepresentor gets a VF PCI address (e.g '0000:03:00.4') and
// returns the uplink represntor netdev name for that VF.
func GetUplinkRepresentor(vfPciAddress string) (string, error) {
	devicePath := filepath.Join(PciSysDir, vfPciAddress, "physfn/net")
	devices, err := utilfs.Fs.ReadDir(devicePath)
	if err != nil {
		return "", fmt.Errorf("failed to lookup %s: %v", vfPciAddress, err)
	}
	for _, device := range devices {
		if isSwitchdev(device.Name()) {
			// Try to get the phys port name, if not exists then fallback to check without it
			// phys_port_name should be in formant p<port-num> e.g p0,p1,p2 ...etc.
			if devicePhysPortName, err := getNetDevPhysPortName(device.Name()); err == nil {
				if !pfPhysPortNameRe.MatchString(devicePhysPortName) {
					continue
				}
			}

			return device.Name(), nil
		}
	}
	return "", fmt.Errorf("uplink for %s not found", vfPciAddress)
}

func GetVfRepresentor(uplink string, vfIndex int) (string, error) {
	swIDFile := filepath.Join(NetSysDir, uplink, netdevPhysSwitchID)
	physSwitchID, err := utilfs.Fs.ReadFile(swIDFile)
	if err != nil || string(physSwitchID) == "" {
		return "", fmt.Errorf("cant get uplink %s switch id", uplink)
	}

	pfSubsystemPath := filepath.Join(NetSysDir, uplink, "subsystem")
	devices, err := utilfs.Fs.ReadDir(pfSubsystemPath)
	if err != nil {
		return "", err
	}
	for _, device := range devices {
		devicePath := filepath.Join(NetSysDir, device.Name())
		deviceSwIDFile := filepath.Join(devicePath, netdevPhysSwitchID)
		deviceSwID, err := utilfs.Fs.ReadFile(deviceSwIDFile)
		if err != nil || string(deviceSwID) != string(physSwitchID) {
			continue
		}
		physPortNameStr, err := getNetDevPhysPortName(device.Name())
		if err != nil {
			continue
		}
		pfRepIndex, vfRepIndex, _ := parsePortName(physPortNameStr)
		if pfRepIndex != -1 {
			pfPCIAddress, err := getPCIFromDeviceName(uplink)
			if err != nil {
				continue
			}
			PCIFuncAddress, err := strconv.Atoi(string((pfPCIAddress[len(pfPCIAddress)-1])))
			if pfRepIndex != PCIFuncAddress || err != nil {
				continue
			}
		}
		// At this point we're confident we have a representor.
		if vfRepIndex == vfIndex {
			return device.Name(), nil
		}
	}
	return "", fmt.Errorf("failed to find VF representor for uplink %s", uplink)
}

func getNetDevPhysPortName(netDev string) (string, error) {
	devicePortNameFile := filepath.Join(NetSysDir, netDev, netdevPhysPortName)
	physPortName, err := utilfs.Fs.ReadFile(devicePortNameFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(physPortName)), nil
}

// GetVfRepresentorSmartNIC returns VF representor on Smart-NIC for a host VF identified by pfID and vfIndex
func GetVfRepresentorSmartNIC(pfID, vfIndex string) (string, error) {
	// TODO(Adrianc): This method should change to get switchID and vfIndex as input, then common logic can
	// be shared with GetVfRepresentor, backward compatibility should be preserved when this happens.

	// pfID should be 0 or 1
	if pfID != "0" && pfID != "1" {
		return "", fmt.Errorf("unexpected pfID(%s). It should be 0 or 1", pfID)
	}

	// vfIndex should be an unsinged integer provided as a decimal number
	if _, err := strconv.ParseUint(vfIndex, 10, 32); err != nil {
		return "", fmt.Errorf("unexpected vfIndex(%s). It should be an unsigned decimal number", vfIndex)
	}

	netdevs, err := utilfs.Fs.ReadDir(NetSysDir)
	if err != nil {
		return "", err
	}

	// map for easy search of expected VF rep port name.
	// Note: no supoport for Multi-Chassis Smart-NICs
	expectedPhysPortNames := map[string]interface{}{
		fmt.Sprintf("pf%svf%s", pfID, vfIndex):   nil,
		fmt.Sprintf("c0pf%svf%s", pfID, vfIndex): nil,
	}

	// iterate all net devs and get phys port name
	// if phys port name == pf<pfIndex>vf<vfIndex> or c0pf<pfIndex>vf<vfIndex> we have a match
	for _, netdev := range netdevs {
		// find matching VF representor
		netdevName := netdev.Name()
		portName, err := getNetDevPhysPortName(netdevName)
		if err != nil {
			// skip
			continue
		}
		if _, ok := expectedPhysPortNames[portName]; ok {
			return netdevName, nil
		}
	}
	return "", fmt.Errorf("vf representor for pfID:%s, vfIndex: %s not found", pfID, vfIndex)
}
