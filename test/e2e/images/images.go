// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

func AgnHost() string {
	return deploymentconfig.Get().GetImage(api.Agnhost).PullSpec
}

func IPerf3() string {
	return deploymentconfig.Get().GetImage(api.IPerf3).PullSpec
}

// DNSMasq returns an image containing the dnsmasq DHCP server, used as the
// external DHCP server on the underlay for DHCP-IPAM localnet tests.
func DNSMasq() string {
	return deploymentconfig.Get().GetImage(api.DNSMasq).PullSpec
}

func Netshoot() string {
	return deploymentconfig.Get().GetImage(api.Netshoot).PullSpec
}

func Nginx() string {
	return deploymentconfig.Get().GetImage(api.Nginx).PullSpec
}

func MetalLBLBService() string {
	return deploymentconfig.Get().GetImage(api.MetalLBLBService).PullSpec
}

func UDPServerSrcIPPrinter() string {
	return deploymentconfig.Get().GetImage(api.UDPServerSrcIPPrinter).PullSpec
}

func FRR() string {
	return deploymentconfig.Get().GetImage(api.FRR).PullSpec
}

func FedoraKubevirtContainerDisk() string {
	return deploymentconfig.Get().GetImage(api.FedoraKubevirtContainerDisk).PullSpec
}
