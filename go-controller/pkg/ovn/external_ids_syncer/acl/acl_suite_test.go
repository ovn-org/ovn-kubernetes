package acl_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAcl(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Acl Suite")
}
