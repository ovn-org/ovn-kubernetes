package loadbalancer

// LB is a desired or existing load_balancer configuration in OVN.
type LB struct {
	Name        string
	UUID        string
	Protocol    string // one of TCP, UDP, SCTP
	ExternalIDs map[string]string
	Opts        LBOpts

	Rules []LBRule

	// the names of logical switches and routers that this LB should be attached to
	Switches []string
	Routers  []string
}

type LBOpts struct {
	// if true, then enable unidling. Otherwise, generate reject
	Unidling bool

	// If true, then enable per-client-IP affinity.
	Affinity bool
}

type Addr struct {
	IP   string
	Port int32
}

type LBRule struct {
	Source  Addr
	Targets []Addr
}
