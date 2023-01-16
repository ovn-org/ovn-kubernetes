package cni

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"time"
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

func newFakeKubeClientWithPod(pod *v1.Pod) *fake.Clientset {
	return fake.NewSimpleClientset(&v1.PodList{Items: []v1.Pod{*pod}})
}

var _ = Describe("CNI Utils tests", func() {
	var defaultPodAnnotation string
	BeforeEach(func() {
		defaultPodAnnotation = `{
  "default":{"ip_addresses":["192.168.2.3/24"],
  "mac_address":"0a:58:c0:a8:02:03",
  "gateway_ips":["192.168.2.1"],
  "ip_address":"192.168.2.3/24",
  "gateway_ip":"192.168.2.1"}
}`
	})

	Context("isOvnReady", func() {
		It("Returns true if OVN pod network annotation exists", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: defaultPodAnnotation}
			_, ready := isOvnReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(true))
		})

		It("Returns false if OVN pod network annotation does not exist", func() {
			podAnnot := map[string]string{}
			_, ready := isOvnReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(false))
		})
	})

	Context("isDPUReady", func() {
		It("Returns true if dpu.connection-status is present and Status is Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:    defaultPodAnnotation,
				util.DPUConnetionStatusAnnot: `{"Status":"Ready"}`}
			_, ready := isDPUReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(true))
		})

		It("Returns false if dpu.connection-status is present and Status is not Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:    defaultPodAnnotation,
				util.DPUConnetionStatusAnnot: `{"Status":"NotReady"}`}
			_, ready := isDPUReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(false))
		})

		It("Returns false if dpu.connection-status Status is not present", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:    defaultPodAnnotation,
				util.DPUConnetionStatusAnnot: `{"Foo":"Bar"}`}
			_, ready := isDPUReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(false))
		})

		It("Returns false if dpu.connection-status is not present", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: defaultPodAnnotation}
			_, ready := isDPUReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(false))
		})

		It("Returns false if OVN pod-networks is not present", func() {
			podAnnot := map[string]string{}
			_, ready := isDPUReady(podAnnot, ovntypes.DefaultNetworkName)
			Expect(ready).To(Equal(false))
		})
	})

	Context("GetPodAnnotations", func() {
		var podLister mocks.PodLister
		var podNamespaceLister mocks.PodNamespaceLister
		var pod *v1.Pod

		BeforeEach(func() {
			podNamespaceLister = mocks.PodNamespaceLister{}
			pod = newPod("some-ns", "some-pod", nil)
			podLister = mocks.PodLister{}
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
		})

		It("Returns Pod annotation if annotation condition is met", func() {
			podAnnot := map[string]string{"foo": "bar"}
			pod.Annotations = podAnnot
			ctx, cancelFunc := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				if _, ok := podAnnotation["foo"]; ok {
					return nil, true
				}
				return nil, false
			}

			fakeClient := newFakeKubeClientWithPod(pod)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			uid, annot, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
			Expect(annot).To(Equal(podAnnot))
			Expect(uid).To(Equal(string(pod.UID)))
		})

		It("Returns with Error if context is canceled", func() {
			ctx, cancelFunc := context.WithCancel(context.Background())

			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				return nil, false
			}

			go func() {
				time.Sleep(20 * time.Millisecond)
				cancelFunc()
			}()

			fakeClient := newFakeKubeClientWithPod(pod)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
		})

		It("Retries Until pod annotation condition is met", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancelFunc()

			calledOnce := false
			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				if calledOnce {
					return nil, true
				}
				calledOnce = true
				return nil, false
			}

			fakeClient := newFakeKubeClientWithPod(pod)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			_, _, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if PodLister fails to get pod annotations", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				return nil, false
			}

			fakeClient := newFakeKubeClientWithPod(pod)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, fmt.Errorf("failed to list pods"))
			_, _, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list pods"))
		})

		It("Tries kube client if PodLister can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			calledOnce := false
			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				if calledOnce {
					return nil, true
				}
				calledOnce = true
				return nil, false
			}

			fakeClient := newFakeKubeClientWithPod(pod)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.NewNotFound(v1.Resource("pod"), name))
			_, _, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Returns an error if PodLister and kube client can't find the pod", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string, netName string) (*util.PodAnnotation, bool) {
				return nil, false
			}

			fakeClient := fake.NewSimpleClientset(&v1.PodList{Items: []v1.Pod{}})
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.NewNotFound(v1.Resource("pod"), name))
			_, _, _, err := GetPodAnnotations(ctx, &podLister, fakeClient, "some-ns", "some-pod", ovntypes.DefaultNetworkName, cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out waiting for pod after 1s"))
		})
	})

	Context("PodAnnotation2PodInfo", func() {
		podAnnot := map[string]string{
			util.OvnPodAnnotationName: `{
"default":{"ip_addresses":["192.168.2.3/24"],
"mac_address":"0a:58:c0:a8:02:03",
"gateway_ips":["192.168.2.1"],
"ip_address":"192.168.2.3/24",
"gateway_ip":"192.168.2.1"}}`,
		}
		podUID := "4d06bae8-9c38-41f6-945c-f92320e782e4"
		It("Creates PodInterfaceInfo in NodeModeFull mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeFull
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeFalse())
		})

		It("Creates PodInterfaceInfo in NodeModeDPUHost mode", func() {
			config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsDPUHostMode).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with checkExtIDs false", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeFalse())
		})

		It("Creates PodInterfaceInfo with checkExtIDs true", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, true, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = true
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeTrue())
		})

		It("Creates PodInterfaceInfo without EnableUDPAggregation", func() {
			config.Default.EnableUDPAggregation = false
			pif, err := PodAnnotation2PodInfo(podAnnot, nil, false, podUID, "", ovntypes.DefaultNetworkName, ovntypes.DefaultNetworkName, config.Default.MTU)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.EnableUDPAggregation).To(BeFalse())
		})
	})
})
