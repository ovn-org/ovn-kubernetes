package kvdpa

import (
	"errors"
	"strings"
)

// ExtractBusAndMgmtDevice extracts the busName and deviceName from a full device address (e.g. pci)
// example 1: pci/65:0000.1   -> "pci", "65:0000.1",  nil
// example 2: vdpa_sim        -> "",    "vdpa_sim",   nil
// example 3: pci/65:0000.1/1 -> "",    "",           err
func ExtractBusAndMgmtDevice(fullMgmtDeviceName string) (busName string, mgmtDeviceName string, err error) {
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
