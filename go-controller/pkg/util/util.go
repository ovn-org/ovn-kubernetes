package util

import (
	"fmt"
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
