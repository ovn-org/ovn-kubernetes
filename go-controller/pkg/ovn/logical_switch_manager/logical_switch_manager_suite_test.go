package logicalswitchmanager

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestLogicalSwitchManager(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Logical Switch Manager Operations Suite")
}
