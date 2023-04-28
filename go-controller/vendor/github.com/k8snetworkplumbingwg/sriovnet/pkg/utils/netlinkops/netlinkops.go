package netlinkops

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

var nlOpsImpl NetlinkOps

// NetlinkOps is an interface wrapping netlink to be used by sriovnet
type NetlinkOps interface {
	// LinkByName gets link by netdev name
	LinkByName(name string) (netlink.Link, error)
	// LinkSetUp sets Link state to up
	LinkSetUp(link netlink.Link) error
	// LinkSetVfHardwareAddr sets VF hardware address
	LinkSetVfHardwareAddr(link netlink.Link, vf int, hwaddr net.HardwareAddr) error
	// LinkSetVfVlan sets VF vlan
	LinkSetVfVlan(link netlink.Link, vf, vlan int) error
	// LinkSetVfNodeGUID sets VF Node GUID
	LinkSetVfNodeGUID(link netlink.Link, vf int, nodeguid net.HardwareAddr) error
	// LinkSetVfPortGUID sets VF Port GUID
	LinkSetVfPortGUID(link netlink.Link, vf int, portguid net.HardwareAddr) error
	// LinkSetVfTrust sets VF trust for the given VF
	LinkSetVfTrust(link netlink.Link, vf int, state bool) error
	// LinkSetVfSpoofchk sets VF spoofchk for the given VF
	LinkSetVfSpoofchk(link netlink.Link, vf int, check bool) error
	// DevLinkGetAllPortList gets all devlink ports
	DevLinkGetAllPortList() ([]*netlink.DevlinkPort, error)
	// DevLinkGetPortByNetdevName gets devlink port by netdev name
	DevLinkGetPortByNetdevName(netdev string) (*netlink.DevlinkPort, error)
}

// GetNetlinkOps returns NetlinkOps interface
func GetNetlinkOps() NetlinkOps {
	if nlOpsImpl == nil {
		nlOpsImpl = &netlinkOps{}
	}
	return nlOpsImpl
}

// SetNetlinkOps sets NetlinkOps interface (to be used by unit tests)
func SetNetlinkOps(nlops NetlinkOps) {
	nlOpsImpl = nlops
}

// ResetNetlinkOps resets nlOpsImpl to nil
func ResetNetlinkOps() {
	nlOpsImpl = nil
}

type netlinkOps struct{}

// LinkByName gets link by netdev name
func (nlo *netlinkOps) LinkByName(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

// LinkSetUp sets Link state to up
func (nlo *netlinkOps) LinkSetUp(link netlink.Link) error {
	return netlink.LinkSetUp(link)
}

// LinkSetVfHardwareAddr sets VF hardware address
func (nlo *netlinkOps) LinkSetVfHardwareAddr(link netlink.Link, vf int, hwaddr net.HardwareAddr) error {
	return netlink.LinkSetVfHardwareAddr(link, vf, hwaddr)
}

// LinkSetVfVlan sets VF vlan
func (nlo *netlinkOps) LinkSetVfVlan(link netlink.Link, vf, vlan int) error {
	return netlink.LinkSetVfVlan(link, vf, vlan)
}

// LinkSetVfNodeGUID sets VF Node GUID
func (nlo *netlinkOps) LinkSetVfNodeGUID(link netlink.Link, vf int, nodeguid net.HardwareAddr) error {
	return netlink.LinkSetVfNodeGUID(link, vf, nodeguid)
}

// LinkSetVfPortGUID sets VF Port GUID
func (nlo *netlinkOps) LinkSetVfPortGUID(link netlink.Link, vf int, portguid net.HardwareAddr) error {
	return netlink.LinkSetVfPortGUID(link, vf, portguid)
}

// LinkSetVfTrust sets VF trust for the given VF
func (nlo *netlinkOps) LinkSetVfTrust(link netlink.Link, vf int, state bool) error {
	return netlink.LinkSetVfTrust(link, vf, state)
}

// LinkSetVfSpoofchk sets VF spoofchk for the given VF
func (nlo *netlinkOps) LinkSetVfSpoofchk(link netlink.Link, vf int, check bool) error {
	return netlink.LinkSetVfSpoofchk(link, vf, check)
}

// DevLinkGetAllPortList gets all devlink ports
func (nlo *netlinkOps) DevLinkGetAllPortList() ([]*netlink.DevlinkPort, error) {
	return netlink.DevLinkGetAllPortList()
}

// DevLinkGetPortByNetdevName gets devlink port by netdev name
func (nlo *netlinkOps) DevLinkGetPortByNetdevName(netdev string) (*netlink.DevlinkPort, error) {
	ports, err := netlink.DevLinkGetAllPortList()
	if err != nil {
		return nil, err
	}

	for _, port := range ports {
		if netdev == port.NetdeviceName {
			return port, nil
		}
	}
	return nil, fmt.Errorf("failed to get devlink port for netdev %s", netdev)
}
