package cni

// contains code for cnishim - one that gets called as the cni Plugin
// This does not do the real cni work. This is just the client to the cniserver
// that does the real work.

import (
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Plugin is the structure to hold the endpoint information and the corresponding
// functions to use it
type SmartNicPlugin struct {
}

// NewCNISmartNicPlugin creates the internal Plugin object
func NewCNISmartNicPlugin() *SmartNicPlugin {
	return &SmartNicPlugin{}
}

// CmdAdd is the callback for 'add' cni calls from skel
func (bp *SmartNicPlugin) CmdAdd(args *skel.CmdArgs) error {
	// read the config stdin args to obtain cniVersion
	conf, err := config.ReadCNIConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("invalid stdin args")
	}

	req := newCNIRequest(args)
	podReq, err := cniRequestToPodRequest(req)
	if err != nil {
		return fmt.Errorf("cniRequestToPodRequest failed %v", err)
	}

	// 1. Verify there is a device id
	if podReq.CNIConf.DeviceID == "" {
		return fmt.Errorf("DeviceID must be set")
	}
	pciAddress := podReq.CNIConf.DeviceID

	// 2. Get the PF index and VF index
	pfPciAddress, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddress)
	if err != nil {
		return err
	}
	vfindex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(pciAddress)
	if err != nil {
		return err
	}

	// 3. Create kubernetes client
	kConf, err := clientcmd.BuildConfigFromFlags("", conf.Kubeconfig)
	if err != nil {
		return fmt.Errorf("unable to set up client config error %v", err)
	}
	clientset, err := kubernetes.NewForConfig(kConf)
	if err != nil {
		return fmt.Errorf("unable to create a kubernetes client error %v", err)
	}
	kube := &kube.Kube{KClient: clientset}

	// 4. Set smart-nic connection-details pod annotation
	var domain, bus, dev, fn int
	if parsed, err := fmt.Sscanf(
		pfPciAddress, "%04x:%02x:%02x.%d", &domain, &bus, &dev, &fn); err != nil || parsed != 4 {
		return fmt.Errorf("failed to parse PF PCI address %s. Unexpected format", pfPciAddress)
	}

	smartNicConnDetails := util.SmartNICConnectionDetails{
		PfId:      fmt.Sprint(fn),
		VfId:      fmt.Sprint(vfindex),
		SandboxId: podReq.SandboxID,
	}

	smartNicAnnotation, err := smartNicConnDetails.AsAnnotation()
	if err != nil {
		// we should not get here
		return fmt.Errorf("failed to generate %s annotation for pod. %v", util.SmartNicConnectionDetailsAnnot, err)
	}

	err = kube.SetAnnotationsOnPod(podReq.PodNamespace, podReq.PodName, smartNicAnnotation)
	if err != nil {
		return err
	}

	// 5. get POD annotation to check that the vf is configured and ready on the smart-nic side
	annotations, err := GetPodAnnotationsWithBackoff(kube, podReq.PodNamespace, podReq.PodName, true)
	if err != nil {
		return err
	}
	podInterfaceInfo, err := PodAnnotation2PodInfo(annotations)
	if err != nil {
		return fmt.Errorf("failed to convert pod annotation to podInterfaceInfo. %v", err)
	}

	// 6. Move VF to pod namespace
	result, err := podReq.getCNIResult(podInterfaceInfo)
	if err != nil {
		return fmt.Errorf("failed to get CNI Result from pod interface info %v: %v", podInterfaceInfo, err)
	}

	return types.PrintResult(result, conf.CNIVersion)
}

// CmdDel is the callback for 'teardown' cni calls from skel
func (bp *SmartNicPlugin) CmdDel(args *skel.CmdArgs) error {
	return nil
}

// CmdCheck is the callback for 'checking' container's networking is as expected.
// Currently not implemented, so returns `nil`.
func (bp *SmartNicPlugin) CmdCheck(args *skel.CmdArgs) error {
	return nil
}
