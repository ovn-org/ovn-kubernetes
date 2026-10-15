// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"cmp"
	"os"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/configs"
)

const (
	ovnKubernetesNamespaceEnvVar = "OVN_TEST_OVNK_NAMESPACE"
	frrK8sNamespaceEnvVar        = "OVN_TEST_FRRK8S_NAMESPACE"
	externalBridgeEnvVar         = "OVN_TEST_EXTERNAL_BRIDGE"
	primaryInterfaceEnvVar       = "OVN_TEST_PRIMARY_INTERFACE"
	nbdbContainerEnvVar          = "OVN_TEST_NBDB_CONTAINER"
	l3UDNMultiSubnetEnvVar       = "OVN_TEST_L3_UDN_MULTI_SUBNET"
)

type kube struct{}

func New() api.DeploymentConfig {
	return kube{}
}

func (kube) OVNKubernetesNamespace() string {
	return cmp.Or(os.Getenv(ovnKubernetesNamespaceEnvVar), "ovn-kubernetes")
}

func (kube) FRRK8sNamespace() string {
	return cmp.Or(os.Getenv(frrK8sNamespaceEnvVar), "frr-k8s-system")
}

func (kube) ExternalBridgeName() string {
	return required(externalBridgeEnvVar)
}

func (kube) PrimaryInterfaceName() string {
	return required(primaryInterfaceEnvVar)
}

func (kube) GetImage(imageID api.ImageID) api.ImageConfig {
	return configs.GetImage(imageID)
}

// Existing clusters pull test images on demand instead of preloading them.
func (kube) AddRequiredImage(...api.ImageID) {}

func (kube) GetRequiredImages() []api.ImageConfig {
	return nil
}

func (kube) IsConfigurationEnabled(config api.Config) bool {
	return config == api.L3UDNMultiSubnetConfig && os.Getenv(l3UDNMultiSubnetEnvVar) == "true"
}

func (kube) NBDBContainerName() string {
	return cmp.Or(os.Getenv(nbdbContainerEnvVar), "nb-ovsdb")
}

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		ginkgo.Skip("set "+name+" to run this spec", 2)
	}
	return value
}
