package services

import (
	"fmt"
	"net"
	"testing"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	utilpointer "k8s.io/utils/pointer"
)

var alwaysReady = func() bool { return true }
var FakeGRs = "GR_1 GR_2"

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
}

func newController() *serviceController {
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	controller := NewController(client,
		informerFactory.Core().V1().Services(),
		informerFactory.Discovery().V1beta1().EndpointSlices(),
		informerFactory.Core().V1().Nodes(),
	)
	controller.servicesSynced = alwaysReady
	controller.endpointSlicesSynced = alwaysReady
	return &serviceController{
		controller,
		informerFactory.Core().V1().Services().Informer().GetStore(),
		informerFactory.Discovery().V1beta1().EndpointSlices().Informer().GetStore(),
	}
}

// TestSyncServices - an end-to-end test for the services controller.
func TestSyncServices(t *testing.T) {
	ns := "testns"
	serviceName := "foo"

	oldGateway := globalconfig.Gateway.Mode
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	globalconfig.IPv4Mode = true
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
		globalconfig.IPv4Mode = false
		globalconfig.Gateway.Mode = oldGateway
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}

	outport := int32(3456)
	tcp := v1.ProtocolTCP

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

	tests := []struct {
		name        string
		slice       *discovery.EndpointSlice
		service     *v1.Service
		ovnCmd      []ovntest.ExpectedCmd
		gatewayMode string
	}{

		{
			name: "create service from Single Stack Service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,protocol,external_ids,vips find load_balancer`,
					Output: `{"data": []}`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_switch`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""}`,
					Output: "uuid-1",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --may-exist ls-lb-add switch-node-a uuid-1 -- --may-exist ls-lb-add switch-node-b uuid-1 -- --may-exist lr-lb-add gr-node-a uuid-1 -- --may-exist lr-lb-add gr-node-b uuid-1`,
				},
			},
		},
		{
			name: "update service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,protocol,external_ids,vips find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],"tcp",["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]],["map",[["192.168.0.1:6443",""]]]]]}`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_switch`,
					Output: `wrong-switch,uuid-1`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
					Output: "gr-node-a,uuid-1\ngr-node-c,uuid-1",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""} ` +
						`-- --may-exist ls-lb-add switch-node-a uuid-1 -- --may-exist ls-lb-add switch-node-b uuid-1 -- --if-exists ls-lb-del wrong-switch uuid-1 -- --may-exist lr-lb-add gr-node-b uuid-1 -- --if-exists lr-lb-del gr-node-c uuid-1`,
				},
			},
		},
		{
			name: "remove service from legacy load balancers",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,protocol,external_ids,vips find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],"tcp",["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]],["map",[["192.168.0.1:6443",""]]]],["",["uuid","uuid-legacy"],"tcp",["map",[["TCP_lb_gateway_router",""]]],["map",[["192.168.1.1:80",""]]]]]}`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_switch`,
					Output: "switch-node-a,uuid-1\nswitch-node-b,uuid-1",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
					Output: "gr-node-a,uuid-1\ngr-node-b,uuid-1",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""}`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --if-exists remove load_balancer uuid-legacy vips  "192.168.1.1:80"`,
				},
			},
		},
		{
			name: "transition to endpoints, create nodeport",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{
					{
						Protocol: &tcp,
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
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
						NodePort:   8989,
					}},
				},
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,protocol,external_ids,vips find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],"tcp",["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]],["map",[["192.168.0.1:6443",""]]]]]}`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_switch`,
					Output: "switch-node-a,uuid-1\nswitch-node-b,uuid-1",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
					Output: "gr-node-a,uuid-1\ngr-node-b,uuid-1",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_node_router+switch_node-a options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"10.0.0.1:8989"="10.128.0.2:3456,10.128.1.2:3456"}`,
					Output: "uuid-rs-nodea",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_node_router+switch_node-b options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"10.0.0.2:8989"="10.128.0.2:3456,10.128.1.2:3456"}`,
					Output: "uuid-rs-nodeb",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true options:skip_snat=false protocol=tcp selection_fields=[] vips={"192.168.1.1:80"="10.128.0.2:3456,10.128.1.2:3456"}` +
						` -- --may-exist ls-lb-add switch-node-a uuid-rs-nodea -- --may-exist lr-lb-add gr-node-a uuid-rs-nodea` +
						` -- --may-exist ls-lb-add switch-node-b uuid-rs-nodeb -- --may-exist lr-lb-add gr-node-b uuid-rs-nodeb`,
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			if tt.gatewayMode != "" {
				globalconfig.Gateway.Mode = globalconfig.GatewayMode(tt.gatewayMode)
			} else {
				globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			}

			ovnlb.TestOnlySetCache(nil)
			controller, err := newController()
			if err != nil {
				t.Fatalf("Error creating controller: %v", err)
			}
			defer controller.close()
			// Add objects to the Store
			controller.endpointSliceStore.Add(tt.slice)
			controller.serviceStore.Add(tt.service)

			controller.nodeTracker.nodes = defaultNodes
			// Expected OVN commands
			fexec := ovntest.NewLooseCompareFakeExec()
			for _, cmd := range tt.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)
			}
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			err = controller.syncService(ns + "/" + serviceName)
			if err != nil {
				t.Errorf("syncServices error: %v", err)
			}

			if !fexec.CalledMatchesExpected() {
				t.Error(fexec.ErrorDesc())
			}
		})
	}
}

// A service can mutate its ports, we need to be sure we don´t left dangling ports
func TestUpdateServicePorts(t *testing.T) {
	config.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
	}()

	// Expected OVN commands
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda30 vips \"192.168.1.1:80\"",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Add a new loadbalancer with the Service Port 80
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.0.0.2:3456"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_1`,
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_2`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer load_balancer_1 vips "192.168.1.1:80"`,
		Output: "",
	})
	// update service starts here
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda30 vips \"192.168.1.1:8888\"",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Add a new loadbalancer with the new Service Port 8888
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:8888"="10.0.0.2:3456"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_1`,
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_2`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer load_balancer_1 vips "192.168.1.1:8888"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: gatewayRouter1,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Remove the old ServicePort
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=%s", gatewayRouter1),
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=2e290f10-3652-11eb-839b-a8a1590cda29"),
		Output: "node_load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: `ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_1 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer node_load_balancer_1 vips "192.168.1.1:80"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda30 vips \"192.168.1.1:80\"",
		Output: "",
	})

	err := util.SetExec(fexec)
	if err != nil {
		t.Errorf("fexec error: %v", err)
	}

	ns := "testns"
	serviceName := "foo"
	slice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "ab23",
			Namespace: ns,
			Labels:    map[string]string{discovery.LabelServiceName: serviceName},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     utilpointer.StringPtr("tcp-example"),
				Protocol: protoPtr(v1.ProtocolTCP),
				Port:     utilpointer.Int32Ptr(int32(3456)),
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Conditions: discovery.EndpointConditions{
					Ready: utilpointer.BoolPtr(true),
				},
				Addresses: []string{"10.0.0.2"},
				Topology:  map[string]string{"kubernetes.io/hostname": "node-1"},
			},
		},
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
	}
	controller := newController()
	// Process the first service
	controller.endpointSliceStore.Add(slice)
	controller.serviceStore.Add(service)
	controller.syncServices(ns + "/" + serviceName)

	// Update the service with a different Port
	serviceNew := service.DeepCopy()
	serviceNew.Spec.Ports[0].Port = 8888
	controller.serviceStore.Delete(service)
	controller.serviceStore.Add(serviceNew)
	// sync service
	controller.syncServices(ns + "/" + serviceName)
	if controller.serviceTracker.hasServiceVIP(serviceName, ns, "192.168.1.1:80", v1.ProtocolTCP) {
		t.Fatalf("Service with port 80 should not exist")
	}
	if !controller.serviceTracker.hasServiceVIP(serviceName, ns, "192.168.1.1:8888", v1.ProtocolTCP) {
		t.Fatalf("Service with port 8888 should exist")
	}
	if !fexec.CalledMatchesExpected() {
		t.Error(fexec.ErrorDesc())
	}
}

// A service can mutate its ports, we need to be sure we don´t left dangling ports
func TestUpdateServiceEndpointsToHost(t *testing.T) {
	config.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
	}()

	// Expected OVN commands
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda30 vips \"192.168.1.1:80\"",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Add a new loadbalancer with the Service Port 80
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.128.0.2:3456"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_1`,
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=1`,
		Output: "load_balancer_worker_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_2`,
		Output: "load_balancer_2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=2`,
		Output: "load_balancer_worker_2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: `ovn-nbctl --timeout=15 --if-exists remove load_balancer load_balancer_1 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_worker_1 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_2 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_worker_2 vips "192.168.1.1:80"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Update endpoints to have host endpoint in shared gw mode
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_1`,
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 get logical_router GR_1 external_ids:physical_ips`,
		Output: "2.2.2.2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=1`,
		Output: "load_balancer_worker_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_2`,
		Output: "load_balancer_2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 get logical_router GR_2 external_ids:physical_ips`,
		Output: "2.2.2.3",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=2`,
		Output: "load_balancer_worker_2",
	})
	// endpoint is self node IP, so need to use special masquerade endpoint
	// use regular backend on the worker switch LB
	// adding to second node will not use special masquerade
	// and regular endpoint IP on the 2nd worker switch
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: `ovn-nbctl --timeout=15 set load_balancer load_balancer_1 vips:"192.168.1.1:80"="169.254.169.2:3456"` +
			` -- set load_balancer load_balancer_worker_1 vips:"192.168.1.1:80"="2.2.2.2:3456"` +
			` -- set load_balancer load_balancer_2 vips:"192.168.1.1:80"="2.2.2.2:3456"` +
			` -- set load_balancer load_balancer_worker_2 vips:"192.168.1.1:80"="2.2.2.2:3456"`,
		Output: "",
	})
	// Ensure the VIP entry is removed on the cluster wide TCP load balancer
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips "192.168.1.1:80"`,
		Output: "",
	})

	err := util.SetExec(fexec)
	if err != nil {
		t.Errorf("fexec error: %v", err)
	}

	ns := "testns"
	serviceName := "foo"
	slice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "ab23",
			Namespace: ns,
			Labels:    map[string]string{discovery.LabelServiceName: serviceName},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     utilpointer.StringPtr("tcp-example"),
				Protocol: protoPtr(v1.ProtocolTCP),
				Port:     utilpointer.Int32Ptr(int32(3456)),
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Conditions: discovery.EndpointConditions{
					Ready: utilpointer.BoolPtr(true),
				},
				Addresses: []string{"10.128.0.2"},
				Topology:  map[string]string{"kubernetes.io/hostname": "node-1"},
			},
		},
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
	}
	oldClusterSubnet := config.Default.ClusterSubnets
	oldGwMode := config.Gateway.Mode
	defer func() {
		config.Gateway.Mode = oldGwMode
		config.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr, _ := net.ParseCIDR("10.128.0.0/24")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr, 26}}
	config.Gateway.Mode = config.GatewayModeShared
	controller := newController()
	// Process the first service
	controller.endpointSliceStore.Add(slice)
	controller.serviceStore.Add(service)
	controller.syncServices(ns + "/" + serviceName)

	// Update endpoints with host network pod
	epsNew := slice.DeepCopy()
	epsNew.Endpoints = []discovery.Endpoint{
		{
			Conditions: discovery.EndpointConditions{
				Ready: utilpointer.BoolPtr(true),
			},
			Addresses: []string{"2.2.2.2"},
			Topology:  map[string]string{"kubernetes.io/hostname": "node-1"},
		}}
	controller.endpointSliceStore.Delete(slice)
	controller.endpointSliceStore.Add(epsNew)
	// sync service
	controller.syncServices(ns + "/" + serviceName)

	if !fexec.CalledMatchesExpected() {
		t.Error(fexec.ErrorDesc())
	}
}

// Update a service that was not idled, change endpoints that are both non host network and ensure that
// there are no unnecessary remove cmds
func TestUpdateServiceEndpointsLessRemoveOps(t *testing.T) {
	config.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
	}()
	// Expected OVN commands
	fexec := ovntest.NewFakeExec()
	// First sync we expect the redundant remove commands
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: idlingloadbalancerTCP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda30 vips \"192.168.1.1:80\"",
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Add a new loadbalancer with the Service Port 80
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.128.0.2:3456"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_1`,
		Output: "load_balancer_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=1`,
		Output: "load_balancer_worker_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=GR_2`,
		Output: "load_balancer_2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-worker-lb-tcp=2`,
		Output: "load_balancer_worker_2",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: `ovn-nbctl --timeout=15 --if-exists remove load_balancer load_balancer_1 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_worker_1 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_2 vips "192.168.1.1:80"` +
			` -- --if-exists remove load_balancer load_balancer_worker_2 vips "192.168.1.1:80"`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Update endpoints to have new endpoint in shared gw mode, should not call redundant remove ops on idling, worker lbs
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.128.0.6:3456"`,
		Output: "",
	})

	err := util.SetExec(fexec)
	if err != nil {
		t.Errorf("fexec error: %v", err)
	}

	ns := "testns"
	serviceName := "foo"
	slice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "ab23",
			Namespace: ns,
			Labels:    map[string]string{discovery.LabelServiceName: serviceName},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     utilpointer.StringPtr("tcp-example"),
				Protocol: protoPtr(v1.ProtocolTCP),
				Port:     utilpointer.Int32Ptr(int32(3456)),
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Conditions: discovery.EndpointConditions{
					Ready: utilpointer.BoolPtr(true),
				},
				Addresses: []string{"10.128.0.2"},
				Topology:  map[string]string{"kubernetes.io/hostname": "node-1"},
			},
		},
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
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
	}
	oldClusterSubnet := config.Default.ClusterSubnets
	oldGwMode := config.Gateway.Mode
	defer func() {
		config.Gateway.Mode = oldGwMode
		config.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr, _ := net.ParseCIDR("10.128.0.0/24")
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr, 26}}
	config.Gateway.Mode = config.GatewayModeShared
	controller := newController()
	// Process the first service
	controller.endpointSliceStore.Add(slice)
	controller.serviceStore.Add(service)
	controller.syncServices(ns + "/" + serviceName)

	// Update endpoints with host network pod
	epsNew := slice.DeepCopy()
	epsNew.Endpoints = []discovery.Endpoint{
		{
			Conditions: discovery.EndpointConditions{
				Ready: utilpointer.BoolPtr(true),
			},
			Addresses: []string{"10.128.0.6"},
			Topology:  map[string]string{"kubernetes.io/hostname": "node-1"},
		}}
	controller.endpointSliceStore.Delete(slice)
	controller.endpointSliceStore.Add(epsNew)
	// sync service
	controller.syncServices(ns + "/" + serviceName)

	if !fexec.CalledMatchesExpected() {
		t.Error(fexec.ErrorDesc())
	}
}

// protoPtr takes a Protocol and returns a pointer to it.
func protoPtr(proto v1.Protocol) *v1.Protocol {
	return &proto
}
