package services

import (
	"fmt"
	"net"
	"testing"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
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
	stopChan           chan struct{}
}

func newController() (*serviceController, error) {
	return newControllerWithDBSetup(libovsdbtest.TestSetup{})
}

func newControllerWithDBSetup(dbSetup libovsdbtest.TestSetup) (*serviceController, error) {
	stopChan := make(chan struct{})
	client := fake.NewSimpleClientset()
	nbClient, err := libovsdbtest.NewNBTestHarness(dbSetup, stopChan)
	if err != nil {
		return nil, err
	}
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	controller := NewController(client,
		nbClient,
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
		stopChan,
	}, nil
}

func (c *serviceController) close() {
	close(c.stopChan)
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
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,external_ids find load_balancer`,
					Output: `{"data": []}`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_switch`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""}`,
					Output: "uuid-1",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --may-exist ls-lb-add switch-node-a uuid-1 -- --may-exist ls-lb-add switch-node-b uuid-1 -- --may-exist lr-lb-add gr-node-a uuid-1 -- --may-exist lr-lb-add gr-node-b uuid-1`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=json --columns=_uuid,name,external_ids,vips,protocol find load_balancer`,
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
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,external_ids find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]]]],"headings":["name","_uuid","external_ids"]}`,
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
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""} ` +
						`-- --may-exist ls-lb-add switch-node-a uuid-1 -- --may-exist ls-lb-add switch-node-b uuid-1 -- --if-exists ls-lb-del wrong-switch uuid-1 -- --may-exist lr-lb-add gr-node-b uuid-1 -- --if-exists lr-lb-del gr-node-c uuid-1`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=json --columns=_uuid,name,external_ids,vips,protocol find load_balancer`,
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
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,external_ids find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]]]],"headings":["name","_uuid","external_ids"]}`,
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
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"192.168.1.1:80"=""}`,
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 --no-heading --format=csv --data=json --columns=_uuid,name,external_ids,vips,protocol find load_balancer`,
					Output: `"[""uuid"",""uuid-legacy""]","""""","[""map"",[[""TCP_lb_gateway_router"",""GR_node-a""]]]","[""map"",[[""192.168.1.1:80"",""10.89.0.137:6443""]]]","""tcp"""`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --if-exists remove load_balancer uuid-legacy vips "192.168.1.1:80"`,
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
					Cmd:    `ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,external_ids find load_balancer`,
					Output: `{"data":[["Service_testns/foo_TCP_cluster",["uuid","uuid-1"],["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","testns/foo"]]]]],"headings":["name","_uuid","external_ids"]}`,
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
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_node_router+switch_node-a options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"10.0.0.1:8989"="10.128.0.2:3456,10.128.1.2:3456"}`,
					Output: "uuid-rs-nodea",
				},
				{
					Cmd:    `ovn-nbctl --timeout=15 create load_balancer external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_node_router+switch_node-b options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"10.0.0.2:8989"="10.128.0.2:3456,10.128.1.2:3456"}`,
					Output: "uuid-rs-nodeb",
				},
				{
					Cmd: `ovn-nbctl --timeout=15 set load_balancer uuid-1 external_ids:k8s.ovn.org/kind=Service external_ids:k8s.ovn.org/owner=testns/foo name=Service_testns/foo_TCP_cluster options:event=false options:reject=true protocol=tcp selection_fields=[] vips={"192.168.1.1:80"="10.128.0.2:3456,10.128.1.2:3456"}` +
						` -- --may-exist ls-lb-add switch-node-a uuid-rs-nodea -- --may-exist lr-lb-add gr-node-a uuid-rs-nodea` +
						` -- --may-exist ls-lb-add switch-node-b uuid-rs-nodeb -- --may-exist lr-lb-add gr-node-b uuid-rs-nodeb`,
				},
				{
					Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=json --columns=_uuid,name,external_ids,vips,protocol find load_balancer`,
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
			err = util.SetExec(fexec)
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
