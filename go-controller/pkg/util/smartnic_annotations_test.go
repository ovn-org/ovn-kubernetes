package util

import (
	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

var _ = Describe("Smart-NIC Annotations test", func() {
	Describe("SmartNICConnectionDetails", func() {
		var cd SmartNICConnectionDetails
		var annot map[string]string
		//t := GinkgoT()

		BeforeEach(func() {
			cd = SmartNICConnectionDetails{}
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for default network", func() {
				cd.PfId = "1"
				cd.VfId = "4"
				cd.SandboxId = "35b82dbe2c3976"
				err := MarshalPodSmartNicConnDetails(&annot, &cd, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodSmartNicConnDetails(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal("1"))
				gomega.Expect(pcd.VfId).To(gomega.Equal("4"))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(
					"35b82dbe2c3976"))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodSmartNicConnDetails(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Non-default network", func() {
			BeforeEach(func() {
				cd.PfId = "1"
				cd.VfId = "4"
				cd.SandboxId = "35b82dbe2c3976"
				err := MarshalPodSmartNicConnDetails(&annot, &cd, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
			})

			It("Get correct Pod annotation for non-default network", func() {
				cd.PfId = "0"
				cd.VfId = "3"
				cd.SandboxId = "35b82dbe2c3973"
				err := MarshalPodSmartNicConnDetails(&annot, &cd, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodSmartNicConnDetails(annot, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal("0"))
				gomega.Expect(pcd.VfId).To(gomega.Equal("3"))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(
					"35b82dbe2c3973"))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodSmartNicConnDetails(annot, "non-default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})

	Describe("SmartNICConnectionStatus", func() {
		var cs SmartNICConnectionStatus
		var annot map[string]string
		//t := GinkgoT()

		BeforeEach(func() {
			cs = SmartNICConnectionStatus{}
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for default network", func() {
				cs.Status = "Ready"
				err := MarshalPodSmartNicConnStatus(&annot, &cs, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcs, err := UnmarshalPodSmartNicConnStatus(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal("Ready"))
				gomega.Expect(pcs.Reason).To(gomega.Equal(""))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodSmartNicConnStatus(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Non-default network", func() {
			BeforeEach(func() {
				cs.Status = "Ready"
				err := MarshalPodSmartNicConnStatus(&annot, &cs, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
			})

			It("Get correct Pod annotation for non-default network", func() {
				cs.Status = "Ready"
				err := MarshalPodSmartNicConnStatus(&annot, &cs, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcs, err := UnmarshalPodSmartNicConnStatus(annot, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal("Ready"))
				gomega.Expect(pcs.Reason).To(gomega.Equal(""))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodSmartNicConnStatus(annot, "non-default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})
})
