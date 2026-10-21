// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package feature

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/label"
)

var (
	Service               = New("Service")
	NetworkPolicy         = New("NetworkPolicy")
	AdminNetworkPolicy    = New("AdminNetworkPolicy")
	BaselineNetworkPolicy = New("BaselineNetworkPolicy")
	NetworkSegmentation   = New("NetworkSegmentation")
	EgressIP              = New("EgressIP")
	EgressService         = New("EgressService")
	EgressFirewall        = New("EgressFirewall")
	EgressQos             = New("EgressQos")
	// DHCPIPAM covers localnet networks with ipam.mode=DHCP; these tests need
	// the dhcp CNI plugin binary on the nodes (kind.sh --install-cni-plugins).
	DHCPIPAM              = New("DHCPIPAM")
	EVPN                  = New("EVPN")
	ExternalGateway       = New("ExternalGateway")
	DisablePacketMTUCheck = New("DisablePacketMTUCheck")
	// DPU covers specs that need the dpu-simulator two-cluster layout together
	// with the Uplink e2e environment (OVN_TEST_DPU_UPLINK_*), and that disrupt
	// a host node (e.g. delete and re-register it). They run in the dpu-sim
	// no-overlay lane after the Uplink specs, via WHAT="DPU".
	DPU                   = New("DPU")
	VirtualMachineSupport = New("VirtualMachineSupport")
	Interconnect          = New("Interconnect")
	Multicast             = New("Multicast")
	MultiHoming           = New("MultiHoming")
	NodeIPMACMigration    = New("NodeIPMACMigration")
	NoOverlay             = New("NoOverlay")
	OVSCPUPin             = New("OVSCPUPin")
	RouteAdvertisements   = New("RouteAdvertisements")
	// RouteAdvertisementsDynamicUDN covers RouteAdvertisements behavior that
	// is specific to dynamic UDN allocation; these tests require clusters
	// with both route advertisements and dynamic UDN allocation enabled.
	RouteAdvertisementsDynamicUDN = New("RouteAdvertisementsDynamicUDN")
	Uplink                        = New("Uplink")
	Unidle                        = New("Unidle")
	NetworkQos                    = New("NetworkQos")
	NetworkConnect                = New("NetworkConnect")
	Observability                 = New("Observability")
	Metrics                       = New("Metrics")
	MACSecurity                   = New("MACSecurity")
)

func New(name string) ginkgo.Labels {
	return label.New("Feature", name).GinkgoLabel()
}
