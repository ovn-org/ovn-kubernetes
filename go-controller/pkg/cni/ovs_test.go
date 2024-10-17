package cni

import (
	"fmt"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CNI OVS tests", func() {
	var fexec *ovntest.FakeExec

	BeforeEach(func() {
		fexec = ovntest.NewFakeExec()
		SetExec(fexec)
	})

	It("returns non-empty elements from ovsFind", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=_uuid find Interface external-ids:iface-id=foobar",
			Output: `75419b50-ec6e-4989-b769-164488f53375
4609184a-cb69-46ed-880f-807b6a4e99f5
d9af11aa-37c3-4ea9-8ba3-a74843cc0f47
`,
		})

		uuids, err := ovsFind("Interface", "_uuid", "external-ids:iface-id=foobar")
		Expect(err).NotTo(HaveOccurred())

		Expect(len(uuids)).To(Equal(3), fmt.Sprintf("got %v", uuids))
		Expect(uuids[0]).To(Equal("75419b50-ec6e-4989-b769-164488f53375"))
		Expect(uuids[1]).To(Equal("4609184a-cb69-46ed-880f-807b6a4e99f5"))
		Expect(uuids[2]).To(Equal("d9af11aa-37c3-4ea9-8ba3-a74843cc0f47"))
	})

	It("returns nil if no element is returned from ovsFind", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=_uuid find Interface external-ids:iface-id=foobar",
			Output: "",
		})

		uuids, err := ovsFind("Interface", "_uuid", "external-ids:iface-id=foobar")
		Expect(err).NotTo(HaveOccurred())
		Expect(uuids).To(BeNil())
	})

	It("returns empty values if the elements themselves are empty from ovsFind", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=_uuid find Interface external-ids:iface-id=foobar",
			Output: `

`,
		})

		uuids, err := ovsFind("Interface", "_uuid", "external-ids:iface-id=foobar")
		Expect(err).NotTo(HaveOccurred())
		Expect(len(uuids)).To(Equal(2), fmt.Sprintf("got %v", uuids))
		Expect(uuids[0]).To(Equal(""))
		Expect(uuids[1]).To(Equal(""))
	})

	It("returns quoted empty values if the elements themselves are empty from ovsFind", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=_uuid find Interface external-ids:iface-id=foobar",
			Output: `""
""
`,
		})

		uuids, err := ovsFind("Interface", "_uuid", "external-ids:iface-id=foobar")
		Expect(err).NotTo(HaveOccurred())
		Expect(len(uuids)).To(Equal(2), fmt.Sprintf("got %v", uuids))
		Expect(uuids[0]).To(Equal(`""`))
		Expect(uuids[1]).To(Equal(`""`))
	})

	It("returns unquoted value if the elements themselves are quoted from ovsGet", func() {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=30 --if-exists get Interface blah external-ids:iface-id",
			Output: `"1234"
`,
		})

		ifaceID, err := ovsGet("Interface", "blah", "external-ids", "iface-id")
		Expect(err).NotTo(HaveOccurred())
		Expect(ifaceID).To(Equal(`1234`))
	})
})
