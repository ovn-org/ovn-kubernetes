package conformance

import (
	"fmt"
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	netpolv1alpha1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	confv1a1 "sigs.k8s.io/network-policy-api/conformance/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/tests"
	netpolv1config "sigs.k8s.io/network-policy-api/conformance/utils/config"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

const (
	showDebug               = true
	shouldCleanup           = true
	NetworkPolicyAPIRepoURL = "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5"
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
	kubeConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("error building Kube config for client-go: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("error when creating Kubernetes ClientSet: %v", err)
	}
	err = netpolv1alpha1.AddToScheme(client.Scheme())
	if err != nil {
		t.Fatalf("Error initializing API scheme: %v", err)
	}

	t.Log("Starting the network policy V2 conformance test suite")
	// Depending on the tests some of them take longer and end up timing
	// out on context, let's bump the GetTimeout to 300 seconds here.
	profiles := sets.Set[suite.ConformanceProfileName]{}
	profiles.Insert(suite.ConformanceProfileName(suite.SupportAdminNetworkPolicy))
	profiles.Insert(suite.ConformanceProfileName(suite.SupportBaselineAdminNetworkPolicy))
	cSuite, err := suite.NewConformanceProfileTestSuite(
		suite.ConformanceProfileOptions{
			Options: suite.Options{
				Client:               client,
				ClientSet:            clientset,
				KubeConfig:           *cfg,
				Debug:                showDebug,
				CleanupBaseResources: shouldCleanup,
				SupportedFeatures: sets.New(
					suite.SupportAdminNetworkPolicyEgressNodePeers,
					suite.SupportBaselineAdminNetworkPolicyEgressNodePeers,
					suite.SupportAdminNetworkPolicyEgressInlineCIDRPeers,
					suite.SupportBaselineAdminNetworkPolicyEgressInlineCIDRPeers,
					suite.SupportAdminNetworkPolicyNamedPorts,
					suite.SupportBaselineAdminNetworkPolicyNamedPorts,
				),
				BaseManifests: conformanceTestsBaseManifests,
				TimeoutConfig: netpolv1config.TimeoutConfig{GetTimeout: 300 * time.Second},
			},
			Implementation: confv1a1.Implementation{
				Organization:          "ovn-org",
				Project:               "ovn-kubernetes",
				URL:                   "https://github.com/ovn-org/ovn-kubernetes",
				Version:               "v1.0.0",
				Contact:               []string{"@tssurya"},
				AdditionalInformation: "https://github.com/ovn-org/ovn-kubernetes/blob/master/test/conformance/network_policy_v2_test.go",
			},
			ConformanceProfiles: profiles,
		})
	if err != nil {
		t.Fatalf("error creating conformance test suite: %v", err)
	}
	cSuite.Setup(t)
	cSuite.Run(t, tests.ConformanceTests)
	const reportFileName = "ovn-kubernetes-anp-test-report.yaml"
	t.Logf("saving the network policy conformance test report to file: %v", reportFileName)
	report, err := cSuite.Report()
	if err != nil {
		t.Fatalf("error generating conformance profile report: %v", err)
	}
	t.Logf("Printing report...%v", report)
	rawReport, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("error marshalling conformance profile report: %v", err)
	}
	err = os.WriteFile("../../"+reportFileName, rawReport, 0o600)
	if err != nil {
		t.Fatalf("error writing conformance profile report: %v", err)
	}
}
