// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container/network"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
)

const (
	// nameFormat is a Go template for extracting network names from inspect output.
	nameFormat = "{{.Name}}"
)

// ContainerOps provides reusable helper methods for container lifecycle and network management.
type ContainerOps struct {
	// runtime string docker or podman
	runtime string
	// cmdRunner executes container runtime commands and returns their output.
	cmdRunner api.Runner
}

func NewContainerOps(runtime string, cmdRunner api.Runner) *ContainerOps {
	return &ContainerOps{runtime: runtime, cmdRunner: cmdRunner}
}

// CreateNetwork creates a network using the provided command runner
func (o *ContainerOps) CreateNetwork(name string, subnets ...string) error {
	exists, err := o.networkExists(name)
	if err != nil {
		return fmt.Errorf("failed to check if network %s exists: %w", name, err)
	}
	if exists {
		attachedContainers, err := o.getContainersAttachedToNetwork(name)
		if err != nil {
			framework.Logf("failed to get containers attached to network %s: %v", name, err)
		}
		if len(attachedContainers) > 0 {
			return fmt.Errorf("network %s already exists with containers attached: '%v'", name, attachedContainers)
		}
		return fmt.Errorf("network %q already exists", name)
	}
	cmdArgs := []string{"network", "create", "--internal", "--driver", "bridge", name}
	var v6 bool
	// detect if IPv6 flag is required
	for _, subnet := range subnets {
		cmdArgs = append(cmdArgs, "--subnet", subnet)
		if utilnet.IsIPv6CIDRString(subnet) {
			v6 = true
		}
	}
	if v6 {
		cmdArgs = append(cmdArgs, "--ipv6")
	}
	stdOut, err := o.run(cmdArgs...)
	if err != nil {
		return fmt.Errorf("failed to create Network with command %q: %s (%s)", strings.Join(cmdArgs, " "), err, stdOut)
	}
	return nil
}

// networkExists checks if a network exists
func (o *ContainerOps) networkExists(networkName string) (bool, error) {
	networks, err := o.ListNetworks()
	if err != nil {
		return false, err
	}
	for _, name := range networks {
		if name == networkName {
			return true, nil
		}
	}
	return false, nil
}

// getContainersAttachedToNetwork returns a list of containers attached to a network
func (o *ContainerOps) getContainersAttachedToNetwork(networkName string) ([]string, error) {
	out, err := o.run("network", "inspect", networkName)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect network %s: %w", networkName, err)
	}
	var result []networkInspect
	err = json.Unmarshal([]byte(out), &result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal network %s inspect output: %w", networkName, err)
	}
	if len(result) == 0 {
		return nil, api.NotFound
	}
	var containers []string
	for _, container := range result[0].Containers {
		containers = append(containers, container.Name)
	}
	return containers, nil
}

// AttachNetwork attaches given network with container
func (o *ContainerOps) AttachNetwork(network api.Network, container string) error {
	exists, err := o.networkExists(network.Name())
	if err != nil {
		return fmt.Errorf("failed to check if network %s exists: %w", network.Name(), err)
	}
	if !exists {
		return fmt.Errorf("network %s doesn't exist", network.Name())
	}
	attached, err := o.isNetworkAttachedToContainer(network.Name(), container)
	if err != nil {
		return err
	}
	if attached {
		return fmt.Errorf("network %s is already attached to container %s", network.Name(), container)
	}
	out, err := o.run("network", "connect", network.Name(), container)
	if err != nil {
		return fmt.Errorf("failed to attach network to container %s: %s (%s)", container, err, out)
	}
	return nil
}

// DetachNetwork disconnects given network from container
func (o *ContainerOps) DetachNetwork(network api.Network, container string) error {
	exists, err := o.networkExists(network.Name())
	if err != nil {
		return fmt.Errorf("failed to check if network %s exists: %w", network.Name(), err)
	}
	if !exists {
		return nil
	}
	attached, err := o.isNetworkAttachedToContainer(network.Name(), container)
	if !attached {
		// err may not be nil here, so return it, the caller might reattempt
		// for transient error (e.g. command timeout).
		return err
	}
	out, err := o.run("network", "disconnect", network.Name(), container)
	if err != nil {
		return fmt.Errorf("failed to disconnect network from container %s: %s (%s)", container, err, out)
	}
	return nil
}

func (o *ContainerOps) isNetworkAttachedToContainer(networkName, containerName string) (bool, error) {
	// Return false for api.NotFound errors (e.g., when the container,
	// network, or network attachment is not found, or when no IPv4/IPv6
	// addresses are assigned).
	_, err := o.GetNetworkInterface(containerName, networkName)
	if err != nil {
		if errors.Is(err, api.NotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (o *ContainerOps) GetNetworkInterface(containerName, networkName string) (api.NetworkInterface, error) {
	exists, err := o.networkExists(networkName)
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to check if network %q exists: %w", networkName, err)
	}
	if !exists {
		return api.NetworkInterface{}, fmt.Errorf("failed to find network %q: %w", networkName, api.NotFound)
	}
	exists, err = o.doesContainerNameExist(containerName)
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to check if container %q exists: %w", containerName, err)
	}
	if !exists {
		return api.NetworkInterface{}, fmt.Errorf("failed to find container %q: %w", containerName, api.NotFound)
	}
	out, err := o.run("inspect", containerName)
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to inspect container %q: %w", containerName, err)
	}
	var inspectResult []containerInspect
	err = json.Unmarshal([]byte(out), &inspectResult)
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to inspect container %q: %w", containerName, err)
	}
	if len(inspectResult) == 0 {
		return api.NetworkInterface{}, fmt.Errorf("container %q inspect returned with no result", containerName)
	}
	ep, ok := inspectResult[0].NetworkSettings.Networks[networkName]
	if !ok {
		return api.NetworkInterface{}, fmt.Errorf("network %q not found with container %q: %w", networkName, containerName, api.NotFound)
	}
	ni := api.NetworkInterface{
		IPv4:        ep.IPAddress,
		IPv4Prefix:  "",
		IPv4Gateway: ep.Gateway,
		IPv6:        ep.GlobalIPv6Address,
		IPv6Prefix:  "",
		IPv6Gateway: ep.IPv6Gateway,
		MAC:         ep.MacAddress,
	}
	// Only set prefix if we have an IP and a valid prefix length
	if ni.IPv4 != "" {
		if ep.IPPrefixLen <= 0 || ep.IPPrefixLen > 32 {
			return api.NetworkInterface{}, fmt.Errorf("invalid IPv4 prefix length %d for container %q network %q",
				ep.IPPrefixLen, containerName, networkName,
			)
		}
		ni.IPv4Prefix = strconv.Itoa(ep.IPPrefixLen)
	}

	if ni.IPv6 != "" {
		if ep.GlobalIPv6PrefixLen <= 0 || ep.GlobalIPv6PrefixLen > 128 {
			return api.NetworkInterface{}, fmt.Errorf("invalid IPv6 prefix length %d for container %q network %q",
				ep.GlobalIPv6PrefixLen, containerName, networkName,
			)
		}
		ni.IPv6Prefix = strconv.Itoa(ep.GlobalIPv6PrefixLen)
	}
	getIPFamilyFlagForIPRoute2 := func(ipStr string) (string, error) {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return "", fmt.Errorf("invalid IP address: %s", ipStr)
		}
		if utilnet.IsIPv6(ip) {
			return "-6", nil
		}
		return "-4", nil
	}

	getInterfaceNameUsingIP := func(ip string) (string, error) {
		ipFlag, err := getIPFamilyFlagForIPRoute2(ip)
		if err != nil {
			return "", fmt.Errorf("failed to get IP family flag for %s: %w", ip, err)
		}
		allInfAddrBytes, err := o.run("exec", "-i", containerName, "ip", "-br", ipFlag, "a", "sh")
		if err != nil {
			return "", fmt.Errorf("failed to find interface with IP %s on container %s with command 'ip -br a sh': err %v, out: %s", ip, containerName,
				err, allInfAddrBytes)
		}
		var ipLine string
		for _, line := range strings.Split(allInfAddrBytes, "\n") {
			if strings.Contains(line, ip) {
				ipLine = line
				break
			}
		}
		if ipLine == "" {
			return "", fmt.Errorf("failed to find IP %q within 'ip a' command on container %q:\n\n%q", ip, containerName, allInfAddrBytes)
		}
		ipLineSplit := strings.Split(ipLine, " ")
		if len(ipLineSplit) == 0 || ipLineSplit[0] == "" {
			return "", fmt.Errorf("failed to find interface name from 'ip a' output line %q", ipLine)
		}
		infNames := ipLineSplit[0]
		splitChar := " "
		if strings.Contains(infNames, "@") {
			splitChar = "@"
		}
		infNamesSplit := strings.Split(infNames, splitChar)
		if len(infNamesSplit) == 0 {
			return "", fmt.Errorf("failed to extract inf name + veth name from %q splitting by %q", infNames, splitChar)
		}
		infName := infNamesSplit[0]
		// validate its an interface name on the Node with iproute2
		out, err := o.run("exec", "-i", containerName, "ip", "link", "show", infName)
		if err != nil {
			return "", fmt.Errorf("failed to validate that interface name %q with IP %s exists in container %s: err %v, out: %s",
				infName, ip, containerName, err, out)
		}
		return infName, nil // second value is veth in 'host' netns
	}

	if ni.IPv4 != "" {
		ni.InfName, err = getInterfaceNameUsingIP(ni.IPv4)
		if err != nil {
			framework.Logf("failed to get network interface name using IPv4 address %s on container %q: %v", ni.IPv4, containerName, err)
		}
	}
	if ni.IPv6 != "" {
		ni.InfName, err = getInterfaceNameUsingIP(ni.IPv6)
		if err != nil {
			framework.Logf("failed to get network interface name using IPv6 address %s on container %q: %v", ni.IPv6, containerName, err)
		}
	}
	// fail if no IPs were found
	if ni.IPv4 == "" && ni.IPv6 == "" {
		return ni, fmt.Errorf("failed to get an IPv4 and/or IPv6 address for interface attached to container %q"+
			" and attached to network %q: %w", containerName, networkName, api.NotFound)
	}
	return ni, nil
}

func (o *ContainerOps) doesContainerNameExist(name string) (bool, error) {
	state, err := o.GetContainerState(name)
	if err != nil {
		return false, err
	}
	// Empty state means container doesn't exist
	return state != "", nil
}

func (o *ContainerOps) ListNetworks() ([]string, error) {
	output, err := o.run("network", "ls", "--format", nameFormat)
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}
	var networks []string
	for _, name := range strings.Split(strings.TrimSpace(output), "\n") {
		if name != "" {
			networks = append(networks, name)
		}
	}
	return networks, nil
}

// GetContainerState returns the state of a container by name
// Returns empty string if container doesn't exist
func (o *ContainerOps) GetContainerState(containerName string) (string, error) {
	stdOut, err := o.run("ps", "-a", "-f", fmt.Sprintf("name=^%s$", containerName), "--format", "{{.State}}")
	if err != nil {
		return "", fmt.Errorf("failed to check container state for %s: %s (%s)", containerName, err, stdOut)
	}

	state := strings.TrimSpace(stdOut)
	return state, nil
}

// GetNetwork retrieves network configuration for the specified network name.
// It inspects the network and returns its IPAM configuration, normalizing
// differences between Docker and Podman formats. Returns api.NotFound if
// the network does not exist.
func (o *ContainerOps) GetNetwork(networkName string) (network.ContainerEngineNetwork, error) {
	n := network.ContainerEngineNetwork{NetName: networkName}
	exists, err := o.networkExists(networkName)
	if err != nil {
		return n, fmt.Errorf("failed to check if network %s exists: %w", networkName, err)
	}
	if !exists {
		return n, api.NotFound
	}
	out, err := o.run("network", "inspect", networkName)
	if err != nil {
		return n, fmt.Errorf("failed to inspect network %s: %w", networkName, err)
	}
	var result []networkInspect
	err = json.Unmarshal([]byte(out), &result)
	if err != nil {
		return n, fmt.Errorf("failed to unmarshal network %s inspect output: %w", networkName, err)
	}
	if len(result) == 0 {
		return n, fmt.Errorf("network %q inspect returned empty result: %w", networkName, api.NotFound)
	}
	// Normalize Docker vs Podman
	configs := result[0].IPAM.Config
	if len(configs) == 0 {
		configs = result[0].Subnets // Podman
	}
	if len(configs) == 0 {
		return n, fmt.Errorf("failed to find any IPAM configuration for network %s", networkName)
	}
	// validate configs
	for _, config := range configs {
		if config.Subnet == "" {
			return n, fmt.Errorf("network %s contains invalid subnet config", networkName)
		}
	}
	n.Configs = configs
	return n, nil
}

func (o *ContainerOps) DeleteNetwork(network api.Network) error {
	return wait.PollImmediate(1*time.Second, 10*time.Second, func() (done bool, err error) {
		exists, err := o.networkExists(network.Name())
		if err != nil {
			framework.Logf("failed to check if network %s exists: %v", network.Name(), err)
			return false, nil
		}
		if !exists {
			return true, nil
		}
		// ensure all containers are disconnected from the network and if any are found, disconnect it.
		containers, err := o.getContainersAttachedToNetwork(network.Name())
		if err != nil {
			framework.Logf("failed to list attached containers for network %s:, err: %v", network, err)
			return false, nil
		}
		for _, container := range containers {
			if err = o.DetachNetwork(network, container); err != nil {
				framework.Logf("while trying to delete network %q, attempted to detach container %q that is "+
					"still attached to network", network.Name(), container)
				return false, nil
			}
		}
		stdOut, err := o.run("network", "rm", network.Name())
		if err != nil {
			framework.Logf("failed to delete network %s: %s (%s)", network.Name(), err, stdOut)
			return false, nil
		}
		return true, nil
	})
}

func (o *ContainerOps) CreateExternalContainer(container api.ExternalContainer) (res api.ExternalContainer, retErr error) {
	if valid, err := container.IsValidPreCreateContainer(); !valid {
		return container, err
	}
	exists, err := o.doesContainerNameExist(container.Name)
	if err != nil {
		return container, fmt.Errorf("failed to check if external container %s exists: %w", container.Name, err)
	}
	if exists {
		return container, fmt.Errorf("external container %s already exists", container.Name)
	}
	cmd := []string{"run", "-itd", "--init", "--privileged", "--name", container.Name, "--network", container.Network.Name(), "--hostname", container.Name}
	if container.IPv4 != "" {
		cmd = append(cmd, "--ip", container.IPv4)
	}
	if container.IPv6 != "" {
		cmd = append(cmd, "--ip6", container.IPv6)
	}
	if container.Entrypoint != "" {
		cmd = append(cmd, "--entrypoint", container.Entrypoint)
	}
	cmd = append(cmd, container.RuntimeArgs...)
	cmd = append(cmd, container.Image)
	if len(container.CmdArgs) > 0 {
		cmd = append(cmd, container.CmdArgs...)
	} else {
		if images.AgnHost() == container.Image {
			cmd = append(cmd, "pause")
		}
	}
	framework.Logf("creating external container with command: %q\n", strings.Join(cmd, " "))
	stdOut, err := o.run(cmd...)
	if err != nil {
		return container, fmt.Errorf("failed to create external container %s: %s (%s)", container, err, stdOut)
	}
	// The container exists from here on, but callers only register their normal
	// cleanup once this function succeeds, so remove it on any later failure.
	defer func() {
		if retErr == nil {
			return
		}
		if delErr := o.DeleteExternalContainer(container); delErr != nil {
			framework.Logf("failed to remove external container %s after create failure: %v (original error: %v)",
				container.Name, delErr, retErr)
		}
	}()
	// fetch IPs for the attached container network. Host networked and --network none containers do not expose IP information.
	if container.Network != nil && !isHostNetworked(container.Network.Name()) {
		err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 360*time.Second, true, func(ctx context.Context) (done bool, err error) {
			ni, err := o.GetNetworkInterface(container.Name, container.Network.Name())
			if err != nil {
				framework.Logf("attempt to get external container %s network interface attached to network %s failed: %v, retrying...", container.Name, container.Network.Name(), err)
				return false, nil
			}
			if ni.GetIPv4() == "" && ni.GetIPv6() == "" {
				return false, nil
			}
			container.IPv4 = ni.GetIPv4()
			container.IPv6 = ni.GetIPv6()
			return true, nil
		})
		if err != nil {
			return container, fmt.Errorf("failed to get network interface information: %w", err)
		}
	}

	if valid, err := container.IsValidPostCreate(); !valid {
		return container, err
	}
	return container, nil
}

func (o *ContainerOps) DeleteExternalContainer(container api.ExternalContainer) error {
	// check if it is present before deleting
	exists, err := o.doesContainerNameExist(container.Name)
	if err != nil {
		return fmt.Errorf("failed to check if external container %s exists: %w", container.Name, err)
	}
	if !exists {
		return nil
	}
	stdOut, err := o.run("rm", "-f", container.Name)
	if err != nil {
		return fmt.Errorf("failed to delete external container (%s): %v (%s)", container, err, stdOut)
	}
	err = wait.ExponentialBackoff(wait.Backoff{Duration: 1 * time.Second, Factor: 5, Steps: 5}, wait.ConditionFunc(func() (done bool, err error) {
		stdOut, err = o.run("ps", "-a", "-f", fmt.Sprintf("name=^%s$", container.Name), "-q")
		if err != nil {
			return false, fmt.Errorf("failed to check if external container (%s) is deleted: %v (%s)", container, err, stdOut)
		}
		if string(stdOut) != "" {
			framework.Logf("Waiting for external container %s to be deleted", container.Name)
			return false, nil
		}
		return true, nil
	}))
	if err != nil {
		return fmt.Errorf("failed to delete external container (%s): %v", container, err)
	}
	return nil
}

func (o *ContainerOps) ExecContainerCommand(name string, cmd []string) (string, error) {
	exists, err := o.doesContainerNameExist(name)
	if err != nil {
		return "", fmt.Errorf("failed to check if container %q exists: %w", name, err)
	}
	if !exists {
		return "", fmt.Errorf("cannot exec into container %q because it doesn't exist: %w", name, api.NotFound)
	}
	cmdArgs := append([]string{"exec", name}, cmd...)
	out, err := o.run(cmdArgs...)
	if err != nil {
		return "", fmt.Errorf("failed to exec container command (%s): err: %v, stdout: %q", strings.Join(cmdArgs, " "), err, out)
	}
	return out, nil
}

func (o *ContainerOps) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	exists, err := o.doesContainerNameExist(container.Name)
	if err != nil {
		return "", fmt.Errorf("failed to check if external container %q exists: %w", container.Name, err)
	}
	if !exists {
		return "", fmt.Errorf("external container %q doesn't exist, therefore no logs can be retrieved: %w", container.Name, api.NotFound)
	}
	stdOut, err := o.run("logs", container.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get logs of external container (%s): %v (%s)", container, err, stdOut)
	}
	return stdOut, nil
}

func (o *ContainerOps) StopContainer(name string) error {
	state, err := o.GetContainerState(name)
	if err != nil {
		return err
	}

	if state == "" {
		return fmt.Errorf("cannot shutdown container %q because it doesn't exist: %w", name, api.NotFound)
	}

	// If container is already stopped/exited, consider it success
	if state == "exited" || state == "stopped" {
		framework.Logf("Container %s is already stopped (state: %s)", name, state)
		return nil
	}

	framework.Logf("Shutting down container %s (current state: %s)", name, state)
	stdOut, err := o.run("stop", name)
	if err != nil {
		return fmt.Errorf("failed to shutdown container %s: %s (%s)", name, err, stdOut)
	}
	framework.Logf("Successfully shut down container %s", name)
	return nil
}

func (o *ContainerOps) StartContainer(name string) error {
	state, err := o.GetContainerState(name)
	if err != nil {
		return err
	}

	if state == "" {
		return fmt.Errorf("cannot start container %q because it doesn't exist: %w", name, api.NotFound)
	}

	// If container is already running, consider it success
	if state == "running" || state == "up" {
		framework.Logf("Container %s is already running (state: %s)", name, state)
		return nil
	}

	framework.Logf("Starting container %s (current state: %s)", name, state)
	stdOut, err := o.run("start", name)
	if err != nil {
		return fmt.Errorf("failed to start container %s: %s (%s)", name, err, stdOut)
	}
	framework.Logf("Successfully started container %s", name)
	return nil
}

func (o *ContainerOps) run(args ...string) (string, error) {
	return o.cmdRunner.Run(o.runtime, args...)
}

func isHostNetworked(networkName string) bool {
	return networkName == "host"
}

// networkInspect represents the JSON output from 'docker/podman network inspect'
// Docker uses IPAM.Config, while Podman uses subnets at the top level
type networkInspect struct {
	Name       string                                 `json:"Name"`
	IPAM       IPAMConfig                             `json:"IPAM"`              // Docker
	Subnets    []network.ContainerEngineNetworkConfig `json:"subnets,omitempty"` // Podman
	Containers map[string]NetworkContainerInfo        `json:"Containers"`
}

type IPAMConfig struct {
	Config []network.ContainerEngineNetworkConfig `json:"Config"`
}

type NetworkContainerInfo struct {
	Name string `json:"Name"`
}

// containerInspect represents the JSON output from 'docker/podman inspect'
// for a container.
type containerInspect struct {
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
}

type EndpointSettings struct {
	Gateway             string `json:"Gateway"`
	IPAddress           string `json:"IPAddress"`
	IPPrefixLen         int    `json:"IPPrefixLen"`
	IPv6Gateway         string `json:"IPv6Gateway"`
	GlobalIPv6Address   string `json:"GlobalIPv6Address"`
	GlobalIPv6PrefixLen int    `json:"GlobalIPv6PrefixLen"`
	MacAddress          string `json:"MacAddress"`
}
