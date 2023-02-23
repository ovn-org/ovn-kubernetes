package ovnmanager

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestOvnManager(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ovn Manager Operations Suite")
}
