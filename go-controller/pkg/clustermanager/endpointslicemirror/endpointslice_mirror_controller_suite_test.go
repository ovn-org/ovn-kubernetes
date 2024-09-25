package endpointslicemirror

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEndpointSliceMirrorController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EndpointSliceMirror Controller Suite")
}
