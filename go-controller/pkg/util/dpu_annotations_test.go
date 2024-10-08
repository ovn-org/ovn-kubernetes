package util

import (
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = Describe("DPU Annotations test", func() {
	Describe("DPUConnectionDetails", func() {
		var defaultCD, secondCD DPUConnectionDetails
		var annot map[string]string
		var legacyAnnot string
		var err error

		BeforeEach(func() {
			defaultCD = DPUConnectionDetails{PfId: "1", VfId: "4", SandboxId: "35b82dbe2c3976"}
			secondCD = DPUConnectionDetails{PfId: "0", VfId: "3", SandboxId: "35b82dbe2c3973"}
			legacyAnnot = `{"pfId": "1", "vfId": "4", "sandboxId": "35b82dbe2c3976"}`
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for the legacy default network pod annotation", func() {
				annot[DPUConnectionDetailsAnnot] = legacyAnnot
				pcd, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal(defaultCD.PfId))
				gomega.Expect(pcd.VfId).To(gomega.Equal(defaultCD.VfId))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(defaultCD.SandboxId))
			})

			It("Get correct Pod annotation for default network", func() {
				annot, err = MarshalPodDPUConnDetails(annot, &defaultCD, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal(defaultCD.PfId))
				gomega.Expect(pcd.VfId).To(gomega.Equal(defaultCD.VfId))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(defaultCD.SandboxId))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Second network", func() {
			It("Get correct Pod annotation for Second network with legacy default network annoation", func() {
				annot[DPUConnectionDetailsAnnot] = legacyAnnot
				annot, err = MarshalPodDPUConnDetails(annot, &secondCD, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnDetails(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal(defaultCD.PfId))
				gomega.Expect(pcd.VfId).To(gomega.Equal(defaultCD.VfId))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(defaultCD.SandboxId))
				pcd, err = UnmarshalPodDPUConnDetails(annot, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal(secondCD.PfId))
				gomega.Expect(pcd.VfId).To(gomega.Equal(secondCD.VfId))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(secondCD.SandboxId))
			})

			It("Get correct Pod annotation for second network", func() {
				annot, err = MarshalPodDPUConnDetails(annot, &defaultCD, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				annot, err = MarshalPodDPUConnDetails(annot, &secondCD, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnDetails(annot, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.PfId).To(gomega.Equal(secondCD.PfId))
				gomega.Expect(pcd.VfId).To(gomega.Equal(secondCD.VfId))
				gomega.Expect(pcd.SandboxId).To(gomega.Equal(secondCD.SandboxId))
			})

			It("Fails to populate on missing annotations", func() {
				annot, err = MarshalPodDPUConnDetails(annot, &defaultCD, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = UnmarshalPodDPUConnDetails(annot, "second")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})

	Describe("DPUConnectionStatus", func() {
		var defaultCS, secondCS DPUConnectionStatus
		var annot map[string]string
		var legacyAnnot string
		var err error

		BeforeEach(func() {
			defaultCS = DPUConnectionStatus{Status: "Ready"}
			secondCS = DPUConnectionStatus{Status: "NotReady"}
			legacyAnnot = `{"Status": "Ready"}`
			annot = make(map[string]string)
		})

		Context("Default network", func() {
			It("Get correct Pod annotation for the legacy default network pod annotation", func() {
				annot[DPUConnectionStatusAnnot] = legacyAnnot
				pcs, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal(defaultCS.Status))
			})

			It("Get correct Pod annotation for default network", func() {
				annot, err = MarshalPodDPUConnStatus(annot, &defaultCS, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcs, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcs.Status).To(gomega.Equal(defaultCS.Status))
			})

			It("Fails to populate on missing annotations", func() {
				_, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})

		Context("Second network", func() {
			It("Get correct Pod annotation for Second network with legacy default network annoation", func() {
				annot[DPUConnectionStatusAnnot] = legacyAnnot
				annot, err = MarshalPodDPUConnStatus(annot, &secondCS, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnStatus(annot, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.Status).To(gomega.Equal(defaultCS.Status))
				pcd, err = UnmarshalPodDPUConnStatus(annot, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.Status).To(gomega.Equal(secondCS.Status))
			})

			It("Get correct Pod annotation for second network", func() {
				annot, err = MarshalPodDPUConnStatus(annot, &defaultCS, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				annot, err = MarshalPodDPUConnStatus(annot, &secondCS, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				pcd, err := UnmarshalPodDPUConnStatus(annot, "second")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(pcd.Status).To(gomega.Equal(secondCS.Status))
			})

			It("Fails to populate on missing annotations", func() {
				annot, err = MarshalPodDPUConnStatus(annot, &defaultCS, "default")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = UnmarshalPodDPUConnStatus(annot, "second")
				gomega.Expect(err).To(gomega.HaveOccurred())
			})
		})
	})
})
