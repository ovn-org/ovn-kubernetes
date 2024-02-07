package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/kubevirt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	butaneconfig "github.com/coreos/butane/config"
	butanecommon "github.com/coreos/butane/config/common"

	kubevirtv1 "kubevirt.io/api/core/v1"
	kvmigrationsv1alpha1 "kubevirt.io/api/migrations/v1alpha1"
)

func newControllerRuntimeClient() (crclient.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	err = kubevirtv1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = kvmigrationsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	return crclient.New(config, crclient.Options{
		Scheme: scheme,
	})
}

var _ = Describe("Kubevirt Virtual Machines", func() {

	var (
		fr                         = wrappedTestFramework("kv-live-migration")
		crClient                   crclient.Client
		namespace                  string
		httpServerPort             = int32(9900)
		isDualStack                = false
		wg                         sync.WaitGroup
		selectedNodes              = []corev1.Node{}
		httpServerTestPods         = []*corev1.Pod{}
		singleConnectionHTTPClient *http.Client
		clientSet                  kubernetes.Interface
		butane                     = `
variant: fcos
version: 1.4.0
systemd:
  units:
    - name: systemd-resolved.service
      mask: true
    - name: replace-resolved.service
      enabled: true
      contents: |
        [Unit]
        Description=Replace systemd resolvd with NetworkManager
        Wants=network-online.target
        After=network-online.target
        [Service]
        ExecStart=rm -f /etc/resolv.conf
        ExecStart=systemctl restart NetworkManager
        Type=oneshot
        [Install]
        WantedBy=multi-user.target
passwd:
  users:
  - name: core
    password_hash: $y$j9T$b7RFf2LW7MUOiF4RyLHKA0$T.Ap/uzmg8zrTcUNXyXvBvT26UgkC6zZUVg3UKXeEp5
`
		labelNode = func(nodeName, label string) error {
			patch := fmt.Sprintf(`{"metadata": {"labels": {"%s": ""}}}`, label)
			_, err := fr.ClientSet.CoreV1().Nodes().Patch(context.Background(), nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			if err != nil {
				return err
			}
			return nil
		}

		unlabelNode = func(nodeName, label string) error {
			patch := fmt.Sprintf(`[{"op": "remove", "path": "/metadata/labels/%s"}]`, label)
			_, err := clientSet.CoreV1().Nodes().Patch(context.Background(), nodeName, types.JSONPatchType, []byte(patch), metav1.PatchOptions{})
			if err != nil {
				return err
			}
			return nil
		}
	)

	BeforeEach(func() {
		namespace = fr.Namespace.Name
		// So we can use it at AfterEach, since fr.ClientSet is nil there
		clientSet = fr.ClientSet

		var err error
		crClient, err = newControllerRuntimeClient()
		Expect(err).ToNot(HaveOccurred())

		workerNodeList, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: labels.FormatLabels(map[string]string{"node-role.kubernetes.io/worker": ""})})
		Expect(err).ToNot(HaveOccurred())
		Expect(workerNodeList.Items).ToNot(BeEmpty())
		hasIPv4Address, hasIPv6Address := false, false
		for _, a := range workerNodeList.Items[0].Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				if utilnet.IsIPv4String(a.Address) {
					hasIPv4Address = true
				}
				if utilnet.IsIPv6String(a.Address) {
					hasIPv6Address = true
				}
			}
		}
		isDualStack = hasIPv4Address && hasIPv6Address

		nodesByOVNZone := map[string][]corev1.Node{}
		for _, workerNode := range workerNodeList.Items {
			ovnZone, ok := workerNode.Labels["k8s.ovn.org/zone-name"]
			if !ok {
				ovnZone = "global"
			}
			_, ok = nodesByOVNZone[ovnZone]
			if !ok {
				nodesByOVNZone[ovnZone] = []corev1.Node{}
			}
			nodesByOVNZone[ovnZone] = append(nodesByOVNZone[ovnZone], workerNode)
		}

		selectedNodes = []corev1.Node{}
		// If there is one global zone select the first three for the
		// migration
		if len(nodesByOVNZone) == 1 {
			selectedNodes = []corev1.Node{
				workerNodeList.Items[0],
				workerNodeList.Items[1],
				workerNodeList.Items[2],
			}
			// Otherwise select a pair of nodes from different OVN zones
		} else {
			for _, nodes := range nodesByOVNZone {
				selectedNodes = append(selectedNodes, nodes[0])
				if len(selectedNodes) == 3 {
					break // we want just three of them
				}
			}
		}

		Expect(selectedNodes).To(HaveLen(3), "at least three nodes in different zones are needed for interconnect scenarios")

		// Label the selected nodes with the generated namespaces, so we can
		// configure VM nodeSelector with it and live migration will take only
		// them into consideration
		for _, node := range selectedNodes {
			Expect(labelNode(node.Name, namespace)).To(Succeed())
		}

		singleConnectionHTTPClient = &http.Client{
			Transport: &http.Transport{
				MaxConnsPerHost: 1,
			},
			Timeout: 2 * time.Second,
		}
	})

	AfterEach(func() {
		for _, node := range selectedNodes {
			unlabelNode(node.Name, namespace)
		}
	})

	type liveMigrationTestData struct {
		mode                kubevirtv1.MigrationMode
		numberOfVMs         int
		shouldExpectFailure bool
	}

	var (
		sendEchoWithConnectionCheck = func(addrs []string, checkConnectionBroken bool) error {
			for _, addr := range addrs {
				connectionBroken := true
				clientTrace := &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) {
						connectionBroken = !info.Reused
					},
				}
				traceCtx := httptrace.WithClientTrace(context.Background(), clientTrace)
				req, err := http.NewRequestWithContext(traceCtx, http.MethodGet, fmt.Sprintf("http://%s/echo?msg=pong", addr), nil)
				if err != nil {
					return err
				}
				res, err := singleConnectionHTTPClient.Do(req)
				if err != nil {
					return err
				}
				if checkConnectionBroken && connectionBroken {
					return fmt.Errorf("http connection to virtual machine was broken")
				}
				//TODO: Check pong
				if _, err := io.Copy(io.Discard, res.Body); err != nil {
					return err
				}
				res.Body.Close()

			}
			return nil
		}

		sendEcho = func(addrs []string) error {
			return sendEchoWithConnectionCheck(addrs, false)
		}

		sendEchoAndCheckConnection = func(addrs []string) error {
			return sendEchoWithConnectionCheck(addrs, true)
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

		createDenyAllPolicy = func(vmName string) (*knet.NetworkPolicy, error) {
			policy := &knet.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "deny-all-" + vmName,
				},
				Spec: knet.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
						kubevirtv1.VirtualMachineNameLabel: vmName,
					}},
					PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress, knet.PolicyTypeIngress},
					Ingress:     []knet.NetworkPolicyIngressRule{},
					Egress:      []knet.NetworkPolicyEgressRule{},
				},
			}
			return fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
		}

		checkConnectivity = func(vmName string, endpoints []string, stage string) {
			by(vmName, "Check connectivity "+stage)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).ToNot(HaveOccurred())
			polling := 15 * time.Second
			timeout := time.Minute
			step := by(vmName, stage+": Check tcp connection is not broken")
			Eventually(func() error { return sendEchoAndCheckConnection(endpoints) }).
				WithPolling(polling).
				WithTimeout(timeout).
				WithOffset(1).
				Should(Succeed(), step)

			step = by(vmName, stage+": Check e/w tcp traffic")
			for _, pod := range httpServerTestPods {
				for _, podIP := range pod.Status.PodIPs {
					output := ""
					Eventually(func() error {
						output, err = kubevirt.RunCommand(vmi, fmt.Sprintf("curl http://%s", net.JoinHostPort(podIP.IP, "8000")), polling)
						return err
					}).
						WithOffset(1).
						WithPolling(polling).
						WithTimeout(timeout).
						Should(Succeed(), func() string { return step + ": " + pod.Name + ": " + output })
				}
			}

			step = by(vmName, stage+": Check n/s tcp traffic")
			output := ""
			Eventually(func() error {
				output, err = kubevirt.RunCommand(vmi, "curl -kL https://www.ovn.org", polling)
				return err
			}).
				WithOffset(1).
				WithPolling(polling).
				WithTimeout(timeout).
				Should(Succeed(), func() string { return step + ": " + output })
		}

		checkConnectivityAndNetworkPolicies = func(vmName string, endpoints []string, stage string) {
			checkConnectivity(vmName, endpoints, stage)
			step := by(vmName, stage+": Create deny all network policy")
			policy, err := createDenyAllPolicy(vmName)
			Expect(err).ToNot(HaveOccurred(), step)

			step = by(vmName, stage+": Check connectivity block after create deny all network policy")
			Eventually(func() error { return sendEcho(endpoints) }).
				WithPolling(time.Second).
				WithTimeout(5*time.Second).
				ShouldNot(Succeed(), step)

			Expect(fr.ClientSet.NetworkingV1().NetworkPolicies(namespace).Delete(context.TODO(), policy.Name, metav1.DeleteOptions{})).To(Succeed())

			// Wait some time for network policy removal to take effect
			time.Sleep(1 * time.Second)

			step = by(vmName, stage+": Check connectivity block after create deny all network policy")
			Expect(sendEcho(endpoints)).To(Succeed(), step)
		}

		composeAgnhostPod = func(name, namespace, nodeName string, args ...string) *v1.Pod {
			agnHostPod := e2epod.NewAgnhostPod(namespace, name, nil, nil, nil, args...)
			agnHostPod.Spec.NodeName = nodeName
			return agnHostPod
		}

		liveMigrateVirtualMachine = func(vmName string, migrationMode kubevirtv1.MigrationMode) {
			vmimCreationRetries := 0
			Eventually(func() error {
				if vmimCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
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
			By("checking the VM live-migrated correctly")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).ToNot(HaveOccurred(), "should success retrieving vmi")
			currentNode := vmi.Status.NodeName

			Eventually(func() *kubevirtv1.VirtualMachineInstanceMigrationState {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(BeNil(), "should have a MigrationState")
			Eventually(func() string {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState.TargetNode
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(10*time.Minute).ShouldNot(Equal(currentNode), "should refresh MigrationState")
			Eventually(func() bool {
				err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
				Expect(err).ToNot(HaveOccurred())
				return vmi.Status.MigrationState.Completed
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(20*time.Minute).Should(BeTrue(), "should complete migration")
			err = crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
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

		checkLiveMigrationFailed = func(vmName string) {
			By("checking the VM live-migrated failed to migrate")
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      vmName,
				},
			}
			err := crClient.Get(context.TODO(), crclient.ObjectKeyFromObject(vmi), vmi)
			Expect(err).ToNot(HaveOccurred(), "should success retrieving vmi")

			Eventually(func() (kubevirtv1.VirtualMachineInstanceMigrationPhase, error) {
				migrations, err := vmiMigrations(crClient)
				if err != nil {
					return kubevirtv1.MigrationPhaseUnset, err
				}
				if len(migrations) > 1 {
					return kubevirtv1.MigrationPhaseUnset, fmt.Errorf("expected one migration, got %d", len(migrations))
				}
				return migrations[0].Status.Phase, nil
			}).WithOffset(1).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(
				Equal(kubevirtv1.MigrationFailed),
			)
		}

		ipv4 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv4.Address
		}

		ipv6 = func(iface kubevirt.Interface) []kubevirt.Address {
			return iface.IPv6.Address
		}

		findNonLoopbackInterface = func(interfaces []kubevirt.Interface) *kubevirt.Interface {
			for _, iface := range interfaces {
				if iface.Name != "lo" {
					return &iface
				}
			}
			return nil
		}

		addressByFamily = func(familyFn func(iface kubevirt.Interface) []kubevirt.Address, vmi *kubevirtv1.VirtualMachineInstance) func() ([]kubevirt.Address, error) {
			return func() ([]kubevirt.Address, error) {
				networkState, err := kubevirt.RetrieveNetworkState(vmi)
				if err != nil {
					return nil, err
				}
				iface := findNonLoopbackInterface(networkState.Interfaces)
				if iface == nil {
					return nil, fmt.Errorf("missing non loopback interface")
				}
				return familyFn(*iface), nil
			}

		}
		fcosVM = func(idx int, labels map[string]string, butane string) (*kubevirtv1.VirtualMachine, error) {
			ignition, _, err := butaneconfig.TranslateBytes([]byte(butane), butanecommon.TranslateBytesOptions{})
			if err != nil {
				return nil, err
			}
			return &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("worker%d", idx),
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					Running: pointer.Bool(true),
					Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								"kubevirt.io/allow-pod-bridge-network-live-migration": "",
							},
							Labels: labels,
						},
						Spec: kubevirtv1.VirtualMachineInstanceSpec{
							NodeSelector: map[string]string{
								namespace: "",
							},
							Domain: kubevirtv1.DomainSpec{
								Resources: kubevirtv1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("512Mi"),
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
											Name: "pod",
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
									Name: "pod",
									NetworkSource: kubevirtv1.NetworkSource{
										Pod: &kubevirtv1.PodNetwork{},
									},
								},
							},
							TerminationGracePeriodSeconds: pointer.Int64(5),
							Volumes: []kubevirtv1.Volume{
								{
									Name: "containerdisk",
									VolumeSource: kubevirtv1.VolumeSource{
										ContainerDisk: &kubevirtv1.ContainerDiskSource{
											Image: "quay.io/kubevirtci/fedora-coreos-kubevirt:v20230905-be4fa50",
										},
									},
								},
								{
									Name: "cloudinitdisk",
									VolumeSource: kubevirtv1.VolumeSource{
										CloudInitConfigDrive: &kubevirtv1.CloudInitConfigDriveSource{
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

		composeVMs = func(numberOfVMs int, labels map[string]string) ([]*kubevirtv1.VirtualMachine, error) {
			vms := []*kubevirtv1.VirtualMachine{}
			for i := 1; i <= numberOfVMs; i++ {
				vm, err := fcosVM(i, labels, butane)
				if err != nil {
					return nil, err
				}
				vms = append(vms, vm)
			}
			return vms, nil
		}
		liveMigrateAndCheck = func(vmName string, migrationMode kubevirtv1.MigrationMode, endpoints []string, step string) {
			liveMigrateVirtualMachine(vmName, migrationMode)
			checkLiveMigrationSucceeded(vmName, migrationMode)
			checkConnectivityAndNetworkPolicies(vmName, endpoints, step)
		}

		runTest = func(td liveMigrationTestData, vm *kubevirtv1.VirtualMachine) {
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
			Expect(err).ToNot(HaveOccurred())
			Expect(kubevirt.LoginToFedora(vmi, "core", "fedora")).To(Succeed(), step)

			step = by(vm.Name, "Wait for virtual machine to receive IPv4 address from DHCP")
			Eventually(addressByFamily(ipv4, vmi)).
				WithPolling(time.Second).
				WithTimeout(5*time.Minute).
				Should(HaveLen(1), step)

			if isDualStack {
				output, err := kubevirt.RunCommand(vmi, `echo '{"interfaces":[{"name":"enp1s0","type":"ethernet","state":"up","ipv4":{"enabled":true,"dhcp":true},"ipv6":{"enabled":true,"dhcp":true,"autoconf":false}}],"routes":{"config":[{"destination":"::/0","next-hop-interface":"enp1s0","next-hop-address":"fe80::1"}]}}' |nmstatectl apply`, 5*time.Second)
				Expect(err).ToNot(HaveOccurred(), output)
				step = by(vm.Name, "Wait for virtual machine to receive IPv6 address from DHCP")
				Eventually(addressByFamily(ipv6, vmi)).
					WithPolling(time.Second).
					WithTimeout(5*time.Minute).
					Should(HaveLen(2), func() string {
						output, _ := kubevirt.RunCommand(vmi, "journalctl -u nmstate", 2*time.Second)
						return step + " -> journal nmstate: " + output
					})
			}

			step = by(vm.Name, "Start httpServer")
			httpServerCommand := fmt.Sprintf("podman run -d --tls-verify=false --privileged --net=host %s netexec --http-port %d", agnhostImage, httpServerPort)
			output, err := kubevirt.RunCommand(vmi, httpServerCommand, time.Minute)
			Expect(err).ToNot(HaveOccurred(), step+": "+output)

			step = by(vm.Name, "Expose httpServer as a service")
			svc, err := fr.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), composeService("httpserver", vm.Name, httpServerPort), metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred(), step)
			endpoints, err := serviceEndpoints(svc)
			Expect(err).ToNot(HaveOccurred(), step)

			step = by(vm.Name, "Send first echo to populate connection pool")
			Eventually(func() error { return sendEcho(endpoints) }).
				WithPolling(1*time.Second).
				WithTimeout(5*time.Second).
				Should(Succeed(), step)

			checkConnectivityAndNetworkPolicies(vm.Name, endpoints, "before live migration")
			// Do just one migration that will fail
			if td.shouldExpectFailure {
				by(vm.Name, fmt.Sprintf("Live migrate virtual machine to check failed migration"))
				liveMigrateVirtualMachine(vm.Name, td.mode)
				checkLiveMigrationFailed(vm.Name)
				checkConnectivityAndNetworkPolicies(vm.Name, endpoints, "after live migrate to check failed migration")
			} else {
				originalNode := vmi.Status.NodeName
				by(vm.Name, fmt.Sprintf("Live migrate for the first time"))
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migrate for the first time")

				by(vm.Name, fmt.Sprintf("Live migrate for the second time to a node not owning the subnet"))
				// Remove the node selector label from original node to force
				// live migration to a different one.
				Expect(unlabelNode(originalNode, namespace)).To(Succeed())
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migration for the second time to node not owning subnet")

				by(vm.Name, fmt.Sprintf("Live migrate for the third time to the node owning the subnet"))
				// Patch back the original node with the label and remove it
				// from the rest of nodes to force live migration target to it.
				Expect(labelNode(originalNode, namespace)).To(Succeed())
				for _, selectedNode := range selectedNodes {
					if selectedNode.Name != originalNode {
						Expect(unlabelNode(selectedNode.Name, namespace)).To(Succeed())
					}
				}
				liveMigrateAndCheck(vm.Name, td.mode, endpoints, "after live migration to node owning the subnet")
			}

		}
	)
	DescribeTable("when live migration", func(td liveMigrationTestData) {
		if td.mode == kubevirtv1.MigrationPostCopy && os.Getenv("GITHUB_ACTIONS") == "true" {
			Skip("Post copy live migration not working at github actions")
		}
		var (
			err error
		)

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
		if td.mode == kubevirtv1.MigrationPostCopy {
			err = crClient.Create(context.TODO(), forcePostCopyMigrationPolicy)
			Expect(err).ToNot(HaveOccurred())
			defer func() {
				Expect(crClient.Delete(context.TODO(), forcePostCopyMigrationPolicy)).To(Succeed())
			}()
		}

		By("Creating a test pod at all worker nodes")
		for _, selectedNode := range selectedNodes {
			httpServerWorkerNode := composeAgnhostPod(
				"testpod-"+selectedNode.Name,
				namespace,
				selectedNode.Name,
				"netexec", "--http-port", "8000")
			_ = e2epod.NewPodClient(fr).CreateSync(context.TODO(), httpServerWorkerNode)
		}

		By("Waiting until both pods have an IP address")
		for _, httpServerTestPod := range httpServerTestPods {
			Eventually(func() error {
				var err error
				httpServerTestPod, err = fr.ClientSet.CoreV1().Pods(fr.Namespace.Name).Get(context.TODO(), httpServerTestPod.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if httpServerTestPod.Status.PodIP == "" {
					return fmt.Errorf("pod %s has no valid IP address yet", httpServerTestPod.Name)
				}
				return nil
			}).
				WithTimeout(time.Minute).
				WithPolling(time.Second).
				Should(Succeed())
		}

		vmLabels := map[string]string{}
		if td.mode == kubevirtv1.MigrationPostCopy {
			vmLabels = forcePostCopyMigrationPolicy.Spec.Selectors.VirtualMachineInstanceSelector
		}
		vms, err := composeVMs(td.numberOfVMs, vmLabels)
		Expect(err).ToNot(HaveOccurred())

		for _, vm := range vms {
			By(fmt.Sprintf("Create virtual machine %s", vm.Name))
			vmCreationRetries := 0
			Eventually(func() error {
				if vmCreationRetries > 0 {
					// retry due to unknown issue where kubevirt webhook gets stuck reading the request body
					// https://github.com/ovn-org/ovn-kubernetes/issues/3902#issuecomment-1750257559
					By(fmt.Sprintf("Retrying vm %s creation", vm.Name))
				}
				err = crClient.Create(context.Background(), vm)
				vmCreationRetries++
				return err
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())
		}

		if td.shouldExpectFailure {
			By("annotating the VMI with `fail fast`")
			vmKey := types.NamespacedName{Namespace: namespace, Name: "worker1"}
			var vmi kubevirtv1.VirtualMachineInstance
			Eventually(func() error {
				return crClient.Get(context.TODO(), vmKey, &vmi)
			}).WithPolling(time.Second).WithTimeout(time.Minute).Should(Succeed())

			vmi.ObjectMeta.Annotations[kubevirtv1.FuncTestLauncherFailFastAnnotation] = "true"

			Expect(crClient.Update(context.TODO(), &vmi)).To(Succeed())
		}

		for _, vm := range vms {
			By(fmt.Sprintf("Waiting for readiness at virtual machine %s", vm.Name))
			Eventually(func() bool {
				err = crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vm), vm)
				Expect(err).ToNot(HaveOccurred())
				return vm.Status.Ready
			}).WithPolling(time.Second).WithTimeout(5 * time.Minute).Should(BeTrue())
		}

		wg.Add(int(td.numberOfVMs))
		for _, vm := range vms {
			go runTest(td, vm)
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

func vmiMigrations(client crclient.Client) ([]kubevirtv1.VirtualMachineInstanceMigration, error) {
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
