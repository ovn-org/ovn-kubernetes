package util

// Contains helper functions for OVN
// Eventually these should all be migrated to go-ovn bindings

import (
	ocpconfigapi "github.com/openshift/api/config/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.OpenStackPlatformType)
}
