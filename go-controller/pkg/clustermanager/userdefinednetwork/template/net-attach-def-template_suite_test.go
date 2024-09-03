package template

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNetworkAttachmentDefinitionTemplate(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NetworkAttachmentDefintion Template Suite")
}
