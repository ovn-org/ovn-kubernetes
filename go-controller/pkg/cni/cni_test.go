package cni

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	v1nadmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

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
		enableMultiNetwork, enableNetworkSegmentation bool
	)

	BeforeEach(func() {

		enableMultiNetwork = config.OVNKubernetesFeature.EnableMultiNetwork
		enableNetworkSegmentation = config.OVNKubernetesFeature.EnableNetworkSegmentation

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
			nadLister: &nadLister,
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
			Expect(pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub)).NotTo(BeNil())
			Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
		})
	})
	Context("with network segmentation fg enabled and annotation with role field", func() {
		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			pod = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pr.PodName,
					Namespace: pr.PodNamespace,
					Annotations: map[string]string{
						"k8s.ovn.org/pod-networks": `{"default":{"ip_address":"100.10.10.3/24","mac_address":"0a:58:fd:98:00:01", "role":"primary"}}`,
					},
				},
			}
		})
		It("should not fail at cmdAdd", func() {
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			Expect(pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientSet, getCNIResultStub)).NotTo(BeNil())
			Expect(obtainedPodIterfaceInfos).ToNot(BeEmpty())
		})
	})

})
