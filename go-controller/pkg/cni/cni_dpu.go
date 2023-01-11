package cni

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
)

// updatePodDPUConnDetailsWithRetry update the pod annotation with the given connection details
func (pr *PodRequest) updatePodDPUConnDetailsWithRetry(kube kube.Interface, podLister corev1listers.PodLister, dpuConnDetails *util.DPUConnectionDetails) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := podLister.Pods(pr.PodNamespace).Get(pr.PodName)
		if err != nil {
			return err
		}

		cpod := pod.DeepCopy()
		cpod.Annotations, err = util.MarshalPodDPUConnDetails(cpod.Annotations, dpuConnDetails, pr.nadName)
		if err != nil {
			return err
		}
		return kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update %s annotation on pod %s/%s for NAD %s: %v",
			util.DPUConnectionDetailsAnnot, pr.PodNamespace, pr.PodName, pr.nadName, resultErr)
	}
	return nil
}

func (pr *PodRequest) addDPUConnectionDetailsAnnot(k kube.Interface, podLister corev1listers.PodLister, vfNetdevName string) error {
	// 1. Verify there is a device id
	if pr.CNIConf.DeviceID == "" {
		return fmt.Errorf("DeviceID must be set for Pod request with DPU")
	}
	pciAddress := pr.CNIConf.DeviceID

	// 2. Get the PF index and VF index
	pfPciAddress, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddress)
	if err != nil {
		return err
	}
	vfindex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(pciAddress)
	if err != nil {
		return err
	}

	// 3. Set dpu connection-details pod annotation
	var domain, bus, dev, fn int
	parsed, err := fmt.Sscanf(pfPciAddress, "%04x:%02x:%02x.%d", &domain, &bus, &dev, &fn)
	if err != nil {
		return fmt.Errorf("error trying to parse PF PCI address %s: %v", pfPciAddress, err)
	}
	if parsed != 4 {
		return fmt.Errorf("failed to parse PF PCI address %s. Unexpected format", pfPciAddress)
	}

	dpuConnDetails := util.DPUConnectionDetails{
		PfId:         fmt.Sprint(fn),
		VfId:         fmt.Sprint(vfindex),
		SandboxId:    pr.SandboxID,
		VfNetdevName: vfNetdevName,
	}

	return pr.updatePodDPUConnDetailsWithRetry(k, podLister, &dpuConnDetails)
}
