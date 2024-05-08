package dnsnameresolver

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestDNSNameResolverController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster Manager DNS Name Resolver Controller Suite")
}
