package cni

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	"time"
)

var _ = Describe("CNI Utils tests", func() {
	Context("isOvnReady", func() {
		It("Returns true if OVN pod network annotation exists", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: `{
																		"default":{"ip_addresses":["192.168.2.3/24"],
																		"mac_address":"0a:58:c0:a8:02:03",
																		"gateway_ips":["192.168.2.1"],
																		"ip_address":"192.168.2.3/24",
																		"gateway_ip":"192.168.2.1"}
																		}`}
			Expect(isOvnReady(podAnnot)).To(Equal(true))
		})

		It("Returns false if OVN pod network annotation does not exist", func() {
			podAnnot := map[string]string{}
			Expect(isOvnReady(podAnnot)).To(Equal(false))
		})
	})

	Context("isSmartNICReady", func() {
		It("Returns true if smartnic.connection-status is present and Status is Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:         `{"ip_address": "192.168.2.3/24"}`,
				util.SmartNicConnetionStatusAnnot: `{"Status":"Ready"}`}
			Expect(isSmartNICReady(podAnnot)).To(Equal(true))
		})

		It("Returns false if smartnic.connection-status is present and Status is not Ready", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:         `{"ip_address": "192.168.2.3/24"}`,
				util.SmartNicConnetionStatusAnnot: `{"Status":"NotReady"}`}
			Expect(isSmartNICReady(podAnnot)).To(Equal(false))
		})

		It("Returns false if smartnic.connection-status Status is not present", func() {
			podAnnot := map[string]string{
				util.OvnPodAnnotationName:         `{"ip_address": "192.168.2.3/24"}`,
				util.SmartNicConnetionStatusAnnot: `{"Foo":"Bar"}`}
			Expect(isSmartNICReady(podAnnot)).To(Equal(false))
		})

		It("Returns false if smartnic.connection-status is not present", func() {
			podAnnot := map[string]string{util.OvnPodAnnotationName: `{"ip_address": "192.168.2.3/24"}`}
			Expect(isSmartNICReady(podAnnot)).To(Equal(false))
		})

		It("Returns false if OVN pod-networks is not present", func() {
			podAnnot := map[string]string{}
			Expect(isSmartNICReady(podAnnot)).To(Equal(false))
		})
	})

	Context("GetPodAnnotations", func() {
		var podLister mocks.PodLister
		var podNamespaceLister mocks.PodNamespaceLister
		var pod v1.Pod

		BeforeEach(func() {
			podNamespaceLister = mocks.PodNamespaceLister{}
			pod = v1.Pod{}
			podLister = mocks.PodLister{}
			podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)

		})

		It("Returns Pod annotation if annotation condition is met", func() {
			podAnnot := map[string]string{"foo": "bar"}
			pod.Annotations = podAnnot
			ctx, cancelFunc := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string) bool {
				if _, ok := podAnnotation["foo"]; ok {
					return true
				}
				return false
			}

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
			annot, err := GetPodAnnotations(ctx, &podLister, "some-ns", "some-pod", cond)
			Expect(err).ToNot(HaveOccurred())
			Expect(annot).To(Equal(podAnnot))
		})

		It("Returns with Error if context is canceled", func() {
			ctx, cancelFunc := context.WithCancel(context.Background())

			cond := func(podAnnotation map[string]string) bool {
				return false
			}

			go func() {
				time.Sleep(20 * time.Millisecond)
				cancelFunc()
			}()

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
			_, err := GetPodAnnotations(ctx, &podLister, "some-ns", "some-pod", cond)
			Expect(err).To(HaveOccurred())
		})

		It("Retries Until pod annotation condition is met", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancelFunc()

			calledOnce := false
			cond := func(podAnnotation map[string]string) bool {
				if calledOnce {
					return true
				}
				calledOnce = true
				return false
			}

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
			_, err := GetPodAnnotations(ctx, &podLister, "some-ns", "some-pod", cond)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if PodLister fails to get pod annotations", func() {
			ctx, cancelFunc := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancelFunc()

			cond := func(podAnnotation map[string]string) bool {
				return false
			}

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, fmt.Errorf("failed to list pods"))
			_, err := GetPodAnnotations(ctx, &podLister, "some-ns", "some-pod", cond)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list pods"))
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
		It("Creates PodInterfaceInfo with IsSmartNIC false", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, false, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsSmartNic).To(BeFalse())
		})

		It("Creates PodInterfaceInfo with IsSmartNIC true", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, false, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.IsSmartNic).To(BeTrue())
		})

		It("Creates PodInterfaceInfo with checkExtIDs false", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, false, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeFalse())
		})

		It("Creates PodInterfaceInfo with checkExtIDs true", func() {
			pif, err := PodAnnotation2PodInfo(podAnnot, true, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(pif.CheckExtIDs).To(BeTrue())
		})
	})
})
