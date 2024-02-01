package services

import (
	"fmt"
	"net"
	"testing"
	"time"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	kube_test "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

var (
	nodeA        = "node-a"
	nodeB        = "node-b"
	defaultNodes = []nodeInfo{
		{
			name:               nodeA,
			l3gatewayAddresses: []net.IP{net.ParseIP("10.0.0.1")},
			hostAddresses:      []net.IP{net.ParseIP("10.0.0.1")},
			gatewayRouterName:  "gr-node-a",
			switchName:         "switch-node-a",
		},
		{
			name:               nodeB,
			l3gatewayAddresses: []net.IP{net.ParseIP("10.0.0.2")},
			hostAddresses:      []net.IP{net.ParseIP("10.0.0.2")},
			gatewayRouterName:  "gr-node-b",
			switchName:         "switch-node-b",
		},
	}

	tcpv1 = v1.ProtocolTCP
	udpv1 = v1.ProtocolUDP

	httpPortName    string = "http"
	httpPortValue   int32  = int32(80)
	httpsPortName   string = "https"
	httpsPortValue  int32  = int32(443)
	customPortName  string = "customApp"
	customPortValue int32  = int32(10600)
)

func getSampleService(publishNotReadyAddresses bool) *v1.Service {
	name := "service-test"
	namespace := "test"
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			UID:       k8stypes.UID(namespace),
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			PublishNotReadyAddresses: publishNotReadyAddresses,
		},
	}
}

func getServicePort(name string, targetPort int32, protocol v1.Protocol) v1.ServicePort {
	return v1.ServicePort{
		Name:       name,
		TargetPort: intstr.FromInt(int(httpPortValue)),
		Protocol:   protocol,
	}
}

func getSampleServiceWithOnePort(name string, targetPort int32, protocol v1.Protocol) *v1.Service {
	service := getSampleService(false)
	service.Spec.Ports = []v1.ServicePort{getServicePort(name, targetPort, protocol)}
	return service
}

func getSampleServiceWithOnePortAndETPLocal(name string, targetPort int32, protocol v1.Protocol) *v1.Service {
	service := getSampleServiceWithOnePort(name, targetPort, protocol)
	service.Spec.Type = v1.ServiceTypeLoadBalancer
	service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	return service
}

func getSampleServiceWithTwoPorts(name1, name2 string, targetPort1, targetPort2 int32, protocol1, protocol2 v1.Protocol) *v1.Service {
	service := getSampleService(false)
	service.Spec.Ports = []v1.ServicePort{
		getServicePort(name1, targetPort1, protocol1),
		getServicePort(name2, targetPort2, protocol2)}
	return service
}

func getSampleServiceWithTwoPortsAndETPLocal(name1, name2 string, targetPort1, targetPort2 int32, protocol1, protocol2 v1.Protocol) *v1.Service {
	service := getSampleServiceWithTwoPorts(name1, name2, targetPort1, targetPort2, protocol1, protocol2)
	service.Spec.Type = v1.ServiceTypeLoadBalancer
	service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	return service
}

func getSampleServiceWithOnePortAndPublishNotReadyAddresses(name string, targetPort int32, protocol v1.Protocol) *v1.Service {
	service := getSampleServiceWithOnePort(name, targetPort, protocol)
	service.Spec.PublishNotReadyAddresses = true
	return service
}

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
				Endpoints:   kube_test.MakeReadyEndpointList(nodeA, v4ips...),
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
				Endpoints:   kube_test.MakeReadyEndpointList(nodeA, v6ips...),
			})
		}

		return out
	}

	makeV4SliceWithEndpoints := func(proto v1.Protocol, endpoints ...discovery.Endpoint) []*discovery.EndpointSlice {
		e := &discovery.EndpointSlice{
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
			Endpoints:   endpoints,
		}
		return []*discovery.EndpointSlice{e}
	}

	type args struct {
		service *v1.Service
		slices  []*discovery.EndpointSlice
	}
	tests := []struct {
		name string
		args args

		resultSharedGatewayCluster  []lbConfig
		resultSharedGatewayTemplate []lbConfig
		resultSharedGatewayNode     []lbConfig

		resultLocalGatewayNode     []lbConfig
		resultLocalGatewayTemplate []lbConfig
		resultLocalGatewayCluster  []lbConfig

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
							Name:       portName,
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
						}},
					},
				},
			},
			resultSharedGatewayCluster: []lbConfig{{
				vips:             []string{"192.168.1.1"},
				protocol:         v1.ProtocolTCP,
				inport:           80,
				clusterEndpoints: lbEndpoints{},
				nodeEndpoints:    map[string]lbEndpoints{},
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
							Name:       portName,
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
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{}, // service is not ETP=local or ITP=local, so nodeEndpoints is not filled out
			}},
			resultsSame: true,
		},
		{
			name: "v4 type=LoadBalancer, ETP=local, one port, endpoints",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, nil, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1"},
						Ports: []v1.ServicePort{{
							Name:       portName,
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
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{ // service is ETP=local, so nodeEndpoints is filled out
					nodeA: {
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
				},
			}},
			resultsSame: true,
		},
		{
			name: "v4 clusterip, two tcp ports, two endpoints",
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
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.128.0.2", "10.128.1.2"),
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport1,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport1,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
			},
		},
		{
			name: "v4 clusterip, one tcp, one udp port, two endpoints",
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
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.128.0.2", "10.128.1.2"),
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
				{
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolUDP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
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
							Name:       portName,
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
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{},
			}},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, external ips + lb status",
			args: args{
				slices: makeSlices([]string{"10.128.0.2"}, []string{"fe00::1:1"}, v1.ProtocolTCP),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeLoadBalancer,
						ClusterIP:  "192.168.1.1",
						ClusterIPs: []string{"192.168.1.1", "2002::1"},
						Ports: []v1.ServicePort{{
							Name:       portName,
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
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{}, // ETP=cluster (default), so nodeEndpoints is not filled out
			}},
		},
		{
			name: "dual-stack clusterip, one port, endpoints, external ips + lb status, ExternalTrafficPolicy=local",
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
							Name:       portName,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						V6IPs: []string{"fe00::1:1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							V6IPs: []string{"fe00::1:1"},
							Port:  outport,
						},
					},
				},
			},
			resultSharedGatewayNode: []lbConfig{
				{
					vips:                 []string{"4.2.2.2", "42::42", "5.5.5.5"},
					protocol:             v1.ProtocolTCP,
					inport:               inport,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						V6IPs: []string{"fe00::1:1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							V6IPs: []string{"fe00::1:1"},
							Port:  outport,
						},
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
							Name:       portName,
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
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{},
			}},
			resultSharedGatewayTemplate: []lbConfig{{
				vips:     []string{"node"},
				protocol: v1.ProtocolTCP,
				inport:   5,
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"10.128.0.2"},
					V6IPs: []string{"fe00::1:1"},
					Port:  outport,
				},
				nodeEndpoints: map[string]lbEndpoints{},
				hasNodePort:   true,
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
							Name:       portName,
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
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
			},
			resultSharedGatewayTemplate: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
					hasNodePort:   true,
				},
			},
			// in local gateway mode, only nodePort is per-node
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
			},
			resultLocalGatewayTemplate: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
					hasNodePort:   true,
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
							Name:       portName,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							V6IPs: []string{"2001::1"},
							Port:  outport,
						},
					},
					externalTrafficLocal: true,
					hasNodePort:          true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							V6IPs: []string{"2001::1"},
							Port:  outport,
						},
					},
				},
			},
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   5,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							V6IPs: []string{"2001::1"},
							Port:  outport,
						},
					},
					externalTrafficLocal: true,
					hasNodePort:          true,
				},
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							V6IPs: []string{"2001::1"},
							Port:  outport,
						},
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
							Name:       portName,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
			},
			resultLocalGatewayNode: []lbConfig{
				{
					vips:     []string{"192.168.1.1", "2002::1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"2001::1"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{},
				},
			},
		},
		{
			name: "LB service with NodePort, one port, two endpoints, external ips + lb status, ExternalTrafficPolicy=local",
			args: args{
				slices: makeV4SliceWithEndpoints(
					v1.ProtocolTCP,
					kube_test.MakeReadyEndpoint(nodeA, "10.128.0.2"),
					kube_test.MakeReadyEndpoint(nodeB, "10.128.1.2"),
				),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1"},
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Ports: []v1.ServicePort{{
							Name:       portName,
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
						ExternalIPs: []string{"4.2.2.2"},
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
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"},
							Port:  outport,
						},
					},
				},
			},
			resultSharedGatewayNode: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5,
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"},
							Port:  outport,
						},
					},
				},
				{
					vips:                 []string{"4.2.2.2", "5.5.5.5"},
					protocol:             v1.ProtocolTCP,
					inport:               inport,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"},
							Port:  outport,
						},
					},
				},
			},
		},
		{
			// The fallback to terminating&serving only if there are no ready endpoints
			// is not done at this stage: we just include candidate endpoints, that is  ready + terminating&serving.
			// The test below will just show both endpoints in its output.
			name: "LB service with NodePort, port, two endpoints, external ips + lb status, ExternalTrafficPolicy=local, one endpoint is ready, the other one is terminating and serving",
			args: args{
				slices: makeV4SliceWithEndpoints(v1.ProtocolTCP,
					kube_test.MakeReadyEndpoint(nodeA, "10.128.0.2"),
					kube_test.MakeTerminatingServingEndpoint(nodeB, "10.128.1.2")),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1"},
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Ports: []v1.ServicePort{{
							Name:       portName,
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
						ExternalIPs: []string{"4.2.2.2"},
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
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"}, // fallback to terminating & serving on nodeB
							Port:  outport,
						},
					},
				},
			},
			resultSharedGatewayNode: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5,
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"}, // fallback to terminating & serving on nodeB
							Port:  outport,
						},
					},
				},
				{
					vips:                 []string{"4.2.2.2", "5.5.5.5"},
					protocol:             v1.ProtocolTCP,
					inport:               inport,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"}, // fallback to terminating & serving on nodeB
							Port:  outport,
						},
					},
				},
			},
		},
		{
			// Terminating & non-serving endpoints are filtered out by buildServiceLBConfigs
			name: "LB service with NodePort, one port, two endpoints, external ips + lb status, ExternalTrafficPolicy=local, both endpoints terminating: one is serving, the other one is not",
			args: args{
				slices: makeV4SliceWithEndpoints(v1.ProtocolTCP,
					kube_test.MakeTerminatingServingEndpoint(nodeA, "10.128.0.2"),
					kube_test.MakeTerminatingNonServingEndpoint(nodeB, "10.128.1.2")),
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ClusterIP:             "192.168.1.1",
						ClusterIPs:            []string{"192.168.1.1"},
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Ports: []v1.ServicePort{{
							Name:       portName,
							Port:       inport,
							Protocol:   v1.ProtocolTCP,
							TargetPort: outportstr,
							NodePort:   5,
						}},
						ExternalIPs: []string{"4.2.2.2"},
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
					vips:     []string{"192.168.1.1"},
					protocol: v1.ProtocolTCP,
					inport:   inport,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
					},
				},
			},
			resultSharedGatewayNode: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5,
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
					},
				},
				{
					vips:                 []string{"4.2.2.2", "5.5.5.5"},
					protocol:             v1.ProtocolTCP,
					inport:               inport,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  outport,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  outport,
						},
					},
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			// shared gateway mode
			globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			perNode, template, clusterWide := buildServiceLBConfigs(tt.args.service, tt.args.slices, defaultNodes, true, true)

			assert.EqualValues(t, tt.resultSharedGatewayNode, perNode, "SGW per-node configs should be equal")
			assert.EqualValues(t, tt.resultSharedGatewayTemplate, template, "SGW template configs should be equal")
			assert.EqualValues(t, tt.resultSharedGatewayCluster, clusterWide, "SGW cluster-wide configs should be equal")

			// local gateway mode
			globalconfig.Gateway.Mode = globalconfig.GatewayModeLocal
			perNode, template, clusterWide = buildServiceLBConfigs(tt.args.service, tt.args.slices, defaultNodes, true, true)
			if tt.resultsSame {
				assert.EqualValues(t, tt.resultSharedGatewayNode, perNode, "LGW per-node configs should be equal")
				assert.EqualValues(t, tt.resultSharedGatewayTemplate, template, "LGW template configs should be equal")
				assert.EqualValues(t, tt.resultSharedGatewayCluster, clusterWide, "LGW cluster-wide configs should be equal")
			} else {
				assert.EqualValues(t, tt.resultLocalGatewayNode, perNode, "LGW per-node configs should be equal")
				assert.EqualValues(t, tt.resultLocalGatewayTemplate, template, "LGW template configs should be equal")
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

	defaultExternalIDs := map[string]string{
		types.LoadBalancerKindExternalID:  "Service",
		types.LoadBalancerOwnerExternalID: fmt.Sprintf("%s/%s", namespace, name),
	}

	defaultRouters := []string{}
	defaultSwitches := []string{}
	defaultGroups := []string{"clusterLBGroup"}
	defaultOpts := LBOpts{Reject: true}

	tc := []struct {
		name      string
		service   *v1.Service
		configs   []lbConfig
		nodeInfos []nodeInfo
		expected  []LB
	}{
		{
			name:    "two tcp services, single stack",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1", "192.168.0.2"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   443,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						Port:  8043,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							Port:  8043,
						},
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8080}, {IP: "192.168.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 443},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8043}},
						},
					},

					Routers:  defaultRouters,
					Switches: defaultSwitches,
					Groups:   defaultGroups,
					Opts:     defaultOpts,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1", "192.168.0.2"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolUDP,
					inport:   443,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						Port:  8043,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							Port:  8043,
						},
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8080}, {IP: "192.168.0.2", Port: 8080}},
						},
					},

					Switches: defaultSwitches,
					Routers:  defaultRouters,
					Groups:   defaultGroups,
					Opts:     defaultOpts,
				},
				{
					Name:        fmt.Sprintf("Service_%s/%s_UDP_cluster", namespace, name),
					Protocol:    "UDP",
					ExternalIDs: defaultExternalIDs,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 443},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8043}},
						},
					},

					Switches: defaultSwitches,
					Routers:  defaultRouters,
					Groups:   defaultGroups,
					Opts:     defaultOpts,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1", "192.168.0.2"},
						V6IPs: []string{"fe90::1", "fe91::1"},

						Port: 8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1", "192.168.0.2"},
							V6IPs: []string{"fe90::1", "fe91::1"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"1.2.3.4", "fe80::1"},
					protocol: v1.ProtocolTCP,
					inport:   443,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"fe90::1"},

						Port: 8043,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"192.168.0.1"},
							V6IPs: []string{"fe90::1"},
							Port:  8043,
						},
					},
				},
			},
			nodeInfos: defaultNodes,
			expected: []LB{
				{
					Name:        fmt.Sprintf("Service_%s/%s_TCP_cluster", namespace, name),
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8080}, {IP: "192.168.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fe80::1", Port: 80},
							Targets: []Addr{{IP: "fe90::1", Port: 8080}, {IP: "fe91::1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 443},
							Targets: []Addr{{IP: "192.168.0.1", Port: 8043}},
						},
						{
							Source:  Addr{IP: "fe80::1", Port: 443},
							Targets: []Addr{{IP: "fe90::1", Port: 8043}},
						},
					},

					Routers:  defaultRouters,
					Switches: defaultSwitches,
					Groups:   defaultGroups,
					Opts:     defaultOpts,
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
	oldServiceCIDRs := globalconfig.Kubernetes.ServiceCIDRs
	defer func() {
		globalconfig.Gateway.Mode = oldGwMode
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
		globalconfig.Kubernetes.ServiceCIDRs = oldServiceCIDRs
	}()

	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}
	_, svcCIDRv4, _ := net.ParseCIDR("192.168.0.0/24")
	_, svcCIDRv6, _ := net.ParseCIDR("fd92::0/80")

	globalconfig.Kubernetes.ServiceCIDRs = []*net.IPNet{svcCIDRv4}

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
			name:               nodeA,
			l3gatewayAddresses: []net.IP{net.ParseIP("10.0.0.1")},
			hostAddresses:      []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.111")},
			gatewayRouterName:  "gr-node-a",
			switchName:         "switch-node-a",
			podSubnets:         []net.IPNet{{IP: net.ParseIP("10.128.0.0"), Mask: net.CIDRMask(24, 32)}},
		},
		{
			name:               nodeB,
			l3gatewayAddresses: []net.IP{net.ParseIP("10.0.0.2")},
			hostAddresses:      []net.IP{net.ParseIP("10.0.0.2")},
			gatewayRouterName:  "gr-node-b",
			switchName:         "switch-node-b",
			podSubnets:         []net.IPNet{{IP: net.ParseIP("10.128.1.0"), Mask: net.CIDRMask(24, 32)}},
		},
	}

	defaultNodesV6 := []nodeInfo{
		{
			name:               nodeA,
			l3gatewayAddresses: []net.IP{net.ParseIP("fd00::1")},
			hostAddresses:      []net.IP{net.ParseIP("fd00::1"), net.ParseIP("fd00::111")},
			gatewayRouterName:  "gr-node-a",
			switchName:         "switch-node-a",
			podSubnets:         []net.IPNet{{IP: net.ParseIP("fe00:0:0:0:1::0"), Mask: net.CIDRMask(64, 64)}},
		},
		{
			name:               nodeB,
			l3gatewayAddresses: []net.IP{net.ParseIP("fd00::2")},
			hostAddresses:      []net.IP{net.ParseIP("fd00::2")},
			gatewayRouterName:  "gr-node-b",
			switchName:         "switch-node-b",
			podSubnets:         []net.IPNet{{IP: net.ParseIP("fe00:0:0:0:2::0"), Mask: net.CIDRMask(64, 64)}},
		},
	}

	defaultExternalIDs := map[string]string{
		types.LoadBalancerKindExternalID:  "Service",
		types.LoadBalancerOwnerExternalID: fmt.Sprintf("%s/%s", namespace, name),
	}
	defaultOpts := LBOpts{Reject: true}

	//defaultRouters := []string{"gr-node-a", "gr-node-b"}
	//defaultSwitches := []string{"switch-node-a", "switch-node-b"}

	tc := []struct {
		name           string
		service        *v1.Service
		configs        []lbConfig
		expectedShared []LB
		expectedLocal  []LB
	}{
		{
			name:    "host-network pod",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"1.2.3.4"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-a", "switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V4IPs: []string{"10.128.0.2"}, Port: 8080},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
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
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		{
			// The most complicated case
			name:    "nodeport service, host-network pod, ExternalTrafficPolicy=local",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"192.168.0.1"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
					},
				},
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               80,
					externalTrafficLocal: true,
					hasNodePort:          true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				// node-a has endpoints: 3 load balancers
				// router clusterip
				// router nodeport
				// switch clusterip + nodeport
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},

				// node-b has no endpoint, 3 lbs
				// router clusterip
				// router nodeport = empty
				// switch clusterip + nodeport
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 80},
							Targets: []Addr{},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		{
			name:    "clusterIP + externalIP service, standard pods, InternalTrafficPolicy=local",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"192.168.0.1"}, // clusterIP config
					protocol:             v1.ProtocolTCP,
					inport:               80,
					internalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.1", "10.128.1.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.1"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.1"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"1.2.3.4"}, // externalIP config
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.1", "10.128.1.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.1"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.1"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a", "gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}}, // filters out the ep present only on node-a
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.1.1", Port: 8080}}, // filters out the ep present only on node-b
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a_merged",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a", "gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}}, // filters out the ep present only on node-a
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.128.1.1", Port: 8080}}, // filters out the ep present only on node-b
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.128.0.1", Port: 8080}, {IP: "10.128.1.1", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		{
			name:    "clusterIP + externalIP service, host-networked pods, InternalTrafficPolicy=local",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"192.168.0.1"}, // clusterIP config
					protocol:             v1.ProtocolTCP,
					inport:               80,
					internalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1", "10.0.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.0.0.2"},
							Port:  8080,
						},
					},
				},
				{
					vips:     []string{"1.2.3.4"}, // externalIP config
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1", "10.0.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.0.0.2"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}, {IP: "10.0.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // filters out the ep present only on node-a
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "169.254.169.2", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.2", Port: 8080}}, // filters out the ep present only on node-b
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}, {IP: "10.0.0.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // filters out the ep present only on node-a
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "169.254.169.2", Port: 8080}}, // no filtering on GR LBs for ITP=local
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "169.254.169.2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.2", Port: 8080}}, // filters out the ep present only on node-b
						},
						{
							Source:  Addr{IP: "1.2.3.4", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}, // ITP is only applicable for clusterIPs
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		{
			// Another complicated case
			name:    "clusterIP + nodeport service, host-network pod, ExternalTrafficPolicy=local, InternalTrafficPolicy=local",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"192.168.0.1"}, // clusterIP config
					protocol:             v1.ProtocolTCP,
					inport:               80,
					internalTrafficLocal: true,
					externalTrafficLocal: false, // ETP is applicable only to nodePorts and LBs
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"}, // only one ep on node-a
							Port:  8080,
						},
					},
				},
				{
					vips:                 []string{"node"}, // nodePort config
					protocol:             v1.ProtocolTCP,
					inport:               34345,
					externalTrafficLocal: true,
					internalTrafficLocal: false, // ITP is applicable only to clusterIPs
					hasNodePort:          true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.0.0.1"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.0.0.1"}, // only one ep on node-a
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}}, // we don't filter clusterIPs at GR for ETP/ITP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 34345},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}}, // special skip_snat=true LB for ETP=local; used in SGW mode
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 34345},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // filter out eps only on node-a for clusterIP
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345}, // add special masqueradeIP VIP for nodePort/LB traffic coming from node via mp0 when ETP=local
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},   // filter out eps only on node-a for nodePorts
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // don't filter out eps for nodePorts on switches when ETP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // we don't filter clusterIPs at GR for ETP/ITP=local
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 34345},
							Targets: []Addr{}, // filter out eps only on node-b for nodePort on GR when ETP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{}, // filter out eps only on node-b for clusterIP
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345}, // add special masqueradeIP VIP for nodePort/LB traffic coming from node via mp0 when ETP=local
							Targets: []Addr{},                               // filter out eps only on node-b for nodePorts
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // don't filter out eps for nodePorts on switches when ETP=local
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}}, // we don't filter clusterIPs at GR for ETP/ITP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 34345},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}}, // special skip_snat=true LB for ETP=local; used in SGW mode
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 34345},
							Targets: []Addr{{IP: "169.254.169.2", Port: 8080}},
						},
					},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // filter out eps only on node-a for clusterIP
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345}, // add special masqueradeIP VIP for nodePort/LB traffic coming from node via mp0 when ETP=local
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},   // filter out eps only on node-a for nodePorts
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // don't filter out eps for nodePorts on switches when ETP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // we don't filter clusterIPs at GR for ETP/ITP=local
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 34345},
							Targets: []Addr{}, // filter out eps only on node-b for nodePort on GR when ETP=local
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "192.168.0.1", Port: 80},
							Targets: []Addr{}, // filter out eps only on node-b for clusterIP
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 34345}, // add special masqueradeIP VIP for nodePort/LB traffic coming from node via mp0 when ETP=local
							Targets: []Addr{},                               // filter out eps only on node-b for nodePorts
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 34345},
							Targets: []Addr{{IP: "10.0.0.1", Port: 8080}}, // don't filter out eps for nodePorts on switches when ETP=local
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		// tests for endpoint selection with ExternalTrafficPolicy=local
		{
			name:    "LB service with NodePort, standard pods on different nodes, ExternalTrafficPolicy=local, both endpoints are ready",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5, // nodePort
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"},
							Port:  8080,
						},
					},
				},
				{
					vips:                 []string{"4.2.2.2", "5.5.5.5"}, // externalIP + LB IP
					protocol:             v1.ProtocolTCP,
					inport:               80,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2", "10.128.1.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {
							V4IPs: []string{"10.128.0.2"},
							Port:  8080,
						},
						nodeB: {
							V4IPs: []string{"10.128.1.2"},
							Port:  8080,
						},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},

					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint (ready)
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint (ready)
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint (ready)
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint (ready)
						},
					},
					Routers: []string{"gr-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
					},
					Switches: []string{"switch-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.2", Port: 5},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (ready)
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (ready)
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (ready)
						},
					},
					Routers: []string{"gr-node-b"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}, {IP: "10.128.1.2", Port: 8080}},
						},
					},
					Switches: []string{"switch-node-b"},
				},
			},
		},
		{
			name:    "LB service with NodePort, standard pods on different nodes, ExternalTrafficPolicy=local, one endpoint is ready, the other one is terminating and serving",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5, // nodePort
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V4IPs: []string{"10.128.0.2"}, Port: 8080},
						nodeB: {V4IPs: []string{"10.128.1.2"}, Port: 8080},
					},
				},
				{
					vips:                 []string{"4.2.2.2", "5.5.5.5"}, // externalIP + LB IP
					protocol:             v1.ProtocolTCP,
					inport:               80,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V4IPs: []string{"10.128.0.2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V4IPs: []string{"10.128.0.2"}, Port: 8080},
						nodeB: {V4IPs: []string{"10.128.1.2"}, Port: 8080},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},

					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.1", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // local endpoint
						},
					},
					Routers: []string{"gr-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.1", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.111", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
					},
					Switches: []string{"switch-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Rules: []LBRule{
						{
							Source:  Addr{IP: "10.0.0.2", Port: 5},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
					},
					Routers: []string{"gr-node-b"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "169.254.169.3", Port: 5},
							Targets: []Addr{{IP: "10.128.1.2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "10.0.0.2", Port: 5},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "4.2.2.2", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "5.5.5.5", Port: 80},
							Targets: []Addr{{IP: "10.128.0.2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
					},
					Switches: []string{"switch-node-b"},
				},
			},
		},
	}

	// needs separate configuration variables for a V6 cluster
	tcV6 := []struct {
		name           string
		service        *v1.Service
		configs        []lbConfig
		expectedShared []LB
		expectedLocal  []LB
	}{
		// exactly the same as the v4 test under the same name
		{
			name:    "ipv6, nodeport service, standard pod",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:     []string{"node"},
					protocol: v1.ProtocolTCP,
					inport:   80,
					clusterEndpoints: lbEndpoints{
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V6IPs: []string{"fe00:0:0:0:1::2"}, Port: 8080},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::1", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fd00::111", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
			expectedLocal: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-a",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-a"},
					Switches:    []string{"switch-node-a"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::1", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fd00::111", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
				{
					Name:        "Service_testns/foo_TCP_node_router+switch_node-b",
					ExternalIDs: defaultExternalIDs,
					Routers:     []string{"gr-node-b"},
					Switches:    []string{"switch-node-b"},
					Protocol:    "TCP",
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
					},
					Opts: defaultOpts,
				},
			},
		},
		{
			// exactly the same as last test case for IPv4 but IPv6
			name:    "IPv6, LB service with NodePort, standard pods on different nodes, ExternalTrafficPolicy=local, one endpoint is ready, the other one is terminating and serving",
			service: defaultService,
			configs: []lbConfig{
				{
					vips:                 []string{"node"},
					protocol:             v1.ProtocolTCP,
					inport:               5, // nodePort
					hasNodePort:          true,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V6IPs: []string{"fe00:0:0:0:1::2"}, Port: 8080},
						nodeB: {V6IPs: []string{"fe00:0:0:0:2::2"}, Port: 8080},
					},
				},
				{
					vips:                 []string{"cafe::2", "abcd::5"}, // externalIP + LB IP
					protocol:             v1.ProtocolTCP,
					inport:               80,
					hasNodePort:          false,
					externalTrafficLocal: true,
					clusterEndpoints: lbEndpoints{
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
					nodeEndpoints: map[string]lbEndpoints{
						nodeA: {V6IPs: []string{"fe00:0:0:0:1::2"}, Port: 8080},
						nodeB: {V6IPs: []string{"fe00:0:0:0:2::2"}, Port: 8080},
					},
				},
			},
			expectedShared: []LB{
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},

					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::1", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "fd00::111", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "cafe::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // local endpoint
						},
						{
							Source:  Addr{IP: "abcd::5", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // local endpoint
						},
					},
					Routers: []string{"gr-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-a",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd69::3", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fd00::1", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "fd69::3", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fd00::111", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "cafe::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "abcd::5", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
					},
					Switches: []string{"switch-node-a"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_local_router_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        LBOpts{SkipSNAT: true, Reject: true},
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd00::2", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:2::2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
						{
							Source:  Addr{IP: "cafe::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:2::2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
						{
							Source:  Addr{IP: "abcd::5", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:2::2", Port: 8080}}, // local endpoint (fallback to terminating and serving)
						},
					},
					Routers: []string{"gr-node-b"},
				},
				{
					Name:        "Service_testns/foo_TCP_node_switch_node-b",
					Protocol:    "TCP",
					ExternalIDs: defaultExternalIDs,
					Opts:        defaultOpts,
					Rules: []LBRule{
						{
							Source:  Addr{IP: "fd69::3", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:2::2", Port: 8080}},
						},
						{
							Source:  Addr{IP: "fd00::2", Port: 5},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "cafe::2", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
						{
							Source:  Addr{IP: "abcd::5", Port: 80},
							Targets: []Addr{{IP: "fe00:0:0:0:1::2", Port: 8080}}, // prefer endpoint on node1 since it's ready
						},
					},
					Switches: []string{"switch-node-b"},
				},
			},
		},
	}
	// v4
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

	// v6
	globalconfig.Kubernetes.ServiceCIDRs = []*net.IPNet{svcCIDRv6}
	for i, tt := range tcV6 {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {

			if tt.expectedShared != nil {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
				actual := buildPerNodeLBs(tt.service, tt.configs, defaultNodesV6)
				assert.Equal(t, tt.expectedShared, actual, "shared gateway mode not as expected")
			}

			if tt.expectedLocal != nil {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeLocal
				actual := buildPerNodeLBs(tt.service, tt.configs, defaultNodesV6)
				assert.Equal(t, tt.expectedLocal, actual, "local gateway mode not as expected")
			}

		})
	}

}

func Test_idledServices(t *testing.T) {
	serviceName := "foo"
	ns := "testns"
	tenSecondsAgo := time.Now().Add(-10 * time.Second).Format(time.RFC3339)
	oneHourAgo := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)

	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
	}()

	tc := []struct {
		name     string
		service  *v1.Service
		expected LBOpts
	}{
		{
			name: "active service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns, Annotations: map[string]string{}},
			},
			expected: LBOpts{
				Reject:        true,
				EmptyLBEvents: false,
			},
		},
		{
			name: "idled service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns, Annotations: map[string]string{
					"k8s.ovn.org/idled-at": "2023-01-01T13:14:15Z",
				}},
			},
			expected: LBOpts{
				Reject:        false,
				EmptyLBEvents: true,
			},
		},
		{
			name: "recently unidled service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns, Annotations: map[string]string{
					"k8s.ovn.org/unidled-at": tenSecondsAgo,
				}},
			},
			expected: LBOpts{
				Reject:        false,
				EmptyLBEvents: true,
			},
		},
		{
			name: "long time unidled service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns, Annotations: map[string]string{
					"k8s.ovn.org/unidled-at": oneHourAgo,
				}},
			},
			expected: LBOpts{
				Reject:        true,
				EmptyLBEvents: false,
			},
		},
	}

	for i, tt := range tc {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			actualLbOpts := lbOpts(tt.service)
			assert.Equal(t, tt.expected, actualLbOpts)
		})
	}
}

func Test_getEndpointsForService(t *testing.T) {
	type args struct {
		slices []*discovery.EndpointSlice
		svc    *v1.Service
		nodes  sets.Set[string]
	}

	tests := []struct {
		name                 string
		args                 args
		wantClusterEndpoints map[string]lbEndpoints
		wantNodeEndpoints    map[string]map[string]lbEndpoints
	}{
		{
			name: "empty slices",
			args: args{
				slices: []*discovery.EndpointSlice{},
				svc:    getSampleServiceWithOnePort(httpPortName, httpPortValue, tcpv1),
			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no cluster-wide endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // no local endpoints
		},
		{
			name: "slice with one local endpoint",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: &tcpv1,
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // no need for local endpoints, service is not ETP or ITP local
		},
		{
			name: "slice with one local endpoint, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: &tcpv1,
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePortAndETPLocal("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.2"}, Port: 80}}}, // ETP=local, one local endpoint
		},
		{
			name: "slice with one non-local endpoint, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: &tcpv1,
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeB, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePortAndETPLocal("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // ETP=local but no local endpoint
		},
		{
			name: "slice of address type FQDN",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: &tcpv1,
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeFQDN,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "example.com"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // no local endpoint
		},
		{
			name: "slice with one endpoint, OVN zone with two nodes, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: &tcpv1,
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeB, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePortAndETPLocal("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA, nodeB), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {nodeB: lbEndpoints{V4IPs: []string{"10.0.0.2"}, Port: 80}}}, // endpoint on nodeB
		},
		{
			name: "slice with different port name than the service",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example-wrong"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(8080)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no cluster-wide endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // no local endpoints
		},
		{
			name: "slice and service without a port name, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Protocol: &tcpv1,
								Port:     ptr.To(int32(8080)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePortAndETPLocal("", 80, tcpv1), // port with no name
				nodes: sets.New(nodeA),                                       // one-node zone

			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, ""): {V4IPs: []string{"10.0.0.2"}, Port: 8080}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, ""): {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.2"}, Port: 8080}}}, // one local endpoint
		},
		{
			name: "slice with an IPv6 endpoint",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "2001:db2::2"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V6IPs: []string{"2001:db2::2"}, Port: 80}}, // one cluster-wide endpoint
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, //  local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "a slice with an IPv4 endpoint and a slice with an IPv6 endpoint (dualstack cluster), ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2"),
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "2001:db2::2"),
					},
				},
				svc:   getSampleServiceWithOnePortAndETPLocal("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, V6IPs: []string{"2001:db2::2"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.2"}, V6IPs: []string{"2001:db2::2"}, Port: 80}}},
		},
		{
			name: "one slice with a duplicate address in the same endpoint",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2", "10.0.0.2"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "one slice with a duplicate address across two endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   []discovery.Endpoint{kube_test.MakeReadyEndpoint(nodeA, "10.0.0.2"), kube_test.MakeReadyEndpoint(nodeA, "10.0.0.2")},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "multiples slices with a duplicate address, with both address being ready",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2", "10.1.1.2"),
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2", "10.2.2.2"),
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2", "10.1.1.2", "10.2.2.2"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "multiples slices with different ports, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2", "10.1.1.2"),
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("other-port"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(8080)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.3", "10.2.2.3"),
					},
				},
				svc:   getSampleServiceWithTwoPortsAndETPLocal("tcp-example", "other-port", 80, 8080, tcpv1, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2", "10.1.1.2"}, Port: 80},
				getServicePortKey(tcpv1, "other-port"):  {V4IPs: []string{"10.0.0.3", "10.2.2.3"}, Port: 8080}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.2", "10.1.1.2"}, Port: 80}},
				getServicePortKey(tcpv1, "other-port"):  {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.3", "10.2.2.3"}, Port: 8080}}},
		},
		{
			name: "multiples slices with different ports, OVN zone with two nodes, ETP=local",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeA, "10.0.0.2", "10.1.1.2"),
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("other-port"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(8080)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints:   kube_test.MakeReadyEndpointList(nodeB, "10.0.0.3", "10.2.2.3"),
					},
				},
				svc:   getSampleServiceWithTwoPortsAndETPLocal("tcp-example", "other-port", 80, 8080, tcpv1, tcpv1),
				nodes: sets.New(nodeA, nodeB), // zone with two nodes
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2", "10.1.1.2"}, Port: 80},
				getServicePortKey(tcpv1, "other-port"):  {V4IPs: []string{"10.0.0.3", "10.2.2.3"}, Port: 8080}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {nodeA: lbEndpoints{V4IPs: []string{"10.0.0.2", "10.1.1.2"}, Port: 80}},
				getServicePortKey(tcpv1, "other-port"):  {nodeB: lbEndpoints{V4IPs: []string{"10.0.0.3", "10.2.2.3"}, Port: 8080}}},
		},
		{
			name: "slice with a mix of ready and terminating (serving and non-serving) endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeReadyEndpoint(nodeA, "2001:db2::2"),
							kube_test.MakeReadyEndpoint(nodeA, "2001:db2::3"),
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::4"),
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::5"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::6"), // ignored
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone

			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V6IPs: []string{"2001:db2::2", "2001:db2::3"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "slice with a mix of terminating (serving and non-serving) endpoints and no ready endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::4"),
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::5"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::6"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::7"),
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V6IPs: []string{"2001:db2::4", "2001:db2::5"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "slice with only terminating non-serving endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::6"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::7"),
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no cluster-wide endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local

		},
		{
			name: "multiple slices with a mix of terminating (serving and non-serving) endpoints and no ready endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::2"), // ignored
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::3"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::4"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::5"), // ignored
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V6IPs: []string{"2001:db2::3", "2001:db2::4"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "multiple slices with only terminating non-serving endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::2"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::5"),
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no cluster-wide endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // no local endpoints

		},
		{
			name: "multiple slices with a mix of IPv4 and IPv6 ready and terminating (serving and non-serving) endpoints (dualstack cluster)",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeReadyEndpoint(nodeA, "10.0.0.2"),
							kube_test.MakeTerminatingServingEndpoint(nodeA, "10.0.0.3"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "10.0.0.4"), // ignored
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeReadyEndpoint(nodeA, "2001:db2::2"),
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::3"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::4"), // ignored
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.2"}, V6IPs: []string{"2001:db2::2"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "multiple slices with a mix of IPv4 and IPv6 terminating (serving and non-serving) endpoints and no ready endpoints (dualstack cluster)",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingServingEndpoint(nodeA, "10.0.0.3"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "10.0.0.4"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::3"),
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::4"),
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {V4IPs: []string{"10.0.0.3"}, V6IPs: []string{"2001:db2::3"}, Port: 80}},
			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
		{
			name: "multiple slices with a mix of IPv4 and IPv6 terminating non-serving endpoints (dualstack cluster)",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "10.0.0.4"), // ignored
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::4"), // ignored
						},
					},
				},
				svc:   getSampleServiceWithOnePort("tcp-example", 80, tcpv1),
				nodes: sets.New(nodeA), // one-node zone

			},
			wantClusterEndpoints: map[string]lbEndpoints{},            // no endpoints
			wantNodeEndpoints:    map[string]map[string]lbEndpoints{}, // no endpoints
		},
		{
			name: "multiple slices with a mix of IPv4 and IPv6 ready and terminating (serving and non-serving) endpoints (dualstack cluster) and service.PublishNotReadyAddresses=true",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeReadyEndpoint(nodeA, "10.0.0.2"),                 // included
							kube_test.MakeTerminatingServingEndpoint(nodeA, "10.0.0.3"),    // included
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "10.0.0.4"), // included
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab24",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     ptr.To("tcp-example"),
								Protocol: ptr.To(v1.ProtocolTCP),
								Port:     ptr.To(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							kube_test.MakeReadyEndpoint(nodeA, "2001:db2::2"),                 // included
							kube_test.MakeTerminatingServingEndpoint(nodeA, "2001:db2::3"),    // included
							kube_test.MakeTerminatingNonServingEndpoint(nodeA, "2001:db2::4"), // included
						},
					},
				},
				svc:   getSampleServiceWithOnePortAndPublishNotReadyAddresses("tcp-example", 80, tcpv1), // <-- publishNotReadyAddresses=true
				nodes: sets.New(nodeA),                                                                  // one-node zone
			},
			wantClusterEndpoints: map[string]lbEndpoints{
				getServicePortKey(tcpv1, "tcp-example"): {
					V4IPs: []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"},
					V6IPs: []string{"2001:db2::2", "2001:db2::3", "2001:db2::4"}, Port: 80}},

			wantNodeEndpoints: map[string]map[string]lbEndpoints{}, // local endpoints not filled in, since service is not ETP or ITP local
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			portToClusterEndpoints, portToNodeToEndpoints := getEndpointsForService(tt.args.slices, tt.args.svc, tt.args.nodes)
			assert.Equal(t, tt.wantClusterEndpoints, portToClusterEndpoints)
			assert.Equal(t, tt.wantNodeEndpoints, portToNodeToEndpoints)

		})
	}
}

func Test_makeNodeSwitchTargetIPs(t *testing.T) {
	tc := []struct {
		name                string
		config              *lbConfig
		node                string
		expectedTargetIPsV4 []string
		expectedTargetIPsV6 []string
		expectedV4Changed   bool
		expectedV6Changed   bool
	}{
		{
			name: "cluster ip service", //ETP=cluster by default on all services
			config: &lbConfig{
				vips:     []string{"1.2.3.4", "fe10::1"},
				protocol: v1.ProtocolTCP,
				inport:   80,
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"192.168.0.1"},
					V6IPs: []string{"fe00:0:0:0:1::2"},
					Port:  8080,
				},
				nodeEndpoints: map[string]lbEndpoints{
					nodeA: {
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
				},
			},
			node:                nodeA,
			expectedTargetIPsV4: []string{"192.168.0.1"},
			expectedTargetIPsV6: []string{"fe00:0:0:0:1::2"},
			expectedV4Changed:   false,
			expectedV6Changed:   false,
		},
		{
			name: "service with ETP=local, endpoint count changes",
			config: &lbConfig{
				vips:     []string{"1.2.3.4", "fe10::1"},
				protocol: v1.ProtocolTCP,
				inport:   80,
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"192.168.0.1", "192.168.1.1"},
					V6IPs: []string{"fe00:0:0:0:1::2", "fe00:0:0:0:2::2"},

					Port: 8080,
				},
				nodeEndpoints: map[string]lbEndpoints{
					nodeA: {
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
				},
				externalTrafficLocal: true,
			},
			node:                nodeA,
			expectedTargetIPsV4: []string{"192.168.0.1"}, // only the endpoint on nodeA is kept
			expectedTargetIPsV6: []string{"fe00:0:0:0:1::2"},
			expectedV4Changed:   true,
			expectedV6Changed:   true,
		},
		{
			name: "service with ETP=local, endpoint count is the same",
			config: &lbConfig{
				vips:     []string{"1.2.3.4", "fe10::1"},
				protocol: v1.ProtocolTCP,
				inport:   80,
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"192.168.0.1"},
					V6IPs: []string{"fe00:0:0:0:1::2"},

					Port: 8080,
				},
				nodeEndpoints: map[string]lbEndpoints{
					nodeA: {
						V4IPs: []string{"192.168.0.1"},
						V6IPs: []string{"fe00:0:0:0:1::2"},
						Port:  8080,
					},
				},
				externalTrafficLocal: true,
			},
			node:                nodeA,
			expectedTargetIPsV4: []string{"192.168.0.1"},
			expectedTargetIPsV6: []string{"fe00:0:0:0:1::2"},
			expectedV4Changed:   false,
		},
		{
			name: "service with ETP=local, no local endpoints left",
			config: &lbConfig{
				vips:     []string{"1.2.3.4", "fe10::1"},
				protocol: v1.ProtocolTCP,
				inport:   80,
				clusterEndpoints: lbEndpoints{
					V4IPs: []string{"192.168.1.1"},     // on nodeB
					V6IPs: []string{"fe00:0:0:0:2::2"}, // on nodeB
					Port:  8080,
				},
				// nothing on nodeA
				externalTrafficLocal: true,
			},
			node:                nodeA,
			expectedTargetIPsV4: []string{},
			expectedTargetIPsV6: []string{}, // no local endpoints
			expectedV4Changed:   true,
			expectedV6Changed:   true,
		},
	}
	for i, tt := range tc {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			actualTargetIPsV4, actualTargetIPsV6, actualV4Changed, actualV6Changed := makeNodeSwitchTargetIPs(tt.node, tt.config)
			assert.Equal(t, tt.expectedTargetIPsV4, actualTargetIPsV4)
			assert.Equal(t, tt.expectedTargetIPsV6, actualTargetIPsV6)
			assert.Equal(t, tt.expectedV4Changed, actualV4Changed)
			assert.Equal(t, tt.expectedV6Changed, actualV6Changed)

		})
	}
}
