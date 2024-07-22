package topology

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestTopologyFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Topology Factory Suite")
}
