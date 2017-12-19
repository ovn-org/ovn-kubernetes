package cluster

import (
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"

	kapi "k8s.io/api/core/v1"
)

func (cluster *namespaceSubnetController) StartMaster(masterNodeName string) error {
	subrange := make([]string, 0)

	existingNamespaces, err := cluster.Kube.GetNamespaces()
	if err != nil {
		return fmt.Errorf("error fetching namespaces: %v", err)
	}
	for _, ns := range existingNamespaces.Items {
		if nssubnet, ok := ns.Annotations[OvnNamespaceSubnet]; ok {
			subrange = append(subrange, nssubnet)
		}
	}

	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'podSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	cluster.podSubnetAllocator, err = netutils.NewSubnetAllocator(cluster.ClusterIPNet.String(), cluster.PodSubnetLength, subrange)
	if err != nil {
		return err
	}

	_, err = newNamespaceWatcher(cluster.Kube, "", cluster.namespaceAdded, nil, cluster.namespaceRemoved, cluster.watchFactory)
	if err != nil {
		return err
	}

	return nil
}

func (cluster *namespaceSubnetController) setupNamespaceLogicalNetwork(ns *kapi.Namespace, nsSubnet string) error {
	// Create a router port and provide it the first address in the 'local_subnet'.
	ip, localSubnetCIDR, err := net.ParseCIDR(nsSubnet)
	if err != nil {
		return fmt.Errorf("Failed to parse local subnet %q: %v", nsSubnet, err)
	}
	localSubnetCIDR.IP = util.NextIP(ip)

	macaddr := gatewayMACForNamespaceSubnet(localSubnetCIDR)

	// Create a logical switch and set its subnet.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "ls-add", ns.Name, "--",
		"set", "logical_switch", ns.Name,
		"other-config:subnet="+nsSubnet,
		"external-ids:gateway_ip="+localSubnetCIDR.String())
	if err != nil {
		logrus.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", ns.Name, stdout, stderr, err)
		return err
	}

	gwPort := "k8s-gw-" + ns.Name
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", ns.Name, gwPort, "--",
		"set", "logical_switch_port", gwPort,
		"type=localport",
		"addresses=\""+macaddr+"\"")
	if err != nil {
		logrus.Errorf("Failed to add gateway port %q to logical switch %q, stdout: %q, stderr: %q, error: %v", gwPort, ns.Name, stdout, stderr, err)
		return err
	}

	return nil
}

func (cluster *namespaceSubnetController) namespaceRemoved(ns *kapi.Namespace) error {
	sub, ok := ns.Annotations[OvnNamespaceSubnet]
	if !ok {
		return fmt.Errorf("Error in obtaining namespace subnet for namespace %q for deletion", ns.Name)
	}

	// FIXME: deallocate subnets after a few minutes, not immediately, to ensure changes
	// get propagated through the system
	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return fmt.Errorf("Error in parsing namepsace subnet %q: %v", sub, err)
	}
	err = cluster.podSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for namespace %q: %v", sub, ns.Name, err)
	}

	logrus.Infof("Deleted NamespaceSubnet %s/%s", ns.Name, sub)
	return nil
}

func (cluster *namespaceSubnetController) namespaceAdded(ns *kapi.Namespace) error {
	if _, ok := ns.Annotations[OvnNamespaceSubnet]; ok {
		// Nothing to do...
		return nil
	}

	// Allocate a new subnet for the namespace
	sn, err := cluster.podSubnetAllocator.GetNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating network for namespace %q: %v", ns.Name, err)
	}

	// Release allocated network and clean up if something failed
	var success bool
	defer func() {
		if !success {
			_ = cluster.podSubnetAllocator.ReleaseNetwork(sn)
			// clean up OVN stuff
		}
	}()

	err = cluster.Kube.SetAnnotationOnNamespace(ns, OvnNamespaceSubnet, sn.String())
	if err != nil {
		return fmt.Errorf("error creating subnet %s for namespace %q: %v", sn.String(), ns.Name, err)
	}
	logrus.Infof("Created NamespaceSubnet %s/%s", ns.Name, sn.String())

	err = cluster.setupNamespaceLogicalNetwork(ns, sn.String())
	if err != nil {
		return fmt.Errorf("error setting up namespace %q logical network: %v", ns.Name, err)
	}

	success = true
	return nil
}
