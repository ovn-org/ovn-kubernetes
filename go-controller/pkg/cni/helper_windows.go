// +build windows

package cni

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/Microsoft/hcsshim"
	"github.com/containernetworking/cni/pkg/types/current"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// getHNSIdFromConfigOrByGatewayIP returns the HNS Id using the Gateway IP or
// the config value
// When the HNS Endpoint is created, it asks for the HNS Id in order to
// attach it to the desired network. This function finds the HNS Id of the
// network based on the gatewayIP. If more than one suitable network it's found,
// return an error asking to give the HNS Network Id in config.
func getHNSIdFromConfigOrByGatewayIP(gatewayIP net.IP) (string, error) {
	if config.CNI.WinHNSNetworkID != "" {
		logrus.Infof("Using HNS Network Id from config: %v", config.CNI.WinHNSNetworkID)
		return config.CNI.WinHNSNetworkID, nil
	}
	if gatewayIP == nil {
		return "", fmt.Errorf("no gateway IP and no HNS Network ID given")
	}
	hnsNetworkId := ""
	hnsNetworks, err := hcsshim.HNSListNetworkRequest("GET", "", "")
	if err != nil {
		return "", err
	}
	for _, hnsNW := range hnsNetworks {
		for _, hnsNWSubnet := range hnsNW.Subnets {
			if strings.Compare(gatewayIP.String(), hnsNWSubnet.GatewayAddress) == 0 {
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
		// Return the error in case delete failed, we don't want to leak any HNS Endpoints
		return err
	}
	// If endpoint was not found just log a message and return no error
	logrus.Infof("No endpoint with name %v was found, error %v", endpointName, err)
	return nil
}

// ConfigureInterface sets up the container interface
// Small note on this, the call to this function should be idempotent on Windows.
// The fact that CNI add should be idempotent on Windows is stated here:
// https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/network/cni/cni_windows.go#L38
// TODO: add proper MTU config (GetCurrentThreadId/SetCurrentThreadId) or via OVS properties
func (pr *PodRequest) ConfigureInterface(namespace string, podName string, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	conf := pr.CNIConf

	if conf.DeviceID != "" {
		return nil, fmt.Errorf("failure OVS-Offload is not supported in Windows")
	}
	ipMaskSize, _ := ifInfo.IP.Mask.Size()
	// NOTE(abalutoiu): The endpoint name should not depend on the container ID.
	// This is for backwards compatibility in kubernetes which calls the CNI
	// even for containers that are not the infra container.
	// This is getting fixed by https://github.com/kubernetes/kubernetes/pull/64189
	endpointName := fmt.Sprintf("%s_%s", namespace, podName)

	var err error
	defer func() {
		// Delete the endpoint in case CNI fails
		if err != nil {
			errHNSDelete := deleteHNSEndpoint(endpointName)
			if errHNSDelete != nil {
				logrus.Warningf("Failed to delete the HNS Endpoint, reason: %q", errHNSDelete)
			}
		}
	}()

	var hnsNetworkId string
	hnsNetworkId, err = getHNSIdFromConfigOrByGatewayIP(ifInfo.GW)
	if err != nil {
		logrus.Infof("Error when detecting the HNS Network Id: %q", err)
		return nil, err
	}

	// Check if endpoint is created, otherwise create it.
	// This is to make the call to add idempotent
	var createdEndpoint *hcsshim.HNSEndpoint
	createdEndpoint, err = hcsshim.GetHNSEndpointByName(endpointName)
	if err != nil {
		logrus.Infof("HNS endpoint %q does not exist", endpointName)

		// HNSEndpoint requires the xx-xx-xx-xx-xx-xx format for the MacAddress field
		macAddressIpFormat := strings.Replace(ifInfo.MAC.String(), ":", "-", -1)

		hnsEndpoint := &hcsshim.HNSEndpoint{
			Name:           endpointName,
			VirtualNetwork: hnsNetworkId,
			IPAddress:      ifInfo.IP.IP,
			MacAddress:     macAddressIpFormat,
			PrefixLength:   uint8(ipMaskSize),
			DNSServerList:  strings.Join(conf.DNS.Nameservers, ","),
			DNSSuffix:      strings.Join(conf.DNS.Search, ","),
		}
		createdEndpoint, err = createHNSEndpoint(hnsEndpoint)
		if err != nil {
			return nil, err
		}
	} else {
		logrus.Infof("HNS endpoint already exists with name: %q", endpointName)
	}

	err = containerHotAttachEndpoint(createdEndpoint, pr.SandboxID)
	if err != nil {
		logrus.Warningf("Failed to hot attach HNS Endpoint %q to container %q, reason: %q", endpointName, pr.SandboxID, err)
		return nil, err
	}

	ifaceID := fmt.Sprintf("%s_%s", namespace, podName)

	// To skip calling powershell for MTU and OVS to add the internal port, checking here
	// if the interface exists and then return. This improves the performance of the CNI.
	// TODO: Remove this once kubelet on Windows does not call the CNI for every container
	// within a pod. Issue: https://github.com/kubernetes/kubernetes/issues/64188
	ifaceName, errFind := ovsFind("interface", "name", "external-ids:iface-id="+ifaceID)
	if errFind == nil && len(ifaceName) > 0 && ifaceName[0] != "" {
		logrus.Infof("HNS endpoint %q already set up for container %q", endpointName, pr.SandboxID)
		return []*current.Interface{}, nil
	}

	// TODO: Revisit this once Hyper-V Containers are supported in Kubernetes
	// "--may-exist"  is added to support the function idempotency
	ovsArgs := []string{
		"--may-exist", "add-port", "br-int", endpointName, "--", "set",
		"interface", endpointName, "type=internal", "--", "set",
		"interface", endpointName,
		fmt.Sprintf("external_ids:attached_mac=%s", ifInfo.MAC),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:ip_address=%s", ifInfo.IP),
	}
	var out []byte
	out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failure in plugging pod interface: %v  %q", err, string(out))
	}

	mtuArgs := []string{
		"Set-NetIPInterface",
		"-IncludeAllCompartments",
		fmt.Sprintf("-InterfaceAlias \"vEthernet (%s)\"", ifaceID),
		fmt.Sprintf("-NlMtuBytes %d", ifInfo.MTU),
	}

	out, err = exec.Command("powershell", mtuArgs...).CombinedOutput()
	if err != nil {
		logrus.Warningf("Failed to set MTU on endpoint %q, with: %q", endpointName, string(out))
		return nil, fmt.Errorf("failed to set MTU on endpoint, reason: %q", err)
	}

	// TODO: uncomment when OVS QoS is supported on Windows
	//if err = clearPodBandwidth(args.ContainerID); err != nil {
	//	return nil, err
	//}
	//if err = setPodBandwidth(args.ContainerID, endpointName, ingress, egress); err != nil {
	//	return nil, err
	//}

	return []*current.Interface{}, nil
}

// PlatformSpecificCleanup deletes the OVS port and also the corresponding
// HNS Endpoint for the OVS port.
func (pr *PodRequest) PlatformSpecificCleanup() error {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		logrus.Warningf("cleanup failed, required CNI variable missing from args: %v", pr)
		return nil
	}

	endpointName := fmt.Sprintf("%s_%s", namespace, podName)
	ovsArgs := []string{
		"del-port", "br-int", endpointName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		logrus.Warningf("failed to delete OVS port %s: %v  %q", endpointName, err, string(out))
	}
	if err = deleteHNSEndpoint(endpointName); err != nil {
		logrus.Warningf("failed to delete HNSEndpoint %v: %v", endpointName, err)
	}
	// TODO: uncomment when OVS QoS is supported on Windows
	// _ = clearPodBandwidth(args.ContainerID)
	return nil
}
