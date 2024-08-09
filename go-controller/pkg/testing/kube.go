package testing

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"
)

// USED ONLY FOR TESTING

// makeReadyEndpointList returns a list of only one endpoint that carries the input addresses.
func MakeReadyEndpointList(node string, addresses ...string) []discovery.Endpoint {
	return []discovery.Endpoint{
		MakeReadyEndpoint(node, addresses...),
	}
}

func MakeReadyEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(true),
			Serving:     ptr.To(true),
			Terminating: ptr.To(false),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MakeTerminatingServingEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(false),
			Serving:     ptr.To(true),
			Terminating: ptr.To(true),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MakeTerminatingNonServingEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(false),
			Serving:     ptr.To(false),
			Terminating: ptr.To(true),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MirrorEndpointSlice(defaultEndpointSlice *discovery.EndpointSlice, network string, keepEndpoints bool) *discovery.EndpointSlice {
	mirror := defaultEndpointSlice.DeepCopy()
	mirror.Name = defaultEndpointSlice.Name + "-mirrored"
	mirror.Labels[discovery.LabelManagedBy] = types.EndpointSliceMirrorControllerName
	mirror.Labels[types.LabelSourceEndpointSlice] = defaultEndpointSlice.Name
	mirror.Labels[types.LabelUserDefinedEndpointSliceNetwork] = network
	mirror.Labels[types.LabelUserDefinedServiceName] = defaultEndpointSlice.Labels[discovery.LabelServiceName]

	if !keepEndpoints {
		mirror.Endpoints = nil
	}

	return mirror
}
