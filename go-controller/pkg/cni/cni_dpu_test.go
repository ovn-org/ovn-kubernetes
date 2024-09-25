package cni

import (
	"fmt"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("cni_dpu tests", func() {
	var fakeKubeInterface kubeMocks.Interface
	var fakeSriovnetOps utilMocks.SriovnetOps
	var pr PodRequest
	var pod *v1.Pod
	var podLister v1mocks.PodLister
	var podNamespaceLister v1mocks.PodNamespaceLister

	BeforeEach(func() {
		fakeKubeInterface = kubeMocks.Interface{}
		fakeSriovnetOps = utilMocks.SriovnetOps{}
		util.SetSriovnetOpsInst(&fakeSriovnetOps)
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
		pod = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pr.PodName,
				Namespace:   pr.PodNamespace,
				Annotations: map[string]string{},
			},
		}
		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
	})
	Context("addDPUConnectionDetailsAnnot", func() {
		It("Sets dpu.connection-details pod annotation", func() {
			var err error
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(0, nil)
			dpuCd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePodStatus", cpod).Return(nil)
			err = pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).ToNot(HaveOccurred())

		})

		It("Fails if DeviceID is not present in CNI config", func() {
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get PF Index from VF PCI", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get PF Index"))
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get VF index from PF PCI address", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get VF index"))
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(0, nil)
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if PF PCI address fails to parse", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to parse PF PCI address"))
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if Set annotation on Pod fails", func() {
			var err error
			pod.Annotations = map[string]string{}
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			fakeSriovnetOps.On("GetPfIndexByVfPciAddress", pr.CNIConf.DeviceID).Return(0, nil)
			dpuCd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			podLister.On("Pods", pr.PodNamespace).Return(&podNamespaceLister)
			podNamespaceLister.On("Get", pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePodStatus", cpod).Return(fmt.Errorf("failed to set annotation"))
			err = pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, &podLister, "")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set annotation"))
		})
	})
})
