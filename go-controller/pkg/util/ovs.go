package util

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
)

const (
	ovsCommandTimeout = 5
	ovsVsctlCommand   = "ovs-vsctl"
	ovsOfctlCommand   = "ovs-ofctl"
	ovnNbctlCommand   = "ovn-nbctl"
	osRelease         = "/etc/os-release"
	rhel              = "RHEL"
	ubuntu            = "Ubuntu"
	windowsOS         = "windows"
	ovnHostOptFile    = "/etc/default/ovn-host"
)

// PathExist checks the path exist or not.
func PathExist(path string) bool {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

func runningPlatform() (string, error) {
	if runtime.GOOS == windowsOS {
		return windowsOS, nil
	}
	fileContents, err := ioutil.ReadFile(osRelease)
	if err != nil {
		return "", fmt.Errorf("failed to parse file %s (%v)", osRelease, err)
	}

	var platform string
	ss := strings.Split(string(fileContents), "\n")
	for _, pair := range ss {
		keyValue := strings.Split(pair, "=")
		if len(keyValue) == 2 {
			if keyValue[0] == "Name" || keyValue[0] == "NAME" {
				platform = keyValue[1]
				break
			}
		}
	}

	if platform == "" {
		return "", fmt.Errorf("failed to find the platform name")
	}

	if strings.Contains(platform, "Fedora") ||
		strings.Contains(platform, rhel) || strings.Contains(platform, "CentOS") {
		return rhel, nil
	} else if strings.Contains(platform, "Debian") ||
		strings.Contains(platform, ubuntu) {
		return ubuntu, nil
	} else if strings.Contains(platform, "VMware") {
		return "Photon", nil
	}
	return "", fmt.Errorf("Unknown platform")
}

// StartOVS starts OVS service
func StartOVS() error {
	platform, err := runningPlatform()
	if err != nil {
		return err
	}
	if platform == rhel {
		out, err := exec.Command("systemctl", "start",
			"openvswitch").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting openvswitch "+
				"service: %v\n  %q", err, string(out))
		}
	} else if platform == ubuntu {
		out, err := exec.Command("service", "openvswitch-switch",
			"start").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting openvswitch "+
				"service: %v\n  %q", err, string(out))
		}
	} else if platform == windowsOS {
		out, err := exec.Command("powershell", "Start-Service",
			"ovs*").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting openvswitch "+
				"service: %v\n  %q", err, string(out))
		}
	}
	return nil
}

// StartOvnNorthd starts ovn-northd
func StartOvnNorthd() error {
	platform, err := runningPlatform()
	if err != nil {
		return err
	}
	if platform == rhel {
		out, err := exec.Command("systemctl", "start",
			"ovn-northd").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting ovn-northd "+
				"service: %v\n  %q", err, string(out))
		}
	} else if platform == ubuntu {
		out, err := exec.Command("service", "ovn-central",
			"start").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting ovn-central "+
				"service: %v\n  %q", err, string(out))
		}
	}
	return nil
}

func persistOvnControllerOptions(clientAuth *config.OvnDBAuth) error {
	fileBytes, err := ioutil.ReadFile(ovnHostOptFile)
	// if the file doesn't exist, then we will create the file as part of ioutil.Writefile() call later
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	index := 0
	found := false
	lines := []string{}
	if len(fileBytes) != 0 {
		lines = strings.Split(string(fileBytes), "\n")
	}

	for i, line := range lines {
		line = strings.TrimLeft(line, "\n\t\r")
		if !strings.HasPrefix(line, "OVN_CTL_OPTS") {
			continue
		}
		index = i
		found = true
		break
	}

	ovnCtlOpt := fmt.Sprintf("OVN_CTL_OPTS=\"--ovn-controller-ssl-key=%s "+
		"--ovn-controller-ssl-cert=%s "+
		"--ovn-controller-ssl-bootstrap-ca-cert=%s\"\n",
		clientAuth.PrivKey,
		clientAuth.Cert,
		clientAuth.CACert)
	if !found {
		lines = append(lines, ovnCtlOpt)
	} else {
		lines[index] = ovnCtlOpt
	}
	// Note that if "/etc/default" directory itself is missing, then we error out here
	// and we don't want to go about creating system directory with correct permissions
	return ioutil.WriteFile(ovnHostOptFile, []byte(strings.Join(lines, "\n")), 0644)
}

// RestartOvnController restarts ovn-controller
func RestartOvnController(clientAuth *config.OvnDBAuth) error {
	platform, err := runningPlatform()
	if err != nil {
		return err
	}
	if clientAuth.Scheme == config.OvnDBSchemeSSL && (platform == rhel || platform == ubuntu) {
		if err := persistOvnControllerOptions(clientAuth); err != nil {
			return fmt.Errorf("error persisting OVN client certificate info in %s",
				ovnHostOptFile)
		}
	}
	if platform == rhel {
		out, err := exec.Command("systemctl", "restart",
			"ovn-controller").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting ovn-controller "+
				"service: %v\n  %q", err, string(out))
		}
	} else if platform == ubuntu {
		out, err := exec.Command("service", "ovn-host",
			"restart").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting ovn-host "+
				"service: %v\n  %q", err, string(out))
		}
	} else if platform == windowsOS {
		out, err := exec.Command("powershell", "Restart-Service",
			"ovn-controller").CombinedOutput()
		if err != nil {
			return fmt.Errorf("error starting ovn-controller "+
				"service: %v\n  %q", err, string(out))
		}
	}
	return nil
}

// RunOVSOfctl runs a command via ovs-ofctl.
func RunOVSOfctl(args ...string) (string, string, error) {
	cmdPath, err := exec.LookPath(ovsOfctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := exec.Command(cmdPath, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(stdout.String(), "\" \n"), stderr.String(), err
}

// RunOVSVsctl runs a command via ovs-vsctl.
func RunOVSVsctl(args ...string) (string, string, error) {
	cmdPath, err := exec.LookPath(ovsVsctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmdArgs := []string{fmt.Sprintf("--timeout=%d", ovsCommandTimeout)}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNbctlWithTimeout runs command via ovs-nbctl with a specific timeout
func RunOVNNbctlWithTimeout(timeout int, args ...string) (string, string,
	error) {
	cmdPath, err := exec.LookPath(ovnNbctlCommand)
	if err != nil {
		return "", "", err
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	var cmd *exec.Cmd
	if config.OvnNorth.ClientAuth.Scheme == config.OvnDBSchemeSSL {
		cmdArgs := []string{
			fmt.Sprintf("--private-key=%s", config.OvnNorth.ClientAuth.PrivKey),
			fmt.Sprintf("--certificate=%s", config.OvnNorth.ClientAuth.Cert),
			fmt.Sprintf("--bootstrap-ca-cert=%s", config.OvnNorth.ClientAuth.CACert),
			fmt.Sprintf("--db=%s", config.OvnNorth.ClientAuth.GetURL()),
			fmt.Sprintf("--timeout=%d", timeout),
		}
		cmdArgs = append(cmdArgs, args...)
		cmd = exec.Command(cmdPath, cmdArgs...)
	} else {
		cmdArgs := []string{
			fmt.Sprintf("--db=%s", config.OvnNorth.ClientAuth.GetURL()),
			fmt.Sprintf("--timeout=%d", timeout),
		}
		cmdArgs = append(cmdArgs, args...)
		cmd = exec.Command(cmdPath, cmdArgs...)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	return strings.Trim(strings.TrimSpace(stdout.String()), "\""), stderr.String(), err
}

// RunOVNNbctl runs a command via ovs-nbctl.
func RunOVNNbctl(args ...string) (string, string, error) {
	return RunOVNNbctlWithTimeout(ovsCommandTimeout, args...)
}
