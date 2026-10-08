// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

const ProviderName = "kube"

const (
	ovnKubeNodeLabel        = "app in (ovnkube-node,ovnkube-node-dpu,ovnkube-node-dpu-host)"
	containerHostEnvVar     = "OVN_TEST_CONTAINER_HOST"
	primaryNetworkEnvVar    = "OVN_TEST_PRIMARY_NETWORK"
	containerHostUserEnvVar = "OVN_TEST_CONTAINER_HOST_USER"
	containerHostPortEnvVar = "OVN_TEST_CONTAINER_HOST_PORT"
	containerHostKeyEnvVar  = "OVN_TEST_CONTAINER_HOST_KEY"
	containerRuntimeEnvVar  = "CONTAINER_RUNTIME"
)

type kube struct {
	*infraprovider.ComposedProvider
	nodeAccess *kubeNodeAccess
	engine     *container.Engine
}

type kubeExternalWorkloads struct {
	engine *container.Engine
}

type kubeNodeAccess struct {
	primaryNetwork string
	nodeShellsMu   sync.Mutex
	nodeShells     map[string]*corev1.Pod
	nodeShellCall  singleflight.Group
}

func New() api.Provider {
	nodeAccess := &kubeNodeAccess{
		primaryNetwork: os.Getenv(primaryNetworkEnvVar),
		nodeShells:     map[string]*corev1.Pod{},
	}
	engine := newContainerEngine()
	external := &kubeExternalWorkloads{engine: engine}
	return &kube{
		ComposedProvider: infraprovider.NewComposedProvider(ProviderName, nodeAccess.primaryNetwork, nodeAccess, external),
		nodeAccess:       nodeAccess,
		engine:           engine,
	}
}

func newContainerEngine() *container.Engine {
	host := os.Getenv(containerHostEnvVar)
	if host == "" {
		return nil
	}
	if os.Getenv(primaryNetworkEnvVar) == "" {
		klog.Fatalf("%s is set, so %s must also be set", containerHostEnvVar, primaryNetworkEnvVar)
	}
	sshRunner, err := runner.NewSSHRunner(host,
		cmp.Or(os.Getenv(containerHostUserEnvVar), "root"),
		cmp.Or(os.Getenv(containerHostPortEnvVar), "22"),
		os.Getenv(containerHostKeyEnvVar))
	if err != nil {
		klog.Fatalf("cannot use container host %q: %v", host, err)
	}
	return container.NewEngine(cmp.Or(os.Getenv(containerRuntimeEnvVar), "docker"), sshRunner)
}

func (w *kubeExternalWorkloads) containerEngine() *container.Engine {
	// An existing cluster does not imply ownership of an external container host.
	if w.engine == nil {
		ginkgo.Skip("set OVN_TEST_CONTAINER_HOST to run this spec", 2)
	}
	return w.engine
}

func skip(op, why string) error {
	err := fmt.Errorf("%s provider does not serve %s: %s", ProviderName, op, why)
	ginkgo.Skip(err.Error(), 2)
	return err
}

func (k *kube) PreloadImages(_ []deploymentconfigapi.ImageConfig) {
	ginkgo.DeferCleanup(k.nodeAccess.deleteNodeShells)
}

func (k *kubeNodeAccess) deleteNodeShells() error {
	client, err := framework.LoadClientset()
	if err != nil {
		return err
	}
	k.nodeShellsMu.Lock()
	defer k.nodeShellsMu.Unlock()
	var errs []error
	for _, shell := range k.nodeShells {
		ctx, cancel := apiCallContext()
		err := client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (k *kubeNodeAccess) GetK8NodeNetworkInterface(nodeName string, network api.Network) (api.NetworkInterface, error) {
	if network.Name() != k.primaryNetwork {
		return api.NetworkInterface{}, skip("GetK8NodeNetworkInterface", "the provider does not attach networks to Nodes")
	}
	client, err := framework.LoadClientset()
	if err != nil {
		return api.NetworkInterface{}, err
	}
	ctx, cancel := apiCallContext()
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	cancel()
	if err != nil {
		return api.NetworkInterface{}, err
	}
	primary, err := util.ParseNodePrimaryIfAddr(node)
	if util.IsAnnotationNotSetError(err) {
		return api.NetworkInterface{}, skip("GetK8NodeNetworkInterface", "OVN-Kubernetes has not published the Node uplink address")
	}
	if err != nil {
		return api.NetworkInterface{}, err
	}
	v4Subnet, v6Subnet, err := network.IPv4IPv6Subnets()
	if err != nil {
		return api.NetworkInterface{}, err
	}
	inf := api.NetworkInterface{}
	if subnetHolds(v4Subnet, primary.V4.IP) {
		inf.IPv4, inf.IPv4Prefix = primary.V4.IP.String(), prefixOf(primary.V4.Net)
	}
	if subnetHolds(v6Subnet, primary.V6.IP) {
		inf.IPv6, inf.IPv6Prefix = primary.V6.IP.String(), prefixOf(primary.V6.Net)
	}
	if inf.IPv4 == "" && inf.IPv6 == "" {
		return api.NetworkInterface{}, fmt.Errorf("node %s is not on network %s", nodeName, network.Name())
	}
	inf.InfName, inf.MAC, err = k.nodeLinkHolding(nodeName, cmp.Or(inf.IPv4, inf.IPv6))
	return inf, err
}

func prefixOf(subnet *net.IPNet) string {
	ones, _ := subnet.Mask.Size()
	return strconv.Itoa(ones)
}

func subnetHolds(subnet string, ip net.IP) bool {
	_, network, err := utilnet.ParseCIDRSloppy(subnet)
	return err == nil && network.Contains(ip)
}

func (k *kubeNodeAccess) nodeLinkHolding(nodeName, address string) (string, string, error) {
	out, err := k.ExecK8NodeCommand(nodeName, []string{"ip", "-j", "addr", "show"})
	if err != nil {
		return "", "", err
	}
	var links []struct {
		Name     string `json:"ifname"`
		MAC      string `json:"address"`
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return "", "", fmt.Errorf("read interfaces on node %s: %w", nodeName, err)
	}
	want := net.ParseIP(address)
	var names []string
	var name, mac string
	for _, link := range links {
		for _, address := range link.AddrInfo {
			if net.ParseIP(address.Local).Equal(want) {
				names = append(names, link.Name)
				name, mac = link.Name, link.MAC
			}
		}
	}
	if len(names) != 1 {
		return "", "", fmt.Errorf("found node address %s on interfaces %v", want, names)
	}
	return name, mac, nil
}

func (k *kubeNodeAccess) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	if disruptsKubelet(cmd) {
		return "", skip("ExecK8NodeCommand", "node shell Pods depend on kubelet and cannot safely stop or restart it")
	}
	shell, err := k.nodeShell(nodeName)
	if err != nil {
		return "", err
	}
	return e2ekubectl.RunKubectl(shell.Namespace,
		append([]string{"exec", shell.Name, "--", "chroot", "/host"}, cmd...)...)
}

func disruptsKubelet(cmd []string) bool {
	return len(cmd) >= 3 && cmd[0] == "systemctl" &&
		((cmd[1] == "stop" && cmd[2] == "kubelet.service") || (cmd[1] == "restart" && cmd[2] == "kubelet"))
}

func (k *kubeNodeAccess) nodeShell(nodeName string) (*corev1.Pod, error) {
	client, err := framework.LoadClientset()
	if err != nil {
		return nil, err
	}
	return k.nodeShellWithClient(client, nodeName, func() (*corev1.Pod, error) {
		namespace := deploymentconfig.Get().OVNKubernetesNamespace()
		image, err := ovnKubeNodeImage(client, namespace, nodeName)
		if err != nil {
			return nil, err
		}
		return k.createNodeShell(client, namespace, nodeName, image)
	})
}

func (k *kubeNodeAccess) nodeShellWithClient(client clientset.Interface, nodeName string, create func() (*corev1.Pod, error)) (*corev1.Pod, error) {
	value, err, _ := k.nodeShellCall.Do(nodeName, func() (any, error) {
		k.nodeShellsMu.Lock()
		shell := k.nodeShells[nodeName]
		k.nodeShellsMu.Unlock()
		if shell != nil {
			ctx, cancel := apiCallContext()
			current, err := client.CoreV1().Pods(shell.Namespace).Get(ctx, shell.Name, metav1.GetOptions{})
			cancel()
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
			if err == nil && current.DeletionTimestamp == nil && current.Status.Phase == corev1.PodRunning {
				return current, nil
			}
			if err == nil && current.DeletionTimestamp == nil {
				ctx, cancel = apiCallContext()
				err = client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
				cancel()
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
			}
			k.nodeShellsMu.Lock()
			delete(k.nodeShells, nodeName)
			k.nodeShellsMu.Unlock()
		}
		shell, err := create()
		if shell != nil {
			k.nodeShellsMu.Lock()
			k.nodeShells[nodeName] = shell
			k.nodeShellsMu.Unlock()
		}
		return shell, err
	})
	if err != nil {
		return nil, err
	}
	return value.(*corev1.Pod), nil
}

func (k *kubeNodeAccess) createNodeShell(client clientset.Interface, namespace, nodeName, image string) (*corev1.Pod, error) {
	ctx, cancel := apiCallContext()
	shell, err := client.CoreV1().Pods(namespace).Create(ctx,
		nodeShellPod(nodeName, image), metav1.CreateOptions{})
	cancel()
	if err != nil {
		return nil, err
	}
	if err := e2epod.WaitForPodRunningInNamespace(context.Background(), client, shell); err != nil {
		ctx, cancel := apiCallContext()
		deleteErr := client.CoreV1().Pods(namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		cancel()
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return shell, errors.Join(err, deleteErr)
		}
		return nil, err
	}
	return shell, nil
}

func ovnKubeNodeImage(client clientset.Interface, namespace, nodeName string) (string, error) {
	ctx, cancel := apiCallContext()
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: ovnKubeNodeLabel, FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod on node %s to take a shell image from", nodeName)
	}
	return pods.Items[0].Spec.Containers[0].Image, nil
}

func apiCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), framework.SingleCallTimeout)
}

func nodeShellPod(nodeName, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-e2e-node-shell-"},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostNetwork:   true,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "shell",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}},
		},
	}
}

func (w *kubeExternalWorkloads) ListNetworks() ([]string, error) {
	return w.containerEngine().ListNetworks()
}

func (w *kubeExternalWorkloads) GetNetwork(name string) (api.Network, error) {
	return w.containerEngine().GetNetwork(name)
}

func (w *kubeExternalWorkloads) GetExternalContainerNetworkInterface(external api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return w.containerEngine().GetExternalContainerNetworkInterface(external, network)
}

func (w *kubeExternalWorkloads) ExecExternalContainerCommand(external api.ExternalContainer, cmd []string) (string, error) {
	return w.containerEngine().ExecExternalContainerCommand(external, cmd)
}

func (w *kubeExternalWorkloads) GetExternalContainerLogs(external api.ExternalContainer) (string, error) {
	return w.containerEngine().GetExternalContainerLogs(external)
}

func (w *kubeExternalWorkloads) GetExternalContainerPort() uint16 {
	return w.containerEngine().GetExternalContainerPort()
}

func (w *kubeExternalWorkloads) ExternalContainerPrimaryInterfaceName() string {
	return w.containerEngine().ExternalContainerPrimaryInterfaceName()
}

func (k *kube) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	var engine *container.Engine
	if k.engine != nil {
		engine = k.engine.WithTestContext(context)
	}
	return &contextKube{
		TestContext: context,
		ExternalContainerContextProvider: &kubeExternalWorkloadContext{
			kubeExternalWorkloads: &kubeExternalWorkloads{engine: engine},
		},
	}
}

type contextKube struct {
	*testcontext.TestContext
	api.ExternalContainerContextProvider
}

type kubeExternalWorkloadContext struct {
	*kubeExternalWorkloads
}

func (w *kubeExternalWorkloadContext) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	return w.containerEngine().CreateNetwork(name, subnets...)
}

func (w *kubeExternalWorkloadContext) DeleteNetwork(network api.Network) error {
	return w.containerEngine().DeleteNetwork(network)
}

func (w *kubeExternalWorkloadContext) CreateExternalContainer(external api.ExternalContainer) (api.ExternalContainer, error) {
	return w.containerEngine().CreateExternalContainer(external)
}

func (w *kubeExternalWorkloadContext) DeleteExternalContainer(external api.ExternalContainer) error {
	return w.containerEngine().DeleteExternalContainer(external)
}

func (w *kubeExternalWorkloadContext) AttachNetwork(network api.Network, instance string) (api.NetworkInterface, error) {
	node, err := isNode(instance)
	if err != nil {
		return api.NetworkInterface{}, err
	}
	if node {
		return api.NetworkInterface{}, skip("AttachNetwork", "the provider does not own Node interfaces")
	}
	return w.containerEngine().AttachNetwork(network, instance)
}

func (w *kubeExternalWorkloadContext) DetachNetwork(network api.Network, instance string) error {
	node, err := isNode(instance)
	if err != nil {
		return err
	}
	if node {
		return skip("DetachNetwork", "the provider does not own Node interfaces")
	}
	return w.containerEngine().DetachNetwork(network, instance)
}

func isNode(name string) (bool, error) {
	client, err := framework.LoadClientset()
	if err != nil {
		return false, err
	}
	ctx, cancel := apiCallContext()
	defer cancel()
	_, err = client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
