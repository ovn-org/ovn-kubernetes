package controller

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLevelDrivenController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Level Driven Controller Suite")
}
