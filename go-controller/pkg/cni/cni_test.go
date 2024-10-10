package cni

import (
	"context"
	"fmt"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	v1nadmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/nad"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type podRequestInterfaceOpsStub struct {
	unconfiguredInterfaces []*PodInterfaceInfo
}

func (stub *podRequestInterfaceOpsStub) ConfigureInterface(pr *PodRequest, getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	return nil, nil
}
func (stub *podRequestInterfaceOpsStub) UnconfigureInterface(pr *PodRequest, ifInfo *PodInterfaceInfo) error {
	stub.unconfiguredInterfaces = append(stub.unconfiguredInterfaces, ifInfo)
	return nil
}

var _ = Describe("Network Segmentation", func() {
	var (
		fakeClientset            *fake.Clientset
		pr                       PodRequest
		pod                      *v1.Pod
		podLister                v1mocks.PodLister
		podNamespaceLister       v1mocks.PodNamespaceLister
		nadLister                v1nadmocks.NetworkAttachmentDefinitionLister
		clientSet                *ClientSet
		kubeAuth                 *KubeAPIAuth
		obtainedPodIterfaceInfos []*PodInterfaceInfo
		getCNIResultStub         = func(request *PodRequest, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
			obtainedPodIterfaceInfos = append(obtainedPodIterfaceInfos, podInterfaceInfo)
			return nil, nil
		}
		prInterfaceOpsStub                            = &podRequestInterfaceOpsStub{}
		enableMultiNetwork, enableNetworkSegmentation bool
		nadController                                 *ovntest.FakeNADController
	)

	BeforeEach(func() {

		config.IPv4Mode = true
		config.IPv6Mode = true
		enableMultiNetwork = config.OVNKubernetesFeature.EnableMultiNetwork
		enableNetworkSegmentation = config.OVNKubernetesFeature.EnableNetworkSegmentation

		podRequestInterfaceOps = prInterfaceOpsStub

		fakeClientset = fake.NewSimpleClientset()
		pr = PodRequest{
			Command:      CNIAdd,
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "eth0",
			CNIConf: &types.NetConf{
				NetConf:  cnitypes.NetConf{},
				DeviceID: "",
			},
			timestamp: time.Time{},
			IsVFIO:    false,
			netName:   ovntypes.DefaultNetworkName,
			nadName:   ovntypes.DefaultNetworkName,
		}
		pr.ctx, pr.cancel = context.WithTimeout(context.Background(), 2*time.Minute)

		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
		nadLister = v1nadmocks.NetworkAttachmentDefinitionLister{}
		clientSet = &ClientSet{
			podLister: &podLister,
			kclient:   fakeClientset,
		}
		kubeAuth = &KubeAPIAuth{
			Kubeconfig:       config.Kubernetes.Kubeconfig,
			KubeAPIServer:    config.Kubernetes.APIServer,
			KubeAPIToken:     config.Kubernetes.Token,
			KubeAPITokenFile: config.Kubernetes.TokenFile,
		}
		podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
	})
	AfterEach(func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = enableMultiNetwork
		config.OVNKubernetesFeature.EnableNetworkSegmentation = enableNetworkSegmentation

		podRequestInterfaceOps = &defaultPodRequestInterfaceOps{}
	})

	Context("with network segmentation fg disabled and annotation without role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = false
			config.OVNKubernetesFeature.EnableNetworkSegmentation = false
			pod = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pr.PodName,
					Namespace: pr.PodNamespace,
					Annotations: map[string]string{
						"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01"}}`,
					},
				},
			}
		})
		It("should not fail at cmdAdd", func() {
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			Expect(pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, nil)).NotTo(BeNil())
			Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
		})
		It("should not fail at cmdDel", func() {
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			Expect(pr.cmdDel(clientSet)).NotTo(BeNil())
			Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(1))
		})

	})
	Context("with network segmentation fg enabled and annotation with role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		})

		Context("pod with default primary network", func() {
			BeforeEach(func() {
				pod = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01", "role":"primary"}}`,
						},
					},
				}
				nadController = &ovntest.FakeNADController{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
			})
			It("should not fail at cmdAdd", func() {
				podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
				Expect(pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, nadController)).NotTo(BeNil())
				Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
			})
			It("should not fail at cmdDel", func() {
				podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
				Expect(pr.cmdDel(clientSet)).NotTo(BeNil())
				Expect(prInterfaceOpsStub.unconfiguredInterfaces).To(HaveLen(2))
			})
		})

		Context("pod with a user defined primary network", func() {
			const (
				dummyMACHostSide = "07:06:05:04:03:02"
				nadName          = "tenantred"
				namespace        = "foo-ns"
			)

			dummyGetCNIResult := func(request *PodRequest, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
				var gatewayIP net.IP
				if len(podInterfaceInfo.Gateways) > 0 {
					gatewayIP = podInterfaceInfo.Gateways[0]
				}
				var ips []*current.IPConfig
				ifaceIdx := 1 // host side of the veth is 0; pod side of the veth is 1
				for _, ip := range podInterfaceInfo.IPs {
					ips = append(ips, &current.IPConfig{Address: *ip, Gateway: gatewayIP, Interface: &ifaceIdx})
				}
				ifaceName := "eth0"
				if request.netName != "default" {
					ifaceName = "ovn-udn1"
				}

				interfaces := []*current.Interface{
					{
						Name: "host_" + ifaceName,
						Mac:  dummyMACHostSide,
					},
					{
						Name:    ifaceName,
						Mac:     podInterfaceInfo.MAC.String(),
						Sandbox: "bobloblaw",
					},
				}
				return &current.Result{
					CNIVersion: "0.3.1",
					Interfaces: interfaces,
					IPs:        ips,
				}, nil
			}

			BeforeEach(func() {
				pod = &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pr.PodName,
						Namespace: pr.PodNamespace,
						Annotations: map[string]string{
							"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["100.10.10.3/24","fd44::33/64"],"mac_address":"0a:58:fd:98:00:01", "role":"infrastructure-locked"}, "meganet":{"ip_addresses":["10.10.10.30/24","fd10::3/64"],"mac_address":"02:03:04:05:06:07", "role":"primary"}}`,
						},
					},
				}
				nad := &nadv1.NetworkAttachmentDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name:      nadName,
						Namespace: namespace,
					},
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: dummyPrimaryUDNConfig(namespace, nadName),
					},
				}
				nadNamespaceLister := &v1nadmocks.NetworkAttachmentDefinitionNamespaceLister{}
				nadNamespaceLister.On("List", labels.Everything()).Return([]*nadv1.NetworkAttachmentDefinition{nad}, nil)
				nadLister.On("NetworkAttachmentDefinitions", "foo-ns").Return(nadNamespaceLister)
				nadNetwork, err := util.ParseNADInfo(nad)
				Expect(err).NotTo(HaveOccurred())
				nadController = &ovntest.FakeNADController{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				nadController.PrimaryNetworks[nad.Namespace] = nadNetwork
				getCNIResultStub = dummyGetCNIResult
			})

			It("should return the information of both the default net and the primary UDN in the result", func() {
				podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
				response, err := pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub, nadController)
				Expect(err).NotTo(HaveOccurred())
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
				Expect(response.Result).To(Equal(
					&current.Result{
						CNIVersion: "0.3.1",
						Interfaces: []*current.Interface{
							{
								Name: "host_eth0",
								Mac:  dummyMACHostSide,
							},
							{
								Name:    "eth0",
								Mac:     "0a:58:fd:98:00:01",
								Sandbox: "bobloblaw",
							},
							{
								Name: "host_ovn-udn1",
								Mac:  dummyMACHostSide,
							},
							{
								Name:    "ovn-udn1",
								Mac:     "02:03:04:05:06:07",
								Sandbox: "bobloblaw",
							},
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
