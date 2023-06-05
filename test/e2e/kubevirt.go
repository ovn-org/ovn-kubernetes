package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/kubevirt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	"k8s.io/utils/pointer"

	butaneconfig "github.com/coreos/butane/config"
	butanecommon "github.com/coreos/butane/config/common"

	kvv1 "kubevirt.io/api/core/v1"
	kvmigrationsv1alpha1 "kubevirt.io/api/migrations/v1alpha1"
	"kubevirt.io/client-go/kubecli"
)

func newKubevirtClient() (kubecli.KubevirtClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	clientSet, err := kubecli.GetKubevirtClientFromRESTConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unexpected error creating kubevirt client: %v", err)
	}
	return clientSet, nil
}

var _ = Describe("Kubevirt Virtual Machines", func() {

	var (
		fr                     = wrappedTestFramework("kv-live-migration")
		kvcli                  kubecli.KubevirtClient
		podWorker1, podWorker2 *corev1.Pod
		namespace              string
		localRegistryPort      = "5000"
		tcprobePort            = int32(9900)
		isDualStack            = false
		wg                     sync.WaitGroup
	)

	BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(fr.ClientSet, 3)
		Expect(err).ToNot(HaveOccurred())
		numberOfNodeInternalAddresses := 0
		for _, a := range nodes.Items[0].Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				numberOfNodeInternalAddresses++
			}
		}
		isDualStack = numberOfNodeInternalAddresses > 1
	})

	var (
		dialTCPRobe = func(addrs []string) ([]net.Conn, error) {
			tcpProbeConns := []net.Conn{}
			for _, addr := range addrs {
				tcpProbeConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					return nil, fmt.Errorf("Unable to dial to server: %v", err)
				}
				tcpProbeConns = append(tcpProbeConns, tcpProbeConn)
			}
			return tcpProbeConns, nil
		}

		sendPing = func(tcpProbeConns []net.Conn, timeout time.Duration) error {
			for _, tcpProbeConn := range tcpProbeConns {
				_, err := fmt.Fprintf(tcpProbeConn, "ping\n")
				if err != nil {
					return fmt.Errorf("Unable to send msg: %v", err)
				}

				tcpProbeConn.SetReadDeadline(time.Now().Add(timeout))
				msg, err := bufio.NewReader(tcpProbeConn).ReadString('\n')
				if err != nil {
					return fmt.Errorf("Unable to read from server: %v", err)
				}
				msg = strings.TrimSuffix(msg, "\n")
				if msg != "pong" {
					return fmt.Errorf("Received unexpected server message: %s", msg)
				}
				time.Sleep(time.Second)
			}
			return nil
		}

		serviceEndpoints = func(svc *corev1.Service) ([]string, error) {
			worker, err := fr.ClientSet.CoreV1().Nodes().Get(context.TODO(), "ovn-worker", metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			endpoints := []string{}
			for _, address := range worker.Status.Addresses {
				if address.Type != corev1.NodeHostName {
					endpoints = append(endpoints, net.JoinHostPort(address.Address, fmt.Sprintf("%d", svc.Spec.Ports[0].NodePort)))
				}
			}
			return endpoints, nil
		}

		discoverRegistryEndpoint = func() (string, error) {
			cmd := exec.Command("docker", "inspect", `-f='{{json .NetworkSettings.Networks.kind.IPAddress}}'`, "kind-registry")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				return "", fmt.Errorf("%s: %s: %v", stdout.String(), stderr.String(), err)
			}
			address := strings.TrimSpace(stdout.String())
			address = strings.ReplaceAll(address, `"`, "")
			address = strings.ReplaceAll(address, "'", "")
			return address + ":" + localRegistryPort, nil
		}

		deployTCProbe = func() error {
			image := "127.0.0.1:" + localRegistryPort + "/tcprobe"
			buildCmd := exec.Command("podman", "build", "../tools/tcprobe", "-t", image)
			pushCmd := exec.Command("podman", "push", "--tls-verify=false", image)
			for _, cmd := range []*exec.Cmd{buildCmd, pushCmd} {
				output, err := cmd.CombinedOutput()
				if err != nil {
					return fmt.Errorf("%s: %v", output, err)
				}
			}
			return nil
		}

		composeService = func(name, vmName string, port int32) *corev1.Service {
			ipFamilyPolicy := corev1.IPFamilyPolicyPreferDualStack
			return &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "tcprobe" + vmName,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port: port,
					}},
					Selector: map[string]string{
						kvv1.VirtualMachineNameLabel: vmName,
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

		checkConnectivityShould = func(matchResult types.GomegaMatcher, vmName string, tcpProbeConns []net.Conn) {
			vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			polling := 2 * time.Second
			timeout := time.Minute

			step := by(vmName, "Check opened tcp connection")
			Eventually(func() error { return sendPing(tcpProbeConns, polling) }).
				WithPolling(polling).
				WithTimeout(timeout).
				WithOffset(1).
				Should(matchResult, step)

			step = by(vmName, "Check e/w tcp traffic")
			for _, pod := range []*corev1.Pod{podWorker1, podWorker2} {
				for _, podIP := range pod.Status.PodIPs {
					output := ""
					Eventually(func() error {
						output, err = kubevirt.RunCommand(kvcli, vmi, fmt.Sprintf("curl http://%s", net.JoinHostPort(podIP.IP, "8000")), polling)
						return err
					}).
						WithOffset(1).
						WithPolling(polling).
						WithTimeout(timeout).
						Should(matchResult, func() string { return step + ": " + pod.Name + ": " + output })
				}
			}

			step = by(vmName, "Check n/s tcp traffic")
			output := ""
			Eventually(func() error {
				output, err = kubevirt.RunCommand(kvcli, vmi, "curl -kL https://www.ovn.org", polling)
				return err
			}).
				WithOffset(1).
				WithPolling(polling).
				WithTimeout(timeout).
				Should(matchResult, func() string { return step + ": " + output })
		}

		createDenyAllPolicy = func() (*knet.NetworkPolicy, error) {
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "deny-all",
				},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress, knet.PolicyTypeIngress},
					Ingress:     []knet.NetworkPolicyIngressRule{},
					Egress:      []knet.NetworkPolicyEgressRule{},
				},
			}
			return fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
		}

		checkConnectivity = func(vmName string, tcpProbeConns []net.Conn) {
			by(vmName, "Check connectivity is fine")
			checkConnectivityShould(Succeed(), vmName, tcpProbeConns)

			step := by(vmName, "Create deny all network policy")
			policy, err := createDenyAllPolicy()
			Expect(err).ToNot(HaveOccurred(), step)

			by(vmName, "Check connectivity block after create deny all network policy")
			checkConnectivityShould(Not(Succeed()), vmName, tcpProbeConns)

			Expect(fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Delete(context.TODO(), policy.Name, metav1.DeleteOptions{})).To(Succeed())

			by(vmName, "Check connectivity is fine after deleting network policy")
			checkConnectivityShould(Succeed(), vmName, tcpProbeConns)
		}

		composeAgnhostPod = func(name, namespace, nodeName string, command ...string) *v1.Pod {
			return &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					Containers: []v1.Container{
						{
							Name:    name,
							Image:   agnhostImage,
							Command: command,
						},
					},
					RestartPolicy: v1.RestartPolicyNever,
				},
			}
		}

		liveMigrateVirtualMachine = func(vmName string, migrationMode kvv1.MigrationMode) {
			vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred(), "should success retrieving vmi")
			currentNode := vmi.Status.NodeName

			Expect(kvcli.VirtualMachine(namespace).Migrate(vmName, &kvv1.MigrateOptions{})).WithOffset(1).To(Succeed())
			Eventually(func() *kvv1.VirtualMachineInstanceMigrationState {
				vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(BeNil(), "should have a MigrationState")
			Eventually(func() string {
				vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState.TargetNode
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(Equal(currentNode), "should refresh MigrationState")
			Eventually(func() bool {
				vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState.Completed
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(20*time.Minute).Should(BeTrue(), "should complete migration")
			vmi, err = kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vmName, &metav1.GetOptions{})
			Expect(err).WithOffset(1).ToNot(HaveOccurred(), "should success retrieving vmi after migration")
			Expect(vmi.Status.MigrationState.Failed).WithOffset(1).To(BeFalse(), func() string {
				vmiJSON, err := json.Marshal(vmi)
				if err != nil {
					return fmt.Sprintf("failed marshaling migrated VM: %v", vmiJSON)
				}
				return fmt.Sprintf("should live migrate successfully: %s", string(vmiJSON))
			})
			Expect(vmi.Status.MigrationState.Mode).WithOffset(1).To(Equal(migrationMode), "should be the expected migration mode %s", migrationMode)
		}

		ipv4 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv4.Address
		}

		ipv6 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv6.Address
		}

		addressByFamily = func(familyFn func(iface kubevirt.Interface) []kubevirt.Address, virtClient kubecli.KubevirtClient, vmi *kvv1.VirtualMachineInstance) func() ([]kubevirt.Address, error) {
			return func() ([]kubevirt.Address, error) {
				networkState, err := kubevirt.RetrieveNetworkState(kvcli, vmi)
				if err != nil {
					return nil, err
				}
				for _, iface := range networkState.Interfaces {
					if iface.Name == "enp1s0" {
						return familyFn(iface), nil
					}
				}
				return []kubevirt.Address{}, nil
			}

		}

		composeVM = func(idx uint, labels map[string]string) (*kvv1.VirtualMachine, error) {
			butaneIPv4 := `
variant: fcos
version: 1.4.0
passwd:
  users:
  - name: core
    password_hash: $y$j9T$b7RFf2LW7MUOiF4RyLHKA0$T.Ap/uzmg8zrTcUNXyXvBvT26UgkC6zZUVg3UKXeEp5
`

			butaneDualSack := `
variant: fcos
version: 1.4.0
storage:
  files:
    - path: /etc/nmstate/001-dual-stack-dhcp.yml
      contents:
        inline: | 
          interfaces:
          - name: enp1s0
            type: ethernet
            state: up
            ipv4:
              enabled: true
              dhcp: true
            ipv6:
              enabled: true
              dhcp: true
              autoconf: false
    - path: /etc/nmstate/002-dual-sack-ipv6-gw.yml
      contents:
        inline: | 
          routes:
            config:
            - destination: ::/0
              next-hop-interface: enp1s0
              next-hop-address: d7b:6b4d:7b25:d22f::1
passwd:
  users:
  - name: core
    password_hash: $y$j9T$b7RFf2LW7MUOiF4RyLHKA0$T.Ap/uzmg8zrTcUNXyXvBvT26UgkC6zZUVg3UKXeEp5

`
			butane := butaneIPv4
			if isDualStack {
				butane = butaneDualSack
			}

			ignition, _, err := butaneconfig.TranslateBytes([]byte(butane), butanecommon.TranslateBytesOptions{})
			if err != nil {
				return nil, err
			}
			return &kvv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("worker%d", idx),
				},
				Spec: kvv1.VirtualMachineSpec{
					Running: pointer.Bool(true),
					Template: &kvv1.VirtualMachineInstanceTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								"kubevirt.io/allow-pod-bridge-network-live-migration": "",
								"k8s.ovn.org/pod-networks":                            `{"default": {"skip_ip_config": true}}`,
							},
							Labels: labels,
						},
						Spec: kvv1.VirtualMachineInstanceSpec{
							Domain: kvv1.DomainSpec{
								Resources: kvv1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
								Devices: kvv1.Devices{
									Disks: []kvv1.Disk{
										{
											DiskDevice: kvv1.DiskDevice{
												Disk: &kvv1.DiskTarget{
													Bus: kvv1.DiskBusVirtio,
												},
											},
											Name: "containerdisk",
										},
										{
											DiskDevice: kvv1.DiskDevice{
												Disk: &kvv1.DiskTarget{
													Bus: kvv1.DiskBusVirtio,
												},
											},
											Name: "cloudinitdisk",
										},
									},
									Interfaces: []kvv1.Interface{
										{
											Name: "pod",
											InterfaceBindingMethod: kvv1.InterfaceBindingMethod{
												Bridge: &kvv1.InterfaceBridge{},
											},
										},
									},
									Rng: &kvv1.Rng{},
								},
							},
							Networks: []kvv1.Network{
								{
									Name: "pod",
									NetworkSource: kvv1.NetworkSource{
										Pod: &kvv1.PodNetwork{},
									},
								},
							},
							TerminationGracePeriodSeconds: pointer.Int64(5),
							Volumes: []kvv1.Volume{
								{
									Name: "containerdisk",
									VolumeSource: kvv1.VolumeSource{
										ContainerDisk: &kvv1.ContainerDiskSource{
											Image: "quay.io/fedora/fedora-coreos-kubevirt:stable",
										},
									},
								},
								{
									Name: "cloudinitdisk",
									VolumeSource: kvv1.VolumeSource{
										CloudInitConfigDrive: &kvv1.CloudInitConfigDriveSource{
											UserData: string(ignition),
										},
									},
								},
							},
						},
					},
				},
			}, nil
		}

		composeVMs = func(numberOfVMs uint, labels map[string]string) ([]*kvv1.VirtualMachine, error) {
			vms := []*kvv1.VirtualMachine{}
			for i := uint(1); i <= numberOfVMs; i++ {
				vm, err := composeVM(i, labels)
				if err != nil {
					return nil, err
				}
				vms = append(vms, vm)
			}
			return vms, nil
		}

		runTest = func(vm *kvv1.VirtualMachine, registryEndpoint string, migrationMode kvv1.MigrationMode) {
			defer GinkgoRecover()
			defer wg.Done()
			by(vm.Name, "Login to virtual machine")
			vmi, err := kvcli.VirtualMachineInstance(namespace).Get(context.TODO(), vm.Name, &metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(kubevirt.LoginToFedora(kvcli, vmi, "core", "fedora")).To(Succeed())

			by(vm.Name, "Wait for virtual machine to receive IPv4 address from DHCP")
			Eventually(addressByFamily(ipv4, kvcli, vmi)).
				WithPolling(time.Second).
				WithTimeout(5*time.Minute).
				Should(HaveLen(1), "should have an ipv4 address assigned")

			if isDualStack {
				Eventually(addressByFamily(ipv6, kvcli, vmi)).
					WithPolling(time.Second).
					WithTimeout(5*time.Minute).
					Should(HaveLen(2), "should have an ipv6 address assigned")
			}

			by(vm.Name, "Start tcprobe")
			tcprobeCommand := fmt.Sprintf("podman run -d --tls-verify=false --privileged --net=host %s/tcprobe s 0.0.0.0:%d", registryEndpoint, tcprobePort)
			output, err := kubevirt.RunCommand(kvcli, vmi, tcprobeCommand, time.Minute)
			Expect(err).ToNot(HaveOccurred(), output)

			by(vm.Name, "Expose tcprobe as a service")
			svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("tcprobe", vm.Name, tcprobePort), metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			endpoints, err := serviceEndpoints(svc)
			Expect(err).ToNot(HaveOccurred())

			by(vm.Name, "Wait for tcprobe readiness and connect to it")
			time.Sleep(10 * time.Second)
			var tcpProbeConns []net.Conn
			Eventually(func() error {
				tcpProbeConns, err = dialTCPRobe(endpoints)
				return err
			}).
				WithPolling(1 * time.Second).
				WithTimeout(5 * time.Second).
				Should(Succeed())
			defer func() {
				for _, tcpProbeConn := range tcpProbeConns {
					tcpProbeConn.Close()
				}
			}()

			by(vm.Name, "Check connectivity before live migration")
			checkConnectivity(vm.Name, tcpProbeConns)

			by(vm.Name, "Live migrate virtual machine")
			liveMigrateVirtualMachine(vm.Name, migrationMode)

			by(vm.Name, "Check connectivity after live migration")
			checkConnectivity(vm.Name, tcpProbeConns)

			by(vm.Name, "Live migrate virtual machine again to return it to original node")
			liveMigrateVirtualMachine(vm.Name, migrationMode)

			by(vm.Name, "Check connectivity after live migration to original node")
			checkConnectivity(vm.Name, tcpProbeConns)
		}
	)

	type liveMigrationTestData struct {
		mode        kvv1.MigrationMode
		numberOfVMs uint
	}
	DescribeTable("when live migrated", func(td liveMigrationTestData) {
		if td.mode == kvv1.MigrationPostCopy && os.Getenv("GITHUB_ACTIONS") == "true" {
			Skip("Post copy live migration not working at github actions")
		}
		var (
			err error
		)

		namespace = fr.Namespace.Name

		kvcli, err = newKubevirtClient()
		Expect(err).ToNot(HaveOccurred())

		bandwidthPerMigration := resource.MustParse("40Mi")
		forcePostCopyMigrationPolicy := &kvmigrationsv1alpha1.MigrationPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "force-post-copy",
			},
			Spec: kvmigrationsv1alpha1.MigrationPolicySpec{
				AllowPostCopy:           pointer.Bool(true),
				CompletionTimeoutPerGiB: pointer.Int64(1),
				BandwidthPerMigration:   &bandwidthPerMigration,
				Selectors: &kvmigrationsv1alpha1.Selectors{
					VirtualMachineInstanceSelector: kvmigrationsv1alpha1.LabelSelector{
						"test-live-migration": "post-copy",
					},
				},
			},
		}
		if td.mode == kvv1.MigrationPostCopy {
			_, err = kvcli.MigrationPolicy().Create(context.TODO(), forcePostCopyMigrationPolicy, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			defer func() {
				Expect(kvcli.MigrationPolicy().Delete(context.TODO(), forcePostCopyMigrationPolicy.Name, metav1.DeleteOptions{})).To(Succeed())
			}()
		}

		Expect(deployTCProbe()).To(Succeed())

		registryEndpoint, err := discoverRegistryEndpoint()
		Expect(err).ToNot(HaveOccurred())

		By("Creating a test pod on both selected worker nodes")
		podWorker1 = composeAgnhostPod(
			"testpod-ovn-worker",
			namespace,
			"ovn-worker",
			"/bin/bash", "-c", "/agnhost netexec --http-port 8000")
		podWorker2 = composeAgnhostPod(
			"testpod-ovn-worker2",
			namespace,
			"ovn-worker2",
			"/bin/bash", "-c", "/agnhost netexec --http-port 8000")
		_ = fr.PodClient().CreateSync(podWorker1)
		_ = fr.PodClient().CreateSync(podWorker2)

		By("Waiting until both pods have an IP address")
		Eventually(func() error {
			var err error
			podWorker1, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), podWorker1.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if podWorker1.Status.PodIP == "" {
				return fmt.Errorf("pod %s has no valid IP address yet", podWorker1.Name)
			}
			podWorker2, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), podWorker2.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if podWorker2.Status.PodIP == "" {
				return fmt.Errorf("pod %s has no valid IP address yet", podWorker2.Name)
			}
			return nil
		}).
			WithTimeout(time.Minute).
			WithPolling(time.Second).
			Should(Succeed())

		By("Create virtual machines")
		vmLabels := map[string]string{}
		if td.mode == kvv1.MigrationPostCopy {
			vmLabels = forcePostCopyMigrationPolicy.Spec.Selectors.VirtualMachineInstanceSelector
		}
		vms, err := composeVMs(td.numberOfVMs, vmLabels)
		Expect(err).ToNot(HaveOccurred())
		for _, vm := range vms {
			vm, err = kvcli.VirtualMachine(namespace).Create(vm)
			Expect(err).ToNot(HaveOccurred())
		}
		for _, vm := range vms {
			Eventually(func() bool {
				vm, err = kvcli.VirtualMachine(namespace).Get(vm.Name, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vm.Status.Ready
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(BeTrue())
		}

		wg.Add(int(td.numberOfVMs))
		for _, vm := range vms {
			go runTest(vm, registryEndpoint, td.mode)
		}
		wg.Wait()
	},
		Entry("with pre-copy should keep connectivity", liveMigrationTestData{
			mode:        kvv1.MigrationPreCopy,
			numberOfVMs: 1,
		}),
		Entry("with post-copy should keep connectivity", liveMigrationTestData{
			mode:        kvv1.MigrationPostCopy,
			numberOfVMs: 1,
		}),
	)
})
