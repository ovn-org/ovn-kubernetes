package cni

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	podnetworkapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1"
	podnetworkfakeclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1/apis/clientset/versioned/fake"
	podnetworkinformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1/apis/informers/externalversions"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func newPod(namespace, name string, annotations map[string]string) *v1.Pod {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         types.UID(name),
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func newPodNet(namespace, name string) *podnetworkapi.PodNetwork {
	return &podnetworkapi.PodNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: podnetworkapi.PodNetworkSpec{
			Networks: map[string]podnetworkapi.OVNNetwork{},
		},
	}
}

func newFakeClientSet(pod *v1.Pod, podnetwork *podnetworkapi.PodNetwork, podNamespaceLister *mocks.PodNamespaceLister) *ClientSet {
	podLister := mocks.PodLister{}
	podLister.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)
	podList := &v1.PodList{}
	if pod != nil {
		podList.Items = []v1.Pod{*pod}
	}
	fakeClient := fake.NewSimpleClientset(podList)
	podnetList := &podnetworkapi.PodNetworkList{}
	if podnetwork != nil {
		podnetList.Items = []podnetworkapi.PodNetwork{*podnetwork}
	}
	fakePodnetClient := podnetworkfakeclientset.NewSimpleClientset(podnetList)
	podNetworkFactory := podnetworkinformerfactory.NewSharedInformerFactory(fakePodnetClient, 0)
	podNetLister := podNetworkFactory.K8s().V1().PodNetworks().Lister()

	return &ClientSet{
		kclient:      fakeClient,
		podLister:    &podLister,
		podNetLister: podNetLister,
		podNetClient: fakePodnetClient,
	}
}

var _ = Describe("CNI Utils tests", func() {
	Context("isOvnReady", func() {
		It("Returns true if OVN pod network annotation exists", func() {
			podAnnot := map[string]string{}
			podNetwork := &podnetworkapi.PodNetwork{
				// don't init ObjectMeta, since it is not used in isOvnReady
				Spec: podnetworkapi.PodNetworkSpec{
					Networks: map[string]podnetworkapi.OVNNetwork{
						ovntypes.DefaultNetworkName: {
							IPs:      []string{"192.168.2.3/24"},
							MAC:      "0a:58:c0:a8:02:03",
							Gateways: []string{"192.168.2.1"},
						},
					},
				},
			}
			Expect(isOvnReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(true))
		})

		It("Returns false if OVN pod network annotation does not exist", func() {
			podAnnot := map[string]string{}
			Expect(isOvnReady(podAnnot, nil, ovntypes.DefaultNetworkName)).To(Equal(false))
		})
	})

	Context("isDPUReady", func() {
		podNetwork := &podnetworkapi.PodNetwork{
			// don't init ObjectMeta, since it is not used in isOvnReady
			Spec: podnetworkapi.PodNetworkSpec{
				Networks: map[string]podnetworkapi.OVNNetwork{
					ovntypes.DefaultNetworkName: {
						IPs: []string{"192.168.2.3/24"},
						MAC: "0a:58:c0:a8:02:03",
					},
				},
			},
		}

		It("Returns true if dpu.connection-status is present and Status is Ready", func() {
			podAnnot := map[string]string{
				util.DPUConnetionStatusAnnot: `{"Status":"Ready"}`}
			Expect(isDPUReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(true))
		})

		It("Returns false if dpu.connection-status is present and Status is not Ready", func() {
			podAnnot := map[string]string{
				util.DPUConnetionStatusAnnot: `{"Status":"NotReady"}`}
			Expect(isDPUReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(false))
		})

		It("Returns false if dpu.connection-status Status is not present", func() {
			podAnnot := map[string]string{
				util.DPUConnetionStatusAnnot: `{"Foo":"Bar"}`}
			Expect(isDPUReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(false))
		})

		It("Returns false if dpu.connection-status is not present", func() {
			podAnnot := map[string]string{}
			Expect(isDPUReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(false))
		})

		It("Returns false if OVN pod-networks is not present", func() {
			podAnnot := map[string]string{}
			Expect(isDPUReady(podAnnot, podNetwork, ovntypes.DefaultNetworkName)).To(Equal(false))
		})
	})

	Context("GetPodNetworkInfo", func() {
		var podNamespaceLister mocks.PodNamespaceLister
		var pod *v1.Pod

		const (
			namespace = "some-ns"
			podName   = "some-pod"
		)

		BeforeEach(func() {
			podNamespaceLister = mocks.PodNamespaceLister{}
			pod = newPod(namespace, podName, nil)
		})

		It("Returns Pod annotation and PodNetwork if annotation condition is met", func() {
			podAnnot := map[string]string{"foo": "bar"}
			pod.Annotations = podAnnot
			ctx, cancelFunc := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				if _, ok := podAnnotation["foo"]; ok {
					return true
				}
				return false
			}

			podNet := newPodNet(namespace, podName)
			clientset := newFakeClientSet(pod, podNet, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			uid, annot, podNetFetched, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
			Expect(podNetFetched).To(Equal(podNet))
			Expect(annot).To(Equal(podAnnot))
			Expect(uid).To(Equal(string(pod.UID)))
		})

		It("Returns with Error if context is canceled", func() {
			ctx, cancelFunc := context.WithCancel(context.Background())

			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				return false
			}

			go func() {
				time.Sleep(20 * time.Millisecond)
				cancelFunc()
			}()

			clientset := newFakeClientSet(pod, nil, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
		})

		It("Retries Until pod network condition is met", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancelFunc()

			calledOnce := false
			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				if calledOnce {
					return true
				}
				calledOnce = true
				return false
			}

			clientset := newFakeClientSet(pod, newPodNet(namespace, podName), &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if PodLister fails to get pod annotations", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				return true
			}

			clientset := newFakeClientSet(pod, nil, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, fmt.Errorf("failed to list pods"))
			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list pods"))
		})

		It("Tries kube client if PodLister can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			calledOnce := false
			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				if calledOnce {
					return true
				}
				calledOnce = true
				return false
			}

			clientset := newFakeClientSet(pod, newPodNet(namespace, podName), &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.NewNotFound(v1.Resource("pod"), name))
			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Returns an error if PodLister and kube client can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				return false
			}

			clientset := newFakeClientSet(nil, nil, &podNamespaceLister)

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.NewNotFound(v1.Resource("pod"), name))
			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out waiting for pod after 1s"))
		})

		It("Fails if PodNetworkLister fails to get podNetwork", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, podNetwork *podnetworkapi.PodNetwork, netName string) bool {
				return true
			}

			clientset := newFakeClientSet(pod, nil, &podNamespaceLister)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)

			_, _, _, err := GetPodNetworkInfo(ctx, clientset, namespace, podName, ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out waiting for podNetwork after 1s"))
		})
	})

	Context("PodNetworkAndAnnotation2PodInfo", func() {
		podAnnot := map[string]string{}
		podNetwork := &podnetworkapi.PodNetwork{
			// don't init ObjectMeta, since it is not used in isOvnReady
			Spec: podnetworkapi.PodNetworkSpec{
				Networks: map[string]podnetworkapi.OVNNetwork{
					ovntypes.DefaultNetworkName: {
						IPs:      []string{"192.168.2.3/24"},
						MAC:      "0a:58:c0:a8:02:03",
						Gateways: []string{"192.168.2.1"},
					},
				},
			},
		}
		podUID := "4d06bae8-9c38-41f6-945c-f92320e782e4"
		It("Creates PodInterfaceInfo in NodeModeFull mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeFull
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeFalse())
		})

		It("Creates PodInterfaceInfo in NodeModeDPUHost mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with checkExtIDs false", func() {
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeFalse())
		})

		It("Creates PodInterfaceInfo with checkExtIDs true", func() {
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, true, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = true
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeTrue())
		})

		It("Creates PodInterfaceInfo without EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = false
			pif, err := PodNetworkAndAnnotation2PodInfo(podAnnot, podNetwork, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeFalse())
		})
	})
})
