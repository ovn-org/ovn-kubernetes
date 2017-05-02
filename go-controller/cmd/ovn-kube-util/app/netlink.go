package app

import (
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// addrList gets netlink.Addr for specified netlink.Link.
// TODO: remove this after https://github.com/vishvananda/netlink/pull/226 is merged.
func addrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	h := &netlink.Handle{}
	req := nl.NewNetlinkRequest(syscall.RTM_GETADDR, syscall.NLM_F_DUMP)
	msg := nl.NewIfInfomsg(family)
	req.AddData(msg)

	msgs, err := req.Execute(syscall.NETLINK_ROUTE, syscall.RTM_NEWADDR)
	if err != nil {
		return nil, err
	}

	indexFilter := 0
	if link != nil {
		base := link.Attrs()

		// ensure index
		if base != nil && base.Index == 0 {
			newlink, _ := h.LinkByName(base.Name)
			if newlink != nil {
				base.Index = newlink.Attrs().Index
			}
		}
		indexFilter = base.Index
	}

	var res []netlink.Addr
	for _, m := range msgs {
		addr, msgFamily, ifindex, err := parseAddr(m)
		if err != nil {
			return res, err
		}

		if link != nil && ifindex != indexFilter {
			// Ignore messages from other interfaces
			continue
		}

		if family != netlink.FAMILY_ALL && msgFamily != family {
			continue
		}

		res = append(res, addr)
	}

	return res, nil
}

// parseAddr parses netlink.Addr from netlink message.
func parseAddr(m []byte) (addr netlink.Addr, family, index int, err error) {
	msg := nl.DeserializeIfAddrmsg(m)

	family = -1
	index = -1

	attrs, err1 := nl.ParseRouteAttr(m[msg.Len():])
	if err1 != nil {
		err = err1
		return
	}

	family = int(msg.Family)
	index = int(msg.Index)

	var local, dst *net.IPNet
	for _, attr := range attrs {
		switch attr.Attr.Type {
		case syscall.IFA_ADDRESS:
			dst = &net.IPNet{
				IP:   attr.Value,
				Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
			}
			addr.Peer = dst
		case syscall.IFA_LOCAL:
			local = &net.IPNet{
				IP:   attr.Value,
				Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
			}
			addr.IPNet = local
		case syscall.IFA_BROADCAST:
			addr.Broadcast = attr.Value
		case syscall.IFA_LABEL:
			addr.Label = string(attr.Value[:len(attr.Value)-1])
		case netlink.IFA_FLAGS:
			addr.Flags = int(nl.NativeEndian().Uint32(attr.Value[0:4]))
		case nl.IFA_CACHEINFO:
			ci := nl.DeserializeIfaCacheInfo(attr.Value)
			addr.PreferedLft = int(ci.IfaPrefered)
			addr.ValidLft = int(ci.IfaValid)
		}
	}

	// IFA_LOCAL should be there but if not, fall back to IFA_ADDRESS
	if local != nil {
		addr.IPNet = local
	} else {
		addr.IPNet = dst
	}
	addr.Scope = int(msg.Scope)

	return
}
