// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	kubeapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type podRequestInterfaceOpsStub struct {
	unconfiguredInterfaces []*PodInterfaceInfo
	unconfigureOVSClients  []client.Client
}

func (stub *podRequestInterfaceOpsStub) ConfigureInterface(pr *PodRequest, _ client.Client, _ PodInfoGetter, pii *PodInterfaceInfo) ([]*current.Interface, error) {
	if len(pii.IPs) > 0 {
		return []*current.Interface{
			{
				Name: "host_" + pr.IfName,
			},
			{
				Name:    pr.IfName,
				Sandbox: "/var/run/netns/" + pr.PodNamespace + "_" + pr.PodName,
			},
		}, nil
	}
	return nil, nil
}
func (stub *podRequestInterfaceOpsStub) UnconfigureInterface(_ *PodRequest, ovsClient client.Client, ifInfo *PodInterfaceInfo, _ corev1listers.PodLister, _ *corev1.Pod) error {
	stub.unconfiguredInterfaces = append(stub.unconfiguredInterfaces, ifInfo)
	stub.unconfigureOVSClients = append(stub.unconfigureOVSClients, ovsClient)
	return nil
}

// dhcpPodRequestInterfaceOpsStub mimics ConfigureInterface for DHCP IPAM
// networks: the pod-networks annotation carries no IPs at ADD time, yet the
// real implementation still reports the host/container interface pair, which
// the DHCP flow needs (the learned IPs reference the sandbox interface).
type dhcpPodRequestInterfaceOpsStub struct {
	podRequestInterfaceOpsStub
	// configuredIfInfo records what cmdAdd handed to ConfigureInterface, so
	// specs can assert which L3 config would have been statically applied
	configuredIfInfo *PodInterfaceInfo
}

func (stub *dhcpPodRequestInterfaceOpsStub) ConfigureInterface(pr *PodRequest, _ client.Client, _ PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	stub.configuredIfInfo = ifInfo
	return []*current.Interface{
		{
			Name: "host_" + pr.IfName,
		},
		{
			Name:    pr.IfName,
			Sandbox: "/var/run/netns/" + pr.PodNamespace + "_" + pr.PodName,
		},
	}, nil
}

// podRequestToHTTPRequest builds the *http.Request that cnishim would POST to
// the cniserver's "/" endpoint, given a PodRequest.
// The reverse mapping (HTTP body -> PodRequest) lives in
// cniserver.go:cniRequestToPodRequest; this helper must stay aligned with it.
func podRequestToHTTPRequest(pr *PodRequest) *http.Request {
	confBytes, err := json.Marshal(pr.CNIConf)
	Expect(err).NotTo(HaveOccurred())
	cniArgs := fmt.Sprintf("K8S_POD_NAMESPACE=%s;K8S_POD_NAME=%s", pr.PodNamespace, pr.PodName)
	if pr.PodUID != "" {
		cniArgs += ";K8S_POD_UID=" + pr.PodUID
	}
	wireReq := &Request{
		Env: map[string]string{
			"CNI_COMMAND":     string(pr.Command),
			"CNI_CONTAINERID": pr.SandboxID,
			"CNI_NETNS":       pr.Netns,
			"CNI_IFNAME":      pr.IfName,
			"CNI_ARGS":        cniArgs,
		},
		Config:     confBytes,
		DeviceInfo: pr.deviceInfo,
	}
	body, err := json.Marshal(wireReq)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.NewRequest(http.MethodPost, "http://dummy/", bytes.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	return res
}

func getTestServer(factory factory.NodeWatchFactory, kclient kubernetes.Interface, networkManager networkmanager.Interface) *Server {
	cs := &ClientSet{
		podLister: factory.PodCoreInformer().Lister(),
		kclient:   kclient,
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cs.nadLister = factory.NADInformer().Lister()
	}
	return &Server{
		clientSet: cs,
		kubeAuth: &KubeAPIAuth{
			Kubeconfig:       config.Kubernetes.Kubeconfig,
			KubeAPIServer:    config.Kubernetes.APIServer,
			KubeAPIToken:     config.Kubernetes.Token,
			KubeAPITokenFile: config.Kubernetes.TokenFile,
		},
		networkManager: networkManager,
	}
}

var _ = Describe("Network Segmentation", func() {
	var (
		pr                 PodRequest
		pod                *corev1.Pod
		fakeNetworkManager *networkmanager.FakeNetworkManager
		prInterfaceOpsStub *podRequestInterfaceOpsStub
		cniServer          *Server
		wf                 factory.NodeWatchFactory
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = true

		prInterfaceOpsStub = &podRequestInterfaceOpsStub{}
		podRequestInterfaceOps = prInterfaceOpsStub

		fakeNetworkManager = &networkmanager.FakeNetworkManager{
			PrimaryNetworks: make(map[string]util.NetInfo),
		}

		pr = PodRequest{
			Command:      CNIAdd,
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "eth0",
			CNIConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					CNIVersion: "1.0.0",
					Type:       "ovn-k8s-cni-overlay",
				},
				DeviceID: "",
			},
			netName: ovntypes.DefaultNetworkName,
			nadName: ovntypes.DefaultNetworkName,
			nadKey:  ovntypes.DefaultNetworkName,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		DeferCleanup(cancel)
		pr.ctx = ctx
	})
	AfterEach(func() {
		podRequestInterfaceOps = &defaultPodRequestInterfaceOps{}
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		cniServer = nil
	})

	startCNIServer := func(objects ...runtime.Object) {
		fakeClient := util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())

		cniServer = getTestServer(wf, fakeClient.KubeClient, fakeNetworkManager)
		ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(ovsCleanup.Cleanup)
		cniServer.ovsClient = ovsClient
	}

	handlePodRequest := func() *Response {
		res, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
		Expect(err).NotTo(HaveOccurred())
		response := &Response{}
		err = json.Unmarshal(res, response)
		Expect(err).NotTo(HaveOccurred())
		return response
	}

	Context("CNI DEL on a simulated DPU host", func() {
		BeforeEach(func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
			config.OvnKubeNode.SimulateDPU = true
			pr.Command = CNIDel
			pr.CNIConf.DeviceID = "eth0-14"
		})

		It("unconfigures the device after the pod is deleted", func() {
			startCNIServer(testing.NewNamespace(pr.PodNamespace))

			handlePodRequest()

			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(ConsistOf(&PodInterfaceInfo{
				IsDPUHostMode:  true,
				IsSimulatedDPU: true,
				NetdevName:     pr.CNIConf.DeviceID,
			}))
		})

		It("unconfigures the device after its connection details are removed", func() {
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        pr.PodName,
					Namespace:   pr.PodNamespace,
					Annotations: map[string]string{},
				},
			}
			startCNIServer(testing.NewNamespace(pr.PodNamespace), pod)

			handlePodRequest()

			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(ConsistOf(&PodInterfaceInfo{
				IsDPUHostMode:  true,
				IsSimulatedDPU: true,
				NetdevName:     pr.CNIConf.DeviceID,
			}))
		})
	})

	Context("with network segmentation fg disabled and annotation without role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = false
			config.OVNKubernetesFeature.EnableNetworkSegmentation = false
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pr.PodName,
					Namespace: pr.PodNamespace,
					Annotations: map[string]string{
						"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01"}}`,
					},
				},
			}
		})
		It("should not fail at cmdAdd or cmdDel", func() {
			startCNIServer(testing.NewNamespace(pod.Namespace), pod)

			By("cmdAdd primary pod interface should be added")
			response := handlePodRequest()
			Expect(response.Result).NotTo(BeNil())
			Expect(response.Result.Interfaces).To(HaveLen(2))
			By("cmdDel primary pod interface should be removed")
			pr.Command = CNIDel
			handlePodRequest()
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
			Expect(prInterfaceOpsStub.unconfigureOVSClients).To(ConsistOf(cniServer.ovsClient))
		})
	})
	Context("with network segmentation fg enabled and annotation with role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		})

		Context("pod with default primary network", func() {
			BeforeEach(func() {
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01", "role":"primary"}}`,
						},
					},
				}
			})

			Context("with CNI Privileged Mode", func() {
				It("should not fail at cmdAdd or cmdDel", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					By("cmdAdd primary pod interface should be added")
					response := handlePodRequest()

					Expect(response.Result).NotTo(BeNil())
					Expect(response.Result.Interfaces).To(HaveLen(2))
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					By("cmdDel primary pod interface should be removed")
					pr.Command = CNIDel
					handlePodRequest()
					Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
					Expect(prInterfaceOpsStub.unconfigureOVSClients).To(ConsistOf(cniServer.ovsClient))
				})
				It("should fail cmdAdd when the server has no OVS client", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					cniServer.ovsClient = nil

					_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
					Expect(err).To(MatchError(ContainSubstring("OVS client is required in privileged mode")))
				})
				It("should fail cmdDel when the server has no OVS client", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					cniServer.ovsClient = nil
					pr.Command = CNIDel

					_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
					Expect(err).To(MatchError(ContainSubstring("OVS client is required in privileged mode")))
				})
			})

			Context("with CNI Unprivileged Mode", func() {
				BeforeEach(func() {
					config.UnprivilegedMode = true
				})
				It("should not fail at cmdAdd", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					response := handlePodRequest()
					Expect(response.Result).To(BeNil())
					Expect(response.PodIFInfo).NotTo(BeNil())
					Expect(response.PrimaryUDNPodReq).To(BeNil())
					Expect(response.PrimaryUDNPodInfo).To(BeNil())
				})
				It("should not fail at cmdDel", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod)
					pr.Command = CNIDel
					response := handlePodRequest()
					Expect(response.Result).To(BeNil())
					Expect(response.PodIFInfo).NotTo(BeNil())
				})
			})
		})

		Context("pod with a user defined primary network", func() {
			const namespace = "foo-ns"

			var nadMegaNet runtime.Object

			BeforeEach(func() {
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["100.10.10.3/24","fd44::33/64"],"mac_address":"0a:58:fd:98:00:01", "role":"infrastructure-locked"}, "foo-ns/meganet":{"ip_addresses":["10.10.10.30/24","fd10::3/64"],"mac_address":"02:03:04:05:06:07", "role":"primary"}}`,
						},
					},
				}
				nad := testing.GenerateNADWithConfig("meganet", namespace, dummyPrimaryUDNConfig(namespace, "meganet"))
				nadMegaNet = nad
				nadNetwork, err := util.ParseNADInfo(nad)
				Expect(err).NotTo(HaveOccurred())
				fakeNetworkManager.PrimaryNetworks[namespace] = nadNetwork
				fakeNetworkManager.NADNetworks = map[string]util.NetInfo{
					namespace + "/meganet": nadNetwork,
				}
			})

			Context("with CNI Privileged Mode", func() {
				It("should return the information of both the default net and the primary UDN in the result", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod, nadMegaNet)
					response := handlePodRequest()

					// for every interface added, we return 2 interfaces; the host side of the
					// veth, then the pod side of the veth.
					// thus, the UDN interface idx will be 3:
					// idx: iface
					//   0: host side primary UDN
					//   1: pod side default network
					//   2: host side default network
					//   3: pod side primary UDN
					podDefaultClusterNetIfaceIDX := 1
					podUDNIfaceIDX := 3
					sandbox := "/var/run/netns/" + pod.Namespace + "_" + pod.Name
					Expect(response.Result).To(Equal(
						&current.Result{
							Interfaces: []*current.Interface{
								{Name: "host_eth0"},
								{Name: "eth0", Sandbox: sandbox},
								{Name: "host_" + primaryUDNIfName},
								{Name: primaryUDNIfName, Sandbox: sandbox},
							},
							IPs: []*current.IPConfig{
								{
									Address: net.IPNet{
										IP:   net.ParseIP("100.10.10.3"),
										Mask: net.CIDRMask(24, 32),
									},
									Interface: &podDefaultClusterNetIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("fd44::33"),
										Mask: net.CIDRMask(64, 128),
									},
									Interface: &podDefaultClusterNetIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("10.10.10.30"),
										Mask: net.CIDRMask(24, 32),
									},
									Interface: &podUDNIfaceIDX,
								},
								{
									Address: net.IPNet{
										IP:   net.ParseIP("fd10::3"),
										Mask: net.CIDRMask(64, 128),
									},
									Interface: &podUDNIfaceIDX,
								},
							},
						},
					))
				})
			})
			Context("with CNI Unprivileged Mode", func() {
				BeforeEach(func() {
					config.UnprivilegedMode = true
				})
				It("should return the information of both the default net and the primary UDN in the result", func() {
					startCNIServer(testing.NewNamespace(pod.Namespace), pod, nadMegaNet)
					response := handlePodRequest()

					Expect(response.Result).To(BeNil())
					podNADAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, "foo-ns/meganet")
					Expect(err).NotTo(HaveOccurred())
					Expect(response.PrimaryUDNPodInfo).To(Equal(
						&PodInterfaceInfo{
							PodAnnotation: *podNADAnnotation,
							MTU:           1400,
							NetName:       "tenantred",
							NADKey:        "foo-ns/meganet",
						}))
					Expect(response.PrimaryUDNPodReq.IfName).To(Equal(primaryUDNIfName))
					Expect(response.PodIFInfo.NetName).To(Equal("default"))
				})
			})

		})
	})

})

func dummyPrimaryUDNConfig(ns, nadName string) string {
	namespacedName := fmt.Sprintf("%s/%s", ns, nadName)
	return fmt.Sprintf(`
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "10.10.0.0/16,fd10::0/64",
            "netAttachDefName": %q,
            "role": "primary"
    }
`, namespacedName)
}

var _ = Describe("checkBridgeMapping", func() {
	const networkName = "test-network"

	Context("when topology is not localnet", func() {
		It("should return nil without checking bridge mappings", func() {
			ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(ovsCleanup.Cleanup)
			Expect(checkBridgeMapping(ovsClient, ovntypes.Layer2Topology, networkName)).To(Succeed())
		})
	})

	Context("when using default network", func() {
		It("should return nil without checking bridge mappings", func() {
			ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(ovsCleanup.Cleanup)
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, ovntypes.DefaultNetworkName)).To(Succeed())
		})
	})

	Context("when bridge mapping exists in external IDs", func() {
		It("should return nil if the bridge mapping is found", func() {
			ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{
				"ovn-bridge-mappings": "test-network:br-int",
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(ovsCleanup.Cleanup)
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, networkName)).To(Succeed())
		})

		It("should return error if the bridge mapping isn't found", func() {
			ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{
				"ovn-bridge-mappings": "other-network:br-int",
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(ovsCleanup.Cleanup)
			Expect(checkBridgeMapping(ovsClient, ovntypes.LocalnetTopology, networkName).Error()).To(
				Equal(`failed to find OVN bridge-mapping for network: "test-network"`))
		})
	})
})

// dhcpOpsStub stubs the DHCP seams the way podRequestInterfaceOpsStub stubs
// the interface plumbing: the counters record which delegation path ran, the
// err fields inject failures, and anything not overridden falls through to
// the real implementation via the embedded defaultDHCPOps.
type dhcpOpsStub struct {
	defaultDHCPOps
	newResult                        func() *current.Result
	oneShotErr, addErr, delErr       error
	oneShotCalls, addCalls, delCalls int
}

func (s *dhcpOpsStub) DoOneShot(*PodRequest) (*current.Result, error) {
	s.oneShotCalls++
	if s.oneShotErr != nil {
		return nil, s.oneShotErr
	}
	return s.newResult(), nil
}

func (s *dhcpOpsStub) ExecAdd(*PodRequest, bool) (*current.Result, error) {
	s.addCalls++
	if s.addErr != nil {
		return nil, s.addErr
	}
	return s.newResult(), nil
}

func (s *dhcpOpsStub) ExecDel(*PodRequest) error {
	s.delCalls++
	return s.delErr
}

var _ = Describe("DHCP IPAM workload differentiation", func() {
	const (
		podNamespace = "foo-ns"
		podName      = "bar-pod"
		nadKey       = "foo-ns/localnet-nad"
	)

	var (
		pr                 PodRequest
		pod                *corev1.Pod
		fakeNetworkManager *networkmanager.FakeNetworkManager
		prInterfaceOpsStub *dhcpPodRequestInterfaceOpsStub
		cniServer          *Server
		wf                 factory.NodeWatchFactory
		fexec              *testing.FakeExec
		dhcpStub           *dhcpOpsStub
	)

	// cmdDel resolves the host-side OVS interface of a DeviceID-backed
	// attachment with these lookups (newest key first, NAD-key fallback)
	const (
		hostIfaceFindCmd         = "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=name find Interface external-ids:sandbox=824bceff24af3 external_ids:pod-if-name=net1"
		hostIfaceFallbackFindCmd = "ovs-vsctl --timeout=30 --no-heading --format=csv --data=bare --columns=name find Interface external-ids:sandbox=824bceff24af3 external_ids:k8s.ovn.org/nad=foo-ns/localnet-nad"
	)

	dhcpIP := func() *current.IPConfig {
		return &current.IPConfig{
			Address: net.IPNet{IP: net.ParseIP("10.1.192.213"), Mask: net.CIDRMask(24, 32)},
			Gateway: net.ParseIP("10.1.192.1"),
		}
	}

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		prInterfaceOpsStub = &dhcpPodRequestInterfaceOpsStub{}
		podRequestInterfaceOps = prInterfaceOpsStub

		// every expected ovs-vsctl invocation is declared per spec
		fexec = testing.NewFakeExec()
		Expect(SetExec(fexec)).To(Succeed())

		fakeNetworkManager = &networkmanager.FakeNetworkManager{
			PrimaryNetworks: make(map[string]util.NetInfo),
		}

		dhcpStub = &dhcpOpsStub{newResult: func() *current.Result {
			return &current.Result{IPs: []*current.IPConfig{dhcpIP()}}
		}}
		dhcpOps = dhcpStub

		pr = PodRequest{
			Command:      CNIAdd,
			PodNamespace: podNamespace,
			PodName:      podName,
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "net1",
			CNIConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					CNIVersion: "1.0.0",
					Name:       "localnet-net",
					Type:       "ovn-k8s-cni-overlay",
					IPAM:       cnitypes.IPAM{Type: "dhcp"},
				},
				NADName:             nadKey,
				Topology:            ovntypes.LocalnetTopology,
				PhysicalNetworkName: "physnet",
			},
			netName: "localnet-net",
			nadName: nadKey,
		}
		// the DHCP annotation patch verifies the request's sandbox is still
		// current by stat-ing its netns; give the request a real file to stat
		pr.Netns = filepath.Join(GinkgoT().TempDir(), "netns")
		Expect(os.WriteFile(pr.Netns, nil, 0o600)).To(Succeed())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		DeferCleanup(cancel)
		pr.ctx = ctx

		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: podNamespace,
				Annotations: map[string]string{
					"k8s.v1.cni.cncf.io/networks": `[{"name":"localnet-nad","namespace":"foo-ns","interface":"net1"}]`,
					"k8s.ovn.org/pod-networks":    `{"foo-ns/localnet-nad":{"mac_address":"0a:58:fd:98:00:01","role":"secondary","ipam_mode":"dhcp"}}`,
				},
			},
		}
	})

	AfterEach(func() {
		dhcpOps = &defaultDHCPOps{}
		podRequestInterfaceOps = &defaultPodRequestInterfaceOps{}
		ResetRunner()
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		cniServer = nil
	})

	startCNIServer := func(objects ...runtime.Object) {
		fakeClient := util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())

		cniServer = getTestServer(wf, fakeClient.KubeClient, fakeNetworkManager)
		ovsClient, ovsCleanup, err := newOVSClientWithExternalIDs(map[string]string{
			"ovn-bridge-mappings": "physnet:br-phys",
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(ovsCleanup.Cleanup)
		cniServer.ovsClient = ovsClient
	}

	handlePodRequest := func() *Response {
		res, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
		Expect(err).NotTo(HaveOccurred())
		response := &Response{}
		Expect(json.Unmarshal(res, response)).To(Succeed())
		return response
	}

	markPodAsVirtLauncher := func() {
		// kubevirt sets this label on every virt-launcher (VM) pod; it is what
		// kubevirt.IsPodOwnedByVirtualMachine keys off.
		pod.Labels = map[string]string{"kubevirt.io": "virt-launcher"}
	}

	// expectDHCPIPsInPodNetworksAnnotation asserts that cmdAdd patched the
	// DHCP-learned IP and gateway into the pod's pod-networks annotation entry
	// for the NAD, so ovnkube-controller can program the LSP with it.
	expectDHCPIPsInPodNetworksAnnotation := func() {
		updatedPod, err := cniServer.clientSet.kclient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		podAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(podAnnotation.IPs).To(HaveLen(1))
		Expect(podAnnotation.IPs[0].String()).To(Equal("10.1.192.213/24"))
		Expect(podAnnotation.Gateways).To(HaveLen(1))
		Expect(podAnnotation.Gateways[0].String()).To(Equal("10.1.192.1"))
		Expect(podAnnotation.IPAMMode).To(Equal("dhcp"), "the ipam_mode marker must be preserved")
	}

	Context("cmdAdd", func() {
		It("delegates a regular pod to the DHCP CNI plugin daemon", func() {
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			response := handlePodRequest()

			Expect(dhcpStub.addCalls).To(Equal(1), "regular pods must go through the DHCP plugin delegation path")
			Expect(dhcpStub.oneShotCalls).To(BeZero(), "regular pods must not take the one-shot VM path")
			Expect(response.Result).NotTo(BeNil())
			Expect(response.Result.IPs).To(HaveLen(1))
			Expect(response.Result.IPs[0].Address.IP.String()).To(Equal("10.1.192.213"))
			expectDHCPIPsInPodNetworksAnnotation()
		})

		It("performs one-shot DHCP discovery for a KubeVirt VM pod", func() {
			markPodAsVirtLauncher()
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			response := handlePodRequest()

			Expect(dhcpStub.oneShotCalls).To(Equal(1), "VM pods must take the one-shot discovery path")
			Expect(dhcpStub.addCalls).To(BeZero(), "VM pods must not be delegated to the DHCP plugin daemon")
			Expect(response.Result).NotTo(BeNil())
			Expect(response.Result.IPs).To(HaveLen(1))
			expectDHCPIPsInPodNetworksAnnotation()
		})

		It("overwrites previously reported DHCP IPs on a repeat ADD with a new lease", func() {
			// the pod already carries an older DHCP-learned IP (e.g. the sandbox
			// was recreated and the DHCP server handed out a new lease)
			pod.Annotations["k8s.ovn.org/pod-networks"] = `{"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.55/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			response := handlePodRequest()

			// the stale lease must not reach the interface configuration: a
			// statically applied stale IP answers ARP for an address the DHCP
			// server may have re-leased, and a stale gateway would become a
			// static default route colliding with the primary network's
			// ("failed to add gateway route to link: file exists"), failing
			// every repeat ADD
			Expect(prInterfaceOpsStub.configuredIfInfo.IPs).To(BeEmpty(),
				"stale annotation IPs must not be statically applied on a DHCP network")
			Expect(prInterfaceOpsStub.configuredIfInfo.Gateways).To(BeEmpty(),
				"stale annotation gateways must not become static default routes")
			// only the fresh lease is reported, not stale+new
			Expect(response.Result.IPs).To(HaveLen(1))
			Expect(response.Result.IPs[0].Address.IP.String()).To(Equal("10.1.192.213"))
			expectDHCPIPsInPodNetworksAnnotation()
		})

		It("refuses to patch the annotation when the request's sandbox netns is gone", func() {
			// the late-writer race: kubelet abandoned this request (nothing
			// cancels the server-side ADD), tore its sandbox down and started
			// a newer one, which already patched its fresh lease (10.1.192.99)
			// into the annotation; the superseded request's stale lease
			// (10.1.192.213) must not overwrite it
			pod.Annotations["k8s.ovn.org/pod-networks"] = `{"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.99/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`
			Expect(os.Remove(pr.Netns)).To(Succeed())
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
			Expect(err).To(MatchError(ContainSubstring("superseded")))

			// the newer sandbox's lease must survive
			updatedPod, err := cniServer.clientSet.kclient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			podAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(podAnnotation.IPs).To(HaveLen(1))
			Expect(podAnnotation.IPs[0].String()).To(Equal("10.1.192.99/24"))
		})

		It("fails the request when one-shot DHCP discovery fails", func() {
			markPodAsVirtLauncher()
			dhcpStub.oneShotErr = fmt.Errorf("no DHCP server responded")
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
			Expect(err).To(MatchError(ContainSubstring("VM DHCP discovery failed")))
		})

		It("is rejected in unprivileged mode", func() {
			config.UnprivilegedMode = true
			DeferCleanup(func() { config.UnprivilegedMode = false })
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
			Expect(err).To(MatchError(ContainSubstring("dhcp IPAM mode for localnet topology is not supported in unprivileged mode")))
			Expect(dhcpStub.addCalls).To(BeZero())
			Expect(dhcpStub.oneShotCalls).To(BeZero())
		})

		It("is rejected for a non-VM pod with a VFIO device", func() {
			// a regular (non-KubeVirt) pod using a vfio-pci bound VF, e.g. a
			// DPDK workload: no netdev exists in the pod netns, so neither
			// DHCP path can serve it
			pr.CNIConf.DeviceID = "0000:65:00.2"
			fakeSriovnetOps := utilMocks.SriovnetOps{}
			// SetSriovnetOpsInst swaps a package-global singleton: restore it
			// so the mock (which panics on any unexpected call) cannot leak
			// into later specs that carry a DeviceID
			prevSriovnetOps := util.GetSriovnetOps()
			util.SetSriovnetOpsInst(&fakeSriovnetOps)
			DeferCleanup(func() { util.SetSriovnetOpsInst(prevSriovnetOps) })
			fakeSriovnetOps.On("IsVfPciVfioBound", "0000:65:00.2").Return(true)
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			_, err := cniServer.handleCNIRequest(podRequestToHTTPRequest(&pr))
			Expect(err).To(MatchError(ContainSubstring("dhcp IPAM mode is not supported for VFIO device")))
			Expect(dhcpStub.addCalls).To(BeZero(), "the DHCP plugin must not be invoked for a VFIO pod")
			Expect(dhcpStub.oneShotCalls).To(BeZero(), "a non-VM pod must not take the one-shot VM path")
		})

		It("writes a DPU host's connection details when a repeat ADD needs no DHCP write", func() {
			// DHCP network pod on a DPU host, repeat ADD: the fixture pod
			// already carries the MAC-only DHCP entry, so
			// allocateDHCPMACAnnotation reuses it (no DHCP write needed) and
			// cmdAdd routes the DPU connection details through the DPU flow's
			// own writer rather than the combined DHCP+DPU update. The details
			// must still land and the DHCP delegation must still run.
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps := utilMocks.SriovnetOps{}
			prevSriovnetOps := util.GetSriovnetOps()
			util.SetSriovnetOpsInst(&fakeSriovnetOps)
			DeferCleanup(func() { util.SetSriovnetOpsInst(prevSriovnetOps) })
			fakeSriovnetOps.On("IsVfPciVfioBound", "0000:05:00.4").Return(false)
			fakeSriovnetOps.On("GetNetDevicesFromPci", "0000:05:00.4").Return([]string{"ens1f0v2"}, nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", "0000:05:00.4").Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", "0000:05:00.4").Return(0, nil)
			// the DPU-readiness wait requires the plumbing status to be Ready
			pod.Annotations[util.DPUConnectionStatusAnnot] = `{"foo-ns/localnet-nad":{"Status":"Ready"}}`
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			response := handlePodRequest()

			By("the DPU connection details were written for the NAD")
			updatedPod, err := cniServer.clientSet.kclient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			dcd, err := util.UnmarshalPodDPUConnDetails(updatedPod.Annotations, nadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(*dcd).To(Equal(util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    pr.SandboxID,
				VfNetdevName: "ens1f0v2",
			}))

			By("the DHCP delegation still ran and patched the lease, keeping the entry's MAC")
			Expect(dhcpStub.addCalls).To(Equal(1))
			Expect(response.Result).NotTo(BeNil())
			expectDHCPIPsInPodNetworksAnnotation()
			podAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(podAnnotation.MAC.String()).To(Equal("0a:58:fd:98:00:01"))
		})

	})

	Context("cmdDel", func() {
		BeforeEach(func() {
			pr.Command = CNIDel
			// the release path first checks that the dhcp plugin binary is
			// installed; pretend it is unless a spec says otherwise
			dhcpPluginExec = &fakeDHCPExec{}
			DeferCleanup(func() { dhcpPluginExec = nil })
		})

		It("releases the DHCP lease for a regular pod", func() {
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(Equal(1), "regular pods must release their daemon-managed lease")
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})

		It("sends the release for a KubeVirt VM pod too, a no-op at the daemon", func() {
			markPodAsVirtLauncher()
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(Equal(1),
				"the daemon tracks its own leases; a DEL for a container it never served releases nothing")
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})

		It("releases without any apiserver or node-local state", func() {
			// force-deleted pod: the object is gone from the API by DEL time;
			// the release needs no state at all — the daemon is the tracker
			startCNIServer(testing.NewNamespace(podNamespace))

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(Equal(1))
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})

		It("skips the release when the dhcp plugin is not installed", func() {
			// the pod delegation path hard-requires the plugin at ADD, so a
			// missing binary means no pod on this node ever obtained a lease
			dhcpPluginExec = &fakeDHCPExec{findErr: fmt.Errorf("no dhcp binary in CNI_PATH")}
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(BeZero())
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1), "the teardown must still run")
		})

		It("succeeds and continues the teardown when the release itself fails", func() {
			// releases are best-effort: a failure only leaks the lease until
			// the server-side TTL, not worth wedging the pod teardown
			dhcpStub.delErr = fmt.Errorf("dhcp daemon not responding")
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(Equal(1))
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})

		It("does not send a release for a VFIO device, whose lease the guest owns", func() {
			pr.CNIConf.DeviceID = "0000:65:00.2"
			fakeSriovnetOps := utilMocks.SriovnetOps{}
			prevSriovnetOps := util.GetSriovnetOps()
			util.SetSriovnetOpsInst(&fakeSriovnetOps)
			DeferCleanup(func() { util.SetSriovnetOpsInst(prevSriovnetOps) })
			fakeSriovnetOps.On("IsVfPciVfioBound", "0000:65:00.2").Return(true)
			// the DeviceID teardown path resolves the host-side OVS interface;
			// none exists in this harness and the lookup failure is tolerated
			fexec.AddFakeCmd(&testing.ExpectedCmd{Cmd: hostIfaceFindCmd, Output: ""})
			fexec.AddFakeCmd(&testing.ExpectedCmd{Cmd: hostIfaceFallbackFindCmd, Output: ""})
			startCNIServer(testing.NewNamespace(podNamespace), pod)

			handlePodRequest()

			Expect(dhcpStub.delCalls).To(BeZero(), "the guest owns a VFIO device's lease; no RELEASE may be sent")
			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
		})
	})
})

// The update helper re-fetches the pod by name, so a pod recreated with the
// same name mid-ADD would inherit the old request's staged annotations. The
// UID guard must fail the write and leave the recreated pod untouched.
var _ = Describe("updateDHCPAndDPUAnnotations pod identity guard", func() {
	const (
		podNamespace = "foo-ns"
		podName      = "bar-pod"
		nadKey       = "foo-ns/dhcpnet"
	)

	var wf factory.NodeWatchFactory

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})
	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
	})

	newPodWithUID := func(uid string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: podNamespace,
				Name:      podName,
				UID:       apimachinerytypes.UID(uid),
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
	}

	start := func(objects ...runtime.Object) (*ClientSet, kubeapi.Interface) {
		fakeClient := util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())
		cs := &ClientSet{
			podLister: wf.PodCoreInformer().Lister(),
			kclient:   fakeClient.KubeClient,
		}
		return cs, &kubeapi.Kube{KClient: fakeClient.KubeClient}
	}

	newDHCPEntry := func() *util.PodAnnotation {
		mac, err := net.ParseMAC("0a:58:fd:98:00:01")
		Expect(err).NotTo(HaveOccurred())
		return &util.PodAnnotation{
			MAC:      mac,
			Role:     ovntypes.NetworkRoleSecondary,
			IPAMMode: ovntypes.IPAMTypeDHCP,
		}
	}

	// the staged-annotation write refuses to run for an abandoned sandbox,
	// so the request needs a live netns path
	newTestNetns := func() string {
		netns, err := os.CreateTemp("", "netns")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { os.Remove(netns.Name()) })
		return netns.Name()
	}

	It("writes the staged entry when the pod is the one the request read", func() {
		pod := newPodWithUID("uid-a")
		cs, kubecli := start(testing.NewNamespace(podNamespace), pod)
		pr := &PodRequest{PodNamespace: podNamespace, PodName: podName, PodUID: "uid-a", nadKey: nadKey, Netns: newTestNetns()}

		Expect(pr.updateDHCPAndDPUAnnotations(cs, kubecli, pod, newDHCPEntry(), nil)).To(Succeed())

		got, err := cs.kclient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Annotations).To(HaveKey(ovntypes.OvnPodAnnotationName))
	})

	It("refuses to write onto a same-name pod with a different UID and leaves it untouched", func() {
		// the cluster now holds the recreated pod...
		cs, kubecli := start(testing.NewNamespace(podNamespace), newPodWithUID("uid-b"))
		// while the in-flight request staged its entry against the old one
		stalePod := newPodWithUID("uid-a")
		// the runtime's request carries the old instance's UID
		pr := &PodRequest{PodNamespace: podNamespace, PodName: podName, PodUID: "uid-a", nadKey: nadKey, Netns: newTestNetns()}

		err := pr.updateDHCPAndDPUAnnotations(cs, kubecli, stalePod, newDHCPEntry(), nil)
		Expect(err).To(MatchError(apierrors.IsNotFound, "IsNotFound"))

		got, err := cs.kclient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Annotations).NotTo(HaveKey(ovntypes.OvnPodAnnotationName),
			"the recreated pod must not inherit the old request's DHCP MAC")
	})
})

func newOVSClientWithExternalIDs(externalIDs map[string]string) (client.Client, *libovsdbtest.Context, error) {
	ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{
				ExternalIDs: externalIDs,
			},
		},
	})
	return ovsClient, ovsCleanup, err
}
