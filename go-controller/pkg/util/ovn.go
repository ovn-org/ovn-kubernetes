package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	"fmt"
	"net"
	"strings"
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
