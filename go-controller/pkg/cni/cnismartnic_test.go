package cni

import (
	"fmt"
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
	"time"
)

var _ = Describe("cnismartnic tests", func() {
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
			timestamp:  time.Time{},
			ctx:        nil,
			cancel:     nil,
			IsSmartNIC: true,
		}
		pod = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pr.PodName,
				Namespace:   pr.PodNamespace,
				Annotations: map[string]string{},
			},
		}
	})
	Context("addSmartNICConnectionDetailsAnnot", func() {
		It("Sets smartnic.connection-details pod annotation", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			smartNicCd := util.SmartNICConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			fakeKubeInterface.On("GetPod", pr.PodNamespace, pr.PodName).Return(pod, nil)
			err := util.MarshalPodSmartNicConnDetails(&pod.Annotations, &smartNicCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePod", pod).Return(nil)

			err = pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if DeviceID is not present in CNI config", func() {
			err := pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get PF PCI from VF PCI", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return(
				"", fmt.Errorf("failed to get PF address"))
			err := pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if srionvet fails to get VF index from PF PCI address", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(
				-1, fmt.Errorf("failed to get VF index"))
			err := pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if PF PCI address fails to parse", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			err := pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if Set annotation on Pod fails", func() {
			pr.CNIConf.DeviceID = "0000:05:00.4"
			fakeSriovnetOps.On("GetPfPciFromVfPci", pr.CNIConf.DeviceID).Return("0000:05:00.0", nil)
			fakeSriovnetOps.On("GetVfIndexByPciAddress", pr.CNIConf.DeviceID).Return(2, nil)
			smartNicCd := util.SmartNICConnectionDetails{
				PfId:      "0",
				VfId:      "2",
				SandboxId: pr.SandboxID,
			}
			fakeKubeInterface.On("GetPod", pr.PodNamespace, pr.PodName).Return(pod, nil)
			err := util.MarshalPodSmartNicConnDetails(&pod.Annotations, &smartNicCd, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			fakeKubeInterface.On("UpdatePod", pod).Return(fmt.Errorf("failed to set annotation"))
			err = pr.addSmartNICConnectionDetailsAnnot(&fakeKubeInterface, ovntypes.DefaultNetworkName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set annotation"))
		})
	})
})
