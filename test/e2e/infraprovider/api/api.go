// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"strings"

	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"

	"k8s.io/kubernetes/test/e2e/framework"
)

// Provider represents the infrastructure provider
type Provider interface {
	ExternalContainerProvider
	ClusterProvider
	// NewTestContext providers a per test sandbox. Dependent infra test constructs are created within each test and automatically cleaned
	// after each test.
	NewTestContext() Context
}

type ClusterProvider interface {
	// PrimaryNetwork returns OVN-Kubernetes primary infrastructure network information
	PrimaryNetwork() (Network, error)
	GetK8NodeNetworkInterface(instance string, network Network) (NetworkInterface, error)
	// ExecK8NodeCommand executes a command on a K8 Node host network namespace and filesystem
	ExecK8NodeCommand(nodeName string, cmd []string) (string, error)
	// GetK8HostPort returns a Node port. Requesting a port that maybe exposed in tests to avoid multiple parallel
	// tests utilizing conflicting ports. It also allows infra provider implementations to set Nodes
	// allowed port range and therefore comply with cloud provider firewall rules.
	GetK8HostPort() uint16 // supported K8 host ports
	// ShutdownNode shuts down the specified node
	ShutdownNode(nodeName string) error
	// StartNode starts the specified node
	StartNode(nodeName string) error
	// PreloadImages pulls the given images and loads them into the cluster
	// so that they are available to pods without a runtime pull. Providers
	// that do not support preloading may implement this as a no-op.
	PreloadImages(images []deploymentconfigapi.ImageConfig)
	// Name returns the name of the cluster provider, example 'kind'.
	Name() string
	// Get platform specific timeout values
	GetDefaultTimeoutContext() *framework.TimeoutContext
}

// NodeRecoveryCapable reports whether an infraprovider can restore a worker after
// its Node object is deleted in e2e tests (via ShutdownNode/StartNode).
type NodeRecoveryCapable interface {
	SupportsNodeRecovery() bool
}

type ExternalContainerProvider interface {
	// ListNetworks returns the names of all networks
	ListNetworks() ([]string, error)
	// GetNetwork returns a network
	GetNetwork(name string) (Network, error)
	// GetExternalContainerNetworkInterface fetches network interface information from the external container attached to a specific network
	GetExternalContainerNetworkInterface(container ExternalContainer, network Network) (NetworkInterface, error)
	ExecExternalContainerCommand(container ExternalContainer, cmd []string) (string, error)
	GetExternalContainerLogs(container ExternalContainer) (string, error)
	// GetExternalContainerPort returns a port. Requesting a port that maybe exposed in tests to avoid multiple parallel
	// tests utilizing conflicting ports. It also allows infra provider implementations to set the external containers
	// allowed port range and therefore comply with cloud provider firewall rules.
	GetExternalContainerPort() uint16
	ExternalContainerPrimaryInterfaceName() string
}

// Underlay represents the configuration for an underlay network.
// Note: The physical network referenced by PhysicalNetworkName must be pre-created and available.
type Underlay struct {
	// PhysicalNetworkName is the name of the pre-created physical network to use.
	PhysicalNetworkName string
	// LogicalNetworkName is the logical network name to be used.
	LogicalNetworkName string
	// BridgeName is the name of the bridge associated with the underlay.
	BridgeName string
	// PortName is the name of the port on the bridge.
	PortName string
	// VlanID is the VLAN identifier for the underlay network.
	VlanID int
}

type ContextCleanUp interface {
	AddCleanUpFn(func() error)
}

type Context interface {
	ContextCleanUp
	ExternalContainerContextProvider
	ClusterContextProvider
}

type ExternalContainerContextProvider interface {
	CreateNetwork(name string, subnets ...string) (Network, error)
	DeleteNetwork(network Network) error
	CreateExternalContainer(container ExternalContainer) (ExternalContainer, error)
	DeleteExternalContainer(container ExternalContainer) error
	AttachNetwork(network Network, instance string) (NetworkInterface, error)
	DetachNetwork(network Network, instance string) error
}

type ClusterContextProvider interface {
	// anything done on the cluster that could be cleaned up after test
	SetupUnderlay(f *framework.Framework, underlay Underlay) error
}

type Network interface {
	Name() string
	IPv4IPv6Subnets() (string, string, error)
	Equal(candidate Network) bool
	String() string
}

type Networks struct {
	List []Network
}

func (n *Networks) Contains(network Network) bool {
	_, found := n.Get(network.Name())
	return found
}

func (n *Networks) Get(name string) (Network, bool) {
	for _, network := range n.List {
		if network.Name() == name {
			return network, true
		}
	}
	return nil, false
}

func (n *Networks) InsertNoDupe(candidate Network) {
	var found bool
	for _, network := range n.List {
		if network.Equal(candidate) {
			found = true
			break
		}
	}
	if !found {
		n.List = append(n.List, candidate)
	}
}

type Attachment struct {
	Network  Network
	Instance string
}

func (a Attachment) equal(candidate Attachment) bool {
	if a.Instance != candidate.Instance {
		return false
	}
	if !a.Network.Equal(candidate.Network) {
		return false
	}
	return true
}

type Attachments struct {
	List []Attachment
}

func (as *Attachments) InsertNoDupe(candidate Attachment) {
	var found bool
	for _, existingNetworkAttachment := range as.List {
		if existingNetworkAttachment.equal(candidate) {
			found = true
			break
		}
	}
	if !found {
		as.List = append(as.List, candidate)
	}
}

type NetworkInterface struct {
	IPv4Gateway string
	IPv4        string
	IPv4Prefix  string
	IPv6Gateway string
	IPv6        string
	IPv6Prefix  string
	MAC         string
	InfName     string
}

func (n NetworkInterface) GetName() string {
	return n.InfName
}

func (n NetworkInterface) GetIPv4Gateway() string {
	return n.IPv4Gateway
}

func (n NetworkInterface) GetIPv4() string {
	return n.IPv4
}

func (n NetworkInterface) GetIPv4Prefix() string {
	return n.IPv4Prefix
}

func (n NetworkInterface) GetIPv6Gateway() string {
	return n.IPv6Gateway
}

func (n NetworkInterface) GetIPv6() string {
	return n.IPv6
}

func (n NetworkInterface) GetIPv6Prefix() string {
	return n.IPv6Prefix
}

func (n NetworkInterface) GetMAC() string {
	return n.MAC
}

type ExternalContainer struct {
	Name        string
	Image       string
	Network     Network
	Entrypoint  string
	CmdArgs     []string
	ExtPort     uint16
	IPv4        string
	IPv6        string
	RuntimeArgs []string
}

func (ec ExternalContainer) GetName() string {
	return ec.Name
}

func (ec ExternalContainer) GetIPv4() string {
	return ec.IPv4
}

func (ec ExternalContainer) GetIPv6() string {
	return ec.IPv6
}

func (ec ExternalContainer) GetPortStr() string {
	if ec.ExtPort == 0 {
		panic("port isn't defined")
	}
	return fmt.Sprintf("%d", ec.ExtPort)
}

func (ec ExternalContainer) GetPort() uint16 {
	if ec.ExtPort == 0 {
		panic("port isn't defined")
	}
	return ec.ExtPort
}

func (ec ExternalContainer) IsIPv4() bool {
	return ec.IPv4 != ""
}

func (ec ExternalContainer) IsIPv6() bool {
	return ec.IPv6 != ""
}

func (ec ExternalContainer) String() string {
	str := fmt.Sprintf("Name: %q, Image: %q, Network: %q, RuntimeArgs: %q, Command: %q", ec.Name, ec.Image, ec.Network, strings.Join(ec.RuntimeArgs, " "), strings.Join(ec.CmdArgs, " "))
	if ec.IsIPv4() {
		str = fmt.Sprintf("%s, IPv4 address: %q", str, ec.GetIPv4())
	}
	if ec.IsIPv6() {
		str = fmt.Sprintf("%s, IPv6 address: %s", str, ec.GetIPv6())
	}
	return str
}

func (ec ExternalContainer) IsValidPreCreateContainer() (bool, error) {
	var errs []error
	if ec.Name == "" {
		errs = append(errs, errors.New("name is not set"))
	}
	if ec.Image == "" {
		errs = append(errs, errors.New("image is not set"))
	}
	if ec.Network.String() == "" {
		errs = append(errs, errors.New("network is not set"))
	}
	if len(errs) == 0 {
		return true, nil
	}
	return false, condenseErrors(errs)
}

func (ec ExternalContainer) IsValidPostCreate() (bool, error) {
	var errs []error
	if ec.IPv4 == "" && ec.IPv6 == "" {
		errs = append(errs, errors.New("provider did not populate an IPv4 or an IPv6 address"))
	}
	if len(errs) == 0 {
		return true, nil
	}
	return false, condenseErrors(errs)
}

func (ec ExternalContainer) IsValidPreDelete() (bool, error) {
	if ec.IPv4 == "" && ec.IPv6 == "" {
		return false, fmt.Errorf("IPv4 or IPv6 must be set")
	}
	return true, nil
}

var NotFound = fmt.Errorf("not found")

func condenseErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	err := errs[0]
	for _, e := range errs[1:] {
		err = errors.Join(err, e)
	}
	return err
}

// Runner abstracts command execution for various tools (docker, podman,
// ssh, kcli, etc.)
// Implementations execute commands with the provided arguments
// and return combined stdout/stderr.
type Runner interface {
	// Run executes a command with the given arguments
	// and returns its output, or an error on failure.
	Run(command string, args ...string) (string, error)
}
