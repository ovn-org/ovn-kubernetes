package testing

import (
	"os"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega/format"

	kruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// OnSupportedPlatformsIt is a wrapper around ginkgo.It to determine if running
// the test is applicable for the current test environment. This is used to skip
// tests that are unable to execute in certain environments. Such as those without
// root or cap_net_admin privileges
func OnSupportedPlatformsIt(description string, f interface{}) {
	if os.Getenv("NOROOT") != "TRUE" {
		ginkgo.It(description, f)
	} else {
		defer ginkgo.GinkgoRecover()
		ginkgo.Skip(description)
	}
}

func init() {
	// Gomega's default string diff behavior makes it impossible to figure
	// out what fake command is failing, so turn it off
	format.TruncatedDiff = false
	// SharedInformers and the Kubernetes cache have internal panic
	// handling that interferes with Gingko. Disable it.
	kruntime.ReallyCrash = false
}
