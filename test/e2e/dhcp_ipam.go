// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/kubevirt"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpclient "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1beta1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

// Localnet DHCP IPAM covers localnet (C)UDNs with ipam.mode: DHCP (OKEP-6224),
// where OVN-Kubernetes allocates no addresses and addressing is delegated to
// an external DHCP server: regular pods via the delegated dhcp CNI IPAM
// plugin, KubeVirt VMs via a one-shot exchange at CNI ADD with the guest
// owning the lease. The suite runs in the kv-live-migration CI lane, which
// installs the dhcp CNI plugin on the nodes (KIND_INSTALL_PLUGINS) and
// enables NetworkQoS; all other lanes skip it via the Feature:DHCPIPAM label
// gate in test/scripts/e2e-cp.sh.
var _ = Describe("Localnet DHCP IPAM", feature.DHCPIPAM, func() {
	var (
		fr          = wrappedTestFramework("dhcp-ipam")
		crClient    crclient.Client
		virtClient  *kubevirt.Client
		providerCtx infraapi.Context
		namespace   string
	)

	// namespaces are created explicitly below, matching the kubevirt suite
	// this Context originated from
	fr.SkipNamespaceCreation = true

	BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()

		var err error
		crClient, err = newControllerRuntimeClient()
		Expect(err).NotTo(HaveOccurred())

		virtClient, err = kubevirt.NewClient("/tmp")
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with user defined networks with DHCP IPAM localnet topology", Ordered, func() {
		const (
			dhcpServerIP = "172.31.100.1"
			dhcpRange    = "172.31.100.10,172.31.100.100,255.255.255.0,12h"
			dhcpSubnet   = "172.31.100.0/24"
			dhcpUserData = `#cloud-config
password: fedora
chpasswd: { expire: False }
`
			// guest runs its own DHCP client on its single (localnet) interface
			dhcpNetworkData = `version: 2
ethernets:
  eth0:
    dhcp4: true`
		)

		var (
			cudn      *udnv1.ClusterUserDefinedNetwork
			nadKey    string
			mnpClient mnpclient.K8sCniCncfIoV1beta1Interface
		)

		// runDHCPServer starts a dnsmasq container on the underlay network,
		// serving leases from dhcpRange with dhcpServerIP as the server
		// address. The provider context deletes the container on cleanup.
		runDHCPServer := func(namespace string, network infraapi.Network) error {
			_, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
				Name:       namespace + "-dhcp-server",
				Image:      images.DNSMasq(),
				Network:    network,
				Entrypoint: "sh",
				CmdArgs: []string{"-c", fmt.Sprintf(
					"ip addr add %s/24 dev eth0 && exec dnsmasq --no-daemon --interface=eth0 --dhcp-range=%s --log-dhcp",
					dhcpServerIP, dhcpRange)},
			})
			return err
		}

		BeforeEach(func(ctx SpecContext) {
			ns, err := fr.CreateNamespace(context.Background(), fr.BaseName, map[string]string{
				"e2e-framework": fr.BaseName,
			})
			Expect(err).ToNot(HaveOccurred())
			fr.Namespace = ns
			namespace = fr.Namespace.Name

			mnpClient, err = mnpclient.NewForConfig(fr.ClientConfig())
			Expect(err).NotTo(HaveOccurred())

			By("creating a localnet CUDN with DHCP IPAM")
			var networkName string
			cudn, networkName = kubevirt.GenerateCUDN(namespace, "dhcpnet",
				udnv1.NetworkTopologyLocalnet, udnv1.NetworkRoleSecondary, udnv1.DualStackCIDRs{})
			// GenerateCUDN defaults subnet-less networks to IPAMDisabled; DHCP
			// delegates addressing to the external server instead
			cudn.Spec.Network.Localnet.IPAM.Mode = udnv1.IPAMDHCP
			createCUDNWithClients(ctx, crClient, fr.DynamicClient, cudn)
			nadKey = namespace + "/" + cudn.Name

			By("setting up the localnet underlay")
			Expect(providerCtx.SetupUnderlay(fr, infraapi.Underlay{LogicalNetworkName: networkName})).To(Succeed())

			By("starting a dnsmasq DHCP server on the underlay")
			underlayNetwork, err := infraprovider.Get().GetNetwork("underlay")
			Expect(err).NotTo(HaveOccurred(), "must get underlay network")
			Expect(runDHCPServer(fr.Namespace.Name, underlayNetwork)).To(Succeed(),
				"must create the dnsmasq container")
		})

		AfterAll(func() {
			Expect(removeImagesFromNodes(fr.ClientSet, images.FedoraKubevirtContainerDisk())).To(Succeed())
		})

		// startVM boots a fedora VM attached to the DHCP CUDN. The role label
		// is set on the VMI template so it propagates to the virt-launcher
		// pod, which is what MultiNetworkPolicy / NetworkQoS pod selectors
		// match.
		startVM := func(name, role string) *kubevirtv1.VirtualMachineInstance {
			vm := composeFedoraWithTestToolingVM(namespace, map[string]string{"role": role}, nil, nil,
				kubevirtv1.NetworkSource{
					Multus: &kubevirtv1.MultusNetwork{NetworkName: cudn.Name},
				}, dhcpUserData, dhcpNetworkData)
			vm.Name = name
			createVirtualMachineWithClient(crClient, vm)
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: vm.Name},
			}
			waitForVMIReadinessWithClient(crClient, vmi, kubevirtv1.VirtualMachineInstanceReady, corev1.ConditionTrue)
			Expect(crClient.Get(context.Background(), crclient.ObjectKeyFromObject(vmi), vmi)).To(Succeed())
			return vmi
		}

		// guestDHCPAddress waits for the guest's own DHCP client to obtain a
		// lease from dnsmasq and returns the address.
		guestDHCPAddress := func(vmi *kubevirtv1.VirtualMachineInstance) string {
			Eventually(func() error { return virtClient.LoginToFedora(vmi, "fedora", "fedora") }).
				WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			output, err := virtClient.RunCommand(vmi, "cloud-init status --wait", 3*time.Minute)
			Expect(err).NotTo(HaveOccurred(), output)
			_, dhcpIPNet, err := net.ParseCIDR(dhcpSubnet)
			Expect(err).NotTo(HaveOccurred())
			ip := ""
			Eventually(func() bool {
				output, _ := virtClient.RunCommand(vmi,
					"ip -4 -o addr show dev eth0 scope global | awk '{print $4}' | cut -d/ -f1", 10*time.Second)
				ip = strings.TrimSpace(output)
				return dhcpIPNet.Contains(net.ParseIP(ip))
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).
				Should(BeTrue(), "guest must obtain a lease from the dnsmasq subnet "+dhcpSubnet)
			return ip
		}

		launcherPodFor := func(vmi *kubevirtv1.VirtualMachineInstance) *corev1.Pod {
			pods, err := fr.ClientSet.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: "vm.kubevirt.io/name=" + vmi.Name,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty())
			return &pods.Items[0]
		}

		// launcherPodAnnotationIP returns the address the CNI one-shot DORA
		// patched into the launcher pod's pod-networks annotation.
		launcherPodAnnotationIP := func(vmi *kubevirtv1.VirtualMachineInstance) string {
			annotation := launcherPodFor(vmi).Annotations["k8s.ovn.org/pod-networks"]
			podNetworks := map[string]struct {
				IPAddresses []string `json:"ip_addresses"`
			}{}
			Expect(json.Unmarshal([]byte(annotation), &podNetworks)).To(Succeed())
			Expect(podNetworks).To(HaveKey(nadKey))
			Expect(podNetworks[nadKey].IPAddresses).To(HaveLen(1))
			ip, _, err := net.ParseCIDR(podNetworks[nadKey].IPAddresses[0])
			Expect(err).NotTo(HaveOccurred())
			return ip.String()
		}

		// startDHCPDaemons runs the containernetworking dhcp IPAM daemon on
		// every node: regular-pod attachments delegate lease handling to it
		// (VMs use the one-shot exchange and need no daemon). The binary is
		// installed by kind.sh --install-cni-plugins (KIND_INSTALL_PLUGINS in
		// CI); without it the spec is skipped rather than failed. The daemon
		// is stopped when the spec ends.
		startDHCPDaemons := func() {
			GinkgoHelper()
			nodes, err := fr.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, node := range nodes.Items {
				if _, err := infraprovider.Get().ExecK8NodeCommand(node.Name,
					[]string{"test", "-x", "/opt/cni/bin/dhcp"}); err != nil {
					Skip("dhcp CNI plugin not installed on the nodes; deploy with kind.sh --install-cni-plugins")
				}
			}
			for _, node := range nodes.Items {
				nodeName := node.Name
				_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c",
					"rm -f /run/cni/dhcp.sock; /opt/cni/bin/dhcp daemon >/var/log/dhcp-daemon.log 2>&1 & " +
						"echo $! > /var/run/dhcp-e2e-daemon.pid; sleep 1"})
				Expect(err).NotTo(HaveOccurred(), "must start the dhcp daemon on node %s", nodeName)
				DeferCleanup(func() {
					_, _ = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c",
						"kill $(cat /var/run/dhcp-e2e-daemon.pid) 2>/dev/null; " +
							"rm -f /var/run/dhcp-e2e-daemon.pid /run/cni/dhcp.sock"})
				})
			}
		}

		// podDHCPAnnotationEntry waits for the delegated plugin's lease to be
		// patched into the pod's pod-networks entry and returns the address,
		// verifying it belongs to the dnsmasq subnet and carries the dhcp
		// ipam_mode marker.
		podDHCPAnnotationEntry := func(podName string) string {
			GinkgoHelper()
			_, dhcpNet, err := net.ParseCIDR(dhcpSubnet)
			Expect(err).NotTo(HaveOccurred())
			leased := ""
			Eventually(func(g Gomega) {
				pod, err := fr.ClientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				podNetworks := map[string]struct {
					IPAddresses []string `json:"ip_addresses"`
					IPAMMode    string   `json:"ipam_mode"`
				}{}
				g.Expect(json.Unmarshal([]byte(pod.Annotations["k8s.ovn.org/pod-networks"]), &podNetworks)).To(Succeed())
				entry, found := podNetworks[nadKey]
				g.Expect(found).To(BeTrue())
				g.Expect(entry.IPAMMode).To(Equal("dhcp"))
				g.Expect(entry.IPAddresses).To(HaveLen(1))
				ip, _, err := net.ParseCIDR(entry.IPAddresses[0])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dhcpNet.Contains(ip)).To(BeTrue(), "lease %s must come from %s", ip, dhcpSubnet)
				leased = ip.String()
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
			return leased
		}

		It("assigns DHCP addresses to VMs and reports them in the pod annotation", func() {
			serverVMI := startVM("vm-server", "server")
			clientVMI := startVM("vm-client", "client")

			By("the guests obtain leases from the external DHCP server")
			serverIP := guestDHCPAddress(serverVMI)
			clientIP := guestDHCPAddress(clientVMI)

			By("the CNI one-shot probe reported the same addresses in the pod-networks annotation")
			Expect(launcherPodAnnotationIP(serverVMI)).To(Equal(serverIP))
			Expect(launcherPodAnnotationIP(clientVMI)).To(Equal(clientIP))

			By("the VMs can reach each other over the localnet")
			output, err := virtClient.RunCommand(clientVMI, "ping -c 3 -W 2 "+serverIP, time.Minute)
			Expect(err).NotTo(HaveOccurred(), output)
		})

		It("enforces MultiNetworkPolicy with pod selector peers on DHCP-assigned addresses", func() {
			serverVMI := startVM("vm-server", "server")
			clientVMI := startVM("vm-client", "client")
			serverIP := guestDHCPAddress(serverVMI)
			_ = guestDHCPAddress(clientVMI)

			By("applying an ingress policy allowing only role=client peers")
			policy := multiNetPolicy(
				"allow-from-client",
				nadKey,
				metav1.LabelSelector{MatchLabels: map[string]string{"role": "server"}},
				[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
				[]mnpapi.MultiNetworkPolicyIngressRule{{
					From: []mnpapi.MultiNetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}},
					}},
				}},
				nil,
			)
			Expect(createMultiNetworkPolicy(mnpClient, namespace, policy)).To(Succeed())

			By("traffic from the labeled client VM is allowed")
			Eventually(func() error {
				_, err := virtClient.RunCommand(clientVMI, "ping -c 2 -W 2 "+serverIP, time.Minute)
				return err
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By("removing the client's role label blocks its traffic (the peer address set follows the label)")
			launcherPod := launcherPodFor(clientVMI)
			_, err := e2ekubectl.RunKubectl(namespace, "label", "pod", launcherPod.Name, "role-")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := virtClient.RunCommand(clientVMI, "ping -c 2 -W 2 "+serverIP, time.Minute)
				return err
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).ShouldNot(Succeed())

			By("restoring the label restores connectivity")
			_, err = e2ekubectl.RunKubectl(namespace, "label", "pod", launcherPod.Name, "role=client")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := virtClient.RunCommand(clientVMI, "ping -c 2 -W 2 "+serverIP, time.Minute)
				return err
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		})

		It("applies NetworkQoS DSCP marking to VM traffic on the DHCP localnet", func() {
			if os.Getenv("OVN_NETWORK_QOS_ENABLE") != "true" {
				Skip("NetworkQoS feature is disabled (OVN_NETWORK_QOS_ENABLE != true)")
			}
			serverVMI := startVM("vm-server", "server")
			clientVMI := startVM("vm-client", "client")
			serverIP := guestDHCPAddress(serverVMI)
			_ = guestDHCPAddress(clientVMI)

			const dscpValue = 50
			By("applying a NetworkQoS selecting the DHCP CUDN")
			// GenerateCUDN labels the CUDN name=<cudn.Name>; the selector
			// matches it
			nqosSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: dhcp-qos
spec:
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          name: %s
  podSelector:
    matchLabels:
      role: client
  priority: 100
  egress:
  - dscp: %d
    classifier:
      to:
      - ipBlock:
          cidr: %s
`, namespace, cudn.Name, dscpValue, dhcpSubnet)
			nqosYaml := "dhcp-networkqos.yaml"
			Expect(os.WriteFile(nqosYaml, []byte(nqosSpec), 0644)).To(Succeed())
			DeferCleanup(func() { _ = os.Remove(nqosYaml) })
			e2ekubectl.RunKubectlOrDie(namespace, "create", "-f", nqosYaml)

			By("traffic from the client VM carries the DSCP mark, verified in the server guest")
			// fedora-with-test-tooling ships tcpdump; (ip[1] & 0xfc) >> 2 is
			// the DSCP field, the same filter networkqos.go uses
			tcpdumpCmd := fmt.Sprintf(
				"sudo timeout 15 tcpdump -i eth0 -c 1 'icmp and (ip[1] & 0xfc) >> 2 == %d' >/dev/null 2>&1 && echo MARKED",
				dscpValue)
			Eventually(func() string {
				go func() {
					defer GinkgoRecover()
					_, _ = virtClient.RunCommand(clientVMI, "ping -c 10 -i 0.5 "+serverIP, time.Minute)
				}()
				out, _ := virtClient.RunCommand(serverVMI, tcpdumpCmd, 30*time.Second)
				return out
			}).WithTimeout(3*time.Minute).WithPolling(2*time.Second).
				Should(ContainSubstring("MARKED"), "expected DSCP-marked packets at the server VM")
		})

		// Not a VM test: regular pods on a DHCP network delegate lease
		// handling to the dhcp CNI plugin daemon (VMs above use the one-shot
		// exchange). It lives in this Context to reuse the DHCP CUDN,
		// underlay and dnsmasq setup.
		It("assigns a regular pod its address via the delegated dhcp IPAM plugin", func() {
			startDHCPDaemons()

			createDHCPPod := func(name string) *corev1.Pod {
				podSpec := composeAgnhostPod(name, namespace, "", "pause")
				podSpec.Annotations = map[string]string{
					"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(
						`[{"name":%q,"namespace":%q,"interface":"net1"}]`, cudn.Name, namespace),
				}
				return e2epod.NewPodClient(fr).CreateSync(context.Background(), podSpec)
			}

			By("creating a pod attached to the DHCP network")
			pod := createDHCPPod("dhcp-pod")

			By("the delegated plugin's lease is reported in the pod-networks annotation")
			leased := podDHCPAnnotationEntry(pod.Name)

			By("the CNI result carried the applied address into the multus network-status")
			Eventually(func(g Gomega) {
				current, err := fr.ClientSet.CoreV1().Pods(namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				statuses := []struct {
					Interface string   `json:"interface"`
					IPs       []string `json:"ips"`
				}{}
				g.Expect(json.Unmarshal([]byte(current.Annotations["k8s.v1.cni.cncf.io/network-status"]), &statuses)).To(Succeed())
				for _, status := range statuses {
					if status.Interface == "net1" {
						g.Expect(status.IPs).To(ContainElement(leased))
						return
					}
				}
				g.Expect(false).To(BeTrue(), "no network-status entry for net1")
			}).WithTimeout(time.Minute).WithPolling(2 * time.Second).Should(Succeed())

			By("a recreated pod obtains a lease again (DEL released the old one, fresh exchange)")
			Expect(fr.ClientSet.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
			pod2 := createDHCPPod("dhcp-pod-2")
			podDHCPAnnotationEntry(pod2.Name)
		})
	})
})
