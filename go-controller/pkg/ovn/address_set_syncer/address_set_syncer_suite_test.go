package address_set_syncer

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestAddressSetSyncer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Address Set Syncer Operations Suite")
}
