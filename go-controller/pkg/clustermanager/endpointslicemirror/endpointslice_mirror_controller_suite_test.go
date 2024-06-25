package endpointslicemirror

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestEndpointSliceMirrorController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EndpointSliceMirror Controller Suite")
}
