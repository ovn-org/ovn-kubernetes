package diagnostics

import (
	"k8s.io/kubernetes/test/e2e/framework"
)

type Diagnostics struct {
	fr                                     *framework.Framework
	conntrack, iptables, ovsflows, tcpdump bool
}

func New(fr *framework.Framework) *Diagnostics {
	return &Diagnostics{
		fr:        fr,
		conntrack: conntrack,
		iptables:  iptables,
		ovsflows:  ovsflows,
		tcpdump:   tcpdump,
	}
}
