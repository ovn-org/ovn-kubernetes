// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

const ProviderName = "kind"

type kind struct {
	*infraprovider.ComposedProvider
	api.NodeInfrastructure
	engine  *container.Engine
	runtime containerRuntime
}

type kindNodeAccess struct {
	engine *container.Engine
}

type kindNodeInfrastructure struct {
	engine *container.Engine
}

func New() api.Provider {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	ce := getContainerRuntime()
	cmdRunner := runner.NewDirectRunner()
	engine := container.NewEngine(ce.String(), cmdRunner)
	nodeAccess := &kindNodeAccess{engine: engine}
	kind := &kind{
		ComposedProvider:   infraprovider.NewComposedProvider(ProviderName, "kind", nodeAccess, engine),
		NodeInfrastructure: &kindNodeInfrastructure{engine: engine},
		engine:             engine,
		runtime:            ce,
	}
	return kind
}

func (k *kindNodeAccess) GetK8NodeNetworkInterface(nodeName string, network api.Network) (api.NetworkInterface, error) {
	return k.engine.GetNetworkInterface(nodeName, network.Name())
}

func (k *kindNodeAccess) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	return k.engine.ExecContainerCommand(nodeName, cmd)
}

func (k *kind) PreloadImages(imgs []deploymentconfigapi.ImageConfig) {
	clusterName := kindClusterName()
	if clusterName == "" {
		framework.Logf("Warning: could not determine KIND cluster name, skipping image preload")
		return
	}
	pullBackoff := wait.Backoff{Duration: 5 * time.Second, Factor: 2, Steps: 5}
	for _, img := range imgs {
		framework.Logf("Preloading image %s into KIND cluster %s", img.PullSpec, clusterName)
		var out []byte
		err := wait.ExponentialBackoff(pullBackoff, func() (bool, error) {
			var pullErr error
			out, pullErr = exec.Command(k.runtime.String(), "pull", img.PullSpec).CombinedOutput()
			if pullErr != nil {
				framework.Logf("Retrying pull for image %s: %v (%s)", img.PullSpec, pullErr, out)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			framework.Logf("Warning: failed to pull image %s after retries: %v (%s)", img.PullSpec, err, out)
			continue
		}
		if k.runtime == podman {
			os.Remove("/tmp/image.tar")
			out, err = exec.Command(k.runtime.String(), "save", "-o", "/tmp/image.tar", img.PullSpec).CombinedOutput()
			if err != nil {
				framework.Logf("Warning: failed to save image %s: %v (%s)", img.PullSpec, err, out)
				continue
			}
			out, err = exec.Command("kind", "load", "image-archive", "/tmp/image.tar", "--name", clusterName).CombinedOutput()
		} else {
			out, err = exec.Command("kind", "load", "docker-image", img.PullSpec, "--name", clusterName).CombinedOutput()
		}
		if err != nil {
			framework.Logf("Warning: failed to load image %s into KIND cluster %s: %v (%s)", img.PullSpec, clusterName, err, out)
			continue
		}
		framework.Logf("Preloaded image %s into KIND cluster %s", img.PullSpec, clusterName)
	}
}

func kindClusterName() string {
	currentCtx, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		return ""
	}
	ctx := strings.TrimSpace(string(currentCtx))
	// KIND contexts are named "kind-<cluster-name>"
	if strings.HasPrefix(ctx, "kind-") {
		return strings.TrimPrefix(ctx, "kind-")
	}
	return ""
}

func (k *kindNodeInfrastructure) ShutdownNode(nodeName string) error {
	return k.engine.StopContainer(nodeName)
}

func (k *kindNodeInfrastructure) StartNode(nodeName string) error {
	return k.engine.StartContainer(nodeName)
}

func (k *kind) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	engine := k.engine.WithTestContext(context)
	ck := &contextKind{
		TestContext:                      context,
		ExternalContainerContextProvider: engine,
		engine:                           engine,
	}
	return ck
}

type contextKind struct {
	*testcontext.TestContext
	api.ExternalContainerContextProvider
	engine *container.Engine
}

func (c *contextKind) SetupUnderlay(f *framework.Framework, underlay api.Underlay) error {
	if underlay.LogicalNetworkName == "" {
		return fmt.Errorf("underlay logical network name must be set")
	}

	if underlay.PhysicalNetworkName == "" {
		underlay.PhysicalNetworkName = "underlay"
	}

	if underlay.BridgeName == "" {
		underlay.BridgeName = secondaryBridge
	}

	c.AddCleanUpFn(func() error {
		// Find the OVS pods again to cover cases that restart the PODs
		ovsPods, err := findOVSPods(f)
		if err != nil {
			return fmt.Errorf("failed finding OVS pods during kind underlay tear down: %w", err)
		}
		for _, ovsPod := range ovsPods {
			if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
				if err := removeOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
					return fmt.Errorf("failed to remove OVS bridge %s for pod %s/%s during cleanup: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
			if err := configureBridgeMappings(
				ovsPod.Namespace,
				ovsPod.Name,
				defaultNetworkBridgeMapping(),
			); err != nil {
				return fmt.Errorf("failed to restore default bridge mappings for pod %s/%s during cleanup: %w", ovsPod.Namespace, ovsPod.Name, err)
			}
		}
		return nil
	})

	ovsPods, err := findOVSPods(f)
	if err != nil {
		return fmt.Errorf("failed finding OVS pods during kind underlay setup: %w", err)
	}
	for _, ovsPod := range ovsPods {
		if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
			underlayInterface, err := c.engine.GetNetworkInterface(ovsPod.Spec.NodeName, underlay.PhysicalNetworkName)
			if err != nil {
				return fmt.Errorf("failed to get underlay interface for network %s on node %s: %w", underlay.PhysicalNetworkName, ovsPod.Spec.NodeName, err)
			}
			if err := ensureOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
				return fmt.Errorf("failed to add OVS bridge %s for pod %s/%s: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}

			if err := ovsAttachPortToBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName); err != nil {
				return fmt.Errorf("failed to attach port %s to bridge %s for pod %s/%s: %w", underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}
			if underlay.VlanID > 0 {
				if err := ovsEnableVLANAccessPort(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName, underlay.VlanID); err != nil {
					return fmt.Errorf("failed to enable VLAN %d on port %s for bridge %s for pod %s/%s: %w", underlay.VlanID, underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
		}
		if err := configureBridgeMappings(
			ovsPod.Namespace,
			ovsPod.Name,
			defaultNetworkBridgeMapping(),
			bridgeMapping(underlay.LogicalNetworkName, underlay.BridgeName),
		); err != nil {
			return fmt.Errorf("failed to configure bridge mappings for pod %s/%s for logical network %s to bridge %s: %w", ovsPod.Namespace, ovsPod.Name, underlay.LogicalNetworkName, underlay.BridgeName, err)
		}
	}

	return nil
}

func findOVSPods(f *framework.Framework) ([]corev1.Pod, error) {
	const ovsKubeNodeLabel = "app=ovnkube-node"
	ovsPodList, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: ovsKubeNodeLabel},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list OVS pods with label %q at namespace %q: %w", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace(), err)
	}

	if len(ovsPodList.Items) == 0 {
		return nil, fmt.Errorf("no pods with label %q in namespace %q", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace())
	}
	return ovsPodList.Items, nil
}
