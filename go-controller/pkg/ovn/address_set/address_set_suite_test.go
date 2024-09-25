package addressset

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAddressSet(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Address Set Operations Suite")
}
