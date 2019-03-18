package util

import (
	"fmt"
	kapi "k8s.io/api/core/v1"
	"os/exec"
	"strings"

	"github.com/urfave/cli"
)

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// FetchIfMacWindows gets the mac of the interfaceName via powershell commands
// There is a known issue with OVS not correctly picking up the
// physical network interface MAC address.
func FetchIfMacWindows(interfaceName string) (string, error) {
	stdoutStderr, err := exec.Command("powershell", "$(Get-NetAdapter", "-IncludeHidden",
		"-InterfaceAlias", fmt.Sprintf("\"%s\"", interfaceName), ").MacAddress").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Failed to get mac address of ovn-k8s-master, stderr: %q, error: %v", fmt.Sprintf("%s", stdoutStderr), err)
	}
	// Windows returns it in 00-00-00-00-00-00 format, we want ':' instead of '-'
	macAddress := strings.Replace(strings.TrimSpace(fmt.Sprintf("%s", stdoutStderr)), "-", ":", -1)

	return strings.ToLower(macAddress), nil
}

// IsServiceIPSet checks if the service is an headless service or not
func IsServiceIPSet(service *kapi.Service) bool {
	return service.Spec.ClusterIP != kapi.ClusterIPNone && service.Spec.ClusterIP != ""
}

// GetK8sMgmtIntfName returns the correct length interface name to be used
// as an OVS internal port on the node
func GetK8sMgmtIntfName(nodeName string) string {
	if len(nodeName) > 11 {
		return "k8s-" + (nodeName[:11])
	}
	return "k8s-" + nodeName
}
