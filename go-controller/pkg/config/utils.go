package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	utilnet "k8s.io/utils/net"
)

// HostPort is the object that holds the definition for a host and port tuple
type HostPort struct {
	Host *net.IP
	Port int32
}

// String representation of a HostPort entry
func (hp *HostPort) String() string {
	switch {
	case hp.Host == nil:
		return fmt.Sprintf(":%d", hp.Port)
	case hp.Host.To4() != nil:
		return fmt.Sprintf("%s:%d", *hp.Host, hp.Port)
	default:
		return fmt.Sprintf("[%s]:%d", *hp.Host, hp.Port)
	}
}

// CIDRNetworkEntry is the object that holds the definition for a single network CIDR range
type CIDRNetworkEntry struct {
	CIDR             *net.IPNet
	HostSubnetLength int
}

func (c CIDRNetworkEntry) String() string {
	return fmt.Sprintf("%s/%d", c.CIDR.String(), c.HostSubnetLength)
}

// ParseClusterSubnetEntriesWithDefaults returns the parsed set of
// CIDRNetworkEntries. These entries define a network space by specifying a set
// of CIDR and netmasks the SDN can allocate addresses from including how that
// network space is partitioned for each of the cluster nodes. When no host
// specific prefix length is specified, the provided ones are assumed as
// default. The host specific prefix length is validated to be greater than the
// overall subnet length. When 0 is specified as default host specific prefix
// length, no host specific prefix length is allowed or validated.
func ParseClusterSubnetEntriesWithDefaults(clusterSubnetCmd string, ipv4HostLength, ipv6HostLength int) ([]CIDRNetworkEntry, error) {
	var parsedClusterList []CIDRNetworkEntry
	clusterEntriesList := strings.Split(clusterSubnetCmd, ",")

	ipv4HostLengthAllowed := ipv4HostLength != 0
	ipv6HostLengthAllowed := ipv6HostLength != 0

	for _, clusterEntry := range clusterEntriesList {
		clusterEntry := strings.TrimSpace(clusterEntry)
		splitClusterEntry := strings.Split(clusterEntry, "/")

		if len(splitClusterEntry) < 2 || len(splitClusterEntry) > 3 {
			return nil, fmt.Errorf("CIDR %q not properly formatted", clusterEntry)
		}

		var err error
		var parsedClusterEntry CIDRNetworkEntry
		_, parsedClusterEntry.CIDR, err = net.ParseCIDR(fmt.Sprintf("%s/%s", splitClusterEntry[0], splitClusterEntry[1]))
		if err != nil {
			return nil, err
		}

		ipv6 := utilnet.IsIPv6(parsedClusterEntry.CIDR.IP)
		hostLengthAllowed := (ipv6 && ipv6HostLengthAllowed) || (!ipv6 && ipv4HostLengthAllowed)

		entryMaskLength, _ := parsedClusterEntry.CIDR.Mask.Size()
		if len(splitClusterEntry) == 3 {
			if !hostLengthAllowed {
				return nil, fmt.Errorf("CIDR %q not properly formatted", clusterEntry)
			}
			tmp, err := strconv.Atoi(splitClusterEntry[2])
			if err != nil {
				return nil, err
			}
			parsedClusterEntry.HostSubnetLength = tmp
		} else {
			if ipv6 {
				parsedClusterEntry.HostSubnetLength = ipv6HostLength
			} else {
				// default for backward compatibility
				parsedClusterEntry.HostSubnetLength = ipv4HostLength
			}
		}

		if hostLengthAllowed {
			if ipv6 && ipv6HostLengthAllowed && parsedClusterEntry.HostSubnetLength != 64 {
				return nil, fmt.Errorf("IPv6 only supports /64 host subnets")
			}

			if !ipv6 && parsedClusterEntry.HostSubnetLength > 32 {
				return nil, fmt.Errorf("invalid host subnet, IPv4 subnet must be < 32")
			}

			if parsedClusterEntry.HostSubnetLength <= entryMaskLength {
				return nil, fmt.Errorf("cannot use a host subnet length mask shorter than or equal to the cluster subnet mask. "+
					"host subnet length: %d, cluster subnet length: %d", parsedClusterEntry.HostSubnetLength, entryMaskLength)
			}
		}

		parsedClusterList = append(parsedClusterList, parsedClusterEntry)
	}

	if len(parsedClusterList) == 0 {
		return nil, fmt.Errorf("failed to parse any CIDRs from %q", clusterSubnetCmd)
	}

	return parsedClusterList, nil
}

// ParseClusterSubnetEntries returns the parsed set of
// CIDRNetworkEntries. If not specified, it assumes a default host specific
// prefix length of 24 or 64 bits for ipv4 and ipv6 respectively.
func ParseClusterSubnetEntries(clusterSubnetCmd string) ([]CIDRNetworkEntry, error) {
	// default to 24 bits host specific prefix length for backward compatibility
	return ParseClusterSubnetEntriesWithDefaults(clusterSubnetCmd, 24, 64)
}

// ParseFlowCollectors returns the parsed set of HostPorts passed by the user on the command line
// These entries define the flow collectors OVS will send flow metadata by using NetFlow/SFlow/IPFIX.
func ParseFlowCollectors(flowCollectors string) ([]HostPort, error) {
	var parsedFlowsCollectors []HostPort
	readCollectors := map[string]struct{}{}
	collectors := strings.Split(flowCollectors, ",")
	for _, v := range collectors {
		host, port, err := net.SplitHostPort(v)
		if err != nil {
			return nil, fmt.Errorf("cannot parse hostport: %v", err)
		}
		var ipp *net.IP
		// If the host IP is not provided, we keep it nil and later will assume the Node IP
		if host != "" {
			ip := net.ParseIP(host)
			if ip == nil {
				return nil, fmt.Errorf("collector IP %s is not a valid IP", host)
			}
			ipp = &ip
		}
		parsedPort, err := strconv.ParseInt(port, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("collector port %s is not a valid port: %v", port, err)
		}
		// checking if HostPort entry is duplicate
		hostPort := HostPort{Host: ipp, Port: int32(parsedPort)}
		hps := hostPort.String()
		if _, ok := readCollectors[hps]; ok {
			// duplicate flow collector. Ignore it
			continue
		}
		readCollectors[hps] = struct{}{}
		parsedFlowsCollectors = append(parsedFlowsCollectors, hostPort)
	}

	return parsedFlowsCollectors, nil
}

type ConfigSubnetType string

const (
	ConfigSubnetJoin       ConfigSubnetType = "built-in join subnet"
	ConfigSubnetCluster    ConfigSubnetType = "cluster subnet"
	ConfigSubnetService    ConfigSubnetType = "service subnet"
	ConfigSubnetHybrid     ConfigSubnetType = "hybrid overlay subnet"
	ConfigSubnetMasquerade ConfigSubnetType = "masquerade subnet"
	ConfigSubnetTransit    ConfigSubnetType = "transit switch subnet"
	UserDefinedSubnets     ConfigSubnetType = "user defined subnet"
	UserDefinedJoinSubnet  ConfigSubnetType = "user defined join subnet"
)

type ConfigSubnet struct {
	SubnetType ConfigSubnetType
	Subnet     *net.IPNet
}

// ConfigSubnets represents a set of configured subnets (and their names)
type ConfigSubnets struct {
	Subnets []ConfigSubnet
	V4      map[ConfigSubnetType]bool
	V6      map[ConfigSubnetType]bool
}

// NewConfigSubnets returns a new ConfigSubnets
func NewConfigSubnets() *ConfigSubnets {
	return &ConfigSubnets{
		V4: make(map[ConfigSubnetType]bool),
		V6: make(map[ConfigSubnetType]bool),
	}
}

// append adds a single subnet to cs
func (cs *ConfigSubnets) Append(subnetType ConfigSubnetType, subnet *net.IPNet) {
	cs.Subnets = append(cs.Subnets, ConfigSubnet{SubnetType: subnetType, Subnet: subnet})
	if subnetType == ConfigSubnetCluster || subnetType == ConfigSubnetService || subnetType == ConfigSubnetHybrid {
		if utilnet.IsIPv6CIDR(subnet) {
			cs.V6[subnetType] = true
		} else {
			cs.V4[subnetType] = true
		}
	}
}

// CheckForOverlaps checks if any of the subnets in cs overlap
func (cs *ConfigSubnets) CheckForOverlaps() error {
	for i, si := range cs.Subnets {
		for j := 0; j < i; j++ {
			sj := cs.Subnets[j]
			if si.Subnet.Contains(sj.Subnet.IP) || sj.Subnet.Contains(si.Subnet.IP) {
				return fmt.Errorf("illegal network configuration: %s %q overlaps %s %q",
					si.SubnetType, si.Subnet.String(),
					sj.SubnetType, sj.Subnet.String())
			}
		}
	}
	return nil
}

func (cs *ConfigSubnets) describeSubnetType(subnetType ConfigSubnetType) string {
	ipv4 := cs.V4[subnetType]
	ipv6 := cs.V6[subnetType]
	var familyType string
	switch {
	case ipv4 && !ipv6:
		familyType = "IPv4"
	case !ipv4 && ipv6:
		familyType = "IPv6"
	case ipv4 && ipv6:
		familyType = "dual-stack"
	default:
		familyType = "unknown type"
	}
	return familyType + " " + string(subnetType)
}

// checkIPFamilies determines if cs contains a valid single-stack IPv4 configuration, a
// valid single-stack IPv6 configuration, a valid dual-stack configuration, or none of the
// above.
func (cs *ConfigSubnets) checkIPFamilies() (usingIPv4, usingIPv6 bool, err error) {
	if len(cs.V6) == 0 {
		// Single-stack IPv4
		return true, false, nil
	} else if len(cs.V4) == 0 {
		// Single-stack IPv6
		return false, true, nil
	} else if reflect.DeepEqual(cs.V4, cs.V6) {
		// Dual-stack
		return true, true, nil
	}

	netConfig := cs.describeSubnetType(ConfigSubnetCluster)
	netConfig += ", " + cs.describeSubnetType(ConfigSubnetService)
	if cs.V4[ConfigSubnetHybrid] || cs.V6[ConfigSubnetHybrid] {
		netConfig += ", " + cs.describeSubnetType(ConfigSubnetHybrid)
	}

	return false, false, fmt.Errorf("illegal network configuration: %s", netConfig)
}

func ContainsJoinIP(ip net.IP) bool {
	var joinSubnetsConfig []string
	if IPv4Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, Gateway.V4JoinSubnet)
	}
	if IPv6Mode {
		joinSubnetsConfig = append(joinSubnetsConfig, Gateway.V6JoinSubnet)
	}

	for _, subnet := range joinSubnetsConfig {
		_, joinSubnet, _ := net.ParseCIDR(subnet)
		if joinSubnet.Contains(ip) {
			return true
		}
	}
	return false
}

// masqueradeIP represents the masqueradeIPs used by the masquerade subnets for host to service traffic
type MasqueradeIPsConfig struct {
	V4OVNMasqueradeIP               net.IP
	V6OVNMasqueradeIP               net.IP
	V4HostMasqueradeIP              net.IP
	V6HostMasqueradeIP              net.IP
	V4HostETPLocalMasqueradeIP      net.IP
	V6HostETPLocalMasqueradeIP      net.IP
	V4DummyNextHopMasqueradeIP      net.IP
	V6DummyNextHopMasqueradeIP      net.IP
	V4OVNServiceHairpinMasqueradeIP net.IP
	V6OVNServiceHairpinMasqueradeIP net.IP
}

// allocateV4/6MasqueradeIPs allocates the masqueradeIPs based off of the passed in masqueradeSubnet (.0)
// it does this by cascading down from the initial ip down to the .5 currently (more masqueradeIps may be added in the future)

func AllocateV4MasqueradeIPs(masqueradeSubnetNetworkAddress net.IP, masqueradeIPs *MasqueradeIPsConfig) error {
	masqueradeIPs.V4OVNMasqueradeIP = iputils.NextIP(masqueradeSubnetNetworkAddress)
	if masqueradeIPs.V4OVNMasqueradeIP == nil {
		return fmt.Errorf("error setting V4OVNMasqueradeIP: %s", masqueradeSubnetNetworkAddress)
	}
	masqueradeIPs.V4HostMasqueradeIP = iputils.NextIP(masqueradeIPs.V4OVNMasqueradeIP) //using the last set ip we can cascade from the .0 down
	if masqueradeIPs.V4HostMasqueradeIP == nil {
		return fmt.Errorf("error setting V4HostMasqueradeIP: %s", masqueradeIPs.V4OVNMasqueradeIP)
	}
	masqueradeIPs.V4HostETPLocalMasqueradeIP = iputils.NextIP(masqueradeIPs.V4HostMasqueradeIP)
	if masqueradeIPs.V4HostETPLocalMasqueradeIP == nil {
		return fmt.Errorf("error setting V4HostETPLocalMasqueradeIP: %s", masqueradeIPs.V4HostMasqueradeIP)
	}
	masqueradeIPs.V4DummyNextHopMasqueradeIP = iputils.NextIP(masqueradeIPs.V4HostETPLocalMasqueradeIP)
	if masqueradeIPs.V4DummyNextHopMasqueradeIP == nil {
		return fmt.Errorf("error setting V4DummyNextHopMasqueradeIP: %s", masqueradeIPs.V4HostETPLocalMasqueradeIP)
	}
	masqueradeIPs.V4OVNServiceHairpinMasqueradeIP = iputils.NextIP(masqueradeIPs.V4DummyNextHopMasqueradeIP)
	if masqueradeIPs.V4OVNServiceHairpinMasqueradeIP == nil {
		return fmt.Errorf("error setting V4OVNServiceHairpinMasqueradeIP: %s", masqueradeIPs.V4DummyNextHopMasqueradeIP)
	}
	return nil
}

func AllocateV6MasqueradeIPs(masqueradeSubnetNetworkAddress net.IP, masqueradeIPs *MasqueradeIPsConfig) error {
	masqueradeIPs.V6OVNMasqueradeIP = iputils.NextIP(masqueradeSubnetNetworkAddress)
	if masqueradeIPs.V6OVNMasqueradeIP == nil {
		return fmt.Errorf("error setting V6OVNMasqueradeIP: %s", masqueradeSubnetNetworkAddress)
	}
	masqueradeIPs.V6HostMasqueradeIP = iputils.NextIP(masqueradeIPs.V6OVNMasqueradeIP) //using the last set ip we can cascade from the .0 down
	if masqueradeIPs.V6HostMasqueradeIP == nil {
		return fmt.Errorf("error setting V6HostMasqueradeIP: %s", masqueradeIPs.V6OVNMasqueradeIP)
	}
	masqueradeIPs.V6HostETPLocalMasqueradeIP = iputils.NextIP(masqueradeIPs.V6HostMasqueradeIP)
	if masqueradeIPs.V6HostETPLocalMasqueradeIP == nil {
		return fmt.Errorf("error setting V6HostETPLocalMasqueradeIP: %s", masqueradeIPs.V6HostMasqueradeIP)
	}
	masqueradeIPs.V6DummyNextHopMasqueradeIP = iputils.NextIP(masqueradeIPs.V6HostETPLocalMasqueradeIP)
	if masqueradeIPs.V6DummyNextHopMasqueradeIP == nil {
		return fmt.Errorf("error setting V6DummyNextHopMasqueradeIP: %s", masqueradeIPs.V6HostETPLocalMasqueradeIP)
	}
	masqueradeIPs.V6OVNServiceHairpinMasqueradeIP = iputils.NextIP(masqueradeIPs.V6DummyNextHopMasqueradeIP)
	if masqueradeIPs.V6OVNServiceHairpinMasqueradeIP == nil {
		return fmt.Errorf("error setting V6OVNServiceHairpinMasqueradeIP: %s", masqueradeIPs.V6DummyNextHopMasqueradeIP)
	}
	return nil
}
