package cni

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1listers "k8s.io/client-go/listers/core/v1"
)

// updatePodDPUConnDetailsWithRetry update the pod annotation with the given connection details for the NAD in
// the PodRequest. If the dpuConnDetails argument is nil, delete the NAD's DPU connection details annotation instead.
func (pr *PodRequest) updatePodDPUConnDetailsWithRetry(kube kube.Interface, podLister corev1listers.PodLister, dpuConnDetails *util.DPUConnectionDetails) error {
	pod, err := podLister.Pods(pr.PodNamespace).Get(pr.PodName)
	if err != nil {
		return err
	}
	err = util.UpdatePodDPUConnDetailsWithRetry(
		podLister,
		kube,
		pod,
		dpuConnDetails,
		pr.nadName,
	)
	if util.IsAnnotationAlreadySetError(err) {
		return nil
	}

	return err
}

func (pr *PodRequest) addDPUConnectionDetailsAnnot(k kube.Interface, podLister corev1listers.PodLister, vfNetdevName string) error {
	if pr.CNIConf.DeviceID == "" {
		return fmt.Errorf("DeviceID must be set for Pod request with DPU")
	}
	pciAddress := pr.CNIConf.DeviceID

	vfindex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(pciAddress)
	if err != nil {
		return err
	}
	pfindex, err := util.GetSriovnetOps().GetPfIndexByVfPciAddress(pciAddress)
	if err != nil {
		return err
	}

	dpuConnDetails := util.DPUConnectionDetails{
		PfId:         fmt.Sprint(pfindex),
		VfId:         fmt.Sprint(vfindex),
		SandboxId:    pr.SandboxID,
		VfNetdevName: vfNetdevName,
	}

	return pr.updatePodDPUConnDetailsWithRetry(k, podLister, &dpuConnDetails)
}
