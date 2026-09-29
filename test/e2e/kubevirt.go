// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/diagnostics"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/kubevirt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	ipamclaimsv1alpha1 "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	iputils "github.com/containernetworking/plugins/pkg/ip"

	kubevirtv1 "kubevirt.io/api/core/v1"
	kvmigrationsv1alpha1 "kubevirt.io/api/migrations/v1alpha1"
)

func newControllerRuntimeClient() (crclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := kubevirtv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := kvmigrationsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := ipamclaimsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := nadapi.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := udnv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := rav1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return crclient.New(config, crclient.Options{
		Scheme: scheme,
	})
}

// The helpers below are package-level so suites outside the kubevirt Describe
// (dhcp_ipam.go) can reuse them; the same-named closures inside the Describe
// delegate here, keeping the original call sites unchanged.

func composeVMI(namespace string, labels, annotations, nodeSelector map[string]string,
	networkSource kubevirtv1.NetworkSource, cloudInitVolumeSource kubevirtv1.VolumeSource, image string) *kubevirtv1.VirtualMachineInstance {
	return &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: "worker-",
			Annotations:  annotations,
			Labels:       labels,
		},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			NodeSelector: nodeSelector,
			Domain: kubevirtv1.DomainSpec{
				Resources: kubevirtv1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("1024Mi"),
					},
				},
				Devices: kubevirtv1.Devices{
					Disks: []kubevirtv1.Disk{
						{
							DiskDevice: kubevirtv1.DiskDevice{
								Disk: &kubevirtv1.DiskTarget{
									Bus: kubevirtv1.DiskBusVirtio,
								},
							},
							Name: "containerdisk",
						},
						{
							DiskDevice: kubevirtv1.DiskDevice{
								Disk: &kubevirtv1.DiskTarget{
									Bus: kubevirtv1.DiskBusVirtio,
								},
							},
							Name: "cloudinitdisk",
						},
					},
					Interfaces: []kubevirtv1.Interface{
						{
							Name: "net1",
							InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
								Bridge: &kubevirtv1.InterfaceBridge{},
							},
						},
					},
					Rng: &kubevirtv1.Rng{},
				},
			},
			Networks: []kubevirtv1.Network{
				{
					Name:          "net1",
					NetworkSource: networkSource,
				},
			},
			TerminationGracePeriodSeconds: ptr.To(int64(5)),
			Volumes: []kubevirtv1.Volume{
				{
					Name: "containerdisk",
					VolumeSource: kubevirtv1.VolumeSource{
						ContainerDisk: &kubevirtv1.ContainerDiskSource{
							Image: image,
						},
					},
				},
				{
					Name:         "cloudinitdisk",
					VolumeSource: cloudInitVolumeSource,
				},
			},
		},
	}
}

func composeVM(namespace string, vmi *kubevirtv1.VirtualMachineInstance) *kubevirtv1.VirtualMachine {
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: vmi.GenerateName,
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: ptr.To(kubevirtv1.RunStrategyAlways),
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: vmi.Annotations,
					Labels:      vmi.Labels,
				},
				Spec: vmi.Spec,
			},
		},
	}
}

func composeFedoraWithTestToolingVMI(namespace string, labels, annotations, nodeSelector map[string]string,
	networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachineInstance {
	cloudInitVolumeSource := kubevirtv1.VolumeSource{
		CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{
			UserData:    userData,
			NetworkData: networkData,
		},
	}
	return composeVMI(namespace, labels, annotations, nodeSelector, networkSource, cloudInitVolumeSource, images.FedoraKubevirtContainerDisk())
}

func composeFedoraWithTestToolingVM(namespace string, labels, annotations, nodeSelector map[string]string,
	networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachine {
	return composeVM(namespace, composeFedoraWithTestToolingVMI(namespace, labels, annotations, nodeSelector, networkSource, userData, networkData))
}

func createVirtualMachineWithClient(cli crclient.Client, vm *kubevirtv1.VirtualMachine) {
	GinkgoHelper()
	By(fmt.Sprintf("Create virtual machine %s", vm.Name))
	vmCreationRetries := 0
	Eventually(func() error {
		if vmCreationRetries > 0 {
			// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
			// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3902#issuecomment-1750257559
			By(fmt.Sprintf("Retrying vm %s creation", vm.Name))
		}
		err := cli.Create(context.Background(), vm)
		vmCreationRetries++
		return err
	}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
}

func waitForVMIReadinessWithClient(
	cli crclient.Client,
	vmi *kubevirtv1.VirtualMachineInstance,
	conditionType kubevirtv1.VirtualMachineInstanceConditionType,
	conditionStatus corev1.ConditionStatus,
) {
	GinkgoHelper()
	By(fmt.Sprintf("Waiting for condition type=%q status=%q at virtual machine %s", conditionType, conditionStatus, vmi.Name))
	Eventually(func() []kubevirtv1.VirtualMachineInstanceCondition {
		err := cli.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)
		Expect(err).To(SatisfyAny(
			WithTransform(apierrors.IsNotFound, BeTrue()),
			Succeed(),
		))
		return vmi.Status.Conditions
	}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(
		ContainElement(SatisfyAll(
			HaveField("Type", conditionType),
			HaveField("Status", conditionStatus),
		)),
	)
}

func createCUDNWithClients(ctx context.Context, cli crclient.Client, dynClient dynamic.Interface, cudn *udnv1.ClusterUserDefinedNetwork) {
	GinkgoHelper()
	By("Creating ClusterUserDefinedNetwork")
	Expect(cli.Create(context.Background(), cudn)).To(Succeed())
	DeferCleanup(func() {
		if e2eframework.TestContext.DeleteNamespace && (e2eframework.TestContext.DeleteNamespaceOnFailure || !CurrentSpecReport().Failed()) {
			cli.Delete(context.Background(), cudn)
		}
	})
	Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, dynClient, cudn.Name), 5*time.Second, time.Second).Should(Succeed())
}

func composeAgnhostPod(name, namespace, nodeName string, args ...string) *corev1.Pod {
	agnHostPod := e2epod.NewAgnhostPod(namespace, name, nil, nil, nil, args...)
	agnHostPod.Spec.NodeName = nodeName
	return agnHostPod
}

func removeImagesInNode(node, imageURL string) error {
	By("Removing unused images in node " + node)
	output, err := infraprovider.Get().ExecK8NodeCommand(node, []string{
		"crictl", "images", "-o", "json",
	})
	if err != nil {
		return err
	}

	// Remove tag if exists.
	taglessImageURL := strings.Split(imageURL, ":")[0]
	imageID, err := images.ImageIDByImageURL(taglessImageURL, output)
	if err != nil {
		return err
	}
	if imageID != "" {
		_, err = infraprovider.Get().ExecK8NodeCommand(node, []string{
			"crictl", "rmi", imageID,
		})
		if err != nil {
			return err
		}
		_, err = infraprovider.Get().ExecK8NodeCommand(node, []string{
			"crictl", "rmi", "--prune",
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func removeImagesFromNodes(cs kubernetes.Interface, imageURL string) error {
	nodesList, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for nodeIdx := range nodesList.Items {
		err = removeImagesInNode(nodesList.Items[nodeIdx].Name, imageURL)
		if err != nil {
			return err
		}
	}
	return nil
}

var _ = Describe("Kubevirt Virtual Machines", feature.VirtualMachineSupport, func() {
	var (
		fr                  = wrappedTestFramework("kv-live-migration")
		d                   = diagnostics.New(fr)
		crClient            crclient.Client
		virtClient          *kubevirt.Client
		namespace           string
		iperf3DefaultPort   = int32(5201)
		wg                  sync.WaitGroup
		selectedNodes       = []corev1.Node{}
		httpServerTestPods  = []*corev1.Pod{}
		iperfServerTestPods = []*corev1.Pod{}
		clientSet           kubernetes.Interface
		nadClient           nadclient.K8sCniCncfIoV1Interface
		providerCtx         infraapi.Context

		isDualStack = func() bool {
			GinkgoHelper()
			nodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeList.Items).NotTo(BeEmpty())
			hasIPv4Address, hasIPv6Address := false, false
			for _, addr := range nodeList.Items[0].Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					if utilnet.IsIPv4String(addr.Address) {
						hasIPv4Address = true
					}
					if utilnet.IsIPv6String(addr.Address) {
						hasIPv6Address = true
					}
				}
			}
			return hasIPv4Address && hasIPv6Address
		}
	)

	// disable automatic namespace creation, we need to add the required UDN label
	fr.SkipNamespaceCreation = true

	type liveMigrationTestData struct {
		mode                kubevirtv1.MigrationMode
		numberOfVMs         int
		shouldExpectFailure bool
	}

	type execFnType = func(cmd string) (string, error)

	var (
		// northSouthIperfLogFile and northSouthIperfPidFile compose the log
		// and pid file paths of the iperf3 clients started by
		// startNorthSouthIperfTraffic for a given traffic direction prefix.
		northSouthIperfLogFile = func(logPrefix, address string, port int32) string {
			return fmt.Sprintf("/tmp/%s_test_%s_%d_iperf3.log", logPrefix, address, port)
		}

		northSouthIperfPidFile = func(logPrefix, address string, port int32) string {
			return fmt.Sprintf("/tmp/%s_test_%s_%d_iperf3.pid", logPrefix, address, port)
		}

		ingressIperfLogFile = func(address string, port int32) string {
			return northSouthIperfLogFile("ingress", address, port)
		}

		ingressIperfPidFile = func(address string, port int32) string {
			return northSouthIperfPidFile("ingress", address, port)
		}

		// iperfServerLogFile composes the log file path of the iperf3 servers
		// started by iperfServerScript, the address parameter also accepts
		// shell expressions and globs.
		iperfServerLogFile = func(address string) string {
			return fmt.Sprintf("/tmp/test_%s_iperf3.log", address)
		}

		// stopIngressIperfClients kills the background iperf3 clients started
		// at the external container using the pidfile written by iperf3. The
		// pidfile is removed before sending the signal since iperf3 also
		// deletes it on termination.
		stopIngressIperfClients = func(container infraapi.ExternalContainer, addresses []string, port int32) {
			for _, address := range addresses {
				// Ignore errors, the iperf3 client may not be running anymore.
				_, _ = infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c",
					fmt.Sprintf(`[ -f %[1]s ] && { pid="$(cat %[1]s)"; rm -f %[1]s; kill "$pid" 2>/dev/null; } || true`, ingressIperfPidFile(address, port))})
			}
		}

		serviceNodePortAddresses = func(client kubernetes.Interface, svc *corev1.Service) ([]string, int32) {
			GinkgoHelper()
			worker, err := e2enode.GetRandomReadySchedulableNode(context.TODO(), client)
			Expect(err).NotTo(HaveOccurred(), "should find a ready and schedulable node")
			nodePort := svc.Spec.Ports[0].NodePort
			port := svc.Spec.Ports[0].Port

			d.TCPDumpDaemonSet([]string{"any", deploymentconfig.Get().PrimaryInterfaceName(), deploymentconfig.Get().ExternalBridgeName()}, fmt.Sprintf("port %d or port %d", port, nodePort))
			addresses := e2enode.GetAddresses(worker, corev1.NodeInternalIP)
			Expect(addresses).NotTo(BeEmpty(), "should find at least one node internal IP address to run the iperf3 clients against")
			return addresses, nodePort
		}

		composeService = func(name, vmName string, port int32) *corev1.Service {
			ipFamilyPolicy := corev1.IPFamilyPolicyPreferDualStack
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: name + vmName,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port: port,
					}},
					Selector: map[string]string{
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					Type:           corev1.ServiceTypeNodePort,
					IPFamilyPolicy: &ipFamilyPolicy,
				},
			}
		}

		by = func(vmName string, step string) string {
			fullStep := fmt.Sprintf("%s: %s", vmName, step)
			By(fullStep)
			return fullStep
		}

		checkEastWestTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, podIPsByName map[string][]string, stage string) {
			GinkgoHelper()
			Expect(podIPsByName).NotTo(BeEmpty())
			polling := 15 * time.Second
			timeout := time.Minute
			for podName, podIPs := range podIPsByName {
				for _, podIP := range podIPs {
					output := ""
					Eventually(func() error {
						var err error
						output, err = virtClient.RunCommand(vmi, fmt.Sprintf("curl http://%s", net.JoinHostPort(podIP, "8000")), polling)
						return err
					}).
						WithPolling(polling).
						WithTimeout(timeout).
						Should(Succeed(), func() string { return stage + ": " + podName + ": " + output })
				}
			}
		}

		startEastWestIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, serverPodIPsByName map[string][]string, stage string) error {
			GinkgoHelper()
			Expect(serverPodIPsByName).NotTo(BeEmpty())
			polling := 15 * time.Second
			for podName, serverPodIPs := range serverPodIPsByName {
				for _, serverPodIP := range serverPodIPs {
					iperfLogFile := fmt.Sprintf("/tmp/%s_%s_iperf3.log", podName, serverPodIP)

					By(fmt.Sprintf("remove iperf3 log for %s: %s", serverPodIP, stage))
					output, err := virtClient.RunCommand(vmi, fmt.Sprintf("rm -f %s", iperfLogFile), polling)
					if err != nil {
						return fmt.Errorf("failed removing iperf3 log file %s: %w", output, err)
					}

					By(fmt.Sprintf("check iperf3 connectivity for %s: %s", serverPodIP, stage))
					output, err = virtClient.RunCommand(vmi, fmt.Sprintf("iperf3 -t 1 -c %s", serverPodIP), polling)
					if err != nil {
						return fmt.Errorf("failed checking iperf3 connectivity %s: %w", output, err)
					}

					By(fmt.Sprintf("start iperf3 to %s: %s", serverPodIP, stage))
					output, err = virtClient.RunCommand(vmi, fmt.Sprintf("nohup iperf3 -t 0 -c %[2]s --logfile %[1]s &", iperfLogFile, serverPodIP), polling)
					if err != nil {
						return fmt.Errorf("%s: %w", output, err)
					}
				}
			}
			return nil
		}

		checkIperfTraffic = func(iperfLogFile string, execFn func(cmd string) (string, error), timeout time.Duration, stage string) {
			GinkgoHelper()
			// Check the last line eventually show traffic flowing
			Eventually(func() (string, error) {
				iperfLog, err := execFn("cat " + iperfLogFile)
				if err != nil {
					return "", err
				}
				// Fail fast
				Expect(iperfLog).NotTo(ContainSubstring("iperf3: error"), stage+": "+iperfLogFile)
				// Remove last carriage return to properly split by new line.
				iperfLog = strings.TrimSuffix(iperfLog, "\n")
				iperfLogLines := strings.Split(iperfLog, "\n")
				if len(iperfLogLines) == 0 {
					return "", nil
				}
				lastIperfLogLine := iperfLogLines[len(iperfLogLines)-1]
				return lastIperfLogLine, nil
			}).
				WithPolling(50*time.Millisecond).
				WithTimeout(timeout).
				Should(
					SatisfyAll(
						ContainSubstring(" sec "),
						Not(ContainSubstring("0.00 Bytes  0.00 bits/sec")),
					),
					stage+": failed checking iperf3 traffic at file "+iperfLogFile,
				)
		}

		checkEastWestIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, podIPsByName map[string][]string, stage string) {
			GinkgoHelper()
			for podName, podIPs := range podIPsByName {
				for _, podIP := range podIPs {
					iperfLogFile := fmt.Sprintf("/tmp/%s_%s_iperf3.log", podName, podIP)
					execFn := func(cmd string) (string, error) {
						return virtClient.RunCommand(vmi, cmd, 2*time.Second)
					}
					checkIperfTraffic(iperfLogFile, execFn, 2*time.Second, stage)
				}
			}
		}
		startNorthSouthIperfTraffic = func(execFn execFnType, addresses []string, port int32, logPrefix, stage string) error {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, address := range addresses {
				iperfLogFile := northSouthIperfLogFile(logPrefix, address, port)
				iperfPidFile := northSouthIperfPidFile(logPrefix, address, port)
				By(fmt.Sprintf("remove iperf3 log and pid files for %s: %s", address, stage))
				output, err := execFn(fmt.Sprintf("rm -f %s %s", iperfLogFile, iperfPidFile))
				if err != nil {
					return fmt.Errorf("failed removing iperf3 log and pid files %s: %w", output, err)
				}

				By(fmt.Sprintf("check iperf3 connectivity for %s: %s", address, stage))
				output, err = execFn(fmt.Sprintf("iperf3 -c %s -p %d", address, port))
				if err != nil {
					return fmt.Errorf("failed checking iperf3 connectivity %s: %w", output, err)
				}

				By(fmt.Sprintf("start from %s: %s", address, stage))
				// Redirect stdio so execFn implementations that capture output
				// do not block waiting for the background iperf3 process to
				// exit.
				output, err = execFn(fmt.Sprintf("nohup iperf3 -t 0 -c %[1]s -p %[2]d --pidfile %[3]s --logfile %[4]s >/dev/null 2>&1 &", address, port, iperfPidFile, iperfLogFile))
				if err != nil {
					return fmt.Errorf("failed at starting iperf3 in background %s: %w", output, err)
				}
			}
			return nil
		}

		startNorthSouthIngressIperfTraffic = func(container infraapi.ExternalContainer, addresses []string, port int32, stage string) error {
			GinkgoHelper()
			execFn := func(cmd string) (string, error) {
				return infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", cmd})
			}
			return startNorthSouthIperfTraffic(execFn, addresses, port, "ingress", stage)
		}

		iperfServerScript = fmt.Sprintf(`
#!/bin/bash -xe
iface=$(ip -j link show | jq -r '.[].ifname | select(. != "eth0" and . != "lo")' | head -1)
iface=${iface:-eth0}

ipv4=$(ip -j -4 addr show dev $iface | jq -r '.[0].addr_info[0].local // empty')
if [ "$ipv4" != "" ]; then
	iperf3 -s -D --bind $ipv4 --logfile %[1]s
	sleep 1
	if grep "iperf3: error" %[1]s; then
		cat %[1]s
		exit 1
	fi
fi

cnt=0
while [ "$ipv6" == "" -a $cnt -lt 10 ]; do
	ipv6=$(ip -j -6 addr show dev $iface | jq -r '.[0].addr_info[] | select(.local | startswith("fe80") | not) | .local' | head -1)
	sleep 1
	cnt=$((cnt+1))
done
if [ "$ipv6" != "" ]; then
	iperf3 -s -D --bind $ipv6 --logfile %[2]s
	sleep 1
	if grep "iperf3: error" %[2]s; then
		cat %[2]s 1>&2
		exit 1
	fi
fi
`, iperfServerLogFile("${ipv4}"), iperfServerLogFile("${ipv6}"))

		startExternalEastWestIperfTraffic = func(container infraapi.ExternalContainer, vmi *kubevirtv1.VirtualMachineInstance, macVRFContainerIPs []string, stage string) error {
			GinkgoHelper()
			Expect(macVRFContainerIPs).NotTo(BeEmpty())
			// Start iperf3 server on the external container using the same script as pods
			output, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", iperfServerScript})
			if err != nil {
				return fmt.Errorf("failed starting iperf3 server on external container: %s: %w", output, err)
			}
			// Start iperf3 client on the VM connecting to the external container's IPs.
			// --timestamps='%s ' adds Unix epoch prefix so checkExternalEastWestIperfTraffic
			// can use iperftest.LogDowntime to verify traffic resumed after migration.
			polling := 15 * time.Second
			for _, ip := range macVRFContainerIPs {
				logFile := fmt.Sprintf("/tmp/external-east-west_%s_iperf3.log", ip)
				output, err := virtClient.RunCommand(vmi, fmt.Sprintf("iperf3 -t 0 --forceflush --timestamps='%%s ' -c %[1]s --logfile %[2]s &", ip, logFile), polling)
				if err != nil {
					return fmt.Errorf("%s: %w", output, err)
				}
			}
			return nil
		}

		startNorthSouthEgressIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, port int32, stage string) error {
			GinkgoHelper()
			execFn := func(cmd string) (string, error) {
				return virtClient.RunCommand(vmi, cmd, 5*time.Second)
			}
			return startNorthSouthIperfTraffic(execFn, addresses, port, "egress", stage)
		}

		checkNorthSouthIngressIperfTraffic = func(container infraapi.ExternalContainer, addresses []string, port int32, timeout time.Duration, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				execFn := func(cmd string) (string, error) {
					return infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c", cmd})
				}
				checkIperfTraffic(ingressIperfLogFile(ip, port), execFn, timeout, stage)
				// A client killed without writing "iperf3: error" (like an
				// OOM kill) leaves a stale good looking last log line, ensure
				// it's still alive so that does not become a false positive.
				output, err := execFn(fmt.Sprintf(`kill -0 "$(cat %s)"`, ingressIperfPidFile(ip, port)))
				Expect(err).NotTo(HaveOccurred(), stage+": iperf3 client for "+ip+" should still be running: "+output)
			}
		}

		checkExternalEastWestIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, macVRFContainerIPs []string, stage string) {
			GinkgoHelper()
			Expect(macVRFContainerIPs).NotTo(BeEmpty())
			for _, ip := range macVRFContainerIPs {
				iperfLogFile := fmt.Sprintf("/tmp/external-east-west_%s_iperf3.log", ip)
				execFn := func(cmd string) (string, error) {
					return virtClient.RunCommand(vmi, cmd, 2*time.Second)
				}
				checkIperfTraffic(iperfLogFile, execFn, 2*time.Second, stage)
			}
		}

		checkNorthSouthEgressIperfTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, port int32, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				if ip == "" {
					continue
				}
				execFn := func(cmd string) (string, error) {
					return virtClient.RunCommand(vmi, cmd, 5*time.Second)
				}
				checkIperfTraffic(northSouthIperfLogFile("egress", ip, port), execFn, 2*time.Second, stage)
			}
		}

		checkNorthSouthEgressICMPTraffic = func(vmi *kubevirtv1.VirtualMachineInstance, addresses []string, stage string) {
			GinkgoHelper()
			Expect(addresses).NotTo(BeEmpty())
			for _, ip := range addresses {
				if ip == "" {
					continue
				}
				cmd := fmt.Sprintf("ping -c 3 -W 2 %s", ip)
				stdout, err := virtClient.RunCommand(vmi, cmd, 5*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(stdout).To(ContainSubstring(" 0% packet loss"))
			}
		}

		httpServerTestPodsDefaultNetworkIPs = func() map[string][]string {
			ips := map[string][]string{}
			for _, pod := range httpServerTestPods {
				for _, podIP := range pod.Status.PodIPs {
					ips[pod.Name] = append(ips[pod.Name], podIP.IP)
				}
			}
			return ips
		}

		podsMultusNetworkIPs = func(pods []*corev1.Pod, networkStatusPredicate func(nadapi.NetworkStatus) bool) map[string][]string {
			GinkgoHelper()
			ips := map[string][]string{}
			for _, pod := range pods {
				var networkStatuses []nadapi.NetworkStatus
				Eventually(func() ([]nadapi.NetworkStatus, error) {
					var err error
					networkStatuses, err = podNetworkStatus(pod, networkStatusPredicate)
					return networkStatuses, err
				}).
					WithTimeout(15 * time.Second).
					WithPolling(200 * time.Millisecond).
					Should(HaveLen(1))
				for _, ip := range networkStatuses[0].IPs {
					ips[pod.Name] = append(ips[pod.Name], ip)
				}
			}
			return ips
		}

		checkConnectivity = func(vmName string, externalContainer infraapi.ExternalContainer, serverAddresses []string, serverPort int32, stage string) {
			GinkgoHelper()
			by(vmName, "Check connectivity "+stage)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred())
			polling := 6 * time.Second
			timeout := 2 * time.Minute
			step := by(vmName, stage+": Check iperf3 traffic from the external container is not broken")
			for _, address := range serverAddresses {
				// IPv6 gateway reconciliation is separate from the IPv4 GARP fix.
				trafficTimeout := timeout
				if net.ParseIP(address).To4() != nil {
					trafficTimeout = 2 * time.Second
				}
				checkNorthSouthIngressIperfTraffic(externalContainer, []string{address}, serverPort, trafficTimeout, step)
			}

			stage = by(vmName, stage+": Check e/w tcp traffic")
			checkEastWestTraffic(vmi, httpServerTestPodsDefaultNetworkIPs(), stage)

			step = by(vmName, stage+": Check n/s tcp traffic")
			output := ""
			Eventually(func() error {
				output, err = virtClient.RunCommand(vmi, "curl -kL https://kubernetes.default.svc.cluster.local", polling)
				return err
			}).
				WithPolling(polling).
				WithTimeout(timeout).
				Should(Succeed(), func() string { return step + ": " + output })
		}

		checkConnectivityAndNetworkPolicies = func(vmName string, externalContainer infraapi.ExternalContainer, serverAddresses []string, serverPort int32, stage string) {
			GinkgoHelper()
			checkConnectivity(vmName, externalContainer, serverAddresses, serverPort, stage)
			By("Skip network policy, test should be fixed after OVN bump broke them")
		}

		liveMigrateVirtualMachine = func(vmName string) {
			GinkgoHelper()
			vmimCreationRetries := 0
			Eventually(func() error {
				if vmimCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vmim %s creation", vmName))
				}
				vmim := &kubevirtv1.VirtualMachineInstanceMigration{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:    namespace,
						GenerateName: vmName,
					},
					Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
						VMIName: vmName,
					},
				}
				err := crClient.Create(context.Background(), vmim)
				vmimCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		checkLiveMigrationSucceeded = func(vmName string, migrationMode kubevirtv1.MigrationMode) {
			GinkgoHelper()
			By("checking the VM live-migrated correctly")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi")
			currentNode := vmi.Status.NodeName

			Eventually(func() *kubevirtv1.VirtualMachineInstanceMigrationState {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState
			}).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(BeNil(), "should have a MigrationState")
			Eventually(func() string {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState.TargetNode
			}).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(Equal(currentNode), "should refresh MigrationState")
			Eventually(func() bool {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).NotTo(HaveOccurred())
				return vmi.Status.MigrationState.Completed
			}).WithPolling(time.Second).WithTimeout(20*time.Minute).Should(BeTrue(), "should complete migration")
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())
			Expect(vmi.Status.MigrationState.SourcePod).NotTo(BeEmpty())
			Eventually(func() corev1.PodPhase {
				sourcePod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      vmi.Status.MigrationState.SourcePod,
					},
				}
				err = crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(sourcePod), sourcePod)
				Expect(err).NotTo(HaveOccurred())
				return sourcePod.Status.Phase
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Equal(corev1.PodSucceeded), "should move source pod to Completed")
			err = crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi after migration")
			Expect(vmi.Status.MigrationState.Failed).To(BeFalse(), func() string {
				vmiJSON, err := json.Marshal(vmi)
				if err != nil {
					return fmt.Sprintf("failed marshaling migrated VM: %v", vmiJSON)
				}
				return fmt.Sprintf("should live migrate successfully: %s", string(vmiJSON))
			})
			Expect(vmi.Status.MigrationState.Mode).To(Equal(migrationMode), "should be the expected migration mode %s", migrationMode)
		}

		liveMigrateSucceed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			liveMigrateVirtualMachine(vmi.Name)
			checkLiveMigrationSucceeded(vmi.Name, kubevirtv1.MigrationPreCopy)
		}

		vmiMigrations = func(client crclient.Client) ([]kubevirtv1.VirtualMachineInstanceMigration, error) {
			unstructuredVMIMigrations := &unstructured.UnstructuredList{}
			unstructuredVMIMigrations.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   kubevirtv1.GroupVersion.Group,
				Kind:    "VirtualMachineInstanceMigrationList",
				Version: kubevirtv1.GroupVersion.Version,
			})

			if err := client.List(context.Background(), unstructuredVMIMigrations); err != nil {
				return nil, err
			}
			if len(unstructuredVMIMigrations.Items) == 0 {
				return nil, fmt.Errorf("empty migration list")
			}

			var migrations []kubevirtv1.VirtualMachineInstanceMigration
			for i := range unstructuredVMIMigrations.Items {
				var vmiMigration kubevirtv1.VirtualMachineInstanceMigration
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
					unstructuredVMIMigrations.Items[i].Object,
					&vmiMigration,
				); err != nil {
					return nil, err
				}
				migrations = append(migrations, vmiMigration)
			}

			return migrations, nil
		}

		checkLiveMigrationFailed = func(vmName string) {
			GinkgoHelper()
			By("checking the VM live-migrated failed to migrate")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred(), "should success retrieving vmi")

			Eventually(func() (kubevirtv1.VirtualMachineInstanceMigrationPhase, error) {
				migrations, err := vmiMigrations(crClient)
				if err != nil {
					return kubevirtv1.MigrationPhaseUnset, err
				}
				if len(migrations) > 1 {
					return kubevirtv1.MigrationPhaseUnset, fmt.Errorf("expected one migration, got %d", len(migrations))
				}
				return migrations[0].Status.Phase, nil
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(
				Equal(kubevirtv1.MigrationFailed),
			)
		}

		liveMigrateFailed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			forceLiveMigrationFailureAnnotationName := kubevirtv1.FuncTestForceLauncherMigrationFailureAnnotation
			By(fmt.Sprintf("Forcing live migration failure by annotating VM with %s", forceLiveMigrationFailureAnnotationName))
			vmiKey := types.NamespacedName{Namespace: namespace, Name: vmi.Name}
			Eventually(func() error {
				err := crClient.Get(context.TODO(), vmiKey, vmi)
				if err == nil {
					vmi.ObjectMeta.Annotations[forceLiveMigrationFailureAnnotationName] = "true"
					err = crClient.Update(context.TODO(), vmi)
				}
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())

			liveMigrateVirtualMachine(vmi.Name)
			checkLiveMigrationFailed(vmi.Name)
		}

		globalAddressesByFamily = func(isOfFamily func(string) bool, vmi *kubevirtv1.VirtualMachineInstance) func() ([]string, error) {
			return func() ([]string, error) {
				addresses, err := kubevirt.RetrieveAllGlobalAddressesFromGuest(virtClient, vmi)
				if err != nil {
					return nil, err
				}
				familyAddresses := []string{}
				for _, address := range addresses {
					if isOfFamily(address) {
						familyAddresses = append(familyAddresses, address)
					}
				}
				return familyAddresses, nil
			}
		}

		addressesFromStatus = func(vmi *kubevirtv1.VirtualMachineInstance) func() ([]string, error) {
			return func() ([]string, error) {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				if err != nil {
					return nil, err
				}
				var addresses []string
				for _, iface := range vmi.Status.Interfaces {
					for _, ip := range iface.IPs {
						if netip.MustParseAddr(ip).IsLinkLocalUnicast() {
							continue
						}
						addresses = append(addresses, ip)
					}
				}
				return addresses, nil
			}
		}

		createVirtualMachine = func(vm *kubevirtv1.VirtualMachine) {
			GinkgoHelper()
			createVirtualMachineWithClient(crClient, vm)
		}

		createVirtualMachineInstance = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			By(fmt.Sprintf("Create virtual machine instance %s", vmi.Name))
			vmiCreationRetries := 0
			Eventually(func() error {
				if vmiCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vmi %s creation", vmi.Name))
				}
				err := crClient.Create(context.Background(), vmi)
				vmiCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		waitVirtualMachineInstanceReadinessWith = func(
			vmi *kubevirtv1.VirtualMachineInstance,
			conditionType kubevirtv1.VirtualMachineInstanceConditionType,
			conditionStatus corev1.ConditionStatus,
		) {
			GinkgoHelper()
			waitForVMIReadinessWithClient(crClient, vmi, conditionType, conditionStatus)
		}

		waitVirtualMachineInstanceReadiness = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			waitVirtualMachineInstanceReadinessWith(vmi, kubevirtv1.VirtualMachineInstanceReady, corev1.ConditionTrue)
		}
		waitVirtualMachineInstanceFailed = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			waitVirtualMachineInstanceReadinessWith(vmi, kubevirtv1.VirtualMachineInstanceReady, corev1.ConditionFalse)
		}

		waitVirtualMachineAddresses = func(vmi *kubevirtv1.VirtualMachineInstance) {
			GinkgoHelper()
			step := by(vmi.Name, "Wait for virtual machine to receive IPv4 address from DHCP")
			Eventually(globalAddressesByFamily(utilnet.IsIPv4String, vmi)).
				WithPolling(time.Second).
				WithTimeout(5*time.Minute).
				Should(HaveLen(1), step)
			if isDualStack() {
				step = by(vmi.Name, "Wait for virtual machine to receive IPv6 address from DHCP")
				Eventually(globalAddressesByFamily(utilnet.IsIPv6String, vmi)).
					WithPolling(time.Second).
					WithTimeout(5*time.Minute).
					Should(HaveLen(1), func() string {
						out, _ := virtClient.RunCommand(vmi, "nmcli device show eth0", 5*time.Second)
						return step + " -> nmcli device show eth0:\n" + out
					})
			}
		}

		waitForVMPodErrorEvent = func(vmName string, expectedErrorSubstring string) {
			GinkgoHelper()
			Eventually(func() []corev1.Event {
				podList, err := fr.ClientSet.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: fmt.Sprintf("%s=%s", kubevirtv1.VirtualMachineNameLabel, vmName),
				})
				if err != nil || len(podList.Items) == 0 {
					return nil
				}

				events, err := fr.ClientSet.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{
					FieldSelector: fmt.Sprintf("involvedObject.name=%s", podList.Items[0].Name),
				})
				if err != nil {
					return nil
				}

				return events.Items
			}).
				WithTimeout(60*time.Second).
				WithPolling(2*time.Second).
				Should(ContainElement(SatisfyAll(
					HaveField("Type", Equal("Warning")),
					HaveField("Message", ContainSubstring(expectedErrorSubstring)),
				)), fmt.Sprintf("VM %s should fail with error: %s", vmName, expectedErrorSubstring))
		}

		virtualMachineAddressesFromStatus = func(vmi *kubevirtv1.VirtualMachineInstance, expectedNumberOfAddresses int) []string {
			GinkgoHelper()
			step := by(vmi.Name, "Wait for virtual machine to report addresses")
			Eventually(addressesFromStatus(vmi)).
				WithPolling(time.Second).
				WithTimeout(10*time.Second).
				Should(HaveLen(expectedNumberOfAddresses), step)

			addresses, err := addressesFromStatus(vmi)()
			Expect(err).NotTo(HaveOccurred())
			return addresses
		}

		generateVM = func(vmi *kubevirtv1.VirtualMachineInstance) *kubevirtv1.VirtualMachine {
			return composeVM(namespace, vmi)
		}

		fedoraWithTestToolingVMI = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachineInstance {
			return composeFedoraWithTestToolingVMI(namespace, labels, annotations, nodeSelector, networkSource, userData, networkData)
		}

		fedoraWithTestToolingVM = func(labels map[string]string, annotations map[string]string, nodeSelector map[string]string, networkSource kubevirtv1.NetworkSource, userData, networkData string) *kubevirtv1.VirtualMachine {
			return composeFedoraWithTestToolingVM(namespace, labels, annotations, nodeSelector, networkSource, userData, networkData)
		}

		createVMWithStaticIP = func(vmName string, staticIPs []string) *kubevirtv1.VirtualMachine {
			GinkgoHelper()
			annotations, err := kubevirt.GenerateAddressesAnnotations("net1", staticIPs)
			Expect(err).NotTo(HaveOccurred())

			vm := fedoraWithTestToolingVM(
				nil,         // labels
				annotations, // annotations with static IP
				nil,         // nodeSelector
				kubevirtv1.NetworkSource{
					Pod: &kubevirtv1.PodNetwork{},
				},
				`#cloud-config
password: fedora
chpasswd: { expire: False }
`,
				`version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`,
			)
			vm.Name = vmName
			vm.Namespace = namespace
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			return vm
		}

		composeDefaultNetworkLiveMigratableVM = func(labels map[string]string, userData, networkData string) *kubevirtv1.VirtualMachine {
			annotations := map[string]string{
				"kubevirt.io/allow-pod-bridge-network-live-migration": "",
			}
			nodeSelector := map[string]string{
				namespace: "true",
			}
			networkSource := kubevirtv1.NetworkSource{
				Pod: &kubevirtv1.PodNetwork{},
			}
			return fedoraWithTestToolingVM(labels, annotations, nodeSelector, networkSource, userData, networkData)
		}

		composeDefaultNetworkLiveMigratableVMs = func(numberOfVMs int, labels map[string]string) []*kubevirtv1.VirtualMachine {
			userData := fmt.Sprintf(`#cloud-config
password: fedora
chpasswd: { expire: False }
write_files:
  - path: /tmp/iperf-server.sh
    encoding: b64
    content: %s
    permissions: '0755'
runcmd:
  - systemctl mask --now systemd-resolved.service
  - rm -f /etc/resolv.conf
  - systemctl restart NetworkManager.service
`, base64.StdEncoding.EncodeToString([]byte(iperfServerScript)))
			// We need v1 to be able to propertly configure ipv6.method=dhcp
			// autoconf should be disable for cluster default network
			// migrating mechanism to work, we cannot do this with v2
			networkData := `version: 1
config:
  - type: physical
    name: eth0
    subnets:
      - type: dhcp4
`
			if isDualStack() {
				networkData += `      - type: ipv6_dhcpv6-stateful
        routes:
          - network: "::"
            prefix: 0
            gateway: "fe80::1"
`
			}
			vms := []*kubevirtv1.VirtualMachine{}
			for i := 1; i <= numberOfVMs; i++ {
				vms = append(vms, composeDefaultNetworkLiveMigratableVM(labels, userData, networkData))
			}
			return vms
		}
		liveMigrateAndCheck = func(vmName string, migrationMode kubevirtv1.MigrationMode, externalContainer infraapi.ExternalContainer, serverAddresses []string, serverPort int32, step string) {
			liveMigrateVirtualMachine(vmName)
			checkLiveMigrationSucceeded(vmName, migrationMode)
			checkConnectivityAndNetworkPolicies(vmName, externalContainer, serverAddresses, serverPort, step)
		}

		runLiveMigrationTest = func(td liveMigrationTestData, vm *kubevirtv1.VirtualMachine, externalContainer infraapi.ExternalContainer) {
			GinkgoHelper()
			defer GinkgoRecover()
			defer wg.Done()
			step := by(vm.Name, "Login to virtual machine")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).NotTo(HaveOccurred())
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			// LoginToFedora only opens the console; cloud-init (the write_files
			// with the iperf3 server script and the runcmd DNS tweaks) may
			// still be running. Wait for it to finish so the script is in
			// place before starting the iperf3 server.
			step = by(vm.Name, "Wait for cloud-init to finish")
			output, err := virtClient.RunCommand(vmi, "cloud-init status --wait", 5*time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			waitVirtualMachineAddresses(vmi)

			step = by(vm.Name, "Start iperf3 server at virtual machine")
			output, err = virtClient.RunCommand(vmi, "/tmp/iperf-server.sh", time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			step = by(vm.Name, "Expose iperf3 server as a service")
			svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("iperf3-server", vm.Name, iperf3DefaultPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), step)
			defer func() {
				// Dumping the iperf3 server logs is best effort diagnostics,
				// it should not fail the test on its own.
				output, err := virtClient.RunCommand(vmi, fmt.Sprintf("cat %s", iperfServerLogFile("*")), 10*time.Second)
				if err != nil {
					fmt.Printf("%s: failed dumping iperf3 server logs: %v: %s\n", vmi.Name, err, output)
					return
				}
				fmt.Printf("%s iperf3 server logs: %s", vmi.Name, output)
			}()

			serverAddresses, serverPort := serviceNodePortAddresses(clientSet, svc)
			defer stopIngressIperfClients(externalContainer, serverAddresses, serverPort)

			By("Wait some time for service to settle")
			Eventually(func() error {
				// Stop possible leftovers from a previous failed attempt so
				// the iperf3 servers at the VM are free to accept new clients.
				stopIngressIperfClients(externalContainer, serverAddresses, serverPort)
				return startNorthSouthIngressIperfTraffic(externalContainer, serverAddresses, serverPort, step)
			}).WithPolling(3*time.Second).WithTimeout(60*time.Second).Should(Succeed(), "Should start iperf3 traffic from the external container once service settled")

			checkConnectivityAndNetworkPolicies(vm.Name, externalContainer, serverAddresses, serverPort, "before live migration")
			// Do just one migration that will fail
			if td.shouldExpectFailure {
				by(vm.Name, "Live migrate virtual machine to check failed migration")
				liveMigrateFailed(vmi)
				checkConnectivityAndNetworkPolicies(vm.Name, externalContainer, serverAddresses, serverPort, "after live migrate to check failed migration")
			} else {
				originalNode := vmi.Status.NodeName
				by(vm.Name, "Live migrate for the first time")
				liveMigrateAndCheck(vm.Name, td.mode, externalContainer, serverAddresses, serverPort, "after live migrate for the first time")

				by(vm.Name, "Live migrate for the second time to a node not owning the subnet")
				// Remove the node selector label from original node to force
				// live migration to a different one.
				e2enode.RemoveLabelOffNode(fr.ClientSet, originalNode, namespace)
				liveMigrateAndCheck(vm.Name, td.mode, externalContainer, serverAddresses, serverPort, "after live migration for the second time to node not owning subnet")

				by(vm.Name, "Live migrate for the third time to the node owning the subnet")
				// Patch back the original node with the label and remove it
				// from the rest of nodes to force live migration target to it.
				e2enode.AddOrUpdateLabelOnNode(fr.ClientSet, originalNode, namespace, "true")
				for _, selectedNode := range selectedNodes {
					if selectedNode.Name != originalNode {
						e2enode.RemoveLabelOffNode(fr.ClientSet, selectedNode.Name, namespace)
					}
				}
				liveMigrateAndCheck(vm.Name, td.mode, externalContainer, serverAddresses, serverPort, "after live migration to node owning the subnet")

				if isIPv4Supported(fr.ClientSet) {
					by(vm.Name, "Re-resolve the IPv4 gateway on its subnet owner before leaving again")
					ctx, cancel := context.WithTimeout(context.Background(), e2eframework.SingleCallTimeout)
					homeNode, err := fr.ClientSet.CoreV1().Nodes().Get(ctx, originalNode, metav1.GetOptions{})
					cancel()
					Expect(err).NotTo(HaveOccurred())
					subnets, err := util.ParseNodeHostSubnetAnnotation(homeNode, "default")
					Expect(err).NotTo(HaveOccurred())
					subnet, err := util.MatchFirstIPNetFamily(false, subnets)
					Expect(err).NotTo(HaveOccurred())
					gateway := util.GetNodeGatewayIfAddr(subnet).IP
					output, err := virtClient.RunCommand(vmi, fmt.Sprintf("ip -4 neigh flush to %s dev eth0 && ping -4 -c 1 -W 2 %s", gateway, gateway), 5*time.Second)
					Expect(err).NotTo(HaveOccurred(), output)
					Eventually(func() (string, error) {
						return virtClient.RunCommand(vmi, fmt.Sprintf("ip -4 neigh show to %s dev eth0", gateway), 5*time.Second)
					}).WithTimeout(10 * time.Second).WithPolling(time.Second).Should(ContainSubstring(util.IPAddrToHWAddr(gateway).String()))
				}

				by(vm.Name, "Live migrate away from the subnet owner again")
				for _, selectedNode := range selectedNodes {
					if selectedNode.Name != originalNode {
						e2enode.AddOrUpdateLabelOnNode(fr.ClientSet, selectedNode.Name, namespace, "true")
					}
				}
				liveMigrateAndCheck(vm.Name, td.mode, externalContainer, serverAddresses, serverPort, "after leaving the subnet owner again")
			}
		}

		checkPodHasIPAtStatus = func(g Gomega, pod *corev1.Pod) {
			g.Expect(pod.Status.PodIP).NotTo(BeEmpty(), "pod %s has no valid IP address yet", pod.Name)
		}

		sanitizeNodeName = func(nodeName string) string {
			return strings.ReplaceAll(nodeName, ".", "-")
		}

		createHTTPServerPods = func(annotations map[string]string) []*corev1.Pod {
			var pods []*corev1.Pod
			for _, selectedNode := range selectedNodes {
				pod := composeAgnhostPod(
					"testpod-"+sanitizeNodeName(selectedNode.Name),
					namespace,
					selectedNode.Name,
					"netexec", "--http-port", "8000")
				pod.Annotations = annotations
				pods = append(pods, e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod))
			}
			return pods
		}

		nextIPs = func(idx int, subnets []string) ([]string, error) {
			var ips []string
			for _, subnet := range subnets {
				ip, ipNet, err := net.ParseCIDR(subnet)
				if err != nil {
					return nil, err
				}
				for range idx {
					ip = iputils.NextIP(ip)
				}
				ipNet.IP = ip
				ips = append(ips, ipNet.String())
			}
			return ips, nil
		}

		createIperfServerPods = func(nodes []corev1.Node, udnName string, role udnv1.NetworkRole, staticSubnets []string) ([]*corev1.Pod, error) {
			var pods []*corev1.Pod
			for i, node := range nodes {
				var nse *nadapi.NetworkSelectionElement
				if role != udnv1.NetworkRolePrimary {
					staticIPs, err := nextIPs(i, staticSubnets)
					if err != nil {
						return nil, err
					}
					nse = &nadapi.NetworkSelectionElement{
						Name:      udnName,
						IPRequest: staticIPs,
					}
				}
				pod, err := createPod(fr, "testpod-"+sanitizeNodeName(node.Name), node.Name, namespace, []string{"bash", "-c"}, map[string]string{}, func(pod *corev1.Pod) {
					if nse != nil {
						pod.Annotations = networkSelectionElements(*nse)
					}
					pod.Spec.Containers[0].Image = images.Netshoot()
					pod.Spec.Containers[0].Args = []string{iperfServerScript + "\n sleep infinity"}
				})
				if err != nil {
					return nil, err
				}
				pods = append(pods, pod)
			}
			return pods, nil
		}

		waitForPodsCondition = func(pods []*corev1.Pod, conditionFn func(g Gomega, pod *corev1.Pod)) {
			for _, pod := range pods {
				Eventually(func(g Gomega) {
					var err error
					pod, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					conditionFn(g, pod)
				}).
					WithTimeout(time.Minute).
					WithPolling(time.Second).
					Should(Succeed())
			}
		}

		updatePods = func(pods []*corev1.Pod) []*corev1.Pod {
			for i, pod := range pods {
				var err error
				pod, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				pods[i] = pod
			}
			return pods
		}

		prepareHTTPServerPods = func(annotations map[string]string, conditionFn func(g Gomega, pod *corev1.Pod)) {
			By("Preparing HTTP server pods")
			httpServerTestPods = createHTTPServerPods(annotations)
			waitForPodsCondition(httpServerTestPods, conditionFn)
			httpServerTestPods = updatePods(httpServerTestPods)
		}

		removeImagesInNodes = func(imageURL string) error {
			return removeImagesFromNodes(fr.ClientSet, imageURL)
		}

		createCUDN = func(ctx context.Context, cudn *udnv1.ClusterUserDefinedNetwork) {
			GinkgoHelper()
			createCUDNWithClients(ctx, crClient, fr.DynamicClient, cudn)
		}

		createRA = func(ra *rav1.RouteAdvertisements) {
			GinkgoHelper()
			By("Creating RouteAdvertisements")
			Expect(crClient.Create(context.Background(), ra)).To(Succeed())
			DeferCleanup(func() {
				if e2eframework.TestContext.DeleteNamespace && (e2eframework.TestContext.DeleteNamespaceOnFailure || !CurrentSpecReport().Failed()) {
					crClient.Delete(context.Background(), ra)
				}
			})

			By("ensure route advertisement matching CUDN was created successfully")
			Eventually(func(g Gomega) string {
				Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(ra), ra)).To(Succeed())
				return ra.Status.Status
			}, 30*time.Second, time.Second).Should(Equal("Accepted"))
		}

		getCUDNSubnets = func(cudn *udnv1.ClusterUserDefinedNetwork) []string {
			nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.TODO(), cudn.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			var result map[string]interface{}
			err = json.Unmarshal([]byte(nad.Spec.Config), &result)
			Expect(err).NotTo(HaveOccurred())
			return strings.Split(result["subnets"].(string), ",")
		}
	)
	BeforeEach(func() {
		// So we can use it at AfterEach, since fr.ClientSet is nil there
		clientSet = fr.ClientSet
		providerCtx = infraprovider.Get().NewTestContext()

		var err error
		crClient, err = newControllerRuntimeClient()
		Expect(err).NotTo(HaveOccurred())

		virtClient, err = kubevirt.NewClient("/tmp")
		Expect(err).NotTo(HaveOccurred())

		nadClient, err = nadclient.NewForConfig(fr.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with default pod network", Ordered, func() {
		BeforeEach(func() {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework": fr.BaseName,
			})
			Expect(err).ToNot(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(workerNodeList.Items)).To(BeNumerically(">=", 3), "at least three worker nodes are needed for interconnect live-migration scenarios")
			selectedNodes = workerNodeList.Items[:3]

			// Label the selected nodes with the generated namespaces, so we can
			// configure VM nodeSelector with it and live migration will take only
			// them into consideration
			for _, node := range selectedNodes {
				e2enode.AddOrUpdateLabelOnNode(fr.ClientSet, node.Name, namespace, "true")
			}

			prepareHTTPServerPods(map[string]string{}, checkPodHasIPAtStatus)
		})

		AfterEach(func() {
			for _, node := range selectedNodes {
				e2enode.RemoveLabelOffNode(fr.ClientSet, node.Name, namespace)
			}
		})

		AfterAll(func() {
			Expect(removeImagesInNodes(images.FedoraKubevirtContainerDisk())).To(Succeed())
		})

		DescribeTable("when live migration", func(td liveMigrationTestData) {
			if td.mode == kubevirtv1.MigrationPostCopy && os.Getenv("GITHUB_ACTIONS") == "true" {
				Skip("Post copy live migration not working at github actions")
			}
			if td.mode == kubevirtv1.MigrationPostCopy && os.Getenv("KUBEVIRT_SKIP_MIGRATE_POST_COPY") == "true" {
				Skip("Post copy live migration explicitly skipped")
			}
			var err error

			d.ConntrackDumpingDaemonSet()
			d.OVSFlowsDumpingDaemonSet(deploymentconfig.Get().ExternalBridgeName())
			d.IPTablesDumpingDaemonSet()
			d.NFTablesDumpingDaemonSet()

			By("Create external container to run the iperf3 clients")
			externalContainer := infraapi.ExternalContainer{
				Name:    namespace + "-iperf",
				Image:   images.Netshoot(),
				CmdArgs: []string{"sleep", "infinity"},
				ExtPort: infraprovider.Get().GetExternalContainerPort(),
			}
			primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
			Expect(err).NotTo(HaveOccurred(), "primary network must be available to attach containers")
			externalContainer.Network = primaryProviderNetwork
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
			Expect(err).NotTo(HaveOccurred(), "creation of external container is test dependency")

			bandwidthPerMigration := resource.MustParse("40Mi")
			forcePostCopyMigrationPolicy := &kvmigrationsv1alpha1.MigrationPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "force-post-copy",
				},
				Spec: kvmigrationsv1alpha1.MigrationPolicySpec{
					AllowPostCopy:           ptr.To(true),
					CompletionTimeoutPerGiB: ptr.To(int64(1)),
					BandwidthPerMigration:   &bandwidthPerMigration,
					Selectors: &kvmigrationsv1alpha1.Selectors{
						VirtualMachineInstanceSelector: kvmigrationsv1alpha1.LabelSelector{
							"test-live-migration": "post-copy",
						},
					},
				},
			}
			if td.mode == kubevirtv1.MigrationPostCopy {
				err = crClient.Create(context.TODO(), forcePostCopyMigrationPolicy)
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					Expect(crClient.Delete(context.TODO(), forcePostCopyMigrationPolicy)).To(Succeed())
				}()
			}

			vmLabels := map[string]string{}
			if td.mode == kubevirtv1.MigrationPostCopy {
				vmLabels = forcePostCopyMigrationPolicy.Spec.Selectors.VirtualMachineInstanceSelector
			}
			vms := composeDefaultNetworkLiveMigratableVMs(td.numberOfVMs, vmLabels)

			for _, vm := range vms {
				By(fmt.Sprintf("Create virtual machine %s", vm.Name))
				vmCreationRetries := 0
				Eventually(func() error {
					if vmCreationRetries > 0 {
						// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
						// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/3902#issuecomment-1750257559
						By(fmt.Sprintf("Retrying vm %s creation", vm.Name))
					}
					err = crClient.Create(context.Background(), vm)
					vmCreationRetries++
					return err
				}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
			}

			for _, vm := range vms {
				By(fmt.Sprintf("Waiting for readiness at virtual machine %s", vm.Name))
				Eventually(func() bool {
					err = crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm), vm)
					Expect(err).NotTo(HaveOccurred())
					return vm.Status.Ready
				}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(BeTrue())
			}
			wg.Add(int(td.numberOfVMs))
			for _, vm := range vms {
				go runLiveMigrationTest(td, vm, externalContainer)
			}
			wg.Wait()
		},
			Entry("with pre-copy succeeds, should keep connectivity", liveMigrationTestData{
				mode:        kubevirtv1.MigrationPreCopy,
				numberOfVMs: 1,
			}),
			Entry("with post-copy succeeds, should keep connectivity", liveMigrationTestData{
				mode:        kubevirtv1.MigrationPostCopy,
				numberOfVMs: 1,
			}),
			Entry("with pre-copy fails, should keep connectivity", liveMigrationTestData{
				mode:                kubevirtv1.MigrationPreCopy,
				numberOfVMs:         1,
				shouldExpectFailure: true,
			}),
		)
	})
	Context("with user defined networks and persistent ips configured", Ordered, func() {
		AfterAll(func() {
			Expect(removeImagesInNodes(images.FedoraKubevirtContainerDisk())).To(Succeed())
		})
		type testCommand struct {
			description string
			cmd         func()
		}
		type resourceCommand struct {
			description string
			cmd         func() string
		}
		const (
			bgpASN = 64512
		)
		var (
			cudn                    *udnv1.ClusterUserDefinedNetwork
			vm                      *kubevirtv1.VirtualMachine
			vmi                     *kubevirtv1.VirtualMachineInstance
			cidrIPv4, cidrIPv6      string
			staticIPv4, staticIPv6  string
			staticMAC               = "02:00:00:00:00:01"
			externalMACVRFContainer = infraapi.ExternalContainer{
				CmdArgs: []string{"sleep", "infinity"},
			}

			restart = testCommand{
				description: "restart",
				cmd: func() {
					By("Restarting vm")
					output, err := virtClient.RestartVirtualMachine(vmi)
					Expect(err).NotTo(HaveOccurred(), output)

					By("Wait some time to vmi conditions to catch up after restart")
					time.Sleep(3 * time.Second)

					waitVirtualMachineInstanceReadiness(vmi)
				},
			}
			liveMigrate = testCommand{
				description: "live migration",
				cmd: func() {
					liveMigrateSucceed(vmi)
				},
			}
			liveMigrateFailed = testCommand{
				description: "live migration failed",
				cmd: func() {
					liveMigrateFailed(vmi)
				},
			}
			// For secondary network interfaces:
			// - DHCPv6 cannot be activated because KubeVirt does not support it.
			// - In Fedora 39, the "may-fail" option is configured by cloud-init in
			//   NetworkManager. This causes the entire interface to remain inactive
			//   if no IPv6 address is assigned.
			networkDataIPv4 = `version: 2
ethernets:
  eth0:
    dhcp4: true
`

			networkDataDualStack = `version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`
			userData = `
#cloud-config
password: fedora
chpasswd: { expire: False }
`

			userDataWithIperfServer = userData + fmt.Sprintf(`
write_files:
  - path: /tmp/iperf-server.sh
    encoding: b64
    content: %s
    permissions: '0755'
`, base64.StdEncoding.EncodeToString([]byte(iperfServerScript)))

			virtualMachine = resourceCommand{
				description: "VirtualMachine",
				cmd: func() string {
					vm = fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
						Multus: &kubevirtv1.MultusNetwork{
							NetworkName: cudn.Name,
						},
					}, userData, networkDataIPv4)
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineWithUDN = resourceCommand{
				description: "VirtualMachine with interface binding for UDN",
				cmd: func() string {
					vm = fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineWithUDNAndStaticIPsAndMAC = resourceCommand{
				description: "VirtualMachine with interface binding for UDN and statics IPs and MAC",
				cmd: func() string {
					GinkgoHelper()
					if !isPreConfiguredUdnAddressesEnabled() {
						Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
					}

					annotations, err := kubevirt.GenerateAddressesAnnotations("net1", filterIPs(fr.ClientSet, staticIPv4, staticIPv6))
					Expect(err).NotTo(HaveOccurred())

					vm = fedoraWithTestToolingVM(nil /*labels*/, annotations, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = staticMAC
					createVirtualMachine(vm)
					return vm.Name
				},
			}

			virtualMachineInstance = resourceCommand{
				description: "VirtualMachineInstance",
				cmd: func() string {
					vmi = fedoraWithTestToolingVMI(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
						Multus: &kubevirtv1.MultusNetwork{
							NetworkName: cudn.Name,
						},
					}, userData, networkDataIPv4)
					createVirtualMachineInstance(vmi)
					return vmi.Name
				},
			}

			virtualMachineInstanceWithUDN = resourceCommand{
				description: "VirtualMachineInstance with interface binding for UDN",
				cmd: func() string {
					vmi = fedoraWithTestToolingVMI(nil /*labels*/, nil /*annotations*/, nil, /*nodeSelector*/
						kubevirtv1.NetworkSource{
							Pod: &kubevirtv1.PodNetwork{},
						}, userDataWithIperfServer, networkDataDualStack)
					vmi.Spec.Domain.Devices.Interfaces[0].Bridge = nil
					vmi.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
					createVirtualMachineInstance(vmi)
					return vmi.Name
				},
			}

			filterOutIPv6 = func(ips map[string][]string) map[string][]string {
				filteredOutIPs := map[string][]string{}
				for podName, podIPs := range ips {
					for _, podIP := range podIPs {
						if !utilnet.IsIPv6String(podIP) {
							_, ok := filteredOutIPs[podName]
							if !ok {
								filteredOutIPs[podName] = []string{}
							}
							filteredOutIPs[podName] = append(filteredOutIPs[podName], podIP)
						}
					}
				}
				return filteredOutIPs
			}
		)
		type testData struct {
			description string
			resource    resourceCommand
			test        testCommand
			topology    udnv1.NetworkTopology
			role        udnv1.NetworkRole
			ingress     string
			ipRequests  []string
			macRequest  string
			evpn        *udnv1.EVPNConfig
		}
		var (
			containerNetwork = func(td testData) (infraapi.Network, error) {
				if td.ingress == "routed" {
					return infraprovider.Get().GetNetwork(bgpExternalNetworkName)
				}
				return infraprovider.Get().PrimaryNetwork()
			}
			exposeVMIperfServer = func(td testData, vmi *kubevirtv1.VirtualMachineInstance, vmiAddresses []string) ([]string, int32) {
				GinkgoHelper()
				if td.ingress == "routed" {
					return vmiAddresses, iperf3DefaultPort
				}
				step := by(vmi.Name, "Expose VM iperf server as a service")
				svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("iperf3-vm-server", vmi.Name, iperf3DefaultPort), metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(svc.Spec.Ports[0].NodePort).NotTo(Equal(0), step)
				serverPort := svc.Spec.Ports[0].NodePort
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), fr.ClientSet, 1)
				Expect(err).NotTo(HaveOccurred())
				serverIPs := e2enode.CollectAddresses(nodes, corev1.NodeInternalIP)
				return serverIPs, serverPort
			}
		)
		DescribeTable("should keep ip", func(ctx SpecContext, td testData) {
			if td.role == "" {
				td.role = udnv1.NetworkRoleSecondary
			}

			l := map[string]string{
				"e2e-framework": fr.BaseName,
			}
			if td.role == udnv1.NetworkRolePrimary {
				l[RequiredUDNNamespaceLabel] = ""
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			bgpAlloc, err := allocators.AllocateBGP(fr, providerCtx)
			Expect(err).NotTo(HaveOccurred())

			networkName := ""
			// Each entry gets its own random subnet to avoid BGP route
			// conflicts between entries (prior routed tests create RAs
			// whose BGP routes may persist in FRR after async cleanup).
			cidrIPv4, cidrIPv6 = bgpAlloc.UDNSubnet, bgpAlloc.UDNSubnet6
			staticIPv4 = subnetOffsetIP(cidrIPv4, 101)
			staticIPv6 = subnetOffsetIP(cidrIPv6, 101)
			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, networkName = kubevirt.GenerateCUDN(namespace, "net1", td.topology, td.role, dualCIDRs)

			var externalContainer infraapi.ExternalContainer
			var macVRFContainerIPs []string
			if td.evpn != nil {
				td.evpn.MACVRF.VNI = int32(bgpAlloc.MACVRFVNI)
				td.evpn.IPVRF.VNI = int32(bgpAlloc.IPVRFVNI)
				externalMACVRFContainer.Name = fmt.Sprintf("iperf3-macvrf-%d", bgpAlloc.MACVRFVNI)
				externalMACVRFContainer.Image = images.Netshoot()
				// Shorten the CUDN name to fit Linux interface name limits.
				// The name is used as testName for runEVPNNetworkAndServers which
				// derives bridge/SVI names: worst-case SVI is "br<name>.4094"
				// which must be ≤15 chars (IFNAMSIZ-1), so name must be ≤8 chars.
				// "kve" prefix = kubevirt evpn.
				shortName := "kve" + namespace[len(namespace)-4:]
				cudn.Name = shortName
				cudn.Labels["name"] = shortName
				networkName = util.GenerateCUDNNetworkName(shortName)

				cudn.Spec.Network.Transport = udnv1.TransportOptionEVPN
				cudn.Spec.Network.EVPN = td.evpn
				if !isLocalGWModeEnabled() {
					e2eskipper.Skipf("EVPN test cases only supported in Local Gateway mode")
				}
				if !isIPv4Supported(fr.ClientSet) {
					// FRR does not support IPv6 underlay for EVPN VXLAN tunnels.
					// See: https://github.com/FRRouting/frr/issues/5885
					e2eskipper.Skipf("EVPN test cases require IPv4 for VXLAN underlay (FRR limitation)")
				}

				// Filter cudn subnets based on cluster IP family support.
				if cudn.Spec.Network.Layer2 != nil {
					cudn.Spec.Network.Layer2.Subnets = filterDualStackCIDRs(fr.ClientSet, cudn.Spec.Network.Layer2.Subnets)
				}
				if cudn.Spec.Network.Layer3 != nil {
					cudn.Spec.Network.Layer3.Subnets = filterL3Subnets(fr.ClientSet, cudn.Spec.Network.Layer3.Subnets)
				}

				ipFamilies := sets.New(getSupportedIPFamiliesSlice(fr.ClientSet)...)

				// Use the kind network CIDRs as VTEP subnets for Unmanaged mode.
				kindNetwork, err := infraprovider.Get().PrimaryNetwork()
				Expect(err).NotTo(HaveOccurred())
				bgpAlloc.VTEPSubnet, _, err = kindNetwork.IPv4IPv6Subnets()
				Expect(err).NotTo(HaveOccurred())
				bgpAlloc.VTEPSubnet6 = ""

				externalContainer = infraapi.ExternalContainer{
					Name:    namespace + "-iperf",
					Image:   images.Netshoot(),
					CmdArgs: []string{"sleep", "infinity"},
					ExtPort: infraprovider.Get().GetExternalContainerPort(),
				}

				By("Setting up EVPN infrastructure and external containers")
				Expect(runEVPNNetworkAndServers(
					fr,
					providerCtx,
					shortName,
					ipFamilies,
					&cudn.Spec.Network,
					bgpAlloc,
					bgpASN,
					"br"+shortName,
					"vx"+shortName,
					sharedNodeIPsVTEPName,
					&externalMACVRFContainer,
					externalMACVRFContainer.Name,
					&externalContainer,
					externalContainer.Name,
					false,
				)).To(Succeed())
				// Register namespace deletion on the infra context so it runs
				// before VTEP poll in LIFO cleanup order. VTEP has a finalizer
				// that blocks until no CUDN references it, and CUDN can't remove
				// its finalizer while pods using its NAD still exist. Without
				// this, namespace (and its pods) is only deleted in the framework
				// AfterEach which runs after all DeferCleanup — too late.
				providerCtx.AddCleanUpFn(func() error {
					return fr.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
				})
			}

			if td.topology == udnv1.NetworkTopologyLocalnet {
				By("setting up the localnet underlay")
				Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())
			}
			createCUDN(ctx, cudn)

			if td.ingress == "routed" {
				ra := &rav1.RouteAdvertisements{
					ObjectMeta: metav1.ObjectMeta{
						Name: cudn.Name,
					},
					Spec: rav1.RouteAdvertisementsSpec{
						Advertisements: []rav1.AdvertisementType{rav1.PodNetwork},
						NetworkSelectors: crdtypes.NetworkSelectors{{
							NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"name": cudn.Name},
								},
							},
						}},
					},
				}
				if td.evpn != nil {
					ra.Spec.FRRConfigurationSelector = metav1.LabelSelector{
						MatchLabels: map[string]string{"network": cudn.Name},
					}
					ra.Spec.TargetVRF = cudn.Name
				}
				createRA(ra)
			}
			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			selectedNodes = workerNodeList.Items
			Expect(selectedNodes).NotTo(BeEmpty())

			iperfServerTestPods, err = createIperfServerPods(selectedNodes, cudn.Name, td.role, []string{})
			Expect(err).NotTo(HaveOccurred())

			if td.role == udnv1.NetworkRolePrimary && td.evpn == nil {
				externalContainerName := namespace + "-iperf"
				externalContainerPort := infraprovider.Get().GetExternalContainerPort()
				externalContainer = infraapi.ExternalContainer{
					Name:    externalContainerName,
					Image:   images.Netshoot(),
					CmdArgs: []string{"sleep", "infinity"},
					ExtPort: externalContainerPort,
				}
				providerNetwork, err := containerNetwork(td)
				Expect(err).ShouldNot(HaveOccurred(), "primary network must be available to attach containers")
				externalContainer.Network = providerNetwork
				externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
				Expect(err).ShouldNot(HaveOccurred(), "creation of external container is test dependency")
			} else if td.role == udnv1.NetworkRolePrimary && td.evpn != nil {
				// Containers were set up by runEVPNNetworkAndServers; collect MAC-VRF IPs.
				if externalMACVRFContainer.IsIPv4() {
					macVRFContainerIPs = append(macVRFContainerIPs, externalMACVRFContainer.IPv4)
				}
				if externalMACVRFContainer.IsIPv6() {
					macVRFContainerIPs = append(macVRFContainerIPs, externalMACVRFContainer.IPv6)
				}
			}

			var externalContainerIPs []string
			if externalContainer.IsIPv4() {
				externalContainerIPs = append(externalContainerIPs, externalContainer.IPv4)
			}
			if externalContainer.IsIPv6() {
				externalContainerIPs = append(externalContainerIPs, externalContainer.IPv6)
			}

			if td.ingress == "routed" && td.evpn == nil {
				// pre=created test dependency and therefore we dont delete
				frrExternalContainer := infraapi.ExternalContainer{Name: routerContainerName}
				frrNetwork, err := containerNetwork(td)
				Expect(err).NotTo(HaveOccurred())
				frrExternalContainerInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(frrExternalContainer, frrNetwork)
				Expect(err).NotTo(HaveOccurred(), "must fetch FRR container network interface attached to secondary network")

				output, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"bash", "-c", fmt.Sprintf(`
set -xe
ip route add %[1]s via %[2]s
ip route add %[3]s via %[4]s
`, cidrIPv4, frrExternalContainerInterface.GetIPv4(), cidrIPv6, frrExternalContainerInterface.GetIPv6())})
				Expect(err).NotTo(HaveOccurred(), output)
			}

			vmiName := td.resource.cmd()
			vmi = &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmiName,
				},
			}

			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step := by(vmi.Name, "Login to virtual machine for the first time")
			Eventually(func() error {
				return virtClient.LoginToFedora(vmi, "fedora", "fedora")
			}).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(Succeed(), step)

			// IPv6 is not supported for secondaries with IPAM since
			// KubeVirt does not run a DHCPv6 server for bridge bindings,
			// so the VMI status will only report IPv4 addresses.
			// For primary UDNs, expect dual-stack (2 addresses); for
			// secondaries, expect IPv4 only (1 address).
			step = by(vmi.Name, "Wait for addresses at the virtual machine")
			expectedNumberOfAddresses := len(dualCIDRs)
			if td.role != udnv1.NetworkRolePrimary {
				expectedNumberOfAddresses = 1
			}
			expectedAddreses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)
			if _, hasIPRequests := vmi.Annotations[kubevirt.AddressesAnnotation]; hasIPRequests {
				Expect(expectedAddreses).To(ConsistOf(filterIPs(fr.ClientSet, staticIPv4, staticIPv6)), "expected addresses should be consistent with the static IPs")
			}
			if vmi.Spec.Domain.Devices.Interfaces[0].MacAddress != "" {
				Expect(vmi.Spec.Domain.Devices.Interfaces[0].MacAddress).To(Equal(vmi.Status.Interfaces[0].MAC), "expected mac address should be consistent with the static MAC")
			}

			expectedAddresesAtGuest := expectedAddreses
			testPodsIPs := podsMultusNetworkIPs(iperfServerTestPods, podNetworkStatusByNetConfigPredicate(namespace, cudn.Name, strings.ToLower(string(td.role))))

			serverIPs, serverPort := exposeVMIperfServer(td, vmi, expectedAddreses)

			// IPv6 is not support for secondaries with IPAM so guest will
			// have only ipv4.
			if td.role != udnv1.NetworkRolePrimary {
				expectedAddresesAtGuest, err = util.MatchAllIPStringFamily(false /*ipv4*/, expectedAddreses)
				Expect(err).NotTo(HaveOccurred())
				testPodsIPs = filterOutIPv6(testPodsIPs)
			}
			Expect(testPodsIPs).NotTo(BeEmpty())

			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(expectedAddresesAtGuest), step)

			step = by(vmi.Name, fmt.Sprintf("Check east/west traffic before %s %s", td.resource.description, td.test.description))
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			if td.role == udnv1.NetworkRolePrimary {
				if isIPv6Supported(fr.ClientSet) {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv6 gateway before %s %s", td.resource.description, td.test.description))

					expectedIPv6GatewayPath, err := kubevirt.GenerateGatewayIPv6RouterLLA(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred())
					Eventually(kubevirt.RetrieveIPv6Gateways).
						WithArguments(virtClient, vmi).
						WithTimeout(5*time.Second).
						WithPolling(time.Second).
						Should(Equal([]string{expectedIPv6GatewayPath}), "should filter remote ipv6 gateway nexthop")
				}
				output, err := virtClient.RunCommand(vmi, "/tmp/iperf-server.sh", time.Minute)
				Expect(err).NotTo(HaveOccurred(), step+": "+output)

				if td.evpn != nil {
					step = by(vmi.Name, fmt.Sprintf("Check east/west external traffic (l2 stretching) before %s %s", td.resource.description, td.test.description))
					Expect(startExternalEastWestIperfTraffic(externalMACVRFContainer, vmi, macVRFContainerIPs, step)).To(Succeed())
					checkExternalEastWestIperfTraffic(vmi, macVRFContainerIPs, step)
				}

				step = by(vmi.Name, fmt.Sprintf("Check north/south traffic before %s %s", td.resource.description, td.test.description))
				Expect(startNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)).To(Succeed())
				checkNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, 2*time.Second, step)
				checkNorthSouthEgressICMPTraffic(vmi, externalContainerIPs, step)
				if td.ingress == "routed" {
					_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{"bash", "-c", iperfServerScript})
					Expect(err).NotTo(HaveOccurred(), step)
					Expect(startNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)).To(Succeed())
					By("Check egress src ip is not node IP on 'routed' ingress mode")
					for _, vmAddress := range expectedAddreses {
						output, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
							"bash", "-c", fmt.Sprintf("grep 'connected to %s' %s", vmAddress, iperfServerLogFile("*")),
						})
						Expect(err).NotTo(HaveOccurred(), step+": "+output)
					}
					checkNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)
				}
			}

			by(vmi.Name, fmt.Sprintf("Running %s for %s", td.test.description, td.resource.description))
			td.test.cmd()

			step = by(vmi.Name, fmt.Sprintf("Login to virtual machine after %s %s", td.resource.description, td.test.description))
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			obtainedAddresses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)

			Expect(obtainedAddresses).To(Equal(expectedAddreses))
			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(expectedAddresesAtGuest), step)

			step = by(vmi.Name, fmt.Sprintf("Check east/west traffic after %s %s", td.resource.description, td.test.description))
			if td.test.description == restart.description {
				// At restart we need re-connect
				Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
				if td.role == udnv1.NetworkRolePrimary {
					output, err := virtClient.RunCommand(vmi, "/tmp/iperf-server.sh", time.Minute)
					Expect(err).NotTo(HaveOccurred(), step+": "+output)
					Expect(startNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, step)).To(Succeed())
					if td.evpn != nil {
						Expect(startExternalEastWestIperfTraffic(externalMACVRFContainer, vmi, macVRFContainerIPs, step)).To(Succeed())
					}
				}
			}
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)
			if td.role == udnv1.NetworkRolePrimary {
				step = by(vmi.Name, fmt.Sprintf("Check north/south traffic after %s %s", td.resource.description, td.test.description))
				checkNorthSouthIngressIperfTraffic(externalContainer, serverIPs, serverPort, 2*time.Second, step)
				checkNorthSouthEgressICMPTraffic(vmi, externalContainerIPs, step)
				if td.ingress == "routed" {
					checkNorthSouthEgressIperfTraffic(vmi, externalContainerIPs, iperf3DefaultPort, step)
				}
				if td.evpn != nil {
					step = by(vmi.Name, fmt.Sprintf("Check east/west external traffic (l2 stretching) after %s %s", td.resource.description, td.test.description))
					checkExternalEastWestIperfTraffic(vmi, macVRFContainerIPs, step)
				}
			}

			if td.role == udnv1.NetworkRolePrimary && td.test.description == liveMigrate.description {
				if isIPv4Supported(fr.ClientSet) {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv4 gateway cached mac after %s %s", td.resource.description, td.test.description))
					Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

					expectedGatewayMAC, err := kubevirt.GenerateGatewayMAC(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred(), step)

					Expect(err).NotTo(HaveOccurred(), step)
					Eventually(kubevirt.RetrieveCachedGatewayMAC).
						WithArguments(virtClient, vmi, "enp1s0", cidrIPv4).
						WithTimeout(10*time.Second).
						WithPolling(time.Second).
						Should(Equal(expectedGatewayMAC), step)
				}
				if isIPv6Supported(fr.ClientSet) {
					step = by(vmi.Name, fmt.Sprintf("Checking IPv6 gateway after %s %s", td.resource.description, td.test.description))
					By(step)

					targetNodeIPv6GatewayPath, err := kubevirt.GenerateGatewayIPv6RouterLLA(getCUDNSubnets(cudn))
					Expect(err).NotTo(HaveOccurred())
					Eventually(kubevirt.RetrieveIPv6Gateways).
						WithArguments(virtClient, vmi).
						WithTimeout(5*time.Second).
						WithPolling(time.Second).
						Should(Equal([]string{targetNodeIPv6GatewayPath}), "should reconcile ipv6 gateway nexthop after live migration")
				}
			}
		},
			func(td testData) string {
				role := udnv1.NetworkRoleSecondary
				if td.role != "" {
					role = td.role
				}
				ingress := "snat"
				if td.ingress != "" {
					ingress = td.ingress
					if ingress == "routed" && td.evpn != nil {
						ingress += " over evpn"
					}
				}
				return fmt.Sprintf("after %s of %s with %s/%s ingress %s", td.test.description, td.resource.description, role, td.topology, ingress)
			},
			Entry(nil, testData{
				resource: virtualMachine,
				test:     restart,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachine,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     restart,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDNAndStaticIPsAndMAC,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
				evpn: &udnv1.EVPNConfig{
					MACVRF: &udnv1.VRFConfig{},
					IPVRF:  &udnv1.VRFConfig{},
				},
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstanceWithUDN,
				test:     liveMigrate,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstanceWithUDN,
				test:     liveMigrateFailed,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
			}),
			Entry(nil, testData{
				resource: virtualMachineInstance,
				test:     liveMigrateFailed,
				topology: udnv1.NetworkTopologyLocalnet,
			}),
			Entry(nil, testData{
				resource: virtualMachineWithUDN,
				test:     liveMigrateFailed,
				topology: udnv1.NetworkTopologyLayer2,
				role:     udnv1.NetworkRolePrimary,
				ingress:  "routed",
				evpn: &udnv1.EVPNConfig{
					MACVRF: &udnv1.VRFConfig{},
					IPVRF:  &udnv1.VRFConfig{},
				},
			}),
		)
	})
	Context("with kubevirt VM using layer2 UDPN", Ordered, func() {
		var (
			cidrIPv4   = "172.31.0.0/24"
			cidrIPv6   = "2010:100:200::/60"
			vmi        *kubevirtv1.VirtualMachineInstance
			vmiCommand = func(command string) (string, error) {
				return virtClient.RunCommand(vmi, command, 5*time.Second)
			}
			primaryUDNDeviceValue = func(field string) ([]string, error) {
				output, err := vmiCommand(fmt.Sprintf(`nmcli -e no -g %s device show eth0`, field))
				if err != nil {
					return nil, err
				}
				output = strings.ReplaceAll(output, "\r", "")
				return strings.Split(output, " | "), nil
			}
		)
		AfterAll(func() {
			Expect(removeImagesInNodes(images.FedoraKubevirtContainerDisk())).To(Succeed())
		})
		BeforeEach(func(ctx SpecContext) {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			cudn.Spec.Network.Layer2.MTU = 1300
			createCUDN(ctx, cudn)

			By("Create VMI with primary UDN")
			networkData := `version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`
			vmi = fedoraWithTestToolingVMI(nil /*labels*/, nil /*annotations*/, nil, /*nodeSelector*/
				kubevirtv1.NetworkSource{
					Pod: &kubevirtv1.PodNetwork{},
				}, "#", networkData)
			vmi.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vmi.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			createVirtualMachineInstance(vmi)

			By("Wait for VMI to be ready")
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			By("Login to the VM console")
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed())
		})
		It("should configure IPv4 and IPv6 using DHCP and NDP", func() {
			dnsService, err := fr.ClientSet.CoreV1().Services(config.Kubernetes.DNSServiceNamespace).
				Get(context.Background(), config.Kubernetes.DNSServiceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedNumberOfAddresses := len(filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)}))
			vmiAddresses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)

			if isIPv4Supported(fr.ClientSet) {
				expectedIP, err := matchIPv4StringFamily(vmiAddresses)
				Expect(err).NotTo(HaveOccurred())

				expectedDNS, err := matchIPv4StringFamily(dnsService.Spec.ClusterIPs)
				Expect(err).NotTo(HaveOccurred())

				_, cidr, err := net.ParseCIDR(cidrIPv4)
				Expect(err).NotTo(HaveOccurred())
				expectedGateway := util.GetNodeGatewayIfAddr(cidr).IP.String()

				Eventually(primaryUDNDeviceValue).
					WithArguments("DHCP4.OPTION").
					WithTimeout(10 * time.Second).
					WithPolling(time.Second).
					Should(ContainElements(
						fmt.Sprintf("host_name = %s", vmi.Name),
						fmt.Sprintf("ip_address = %s", expectedIP),
						fmt.Sprintf("domain_name_servers = %s", expectedDNS),
						fmt.Sprintf("routers = %s", expectedGateway),
						"interface_mtu = 1300",
					))
				Expect(primaryUDNDeviceValue("IP4.ADDRESS")).To(ConsistOf(expectedIP + "/24"))
				Expect(primaryUDNDeviceValue("IP4.GATEWAY")).To(ConsistOf(expectedGateway))
				Expect(primaryUDNDeviceValue("IP4.DNS")).To(ConsistOf(expectedDNS))
				Expect(primaryUDNDeviceValue("GENERAL.MTU")).To(ConsistOf("1300"))
			}

			if isIPv6Supported(fr.ClientSet) {
				expectedIP, err := matchIPv6StringFamily(vmiAddresses)
				Expect(err).NotTo(HaveOccurred())
				Eventually(primaryUDNDeviceValue).
					WithArguments("DHCP6.OPTION").
					WithTimeout(10 * time.Second).
					WithPolling(time.Second).
					Should(ContainElements(
						fmt.Sprintf("fqdn_fqdn = %s", vmi.Name),
						fmt.Sprintf("ip6_address = %s", expectedIP),
					))
				Expect(primaryUDNDeviceValue("IP6.ADDRESS")).To(SatisfyAll(HaveLen(2), ContainElements(expectedIP+"/128")))
				Expect(primaryUDNDeviceValue("IP6.GATEWAY")).To(ConsistOf(WithTransform(func(ipv6 string) bool {
					return netip.MustParseAddr(ipv6).IsLinkLocalUnicast()
				}, BeTrue())))
				Expect(primaryUDNDeviceValue("IP6.ROUTE")).To(ContainElement(ContainSubstring(fmt.Sprintf("dst = %s", cidrIPv6))))
				Expect(primaryUDNDeviceValue("GENERAL.MTU")).To(ConsistOf("1300"))
			}
		})
	})
	Context("with user defined networks with ipamless localnet topology", Ordered, func() {
		BeforeEach(func() {
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, map[string]string{
				"e2e-framework": fr.BaseName,
			})
			Expect(err).ToNot(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
		})
		AfterAll(func() {
			Expect(removeImagesInNodes(images.FedoraKubevirtContainerDisk())).To(Succeed())
		})
		var (
			ipv4CIDR             = "172.31.0.0/24"
			ipv6CIDR             = "2010:100:200::0/60"
			vmiIPv4              = "172.31.0.100/24"
			vmiIPv6              = "2010:100:200::100/60"
			vmiMAC               = "0A:58:0A:80:00:64"
			staticIPsNetworkData = func(ips []string) (string, error) {
				type Ethernet struct {
					DHCP4     *bool    `json:"dhcp4,omitempty"`
					DHCP6     *bool    `json:"dhcp6,omitempty"`
					Addresses []string `json:"addresses,omitempty"`
				}
				networkData, err := yaml.Marshal(&struct {
					Version   int                 `json:"version,omitempty"`
					Ethernets map[string]Ethernet `json:"ethernets,omitempty"`
				}{
					Version: 2,
					Ethernets: map[string]Ethernet{
						"eth0": {
							DHCP4:     ptr.To(false),
							DHCP6:     ptr.To(false),
							Addresses: ips,
						},
					},
				})
				if err != nil {
					return "", err
				}
				return string(networkData), nil
			}

			userData = `#cloud-config
password: fedora
chpasswd: { expire: False }
`
		)
		It("should start multiple VMs with same hostname", func(ctx SpecContext) {
			By("setting up the localnet underlay")
			cudn, networkName := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLocalnet, udnv1.NetworkRoleSecondary, udnv1.DualStackCIDRs{})
			createCUDN(ctx, cudn)

			Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())

			type vmConfig struct {
				ipv4 string
				ipv6 string
				mac  string
			}
			configs := []vmConfig{
				{ipv4: "172.31.0.101/24", ipv6: "2010:100:200::101/60", mac: "0A:58:0A:80:00:65"},
				{ipv4: "172.31.0.102/24", ipv6: "2010:100:200::102/60", mac: "0A:58:0A:80:00:66"},
				{ipv4: "172.31.0.103/24", ipv6: "2010:100:200::103/60", mac: "0A:58:0A:80:00:67"},
			}

			vmis := make([]*kubevirtv1.VirtualMachineInstance, len(configs))
			for i, cfg := range configs {
				filteredCIDRs := filterCIDRs(fr.ClientSet, cfg.ipv4, cfg.ipv6)
				networkData, err := staticIPsNetworkData(filteredCIDRs)
				Expect(err).NotTo(HaveOccurred())

				vm := fedoraWithTestToolingVM(nil, nil, nil, kubevirtv1.NetworkSource{
					Multus: &kubevirtv1.MultusNetwork{
						NetworkName: cudn.Name,
					},
				}, userData, networkData)
				vm.Spec.Template.Spec.Hostname = "shared-hostname"
				vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = cfg.mac
				createVirtualMachine(vm)

				vmis[i] = &kubevirtv1.VirtualMachineInstance{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      vm.Name,
					},
				}
			}

			By("Waiting for all 3 VMIs to become ready")
			for _, vmi := range vmis {
				waitVirtualMachineInstanceReadiness(vmi)
			}
		})
		DescribeTable("should maintain tcp connection with minimal downtime", func(ctx SpecContext, td func(vmi *kubevirtv1.VirtualMachineInstance)) {
			By("setting up the localnet underlay")
			cudn, networkName := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLocalnet, udnv1.NetworkRoleSecondary, udnv1.DualStackCIDRs{})
			createCUDN(ctx, cudn)

			Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())

			workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
			Expect(err).NotTo(HaveOccurred())
			selectedNodes = workerNodeList.Items
			Expect(selectedNodes).NotTo(BeEmpty())

			iperfServerTestPods, err = createIperfServerPods(selectedNodes, cudn.Name, cudn.Spec.Network.Localnet.Role, filterCIDRs(fr.ClientSet, ipv4CIDR, ipv6CIDR))
			Expect(err).NotTo(HaveOccurred())

			filteredCIDRs := filterCIDRs(fr.ClientSet, vmiIPv4, vmiIPv6)
			networkData, err := staticIPsNetworkData(filteredCIDRs)
			Expect(err).NotTo(HaveOccurred())

			vm := fedoraWithTestToolingVM(nil /*labels*/, nil /*annotations*/, nil /*nodeSelector*/, kubevirtv1.NetworkSource{
				Multus: &kubevirtv1.MultusNetwork{
					NetworkName: cudn.Name,
				},
			}, userData, networkData)
			// Harcode mac address so it's the same after live migration
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].MacAddress = vmiMAC
			createVirtualMachine(vm)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step := by(vmi.Name, "Login to virtual machine for the first time")
			Eventually(func() error {
				return virtClient.LoginToFedora(vmi, "fedora", "fedora")
			}).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(Succeed(), step)

			step = by(vmi.Name, "Wait for cloud init to finish at first boot")
			output, err := virtClient.RunCommand(vmi, "cloud-init status --wait", time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			testPodsIPs := podsMultusNetworkIPs(iperfServerTestPods, podNetworkStatusByNetConfigPredicate(namespace, cudn.Name, strings.ToLower(string(cudn.Spec.Network.Localnet.Role))))
			Expect(testPodsIPs).NotTo(BeEmpty())

			step = by(vmi.Name, "Check east/west traffic before virtual machine instance live migration")
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			by(vmi.Name, "Running live migration for virtual machine instance")
			td(vmi)

			// Update vmi status after live migration
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			step = by(vmi.Name, "Login to virtual machine after virtual machine instance live migration")
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			step = by(vmi.Name, "Check east/west traffic after virtual machine instance live migration")
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			By("Stop iperf3 traffic before force killing vm, so iperf3 server do not get stuck")
			output, err = virtClient.RunCommand(vmi, "killall --wait iperf3", 5*time.Second)
			Expect(err).ToNot(HaveOccurred(), output)

			step = by(vmi.Name, fmt.Sprintf("Force kill qemu at node %q where VM is running on", vmi.Status.NodeName))
			Expect(kubevirt.ForceKillVirtLauncherAtNode(infraprovider.Get(), vmi.Status.NodeName, vmi.Namespace, vmi.Name)).To(Succeed(), step)

			step = by(vmi.Name, "Waiting for failed restarted VMI to reach ready state")
			waitVirtualMachineInstanceFailed(vmi)
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed(), step)

			step = by(vmi.Name, "Login to virtual machine after virtual machine instance force killed")
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			step = by(vmi.Name, "Wait for cloud init to finish after vm restart")
			output, err = virtClient.RunCommand(vmi, "cloud-init status --wait", time.Minute)
			Expect(err).NotTo(HaveOccurred(), step+": "+output)

			step = by(vmi.Name, "Verify static IP is configured after vm restart")
			filteredIPs := []string{}
			for _, filteredCIDR := range filteredCIDRs {
				filteredIPs = append(filteredIPs, strings.Split(filteredCIDR, "/")[0])
			}
			Eventually(kubevirt.RetrieveAllGlobalAddressesFromGuest).
				WithArguments(virtClient, vmi).
				WithTimeout(5*time.Second).
				WithPolling(time.Second).
				Should(ConsistOf(filteredIPs), step)

			step = by(vmi.Name, "Restart iperf traffic after forcing a vm failure")
			Expect(startEastWestIperfTraffic(vmi, testPodsIPs, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)

			by(vmi.Name, "Running live migration after forcing vm failure")
			td(vmi)

			step = by(vmi.Name, "Check east/west traffic for failed virtual machine after live migration")
			checkEastWestIperfTraffic(vmi, testPodsIPs, step)
		},
			Entry("after succeeded live migration", liveMigrateSucceed),
			Entry("after failed live migration", liveMigrateFailed),
		)
	})

	DescribeTable("user-defined network port-security disabled, TCP connections with spoofed MAC should survive successful and failed live migration", feature.MACSecurity,
		func(ctx SpecContext, topology udnv1.NetworkTopology) {
			const (
				serverCIDRv4 = "10.10.10.20/24"
				serverCIDRv6 = "2001:db8:abcd:1234::20/64"

				clientCIDRv4  = "10.10.10.10/24"
				clientCIDRv6  = "2001:db8:abcd:1234::10/64"
				clientIface   = "eth0"
				clientBrIface = "br0"
				spoofedMAC    = "02:00:00:fa:fb:fc"
			)
			clientCIDRs := filterCIDRs(fr.ClientSet, clientCIDRv4, clientCIDRv6)
			serverCIDRs := filterCIDRs(fr.ClientSet, serverCIDRv4, serverCIDRv6)

			By("create test namespace")
			fr.BaseName = "kv-port-security-test"
			ns, err := fr.CreateNamespace(context.Background(), fr.BaseName, map[string]string{"e2e-framework": fr.BaseName})
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			By("create network resource")
			cudn, networkName := kubevirt.GenerateCUDN(namespace, "netsted-virt-net", topology, udnv1.NetworkRoleSecondary, nil,
				kubevirt.WithMACSecurityConfig(udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityDisabled}))
			createCUDN(ctx, cudn)

			if topology == udnv1.NetworkTopologyLocalnet {
				By("setting up the localnet underlay")
				Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())
			}

			By("create client VM")
			netSrc := kubevirtv1.NetworkSource{Multus: &kubevirtv1.MultusNetwork{NetworkName: cudn.Name}}
			// Use cloud-init for configure the guest to have linux-bridge on top the default NIC, with custom MAC address and static IP addresses.
			networkData, err := yaml.Marshal(kubevirt.UplinkLinuxBridgeNetworkData(clientIface, clientBrIface, spoofedMAC, clientCIDRs))
			Expect(err).NotTo(HaveOccurred())
			// The bridge MAC address is configured explicitly due to bug in cloud-init
			// TODO: remove the addition 'runcmd' once newer guest image is used, containing newer cloud-init
			userData := fmt.Sprintf(`#cloud-config
runcmd:
 - sudo nmcli c mod 'cloud-init %[1]s' bridge.mac-address %[2]s
 - sudo nmcli c down 'cloud-init %[1]s' && sudo nmcli c up 'cloud-init %[1]s'
`, clientBrIface, spoofedMAC)
			vm := fedoraWithTestToolingVM(nil, nil, nil, netSrc, userData, string(networkData))
			createVirtualMachine(vm)

			step := by(vm.Name, "waiting for client readiness")
			vmi := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: vm.Name}}
			waitVirtualMachineInstanceReadinessWith(vmi, kubevirtv1.VirtualMachineInstanceAgentConnected, corev1.ConditionTrue)
			Expect(virtClient.LoginToFedora(vmi, "fedora", "fedora")).To(Succeed(), step)

			By("creating server pods")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), fr.ClientSet, 3)
			Expect(err).NotTo(HaveOccurred())
			var testNodes []corev1.Node
			// Exclude the VM's source node: an iperf server on that node loses connectivity after migration
			// because a stale OVN FDB entry directs traffic to the disabled/addressless source LSP instead
			// of the underlay. A defect in the iperf3 downtime detector can make this persistent failure
			// appear intermittent. Ref: https://redhat.atlassian.net/browse/FDP-4420
			// TODO: remove this node filtering once the OVN bug is resolved.
			for _, node := range nodes.Items {
				if node.Name != vmi.Status.NodeName {
					testNodes = append(testNodes, node)
				}
			}
			Expect(testNodes).NotTo(BeEmpty())
			iperfServerTestPods, err = createIperfServerPods(testNodes, cudn.Name, udnv1.NetworkRoleSecondary, serverCIDRs)
			Expect(err).NotTo(HaveOccurred())
			serverIPsByName := podsMultusNetworkIPs(iperfServerTestPods, podNetworkStatusByNetConfigPredicate(namespace, cudn.Name, strings.ToLower(string(udnv1.NetworkRoleSecondary))))
			Expect(serverIPsByName).NotTo(BeEmpty())

			step = by(vmi.Name, "Check TCP connections")
			Expect(startEastWestIperfTraffic(vmi, serverIPsByName, step)).To(Succeed(), step)
			checkEastWestIperfTraffic(vmi, serverIPsByName, step)

			by(vmi.Name, "live-migrate client VM")
			liveMigrateSucceed(vmi)

			step = by(vmi.Name, "Check TCP connections, after successful migration")
			checkEastWestIperfTraffic(vmi, serverIPsByName, step)

			by(vmi.Name, "simulate client VM live-migration failure")
			// Delete the successful migration's VMIM because liveMigrateFailed() expects exactly one migration.
			// TODO: remove this cleanup once the helper fix in https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6981 is incorporated.
			Expect(crClient.DeleteAllOf(context.Background(), &kubevirtv1.VirtualMachineInstanceMigration{}, &crclient.DeleteAllOfOptions{
				ListOptions: crclient.ListOptions{Namespace: vmi.Namespace},
			})).To(Succeed())
			liveMigrateFailed(vmi)

			step = by(vmi.Name, "Check TCP connections, after failed migration")
			checkEastWestIperfTraffic(vmi, serverIPsByName, step)
		},
		Entry("over secondary layer2", udnv1.NetworkTopologyLayer2),
		Entry("over localnet", udnv1.NetworkTopologyLocalnet),
	)

	getIPAMClaimName := func(vmName, netName string) string {
		return fmt.Sprintf("%s.%s", vmName, netName)
	}

	verifyIPAMClaimStatusSuccess := func(ipamClaimName string) {
		Eventually(func(g Gomega) {
			ipamClaim := &ipamclaimsv1alpha1.IPAMClaim{}
			g.Expect(crClient.Get(context.Background(), crclient.ObjectKey{
				Namespace: namespace,
				Name:      ipamClaimName,
			}, ipamClaim)).To(Succeed(), "Should get IPAMClaim")

			g.Expect(ipamClaim.Status.OwnerPod).NotTo(BeNil(), "OwnerPod should be set")
			g.Expect(ipamClaim.Status.OwnerPod.Name).To(HavePrefix("virt-launcher-"), "OwnerPod should be the virt-launcher pod")

			g.Expect(ipamClaim.Status.Conditions).NotTo(BeEmpty(), "Conditions should be set")
			condition := meta.FindStatusCondition(ipamClaim.Status.Conditions, "IPsAllocated")
			g.Expect(condition).NotTo(BeNil(), "IPsAllocated condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue), "Condition status should be True")
			g.Expect(condition.Reason).To(Equal("SuccessfulAllocation"), "Condition reason should be SuccessfulAllocation")

			g.Expect(ipamClaim.Status.IPs).NotTo(BeEmpty(), "IPs should be set on successful allocation")
		}).
			WithTimeout(30*time.Second).
			WithPolling(2*time.Second).
			Should(Succeed(), fmt.Sprintf("IPAMClaim %s should have expected successful status", ipamClaimName))
	}

	verifyIPAMClaimStatusFailure := func(ipamClaimName string, expectedReason string) {
		Eventually(func(g Gomega) {
			ipamClaim := &ipamclaimsv1alpha1.IPAMClaim{}
			g.Expect(crClient.Get(context.Background(), crclient.ObjectKey{
				Namespace: namespace,
				Name:      ipamClaimName,
			}, ipamClaim)).To(Succeed(), "Should get IPAMClaim")

			g.Expect(ipamClaim.Status.OwnerPod).NotTo(BeNil(), "OwnerPod should be set")
			g.Expect(ipamClaim.Status.OwnerPod.Name).To(HavePrefix("virt-launcher-"), "OwnerPod should be the virt-launcher pod")

			g.Expect(ipamClaim.Status.Conditions).NotTo(BeEmpty(), "Conditions should be set")
			condition := meta.FindStatusCondition(ipamClaim.Status.Conditions, "IPsAllocated")
			g.Expect(condition).NotTo(BeNil(), "IPsAllocated condition should exist")
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse), "Condition status should be False")
			g.Expect(condition.Reason).To(Equal(expectedReason), "Condition reason should match")

			g.Expect(ipamClaim.Status.IPs).To(BeEmpty(), "IPs should not be set on failed allocation")
		}).
			WithTimeout(30*time.Second).
			WithPolling(2*time.Second).
			Should(Succeed(), fmt.Sprintf("IPAMClaim %s should have expected failure status", ipamClaimName))
	}

	Context("duplicate addresses validation", func() {
		const networkName = "net1"
		var (
			cudn          *udnv1.ClusterUserDefinedNetwork
			duplicateIPv4 = "10.128.0.200" // Static IP that will be used by both VMs
			duplicateIPv6 = "2010:100:200::200"
			cidrIPv4      = "10.128.0.0/24"
			cidrIPv6      = "2010:100:200::0/60"
		)

		BeforeEach(func(ctx SpecContext) {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ = kubevirt.GenerateCUDN(namespace, networkName, udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(ctx, cudn)
		})

		waitForVMReadinessAndVerifyIPs := func(vmName string, expectedIPs []string) {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			expectedNumberOfAddresses := len(filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)}))
			actualAddresses := virtualMachineAddressesFromStatus(vmi, expectedNumberOfAddresses)
			Expect(actualAddresses).To(ConsistOf(expectedIPs), fmt.Sprintf("VM %s should get the requested static IPs", vmName))
		}

		It("should fail when creating second VM with duplicate static IP", func() {
			staticIPs := filterIPs(fr.ClientSet, duplicateIPv4, duplicateIPv6)

			By("Creating first VM with static IP")
			vm1 := createVMWithStaticIP("test-vm-1", staticIPs)
			createVirtualMachine(vm1)
			waitForVMReadinessAndVerifyIPs(vm1.Name, staticIPs)

			By("Verifying first VM IPAMClaim has successful status")
			verifyIPAMClaimStatusSuccess(getIPAMClaimName(vm1.Name, networkName))

			By("Creating second VM with duplicate static IP - should fail")
			vm2 := createVMWithStaticIP("test-vm-2", staticIPs)
			createVirtualMachine(vm2)

			By("Verifying pod fails with duplicate IP allocation error")
			waitForVMPodErrorEvent(vm2.Name, "provided IP is already allocated")

			By("Verifying second VM IPAMClaim has failure status with IPAddressConflict")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(vm2.Name, networkName), "IPAddressConflict")

			By("Verifying first VM is still running normally")
			waitForVMReadinessAndVerifyIPs(vm1.Name, staticIPs)
		})

		newVMIWithPrimaryIfaceMAC := func(mac string) *kubevirtv1.VirtualMachineInstance {
			vm := fedoraWithTestToolingVMI(nil, nil, nil, kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}, "#", "")
			vm.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			vm.Spec.Domain.Devices.Interfaces[0].MacAddress = mac
			return vm
		}

		It("should fail when creating second VM with duplicate user requested MAC", func() {
			const testMAC = "02:a1:b2:c3:d4:e5"
			vmi1 := newVMIWithPrimaryIfaceMAC(testMAC)
			vm1 := generateVM(vmi1)
			createVirtualMachine(vm1)

			By("Asserting VM with static MAC is running as expected")
			Eventually(func(g Gomega) []kubevirtv1.VirtualMachineInstanceCondition {
				g.Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm1), vmi1)).To(Succeed())
				return vmi1.Status.Conditions
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(ContainElement(SatisfyAll(
				HaveField("Type", kubevirtv1.VirtualMachineInstanceAgentConnected),
				HaveField("Status", corev1.ConditionTrue),
			)))
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm1), vmi1)).To(Succeed())
			Expect(vmi1.Status.Interfaces[0].MAC).To(Equal(testMAC), "vmi status should report the requested mac")

			By("Verifying first VM IPAMClaim has successful status")
			verifyIPAMClaimStatusSuccess(getIPAMClaimName(vm1.Name, networkName))

			By("Create second VM requesting the same MAC address")
			vmi2 := newVMIWithPrimaryIfaceMAC(testMAC)
			vm2 := generateVM(vmi2)
			createVirtualMachine(vm2)

			By("Asserting second VM pod has attached event reflecting MAC conflict error")
			vm2Selector := fmt.Sprintf("%s=%s", kubevirtv1.VirtualMachineNameLabel, vm2.Name)
			Eventually(func(g Gomega) []corev1.Event {
				podList, err := fr.ClientSet.CoreV1().Pods(vm2.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: vm2Selector})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(podList.Items).ToNot(BeEmpty())
				events, err := fr.ClientSet.CoreV1().Events(vm2.Namespace).SearchWithContext(context.Background(), scheme.Scheme, &podList.Items[0])
				g.Expect(err).ToNot(HaveOccurred())
				return events.Items
			}).WithTimeout(time.Minute * 1).WithPolling(time.Second * 3).Should(ContainElement(SatisfyAll(
				HaveField("Type", "Warning"),
				HaveField("Reason", "ErrorAllocatingPod"),
				HaveField("Message", ContainSubstring("MAC address already in use")),
			)))

			By("Verifying second VM IPAMClaim has failure status with MACAddressConflict")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(vm2.Name, networkName), "MACAddressConflict")

			By("Assert second VM not running")
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm2), vmi2)).To(Succeed())
			Expect(vmi2.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", kubevirtv1.VirtualMachineInstanceReady),
				HaveField("Status", corev1.ConditionFalse),
			)), "second VM should not be ready due to MAC conflict")
		})
	})

	Context("IP family validation for layer2 primary networks", func() {
		BeforeEach(func() {
			if !isPreConfiguredUdnAddressesEnabled() {
				Skip("ENABLE_PRE_CONF_UDN_ADDR not configured")
			}

			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.TODO(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name
		})

		It("should fail when dual-stack network requests only IPv4", func(ctx SpecContext) {
			cidrIPv4 := "10.130.0.0/24"
			cidrIPv6 := "2010:100:201::0/60"
			staticIPv4 := "10.130.0.101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(ctx, cudn)

			vm := createVMWithStaticIP("test-vm-dualstack-ipv4-only", []string{staticIPv4})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when dual-stack network requests only IPv6", func(ctx SpecContext) {
			cidrIPv4 := "10.131.0.0/24"
			cidrIPv6 := "2010:100:202::0/60"
			staticIPv6 := "2010:100:202::101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(ctx, cudn)

			vm := createVMWithStaticIP("test-vm-dualstack-ipv6-only", []string{staticIPv6})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when single-stack IPv4 network requests multiple IPv4 IPs", func(ctx SpecContext) {
			cidrIPv4 := "10.132.0.0/24"
			staticIPv4_1 := "10.132.0.101"
			staticIPv4_2 := "10.132.0.102"

			singleStackIPv4CIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4)})

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, singleStackIPv4CIDRs)
			createCUDN(ctx, cudn)

			vm := createVMWithStaticIP("test-vm-ipv4-network-two-ipv4", []string{staticIPv4_1, staticIPv4_2})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should fail when single-stack IPv6 network requests multiple IPv6 IPs", func(ctx SpecContext) {
			cidrIPv6 := "2010:100:204::0/60"
			staticIPv6_1 := "2010:100:204::101"
			staticIPv6_2 := "2010:100:204::102"

			singleStackIPv6CIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv6)})
			if len(singleStackIPv6CIDRs) == 0 {
				Skip("Cluster does not support IPv6")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, singleStackIPv6CIDRs)
			createCUDN(ctx, cudn)

			vm := createVMWithStaticIP("test-vm-ipv6-network-two-ipv6", []string{staticIPv6_1, staticIPv6_2})
			createVirtualMachine(vm)
			waitForVMPodErrorEvent(vm.Name, "requested IPs family types must match network's IP family configuration")
		})

		It("should succeed when dual-stack network requests correct IPs (1 IPv4 + 1 IPv6)", func(ctx SpecContext) {
			cidrIPv4 := "10.134.0.0/24"
			cidrIPv6 := "2010:100:205::0/60"
			staticIPv4 := "10.134.0.101"
			staticIPv6 := "2010:100:205::101"

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			if len(dualCIDRs) < 2 {
				Skip("Cluster does not support dual-stack")
			}

			cudn, _ := kubevirt.GenerateCUDN(namespace, "net1", udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(ctx, cudn)

			staticIPs := filterIPs(fr.ClientSet, staticIPv4, staticIPv6)
			vm := createVMWithStaticIP("test-vm-dualstack-correct", staticIPs)
			createVirtualMachine(vm)

			// VM should start successfully
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vm.Name,
				},
			}
			waitVirtualMachineInstanceReadiness(vmi)
			Expect(crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())

			// Verify it got the requested IPs
			actualAddresses := virtualMachineAddressesFromStatus(vmi, len(staticIPs))
			Expect(actualAddresses).To(ConsistOf(staticIPs), "VM should get the requested static IPs")
		})
	})

	Context("ipv4 subnet exhaustion", func() {
		const networkName = "net1"
		var (
			cudn     *udnv1.ClusterUserDefinedNetwork
			cidrIPv4 = "10.130.0.0/30" // subnet with no usable IPs
			cidrIPv6 = "2011:100:200::0/120"
		)

		BeforeEach(func(ctx SpecContext) {
			l := map[string]string{
				"e2e-framework":           fr.BaseName,
				RequiredUDNNamespaceLabel: "",
			}
			ns, err := fr.CreateNamespace(context.Background(), fr.BaseName, l)
			Expect(err).NotTo(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			dualCIDRs := filterDualStackCIDRs(fr.ClientSet, []udnv1.CIDR{udnv1.CIDR(cidrIPv4), udnv1.CIDR(cidrIPv6)})
			cudn, _ = kubevirt.GenerateCUDN(namespace, networkName, udnv1.NetworkTopologyLayer2, udnv1.NetworkRolePrimary, dualCIDRs)
			createCUDN(ctx, cudn)
		})

		It("should fail when subnet is exhausted", func() {
			By("Creating VM that should fail due to subnet exhaustion")
			exhaustedVMName := "exhausted-vm"
			vm := fedoraWithTestToolingVM(
				nil, // labels
				nil, // no static IP annotations
				nil, // nodeSelector
				kubevirtv1.NetworkSource{
					Pod: &kubevirtv1.PodNetwork{},
				},
				`#cloud-config
password: fedora
chpasswd: { expire: False }
`,
				`version: 2
ethernets:
  eth0:
    dhcp4: true
    dhcp6: true
    ipv6-address-generation: eui64`,
			)
			vm.Name = exhaustedVMName
			vm.Namespace = namespace
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Bridge = nil
			vm.Spec.Template.Spec.Domain.Devices.Interfaces[0].Binding = &kubevirtv1.PluginBinding{Name: "l2bridge"}
			createVirtualMachine(vm)

			By("Verifying pod fails with subnet exhaustion error")
			waitForVMPodErrorEvent(exhaustedVMName, "subnet address pool exhausted")

			By("Verifying VM IPAMClaim has failure status with SubnetExhausted")
			verifyIPAMClaimStatusFailure(getIPAMClaimName(exhaustedVMName, networkName), "SubnetExhausted")
		})
	})
})
