package logical_router_policy

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestPortGroup(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LRP Suite")
}
