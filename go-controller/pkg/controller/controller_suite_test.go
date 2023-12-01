package controller

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestLevelDrivenController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Level Driven Controller Suite")
}
