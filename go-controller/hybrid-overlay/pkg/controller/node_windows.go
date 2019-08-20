package controller

import (
	"bytes"
	"encoding/json"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Microsoft/hcsshim/hcn"
)

const (
	// Hard-coded constants
	useAutomaticDns = true               // If true, the Windows host DNS is used to resolve container DNS requests
	networkName     = "OpenShiftNetwork" // In practice, this is the virtual switch name
)

// NodeController is the node hybrid overlay controller
type NodeController struct {
	kube            *kube.Kube
	thisNode        *kapi.Node
	networkID       string
	localNodeCIDR   net.IPNet
	localNodeIP     net.IP
	remoteSubnetMap map[string]string // Maps a remote node to its remote subnet
}

// NewNode returns a node handler that listens for node events
// so that Add/Update/Delete events are appropriately handled.
func NewNode(clientset kubernetes.Interface, nodeName string) (*NodeController, error) {
	n := &NodeController{
		kube: &kube.Kube{KClient: clientset},
	}

	thisNode, err := n.kube.GetNode(nodeName)
	if err != nil {
		return nil, err
	}
	n.thisNode = thisNode
	n.remoteSubnetMap = make(map[string]string)

	n.Add(thisNode)

	return n, nil
}

// Start is the top level function to run hybrid-sdn in node mode
func (n *NodeController) Start(wf *factory.WatchFactory) error {
	return houtil.StartNodeWatch(n, wf)
}

// Add function learns about a new node being added to the cluster
// For a windows node, this means watching for all nodes and programming the routing
func (n *NodeController) Add(node *kapi.Node) {

	_, ok := node.Annotations[types.HybridOverlayHostSubnet]
	if !ok {
		logrus.Debugf("Cannot add node '%s' as the k8s.v1.ovn.org/hybrid-overlay-hostsubnet annotation is missing on that node!", node.Name)
		return
	}

	// Treat the local node differently than other nodes
	// If the local node is added, we want to create the network object and do all the VXLAN plumbing towards other existing nodes
	// If a remote node is added, we just want to plumb the VXLAN tunnel (i.e. the remote subnet) to it.
	if node.Status.NodeInfo.MachineID != n.thisNode.Status.NodeInfo.MachineID {
		cidr, nodeIP, drMAC := getNodeDetails(node, true)

		// Make sure the node is valid before adding it
		if cidr != nil && nodeIP != nil && drMAC != nil {
			network, err := hcn.GetNetworkByID(n.networkID)
			if err != nil {
				logrus.Error(err)
				return
			}

			logrus.Infof("Adding a remote subnet route for CIDR '%s' (remote node address: %s, distributed router MAC: %s, VNI: %v).", cidr.String(), nodeIP.String(), drMAC.String(), types.HybridOverlayVNI)
			networkPolicySettings := hcn.RemoteSubnetRoutePolicySetting{
				IsolationId:                 types.HybridOverlayVNI, // VXLAN virtual network Identifier. Is expected to be 4097 or higher on Windows
				DistributedRouterMacAddress: drMAC.String(),         // Distributed router/gateway MAC address
				ProviderAddress:             nodeIP.String(),        // Host IP address of the node
				DestinationPrefix:           cidr.String(),          // Prefix used on the destination node
			}

			n.remoteSubnetMap[node.Status.NodeInfo.MachineID] = cidr.String()

			err = AddRemoteSubnetPolicy(network, &networkPolicySettings)
			if err != nil {
				logrus.Error(err)
				return
			}
		} else {
			n.Delete(node)
		}
	} else {
		cidr, nodeIP, _ := getNodeDetails(node, false)

		// Make sure the node is valid and wasn't already initialized before adding it
		if cidr != nil && nodeIP != nil && (!cidr.IP.Equal(n.localNodeCIDR.IP) || !bytes.Equal(cidr.Mask, n.localNodeCIDR.Mask) || !nodeIP.Equal(n.localNodeIP)) {
			n.localNodeCIDR = *cidr
			n.localNodeIP = nodeIP

			// initialize self
			n.InitSelf()
		}
	}
}

// Update handles node updates
func (n *NodeController) Update(oldNode, newNode *kapi.Node) {

	// Only modify the node if relevant annotations have changed
	if nodeChanged(oldNode, newNode) {
		n.Delete(oldNode)
		n.Add(newNode)
	}
}

// Delete handles node deletions
func (n *NodeController) Delete(node *kapi.Node) {
	// Treat the local node differently than other nodes
	// If the local node is removed, we want to delete the network object and remove all the VXLAN plumbing towards other existing nodes
	// If a remote node is removed, we just want to remove the VXLAN tunnel (i.e. the remote subnet) to it.

	if node.Status.NodeInfo.MachineID == n.thisNode.Status.NodeInfo.MachineID {
		n.UninitSelf()
		return
	}

	if n.networkID == "" {
		// Just silently return, no need to clean up a non-initialized network
		return
	}

	network, err := hcn.GetNetworkByID(n.networkID)
	if err != nil {
		if _, isNotExist := err.(hcn.NetworkNotFoundError); !isNotExist {
			logrus.Errorf("Couldn't retreive network with ID '%s' on node '%s'", n.networkID, node.Name)
		} else {
			logrus.Infof("No need to cleanup remote subnet for network with ID '%s' towards node '%s' as it doesn't exist", n.networkID, node.Name)
		}
		return
	}

	nodeSubnet, ok := n.remoteSubnetMap[node.Status.NodeInfo.MachineID]
	if !ok {
		logrus.Errorf("Can't retreive the host subnet from the '%s' node's annotations", node.Name)
		return
	}

	err = RemoveRemoteSubnetPolicy(network, nodeSubnet)
	if err != nil {
		logrus.Errorf("Error removing subnet policy '%s' node's annotations from network '%s' on node '%s'. Error: %v", nodeSubnet, n.networkID, node.Name, err)
		return
	}

	delete(n.remoteSubnetMap, node.Status.NodeInfo.MachineID)
}

// Sync handles synchronizing the initial node list
func (n *NodeController) Sync(nodes []*kapi.Node) {
}

// InitSelf initializes the node it is currently running on.
// On Windows, this means:
//  1. Setting up this node and its VxLAN extension for talking to other nodes
//  2. Setting back annotations about its VTEP and gateway MAC address to its own node object
//  3. Initializing every VXLAN tunnels toward other
func (n *NodeController) InitSelf() {
	// Create network
	var networkInfo NetworkInfo

	// Retrieve the host prefix from the annotations
	hostsubnet, ok := n.thisNode.Annotations[types.HybridOverlayHostSubnet]

	if !ok {
		logrus.Errorf("Couldn't retreive the host subnet from the '%s' node's annotations", n.thisNode.Name)
		return
	}

	_, addressPrefix, err := net.ParseCIDR(hostsubnet)
	if err != nil {
		logrus.Error(err)
		return
	}

	// The distributed router IP (i.e. the gateway, from a container perspective) is hardcoded here to be the first IP on the subnet
	// In the future, this could be made configurable if necessary as Windows doesn't have any restrictions as to what this gateway address should be.
	gatewayAddress := util.NextIP(addressPrefix.IP)

	network := GetExistingNetwork(networkName, addressPrefix.String(), gatewayAddress.String())

	if network == nil {
		networkInfo.AutomaticDNS = useAutomaticDns
		networkInfo.Name = networkName

		networkInfo.Subnets = []SubnetInfo{
			{
				AddressPrefix:  *addressPrefix,
				GatewayAddress: gatewayAddress,
				Vsid:           types.HybridOverlayVNI,
			},
		}

		logrus.Infof("Creating overlay network '%s' (address prefix %v) with gateway address: %v. Use automatic DNS: %t.", networkName, addressPrefix, gatewayAddress, useAutomaticDns)

		// Retreive the network schema object
		var networkSchema *hcn.HostComputeNetwork
		networkSchema, err = networkInfo.GetHostComputeNetworkConfig()
		if err != nil {
			logrus.Errorf("Unable to generate a schema to create an overlay network, error: %v", err)
			return
		}

		logrus.Infof("Network creation may take up to a minute to complete...")
		// Create the actual network from schema object
		network, err = networkSchema.Create()
		if err != nil {
			logrus.Errorf("Unable to create the overlay network, error: %v", err)
			return
		}
	} else {
		logrus.Infof("Reusing existing overlay network '%s' (address prefix %v) with gateway address: %v.", networkName, addressPrefix, gatewayAddress)

		// TODO: there is a better approach than clearing all the remote subnet policies, and then re-creating the ones still applicable.
		// we should instead take an update approach by removing the stale policies and create the missing ones
		ClearRemoteSubnetPolicies(network)
		if err != nil {
			logrus.Errorf("Failed to clear the existing remote subnet policies. Some stale policies were left behind.: %v", err)
			// Don't return here. We can still work with stale policies. We will re-create the policies later in this function.
		}
	}

	n.networkID = network.Id

	// Set the HybridOverlayDrMac annotation on the node
	for _, policy := range network.Policies {
		if policy.Type == hcn.DrMacAddress {
			policySettings := hcn.DrMacAddressNetworkPolicySetting{}

			err = json.Unmarshal(policy.Settings, &policySettings)

			if err != nil {
				logrus.Errorf("Unable to unmarshall the DRMAC policy setting, error: %v", err)
				return
			}

			if len(policySettings.Address) == 0 {
				logrus.Errorf("Error creating the network: no DRMAC address")
				return
			}
			n.kube.SetAnnotationOnNode(n.thisNode, types.HybridOverlayDrMac, policySettings.Address)

			break
		}
	}

	// Add existing nodes
	nodes, err := n.kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching nodes: %v", err)
		return
	}

	for _, node := range nodes.Items {

		// Add VXLAN tunnel to the remote nodes
		if node.Status.NodeInfo.MachineID != n.thisNode.Status.NodeInfo.MachineID {
			n.Add(&node)
		}
	}
}

// UninitSelf un-initializes the node it is currently running on.
// On Windows, this means:
//  1. Cleaning up this node and its VxLAN extension for talking to other nodes
//  2. Cleaning up annotations about its VTEP and gateway MAC address to its own node object
//  3. Uninitializing every VXLAN tunnels toward other
func (n *NodeController) UninitSelf() {
	logrus.Infof("Removing overlay network '%s' (ID: %v) from local node '%s'", networkName, n.networkID, n.thisNode.Name)

	// Remove existing nodes
	nodes, err := n.kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in uninitializing/fetching nodes: %v", err)
		return
	}

	for _, node := range nodes.Items {

		// Add VXLAN tunnel to the remote nodes
		if node.Status.NodeInfo.MachineID != n.thisNode.Status.NodeInfo.MachineID {
			n.Delete(&node)
		}
	}

	// remove the node annotations
	n.kube.DeleteAnnotationOnNode(n.thisNode, types.HybridOverlayDrMac)

	// Find the network and remove it
	network, err := hcn.GetNetworkByID(n.networkID)

	if err != nil {
		logrus.Error(err)
		return
	}

	network.Delete()
}
