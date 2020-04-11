package testing

import (
	"github.com/onsi/gomega/format"
	kruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func init() {
	// Gomega's default string diff behavior makes it impossible to figure
	// out what fake command is failing, so turn it off
	format.TruncatedDiff = false
	// SharedInformers and the Kubernetes cache have internal panic
	// handling that interferes with Gingko. Disable it.
	kruntime.ReallyCrash = false
}
