package ovn

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestClusterNode(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OVN Operations Suite")
}
