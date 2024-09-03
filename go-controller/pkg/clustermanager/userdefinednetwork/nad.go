package userdefinednetwork

import (
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	cnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// NetAttachDefNotInUse checks no pod is attached to given NAD.
// Pod considered not attached to network in case its OVN pod annotation doesn't specify
// the given NAD key.
func NetAttachDefNotInUse(nad *netv1.NetworkAttachmentDefinition, pods []*v1.Pod) error {
	nadName := util.GetNADName(nad.Namespace, nad.Name)
	var connectedPods []string
	for _, pod := range pods {
		podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to verify NAD not in use [%[1]s/%[2]s]: failed to unmarshal pod annotation [%[1]s/%[3]s]: %[4]w",
				nad.Namespace, nad.Name, pod.Name, err)
		}
		if _, ok := podNetworks[nadName]; ok {
			connectedPods = append(connectedPods, pod.Namespace+"/"+pod.Name)
		}
	}
	if len(connectedPods) > 0 {
		return fmt.Errorf("network in use by the following pods: %v", connectedPods)
	}
	return nil
}

// PrimaryNetAttachDefNotExist checks no OVN-K primary network NAD exist in the given slice.
func PrimaryNetAttachDefNotExist(nads []*netv1.NetworkAttachmentDefinition) error {
	for _, nad := range nads {
		var netConf *cnitypes.NetConf
		if err := json.Unmarshal([]byte(nad.Spec.Config), &netConf); err != nil {
			return fmt.Errorf("failed to validate no primary network exist: unmarshal failed [%s/%s]: %w",
				nad.Namespace, nad.Name, err)
		}
		if netConf.Type == template.OvnK8sCNIOverlay && netConf.Role == ovntypes.NetworkRolePrimary {
			return fmt.Errorf("primary network already exist in namespace %q: %q", nad.Namespace, nad.Name)
		}
	}
	return nil
}
