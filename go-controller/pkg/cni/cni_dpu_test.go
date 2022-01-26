package cni

import (
	"fmt"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("cni_dpu tests", func() {
	var fakeKubeInterface kubeMocks.KubeInterface
	var fakeSriovnetOps utilMocks.SriovnetOps
	var pr PodRequest
	var pod *v1.Pod

	BeforeEach(func() {
		fakeKubeInterface = kubeMocks.KubeInterface{}
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
			timestamp:        time.Time{},
			effectiveNetName: ovntypes.DefaultNetworkName,
			effectiveNADName: ovntypes.DefaultNetworkName,
		}

		pod = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pr.PodName,
				Namespace:   pr.PodNamespace,
				Annotations: map[string]string{},
			},
		}
	})
	Context("addDPUConnectionDetailsAnnot", func() {
		It("Sets dpu.connection-details pod annotation", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			dpuCd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			fakeKubeInterface.On("GetPod", pr.PodNamespace, pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			err := util.MarshalPodDPUConnDetails(&cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePod", cpod).Return(nil)

			err = pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if DeviceID is not present in CNI config", func() {
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get PF PCI from VF PCI", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return(
				"", fmt.Errorf("failed to get PF address"))
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get VF index from PF PCI address", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get VF index"))
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if PF PCI address fails to parse", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			err := pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).To(HaveOccurred())
		})

		It("Fails if Set annotation on Pod fails", func() {
			pod.Annotations = map[string]string{}
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			dpuCd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			fakeKubeInterface.On("GetPod", pr.PodNamespace, pr.PodName).Return(pod, nil)
			cpod := pod.DeepCopy()
			err := util.MarshalPodDPUConnDetails(&cpod.Annotations, &dpuCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePod", cpod).Return(fmt.Errorf("failed to set annotation"))
			err = pr.addDPUConnectionDetailsAnnot(&fakeKubeInterface, "")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set annotation"))
		})
	})
})
