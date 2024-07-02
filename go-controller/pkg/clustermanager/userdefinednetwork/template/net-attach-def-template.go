package template

import (
	"errors"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

// RenderNetAttachDefManifest return NetworkAttachmentDefinition according to the given UserDefinedNetwork spec
func RenderNetAttachDefManifest(_ *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	// TODO: implement NAD creation from UDN spec
	return nil, errors.New("implement me")
}
