package multi_homing

import (
	"errors"
	"sync"
)

var ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")

// NetNameInfo is structure which holds network name information
type NetNameInfo struct {
	// netconf's name, default for default network
	NetName string
	// Prefix of OVN logical entities for this network
	Prefix      string
	IsSecondary bool
}

// NetAttachDefInfo is structure which holds specific per-network information
type NetAttachDefInfo struct {
	NetNameInfo
	NetCidr string
	MTU     int
	// net-attach-defs shared the same CNI Conf, key is <Namespace>/<Name> of net-attach-def.
	// Note that it means they share the same logical switch (subnet cidr/MTU etc), but they might
	// have different resource requirement (requires or not require VF, or different VF resource set)
	NetAttachDefs sync.Map
}
