package clustermanager

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestClusterManager(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster Manager Operations Suite")
}
