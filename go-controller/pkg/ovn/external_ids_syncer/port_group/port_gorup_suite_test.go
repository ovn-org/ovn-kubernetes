package port_group

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPortGroup(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PortGroup Suite")
}
