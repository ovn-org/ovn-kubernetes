package udn

import (
	"fmt"

	"k8s.io/klog/v2"

	nadlister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	cnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// wait on a certain pod annotation related condition
type podAnnotWaitCond = func(map[string]string, string) (*util.PodAnnotation, bool)

type UserDefinedPrimaryNetwork struct {
	nadLister     nadlister.NetworkAttachmentDefinitionLister
	annotation    *util.PodAnnotation
	activeNetwork *ovncnitypes.NetConf
}

func NewPrimaryNetwork(nadLister nadlister.NetworkAttachmentDefinitionLister) *UserDefinedPrimaryNetwork {
	return &UserDefinedPrimaryNetwork{
		nadLister: nadLister,
	}
}

func (p *UserDefinedPrimaryNetwork) InterfaceName() string {
	return "sdn1"
}

func (p *UserDefinedPrimaryNetwork) Annotation() *util.PodAnnotation {
	return p.annotation
}

func (p *UserDefinedPrimaryNetwork) NetworkName() string {
	if p.activeNetwork == nil {
		return ""
	}
	return p.activeNetwork.Name
}

func (p *UserDefinedPrimaryNetwork) NADName() string {
	if p.activeNetwork == nil {
		return ""
	}
	return p.activeNetwork.NADName
}

func (p *UserDefinedPrimaryNetwork) NetConf() *cnitypes.NetConf {
	return p.activeNetwork
}

func (p *UserDefinedPrimaryNetwork) Found() bool {
	return p.annotation != nil && p.activeNetwork != nil
}

func (p *UserDefinedPrimaryNetwork) WaitForPrimaryAnnotationFn(namespace string, annotCondFn podAnnotWaitCond) podAnnotWaitCond {
	return func(annotations map[string]string, nadName string) (*util.PodAnnotation, bool) {
		annotation, isReady := annotCondFn(annotations, nadName)
		if nadName != types.DefaultNetworkName || annotation.Role == types.NetworkRolePrimary {
			return annotation, isReady
		}

		if err := p.ensureAnnotation(annotations); err != nil {
			klog.Warningf("Failed looking for primary network annotation: %v", err)
			return nil, false
		}
		if err := p.ensureActiveNetwork(namespace); err != nil {
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
	activeNetwork, err := util.FindActiveNetworkForNamespace(namespace, p.nadLister)
	if err != nil {
		return err
	}
	if activeNetwork == nil {
		return fmt.Errorf("unknown primary network for namespace '%s'", namespace)
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
	return nil
}
