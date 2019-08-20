package controller

import (
	"encoding/json"
	"net"
	"strconv"

	"github.com/sirupsen/logrus"

	"github.com/Microsoft/hcsshim/hcn"
)

// Datastore for NetworkInfo.
type NetworkInfo struct {
	ID           string
	Name         string
	Subnets      []SubnetInfo
	Vsid         uint16
	AutomaticDNS bool
}

// Datastore for SubnetInfo.
type SubnetInfo struct {
	AddressPrefix  net.IPNet
	GatewayAddress net.IP
	Vsid           uint16
}

// GetHostComputeSubnetConfig converts SubnetInfo into an HCN format.
func (subnet *SubnetInfo) GetHostComputeSubnetConfig() (*hcn.Subnet, error) {
	// Check for nil on address objects.
	ipAddr := ""
	if subnet.AddressPrefix.IP != nil && subnet.AddressPrefix.Mask != nil {
		ipAddr = subnet.AddressPrefix.String()
	}
	gwAddr := ""
	destPrefix := ""
	if subnet.GatewayAddress != nil {
		gwAddr = subnet.GatewayAddress.String()
		destPrefix = "0.0.0.0/0"
	}

	subnetPolicy := hcn.SubnetPolicy{
		Type:     "VSID",
		Settings: []byte("{ \"IsolationId\" : " + strconv.FormatUint(uint64(subnet.Vsid), 10) + " }"),
	}

	subnetPolicyJson, err := json.Marshal(subnetPolicy)

	if err != nil {
		logrus.Error(err)
		return nil, err
	}

	return &hcn.Subnet{
		IpAddressPrefix: ipAddr,
		Routes: []hcn.Route{{
			NextHop:           gwAddr,
			DestinationPrefix: destPrefix,
		},
		},
		Policies: []json.RawMessage{
			subnetPolicyJson,
		},
	}, nil
}

// GetHostComputeNetworkConfig converts NetworkInfo to HCN format.
func (info *NetworkInfo) GetHostComputeNetworkConfig() (*hcn.HostComputeNetwork, error) {
	subnets := []hcn.Subnet{}
	for _, subnet := range info.Subnets {
		subnetConfig, err := subnet.GetHostComputeSubnetConfig()
		if err != nil {
			logrus.Error(err)
			return nil, err
		}

		subnets = append(subnets, *subnetConfig)
	}

	hcnPolicies := []hcn.NetworkPolicy{{
		Type:     hcn.AutomaticDNS,
		Settings: []byte("{ \"Enable\" : " + strconv.FormatBool(info.AutomaticDNS) + " }"),
	},
	}

	ipams := []hcn.Ipam{}
	if len(subnets) > 0 {
		ipams = []hcn.Ipam{{
			Type:    "Static",
			Subnets: subnets,
		},
		}
	}

	return &hcn.HostComputeNetwork{
		Name:  info.Name,
		Type:  hcn.NetworkType("Overlay"),
		Ipams: ipams,
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
		Policies: hcnPolicies,
	}, nil
}

// CreateNetworkPolicySetting builds a NetAdapterNameNetworkPolicySetting.
func CreateNetworkPolicySetting(networkAdapterName string) (*hcn.NetworkPolicy, error) {
	netAdapterPolicy := hcn.NetAdapterNameNetworkPolicySetting{
		NetworkAdapterName: networkAdapterName,
	}
	policyJSON, err := json.Marshal(netAdapterPolicy)
	if err != nil {
		logrus.Error(err)
		return nil, err
	}

	return &hcn.NetworkPolicy{
		Type:     hcn.NetAdapterName,
		Settings: policyJSON,
	}, nil
}

// AddRemoteSubnetPolicy adds a remote subnet policy
func AddRemoteSubnetPolicy(network *hcn.HostComputeNetwork, settings *hcn.RemoteSubnetRoutePolicySetting) error {
	rawJSON, err := json.Marshal(settings)

	if err != nil {
		logrus.Errorf("Failed to marshall settings, error: %v", err)
		return err
	}

	networkPolicy := hcn.NetworkPolicy{
		Type:     hcn.RemoteSubnetRoute,
		Settings: rawJSON,
	}

	policyNetworkRequest := hcn.PolicyNetworkRequest{
		Policies: []hcn.NetworkPolicy{networkPolicy},
	}

	network.AddPolicy(policyNetworkRequest)

	return nil
}

func removeOneRemoteSubnetPolicy(network *hcn.HostComputeNetwork, policySettings hcn.RemoteSubnetRoutePolicySetting) error {
	existingPolicyJson, err := json.Marshal(policySettings)

	if err != nil {
		logrus.Errorf("Failed to marshal settings, error: %v", err)
		return err
	}

	existingPolicy := hcn.NetworkPolicy{
		Type:     hcn.RemoteSubnetRoute,
		Settings: existingPolicyJson,
	}

	existingPolicyNetworkRequest := hcn.PolicyNetworkRequest{
		Policies: []hcn.NetworkPolicy{existingPolicy},
	}

	network.RemovePolicy(existingPolicyNetworkRequest)

	return nil
}

// RemoveRemoteSubnetPolicy removes a remote subnet policy
func RemoveRemoteSubnetPolicy(network *hcn.HostComputeNetwork, destinationPrefix string) error {

	for _, policy := range network.Policies {
		if policy.Type == hcn.RemoteSubnetRoute {
			existingPolicySettings := hcn.RemoteSubnetRoutePolicySetting{}

			err := json.Unmarshal(policy.Settings, &existingPolicySettings)

			if err != nil {
				logrus.Errorf("Failed to unmarshal settings, error: %v", err)
				return err
			}

			if existingPolicySettings.DestinationPrefix == destinationPrefix {

				err := removeOneRemoteSubnetPolicy(network, existingPolicySettings)

				if err != nil {
					logrus.Errorf("Failed to remove remote subnet policy %v, error: %v", existingPolicySettings.DestinationPrefix, err)
					return err
				}
			}
		}
	}

	return nil
}

func ClearRemoteSubnetPolicies(network *hcn.HostComputeNetwork) error {

	for _, policy := range network.Policies {
		if policy.Type == hcn.RemoteSubnetRoute {
			existingPolicySettings := hcn.RemoteSubnetRoutePolicySetting{}

			err := json.Unmarshal(policy.Settings, &existingPolicySettings)

			if err != nil {
				logrus.Errorf("Failed to unmarshal settings, error: %v", err)
				return err
			}

			err = removeOneRemoteSubnetPolicy(network, existingPolicySettings)

			if err != nil {
				logrus.Errorf("Failed to remove remote subnet policy %v, error: %v", existingPolicySettings.DestinationPrefix, err)
				// We don't return the error in this case, we take a best effort approach to clear the remote subnets.
			}
		}
	}

	return nil
}

func GetGatewayAddress(subnet *hcn.Subnet) string {
	for _, route := range subnet.Routes {
		if route.DestinationPrefix == "0.0.0.0/0" || route.DestinationPrefix == "::/0" {
			return route.NextHop
		}
	}

	return ""
}

func GetExistingNetwork(networkName string, expectedAddressPrefix string, expectedGW string) *hcn.HostComputeNetwork {
	existingNetwork, err := hcn.GetNetworkByName(networkName)
	if err == nil {
		if existingNetwork.Type == hcn.Overlay {
			for _, existingIpams := range existingNetwork.Ipams {
				for _, existingSubnet := range existingIpams.Subnets {
					gatewayAddress := GetGatewayAddress(&existingSubnet)
					if existingSubnet.IpAddressPrefix == expectedAddressPrefix && gatewayAddress == expectedGW {
						return existingNetwork
					}
				}
			}
		}
	}

	return nil
}
