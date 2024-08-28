package udn

import (
	"fmt"

	"k8s.io/klog/v2"

	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// wait on a certain pod annotation related condition
type podAnnotWaitCond = func(map[string]string, string) (*util.PodAnnotation, bool)

type UserDefinedPrimaryNetwork struct {
	nadController nad.NADController
	annotation    *util.PodAnnotation
	activeNetwork util.NetInfo
}

func NewPrimaryNetwork(nadController nad.NADController) *UserDefinedPrimaryNetwork {
	return &UserDefinedPrimaryNetwork{
		nadController: nadController,
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
		if err := p.ensure(namespace, annotations, nadName, annotation); err != nil {
			klog.Errorf("Failed ensuring user defined primary network: %v", err)
			return nil, false
		}
		return annotation, isReady
	}
}

func (p *UserDefinedPrimaryNetwork) Ensure(namespace string, annotations map[string]string, nadName string) error {
	return p.ensure(namespace, annotations, nadName, nil /* parse annotation */)
}

func (p *UserDefinedPrimaryNetwork) ensure(namespace string, annotations map[string]string, nadName string, annotation *util.PodAnnotation) error {
	// non default network is not related to primary UDNs
	if nadName != types.DefaultNetworkName {
		return nil
	}

	if annotation == nil {
		var err error
		annotation, err = util.UnmarshalPodAnnotation(annotations, nadName)
		if err != nil {
			return fmt.Errorf("failed looking for ovn pod annotations for nad '%s': %w", nadName, err)
		}
	}

	// If these are pods created before primary UDN functionality the
	// default network without role is the primary network
	if annotation.Role == "" {
		annotation.Role = types.NetworkRolePrimary
	}

	// If default network is the primary there is nothing else to do
	if annotation.Role == types.NetworkRolePrimary {
		return nil
	}

	if err := p.ensureAnnotation(annotations); err != nil {
		return fmt.Errorf("failed looking for primary network annotation: %w", err)
	}
	if err := p.ensureActiveNetwork(namespace); err != nil {
		return fmt.Errorf("failed looking for primary network name: %w", err)
	}
	return nil
}

func (p *UserDefinedPrimaryNetwork) ensureActiveNetwork(namespace string) error {
	if p.activeNetwork != nil {
		return nil
	}
	activeNetwork, err := p.nadController.GetActiveNetworkForNamespace(namespace)
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
