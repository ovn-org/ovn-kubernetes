package loadbalancer

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

// LB is a desired or existing load_balancer configuration in OVN.
type LB struct {
	Name        string
	UUID        string
	Protocol    string // one of TCP, UDP, SCTP
	ExternalIDs map[string]string
	Opts        LBOpts

	Rules []LBRule

	// the names of logical switches, routers and LB groups that this LB should be attached to
	Switches []string
	Routers  []string
	Groups   []string
}

type LBOpts struct {
	// if true, then enable unidling. Otherwise, generate reject
	Unidling bool

	// If greater than 0, then enable per-client-IP affinity.
	AffinityTimeOut int32

	// If true, then disable SNAT entirely
	SkipSNAT bool
}

type Addr struct {
	IP   string
	Port int32
}

type LBRule struct {
	Source  Addr
	Targets []Addr
}

// JoinJostsPort takes a list of IPs and a port and converts it to a list of Addrs
func JoinHostsPort(ips []string, port int32) []Addr {
	out := make([]Addr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, Addr{IP: ip, Port: port})
	}
	return out
}

func (a *Addr) String() string {
	return util.JoinHostPortInt32(a.IP, a.Port)
}

func (a *Addr) Equals(b *Addr) bool {
	return a.Port == b.Port && a.IP == b.IP
}
