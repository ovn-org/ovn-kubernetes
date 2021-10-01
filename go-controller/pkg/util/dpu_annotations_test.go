package util

import (
	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

var _ = Describe("DPU Annotations test", func() {
	Describe("DPUConnectionDetails", func() {
		var cd DPUConnectionDetails
		var annot map[string]string
		//t := GinkgoT()

		BeforeEach(func() {
			cd = DPUConnectionDetails{}
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for default network", func() {
				cd.PfId = "1"
				cd.VfId = "4"
				cd.SandboxId = "35b82dbe2c3976"
				err := MarshalPodDPUConnDetails(&annot, &cd, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal("1"))
				gomega.Expect(pcd.VfId).To(gomega.Equal("4"))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(
					"35b82dbe2c3976"))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Non-default network", func() {
			BeforeEach(func() {
				cd.PfId = "1"
				cd.VfId = "4"
				cd.SandboxId = "35b82dbe2c3976"
				err := MarshalPodDPUConnDetails(&annot, &cd, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
			})

			It("Get correct Pod annotation for non-default network", func() {
				cd.PfId = "0"
				cd.VfId = "3"
				cd.SandboxId = "35b82dbe2c3973"
				err := MarshalPodDPUConnDetails(&annot, &cd, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnDetails(annot, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal("0"))
				gomega.Expect(pcd.VfId).To(gomega.Equal("3"))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(
					"35b82dbe2c3973"))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnDetails(annot, "non-default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})

	Describe("DPUConnectionStatus", func() {
		var cs DPUConnectionStatus
		var annot map[string]string
		//t := GinkgoT()

		BeforeEach(func() {
			cs = DPUConnectionStatus{}
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for default network", func() {
				cs.Status = "Ready"
				err := MarshalPodDPUConnStatus(&annot, &cs, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcs, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal("Ready"))
				gomega.Expect(pcs.Reason).To(gomega.Equal(""))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Non-default network", func() {
			BeforeEach(func() {
				cs.Status = "Ready"
				err := MarshalPodDPUConnStatus(&annot, &cs, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
			})

			It("Get correct Pod annotation for non-default network", func() {
				cs.Status = "Ready"
				err := MarshalPodDPUConnStatus(&annot, &cs, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcs, err := UnmarshalPodDPUConnStatus(annot, "non-default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal("Ready"))
				gomega.Expect(pcs.Reason).To(gomega.Equal(""))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnStatus(annot, "non-default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})
})
