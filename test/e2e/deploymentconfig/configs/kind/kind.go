// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"os"

	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

var (
	agnHost = imageutils.GetE2EImage(imageutils.Agnhost)
	// We limit the set of images used by e2e to reduce duplication and to allow us to provide offline mirroring of images
	// for customers and restricted test environments.
	// Ideally, every image used in e2e must be part of this package.
	// New test images should ideally be sourced from the upstream k8s.io/kubernetes/test/utils/image package.
	// Failing to find an image from upstream k8s, please get community approval because downstream consumers must
	// pre-approve new images.
	// FIXME: iperf3 image should not be retrieved from a users repo and should not have latest tag
	iperf3                = "quay.io/sronanrh/iperf:latest"
	netshoot              = "ghcr.io/nicolaka/netshoot:v0.13"
	nginx                 = "nginx:1"
	metallbLBService      = "quay.io/itssurya/dev-images:metallb-lbservice"
	udpServerSrcIPPrinter = "quay.io/itssurya/dev-images:udp-server-srcip-printer"
	frr                   = "quay.io/frrouting/frr:10.5.3"
	// dnsmasq 2.83; pinned by digest for CI reproducibility.
	// TODO: mirror to a project-controlled registry (ghcr/quay) — docker.io
	// pulls are rate-limited in CI and this is a personal repository.
	dnsmasq                     = "docker.io/andyshinn/dnsmasq:2.83@sha256:e937327fede666e55ba4c2ab8e715a2ce561945363016d42f9d698d1b18ff1be"
	fedoraKubevirtContainerDisk = "quay.io/kubevirtci/fedora-with-test-tooling:v20250416-e37573e"

	imageConfigs map[api.ImageID]string
)

func init() {
	if agnHostOverride := os.Getenv("AGNHOST_IMAGE"); agnHostOverride != "" {
		agnHost = agnHostOverride
	}
	if iperf3Override := os.Getenv("IPERF3_IMAGE"); iperf3Override != "" {
		iperf3 = iperf3Override
	}
	if netshootOverride := os.Getenv("NETSHOOT_IMAGE"); netshootOverride != "" {
		netshoot = netshootOverride
	}
	if nginxOverride := os.Getenv("NGINX_IMAGE"); nginxOverride != "" {
		nginx = nginxOverride
	}
	if metallbLBServiceOverride := os.Getenv("METALLB_LB_SERVICE_IMAGE"); metallbLBServiceOverride != "" {
		metallbLBService = metallbLBServiceOverride
	}
	if udpServerOverride := os.Getenv("UDP_SERVER_SRCIP_PRINTER_IMAGE"); udpServerOverride != "" {
		udpServerSrcIPPrinter = udpServerOverride
	}
	if frrOverride := os.Getenv("FRR_IMAGE"); frrOverride != "" {
		frr = frrOverride
	}
	if fedoraKubevirtContainerDiskOverride := os.Getenv("FEDORA_KUBEVIRT_CONTAINER_DISK_IMAGE"); fedoraKubevirtContainerDiskOverride != "" {
		fedoraKubevirtContainerDisk = fedoraKubevirtContainerDiskOverride
	}

	imageConfigs = map[api.ImageID]string{
		api.Agnhost:                     agnHost,
		api.IPerf3:                      iperf3,
		api.Netshoot:                    netshoot,
		api.Nginx:                       nginx,
		api.MetalLBLBService:            metallbLBService,
		api.UDPServerSrcIPPrinter:       udpServerSrcIPPrinter,
		api.FRR:                         frr,
		api.DNSMasq:                     dnsmasq,
		api.FedoraKubevirtContainerDisk: fedoraKubevirtContainerDisk,
	}
}

type kind struct {
	requiredImages map[api.ImageID]struct{}
}

func New() api.DeploymentConfig {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	return &kind{
		requiredImages: make(map[api.ImageID]struct{}),
	}
}

func (k *kind) OVNKubernetesNamespace() string {
	return "ovn-kubernetes"
}

func (k *kind) FRRK8sNamespace() string {
	return "frr-k8s-system"
}

func (k *kind) ExternalBridgeName() string {
	return "breth0"
}

func (k *kind) PrimaryInterfaceName() string {
	return "eth0"
}

func (k *kind) IsConfigurationEnabled(config api.Config) bool {
	switch config {
	case api.L3UDNMultiSubnetConfig:
		// Currently enabled by default for Kind cluster. Could use
		// an ENV variable check instead if we need variability later.
		return true
	default:
		return false
	}
}

func (k *kind) NBDBContainerName() string {
	return "nb-ovsdb"
}

func (k *kind) GetImage(imageID api.ImageID) api.ImageConfig {
	return api.ImageConfig{
		ImageID:  imageID,
		PullSpec: imageConfigs[imageID],
	}
}

func (k *kind) AddRequiredImage(imageID ...api.ImageID) {
	for _, imgID := range imageID {
		k.requiredImages[imgID] = struct{}{}
	}
}

func (k *kind) GetRequiredImages() []api.ImageConfig {
	k.AddRequiredImage(api.Agnhost)
	imageConfigs := make([]api.ImageConfig, 0, len(k.requiredImages))
	for imageID := range k.requiredImages {
		imageConfigs = append(imageConfigs, k.GetImage(imageID))
	}
	return imageConfigs
}
