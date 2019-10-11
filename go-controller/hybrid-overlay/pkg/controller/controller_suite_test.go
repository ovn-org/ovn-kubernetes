package controller

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestHybridOverlayControllerSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hybrid Overlay Controller Suite")
}
