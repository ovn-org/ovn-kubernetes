// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	kubeMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// dpuTestCtxKey tags the request context so it is distinguishable from a fresh
// root, letting the kube mock assert the request's own context reaches the API.
type dpuTestCtxKey struct{}

var _ = Describe("cni_dpu tests", func() {
	var fakeKubeInterface kubeMocks.Interface
	var fakeSriovnetOps utilMocks.SriovnetOps
	var pr PodRequest
	var prCtx context.Context
	var pod *corev1.Pod
	var podLister v1mocks.PodLister
	var podNamespaceLister v1mocks.PodNamespaceLister

	BeforeEach(func() {
		fakeKubeInterface = kubeMocks.Interface{}
		fakeSriovnetOps = utilMocks.SriovnetOps{}
		util.SetSriovnetOpsInst(&fakeSriovnetOps)
		prCtx = context.WithValue(context.Background(), dpuTestCtxKey{}, "cni_dpu")
		pr = PodRequest{
			ctx:          prCtx,
			Command:      CNIAdd,
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			SandboxID:    "824bceff24af3",
			Netns:        "ns",
			IfName:       "eth0",
			CNIConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{},
				DeviceID: "",
			},
			timestamp:  time.Time{},
			IsVFIO:     false,
			netName:    ovntypes.DefaultNetworkName,
			nadName:    ovntypes.DefaultNetworkName,
			nadKey:     ovntypes.DefaultNetworkName,
			deviceInfo: nadapi.DeviceInfo{},
		}
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pr.PodName,
				Namespace:   pr.PodNamespace,
				Annotations: map[string]string{},
			},
		}
		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
	})
	Context("allocateDPUConnectionDetails", func() {
		It("Resolves the connection details for the VF", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(0, nil)
			dpuCd, err := pr.allocateDPUConnectionDetails("ens1f0v2")
			Expect(err).ToNot(HaveOccurred())
			Expect(*dpuCd).To(Equal(util.DPUConnectionDetails{
				PfId:         "0",
				VfId:         "2",
				SandboxId:    pr.SandboxID,
				VfNetdevName: "ens1f0v2",
			}))
		})

		It("Fails if DeviceID is not present in CNI config", func() {
			_, err := pr.allocateDPUConnectionDetails("")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get PF Index from VF PCI", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get PF Index"))
			_, err := pr.allocateDPUConnectionDetails("")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get VF index from PF PCI address", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get VF index"))
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(0, nil)
			_, err := pr.allocateDPUConnectionDetails("")
			Expect(err).To(HaveOccurred())
		})
	})

	Context("updatePodDPUConnDetailsWithRetry", func() {
		dpuCd := util.DPUConnectionDetails{
			PfId:      "0",
			VfId:      "2",
			SandboxId: "824bceff24af3",
		}

		It("Sets dpu.connection-details pod annotation", func() {
			var err error
			podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("PatchPodStatusAnnotations", prCtx, pod, cpod).Return(nil)
			err = pr.updatePodDPUConnDetailsWithRetry(&fakeKubeInterface, &podLister, pod, &dpuCd)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Deletes the NAD's dpu.connection-details from the pod annotation", func() {
			var err error
			pod.Annotations, err = util.MarshalPodDPUConnDetails(pod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, nil, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("PatchPodStatusAnnotations", prCtx, pod, cpod).Return(nil)
			err = pr.updatePodDPUConnDetailsWithRetry(&fakeKubeInterface, &podLister, pod, nil)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if Set annotation on Pod fails", func() {
			var err error
			podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("PatchPodStatusAnnotations", prCtx, pod, cpod).Return(fmt.Errorf("failed to set annotation"))
			err = pr.updatePodDPUConnDetailsWithRetry(&fakeKubeInterface, &podLister, pod, &dpuCd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set annotation"))
		})
	})
})
