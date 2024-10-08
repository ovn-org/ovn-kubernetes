package zoneinterconnect

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestZoneInterconnect(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Zone interconnect Operations Suite")
}
