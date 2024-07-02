package template

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetAttachDefTemplate", func() {
	It("should fail", func() {
		_, err := RenderNetAttachDefManifest(nil)
		Expect(err).To(HaveOccurred())
	})
})
