package logical_router_policy

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPortGroup(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LRP Suite")
}
