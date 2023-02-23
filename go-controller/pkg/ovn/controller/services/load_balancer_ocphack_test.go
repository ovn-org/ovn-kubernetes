package services

import (
	"fmt"
	"net"
	"testing"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OCP hack begin
func Test_buildPerNodeLBs_OCPHackForDNS(t *testing.T) {
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	oldGwMode := globalconfig.Gateway.Mode
	defer func() {
		globalconfig.Gateway.Mode = oldGwMode
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	name := "dns-default"
	namespace := "openshift-dns"

	defaultService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
		},
	}

	defaultNodes := []nodeInfo{
		{
			name:              "node-a",
			nodeIPs:           []string{"10.0.0.1"},
			gatewayRouterName: "gr-node-a",
			switchName:        "switch-node-a",
			podSubnets:        []net.IPNet{{IP: net.ParseIP("10.128.0.0"), Mask: net.CIDRMask(24, 32)}},
		},
		{
			name:              "node-b",
			nodeIPs:           []string{"10.0.0.2"},
			gatewayRouterName: "gr-node-b",
			switchName:        "switch-node-b",
			podSubnets:        []net.IPNet{{IP: net.ParseIP("10.128.1.0"), Mask: net.CIDRMask(24, 32)}},
		},
	}

	defaultExternalIDs := map[string]string{
		"k8s.ovn.org/kind":  "Service",
		"k8s.ovn.org/owner": fmt.Sprintf("%s/%s", namespace, name),
	}

	//defaultRouters := []string{"gr-node-a", "gr-node-b"}
	//defaultSwitches := []string{"switch-node-a", "switch-node-b"}

	defaultOpts := LBOpts{Reject: true}

	tc := []struct {
		name     string
		service  *v1.Service
		configs  []lbConfig
		expected []LB
	}{
		{
			name:    "clusterIP service, standard pods",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  8080,
					},
				},
			},
			expected: []LB{
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_router_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a", "gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{"192.168.1.1", 80},
							Targets: []Addr{{"10.128.0.2", 8080}, {"10.128.1.2", 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{"192.168.1.1", 80},
							Targets: []Addr{{"10.128.0.2", 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{"192.168.1.1", 80},
							Targets: []Addr{{"10.128.1.2", 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
		},
	}

	for i, tt := range tc {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {

			globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			actual := buildPerNodeLBs(tt.service, tt.configs, defaultNodes)
			assert.Equal(t, tt.expected, actual, "shared gateway mode not as expected")

			globalconfig.Gateway.Mode = globalconfig.GatewayModeLocal
			actual = buildPerNodeLBs(tt.service, tt.configs, defaultNodes)
			assert.Equal(t, tt.expected, actual, "local gateway mode not as expected")
		})
	}
}

// OCP hack end
