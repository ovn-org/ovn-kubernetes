package cni

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/client-go/util/retry"
)

// updatePodSmartNicConnDetailsWithRetry update the pod annotion with the givin connection details
func (pr *PodRequest) updatePodSmartNicConnDetailsWithRetry(kube kube.Interface, smartNicConnDetails *util.SmartNICConnectionDetails) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := kube.GetPod(pr.PodNamespace, pr.PodName)
		if err != nil {
			return err
		}

		cpod := pod.DeepCopy()
		err = util.MarshalPodSmartNicConnDetails(&cpod.Annotations, smartNicConnDetails, types.DefaultNetworkName)
		if err != nil {
			return err
		}
		return kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update %s annotation on pod %s/%s: %v",
			util.SmartNicConnectionDetailsAnnot, pr.PodNamespace, pr.PodName, resultErr)
	}
	return nil
}

func (pr *PodRequest) addSmartNICConnectionDetailsAnnot(k kube.Interface) error {
	// 1. Verify there is a device id
	if pr.CNIConf.DeviceID == "" {
		return fmt.Errorf("DeviceID must be set for Pod request with SmartNIC")
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

	// 3. Set smart-nic connection-details pod annotation
	var domain, bus, dev, fn int
	parsed, err := fmt.Sscanf(pfPciAddress, "%04x:%02x:%02x.%d", &domain, &bus, &dev, &fn)
	if err != nil {
		return fmt.Errorf("error trying to parse PF PCI address %s: %v", pfPciAddress, err)
	}
	if parsed != 4 {
		return fmt.Errorf("failed to parse PF PCI address %s. Unexpected format", pfPciAddress)
	}

	smartNicConnDetails := util.SmartNICConnectionDetails{
		PfId:      fmt.Sprint(fn),
		VfId:      fmt.Sprint(vfindex),
		SandboxId: pr.SandboxID,
	}

	return pr.updatePodSmartNicConnDetailsWithRetry(k, &smartNicConnDetails)
}
