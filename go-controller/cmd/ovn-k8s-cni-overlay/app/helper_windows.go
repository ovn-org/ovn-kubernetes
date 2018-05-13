// +build windows

package app

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/Microsoft/hcsshim"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
)

// noNameNetNS - This is received for the infra container, in this case we
// have to create the network endpoint and attach it to the infra container
// containerNetNS - This is received for all the other containers from
// the same pod, do not create the network endpoint in this case
const (
	noNameNetNS    = "none"
	containerNetNS = "container:"
)

// More details about the above constants can be found in the following PR:
// https://github.com/kubernetes/kubernetes/pull/51063

// getHNSIdFromConfigOrByGatewayIP returns the HNS Id using the Gateway IP or
// the config value
// When the HNS Endpoint is created, it asks for the HNS Id in order to
// attach it to the desired network. This function finds the HNS Id of the
// network based on the gatewayIP. If more than one suitable network it's found,
// return an error asking to give the HNS Network Id in config.
func getHNSIdFromConfigOrByGatewayIP(gatewayIP string) (string, error) {
	if config.CNI.WinHNSNetworkID != "" {
		logrus.Infof("Using HNS Network Id from config: %v", config.CNI.WinHNSNetworkID)
		return config.CNI.WinHNSNetworkID, nil
	}
	hnsNetworkId := ""
	hnsNetworks, err := hcsshim.HNSListNetworkRequest("GET", "", "")
	if err != nil {
		return "", err
	}
	for _, hnsNW := range hnsNetworks {
		for _, hnsNWSubnet := range hnsNW.Subnets {
			if strings.Compare(gatewayIP, hnsNWSubnet.GatewayAddress) == 0 {
				if len(hnsNetworkId) == 0 {
					hnsNetworkId = hnsNW.Id
				} else {
					return "", fmt.Errorf("Found more than one network suitable for containers, " +
						"please specify win-hnsnetwork-id in config")
				}
			}
		}
	}
	if len(hnsNetworkId) != 0 {
		logrus.Infof("HNS Network Id found: %v", hnsNetworkId)
		return hnsNetworkId, nil
	}
	return "", fmt.Errorf("Could not find any suitable network to attach the container")
}

// createHNSEndpoint creates the HNS endpoint with the given configuration.
// On success it returns the created HNS endpoint.
func createHNSEndpoint(hnsConfiguration *hcsshim.HNSEndpoint) (*hcsshim.HNSEndpoint, error) {
	logrus.Infof("Creating HNS endpoint")
	hnsConfigBytes, err := json.Marshal(hnsConfiguration)
	if err != nil {
		return nil, err
	}
	logrus.Infof("hnsConfigBytes: %v", string(hnsConfigBytes))

	createdHNSEndpoint, err := hcsshim.HNSEndpointRequest("POST", "", string(hnsConfigBytes))
	if err != nil {
		logrus.Errorf("Could not create the HNSEndpoint, error: %v", err)
		return nil, err
	}
	logrus.Infof("Created HNS endpoint with ID: %v", createdHNSEndpoint.Id)
	return createdHNSEndpoint, nil
}

// containerHotAttachEndpoint attaches the given endpoint to a running container
func containerHotAttachEndpoint(existingHNSEndpoint *hcsshim.HNSEndpoint, containerID string) error {
	logrus.Infof("Attaching endpoint %v to container %v", existingHNSEndpoint.Id, containerID)
	if err := hcsshim.HotAttachEndpoint(containerID, existingHNSEndpoint.Id); err != nil {
		logrus.Infof("Error attaching the endpoint to the container, error: %v", err)
		return err
	}
	logrus.Infof("Endpoint attached successfully to the container")
	return nil
}

// deleteHNSEndpoint deletes the given endpoint if it exists
func deleteHNSEndpoint(endpointName string) error {
	logrus.Infof("Deleting HNS endpoint: %v", endpointName)
	// The HNS endpoint must be manually deleted
	hnsEndpoint, err := hcsshim.GetHNSEndpointByName(endpointName)
	if err == nil {
		logrus.Infof("Fetched endpoint: %v", endpointName)
		// Endpoint exists, try to delete it
		_, err = hnsEndpoint.Delete()
		if err != nil {
			logrus.Warningf("Failed to delete HNS endpoint: %q", err)
		} else {
			logrus.Infof("HNS endpoint successfully deleted: %q", endpointName)
		}
	} else {
		logrus.Infof("No endpoint with name %v was found, error %v", endpointName, err)
	}
	// Return the error in case it failed, we don't want to leak any HNS Endpoints
	return err
}

// ConfigureInterface sets up the container interface
// Small note on this, the call to this function should be idempotent on Windows.
// The fact that CNI add should be idempotent on Windows is stated here:
// https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/network/cni/cni_windows.go#L38
// TODO: add support for custom MTU
func ConfigureInterface(args *skel.CmdArgs, namespace string, podName string, macAddress string, ipAddress string, gatewayIP string, mtu int) ([]*current.Interface, error) {
	if strings.HasPrefix(args.Netns, containerNetNS) || strings.Compare(args.Netns, noNameNetNS) != 0 {
		// If it is a normal container from the pod, there is nothing to do.
		// Also, if it is not an infra container, nothing to do in this case as well.
		logrus.Infof("CNI called for normal container or infra container, nothing to do")
		return []*current.Interface{}, nil
	}

	ipAddr, ipNet, err := net.ParseCIDR(ipAddress)
	if err != nil {
		return nil, err
	}
	ipMaskSize, _ := ipNet.Mask.Size()
	endpointName := args.ContainerID

	var hnsNetworkId string
	hnsNetworkId, err = getHNSIdFromConfigOrByGatewayIP(gatewayIP)
	if err != nil {
		logrus.Infof("Error when detecting the HNS Network Id: %q", err)
		return nil, err
	}

	// Ensure that the macAddress is given in xx:xx:xx:xx:xx:xx format
	macAddressIpFormat := strings.Replace(macAddress, ":", "-", -1)

	// Check if endpoint is created, otherwise create it.
	// This is to make the call to add idempotent
	var createdEndpoint *hcsshim.HNSEndpoint
	createdEndpoint, err = hcsshim.GetHNSEndpointByName(endpointName)
	if err != nil {
		logrus.Infof("HNS endpoint %q does not exist", endpointName)
		hnsEndpoint := &hcsshim.HNSEndpoint{
			Name:           endpointName,
			VirtualNetwork: hnsNetworkId,
			IPAddress:      ipAddr,
			MacAddress:     macAddressIpFormat,
			PrefixLength:   uint8(ipMaskSize),
		}
		createdEndpoint, err = createHNSEndpoint(hnsEndpoint)
		if err != nil {
			return nil, err
		}
	} else {
		logrus.Infof("HNS endpoint already exists with name: %q", endpointName)
	}

	err = containerHotAttachEndpoint(createdEndpoint, args.ContainerID)
	if err != nil {
		logrus.Warningf("Failed to hot attach HNS Endpoint %q to container %q, reason: %q", endpointName, args.ContainerID, err)
		// In case the attach failed, delete the endpoint
		errHNSDelete := deleteHNSEndpoint(args.ContainerID)
		if errHNSDelete != nil {
			logrus.Warningf("Failed to delete the HNS Endpoint, reason: %q", errHNSDelete)
		}
		return nil, err
	}

	ifaceID := fmt.Sprintf("%s_%s", namespace, podName)
	// TODO: Revisit this once Hyper-V Containers are supported in Kubernetes
	// "--may-exist"  is added to support the function idempotency
	ovsArgs := []string{
		"--may-exist", "add-port", "br-int", endpointName, "--", "set",
		"interface", endpointName, "type=internal", "--", "set",
		"interface", endpointName,
		fmt.Sprintf("external_ids:attached_mac=%s", macAddress),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:ip_address=%s", ipAddress),
	}
	var out []byte
	out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failure in plugging pod interface: %v  %q", err, string(out))
	}

	return []*current.Interface{}, nil
}

// PlatformSpecificCleanup deletes the OVS port and also the corresponding
// HNS Endpoint for the OVS port.
func PlatformSpecificCleanup(args *skel.CmdArgs) error {
	endpointName := args.ContainerID
	ovsArgs := []string{
		"del-port", "br-int", endpointName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		logrus.Warningf("failed to delete OVS port %s: %v  %q", endpointName, err, string(out))
	}
	// Return the error if we can't delete the HNS Endpoint, we don't want any leaks
	return deleteHNSEndpoint(endpointName)
}

// InitialPlatformCheck checks to see if the container is an infra container
// by looking at the namespace prefix. If it's not, then it has nothing to
// do and the CNI should stop here.
func InitialPlatformCheck(args *skel.CmdArgs) (bool, *current.Result) {
	if strings.HasPrefix(args.Netns, containerNetNS) || strings.Compare(args.Netns, noNameNetNS) != 0 {
		// If it is a normal container from the pod, there is nothing to do.
		// Also, if it is not an infra container, nothing to do in this case as well.
		logrus.Infof("InitialPlatformCheck: CNI called for normal container or infra container, nothing to do")
		// This result is ignored anyway by Kubernetes.
		fakeAddr, fakeAddrNet, _ := net.ParseCIDR("1.2.3.4/32")
		fakeResult := &current.Result{
			IPs: []*current.IPConfig{
				{
					Version:   "4",
					Interface: current.Int(1),
					Address:   net.IPNet{IP: fakeAddr, Mask: fakeAddrNet.Mask},
					Gateway:   net.ParseIP("1.2.3.4"),
				},
			},
		}
		return true, fakeResult
	}
	return false, &current.Result{}
}
