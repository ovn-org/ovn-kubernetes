package services

import (
	"fmt"
	"net"
	"testing"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilpointer "k8s.io/utils/pointer"
)

func Test_buildServiceLBConfigs(t *testing.T) {
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	oldGwMode := globalconfig.Gateway.Mode
	defer func() {
		globalconfig.Gateway.Mode = oldGwMode
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	// constants
	serviceName := "foo"
	ns := "testns"
	portName := "port80"
	portName1 := "port81"
	inport := int32(80)
	outport := int32(8080)
	inport1 := int32(81)
	outport1 := int32(8081)
	outportstr := intstr.FromInt(int(outport))
	emptyEPs := util.LbEndpoints{V4IPs: []string{}, V6IPs: []string{}, Port: 0}
	tcp := v1.ProtocolTCP
	udp := v1.ProtocolUDP

	// make slices
	// nil slice = don't use this family
	// empty slice = family is empty
	makeSlices := func(v4ips, v6ips []string, proto v1.Protocol) []*discovery.EndpointSlice {
		out := []*discovery.EndpointSlice{}
		if v4ips != nil && len(v4ips) == 0 {
			out = append(out, &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			})
		} else if v4ips != nil {
			out = append(out, &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{{
					Protocol: &proto,
					Port:     &outport,
					Name:     &portName,
				}},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.BoolPtr(true),
						},
						Addresses: v4ips,
					},
				},
			})
		}

		if v6ips != nil && len(v6ips) == 0 {
			out = append(out, &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab2",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv6,
				Endpoints:   []discovery.Endpoint{},
			})
		} else if v6ips != nil {
			out = append(out, &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab2",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{{
					Protocol: &proto,
					Port:     &outport,
					Name:     &portName,
				}},
				AddressType: discovery.AddressTypeIPv6,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.BoolPtr(true),
						},
						Addresses: v6ips,
					},
				},
			})
		}

		return out
	}

	type args struct {
		service *v1.Service
		slices  []*discovery.EndpointSlice
	}
	tests := []struct {
		name string
		args args

		resultSharedGatewayCluster []lbConfig
		resultSharedGatewayNode    []lbConfig

		resultLocalGatewayNode    []lbConfig
		resultLocalGatewayCluster []lbConfig

		resultsSame bool //if true, then just use the SharedGateway results for the LGW test
	}{
		{
			name: "v4 clusterip, one port, no endpoints",
			args: args{
				slices: makeSlices([]string{}, nil, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
					},
				},
			},
			resultSharedGatewayCluster: []lbConfig{{
				vips:     []string{"192.168.1.1"},
				protocol: v1.ProtocolTCP,
				inport:   80,
				eps:      emptyEPs,
			}},
			resultsSame: true,
		},
		{
			name: "v4 clusterip, one port, endpoints",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, nil, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
					},
				},
			},
			resultSharedGatewayCluster: []lbConfig{{
				vips:     []string{"192.168.1.1"},
				protocol: v1.ProtocolTCP,
				inport:   inport,
				eps: util.LbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{},
					Port:  outport,
				},
			}},
			resultsSame: true,
		},
		{
			name: "v4 clusterip, two tcp ports, endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceName + "ab1",
							Namespace: ns,
							Labels:    map[string]string{discovery.LabelServiceName: serviceName},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     &portName,
								Protocol: &tcp,
								Port:     &outport,
							}, {
								Name:     &portName1,
								Protocol: &tcp,
								Port:     &outport1,
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.128.0.2", "10.128.1.2"},
							},
						},
					},
				},
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1"},
						Ports: []v1.ServicePort{
							{
								Name:       portName,
								Port:       inport,
								Protocol:   v1.ProtocolTCP,
								TargetPort: outportstr,
							},
							{
								Name:       portName1,
								Port:       inport1,
								Protocol:   v1.ProtocolTCP,
								TargetPort: intstr.FromInt(int(outport1)),
							},
						},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						V6IPs: []string{},
						Port:  outport,
					},
				},
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport1,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						V6IPs: []string{},
						Port:  outport1,
					},
				},
			},
		},
		{
			name: "v4 clusterip, one tcp, one udp port, endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceName + "ab1",
							Namespace: ns,
							Labels:    map[string]string{discovery.LabelServiceName: serviceName},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     &portName,
								Protocol: &tcp,
								Port:     &outport,
							}, {
								Name:     &portName1,
								Protocol: &udp,
								Port:     &outport,
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.128.0.2", "10.128.1.2"},
							},
						},
					},
				},
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1"},
						Ports: []v1.ServicePort{
							{
								Name:       portName,
								Port:       inport,
								Protocol:   v1.ProtocolTCP,
								TargetPort: outportstr,
							},
							{
								Name:       portName1,
								Port:       inport,
								Protocol:   v1.ProtocolUDP,
								TargetPort: intstr.FromInt(int(outport)),
							},
						},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						V6IPs: []string{},
						Port:  outport,
					},
				},
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolUDP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						V6IPs: []string{},
						Port:  outport,
					},
				},
			},
		},
		{
			name: "dual-stack clusterip, one port, endpoints",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, []string{"fe00::1:1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{{
				vips:     []string{"192.168.1.1", "2002::1"},
				protocol: v1.ProtocolTCP,
				inport:   inport,
				eps: util.LbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
			}},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, external ips + lb status",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, []string{"fe00::1:1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
						ExternalIPs: []string{"4.2.2.2", "42::42"},
					},
					Status: v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{{
				vips:     []string{"192.168.1.1", "2002::1", "4.2.2.2", "42::42", "5.5.5.5"},
				protocol: v1.ProtocolTCP,
				inport:   inport,
				eps: util.LbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
			}},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, external ips + lb status, ExternalTrafficPolicy",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, []string{"fe00::1:1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1", "2002::1"},
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
						ExternalIPs: []string{"4.2.2.2", "42::42"},
					},
					Status: v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						V6IPs: []string{"fe00::1:1"},
						Port:  outport,
					},
				},
			},
			resultSharedGatewayNode: []lbConfig{
				{
					vips:                 []string{"4.2.2.2", "42::42", "5.5.5.5"},
					protocol:             v1.ProtocolTCP,
					inport:               inport,
					externalTrafficLocal: true,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						V6IPs: []string{"fe00::1:1"},
						Port:  outport,
					},
				},
			},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, nodePort",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, []string{"fe00::1:1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
					},
				},
			},
			resultsSame: true,
			resultSharedGatewayCluster: []lbConfig{{
				vips:     []string{"192.168.1.1", "2002::1"},
				protocol: v1.ProtocolTCP,
				inport:   inport,
				eps: util.LbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
			}},
			resultSharedGatewayNode: []lbConfig{{
				vips:     []string{"node"},
				protocol: v1.ProtocolTCP,
				inport:   5,
				eps: util.LbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
				hasNodePort: true,
			}},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, nodePort, hostNetwork",
			args: args{
				// These slices are outside of the config, and thus are host network
				slices: makeSlices([]string{"192.168.0.1"}, []string{"2001::1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
					},
				},
			},
			// In shared and local gateway modes, nodeport and host-network-pods must be per-node
			resultSharedGatewayNode: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					hasNodePort: true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
			// in local gateway mode, only nodePort is per-node
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					hasNodePort: true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, nodePort, hostNetwork, ExternalTrafficPolicy=Local",
			args: args{
				// These slices are outside of the config, and thus are host network
				slices: makeSlices([]string{"192.168.0.1"}, []string{"2001::1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeNodePort,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1", "2002::1"},
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
					},
				},
			},
			// In shared & local gateway modes, nodeport and host-network-pods must be per-node
			resultSharedGatewayNode: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					externalTrafficLocal: true,
					hasNodePort:          true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					externalTrafficLocal: true,
					hasNodePort:          true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, hostNetwork",
			args: args{
				// These slices are outside of the config, and thus are host network
				slices: makeSlices([]string{"192.168.0.1"}, []string{"2001::1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
					},
				},
			},
			// In shared gateway mode, nodeport and host-network-pods must be per-node
			resultSharedGatewayNode: []lbConfig{
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			perNode, clusterWide := buildServiceLBConfigs(tt.args.service, tt.args.slices)
			assert.EqualValues(t, tt.resultSharedGatewayNode, perNode, "SGW per-node configs should be equal")
			assert.EqualValues(t, tt.resultSharedGatewayCluster, clusterWide, "SGW cluster-wide configs should be equal")

			globalconfig.Gateway.Mode = globalconfig.GatewayModeLocal
			perNode, clusterWide = buildServiceLBConfigs(tt.args.service, tt.args.slices)
			if tt.resultsSame {
				assert.EqualValues(t, tt.resultSharedGatewayNode, perNode, "LGW per-node configs should be equal")
				assert.EqualValues(t, tt.resultSharedGatewayCluster, clusterWide, "LGW cluster-wide configs should be equal")
			} else {
				assert.EqualValues(t, tt.resultLocalGatewayNode, perNode, "LGW per-node configs should be equal")
				assert.EqualValues(t, tt.resultLocalGatewayCluster, clusterWide, "LGW cluster-wide configs should be equal")
			}
		})
	}
}

func Test_buildClusterLBs(t *testing.T) {
	name := "foo"
	namespace := "testns"

	oldGwMode := globalconfig.Gateway.Mode
	defer func() {
		globalconfig.Gateway.Mode = oldGwMode
	}()
	globalconfig.Gateway.Mode = globalconfig.GatewayModeShared

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
		},
		{
			name:              "node-b",
			nodeIPs:           []string{"10.0.0.2"},
			gatewayRouterName: "gr-node-b",
			switchName:        "switch-node-b",
		},
	}

	defaultExternalIDs := map[string]string{
		"k8s.ovn.org/kind":  "Service",
		"k8s.ovn.org/owner": fmt.Sprintf("%s/%s", namespace, name),
	}

	defaultRouters := []string{}
	defaultSwitches := []string{}
	defaultGroups := []string{"clusterLBGroup"}

	tc := []struct {
		name      string
		service   *v1.Service
		configs   []lbConfig
		nodeInfos []nodeInfo
		expected  []ovnlb.LB
	}{
		{
			name:    "two tcp services, single stack",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						Port:  8080,
					},
				},
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   443,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						Port:  8043,
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []ovnlb.LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 80},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8080}, {"192.168.0.2", 8080}},
						},
						{
							Source:  ovnlb.Addr{"1.2.3.4", 443},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8043}},
						},
					},

					Routers:  defaultRouters,
					Switches: defaultSwitches,
					Groups:   defaultGroups,
				},
			},
		},
		{
			name:    "tcp / udp services, single stack",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						Port:  8080,
					},
				},
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolUDP,
					inport:   443,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						Port:  8043,
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []ovnlb.LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 80},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8080}, {"192.168.0.2", 8080}},
						},
					},

					Switches: defaultSwitches,
					Routers:  defaultRouters,
					Groups:   defaultGroups,
				},
				{
					Name:        fmt.Sprintf("Service_%s/%s_UDP_cluster", namespace, name),
					Protocol:    "UDP",
					ExternalIDs: defaultExternalIDs,
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 443},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8043}},
						},
					},

					Switches: defaultSwitches,
					Routers:  defaultRouters,
					Groups:   defaultGroups,
				},
			},
		},
		{
			name:    "dual stack",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4", "fe80::1"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						V6IPs: []string{"fe90::1", "fe91::1"},
						Port:  8080,
					},
				},
				{
					vips:     []string{"1.2.3.4", "fe80::1"},
					protocol: v1.ProtocolTCP,
					inport:   443,
					eps: util.LbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"fe90::1"},
						Port:  8043,
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []ovnlb.LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 80},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8080}, {"192.168.0.2", 8080}},
						},
						{
							Source:  ovnlb.Addr{"fe80::1", 80},
							Targets: []ovnlb.Addr{{"fe90::1", 8080}, {"fe91::1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"1.2.3.4", 443},
							Targets: []ovnlb.Addr{{"192.168.0.1", 8043}},
						},
						{
							Source:  ovnlb.Addr{"fe80::1", 443},
							Targets: []ovnlb.Addr{{"fe90::1", 8043}},
						},
					},

					Routers:  defaultRouters,
					Switches: defaultSwitches,
					Groups:   defaultGroups,
				},
			},
		},
	}

	for i, tt := range tc {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			actual := buildClusterLBs(tt.service, tt.configs, tt.nodeInfos, true)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func Test_buildPerNodeLBs(t *testing.T) {
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	oldGwMode := globalconfig.Gateway.Mode
	defer func() {
		globalconfig.Gateway.Mode = oldGwMode
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	name := "foo"
	namespace := "testns"

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

	tc := []struct {
		name           string
		service        *v1.Service
		configs        []lbConfig
		expectedShared []ovnlb.LB
		expectedLocal  []ovnlb.LB
	}{
		{
			name:    "host-network pod",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
				},
			},
			expectedShared: []ovnlb.LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-a", "switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"1.2.3.4", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
			},
		},
		{
			name:    "nodeport service, standard pod",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  8080,
					},
				},
			},
			expectedShared: []ovnlb.LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}},
						},
					},
				},
			},
			expectedLocal: []ovnlb.LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}},
						},
					},
				},
			},
		},
		{
			name:    "nodeport service, host-network pod",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"192.168.0.1"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
				},
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
				},
			},
			expectedShared: []ovnlb.LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
			},
			expectedLocal: []ovnlb.LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
			},
		},
		{
			// The most complicated case
			name:    "nodeport service, host-network pod, ExternalTrafficPolicy",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"192.168.0.1"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
				},
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               80,
					externalTrafficLocal: true,
					hasNodePort:          true,
					eps: util.LbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
				},
			},
			expectedShared: []ovnlb.LB{
				// node-a has endpoints: 3 load balancers
				// router clusterip
				// router nodeport
				// switch clusterip + nodeport
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Opts:        ovnlb.LBOpts{SkipSNAT: true},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"169.254.169.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"169.254.169.3", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},

				// node-b has no service, 3 lbs
				// router clusterip
				// router nodeport = empty
				// switch clusterip + nodeport
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.0.1", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
						{
							Source:  ovnlb.Addr{"169.254.169.3", 80},
							Targets: []ovnlb.Addr{},
						},
						{
							Source:  ovnlb.Addr{"10.0.0.2", 80},
							Targets: []ovnlb.Addr{{"10.0.0.1", 8080}},
						},
					},
				},
			},
		},
	}

	for i, tt := range tc {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {

			if tt.expectedShared != nil {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
				actual := buildPerNodeLBs(tt.service, tt.configs, defaultNodes)
				assert.Equal(t, tt.expectedShared, actual, "shared gateway mode not as expected")
			}

			if tt.expectedLocal != nil {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeLocal
				actual := buildPerNodeLBs(tt.service, tt.configs, defaultNodes)
				assert.Equal(t, tt.expectedLocal, actual, "local gateway mode not as expected")
			}

		})
	}
}

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

	tc := []struct {
		name     string
		service  *v1.Service
		configs  []lbConfig
		expected []ovnlb.LB
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
			expected: []ovnlb.LB{
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_router_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a", "gr-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.1.1", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}, {"10.128.1.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.1.1", 80},
							Targets: []ovnlb.Addr{{"10.128.0.2", 8080}},
						},
					},
				},
				{
					Name:        "Service_openshift-dns/dns-default_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []ovnlb.LBRule{
						{
							Source:  ovnlb.Addr{"192.168.1.1", 80},
							Targets: []ovnlb.Addr{{"10.128.1.2", 8080}},
						},
					},
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
