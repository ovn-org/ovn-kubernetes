package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	"fmt"
	"net"
	"strings"
	"time"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// UpdateRouterSNAT checks if a NAT entry exists in OVN for a specific router and updates it if necessary
// may-exist works only if the the nat rule being added has everything the same i.e.,
// the type, the router name, external IP and the logical IP must match
// else the tuple is considered different one than existing.
// If the type is snat and the logical IP is the same, but external IP is different,
// even with --may-exist, the add may error out. this is because, for snat,
// (type, router, logical ip) is considered a key for uniqueness
// Combining lr-nat-del and lr-nat-add will fail OVSDB transaction check because
// the entry will already exist so need to do these as two separate transactions
// Therefore we need to iterate through existing SNAT rules on GR and decide if we need to delete/add
// TODO(trozet) use go-bindings
func UpdateRouterSNAT(router string, externalIP net.IP, logicalSubnet *net.IPNet) error {
	natType := "snat"
	logicalIPVal := logicalSubnet.String()
	logicalIPMask, _ := logicalSubnet.Mask.Size()
	// OVN Values with full prefix mask wont print the /mask
	if logicalIPMask == 32 || logicalIPMask == 128 {
		logicalIPVal = logicalSubnet.IP.String()
	}

	// Search for exact match to see if entry already exists
	uuids, stderr, err := RunOVNNbctl("--columns", "_uuid", "--format=csv", "--no-headings", "find", "nat",
		fmt.Sprintf("external_ip=\"%s\"", externalIP.String()),
		"type="+natType,
		fmt.Sprintf("logical_ip=\"%s\"", logicalIPVal),
	)
	if err != nil {
		return fmt.Errorf("failed to search NAT rules for external_ip %s, logicalSubnet: %s, "+
			"stderr: %q, error: %v", externalIP, logicalSubnet, stderr, err)
	}

	for _, uuid := range strings.Split(uuids, "\n") {
		if len(uuid) == 0 {
			continue
		}
		// Potential matches found, check logical_router
		routerID, stderr, err := RunOVNNbctl("--columns", "_uuid", "--no-headings", "find", "logical_router",
			"name="+router,
			"nat{>=}"+uuid,
		)
		if err != nil {
			return fmt.Errorf("failed to search logical_router for nat entry %s, router: %s, "+
				"stderr: %q, error: %v", uuid, router, stderr, err)
		}
		if len(routerID) > 0 {
			// entry already exists, no update needed
			return nil
		}
	}

	// If we made it here, need to create the entry. To avoid collision with another entry created on the router
	// for the same logicalSubnet, ensure we remove any incorrect entry first
	_, stderr, err = RunOVNNbctl("--if-exists", "lr-nat-del", router, natType, logicalSubnet.String())
	if err != nil {
		return fmt.Errorf("failed to delete NAT rule for pod on router %s, "+
			"stderr: %q, error: %v", router, stderr, err)
	}
	stdout, stderr, err := RunOVNNbctl("lr-nat-add", router, natType, externalIP.String(),
		logicalSubnet.String())
	if err != nil {
		return fmt.Errorf("failed to create NAT rule for pod on router %s, "+
			"stdout: %q, stderr: %q, error: %v", router, stdout, stderr, err)
	}

	return nil
}

// CreateMACBinding Creates MAC binding in OVN SBDB
func CreateMACBinding(logicalPort, datapathName string, portMAC net.HardwareAddr, nextHop net.IP) error {
	datapath, err := GetDatapathUUID(datapathName)
	if err != nil {
		return err
	}

	// Check if exact match already exists
	stdout, stderr, err := RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "MAC_Binding",
		"logical_port="+logicalPort,
		fmt.Sprintf(`mac="%s"`, portMAC),
		"datapath="+datapath,
		fmt.Sprintf("ip=\"%s\"", nextHop))
	if err != nil {
		return fmt.Errorf("failed to check existence of MAC_Binding entry of (%s, %s, %s, %s)"+
			"stderr: %q, error: %v", datapath, logicalPort, portMAC, nextHop, stderr, err)
	}
	if stdout != "" {
		klog.Infof("The MAC_Binding entry of (%s, %s, %s, %s) exists with uuid %s",
			datapath, logicalPort, portMAC, nextHop, stdout)
		return nil
	}

	// Create new binding
	_, stderr, err = RunOVNSbctl("create", "mac_binding", "datapath="+datapath, fmt.Sprintf("ip=\"%s\"", nextHop),
		"logical_port="+logicalPort, fmt.Sprintf(`mac="%s"`, portMAC))
	if err != nil {
		return fmt.Errorf("failed to create a MAC_Binding entry of (%s, %s, %s, %s) "+
			"stderr: %q, error: %v", datapath, logicalPort, portMAC, nextHop, stderr, err)
	}

	return nil
}

// GetDatapathUUID returns the OVN SBDB UUID for a datapath
func GetDatapathUUID(datapathName string) (string, error) {
	// Get datapath from southbound, depending on startup this may take some time, so
	// wait a bit for northd to create the cluster router's datapath in southbound
	var datapath string
	err := wait.PollImmediate(time.Second, 30*time.Second, func() (bool, error) {
		datapath, _, _ = RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "datapath",
			"external_ids:name="+datapathName)
		datapath = strings.TrimSuffix(datapath, "\n")
		// Ignore errors; can't easily detect which are transient or fatal
		return datapath != "", nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to get the datapath UUID of %s from OVN SB "+
			"stdout: %q, error: %v", ovntypes.OVNClusterRouter, datapath, err)
	}
	return datapath, nil
}
