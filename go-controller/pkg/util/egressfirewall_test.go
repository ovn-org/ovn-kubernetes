package util

import (
	"net"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type output struct {
	cidrSelector              string
	dnsName                   string
	clusterSubnetIntersection bool
	nodeSelector              *metav1.LabelSelector
}

func TestValidateAndGetEgressFirewallDestination(t *testing.T) {
	clusterSubnetStr := "10.1.0.0/16"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	testcases := []struct {
		name                      string
		egressFirewallDestination egressfirewallapi.EgressFirewallDestination
		dnsNameResolverEnabled    bool
		expectedErr               bool
		expectedOutput            output
	}{
		{
			name: "should correctly validate dns name",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "www.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "www.example.com",
			},
		},
		{
			name: "should throw an error for wildcard dns name when dns name resolver is not enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: false,
			expectedErr:            true,
		},
		{
			name: "should correctly validate wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "*.example.com",
			},
		},
		{
			name: "should throw an error for tld dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for tld wildcard dns name when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "*.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for dns name with more than 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz123456789012.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should validate dns name with 63 characters when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            false,
			expectedOutput: output{
				dnsName: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz12345678901.com",
			},
		},
		{
			name: "should throw an error for a dns name with a label starting with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "-example.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should throw an error for a dns name with a label ending with '-' when dns name resolver is enabled",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				DNSName: "example-.com",
			},
			dnsNameResolverEnabled: true,
			expectedErr:            true,
		},
		{
			name: "should correctly validate cidr selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "1.2.3.5/23",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "1.2.3.5/23",
				clusterSubnetIntersection: false,
			},
		},
		{
			name: "should throw an error for invalid cidr selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "1.2.3.5",
			},
			expectedErr: true,
		},
		{
			name: "should correctly validate cidr selector and cluster subnet intersection",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				CIDRSelector: "10.1.1.1/24",
			},
			expectedErr: false,
			expectedOutput: output{
				cidrSelector:              "10.1.1.1/24",
				clusterSubnetIntersection: true,
			},
		},
		{
			name: "should correctly validate node selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				NodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
			expectedErr: false,
			expectedOutput: output{
				nodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		},
		{
			name: "should correctly validate empty node selector",
			egressFirewallDestination: egressfirewallapi.EgressFirewallDestination{
				NodeSelector: &metav1.LabelSelector{},
			},
			expectedErr: false,
			expectedOutput: output{
				nodeSelector: &metav1.LabelSelector{},
			},
		},
	}

	config.PrepareTestConfig()
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {

			if tc.dnsNameResolverEnabled {
				config.OVNKubernetesFeature.EnableDNSNameResolver = true
			}

			cidrSelector, dnsName, clusterSubnetIntersection, nodeSelector, err :=
				ValidateAndGetEgressFirewallDestination(tc.egressFirewallDestination)
			if tc.expectedErr {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
				assert.Equal(t, tc.expectedOutput.dnsName, dnsName)
				assert.Equal(t, tc.expectedOutput.cidrSelector, cidrSelector)
				assert.Equal(t, tc.expectedOutput.clusterSubnetIntersection, clusterSubnetIntersection)
				assert.Equal(t, tc.expectedOutput.nodeSelector, nodeSelector)
			}
		})
	}
}

func TestIsWildcard(t *testing.T) {
	tests := []struct {
		dnsName        string
		expectedOutput bool
	}{
		// success
		{
			dnsName:        "*.example.com",
			expectedOutput: true,
		},
		{
			dnsName:        "*.sub1.example.com",
			expectedOutput: true,
		},
		// negative
		{
			dnsName:        "www.example.com",
			expectedOutput: false,
		},
		{
			dnsName:        "sub2.sub1.example.com",
			expectedOutput: false,
		},
	}

	for _, tc := range tests {
		actualOutput := IsWildcard(tc.dnsName)
		assert.Equal(t, tc.expectedOutput, actualOutput)
	}
}

func TestGetNames(t *testing.T) {
	tests := []struct {
		name            string
		ef              *egressfirewallapi.EgressFirewall
		expectedDNSName []string
	}{
		{
			name: "DNS names in allow DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
		{
			name: "DNS names in deny DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
		{
			name: "DNS names in allow and deny DNS rules",
			ef: &egressfirewallapi.EgressFirewall{
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.example.com",
							},
						},
						{
							Type: egressfirewallapi.EgressFirewallRuleDeny,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: "www.test.com",
							},
						},
					},
				},
			},
			expectedDNSName: []string{"www.example.com.", "www.test.com."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dnsNames := GetDNSNames(tc.ef)
			dnsNameSet := sets.New(dnsNames...)
			expectedDNSNameSet := sets.New(tc.expectedDNSName...)
			if dnsNameSet.Intersection(expectedDNSNameSet).Len() != dnsNameSet.Len() {
				t.Fatalf("Unexpected DNS names. Expected: %v, Actual: %v", tc.expectedDNSName, dnsNames)
			}
		})
	}
}
