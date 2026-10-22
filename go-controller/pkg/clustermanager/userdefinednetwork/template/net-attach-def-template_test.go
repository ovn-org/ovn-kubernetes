// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"encoding/json"
	"strings"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetAttachDefTemplate", func() {

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = true
		// Enable EVPN for tests that use EVPN transport
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		// satisfy EVPN LGW restriction, otherwise no effect
		config.Gateway.Mode = config.GatewayModeLocal
	})

	DescribeTable("should fail to render NAD spec given",
		func(spec *udnv1.UserDefinedNetworkSpec, expectedError string) {
			_, err := renderNADSpec("foo", "bar", spec, "", nil)
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		},
		Entry("invalid layer2 subnets",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Subnets: udnv1.DualStackCIDRs{"abc"},
				},
			},
			config.NewCIDRNotProperlyFormattedError("abc").Error(),
		),
		Entry("invalid layer3 cluster-subnet",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{{CIDR: "!", HostSubnet: 16}},
				},
			},
			config.NewInvalidCIDRAddressError().Error(),
		),
		Entry("invalid layer3 host-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: -1},
					},
				},
			},
			config.NewHostSubnetMaskError(24, 24).Error(), // -1 is not a valid host subnet mask, it's converted to 24
		),
		Entry("layer3 host-subnet mask is smaller then cluster-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 16},
					},
				},
			},
			config.NewHostSubnetMaskError(16, 24).Error(),
		),
		Entry("layer3 host-subnet mask equal to cluster-subnet mask",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 24},
					},
				},
			},
			config.NewHostSubnetMaskError(24, 24).Error(),
		),
		Entry("invalid layer3 host-subnet; IPv4 mask is bigger then 32",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.10.0.0/24", HostSubnet: 33},
					},
				},
			},
			config.NewInvalidIPv4HostSubnetError().Error(),
		),
		Entry("invalid layer2 join subnets",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"abc"},
				},
			},
			config.NewCIDRNotProperlyFormattedError("abc").Error(),
		),
		Entry("invalid layer2 dual-stack join subnets, invalid IPv4 CIDR",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"fd50::0/125", "!"},
				},
			},
			config.NewCIDRNotProperlyFormattedError("!").Error(),
		),
		Entry("invalid layer2 dual-stack join subnets, invalid IPv6 CIDR",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"10.10.0.0/24", "!"},
				},
			},
			config.NewCIDRNotProperlyFormattedError("!").Error(),
		),
		// The validation for max number of subnets is moved to the CRD validation,
		// no need to test it here.
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv4",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.64.10.0/24"},
				},
			},
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("100.64.10.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("100.64.0.0/16")}).Error(),
		),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, IPv6",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"fd98::4/127"},
				},
			},
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("fd98::4/127")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("fd98::/64")}).Error(),
		),
		Entry("invalid join subnets, overlapping with cluster-default join-subnet, dual-stack",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"10.10.0.0/24"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.64.10.0/24", "fd98::4/127"},
				},
			},
			config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: util.MustParseCIDR("100.64.10.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: util.MustParseCIDR("100.64.0.0/16")}).Error(),
		),
	)

	DescribeTable("should fail to render NAD manifest, given",
		func(obj client.Object, expectedError string) {
			_, err := RenderNetAttachDefManifest(obj, "test")
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		},
		Entry("UDN, invalid topology: topology layer2 & layer3 config",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Layer3: &udnv1.Layer3Config{}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("UDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
		),
		Entry("UDN, invalid IPAM config: IPAM lifecycle & disabled ipam mode",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/16"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
						Mode:      udnv1.IPAMDisabled,
					},
				},
			}},
			config.NewIPAMLifecycleNotSupportedError().Error(),
		),
		Entry("UDN, invalid IPAM config: IPAM enabled & no subnet",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{},
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMEnabled,
					},
				},
			}},
			config.NewSubnetsRequiredError().Error(),
		),
		Entry("UDN, invalid IPAM config: IPAM disabled & subnet",
			&udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/16"},
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMDisabled,
					},
				},
			}},
			config.NewSubnetsMustBeUnsetError().Error(),
		),
		Entry("CUDN, invalid topology: topology layer2 & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Layer3: &udnv1.Layer3Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer2 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2, Localnet: &udnv1.LocalnetConfig{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer2)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer3 & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Layer2: &udnv1.Layer2Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
		),
		Entry("CUDN, invalid topology: topology layer3 & localnet config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3, Localnet: &udnv1.LocalnetConfig{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLayer3)).Error(),
		),
		Entry("CUDN, invalid topology: topology localnet & layer2 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer2: &udnv1.Layer2Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLocalnet)).Error(),
		),
		Entry("CUDN, invalid topology: topology localnet & layer3 config",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet, Layer3: &udnv1.Layer3Config{}}}},
			config.NewTopologyConfigMismatchError(string(udnv1.NetworkTopologyLocalnet)).Error(),
		),
		// The IPAMConfig CEL rule rejects this combination at admission; the
		// renderer re-validates so clusters with CRDs predating the rule are
		// covered too.
		Entry("CUDN, localnet: invalid IPAM config: persistent lifecycle & DHCP ipam mode",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
						Mode:      udnv1.IPAMDHCP,
					},
				},
			}}},
			config.NewIPAMLifecycleNotSupportedError().Error(),
		),
		// The localnet CEL rule forbids subnets with DHCP ipam.mode at
		// admission; render-time rejection covers clusters with CRDs
		// predating the rule.
		Entry("CUDN, localnet: invalid IPAM config: DHCP ipam mode & subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets: udnv1.DualStackCIDRs{"172.18.0.0/16"},
					IPAM:    &udnv1.IPAMConfig{Mode: udnv1.IPAMDHCP},
				},
			}}},
			config.NewSubnetsMustBeUnsetError().Error(),
		),
		Entry("CUDN, localnet: IPv4 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"192.200.0.0/30"},
				},
			}}},
			config.NewExcludedSubnetNotContainedError("192.200.0.0/30").Error(),
		),
		Entry("CUDN, localnet: IPv6 excludeSubnets not in range of subnets",
			&udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{Role: udnv1.NetworkRoleSecondary, PhysicalNetworkName: "localnet1",
					Subnets:        udnv1.DualStackCIDRs{"192.168.0.0/16", "2001:dbb::/64"},
					ExcludeSubnets: []udnv1.CIDR{"2001:aaa::/127"},
				},
			}}},
			config.NewExcludedSubnetNotContainedError("2001:aaa::/127").Error(),
		),
	)

	It("should return no error given no UDN", func() {
		_, err := RenderNetAttachDefManifest(nil, "")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should fail given no target namespace", func() {
		cudn := &udnv1.UserDefinedNetwork{Spec: udnv1.UserDefinedNetworkSpec{
			Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{}},
		}
		_, err := RenderNetAttachDefManifest(cudn, "")
		Expect(err).To(HaveOccurred())
	})

	It("should fail given unknown type", func() {
		_, err := RenderNetAttachDefManifest(&netv1.NetworkAttachmentDefinition{}, "foo")
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("should create UDN NAD from spec",
		func(testSpec udnv1.UserDefinedNetworkSpec, expectedNadNetConf string) {
			testUdn := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Namespace: "mynamespace", Name: "test-net", UID: "1",
					Annotations: map[string]string{"testAnnotation": "test", "k8s.ovn.org/testAnnotation": "test"},
					Labels:      map[string]string{"testLabel": "test"}},
				Spec: testSpec,
			}
			testNs := "mynamespace"
			ownerRef := metav1.OwnerReference{
				APIVersion:         "k8s.ovn.org/v1",
				Kind:               "UserDefinedNetwork",
				Name:               "test-net",
				UID:                "1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
			}
			expectedNAD := &netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-net",
					Annotations:     map[string]string{"testAnnotation": "test"},
					Labels:          map[string]string{"k8s.ovn.org/user-defined-network": "", "testLabel": "test"},
					OwnerReferences: []metav1.OwnerReference{ownerRef},
					Finalizers:      []string{"k8s.ovn.org/user-defined-network-protection"},
				},
				Spec: netv1.NetworkAttachmentDefinitionSpec{Config: expectedNadNetConf},
			}

			// must be defined so the primary user defined network can match the ip families of the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnablePersistentIPs = true
			nad, err := RenderNetAttachDefManifest(testUdn, testNs)
			Expect(err).NotTo(HaveOccurred())
			Expect(nad.TypeMeta).To(Equal(expectedNAD.TypeMeta))
			Expect(nad.ObjectMeta).To(Equal(expectedNAD.ObjectMeta))
			Expect(nad.Spec.Config).To(MatchJSON(expectedNAD.Spec.Config))
		},
		Entry("primary network, layer3",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
						{CIDR: "2001:dbb::/60"},
					},
					MTU: 1500,
				},
			},
			`{
				"cniVersion": "1.1.0",
				"type": "ovn-k8s-cni-overlay",
				"name": "mynamespace_test-net",
				"netAttachDefName": "mynamespace/test-net",
				"role": "primary",
				"topology": "layer3",
				"joinSubnet": "100.65.0.0/16,fd99::/64",
				"subnets": "192.168.100.0/16,2001:dbb::/60",
				"mtu": 1500
			}`,
		),
		Entry("primary network, layer2",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
        	}`,
		),
		Entry("primary network, should override join-subnets when specified",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.62.0.0/24", "fd92::/64"},
					MTU:         1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.62.0.0/24,fd92::/64",
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, no join-subnets should be set",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, layer2, macSecurity mode Disabled",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRoleSecondary,
					IPAM:        &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "macSecurityMode": "disabled"
			}`,
		),
		Entry("secondary network, layer2, macSecurity mode Enabled",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRoleSecondary,
					IPAM:        &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityEnabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "macSecurityMode": "enabled"
			}`,
		),
		Entry("secondary network, layer2, macSecurity unset should not render macSecurityMode for backward compatibility",
			udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
					IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "mynamespace_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2"
			}`,
		),
	)

	DescribeTable("should create CUDN NAD from spec",
		func(testSpec udnv1.NetworkSpec, expectedNadNetConf string) {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-net", UID: "1"},
				Spec:       udnv1.ClusterUserDefinedNetworkSpec{Network: testSpec},
			}
			testNs := "mynamespace"

			expectedOwnerRef := metav1.OwnerReference{
				APIVersion:         "k8s.ovn.org/v1",
				Kind:               "ClusterUserDefinedNetwork",
				Name:               "test-net",
				UID:                "1",
				BlockOwnerDeletion: ptr.To(true),
				Controller:         ptr.To(true),
			}
			expectedNAD := &netv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-net",
					OwnerReferences: []metav1.OwnerReference{expectedOwnerRef},
					Labels:          map[string]string{"k8s.ovn.org/user-defined-network": ""},
					Finalizers:      []string{"k8s.ovn.org/user-defined-network-protection"},
				},
				Spec: netv1.NetworkAttachmentDefinitionSpec{Config: expectedNadNetConf},
			}
			// must be defined so the primary user defined network can match the ip families of the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnablePersistentIPs = true
			nad, err := RenderNetAttachDefManifest(cudn, testNs)
			Expect(err).NotTo(HaveOccurred())
			Expect(nad.TypeMeta).To(Equal(expectedNAD.TypeMeta))
			Expect(nad.ObjectMeta).To(Equal(expectedNAD.ObjectMeta))
			Expect(nad.Spec.Config).To(MatchJSON(expectedNAD.Spec.Config))
		},
		Entry("primary network, layer3",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
						{CIDR: "2001:dbb::/60"},
					},
					MTU: 1500,
				},
			},
			`{
				"cniVersion": "1.1.0",
				"type": "ovn-k8s-cni-overlay",
				"name": "cluster_udn_test-net",
				"netAttachDefName": "mynamespace/test-net",
				"role": "primary",
				"topology": "layer3",
				"joinSubnet": "100.65.0.0/16,fd99::/64",
				"subnets": "192.168.100.0/16,2001:dbb::/60",
					"mtu": 1500
				}`,
		),
		Entry("primary network, layer3 with no-overlay unmanaged routing",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
					},
					MTU: 1500,
				},
				Transport: udnv1.TransportOptionNoOverlay,
				NoOverlay: &udnv1.NoOverlayConfig{
					OutboundSNAT: udnv1.SNATEnabled,
					Routing:      udnv1.RoutingUnmanaged,
				},
			},
			`{
					"cniVersion": "1.1.0",
					"type": "ovn-k8s-cni-overlay",
					"name": "cluster_udn_test-net",
					"netAttachDefName": "mynamespace/test-net",
					"role": "primary",
					"topology": "layer3",
					"joinSubnet": "100.65.0.0/16,fd99::/64",
					"subnets": "192.168.100.0/16",
					"mtu": 1500,
					"transport": "no-overlay",
					"outboundSNAT": "enabled",
					"noOverlayRouting": "unmanaged"
				}`,
		),
		Entry("primary network, layer2",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
        	}`,
		),
		Entry("primary network, should override join-subnets when specified",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRolePrimary,
					Subnets:     udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					JoinSubnets: udnv1.DualStackCIDRs{"100.62.0.0/24", "fd92::/64"},
					MTU:         1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.62.0.0/24,fd92::/64",
			  "transitSubnet": "100.88.0.0/16,fd97::/64",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, layer2",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					MTU:     1500,
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1500,
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, localnet",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					MTU:                 1600,
					VLAN:                &udnv1.VLANConfig{Mode: udnv1.VLANModeAccess, Access: &udnv1.AccessVLANConfig{ID: 200}},
					Subnets:             udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					ExcludeSubnets:      []udnv1.CIDR{"192.168.100.1/32", "2001:dbb::0/128"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
              "excludeSubnets": "192.168.100.1/32,2001:dbb::0/128",
			  "mtu": 1600,
              "vlanID": 200, 
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, layer2, macSecurity mode Disabled",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRoleSecondary,
					IPAM:        &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
			  "macSecurityMode": "disabled"
			}`,
		),
		Entry("secondary network, layer2, macSecurity mode Enabled",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:        udnv1.NetworkRoleSecondary,
					IPAM:        &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity: &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityEnabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2",
              "macSecurityMode": "enabled"		  
			}`,
		),
		Entry("secondary network, layer2, macSecurity unset should not render macSecurityMode for backward compatibility",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
					IPAM: &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "layer2"
			}`,
		),
		Entry("secondary network, localnet, macSecurity mode Disabled",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					IPAM:                &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity:         &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
			  "physicalNetworkName": "mylocalnet1",
			  "mtu": 1500,
			  "macSecurityMode": "disabled"
			}`,
		),
		Entry("secondary network, localnet, macSecurity mode Enabled",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					IPAM:                &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
					MACSecurity:         &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityEnabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
			  "physicalNetworkName": "mylocalnet1",
			  "mtu": 1500,
			  "macSecurityMode": "enabled"
			}`,
		),
		Entry("secondary network, localnet, macSecurity unset should not render macSecurityMode for backward compatibility",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					IPAM:                &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
			  "physicalNetworkName": "mylocalnet1",
			  "mtu": 1500
			}`,
		),
		Entry("secondary network, localnet, when MTU is unset it should set default MTU",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					VLAN:                &udnv1.VLANConfig{Mode: udnv1.VLANModeAccess, Access: &udnv1.AccessVLANConfig{ID: 200}},
					Subnets:             udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					ExcludeSubnets:      []udnv1.CIDR{"192.168.100.1/32", "2001:dbb::0/128"},
					IPAM: &udnv1.IPAMConfig{
						Lifecycle: udnv1.IPAMLifecyclePersistent,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
              "excludeSubnets": "192.168.100.1/32,2001:dbb::0/128",
			  "mtu": 1500,
              "vlanID": 200, 
			  "allowPersistentIPs": true
			}`,
		),
		Entry("secondary network, localnet, with DHCP IPAM mode should set dhcp ipam",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					MTU:                 1600,
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMDHCP,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "mtu": 1600,
			  "ipam": {"type": "dhcp"}
			}`,
		),
		Entry("secondary network, localnet, with Enabled IPAM mode should not set ipam key",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLocalnet,
				Localnet: &udnv1.LocalnetConfig{
					Role:                udnv1.NetworkRoleSecondary,
					PhysicalNetworkName: "mylocalnet1",
					MTU:                 1600,
					Subnets:             udnv1.DualStackCIDRs{"192.168.100.0/24", "2001:dbb::/64"},
					IPAM: &udnv1.IPAMConfig{
						Mode: udnv1.IPAMEnabled,
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "secondary",
			  "topology": "localnet",
		      "physicalNetworkName": "mylocalnet1",
			  "subnets": "192.168.100.0/24,2001:dbb::/64",
			  "mtu": 1600
			}`,
		),
		Entry("primary network, layer2 with EVPN transport and MAC-VRF",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24"},
					MTU:     1500,
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "my-vtep",
					MACVRF: &udnv1.VRFConfig{
						VNI:         100,
						RouteTarget: "65000:100",
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "transitSubnet": "100.88.0.0/16",
			  "subnets": "192.168.100.0/24",
			  "mtu": 1500,
			  "transport": "evpn",
			  "evpn": {
			    "vtep": "my-vtep",
			    "macVRF": {
			      "vni": 100,
			      "routeTarget": "65000:100"
			    }
			  }
			}`,
		),
		Entry("primary network, layer3 with EVPN transport and IP-VRF",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "192.168.100.0/16"},
					},
					MTU: 1500,
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "my-vtep",
					IPVRF: &udnv1.VRFConfig{
						VNI:         200,
						RouteTarget: "65000:200",
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer3",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "subnets": "192.168.100.0/16",
			  "mtu": 1500,
			  "transport": "evpn",
			  "evpn": {
			    "vtep": "my-vtep",
			    "ipVRF": {
			      "vni": 200,
			      "routeTarget": "65000:200"
			    }
			  }
			}`,
		),
		Entry("primary network, layer2 with EVPN transport, MAC-VRF and IP-VRF",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24"},
					MTU:     1500,
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "my-vtep",
					MACVRF: &udnv1.VRFConfig{
						VNI:         100,
						RouteTarget: "100000:100", // 4-byte ASN format
					},
					IPVRF: &udnv1.VRFConfig{
						VNI:         200,
						RouteTarget: "192.168.1.1:200", // IPv4 format
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "transitSubnet": "100.88.0.0/16",
			  "subnets": "192.168.100.0/24",
			  "mtu": 1500,
			  "transport": "evpn",
			  "evpn": {
			    "vtep": "my-vtep",
			    "macVRF": {
			      "vni": 100,
			      "routeTarget": "100000:100"
			    },
			    "ipVRF": {
			      "vni": 200,
			      "routeTarget": "192.168.1.1:200"
			    }
			  }
			}`,
		),
		Entry("primary network, layer2 with EVPN transport, MAC-VRF with VNI only (no RouteTarget)",
			udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24"},
					MTU:     1500,
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN: &udnv1.EVPNConfig{
					VTEP: "my-vtep",
					MACVRF: &udnv1.VRFConfig{
						VNI: 100,
						// RouteTarget intentionally omitted
					},
				},
			},
			`{
			  "cniVersion": "1.1.0",
			  "type": "ovn-k8s-cni-overlay",
			  "name": "cluster_udn_test-net",
			  "netAttachDefName": "mynamespace/test-net",
			  "role": "primary",
			  "topology": "layer2",
			  "joinSubnet": "100.65.0.0/16,fd99::/64",
			  "transitSubnet": "100.88.0.0/16",
			  "subnets": "192.168.100.0/24",
			  "mtu": 1500,
			  "transport": "evpn",
			  "evpn": {
			    "vtep": "my-vtep",
			    "macVRF": {
			      "vni": 100
			    }
			  }
			}`,
		),
	)

	It("should render CUDN uplink into the NAD CNI config", func() {
		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "test-net", UID: "1"},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Uplinks: []string{"br-test"},
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: udnv1.NetworkRolePrimary,
						Subnets: []udnv1.Layer3Subnet{
							{CIDR: "192.168.100.0/16"},
						},
					},
				},
			},
		}

		config.IPv4Mode = true
		config.IPv6Mode = false
		config.Gateway.Mode = config.GatewayModeShared
		nad, err := RenderNetAttachDefManifest(cudn, "mynamespace")
		Expect(err).NotTo(HaveOccurred())

		netConf := map[string]interface{}{}
		err = json.Unmarshal([]byte(nad.Spec.Config), &netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf).To(HaveKeyWithValue("uplink", "br-test"))
	})

	Context("EVPN VID injection", func() {
		It("should inject VIDs into EVPN config when provided via WithEVPNVIDs", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-evpn", UID: "1"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    udnv1.NetworkRoleSecondary,
							Subnets: udnv1.DualStackCIDRs{"192.168.0.0/16"},
						},
						Transport: udnv1.TransportOptionEVPN,
						EVPN: &udnv1.EVPNConfig{
							VTEP: "my-vtep",
							MACVRF: &udnv1.VRFConfig{
								VNI:         100,
								RouteTarget: "65000:100",
							},
							IPVRF: &udnv1.VRFConfig{
								VNI:         200,
								RouteTarget: "65000:200",
							},
						},
					},
				},
			}

			nad, err := RenderNetAttachDefManifest(cudn, "test-ns", WithEVPNVIDs(12, 13))
			Expect(err).NotTo(HaveOccurred())
			Expect(nad).NotTo(BeNil())

			var netConf ovncnitypes.NetConf
			err = json.Unmarshal([]byte(nad.Spec.Config), &netConf)
			Expect(err).NotTo(HaveOccurred())

			Expect(netConf.EVPN).NotTo(BeNil(), "evpnConfig should be present")
			Expect(netConf.EVPN.MACVRF).NotTo(BeNil(), "macVRF should be present")
			Expect(netConf.EVPN.MACVRF.VID).To(Equal(12), "macVRF VID should be 12")
			Expect(netConf.EVPN.IPVRF).NotTo(BeNil(), "ipVRF should be present")
			Expect(netConf.EVPN.IPVRF.VID).To(Equal(13), "ipVRF VID should be 13")
		})

		It("should omit VID when zero (VID=0 not injected)", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-evpn-no-vid", UID: "1"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    udnv1.NetworkRoleSecondary,
							Subnets: udnv1.DualStackCIDRs{"192.168.0.0/16"},
						},
						Transport: udnv1.TransportOptionEVPN,
						EVPN: &udnv1.EVPNConfig{
							VTEP: "my-vtep",
							MACVRF: &udnv1.VRFConfig{
								VNI:         100,
								RouteTarget: "65000:100",
							},
						},
					},
				},
			}

			// Pass VID=0 for both (should be omitted from JSON, unmarshals as zero value)
			nad, err := RenderNetAttachDefManifest(cudn, "test-ns", WithEVPNVIDs(0, 0))
			Expect(err).NotTo(HaveOccurred())
			Expect(nad).NotTo(BeNil())

			var netConf ovncnitypes.NetConf
			err = json.Unmarshal([]byte(nad.Spec.Config), &netConf)
			Expect(err).NotTo(HaveOccurred())

			Expect(netConf.EVPN).NotTo(BeNil(), "evpnConfig should be present")
			Expect(netConf.EVPN.MACVRF).NotTo(BeNil(), "macVRF should be present")
			Expect(netConf.EVPN.MACVRF.VID).To(Equal(0), "VID should be zero when not injected")

			// Also verify the raw JSON doesn't contain "vid" field (omitempty)
			Expect(nad.Spec.Config).NotTo(ContainSubstring(`"vid"`), "vid field should be omitted from JSON when zero")
		})

		It("should omit empty RouteTarget in EVPN config", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-evpn-no-rt", UID: "1"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    udnv1.NetworkRoleSecondary,
							Subnets: udnv1.DualStackCIDRs{"192.168.0.0/16"},
						},
						Transport: udnv1.TransportOptionEVPN,
						EVPN: &udnv1.EVPNConfig{
							VTEP: "my-vtep",
							MACVRF: &udnv1.VRFConfig{
								VNI: 100,
								// RouteTarget intentionally omitted (empty)
							},
						},
					},
				},
			}

			nad, err := RenderNetAttachDefManifest(cudn, "test-ns", WithEVPNVIDs(5, 0))
			Expect(err).NotTo(HaveOccurred())
			Expect(nad).NotTo(BeNil())

			var netConf ovncnitypes.NetConf
			err = json.Unmarshal([]byte(nad.Spec.Config), &netConf)
			Expect(err).NotTo(HaveOccurred())

			// RouteTarget should be empty (omitted in JSON, unmarshals as empty string)
			Expect(netConf.EVPN.MACVRF.RouteTarget).To(BeEmpty(), "empty routeTarget should unmarshal as empty string")

			// Also verify the raw JSON doesn't contain "routeTarget" field
			Expect(nad.Spec.Config).NotTo(ContainSubstring(`"routeTarget"`), "routeTarget should be omitted from JSON when empty")

			// VID should be present
			Expect(netConf.EVPN.MACVRF.VID).To(Equal(5), "macVRF VID should be 5")
		})

		It("should handle nil RenderOption without panic", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nil-option", UID: "1"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    udnv1.NetworkRoleSecondary,
							Subnets: udnv1.DualStackCIDRs{"192.168.0.0/16"},
						},
					},
				},
			}

			// Pass nil option - should not panic
			var nilOpt RenderOption
			Expect(func() {
				_, _ = RenderNetAttachDefManifest(cudn, "test-ns", nilOpt, WithEVPNVIDs(1, 2))
			}).NotTo(Panic())
		})

		It("should fail when EVPN transport is requested but EVPN feature is disabled", func() {
			// Disable EVPN feature flag for this test.
			// No defer needed - BeforeEach resets config via PrepareTestConfig().
			config.OVNKubernetesFeature.EnableEVPN = false

			cudn := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "test-evpn-disabled", UID: "1"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    udnv1.NetworkRolePrimary,
							Subnets: udnv1.DualStackCIDRs{"192.168.100.0/24"},
						},
						Transport: udnv1.TransportOptionEVPN,
						EVPN: &udnv1.EVPNConfig{
							VTEP: "my-vtep",
							MACVRF: &udnv1.VRFConfig{
								VNI:         100,
								RouteTarget: "65000:100",
							},
						},
					},
				},
			}

			_, err := RenderNetAttachDefManifest(cudn, "test-ns")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("EVPN transport requested but EVPN feature is not enabled"))
		})
	})

	It("should correctly assign transit Subnets", func() {
		// check no overlap, use default values
		netConf := &ovncnitypes.NetConf{
			Role:     strings.ToLower(types.NetworkRolePrimary),
			Topology: strings.ToLower(types.Layer2Topology),
			Subnets:  "10.12.0.0/16,fd12:dbba::/64",
		}
		err := util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.88.0.0/16,fd97::/64"))
		// check Subnet with the default Transit subnet overlap
		netConf = &ovncnitypes.NetConf{
			Role:     strings.ToLower(types.NetworkRolePrimary),
			Topology: strings.ToLower(types.Layer2Topology),
			Subnets:  "100.88.0.0/15,fd97::/63",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.90.0.0/16,fd97:0:0:2::/64"))
		// check joinSubnet with the default Transit subnet overlap
		netConf = &ovncnitypes.NetConf{
			Role:       strings.ToLower(types.NetworkRolePrimary),
			Topology:   strings.ToLower(types.Layer2Topology),
			Subnets:    "10.12.0.0/16,fd12:dbba::/64",
			JoinSubnet: "100.88.0.0/17,fd97::/65",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.89.0.0/16,fd97:0:0:1::/64"))
		// check Subnet with the default Transit subnet overlap, then joinSubnet overlaps with the next selected transit subnet
		netConf = &ovncnitypes.NetConf{
			Role:       strings.ToLower(types.NetworkRolePrimary),
			Topology:   strings.ToLower(types.Layer2Topology),
			Subnets:    "100.88.0.0/15,fd97::/65",
			JoinSubnet: "100.90.0.0/16,fd97:0:0:1::/64",
		}
		err = util.SetTransitSubnets(netConf)
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.TransitSubnet).To(Equal("100.91.0.0/16,fd97:0:0:2::/64"))
	})
})
