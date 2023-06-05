//go:build conformance_tests
// +build conformance_tests

package conformance

import (
	"flag"
	"fmt"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"k8s.io/kubernetes/test/e2e/framework"
	e2econfig "k8s.io/kubernetes/test/e2e/framework/config"
	netpolv1alpha1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/tests"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

const (
	showDebug                  = true
	shouldCleanup              = true
	enableAllSupportedFeatures = true
	NetworkPolicyAPIRepoURL    = "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.0"
)

var conformanceTestsBaseManifests = fmt.Sprintf("%s/conformance/base/manifests.yaml", NetworkPolicyAPIRepoURL)

func TestNetworkPolicyV2Conformance(t *testing.T) {
	t.Log("Configuring environment for network policy V2 API conformance tests")
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatalf("Error loading Kubernetes config: %v", err)
	}
	client, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("Error initializing Kubernetes client: %v", err)
	}
	err = netpolv1alpha1.AddToScheme(client.Scheme())
	if err != nil {
		t.Fatalf("Error initializing API scheme: %v", err)
	}

	// Register test flags, then parse flags.
	handleFlags()

	t.Log("Starting the network policy V2 conformance test suite")
	cSuite := suite.New(suite.Options{
		Client:                     client,
		Debug:                      showDebug,
		CleanupBaseResources:       shouldCleanup,
		EnableAllSupportedFeatures: enableAllSupportedFeatures,
		BaseManifests:              conformanceTestsBaseManifests,
	})
	cSuite.Setup(t)
	cSuite.Run(t, tests.ConformanceTests)
}

// handleFlags sets up all flags and parses the command line.
func handleFlags() {
	e2econfig.CopyFlags(e2econfig.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	flag.Parse()
}
