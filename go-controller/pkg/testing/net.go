package testing

import (
	. "github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

// AddLink sets up a dummy link for testing networking implementations
// on the node side
func AddLink(name string) netlink.Link {
	err := netlink.LinkAdd(&netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	})
	Expect(err).NotTo(HaveOccurred())
	origLink, err := netlink.LinkByName(name)
	Expect(err).NotTo(HaveOccurred())
	err = netlink.LinkSetUp(origLink)
	Expect(err).NotTo(HaveOccurred())
	return origLink
}

func DelLink(name string) {
	origLink, err := netlink.LinkByName(name)
	Expect(err).NotTo(HaveOccurred())
	err = netlink.LinkDel(origLink)
	Expect(err).NotTo(HaveOccurred())
}
