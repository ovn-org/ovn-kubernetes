// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestParseNetworkSubnets(t *testing.T) {
	tests := []struct {
		desc            string
		topology        string
		subnets         string
		expectedSubnets []config.CIDRNetworkEntry
		expectError     bool
	}{
		{
			desc:     "multiple subnets layer 3 topology",
			topology: ovntypes.Layer3Topology,
			subnets:  "192.168.1.1/26/28, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("192.168.1.0/26"),
					HostSubnetLength: 28,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fda6::/48"),
					HostSubnetLength: 64,
				},
			},
		},
		{
			desc:     "multiple ipv4 subnets layer3 topology",
			topology: ovntypes.Layer3Topology,
			subnets:  "192.168.1.0/24/28, 192.168.2.0/24/28",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("192.168.1.0/24"),
					HostSubnetLength: 28,
				},
				{
					CIDR:             ovntest.MustParseIPNet("192.168.2.0/24"),
					HostSubnetLength: 28,
				},
			},
		},
		{
			desc:     "multiple ipv6 subnets layer3 topology",
			topology: ovntypes.Layer3Topology,
			subnets:  "fda6::/48, fda7::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("fda6::/48"),
					HostSubnetLength: 64,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fda7::/48"),
					HostSubnetLength: 64,
				},
			},
		},
		{
			desc:     "empty subnets layer 3 topology",
			topology: ovntypes.Layer3Topology,
		},
		{
			desc:     "multiple subnets layer 2 topology",
			topology: ovntypes.Layer2Topology,
			subnets:  "192.168.1.1/26, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
		},
		{
			desc:     "empty subnets layer 2 topology",
			topology: ovntypes.Layer2Topology,
			subnets:  "",
		},
		{
			desc:     "multiple subnets localnet topology",
			topology: ovntypes.LocalnetTopology,
			subnets:  "192.168.1.1/26, fda6::/48",
			expectedSubnets: []config.CIDRNetworkEntry{
				{
					CIDR: ovntest.MustParseIPNet("192.168.1.0/26"),
				},
				{
					CIDR: ovntest.MustParseIPNet("fda6::/48"),
				},
			},
		},
		{
			desc:     "empty subnets localnet topology",
			topology: ovntypes.LocalnetTopology,
			subnets:  "",
		},
		{
			desc:        "unsupported topology",
			topology:    "unsupported",
			subnets:     "192.168.1.1/26",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			subnets, err := parseNetworkSubnets(tc.subnets, tc.topology)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
		})
	}
}

func TestParseSubnetList(t *testing.T) {
	tests := []struct {
		desc            string
		subnets         string
		expectedSubnets []*net.IPNet
		expectError     bool
	}{
		{
			desc:            "multiple subnets",
			subnets:         "192.168.1.38/32, fda6::38/128",
			expectedSubnets: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
		},
		{
			desc: "empty subnets",
		},
		{
			desc:        "invalid formatted subnets",
			subnets:     "192.168.1.1/26/32",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			subnets, err := parseSubnetList(tc.subnets)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(subnets).To(gomega.ConsistOf(tc.expectedSubnets))
		})
	}
}

func TestValidateSubnetContainment(t *testing.T) {
	tests := []struct {
		desc             string
		subnets          []*net.IPNet
		containerSubnets []config.CIDRNetworkEntry
		expectError      bool
	}{
		{
			desc:    "valid containment",
			subnets: ovntest.MustParseIPNets("192.168.1.38/32", "fda6::38/128"),
			containerSubnets: []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("192.168.1.0/26")},
				{CIDR: ovntest.MustParseIPNet("fda6::/48")},
			},
		},
		{
			desc:    "invalid containment",
			subnets: ovntest.MustParseIPNets("fda7::38/128"),
			containerSubnets: []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("fda6::/48")},
			},
			expectError: true,
		},
		{
			desc:             "empty subnets",
			containerSubnets: []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("192.168.1.0/26")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := validateSubnetContainment(tc.subnets, tc.containerSubnets, config.NewExcludedSubnetNotContainedError)
			if tc.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err).To(gomega.BeAssignableToTypeOf(&config.ValidationError{}))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestParseNetconf(t *testing.T) {
	type testConfig struct {
		desc                        string
		inputNetAttachDefConfigSpec string
		expectedNetConf             *ovncnitypes.NetConf
		expectedError               error
		unsupportedReason           string
	}

	tests := []testConfig{
		{
			desc:          "empty network attachment configuration",
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: unexpected end of JSON input"),
		},
		{
			desc: "net-attach-def-name attribute does not match the metadata",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("net-attach-def name (ns1/nad1) is inconsistent with config (default/tenantred)"),
		},
		{
			desc: "attachment definition with no `name` attribute",
			inputNetAttachDefConfigSpec: `
    {
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: invalid name in in secondary network netconf ()"),
		},
		{
			desc: "attachment definition for another plugin",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "some other thing",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred"
    }
`,
			expectedError: fmt.Errorf("net-attach-def not managed by OVN"),
		},
		{
			desc: "attachment definition with IPAM key defined, using a wrong type",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "default/tenantred",
            "ipam": "this is wrong"
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: json: cannot unmarshal string into Go struct field NetConf.NetConf.ipam of type types.IPAM"),
		},
		{
			desc: "attachment definition with IPAM key defined",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "ns1/nad1",
            "ipam": {"type": "ninjaturtle"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: IPAM key is not supported. Use OVN-K provided IPAM via the `subnets` attribute"),
		},
		{
			desc: "valid attachment definition for a localnet topology with DHCP IPAM and no subnets",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "netAttachDefName": "ns1/nad1",
            "ipam": {"type": "dhcp"}
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "localnet",
				NADName:  "ns1/nad1",
				MTU:      1400,
				NetConf:  cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay", IPAM: cnitypes.IPAM{Type: "dhcp"}},
			},
		},
		{
			desc: "attachment definition for a layer2 topology with DHCP IPAM",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "netAttachDefName": "ns1/nad1",
            "ipam": {"type": "dhcp"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: ipam.type \"dhcp\" is only supported with localnet topology"),
		},
		{
			desc: "attachment definition for a localnet topology with DHCP IPAM and persistent IPs",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "netAttachDefName": "ns1/nad1",
            "allowPersistentIPs": true,
            "ipam": {"type": "dhcp"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: allowPersistentIPs requires OVN-Kubernetes-managed IPAM (the subnets attribute must be set)"),
		},
		{
			desc: "attachment definition with persistent IPs but no subnets (IPAM-less)",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "netAttachDefName": "ns1/nad1",
            "allowPersistentIPs": true
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: allowPersistentIPs requires OVN-Kubernetes-managed IPAM (the subnets attribute must be set)"),
		},
		{
			desc: "attachment definition for a localnet topology with DHCP IPAM and subnets",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "netAttachDefName": "ns1/nad1",
            "subnets": "172.18.0.0/16",
            "ipam": {"type": "dhcp"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: ipam.type \"dhcp\" cannot be used together with the subnets attribute; addresses are assigned by the external DHCP server"),
		},
		{
			desc: "attachment definition for a layer3 topology with DHCP IPAM",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
            "netAttachDefName": "ns1/nad1",
            "ipam": {"type": "dhcp"}
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: ipam.type \"dhcp\" is only supported with localnet topology"),
		},
		{
			desc: "attachment definition missing the NAD name attribute",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10
    }
`,
			expectedError: fmt.Errorf("error parsing Network Attachment Definition ns1/nad1: missing NADName in secondary network netconf tenantred"),
		},
		{
			desc: "valid attachment definition for a localnet topology with a VLAN",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "vlanID": 10,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "localnet",
				NADName:  "ns1/nad1",
				MTU:      1400,
				VLANID:   10,
				NetConf:  cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for the default network",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				NADName: "ns1/nad1",
				MTU:     1400,
				NetConf: cnitypes.NetConf{Name: "default", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for a localnet topology with a VLAN using a plugins list",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "cniVersion": "1.1.0",
            "plugins": [
              {
                "type": "ovn-k8s-cni-overlay",
                "topology": "localnet",
                "vlanID": 10,
                "netAttachDefName": "ns1/nad1"
              }
            ]
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "localnet",
				NADName:  "ns1/nad1",
				MTU:      1400,
				VLANID:   10,
				NetConf:  cnitypes.NetConf{Name: "tenantred", CNIVersion: "1.1.0", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "valid attachment definition for a localnet topology with persistent IPs and a subnet",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:           "localnet",
				NADName:            "ns1/nad1",
				MTU:                1400,
				NetConf:            cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
				AllowPersistentIPs: true,
				Subnets:            "192.168.200.0/16",
			},
		},
		{
			desc: "valid attachment definition for a layer2 topology with persistent IPs and a subnet",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:           "layer2",
				NADName:            "ns1/nad1",
				MTU:                1400,
				AllowPersistentIPs: true,
				Subnets:            "192.168.200.0/16",
				NetConf:            cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "invalid attachment definition for a layer3 topology with persistent IPs",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"allowPersistentIPs": true,
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("layer3 topology does not allow persistent IPs"),
		},
		{
			desc: "valid attachment definition for a layer2 topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
			"subnets": "192.168.200.0/16",
			"role": "primary",
            "netAttachDefName": "ns1/nad1",
			"joinSubnet": "100.66.0.0/16,fd99::/64"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:      "layer2",
				NADName:       "ns1/nad1",
				MTU:           1400,
				Role:          "primary",
				Subnets:       "192.168.200.0/16",
				TransitSubnet: config.ClusterManager.V4TransitSubnet,
				NetConf:       cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
				JoinSubnet:    "100.66.0.0/16,fd99::/64",
			},
		},
		{
			desc: "valid attachment definition for a layer3 topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "primary",
			"netAttachDefName": "ns1/nad1",
			"joinSubnet": "100.66.0.0/16"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:   "layer3",
				NADName:    "ns1/nad1",
				MTU:        1400,
				Role:       "primary",
				Subnets:    "192.168.200.0/16",
				NetConf:    cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
				JoinSubnet: "100.66.0.0/16",
			},
		},
		{
			desc: "valid attachment definition for a layer3 topology with role:secondary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "secondary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology: "layer3",
				NADName:  "ns1/nad1",
				MTU:      1400,
				Role:     "secondary",
				Subnets:  "192.168.200.0/16",
				NetConf:  cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "invalid attachment definition for a layer3 topology with role:Primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenant-red",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
			"subnets": "192.168.200.0/16",
			"role": "Primary",
			"netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("invalid network role value Primary"),
		},
		{
			desc: "invalid attachment definition for a localnet topology with role:primary",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"role": "primary",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("unexpected network field \"role\" primary for \"localnet\" topology, " +
				"localnet topology does not allow network roles to be set since its always a secondary network"),
		},
		{
			desc: "invalid attachment definition for a localnet topology with joinsubnet provided",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
			"subnets": "192.168.200.0/16",
			"joinSubnet": "100.66.0.0/16",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("localnet topology does not allow specifying join-subnet as services are not supported"),
		},
		{
			desc: "A layer2 primary UDN requires a subnet",
			inputNetAttachDefConfigSpec: `
{
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "netAttachDefName": "ns1/nad1",
            "role": "primary"
}`,
			expectedError: fmt.Errorf("the subnet attribute must be defined for layer2 primary user defined networks"),
		},
		{
			desc: "invalid not contained excludes layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "fda6::/48",
            "excludeSubnets": "fda7::38/128",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedError: fmt.Errorf("invalid subnet configuration: error while parsing subnets: the provided network subnets do not contain excluded subnets fda7::38/128"),
		},
		{
			desc: "multiple subnets and excludes localnet topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "localnet",
            "subnets": "192.168.1.1/26, fda6::/48",
            "excludeSubnets": "192.168.1.38/32, fda6::38/128",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:       "localnet",
				NADName:        "ns1/nad1",
				MTU:            1400,
				Subnets:        "192.168.1.1/26, fda6::/48",
				ExcludeSubnets: "192.168.1.38/32, fda6::38/128",
				NetConf:        cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "multiple subnets and reserved subnets layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24, fda6::/64",
            "reservedSubnets": "192.168.1.0/28, fda6::0/80",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24, fda6::/64",
				ReservedSubnets: "192.168.1.0/28, fda6::0/80",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "layer 2 with both excludes and reserved subnets",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24",
            "excludeSubnets": "192.168.1.200/29",
            "reservedSubnets": "192.168.1.0/28",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24",
				ExcludeSubnets:  "192.168.1.200/29",
				ReservedSubnets: "192.168.1.0/28",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
		{
			desc: "dual-stack reserved subnets layer 2 topology",
			inputNetAttachDefConfigSpec: `
    {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "subnets": "192.168.1.0/24, 2001:db8::/64",
            "reservedSubnets": "192.168.1.0/28, 2001:db8::/80",
            "netAttachDefName": "ns1/nad1"
    }
`,
			expectedNetConf: &ovncnitypes.NetConf{
				Topology:        "layer2",
				NADName:         "ns1/nad1",
				MTU:             1400,
				Subnets:         "192.168.1.0/24, 2001:db8::/64",
				ReservedSubnets: "192.168.1.0/28, 2001:db8::/80",
				NetConf:         cnitypes.NetConf{Name: "tenantred", Type: "ovn-k8s-cni-overlay"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			config.IPv4Mode = true
			config.IPv6Mode = true
			// Enable feature flags for reserved subnets tests
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true
			if test.unsupportedReason != "" {
				t.Skip(test.unsupportedReason)
			}
			g := gomega.NewWithT(t)
			networkAttachmentDefinition := applyNADDefaults(
				&nadv1.NetworkAttachmentDefinition{
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: test.inputNetAttachDefConfigSpec,
					},
				})
			if test.expectedError != nil {
				_, err := ParseNetConf(networkAttachmentDefinition)
				g.Expect(err).To(gomega.MatchError(test.expectedError.Error()))
			} else {
				g.Expect(ParseNetConf(networkAttachmentDefinition)).To(gomega.Equal(test.expectedNetConf))
			}
		})
	}
}

func TestIPAMType(t *testing.T) {
	t.Run("localnet network with DHCP IPAM", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network", IPAM: cnitypes.IPAM{Type: "dhcp"}},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(netInfo.IPAMType()).To(gomega.Equal("dhcp"))
	})

	t.Run("localnet network without IPAM", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network"},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(netInfo.IPAMType()).To(gomega.BeEmpty())
	})

	t.Run("default network", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect((&DefaultNetInfo{}).IPAMType()).To(gomega.BeEmpty())
	})

	t.Run("copy preserves the IPAM type", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network", IPAM: cnitypes.IPAM{Type: "dhcp"}},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(NewMutableNetInfo(netInfo).IPAMType()).To(gomega.Equal("dhcp"))
	})
}

// TestIPModeForDHCPNetworks pins the address-family correction for subnet-less
// DHCP networks: IPMode is derived from subnets, which a DHCP network has
// none of, yet its pods carry IPv4 addresses assigned by the external server.
// Without the correction every IPMode consumer (QoS and policy match strings,
// selector address-set creation) sees "no families" and silently produces
// nothing.
func TestIPModeForDHCPNetworks(t *testing.T) {
	t.Run("subnet-less localnet with DHCP IPAM is IPv4", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network", IPAM: cnitypes.IPAM{Type: "dhcp"}},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		v4, v6 := netInfo.IPMode()
		g.Expect(v4).To(gomega.BeTrue(), "DHCP IPAM assigns IPv4 addresses")
		g.Expect(v6).To(gomega.BeFalse(), "DHCP IPAM mode is IPv4-only")
	})

	t.Run("subnet-less localnet without IPAM has no families", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network"},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		v4, v6 := netInfo.IPMode()
		g.Expect(v4).To(gomega.BeFalse())
		g.Expect(v6).To(gomega.BeFalse())
	})

	t.Run("copy preserves the corrected mode", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network", IPAM: cnitypes.IPAM{Type: "dhcp"}},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		v4, v6 := NewMutableNetInfo(netInfo).IPMode()
		g.Expect(v4).To(gomega.BeTrue())
		g.Expect(v6).To(gomega.BeFalse())
	})
}

func TestDoesNetworkHaveDiscoverablePodIPs(t *testing.T) {
	newLocalnet := func(t *testing.T, ipamType, subnets string) NetInfo {
		t.Helper()
		g := gomega.NewWithT(t)
		netInfo, err := NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "localnet-network", IPAM: cnitypes.IPAM{Type: ipamType}},
			Topology: ovntypes.LocalnetTopology,
			NADName:  "ns1/nad1",
			Subnets:  subnets,
		})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return netInfo
	}

	t.Run("localnet with subnets (own IPAM)", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(DoesNetworkHaveDiscoverablePodIPs(newLocalnet(t, "", "10.10.0.0/16"))).To(gomega.BeTrue())
	})

	t.Run("subnet-less localnet with DHCP IPAM", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(DoesNetworkHaveDiscoverablePodIPs(newLocalnet(t, "dhcp", ""))).To(gomega.BeTrue(),
			"DHCP-learned IPs are reported via the pod-networks annotation, so they are discoverable")
	})

	t.Run("subnet-less localnet without IPAM", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(DoesNetworkHaveDiscoverablePodIPs(newLocalnet(t, "", ""))).To(gomega.BeFalse(),
			"pods configure their own IPs, invisible to ovnkube")
	})
}

func TestJoinSubnets(t *testing.T) {
	type testConfig struct {
		desc            string
		inputNetConf    *ovncnitypes.NetConf
		expectedSubnets []*net.IPNet
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(config.Gateway.V4JoinSubnet),
				ovntest.MustParseIPNet(config.Gateway.V6JoinSubnet),
			},
		},
		{
			desc: "secondaryL3NetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6),
			},
		},
		{
			desc: "secondaryL2NetInfo with default join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.Layer2Topology,
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6),
			},
		},
		{
			desc: "secondaryLocalNetInfo with nil join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "blue-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			expectedSubnets: nil,
		},
		{
			desc: "secondaryL2NetInfo with user configured v4 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer2Topology,
				JoinSubnet: "100.68.0.0/16",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("100.68.0.0/16"),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL3NetInfo with user configured v6 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer3Topology,
				JoinSubnet: "2001:db8:abcd:1234::/64",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet("2001:db8:abcd:1234::/64"), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL3NetInfo with user configured v4&&v6 join subnet",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer3Topology,
				JoinSubnet: "100.68.0.0/16,2001:db8:abcd:1234::/64",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("100.68.0.0/16"),
				ovntest.MustParseIPNet("2001:db8:abcd:1234::/64"), // given user only provided v4, we set v6 to default value
			},
		},
		{
			desc: "secondaryL2NetInfo with user configured empty join subnet value takes default value",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:    cnitypes.NetConf{Name: "blue-network"},
				Topology:   ovntypes.Layer2Topology,
				JoinSubnet: "",
			},
			expectedSubnets: []*net.IPNet{
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV4),
				ovntest.MustParseIPNet(ovntypes.UserDefinedPrimaryNetworkJoinSubnetV6), // given user only provided v4, we set v6 to default value
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.JoinSubnets()).To(gomega.Equal(test.expectedSubnets))
			if netInfo.TopologyType() != ovntypes.LocalnetTopology {
				g.Expect(netInfo.JoinSubnetV4()).To(gomega.Equal(test.expectedSubnets[0]))
				g.Expect(netInfo.JoinSubnetV6()).To(gomega.Equal(test.expectedSubnets[1]))
			}
		})
	}
}

func TestIsPrimaryNetwork(t *testing.T) {
	type testConfig struct {
		desc            string
		inputNetConf    *ovncnitypes.NetConf
		expectedPrimary bool
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "defaultNetInfo with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: false,
		},
		{
			desc: "defaultNetInfo with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL3 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL3 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: true,
		},
		{
			desc: "secondaryNetInfoL3 with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL2 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
			},
			expectedPrimary: false,
		},
		{
			desc: "secondaryNetInfoL2 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedPrimary: true,
		},
		{
			desc: "secondaryNetInfoL2 with role set to secondary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRoleSecondary,
			},
			expectedPrimary: false,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.IsPrimaryNetwork()).To(gomega.Equal(test.expectedPrimary))
		})
	}
}

func TestIsDefault(t *testing.T) {
	type testConfig struct {
		desc               string
		inputNetConf       *ovncnitypes.NetConf
		expectedDefaultVal bool
	}

	tests := []testConfig{
		{
			desc: "defaultNetInfo",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedDefaultVal: true,
		},
		{
			desc: "secondaryNetInfoL3 with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedDefaultVal: false,
		},
		{
			desc: "secondaryNetInfoL2 with role set to primary",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			expectedDefaultVal: false,
		},
		{
			desc: "secondaryNetInfoLocalNet with role unspecified",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			expectedDefaultVal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(netInfo.IsDefault()).To(gomega.Equal(test.expectedDefaultVal))
		})
	}
}

func TestGetPodNADToNetworkMapping(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                          string
		inputNamespace                string
		inputNetConf                  *ovncnitypes.NetConf
		inputPodAnnotations           map[string]string
		expectedError                 error
		expectedIsAttachmentRequested bool
		expectedNetworkMapping        map[string]struct{}
	}

	tests := []testConfig{
		{
			desc:                "Looking for a network *not* present in the pod's attachment requests",
			inputNamespace:      namespaceName,
			inputPodAnnotations: map[string]string{nadv1.NetworkAttachmentAnnot: "[]"},
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: false,
			expectedNetworkMapping:        map[string]struct{}{},
		},
		{
			desc: "Looking for a network present in the pod's attachment requests",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkMapping:        map[string]struct{}{GetNADName(namespaceName, attachmentName): {}},
		},
		{
			desc:           "Multiple attachments to the same network in the same pod",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: fmt.Sprintf("%[1]s,%[1]s", GetNADName(namespaceName, attachmentName)),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkMapping: map[string]struct{}{
				GetNADName(namespaceName, attachmentName):                      {},
				GetIndexedNADKey(GetNADName(namespaceName, attachmentName), 1): {},
			},
		},
		{
			desc:           "Attaching to a secondary network to a user defined primary network is not supported",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedError:          fmt.Errorf("unexpected primary network \"l3-network\" specified with a NetworkSelectionElement &{Name:attachment1 Namespace:ns1 IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}"),
			expectedNetworkMapping: map[string]struct{}{},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   test.inputNamespace,
					Annotations: test.inputPodAnnotations,
				},
			}

			var resolver func(string) string
			if netInfo.IsUserDefinedNetwork() {
				expectedNADKey := test.inputNetConf.NADName
				if expectedNADKey == "" {
					t.Fatalf("missing NAD name for user-defined network %q", netInfo.GetNetworkName())
				}
				resolver = func(nadKey string) string {
					if nadKey == expectedNADKey {
						return netInfo.GetNetworkName()
					}
					return ""
				}
			}

			isAttachmentRequested, networkMap, err := getPodNADToNetworkMapping(pod, netInfo, resolver)
			if test.expectedError != nil {
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
			actualNetworkMapping := map[string]struct{}{}
			for nadName := range networkMap {
				actualNetworkMapping[nadName] = struct{}{}
			}
			g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
			g.Expect(actualNetworkMapping).To(gomega.Equal(test.expectedNetworkMapping))
		})
	}
}

func TestGetPodNADToNetworkMappingWithActiveNetwork(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                             string
		inputNamespace                   string
		inputNetConf                     *ovncnitypes.NetConf
		inputPrimaryUDNConfig            *ovncnitypes.NetConf
		inputPodAnnotations              map[string]string
		expectedError                    error
		expectedIsAttachmentRequested    bool
		expectedNetworkSelectionElements map[string]*nadv1.NetworkSelectionElement
		enablePreconfiguredUDNAddresses  bool
		injectPrimaryUDNNADs             []string
	}

	tests := []testConfig{
		{
			desc: "there isn't a primary UDN",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the netinfo is different from the active network",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "another-network"},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, "another-network"),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: false,
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs but the pod does not request it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Using deprecated UDN IPAMClaim annotation",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Pod has both defaultNSE with ipam-claim reference and UDNIPAMClaim annotations, specifying the same IPAMClaim CR",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:          `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it." +
				"Pod has both defaultNSE with ipam-claim reference and UDNIPAMClaim annotations, specifying different IPAMClaim CR." +
				"DefaultNSE's ipam-claim reference should take precedence",
			// verify backward compatibility for deprecated annotation 'k8s.ovn.org/primary-udn-ipamclaim'
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:          `[{"ipam-claim-reference":"the-one-to-the-left-of-the-pony","namespace":"ns1","name":"attachment1"}]`,
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-right-of-the-horse",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a secondary layer2 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer3 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer3Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer3Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot:  GetNADName(namespaceName, "another-network"),
				DeprecatedOvnUDNIPAMClaimName: "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP, MAC and IPAMClaimRef on default network annotation for it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           ovntypes.Layer2Topology,
				NADName:            GetNADName(namespaceName, attachmentName),
				Role:               ovntypes.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff", "ipam-claim-reference": "my-ipam-claim"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPRequest:          []string{"192.168.0.3/24", "fda6::3/48"},
					MacRequest:         "aa:bb:cc:dd:ee:ff",
					IPAMClaimReference: "my-ipam-claim",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected namespace",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "other-namespace", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE namespace "other-namespace", expected "ovn-kubernetes"`),
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected name",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "unexpected-name", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE name "unexpected-name", expected "default"`),
		},
		{
			desc:           "should fail when no nad of the active network found on the pod namespace",
			inputNamespace: "non-existent-ns",
			expectedError:  fmt.Errorf(`failed to get primary NAD for namespace "non-existent-ns": no active NAD found for namespace "non-existent-ns"`),
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				NADName:  GetNADName(namespaceName, attachmentName),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
		},
		{
			desc: "primary l2 CUDN (replicated NADs), should return the correct active network according to pod namespace",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "cluster_udn_l2p"},
				NADName:  GetNADName("red", "l2p"),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "cluster_udn_l2p"},
				NADName:  GetNADName("red", "l2p"),
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
			},
			injectPrimaryUDNNADs: []string{"blue/l2p", "green/l2p"},
			inputNamespace:       "blue",
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedIsAttachmentRequested:   true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"blue/l2p": {
					Name:       "l2p",
					Namespace:  "blue",
					IPRequest:  []string{"192.168.0.3/24", "fda6::3/48"},
					MacRequest: "aa:bb:cc:dd:ee:ff",
				},
			},
		},

		{
			desc: "default-network MAC is applied and IPs are ignored for Layer3 topology",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:       "attachment1",
					Namespace:  "ns1",
					IPRequest:  nil,
					MacRequest: "aa:bb:cc:dd:ee:ff",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "default-network IPs and MAC are ignored for Layer3 topology when the feature is disabled",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "default-network NAD without a MAC is ignored for Layer3 topology",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer3Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "default"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "default-network with bad format",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: ovntypes.Layer2Topology,
				NADName:  GetNADName(namespaceName, attachmentName),
				Role:     ovntypes.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"foo}`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`failed getting default-network annotation for pod "ns1/test-pod": %w`, fmt.Errorf(`GetK8sPodDefaultNetwork: failed to parse CRD object: parsePodNetworkAnnotation: failed to parse pod Network Attachment Selection Annotation JSON format: unexpected end of JSON input`)),
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
			})

			// Set custom network config based on test requirements
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = test.enablePreconfiguredUDNAddresses
			if test.enablePreconfiguredUDNAddresses {
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			}

			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			var primaryUDNNetInfo NetInfo
			if test.inputPrimaryUDNConfig != nil {
				primaryUDNNetInfo, err = NewNetInfo(test.inputPrimaryUDNConfig)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if test.inputPrimaryUDNConfig.NADName != "" {
					mutableNetInfo := NewMutableNetInfo(primaryUDNNetInfo)
					mutableNetInfo.AddNADs(test.inputPrimaryUDNConfig.NADName)
					if len(test.injectPrimaryUDNNADs) > 0 {
						mutableNetInfo.AddNADs(test.injectPrimaryUDNNADs...)
					}
					primaryUDNNetInfo = mutableNetInfo
				}
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   namespaceName,
					Annotations: test.inputPodAnnotations,
				},
			}
			if test.inputNamespace != "" {
				pod.Namespace = test.inputNamespace
			}

			expectedNADKey := test.inputNetConf.NADName
			if expectedNADKey == "" {
				t.Fatalf("missing NAD name for user-defined network %q", netInfo.GetNetworkName())
			}
			nadNetworkNames := map[string]string{
				expectedNADKey: netInfo.GetNetworkName(),
			}
			primaryNADByNamespace := map[string]string{}
			if primaryUDNNetInfo != nil {
				primaryNADKeys := make([]string, 0, 1+len(test.injectPrimaryUDNNADs))
				if test.inputPrimaryUDNConfig != nil && test.inputPrimaryUDNConfig.NADName != "" {
					primaryNADKeys = append(primaryNADKeys, test.inputPrimaryUDNConfig.NADName)
				}
				primaryNADKeys = append(primaryNADKeys, test.injectPrimaryUDNNADs...)
				for _, nadKey := range primaryNADKeys {
					nadNamespace, _, err := cache.SplitMetaNamespaceKey(nadKey)
					if err != nil {
						t.Fatalf("failed to split NAD key %q: %v", nadKey, err)
					}
					primaryNADByNamespace[nadNamespace] = nadKey
					nadNetworkNames[nadKey] = primaryUDNNetInfo.GetNetworkName()
				}
			}
			resolver := func(nadKey string) string {
				if networkName, ok := nadNetworkNames[nadKey]; ok {
					return networkName
				}
				return ""
			}

			isAttachmentRequested, networkSelectionElements, err := GetPodNADToNetworkMappingWithActiveNetwork(
				pod,
				netInfo,
				primaryUDNNetInfo,
				resolver,
				func(namespace string) (string, error) {
					if primaryUDNNetInfo == nil {
						return ovntypes.DefaultNetworkName, nil
					}
					if nadKey, ok := primaryNADByNamespace[namespace]; ok {
						return nadKey, nil
					}
					return "", fmt.Errorf("no active NAD found for namespace %q", namespace)
				},
			)

			if test.expectedError != nil {
				g.Expect(err).To(gomega.HaveOccurred(), "unexpected success operation, epecting error")
				g.Expect(err.Error()).To(gomega.Equal(test.expectedError.Error()))
			} else {
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
				g.Expect(networkSelectionElements).To(gomega.Equal(test.expectedNetworkSelectionElements))
			}
		})
	}
}

func TestSubnetOverlapCheck(t *testing.T) {
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/14")
	_, cidr6, _ := net.ParseCIDR("fe00::/16")
	_, svcCidr4, _ := net.ParseCIDR("172.30.0.0/16")
	_, svcCidr6, _ := net.ParseCIDR("fe01::/16")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 24}, {CIDR: cidr6, HostSubnetLength: 64}}
	config.Kubernetes.ServiceCIDRs = []*net.IPNet{svcCidr4, svcCidr6}
	config.Gateway.V4MasqueradeSubnet = "169.254.169.0/29"
	config.Gateway.V6MasqueradeSubnet = "fd69::/125"
	config.Gateway.V4JoinSubnet = "100.64.0.0/16"
	config.Gateway.V6JoinSubnet = "fd98::/64"
	config.ClusterManager.V4TransitSubnet = "100.88.0.0/16"
	config.ClusterManager.V6TransitSubnet = "fd97::/64"
	type testConfig struct {
		desc                        string
		inputNetAttachDefConfigSpec string
		expectedError               error
	}

	tests := []testConfig{
		{
			desc: "return error when IPv4 POD subnet in net-attach-def overlaps with transit switch subnet",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer3",
                    "subnets": "100.88.0.0/17",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("100.88.0.0/17")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetTransit, Subnet: MustParseCIDR(config.ClusterManager.V4TransitSubnet)}),
		},
		{
			desc: "return error when IPv4 POD subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "10.129.0.0/16",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("10.129.0.0/16")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetCluster, Subnet: cidr4}),
		},
		{
			desc: "return error when IPv4 join subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
				{
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16",
                    "joinSubnet": "100.64.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: MustParseCIDR("100.64.0.0/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetJoin, Subnet: MustParseCIDR(config.Gateway.V4JoinSubnet)}),
		},
		{
			desc: "return error when IPv6 POD subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16,fe01::/24",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedSubnets, Subnet: MustParseCIDR("fe01::/24")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetService, Subnet: svcCidr6},
			),
		},
		{
			desc: "return error when IPv6 join subnet in net-attach-def overlaps other subnets",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "192.168.0.0/16,fe02::/24",
                    "joinSubnet": "100.65.0.0/24,fd69::/112",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
			expectedError: config.NewSubnetOverlapError(
				config.ConfigSubnet{SubnetType: config.UserDefinedJoinSubnet, Subnet: MustParseCIDR("fd69::/112")},
				config.ConfigSubnet{SubnetType: config.ConfigSubnetMasquerade, Subnet: MustParseCIDR(config.Gateway.V6MasqueradeSubnet)},
			),
		},
		{
			desc: "excluded subnet should not be considered for overlap check",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "tenantred",
                    "type": "ovn-k8s-cni-overlay",
                    "topology": "layer2",
                    "subnets": "10.0.0.0/8",
                    "excludeSubnets": "10.128.0.0/14",
                    "joinSubnet": "100.65.0.0/24",
                    "primaryNetwork": true,
                    "netAttachDefName": "ns1/nad1"
                }
			`,
		},
		{
			desc: "return error when the network is not ovnk",
			inputNetAttachDefConfigSpec: `
                {
                    "name": "test",
                    "type": "sriov-cni"
                }
			`,
			expectedError: ErrorAttachDefNotOvnManaged,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			networkAttachmentDefinition := applyNADDefaults(
				&nadv1.NetworkAttachmentDefinition{
					Spec: nadv1.NetworkAttachmentDefinitionSpec{
						Config: test.inputNetAttachDefConfigSpec,
					},
				})
			if test.expectedError != nil {
				_, err := ParseNADInfo(networkAttachmentDefinition)
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(test.expectedError.Error())))
			} else {
				_, err := ParseNADInfo(networkAttachmentDefinition)
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestGetAnnotatedNetworkID(t *testing.T) {
	tests := []struct {
		desc        string
		annotations map[string]string
		expectedID  int
		expectError bool
	}{
		{
			desc:       "missing annotation returns InvalidID without error",
			expectedID: ovntypes.InvalidID,
		},
		{
			desc:        "malformed annotation returns an error",
			annotations: map[string]string{ovntypes.OvnNetworkIDAnnotation: "not-a-number"},
			expectError: true,
		},
		{
			desc:        "valid annotation",
			annotations: map[string]string{ovntypes.OvnNetworkIDAnnotation: "7"},
			expectedID:  7,
		},
		{
			desc:        "zero, the default network ID",
			annotations: map[string]string{ovntypes.OvnNetworkIDAnnotation: "0"},
			expectedID:  0,
		},
		{
			desc:        "negative annotation parses, indistinguishable from InvalidID",
			annotations: map[string]string{ovntypes.OvnNetworkIDAnnotation: "-1"},
			expectedID:  ovntypes.InvalidID,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			nad := applyNADDefaults(&nadv1.NetworkAttachmentDefinition{
				ObjectMeta: metav1.ObjectMeta{Annotations: test.annotations},
			})
			id, err := GetAnnotatedNetworkID(nad)
			if test.expectError {
				g.Expect(err).To(gomega.HaveOccurred())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(id).To(gomega.Equal(test.expectedID))
		})
	}
}

func TestParseNADInfoNetworkIDAnnotation(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	const nadConfig = `
        {
            "name": "tenantred",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer2",
            "netAttachDefName": "ns1/nad1"
        }
	`
	newNAD := func(annotations map[string]string) *nadv1.NetworkAttachmentDefinition {
		return applyNADDefaults(&nadv1.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Spec:       nadv1.NetworkAttachmentDefinitionSpec{Config: nadConfig},
		})
	}

	t.Run("annotated network ID overrides the default", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := ParseNADInfo(newNAD(map[string]string{ovntypes.OvnNetworkIDAnnotation: "7"}))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(netInfo.GetNetworkID()).To(gomega.Equal(7))
	})

	t.Run("missing annotation leaves InvalidID", func(t *testing.T) {
		g := gomega.NewWithT(t)
		netInfo, err := ParseNADInfo(newNAD(nil))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(netInfo.GetNetworkID()).To(gomega.Equal(ovntypes.InvalidID))
	})

	t.Run("malformed annotation fails the parse", func(t *testing.T) {
		g := gomega.NewWithT(t)
		_, err := ParseNADInfo(newNAD(map[string]string{ovntypes.OvnNetworkIDAnnotation: "not-a-number"}))
		g.Expect(err).To(gomega.HaveOccurred())
	})
}

func TestValidateNetConfOutboundSNAT(t *testing.T) {
	tests := []struct {
		name          string
		transport     string
		outboundSNAT  string
		expectedError string
	}{
		{
			name:         "empty outboundSNAT is accepted without no-overlay transport",
			outboundSNAT: "",
		},
		{
			name:         "enabled outboundSNAT is accepted with no-overlay transport",
			transport:    ovntypes.NetworkTransportNoOverlay,
			outboundSNAT: ovntypes.NoOverlaySNATEnabled,
		},
		{
			name:         "disabled outboundSNAT is accepted with no-overlay transport",
			transport:    ovntypes.NetworkTransportNoOverlay,
			outboundSNAT: ovntypes.NoOverlaySNATDisabled,
		},
		{
			name:          "enabled outboundSNAT is rejected without no-overlay transport",
			outboundSNAT:  ovntypes.NoOverlaySNATEnabled,
			expectedError: `outboundSNAT is only valid when transport is "no-overlay"`,
		},
		{
			name:          "enabled outboundSNAT is rejected with EVPN transport",
			transport:     ovntypes.NetworkTransportEVPN,
			outboundSNAT:  ovntypes.NoOverlaySNATEnabled,
			expectedError: `outboundSNAT is only valid when transport is "no-overlay"`,
		},
		{
			name:          "invalid outboundSNAT is rejected with no-overlay transport",
			transport:     ovntypes.NetworkTransportNoOverlay,
			outboundSNAT:  "maybe",
			expectedError: `invalid outboundSNAT "maybe"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			nadName := "namespace/network"
			netconf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "network",
				},
				NADName:      nadName,
				Topology:     ovntypes.Layer3Topology,
				Transport:    test.transport,
				OutboundSNAT: test.outboundSNAT,
			}

			err := ValidateNetConf(nadName, netconf)
			if test.expectedError != "" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(test.expectedError)))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestValidateNetConfUplinkGatewayMode(t *testing.T) {
	tests := []struct {
		name          string
		gatewayMode   config.GatewayMode
		uplink        string
		expectedError string
	}{
		{
			name:        "uplink is accepted in shared gateway mode",
			gatewayMode: config.GatewayModeShared,
			uplink:      "blue-uplink",
		},
		{
			name:          "uplink is rejected in local gateway mode",
			gatewayMode:   config.GatewayModeLocal,
			uplink:        "blue-uplink",
			expectedError: `uplink "blue-uplink" is supported only in shared gateway mode`,
		},
		{
			name:        "local gateway mode is accepted without uplink",
			gatewayMode: config.GatewayModeLocal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.Gateway.Mode = test.gatewayMode

			nadName := "namespace/network"
			netconf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "network",
				},
				NADName:  nadName,
				Topology: ovntypes.Layer3Topology,
				Uplink:   test.uplink,
			}

			err := ValidateNetConf(nadName, netconf)
			if test.expectedError != "" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(test.expectedError)))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestValidateNetConfMACSecurityMode(t *testing.T) {
	config.IPv4Mode = true
	tests := []struct {
		name               string
		topology           string
		role               string
		subnets            string
		allowPersistentIPs bool
		macSecurityMode    string
		expectedError      string
	}{
		// MAC spoof protection is enabled by default; when empty or unset, it should preserve the current behavior
		{
			name:            "empty is accepted. Preserve current behaviour - enable MAC spoof protection",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: "",
		},
		{
			name:            "Enabled is accepted, undefined role",
			topology:        ovntypes.Layer2Topology,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
		},
		{
			name:            "Enabled is accepted, primary network",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRolePrimary,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
			subnets:         "192.168.1.0/16",
		},
		{
			name:            "Enabled is accepted, primary network",
			topology:        ovntypes.Layer3Topology,
			role:            ovntypes.NetworkRolePrimary,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
		},
		{
			name:            "Enabled is accepted, secondary network, L2",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
		},
		{
			name:            "Enabled is accepted, secondary network, localnet",
			topology:        ovntypes.LocalnetTopology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
		},
		{
			name:            "Enabled is accepted, secondary network, L3",
			topology:        ovntypes.Layer3Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeEnabled,
		},
		//  MAC spoof protection disabled support secondary layer2 or localnet, ipamless, networks
		{
			name:            "Disabled on layer2 secondary network is accepted",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeDisabled,
		},
		{
			name:            "Disabled on localnet secondary network is accepted",
			topology:        ovntypes.LocalnetTopology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeDisabled,
		},
		{
			name:            "invalid value is rejected",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: "maybe",
			expectedError:   "invalid MAC security mode: invalid value, must be one of: [enabled disabled]",
		},
		{
			name:            "Disabled on primary network is rejected",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRolePrimary,
			macSecurityMode: ovntypes.MACSecurityModeDisabled,
			expectedError:   "invalid MAC security mode: only supported on secondary networks",
		},
		{
			name:            "Disabled on layer3 topology is rejected",
			topology:        ovntypes.Layer3Topology,
			role:            ovntypes.NetworkRoleSecondary,
			macSecurityMode: ovntypes.MACSecurityModeDisabled,
			expectedError:   "invalid MAC security mode: unsupported topology, must be one of [layer2 localnet]",
		},
		{
			name:            "Disabled when IPAM is enabled, subnets are set, is rejected",
			topology:        ovntypes.Layer2Topology,
			role:            ovntypes.NetworkRoleSecondary,
			subnets:         "192.168.1.0/24",
			macSecurityMode: ovntypes.MACSecurityModeDisabled,
			expectedError:   "invalid MAC security mode: cannot be used when IPAM is enabled (subnets are specified)",
		},
		{
			name:               "Disabled when IPAM features are set, allowPersistentIPs is set, is rejected",
			topology:           ovntypes.Layer2Topology,
			role:               ovntypes.NetworkRoleSecondary,
			allowPersistentIPs: true,
			macSecurityMode:    ovntypes.MACSecurityModeDisabled,
			expectedError:      "allowPersistentIPs requires OVN-Kubernetes-managed IPAM (the subnets attribute must be set)",
		},
		{
			name:               "Disabled when IPAM features are set, subnets & allowPersistentIPs are set, is rejected",
			topology:           ovntypes.Layer2Topology,
			role:               ovntypes.NetworkRoleSecondary,
			subnets:            "192.168.1.0/24",
			allowPersistentIPs: true,
			macSecurityMode:    ovntypes.MACSecurityModeDisabled,
			expectedError:      "invalid MAC security mode: cannot be used when IPAM is enabled (subnets are specified)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			nadName := "myns/mynetwork"
			netconf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "mynetwork",
				},
				NADName:            nadName,
				Topology:           test.topology,
				Role:               test.role,
				Subnets:            test.subnets,
				AllowPersistentIPs: test.allowPersistentIPs,
				MACSecurityMode:    test.macSecurityMode,
			}

			err := ValidateNetConf(nadName, netconf)
			if test.expectedError != "" {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(test.expectedError)))
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func TestDefaultNetInfoMACSecurityMode(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect((&DefaultNetInfo{}).MACSecurityMode()).To(gomega.Equal(ovntypes.MACSecurityModeEnabled),
		"default network should always report MAC security as enabled")
}

func TestUDNNetInfoMACSecurityMode(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect((&userDefinedNetInfo{}).MACSecurityMode()).To(gomega.Equal(ovntypes.MACSecurityModeEnabled),
		"when mac-security-mode unset, udn network should report its enabled")
}

func TestNewNetInfo(t *testing.T) {
	type testConfig struct {
		desc          string
		subnets       string
		ipv4Cluster   bool
		ipv6Cluster   bool
		expectedError error
	}

	tests := []testConfig{
		{
			desc:        "ipv4 primary network in ipv4 cluster",
			subnets:     "192.168.200.0/16",
			ipv4Cluster: true,
		},
		{
			desc:          "ipv4 primary network in ipv6 cluster",
			subnets:       "192.168.200.0/16",
			ipv6Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv4 subnets but the cluster does not support ipv4"),
		},
		{
			desc:        "ipv4 primary network in dualstack cluster",
			subnets:     "192.168.200.0/16",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
		{
			desc:          "ipv6 primary network in ipv4 cluster",
			subnets:       "fda6::/48",
			ipv4Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv6 subnets but the cluster does not support ipv6"),
		},
		{
			desc:        "ipv6 primary network in ipv6 cluster",
			subnets:     "fda6::/48",
			ipv6Cluster: true,
		},
		{
			desc:        "ipv6 primary network in dualstack cluster",
			subnets:     "fda6::/48",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
		{
			desc:          "dualstack primary network in ipv4 cluster",
			subnets:       "192.168.200.0/16, fda6::/48",
			ipv4Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv6 subnets but the cluster does not support ipv6"),
		},
		{
			desc:          "dualstack primary network in ipv6 cluster",
			subnets:       "192.168.200.0/16, fda6::/48",
			ipv6Cluster:   true,
			expectedError: fmt.Errorf("network l3-network is attempting to use ipv4 subnets but the cluster does not support ipv4"),
		},
		{
			desc:        "dualstack primary network in dualstack cluster",
			subnets:     "192.168.200.0/16, fda6::/48",
			ipv4Cluster: true,
			ipv6Cluster: true,
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			inputNetConf := &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  test.subnets,
			}
			config.IPv4Mode = test.ipv4Cluster
			config.IPv6Mode = test.ipv6Cluster
			g := gomega.NewWithT(t)
			_, err := NewNetInfo(inputNetConf)
			if test.expectedError != nil {
				g.Expect(err).To(gomega.MatchError(test.expectedError), "should return an error for invalid network configuration")
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred(), "should not return an error for valid network configuration")
			}
		})
	}
}

func TestAreNetworksCompatible(t *testing.T) {
	tests := []struct {
		desc                   string
		aNetwork               NetInfo
		anotherNetwork         NetInfo
		expectedResult         bool
		expectationDescription string
	}{
		{
			desc:                   "physical network name update",
			aNetwork:               &userDefinedNetInfo{physicalNetworkName: "A"},
			anotherNetwork:         &userDefinedNetInfo{physicalNetworkName: "B"},
			expectedResult:         false,
			expectationDescription: "we should reconcile on physical network name updates",
		},
		{
			desc:                   "networks with empty (default) transport should be compatible",
			aNetwork:               &userDefinedNetInfo{transport: ""},
			anotherNetwork:         &userDefinedNetInfo{transport: ""},
			expectedResult:         true,
			expectationDescription: "networks with no EVPN config should be compatible",
		},
		{
			desc:                   "ipam.type update (no IPAM to DHCP)",
			aNetwork:               &userDefinedNetInfo{},
			anotherNetwork:         &userDefinedNetInfo{ipamType: "dhcp"},
			expectedResult:         false,
			expectationDescription: "toggling ipam.type must recreate the network controller",
		},
		{
			desc:                   "same dhcp ipam.type is compatible",
			aNetwork:               &userDefinedNetInfo{ipamType: "dhcp"},
			anotherNetwork:         &userDefinedNetInfo{ipamType: "dhcp"},
			expectedResult:         true,
			expectationDescription: "unchanged ipam.type must not force a recreate",
		},
		{
			desc:           "macSecurityMode, unchanged is compatible (enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "enabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "enabled"},
			expectedResult: true,
		},
		{
			desc:           "macSecurityMode, unchanged is compatible (implicitly enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: ""},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: ""},
			expectedResult: true,
		},
		{
			desc:           "macSecurityMode, unchanged is compatible (disabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "disabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "disabled"},
			expectedResult: true,
		},
		{
			desc:           "macSecurityMode, unchanged is compatible (implicit to explicit enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: ""},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "enabled"},
			expectedResult: true,
		},
		{
			desc:           "macSecurityMode, unchanged is compatible (explicit to implicit enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "enabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: ""},
			expectedResult: true,
		},
		{
			desc:           "macSecurityMode change is not compatible (enabled -> disabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "enabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "disabled"},
			expectedResult: false,
		},
		{
			desc:           "macSecurityMode change is not compatible (implicit enabled -> disabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: ""},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "disabled"},
			expectedResult: false,
		},
		{
			desc:           "macSecurityMode change is not compatible (disabled -> implicit enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "disabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: ""},
			expectedResult: false,
		},
		{
			desc:           "macSecurityMode change is not compatible (disabled -> enabled)",
			aNetwork:       &userDefinedNetInfo{macSecurityMode: "disabled"},
			anotherNetwork: &userDefinedNetInfo{macSecurityMode: "enabled"},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(AreNetworksCompatible(test.aNetwork, test.anotherNetwork)).To(
				gomega.Equal(test.expectedResult),
				test.expectationDescription,
			)
		})
	}
}

func applyNADDefaults(nad *nadv1.NetworkAttachmentDefinition) *nadv1.NetworkAttachmentDefinition {
	const (
		name      = "nad1"
		namespace = "ns1"
	)
	nad.Name = name
	nad.Namespace = namespace
	return nad
}

func TestGetNodeManagementIP(t *testing.T) {
	testCases := []struct {
		name       string
		netConf    *ovncnitypes.NetConf
		hostSubnet string
		expectedIP *net.IPNet
	}{
		{
			name: "DefaultNetInfo should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer3 UDN should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l3-network"},
				Topology:              ovntypes.Layer3Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer2 primary UDN without infrastructure subnets should return traditional .2 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.2/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate the second usable IP from infrastructure subnet",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.4/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.5/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the broadcast IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.5",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.255/32, 10.0.0.100/32",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.100/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet without conflicting with default gateway",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.2",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet without conflicting with the default GW ip",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "10.0.0.2",
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::/126",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::2/64"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6 - last available IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs:     "2001:db8::ffff:ffff:ffff:fffe",
				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::ffff:ffff:ffff:fffe/127",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::ffff:ffff:ffff:ffff/64"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true

			netInfo, err := NewNetInfo(tc.netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			hostSubnet := ovntest.MustParseIPNet(tc.hostSubnet)

			result := netInfo.GetNodeManagementIP(hostSubnet)
			if result == nil {
				t.Fatalf("GetNodeManagementIP returned nil")
			}

			if !result.IP.Equal(tc.expectedIP.IP) {
				t.Errorf("Expected IP %s, got %s", tc.expectedIP.IP.String(), result.IP.String())
			}

			if result.Mask.String() != tc.expectedIP.Mask.String() {
				t.Errorf("Expected mask %s, got %s", tc.expectedIP.Mask.String(), result.Mask.String())
			}
		})
	}
}

func TestEVPNConfig(t *testing.T) {
	type testConfig struct {
		desc                      string
		inputNetConf              *ovncnitypes.NetConf
		expectedTransport         string
		expectedVTEPName          string
		expectedMACVRFVNI         int32
		expectedMACVRFRouteTarget string
		expectedMACVRFVID         int
		expectedIPVRFVNI          int32
		expectedIPVRFRouteTarget  string
		expectedIPVRFVID          int
	}

	tests := []testConfig{
		{
			desc: "default network has no EVPN config",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
				Topology: ovntypes.Layer3Topology,
			},
			expectedTransport:         "",
			expectedVTEPName:          "",
			expectedMACVRFVNI:         0,
			expectedMACVRFRouteTarget: "",
			expectedIPVRFVNI:          0,
			expectedIPVRFRouteTarget:  "",
		},
		{
			desc: "layer3 network without EVPN config",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: ovntypes.Layer3Topology,
			},
			expectedTransport:         "",
			expectedVTEPName:          "",
			expectedMACVRFVNI:         0,
			expectedMACVRFRouteTarget: "",
			expectedIPVRFVNI:          0,
			expectedIPVRFRouteTarget:  "",
		},
		{
			desc: "layer3 network with EVPN transport and IP-VRF only",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:   cnitypes.NetConf{Name: "evpn-l3-network"},
				Topology:  ovntypes.Layer3Topology,
				Transport: "evpn",
				EVPN: &ovncnitypes.EVPNConfig{
					VTEP: "my-vtep",
					IPVRF: &ovncnitypes.VRFConfig{
						VNI:         2000,
						RouteTarget: "65000:2000",
					},
				},
			},
			expectedTransport:         "evpn",
			expectedVTEPName:          "my-vtep",
			expectedMACVRFVNI:         0,
			expectedMACVRFRouteTarget: "",
			expectedIPVRFVNI:          2000,
			expectedIPVRFRouteTarget:  "65000:2000",
		},
		{
			desc: "layer2 network with EVPN transport and MAC-VRF only",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:   cnitypes.NetConf{Name: "evpn-l2-network"},
				Topology:  ovntypes.Layer2Topology,
				Transport: "evpn",
				EVPN: &ovncnitypes.EVPNConfig{
					VTEP: "my-vtep",
					MACVRF: &ovncnitypes.VRFConfig{
						VNI:         100,
						RouteTarget: "65000:100",
					},
				},
			},
			expectedTransport:         "evpn",
			expectedVTEPName:          "my-vtep",
			expectedMACVRFVNI:         100,
			expectedMACVRFRouteTarget: "65000:100",
			expectedIPVRFVNI:          0,
			expectedIPVRFRouteTarget:  "",
		},
		{
			desc: "layer2 network with EVPN transport and both MAC-VRF and IP-VRF (symmetric IRB)",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:   cnitypes.NetConf{Name: "evpn-l2-symmetric"},
				Topology:  ovntypes.Layer2Topology,
				Transport: "evpn",
				EVPN: &ovncnitypes.EVPNConfig{
					VTEP: "symmetric-vtep",
					MACVRF: &ovncnitypes.VRFConfig{
						VNI:         100,
						RouteTarget: "65000:100",
					},
					IPVRF: &ovncnitypes.VRFConfig{
						VNI:         1000,
						RouteTarget: "65000:1000",
					},
				},
			},
			expectedTransport:         "evpn",
			expectedVTEPName:          "symmetric-vtep",
			expectedMACVRFVNI:         100,
			expectedMACVRFRouteTarget: "65000:100",
			expectedIPVRFVNI:          1000,
			expectedIPVRFRouteTarget:  "65000:1000",
		},
		{
			desc: "layer2 network with EVPN transport including VIDs (allocated by controller)",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:   cnitypes.NetConf{Name: "evpn-with-vids"},
				Topology:  ovntypes.Layer2Topology,
				Transport: "evpn",
				EVPN: &ovncnitypes.EVPNConfig{
					VTEP: "vid-vtep",
					MACVRF: &ovncnitypes.VRFConfig{
						VNI:         100,
						RouteTarget: "65000:100",
						VID:         12,
					},
					IPVRF: &ovncnitypes.VRFConfig{
						VNI:         1000,
						RouteTarget: "65000:1000",
						VID:         13,
					},
				},
			},
			expectedTransport:         "evpn",
			expectedVTEPName:          "vid-vtep",
			expectedMACVRFVNI:         100,
			expectedMACVRFRouteTarget: "65000:100",
			expectedMACVRFVID:         12,
			expectedIPVRFVNI:          1000,
			expectedIPVRFRouteTarget:  "65000:1000",
			expectedIPVRFVID:          13,
		},
		{
			desc: "EVPN config with VNI only (no route target)",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:   cnitypes.NetConf{Name: "evpn-minimal"},
				Topology:  ovntypes.Layer2Topology,
				Transport: "evpn",
				EVPN: &ovncnitypes.EVPNConfig{
					VTEP: "minimal-vtep",
					MACVRF: &ovncnitypes.VRFConfig{
						VNI: 500,
					},
				},
			},
			expectedTransport:         "evpn",
			expectedVTEPName:          "minimal-vtep",
			expectedMACVRFVNI:         500,
			expectedMACVRFRouteTarget: "",
			expectedIPVRFVNI:          0,
			expectedIPVRFRouteTarget:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(netInfo.Transport()).To(gomega.Equal(test.expectedTransport), "Transport mismatch")
			g.Expect(netInfo.EVPNVTEPName()).To(gomega.Equal(test.expectedVTEPName), "VTEP name mismatch")
			g.Expect(netInfo.EVPNMACVRFVNI()).To(gomega.Equal(test.expectedMACVRFVNI), "MAC-VRF VNI mismatch")
			g.Expect(netInfo.EVPNMACVRFRouteTarget()).To(gomega.Equal(test.expectedMACVRFRouteTarget), "MAC-VRF RouteTarget mismatch")
			g.Expect(netInfo.EVPNMACVRFVID()).To(gomega.Equal(test.expectedMACVRFVID), "MAC-VRF VID mismatch")
			g.Expect(netInfo.EVPNIPVRFVNI()).To(gomega.Equal(test.expectedIPVRFVNI), "IP-VRF VNI mismatch")
			g.Expect(netInfo.EVPNIPVRFRouteTarget()).To(gomega.Equal(test.expectedIPVRFRouteTarget), "IP-VRF RouteTarget mismatch")
			g.Expect(netInfo.EVPNIPVRFVID()).To(gomega.Equal(test.expectedIPVRFVID), "IP-VRF VID mismatch")
		})
	}
}

func TestEVPNNetworkCompatibility(t *testing.T) {
	tests := []struct {
		desc                   string
		aNetwork               NetInfo
		anotherNetwork         NetInfo
		expectedResult         bool
		expectationDescription string
	}{
		{
			desc:                   "same EVPN config should be compatible",
			aNetwork:               &userDefinedNetInfo{transport: "evpn", evpn: &ovncnitypes.EVPNConfig{VTEP: "vtep1"}},
			anotherNetwork:         &userDefinedNetInfo{transport: "evpn", evpn: &ovncnitypes.EVPNConfig{VTEP: "vtep1"}},
			expectedResult:         true,
			expectationDescription: "networks with same EVPN config should be compatible",
		},
		{
			desc:                   "different transport should not be compatible",
			aNetwork:               &userDefinedNetInfo{transport: "evpn"},
			anotherNetwork:         &userDefinedNetInfo{transport: "no-overlay"},
			expectedResult:         false,
			expectationDescription: "networks with different transport should not be compatible",
		},
		{
			desc:                   "different VTEP name should not be compatible",
			aNetwork:               &userDefinedNetInfo{transport: "evpn", evpn: &ovncnitypes.EVPNConfig{VTEP: "vtep1"}},
			anotherNetwork:         &userDefinedNetInfo{transport: "evpn", evpn: &ovncnitypes.EVPNConfig{VTEP: "vtep2"}},
			expectedResult:         false,
			expectationDescription: "networks with different VTEP name should not be compatible",
		},
		{
			desc: "different MAC-VRF VNI should not be compatible",
			aNetwork: &userDefinedNetInfo{
				transport: "evpn",
				evpn: &ovncnitypes.EVPNConfig{
					VTEP:   "vtep1",
					MACVRF: &ovncnitypes.VRFConfig{VNI: 100},
				},
			},
			anotherNetwork: &userDefinedNetInfo{
				transport: "evpn",
				evpn: &ovncnitypes.EVPNConfig{
					VTEP:   "vtep1",
					MACVRF: &ovncnitypes.VRFConfig{VNI: 200},
				},
			},
			expectedResult:         false,
			expectationDescription: "networks with different MAC-VRF VNI should not be compatible",
		},
		{
			desc: "different IP-VRF route target should not be compatible",
			aNetwork: &userDefinedNetInfo{
				transport: "evpn",
				evpn: &ovncnitypes.EVPNConfig{
					VTEP:  "vtep1",
					IPVRF: &ovncnitypes.VRFConfig{VNI: 1000, RouteTarget: "65000:1000"},
				},
			},
			anotherNetwork: &userDefinedNetInfo{
				transport: "evpn",
				evpn: &ovncnitypes.EVPNConfig{
					VTEP:  "vtep1",
					IPVRF: &ovncnitypes.VRFConfig{VNI: 1000, RouteTarget: "65001:1000"},
				},
			},
			expectedResult:         false,
			expectationDescription: "networks with different IP-VRF route target should not be compatible",
		},
		{
			desc:                   "both nil EVPN config should be compatible",
			aNetwork:               &userDefinedNetInfo{transport: ""},
			anotherNetwork:         &userDefinedNetInfo{transport: ""},
			expectedResult:         true,
			expectationDescription: "networks with no EVPN config should be compatible",
		},
		{
			desc:                   "one nil EVPN config should not be compatible",
			aNetwork:               &userDefinedNetInfo{transport: "evpn", evpn: &ovncnitypes.EVPNConfig{VTEP: "vtep1"}},
			anotherNetwork:         &userDefinedNetInfo{transport: "evpn"},
			expectedResult:         false,
			expectationDescription: "network with EVPN config vs without should not be compatible",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(AreNetworksCompatible(test.aNetwork, test.anotherNetwork)).To(
				gomega.Equal(test.expectedResult),
				test.expectationDescription,
			)
		})
	}
}

func TestGetNodeGatewayIP(t *testing.T) {
	testCases := []struct {
		name       string
		netConf    *ovncnitypes.NetConf
		hostSubnet string
		expectedIP *net.IPNet
	}{
		{
			name: "DefaultNetInfo should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: ovntypes.DefaultNetworkName},
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer3 UDN should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l3-network"},
				Topology:              ovntypes.Layer3Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN without infrastructure subnets should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,
				Subnets:  "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with custom default gateway IP should return that custom IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:           cnitypes.NetConf{Name: "l2-network"},
				Topology:          ovntypes.Layer2Topology,
				Role:              ovntypes.NetworkRolePrimary,
				DefaultGatewayIPs: "10.0.0.5",
				Subnets:           "10.0.0.0/24",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.5/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate the first usable IP from infrastructure subnet",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.4/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.4/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the network IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.0/30",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets should allocate from infrastructure subnet skipping the broadcast IP",
			netConf: &ovncnitypes.NetConf{
				NetConf:               cnitypes.NetConf{Name: "l2-network"},
				Topology:              ovntypes.Layer2Topology,
				Role:                  ovntypes.NetworkRolePrimary,
				Subnets:               "10.0.0.0/24",
				InfrastructureSubnets: "10.0.0.255/32, 10.0.0.9/32",
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.9/24"),
		},
		{
			name: "Layer2 primary UDN with infrastructure subnets IPv6",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2-network"},
				Topology: ovntypes.Layer2Topology,
				Role:     ovntypes.NetworkRolePrimary,

				Subnets:               "2001:db8::/64",
				InfrastructureSubnets: "2001:db8::/126",
			},
			hostSubnet: "2001:db8::/64",
			expectedIP: ovntest.MustParseIPNet("2001:db8::1/64"),
		},
		{
			name: "Localnet topology should return traditional .1 address",
			netConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-network"},
				Topology: ovntypes.LocalnetTopology,
			},
			hostSubnet: "10.0.0.0/24",
			expectedIP: ovntest.MustParseIPNet("10.0.0.1/24"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := NewNetInfo(tc.netConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.IPv4Mode = true
			config.IPv6Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = true

			hostSubnet := ovntest.MustParseIPNet(tc.hostSubnet)

			result := netInfo.GetNodeGatewayIP(hostSubnet)
			if result == nil {
				t.Fatalf("GetNodeGatewayIP returned nil")
			}

			if !result.IP.Equal(tc.expectedIP.IP) {
				t.Errorf("Expected IP %s, got %s", tc.expectedIP.IP.String(), result.IP.String())
			}

			if result.Mask.String() != tc.expectedIP.Mask.String() {
				t.Errorf("Expected mask %s, got %s", tc.expectedIP.Mask.String(), result.Mask.String())
			}
		})
	}
}

func TestGetNetworkVRFName(t *testing.T) {
	tests := []struct {
		desc        string
		networkName string
		networkID   int
		expected    string
	}{
		{
			desc:        "default network",
			networkName: ovntypes.DefaultNetworkName,
			networkID:   ovntypes.DefaultNetworkID,
			expected:    ovntypes.DefaultNetworkName,
		},
		{
			desc:        "CUDN with a name that fits the device name length limit",
			networkName: "cluster_udn_tenant-red",
			networkID:   3,
			expected:    "tenant-red",
		},
		{
			desc:        "CUDN with a 15-character name",
			networkName: "cluster_udn_tenant-12345678",
			networkID:   3,
			expected:    "tenant-12345678",
		},
		{
			desc:        "CUDN with a 16-character name",
			networkName: "cluster_udn_tenant-123456789",
			networkID:   7,
			expected:    "mp7-udn-vrf",
		},
		{
			desc:        "CUDN with a name longer than the device name length limit",
			networkName: "cluster_udn_tenant-1234567890",
			networkID:   7,
			expected:    "mp7-udn-vrf",
		},
		{
			desc:        "CUDN named default",
			networkName: "cluster_udn_default",
			networkID:   4,
			expected:    "mp4-udn-vrf",
		},
		{
			desc:        "namespaced UDN",
			networkName: "ns_tenant-red",
			networkID:   5,
			expected:    "mp5-udn-vrf",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			var netInfo NetInfo = &DefaultNetInfo{}
			if tc.networkName != ovntypes.DefaultNetworkName {
				var err error
				netInfo, err = NewNetInfo(&ovncnitypes.NetConf{
					NetConf:  cnitypes.NetConf{Name: tc.networkName},
					Topology: ovntypes.Layer2Topology,
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
			mutableNetInfo := NewMutableNetInfo(netInfo)
			mutableNetInfo.SetNetworkID(tc.networkID)
			g.Expect(GetNetworkVRFName(mutableNetInfo)).To(gomega.Equal(tc.expected))
		})
	}
}

func TestGetCUDNVRFName(t *testing.T) {
	tests := []struct {
		desc      string
		cudnName  string
		networkID int
		expected  string
	}{
		{
			desc:      "name that fits the device name length limit",
			cudnName:  "tenant-red",
			networkID: 3,
			expected:  "tenant-red",
		},
		{
			desc:      "name that fits the device name length limit, network ID unknown",
			cudnName:  "tenant-red",
			networkID: ovntypes.InvalidID,
			expected:  "tenant-red",
		},
		{
			desc:      "16-character name",
			cudnName:  "tenant-123456789",
			networkID: 7,
			expected:  "mp7-udn-vrf",
		},
		{
			desc:      "name longer than the device name length limit",
			cudnName:  "tenant-1234567890",
			networkID: 7,
			expected:  "mp7-udn-vrf",
		},
		{
			desc:      "name longer than the device name length limit, network ID unknown",
			cudnName:  "tenant-1234567890",
			networkID: ovntypes.InvalidID,
			expected:  "",
		},
		{
			desc:      "name is the default network name",
			cudnName:  ovntypes.DefaultNetworkName,
			networkID: 4,
			expected:  "mp4-udn-vrf",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(GetCUDNVRFName(tc.cudnName, tc.networkID)).To(gomega.Equal(tc.expected))
		})
	}
}

func TestGetUDNVRFName(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(GetUDNVRFName(9)).To(gomega.Equal("mp9-udn-vrf"),
		"the VRF name of a namespaced UDN is always ID-derived")
	g.Expect(GetUDNVRFName(ovntypes.InvalidID)).To(gomega.BeEmpty(),
		"the name can't be derived while the network ID is unknown")
}
