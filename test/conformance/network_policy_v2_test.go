package conformance

import (
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	netpolv1alpha1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	"sigs.k8s.io/network-policy-api/conformance/tests"
	netpolv1config "sigs.k8s.io/network-policy-api/conformance/utils/config"
	"sigs.k8s.io/network-policy-api/conformance/utils/suite"
)

const (
	showDebug                  = true
	shouldCleanup              = true
	enableAllSupportedFeatures = true
	NetworkPolicyAPIRepoURL    = "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.1"
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
	cSuite := suite.New(suite.Options{
		Client:                     client,
		ClientSet:                  clientset,
		KubeConfig:                 *cfg,
		Debug:                      showDebug,
		CleanupBaseResources:       shouldCleanup,
		EnableAllSupportedFeatures: enableAllSupportedFeatures,
		BaseManifests:              conformanceTestsBaseManifests,
		TimeoutConfig:              netpolv1config.TimeoutConfig{GetTimeout: 300 * time.Second},
	})
	cSuite.Setup(t)
	cSuite.Run(t, tests.ConformanceTests)
}
