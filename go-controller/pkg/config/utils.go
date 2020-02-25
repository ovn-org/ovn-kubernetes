package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// CIDRNetworkEntry is the object that holds the definition for a single network CIDR range
type CIDRNetworkEntry struct {
	CIDR             *net.IPNet
	HostSubnetLength uint32
}

func (e CIDRNetworkEntry) HostBits() uint32 {
	_, addrLen := e.CIDR.Mask.Size()
	return uint32(addrLen) - e.HostSubnetLength
}

// ParseClusterSubnetEntries returns the parsed set of CIDRNetworkEntries passed by the user on the command line
// These entries define the clusters network space by specifying a set of CIDR and netmasks the SDN can allocate
// addresses from.
func ParseClusterSubnetEntries(clusterSubnetCmd string) ([]CIDRNetworkEntry, error) {
	var parsedClusterList []CIDRNetworkEntry
	ipv6 := false
	clusterEntriesList := strings.Split(clusterSubnetCmd, ",")

	for _, clusterEntry := range clusterEntriesList {
		var parsedClusterEntry CIDRNetworkEntry

		splitClusterEntry := strings.Split(clusterEntry, "/")

		if len(splitClusterEntry) < 2 || len(splitClusterEntry) > 3 {
			return nil, fmt.Errorf("CIDR %q not properly formatted", clusterEntry)
		}

		var err error
		_, parsedClusterEntry.CIDR, err = net.ParseCIDR(fmt.Sprintf("%s/%s", splitClusterEntry[0], splitClusterEntry[1]))
		if err != nil {
			return nil, err
		}

		if parsedClusterEntry.CIDR.IP.To4() == nil {
			ipv6 = true
		}

		entryMaskLength, _ := parsedClusterEntry.CIDR.Mask.Size()
		if len(splitClusterEntry) == 3 {
			tmp, err := strconv.ParseUint(splitClusterEntry[2], 10, 32)
			if err != nil {
				return nil, err
			}
			parsedClusterEntry.HostSubnetLength = uint32(tmp)

			if ipv6 && parsedClusterEntry.HostSubnetLength != 64 {
				return nil, fmt.Errorf("IPv6 only supports /64 host subnets")
			}
		} else {
			if ipv6 {
				parsedClusterEntry.HostSubnetLength = 64
			} else {
				// default for backward compatibility
				parsedClusterEntry.HostSubnetLength = 24
			}
		}

		if parsedClusterEntry.HostSubnetLength <= uint32(entryMaskLength) {
			return nil, fmt.Errorf("cannot use a host subnet length mask shorter than or equal to the cluster subnet mask. "+
				"host subnet length: %d, cluster subnet length: %d", parsedClusterEntry.HostSubnetLength, entryMaskLength)
		}

		//check to make sure that no cidrs overlap
		if cidrsOverlap(parsedClusterEntry.CIDR, parsedClusterList) {
			return nil, fmt.Errorf("CIDR %q overlaps with another cluster network CIDR", clusterEntry)
		}

		parsedClusterList = append(parsedClusterList, parsedClusterEntry)
	}

	if len(parsedClusterList) == 0 {
		return nil, fmt.Errorf("failed to parse any CIDRs from %q", clusterSubnetCmd)
	}

	return parsedClusterList, nil
}

//cidrsOverlap returns a true if the cidr range overlaps any in the list of cidr ranges
func cidrsOverlap(cidr *net.IPNet, cidrList []CIDRNetworkEntry) bool {
	for _, clusterEntry := range cidrList {
		if cidr.Contains(clusterEntry.CIDR.IP) || clusterEntry.CIDR.Contains(cidr.IP) {
			return true
		}
	}
	return false
}
