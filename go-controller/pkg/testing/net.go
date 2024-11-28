package testing

import (
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vishvananda/netlink"
)

// AddLink sets up a dummy link for testing networking implementations
// on the node side
func AddLink(name string) netlink.Link {
	ginkgo.GinkgoHelper()
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
	ginkgo.GinkgoHelper()
	origLink, err := netlink.LinkByName(name)
	Expect(err).NotTo(HaveOccurred())
	err = netlink.LinkDel(origLink)
	Expect(err).NotTo(HaveOccurred())
}

func AddVRFLink(name string, tableId uint32) netlink.Link {
	ginkgo.GinkgoHelper()
	vrfLink := &netlink.Vrf{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Table:     tableId,
	}
	err := netlink.LinkAdd(vrfLink)
	Expect(err).NotTo(HaveOccurred())
	origLink, err := netlink.LinkByName(name)
	Expect(err).NotTo(HaveOccurred())
	err = netlink.LinkSetUp(origLink)
	Expect(err).NotTo(HaveOccurred())
	return origLink
}
