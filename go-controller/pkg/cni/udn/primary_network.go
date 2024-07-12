package udn

import (
	"fmt"

	"k8s.io/klog/v2"

	nadlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// wait on a certain pod annotation related condition
type podAnnotWaitCond = func(map[string]string, string) (*util.PodAnnotation, bool)

type UserDefinedPrimaryNetwork struct {
	nadLister     nadlister.NetworkAttachmentDefinitionLister
	annotation    *util.PodAnnotation
	activeNetwork util.NetInfo
}

func NewPrimaryNetwork(nadLister nadlister.NetworkAttachmentDefinitionLister) *UserDefinedPrimaryNetwork {
	return &UserDefinedPrimaryNetwork{
		nadLister: nadLister,
	}
}

func (p *UserDefinedPrimaryNetwork) InterfaceName() string {
	return "ovn-udn1"
}

func (p *UserDefinedPrimaryNetwork) NetworkDevice() string {
	// TODO: Support for non VFIO devices like SRIOV have to be implemented
	return ""
}

func (p *UserDefinedPrimaryNetwork) Annotation() *util.PodAnnotation {
	return p.annotation
}

func (p *UserDefinedPrimaryNetwork) NetworkName() string {
	if p.activeNetwork == nil {
		return ""
	}
	return p.activeNetwork.GetNetworkName()
}

func (p *UserDefinedPrimaryNetwork) NADName() string {
	if p.activeNetwork == nil || p.activeNetwork.IsDefault() {
		return ""
	}
	nads := p.activeNetwork.GetNADs()
	if len(nads) < 1 {
		return ""
	}
	return nads[0]
}

func (p *UserDefinedPrimaryNetwork) MTU() int {
	if p.activeNetwork == nil {
		return 0
	}
	return p.activeNetwork.MTU()
}

func (p *UserDefinedPrimaryNetwork) Found() bool {
	return p.annotation != nil && p.activeNetwork != nil
}

func (p *UserDefinedPrimaryNetwork) WaitForPrimaryAnnotationFn(namespace string, annotCondFn podAnnotWaitCond) podAnnotWaitCond {
	return func(annotations map[string]string, nadName string) (*util.PodAnnotation, bool) {
		annotation, isReady := annotCondFn(annotations, nadName)
		if annotation == nil {
			return nil, false
		}
		if nadName != types.DefaultNetworkName || annotation.Role == types.NetworkRolePrimary {
			return annotation, isReady
		}

		if err := p.ensureAnnotation(annotations); err != nil {
			//TODO: Event ?
			klog.Warningf("Failed looking for primary network annotation: %v", err)
			return nil, false
		}
		if err := p.ensureActiveNetwork(namespace); err != nil {
			//TODO: Event ?
			klog.Warningf("Failed looking for primary network name: %v", err)
			return nil, false
		}
		return annotation, isReady
	}
}

func (p *UserDefinedPrimaryNetwork) ensureActiveNetwork(namespace string) error {
	if p.activeNetwork != nil {
		return nil
	}
	activeNetwork, err := util.GetActiveNetworkForNamespace(namespace, p.nadLister)
	if err != nil {
		return err
	}
	if activeNetwork.IsDefault() {
		return fmt.Errorf("missing primary user defined network NAD")
	}
	p.activeNetwork = activeNetwork
	return nil
}

func (p *UserDefinedPrimaryNetwork) ensureAnnotation(annotations map[string]string) error {
	if p.annotation != nil {
		return nil
	}
	podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(annotations)
	if err != nil {
		return err
	}
	for nadName, podNetwork := range podNetworks {
		if podNetwork.Role != types.NetworkRolePrimary {
			continue
		}
		p.annotation, err = util.UnmarshalPodAnnotation(annotations, nadName)
		if err != nil {
			return err
		}
		break
	}
	if p.annotation == nil {
		return fmt.Errorf("missing network annotation with primary role '%+v'", annotations)
	}
	return nil
}
