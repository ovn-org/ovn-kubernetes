// +build windows

package util

import (
	"fmt"
	"strings"
)

func GetNicName(brName string) (string, error) {
	// The bridge has format "vEthernet (<nic_name>)" on Windows
	nameSplitted := strings.SplitAfterN(brName, " ", 2)
	if len(nameSplitted) != 2 {
		return "", fmt.Errorf("invalid bridge name")
	}
	nicName := fmt.Sprintf("%s", nameSplitted[1][1:len(nameSplitted[1])-1])
	return nicName, nil
}

func NicToBridge(iface string) (string, error) {
	return "", fmt.Errorf("Not implemented yet on Windows")
}

func BridgeToNic(bridge string) error {
	return fmt.Errorf("Not implemented yet on Windows")
}
