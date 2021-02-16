package services

import (
	"testing"

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

const (
	loadbalancerTCP = "a08ea426-2288-11eb-a30b-a8a1590cda29"
	portGroupUUID   = "58a1ef18-3649-11eb-bd94-a8a1590cda29"
	gatewayRouter1  = "2e290f10-3652-11eb-839b-a8a1590cda29"
	logicalSwitch1  = "17bde5e8-3652-11eb-b53b-a8a1590cda29"
)

var alwaysReady = func() bool { return true }

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
		portGroupUUID,
	)
	controller.servicesSynced = alwaysReady
	controller.endpointSlicesSynced = alwaysReady
	return &serviceController{
		controller,
		informerFactory.Core().V1().Services().Informer().GetStore(),
		informerFactory.Discovery().V1beta1().EndpointSlices().Informer().GetStore(),
	}
}

func TestSyncServices(t *testing.T) {
	ns := "testns"
	serviceName := "foo"

	tests := []struct {
		name          string
		slice         *discovery.EndpointSlice
		service       *v1.Service
		updateTracker bool
		ovnCmd        []ovntest.ExpectedCmd
	}{
		{
			name: "delete endpoint slice with no service",
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
			service:       &v1.Service{},
			updateTracker: true,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
					Output: loadbalancerTCP,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
					Output: "",
				},
			},
		},
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
			updateTracker: true,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
					Output: loadbalancerTCP,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
					Output: "",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.1.1 && tcp && tcp.dst==80" action=reject name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80 -- add port_group 58a1ef18-3649-11eb-bd94-a8a1590cda29 acls @reject-acl`,
					Output: "",
				},
			},
		},
		{
			name: "create OVN LoadBalancer from Single Stack NodePort Service without endpoints",
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
						NodePort:   32766,
					}},
				},
			},
			updateTracker: true,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
					Output: loadbalancerTCP,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
					Output: "",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.1.1 && tcp && tcp.dst==80" action=reject name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80 -- add port_group 58a1ef18-3649-11eb-bd94-a8a1590cda29 acls @reject-acl`,
					Output: "",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null`,
					Output: gatewayRouter1,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=` + gatewayRouter1,
					Output: "",
				},

				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_switch load_balancer{>=}a08ea426-2288-11eb-a30b-a8a1590cda29`,
					Output: logicalSwitch1,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.1.1 && tcp && tcp.dst==80" action=reject name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80 -- add logical_switch 17bde5e8-3652-11eb-b53b-a8a1590cda29 acls @reject-acl`,
					Output: "",
				},
			},
		},
		{
			name: "create OVN LoadBalancer from Single Stack Service with endpoints",
			slice: &discovery.EndpointSlice{
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
			updateTracker: false,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
					Output: loadbalancerTCP,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 set load_balancer ` + loadbalancerTCP + ` vips:"192.168.1.1:80"="10.0.0.2:3456"`,
					Output: "",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
					Output: "",
				},
			},
		},
		{
			name: "create OVN LoadBalancer from Dual Stack Service with dual stack endpoints",
			slice: &discovery.EndpointSlice{
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
			updateTracker: false,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
					Output: loadbalancerTCP,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.0.0.2:3456"`,
					Output: "",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
					Output: "",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := newController()
			// Add objects to the Store
			controller.endpointSliceStore.Add(tt.slice)
			controller.serviceStore.Add(tt.service)
			if tt.updateTracker {
				controller.serviceTracker.updateKubernetesService(tt.service)
			}

			// Expected OVN commands
			fexec := ovntest.NewFakeExec()
			for _, cmd := range tt.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)
			}
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			controller.syncServices(ns + "/" + serviceName)
		})
	}
}

// A service can mutate its ports, we need to be sure we don´t left dangling ports
func TestUpdateServicePorts(t *testing.T) {
	// Expected OVN commands
	fexec := ovntest.NewFakeExec()
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
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
		Output: "",
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
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:8888`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Remove the old ServicePort
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips "192.168.1.1:80"`,
		Output: "",
	})
	// Remove the ACL if exist
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
		Output: "",
	})
	// Check if there are NodePort LoadBalancers
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null`,
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
}

// A service created has no VIP entry and reject acl is created
func TestServiceCreateReject(t *testing.T) {
	// Expected OVN commands
	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Find if ACL exists
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
		Output: "",
	})
	// Create ACL
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.1.1 && tcp && tcp.dst==80" action=reject name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80 -- add port_group 58a1ef18-3649-11eb-bd94-a8a1590cda29 acls @reject-acl`,
		Output: "",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: loadbalancerTCP,
	})
	// Endpoints got added, create LB entry
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 set load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips:"192.168.1.1:80"="10.0.0.2:3456"`,
		Output: loadbalancerTCP,
	})
	// Find existing ACL
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-192.168.1.1\:80`,
		Output: "1234",
	})
	// Remove ACL
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    `ovn-nbctl --timeout=15 -- --if-exists remove port_group 58a1ef18-3649-11eb-bd94-a8a1590cda29 acls 1234`,
		Output: "",
	})

	err := util.SetExec(fexec)
	if err != nil {
		t.Errorf("fexec error: %v", err)
	}

	ns := "testns"
	serviceName := "foo"
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
	controller := newController()
	// Process the first service, should not create VIP in LB and create reject ACL instead
	controller.serviceStore.Add(service)
	controller.syncServices(ns + "/" + serviceName)

	// Now add endpoints and ensure ACL is removed and LB VIP gets created
	controller.endpointSliceStore.Add(slice)
	controller.syncServices(ns + "/" + serviceName)
}

// protoPtr takes a Protocol and returns a pointer to it.
func protoPtr(proto v1.Protocol) *v1.Protocol {
	return &proto
}
