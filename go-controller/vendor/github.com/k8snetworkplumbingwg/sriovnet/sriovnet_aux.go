/*----------------------------------------------------
 *
 *  2022 NVIDIA CORPORATION & AFFILIATES
 *
 *  Licensed under the Apache License, Version 2.0 (the License);
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an AS IS BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 *----------------------------------------------------
 */

package sriovnet

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	utilfs "github.com/k8snetworkplumbingwg/sriovnet/pkg/utils/filesystem"
)

// GetNetDeviceFromAux gets auxiliary device name (e.g 'mlx5_core.sf.2') and
// returns the correlate netdevice
func GetNetDevicesFromAux(auxDev string) ([]string, error) {
	auxDir := filepath.Join(AuxSysDir, auxDev, "net")
	return getFileNamesFromPath(auxDir)
}

// GetSfIndexByAuxDev gets a SF device name (e.g 'mlx5_core.sf.2') and
// returns the correlate SF index.
func GetSfIndexByAuxDev(auxDev string) (int, error) {
	sfNumFile := filepath.Join(AuxSysDir, auxDev, "sfnum")
	if _, err := utilfs.Fs.Stat(sfNumFile); err != nil {
		return -1, fmt.Errorf("cannot get sfnum for %s device: %v", auxDev, err)
	}

	sfNumStr, err := utilfs.Fs.ReadFile(sfNumFile)
	if err != nil {
		return -1, fmt.Errorf("cannot read sfnum file for %s device: %v", auxDev, err)
	}

	sfnum, err := strconv.Atoi(strings.TrimSpace(string(sfNumStr)))
	if err != nil {
		return -1, err
	}
	return sfnum, nil
}

// GetPfPciFromAux retrieves the parent PF PCI address of the provided auxiliary device in D.T.f format
func GetPfPciFromAux(auxDev string) (string, error) {
	auxPath := filepath.Join(AuxSysDir, auxDev)
	absoluteAuxPath, err := utilfs.Fs.Readlink(auxPath)
	if err != nil {
		return "", fmt.Errorf("failed to read auxiliary link, provided device ID may be not auxiliary device. %v", err)
	}
	// /sys/bus/auxiliary/devices/mlx5_core.sf.7 ->
	//		./../../devices/pci0000:00/0000:00:00.0/0000:01:00.0/0000:02:00.0/0000:03:00.0/mlx5_core.sf.7
	parent := filepath.Dir(absoluteAuxPath)
	base := filepath.Base(parent)
	for !pciAddressRe.MatchString(base) {
		// it's a nested auxiliary device. repeat
		parent = filepath.Dir(parent)
		base = filepath.Base(parent)
	}
	if base == "" {
		return base, fmt.Errorf("could not find PF PCI Address")
	}
	return base, err
}

// GetUplinkRepresentorFromAux gets auxiliary device name (e.g 'mlx5_core.sf.2') and
// returns the uplink representor netdev name for device.
func GetUplinkRepresentorFromAux(auxDev string) (string, error) {
	pfPci, err := GetPfPciFromAux(auxDev)
	if err != nil {
		return "", fmt.Errorf("failed to find uplink PCI device: %v", err)
	}

	return GetUplinkRepresentor(pfPci)
}

// GetAuxNetDevicesFromPci returns a list of auxiliary devices names for the specified PCI network device
func GetAuxNetDevicesFromPci(pciAddr string) ([]string, error) {
	baseDev := filepath.Join(PciSysDir, pciAddr)
	// ensure that "net" folder exists, meaning it is network PCI device
	if _, err := utilfs.Fs.Stat(filepath.Join(baseDev, "net")); err != nil {
		return nil, err
	}

	files, err := utilfs.Fs.ReadDir(baseDev)
	if err != nil {
		return nil, err
	}

	auxDevs := make([]string, 0)
	for _, file := range files {
		if auxiliaryDeviceRe.MatchString(file.Name()) {
			auxDevs = append(auxDevs, file.Name())
		}
	}
	return auxDevs, nil
}
