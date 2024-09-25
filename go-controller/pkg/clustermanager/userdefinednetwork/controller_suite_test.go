package userdefinednetwork

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestUserDefinedNetworkController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "User Defined Network Controller Suite")
}
