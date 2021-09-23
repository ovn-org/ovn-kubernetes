package cni

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (pr *PodRequest) addSmartNICConnectionDetailsAnnot(kube kube.Interface) error {
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

	smartNicAnnotation, err := smartNicConnDetails.AsAnnotation()
	if err != nil {
		// we should not get here
		return fmt.Errorf("failed to generate %s annotation for pod. %v", util.SmartNicConnectionDetailsAnnot, err)
	}

	annot := make(map[string]interface{}, len(smartNicAnnotation))
	for key, val := range smartNicAnnotation {
		annot[key] = val
	}
	err = kube.SetAnnotationsOnPod(pr.PodNamespace, pr.PodName, annot)
	if err != nil {
		return err
	}

	return nil
}
