package apbroute

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestApbroute(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Admin Based Policy External Route Controller Suite")
}
