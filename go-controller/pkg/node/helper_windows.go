// +build windows

package node

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// It also returns the default gateway itself.
func getDefaultGatewayInterfaceDetails() (string, string, error) {
	// TODO: Implement this
	return "", "", fmt.Errorf("Not implemented yet on Windows")
}

func getIntfName(gatewayIntf string) (string, error) {
	// Is intfName a port of gatewayIntf?
	intfName, err := util.GetNicName(gatewayIntf)
	if err != nil {
		return "", err
	}
	_, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}
