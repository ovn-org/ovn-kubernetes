// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package configs

import (
	"os"

	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
)

// Kind preloads these images, while existing clusters pull the same specs directly.
var imageConfigs = map[api.ImageID]string{
	api.Agnhost:                     imageutils.GetE2EImage(imageutils.Agnhost),
	api.IPerf3:                      "quay.io/sronanrh/iperf:latest",
	api.Netshoot:                    "ghcr.io/nicolaka/netshoot:v0.13",
	api.Nginx:                       "nginx:1",
	api.MetalLBLBService:            "quay.io/itssurya/dev-images:metallb-lbservice",
	api.UDPServerSrcIPPrinter:       "quay.io/itssurya/dev-images:udp-server-srcip-printer",
	api.FRR:                         "quay.io/frrouting/frr:10.5.3",
	api.DNSMasq:                     "docker.io/andyshinn/dnsmasq:2.83@sha256:e937327fede666e55ba4c2ab8e715a2ce561945363016d42f9d698d1b18ff1be",
	api.FedoraKubevirtContainerDisk: "quay.io/kubevirtci/fedora-with-test-tooling:v20250416-e37573e",
}

func GetImage(imageID api.ImageID) api.ImageConfig {
	image := imageConfigs[imageID]
	if override := os.Getenv(imageEnvVar(imageID)); override != "" {
		image = override
	}
	return api.ImageConfig{ImageID: imageID, PullSpec: image}
}

func imageEnvVar(imageID api.ImageID) string {
	switch imageID {
	case api.Agnhost:
		return "AGNHOST_IMAGE"
	case api.IPerf3:
		return "IPERF3_IMAGE"
	case api.Netshoot:
		return "NETSHOOT_IMAGE"
	case api.Nginx:
		return "NGINX_IMAGE"
	case api.MetalLBLBService:
		return "METALLB_LB_SERVICE_IMAGE"
	case api.UDPServerSrcIPPrinter:
		return "UDP_SERVER_SRCIP_PRINTER_IMAGE"
	case api.FRR:
		return "FRR_IMAGE"
	case api.FedoraKubevirtContainerDisk:
		return "FEDORA_KUBEVIRT_CONTAINER_DISK_IMAGE"
	default:
		return ""
	}
}
