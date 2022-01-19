package services

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"runtime/trace"
	"testing"
	"time"

	"github.com/google/uuid"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
)

const extraServices = 2000 // how many extra services to add to the dbs but not sync

func BenchmarkUpdateServiceLBs(b *testing.B) {
	klog.SetOutput(io.Discard)
	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	flags.Set("logtostderr", "false")
	ns := "testnnnns"
	serviceName := "foo"

	oldGateway := globalconfig.Gateway.Mode
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	globalconfig.IPv4Mode = true
	globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
		globalconfig.IPv4Mode = false
		globalconfig.Gateway.Mode = oldGateway
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	defaultNodes := map[string]nodeInfo{
		"node-a": {
			name:              "node-a",
			nodeIPs:           []string{"10.0.0.1"},
			gatewayRouterName: "gr-node-a",
			switchName:        "switch-node-a",
		},
		"node-b": {
			name:              "node-b",
			nodeIPs:           []string{"10.0.0.2"},
			gatewayRouterName: "gr-node-b",
			switchName:        "switch-node-b",
		},
	}

	slices := make([]*discovery.EndpointSlice, 0, b.N)
	services := make([]*v1.Service, 0, b.N)
	initialDb := make([]libovsdbtest.TestData, 0, 10+b.N)
	lbuuids := make([]string, 0, b.N)
	for i := 0; i < b.N+extraServices; i++ {
		sn := fmt.Sprintf("%s-%d", serviceName, i)
		slices = append(slices, &discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sn + "ab23",
				Namespace: ns,
				Labels:    map[string]string{discovery.LabelServiceName: sn},
			},
			Ports:       []discovery.EndpointPort{},
			AddressType: discovery.AddressTypeIPv4,
			Endpoints: []discovery.Endpoint{
				{
					Conditions: discovery.EndpointConditions{
						Ready: utilpointer.BoolPtr(true),
					},
					Addresses: []string{"10.128.0.2", "10.128.1.2"},
				},
			},
		})
		services = append(services, &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: sn, Namespace: ns},
			Spec: v1.ServiceSpec{
				Type:       v1.ServiceTypeClusterIP,
				ClusterIP:  "192.168.1.1",
				ClusterIPs: []string{"192.168.1.1"},
				Selector:   map[string]string{"foo": "bar"},
				Ports: []v1.ServicePort{{
					Port:       80,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.FromInt(3456),
				}},
			},
		})
		uu := uuid.New()
		initialDb = append(initialDb,
			&nbdb.LoadBalancer{
				UUID: uu.String(),
				Name: "Service_" + ns + "/" + sn + "_TCP_cluster",
				Options: map[string]string{
					"event":     "false",
					"reject":    "true",
					"skip_snat": "false",
				},
				Protocol: &nbdb.LoadBalancerProtocolTCP,
				Vips: map[string]string{
					"192.168.0.1:6443": "",
				},
				ExternalIDs: map[string]string{
					"k8s.ovn.org/kind":  "Service",
					"k8s.ovn.org/owner": ns + "/" + sn,
				},
			})
		lbuuids = append(lbuuids, uu.String())

	}

	initialDb = append(initialDb,
		&nbdb.LogicalSwitch{
			UUID: "switch-node-a",
			Name: "switch-node-a",
		},
		&nbdb.LogicalSwitch{
			UUID: "switch-node-b",
			Name: "switch-node-b",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-a",
			Name: "gr-node-a",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-b",
			Name: "gr-node-b",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-c",
			Name: "gr-node-c",
		},
		&nbdb.LoadBalancerGroup{
			Name:         types.ClusterLBGroupName,
			LoadBalancer: lbuuids,
		},
	)

	controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: initialDb})
	if err != nil {
		b.Fatalf("Error creating controller: %v", err)
	}
	controller.useLBGroups = true
	controller.repair.semLegacyLBsDeleted = 1
	defer controller.close()
	// Add objects to the Store
	for i := 0; i < b.N; i++ {
		controller.endpointSliceStore.Add(slices[i])
		controller.serviceStore.Add(services[i])
	}

	for {
		lbs, _ := libovsdbops.ListLoadBalancers(controller.nbClient)
		if len(lbs) == b.N+extraServices {
			break
		}
		b.Log("waiting for lbs to coalesce")
		time.Sleep(100 * time.Millisecond)
	}

	controller.nodeTracker.nodes = defaultNodes
	b.ResetTimer()
	b.ReportAllocs()
	b.Log("Setup complete, starting benchmark.")
	fmt.Println("Setup complete, starting...")

	for i := 0; i < b.N; i++ {
		sn := fmt.Sprintf("%s/%s-%d", ns, serviceName, i)
		err = controller.syncService(sn)
		if err != nil {
			b.Errorf("syncServices error: %v", err)
		}
	}

}

func BenchmarkSyncCreateService(b *testing.B) {
	klog.SetOutput(io.Discard)
	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	flags.Set("logtostderr", "false")
	ns := "testnnnns"
	serviceName := "foo"

	oldGateway := globalconfig.Gateway.Mode
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	globalconfig.IPv4Mode = true
	globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
		globalconfig.IPv4Mode = false
		globalconfig.Gateway.Mode = oldGateway
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	defaultNodes := map[string]nodeInfo{
		"node-a": {
			name:              "node-a",
			nodeIPs:           []string{"10.0.0.1"},
			gatewayRouterName: "gr-node-a",
			switchName:        "switch-node-a",
		},
		"node-b": {
			name:              "node-b",
			nodeIPs:           []string{"10.0.0.2"},
			gatewayRouterName: "gr-node-b",
			switchName:        "switch-node-b",
		},
	}

	slices := make([]*discovery.EndpointSlice, 0, b.N)
	services := make([]*v1.Service, 0, b.N)
	initialDb := make([]libovsdbtest.TestData, 0, 10+b.N)
	lbuuids := make([]string, 0, b.N)
	for i := 0; i < b.N+extraServices; i++ {
		sn := fmt.Sprintf("%s-%d", serviceName, i)
		slices = append(slices, &discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sn + "ab23",
				Namespace: ns,
				Labels:    map[string]string{discovery.LabelServiceName: sn},
			},
			Ports:       []discovery.EndpointPort{},
			AddressType: discovery.AddressTypeIPv4,
			Endpoints: []discovery.Endpoint{
				{
					Conditions: discovery.EndpointConditions{
						Ready: utilpointer.BoolPtr(true),
					},
					Addresses: []string{"10.128.0.2", "10.128.1.2"},
				},
			},
		})
		services = append(services, &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: sn, Namespace: ns},
			Spec: v1.ServiceSpec{
				Type:       v1.ServiceTypeClusterIP,
				ClusterIP:  "192.168.1.1",
				ClusterIPs: []string{"192.168.1.1"},
				Selector:   map[string]string{"foo": "bar"},
				Ports: []v1.ServicePort{{
					Port:       80,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.FromInt(3456),
				}},
			},
		})

		// if this is an "extra" service, create a load balancer for us to deal with.
		if i >= b.N {
			uu := uuid.New()
			initialDb = append(initialDb,
				&nbdb.LoadBalancer{
					UUID: uu.String(),
					Name: "Service_" + ns + "/" + sn + "_TCP_cluster",
					Options: map[string]string{
						"event":     "false",
						"reject":    "true",
						"skip_snat": "false",
					},
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: map[string]string{
						"k8s.ovn.org/kind":  "Service",
						"k8s.ovn.org/owner": ns + "/" + sn,
					},
				})
			lbuuids = append(lbuuids, uu.String())
		}
	}

	initialDb = append(initialDb,
		&nbdb.LogicalSwitch{
			UUID: "switch-node-a",
			Name: "switch-node-a",
		},
		&nbdb.LogicalSwitch{
			UUID: "switch-node-b",
			Name: "switch-node-b",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-a",
			Name: "gr-node-a",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-b",
			Name: "gr-node-b",
		},
		&nbdb.LogicalRouter{
			UUID: "gr-node-c",
			Name: "gr-node-c",
		},
		&nbdb.LoadBalancerGroup{
			Name:         types.ClusterLBGroupName,
			LoadBalancer: lbuuids,
		},
	)

	controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: initialDb})
	if err != nil {
		b.Fatalf("Error creating controller: %v", err)
	}
	controller.useLBGroups = true
	controller.repair.semLegacyLBsDeleted = 1
	defer controller.close()
	// Add objects to the Store
	for i := 0; i < b.N; i++ {
		controller.endpointSliceStore.Add(slices[i])
		controller.serviceStore.Add(services[i])
	}

	for {
		lbs, _ := libovsdbops.ListLoadBalancers(controller.nbClient)
		if len(lbs) == extraServices {
			break
		}
		b.Log("waiting for lbs to coalesce")
		time.Sleep(100 * time.Millisecond)
	}

	controller.nodeTracker.nodes = defaultNodes
	b.ResetTimer()
	b.Log("Setup complete, starting benchmark.")
	fmt.Println("Setup complete, starting...")
	defer trace.StartRegion(context.Background(), "runBench").End()

	for i := 0; i < b.N; i++ {
		sn := fmt.Sprintf("%s/%s-%d", ns, serviceName, i)
		err = controller.syncService(sn)
		if err != nil {
			b.Errorf("syncServices error: %v", err)
		}
	}

}
