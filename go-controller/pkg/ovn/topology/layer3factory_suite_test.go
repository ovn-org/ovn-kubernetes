package topology

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTopologyFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Topology Factory Suite")
}
