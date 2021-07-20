package egressfirewall

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/miekg/dns"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	efapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	fakeefclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	efinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers/externalversions"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	addrset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	addrsetmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	mock "github.com/stretchr/testify/mock"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

const (
	ns       = "testing"
	cidr     = "1.0.0.0/10"
	nodeName = "test-Node1"

	ipv4Source = "ipv4Source"
	ipv6Source = "ipv6Source"

	efACLToDelete = "a08ea426-2288-11eb-a30b-a8a1590cda30"
)

type egressFirewallController struct {
	*Controller
	kubeMocks           *kubemocks.KubeInterface
	addrsetFactoryMocks *addrsetmocks.AddressSetFactory
	efStore             cache.Store
}

func newController() *egressFirewallController {
	client := fake.NewSimpleClientset(&kapi.NodeList{
		Items: []kapi.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					UID:  ktypes.UID(nodeName),
				},
			},
		},
	})
	efClient := fakeefclientset.NewSimpleClientset()
	efInformerFactory := efinformer.NewSharedInformerFactory(efClient, 0)
	fakeClient := &util.OVNClientset{
		KubeClient:           client,
		EgressIPClient:       nil,
		EgressFirewallClient: efClient,
	}
	watcher, _ := factory.NewMasterWatchFactory(fakeClient)
	kubeMocks := new(kubemocks.KubeInterface)
	addrsetFactoryMocks := new(addrsetmocks.AddressSetFactory)

	controller := NewController(client,
		kubeMocks,
		efClient,
		efInformerFactory.K8s().V1().EgressFirewalls(),
		addrsetFactoryMocks,
		watcher,
	)
	return &egressFirewallController{
		controller,
		kubeMocks,
		addrsetFactoryMocks,
		efInformerFactory.K8s().V1().EgressFirewalls().Informer().GetStore(),
	}
}

func TestSyncEgressFirewall(t *testing.T) {
	ipv4HashedNamespace, _ := addrset.MakeAddressSetHashNames(ns)

	tests := []struct {
		name                       string
		ipv4Mode                   bool
		ipv6Mode                   bool
		isLocalGatewayMode         bool
		ef                         *efapi.EgressFirewall
		egressFirewall             *egressFirewall
		ovnCmd                     []ovntest.ExpectedCmd
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		kubeMocksOpsHelper         []ovntest.TestifyMockHelper
	}{
		{
			name:               "create basic egressFirewall in local GW mode ipv4 only",
			ipv4Mode:           true,
			ipv6Mode:           false,
			isLocalGatewayMode: true,
			ef: &efapi.EgressFirewall{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: ns,
				},
				Spec: efapi.EgressFirewallSpec{
					Egress: []efapi.EgressFirewallRule{
						{
							Type: efapi.EgressFirewallRuleDeny,
							To: efapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					},
				},
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && ip4.dst != %s\" action=drop external-ids:egressFirewall=%s",
						ipv4HashedNamespace,
						cidr,
						ns),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --id=@%s-10000 create acl priority=10000 direction=%s match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && ip4.dst != %s\" action=drop external-ids:egressFirewall=%s -- add logical_switch %s acls @%s-10000",
						nodeName,
						types.DirectionToLPort,
						ipv4HashedNamespace,
						cidr,
						ns,
						nodeName,
						nodeName),
					Output: "",
				},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"EnsureAddressSet", []string{"string"}, []interface{}{nil}, 0, 1},
			},
			kubeMocksOpsHelper: []ovntest.TestifyMockHelper{
				{"UpdateEgressFirewall", []string{"*v1.EgressFirewall"}, []interface{}{nil}, 0, 1},
			},
		},
		{
			name:               "create basic egressFirewall in Shared GW mode ipv4 only",
			ipv4Mode:           true,
			ipv6Mode:           false,
			isLocalGatewayMode: false,
			ef: &efapi.EgressFirewall{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: ns,
				},
				Spec: efapi.EgressFirewallSpec{
					Egress: []efapi.EgressFirewallRule{
						{
							Type: efapi.EgressFirewallRuleDeny,
							To: efapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					},
				},
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && inport == \\\"%s%s\\\"\" action=drop external-ids:egressFirewall=%s",
						ipv4HashedNamespace,
						types.JoinSwitchToGWRouterPrefix,
						types.OVNClusterRouter,
						ns),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --id=@join-10000 create acl priority=10000 direction=%s match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && inport == \\\"%s%s\\\"\" action=drop external-ids:egressFirewall=%s -- add logical_switch %s acls @join-10000",
						types.DirectionToLPort,
						ipv4HashedNamespace,
						types.JoinSwitchToGWRouterPrefix,
						types.OVNClusterRouter,
						ns,
						types.OVNJoinSwitch),

					Output: "",
				},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"EnsureAddressSet", []string{"string"}, []interface{}{nil}, 0, 1},
			},
			kubeMocksOpsHelper: []ovntest.TestifyMockHelper{
				{"UpdateEgressFirewall", []string{"*v1.EgressFirewall"}, []interface{}{nil}, 0, 1},
			},
		},
		{
			name:               "creates an egressFirewall denying udp traffic on port 100 in local GW mode",
			ipv4Mode:           true,
			ipv6Mode:           false,
			isLocalGatewayMode: true,
			ef: &efapi.EgressFirewall{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: ns,
				},
				Spec: efapi.EgressFirewallSpec{
					Egress: []efapi.EgressFirewallRule{
						{
							Type: efapi.EgressFirewallRuleDeny,
							Ports: []efapi.EgressFirewallPort{
								{
									Protocol: "UDP",
									Port:     100,
								},
							},
							To: efapi.EgressFirewallDestination{
								CIDRSelector: "0.0.0.0/0",
							},
						},
					},
				},
			},
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && ((udp && ( udp.dst == 100 ))) && ip4.dst != 1.0.0.0/10\" action=drop external-ids:egressFirewall=%s",
						ipv4HashedNamespace,
						ns),
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --id=@%s-10000 create acl priority=10000 direction=%s match=\"(ip4.dst == 0.0.0.0/0) && ip4.src == $%s && ((udp && ( udp.dst == 100 ))) && ip4.dst != 1.0.0.0/10\" action=drop external-ids:egressFirewall=%s -- add logical_switch %s acls @%s-10000",
						nodeName,
						types.DirectionToLPort,
						ipv4HashedNamespace,
						ns,
						nodeName,
						nodeName),
				},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"EnsureAddressSet", []string{"string"}, []interface{}{nil}, 0, 1},
			},
			kubeMocksOpsHelper: []ovntest.TestifyMockHelper{
				{"UpdateEgressFirewall", []string{"*v1.EgressFirewall"}, []interface{}{nil}, 0, 1},
			},
		},
		{
			name:               "deletes an egressFirewall shared GW Mode",
			ipv4Mode:           true,
			ipv6Mode:           false,
			isLocalGatewayMode: false,
			ef:                 nil,
			egressFirewall: &egressFirewall{
				name:      "default",
				namespace: "testing",
			},
			ovnCmd: []ovntest.ExpectedCmd{

				{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:egressFirewall=testing",
					Output: efACLToDelete,
				},
				{
					Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch %s acls a08ea426-2288-11eb-a30b-a8a1590cda30", types.OVNJoinSwitch),
					Output: "",
				},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
		},
		{
			name:               "deletes an egressFirewall local GW Mode",
			ipv4Mode:           true,
			ipv6Mode:           false,
			isLocalGatewayMode: true,
			ef:                 nil,
			egressFirewall: &egressFirewall{
				name:      "default",
				namespace: "testing",
			},
			ovnCmd: []ovntest.ExpectedCmd{

				{
					Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:egressFirewall=%s", ns),
					Output: efACLToDelete,
				},
				{
					Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch %s acls %s", nodeName, efACLToDelete),
					Output: "",
				},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
		},
	}

	clusterSubnets, _ := config.ParseClusterSubnetEntries(cidr)

	config.Default.ClusterSubnets = append(config.Default.ClusterSubnets, clusterSubnets...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDnsOps := new(util_mocks.DNSOps)
			util.SetDNSLibOpsMockInst(mockDnsOps)

			for _, item := range tt.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			config.IPv4Mode = tt.ipv4Mode
			config.IPv6Mode = tt.ipv6Mode
			config.Gateway.Mode = config.GatewayModeLocal
			if !tt.isLocalGatewayMode {
				config.Gateway.Mode = config.GatewayModeShared
			}

			controller := newController()
			if tt.ef != nil {
				controller.efStore.Add(tt.ef)
			}
			if tt.egressFirewall != nil {
				controller.egressFirewalls.Store(tt.egressFirewall.namespace, tt.egressFirewall)
			}
			fexec := ovntest.NewLooseCompareFakeExec()
			for _, cmd := range tt.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)
			}

			for _, item := range tt.addressSetFactoryOpsHelper {
				call := controller.addrsetFactoryMocks.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tt.kubeMocksOpsHelper {
				call := controller.kubeMocks.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			controller.syncEgressFirewalls("testing/default")

			if !fexec.CalledMatchesExpected() {
				t.Error(fexec.ErrorDesc())
			}
		})
	}
}

func TestMatchFunction(t *testing.T) {
	tests := []struct {
		name         string
		sourceIPv4   string
		sourceIPv6   string
		internalCIDR string
		destinations []matchTarget
		ports        []efapi.EgressFirewallPort
		output       string
	}{
		{
			name:         "generate match string ipv4 no ports ipv4 cidr destination only",
			sourceIPv4:   ipv4Source,
			sourceIPv6:   "",
			internalCIDR: "10.128.0.0/14",
			destinations: []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
			ports:        nil,
			output:       `match="(ip4.dst == 1.2.3.4/32) && ip4.src == $` + ipv4Source + ` && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
		{
			name:         "generate match string dual stack, no ports, ipv4 cidr destination only",
			internalCIDR: "10.128.0.0/14",
			sourceIPv4:   ipv4Source,
			sourceIPv6:   ipv6Source,
			destinations: []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
			ports:        nil,
			output:       `match="(ip4.dst == 1.2.3.4/32) && (ip4.src == $` + ipv4Source + ` || ip6.src == $` + ipv6Source + `) && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
		{
			name:         "generate match string dual stack, no ports, ipv4 and ipv6 addressset destions",
			internalCIDR: "10.128.0.0/14",
			sourceIPv4:   ipv4Source,
			sourceIPv6:   ipv6Source,
			destinations: []matchTarget{{matchKindV4AddressSet, "destv4"}, {matchKindV6AddressSet, "destv6"}},
			ports:        nil,
			output:       `match="(ip4.dst == $destv4 || ip6.dst == $destv6) && (ip4.src == $` + ipv4Source + ` || ip6.src == $` + ipv6Source + `) && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
		{
			name:         "generate match ipv4, no ports,  addressset destination only",
			internalCIDR: "10.128.0.0/14",
			sourceIPv4:   ipv4Source,
			sourceIPv6:   "",
			destinations: []matchTarget{{matchKindV4AddressSet, "destv4"}},
			ports:        nil,
			output:       `match="(ip4.dst == $destv4) && ip4.src == $` + ipv4Source + ` && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
		{
			name:         "generate match dual stack no ports ipv6 destination cidr",
			internalCIDR: "10.128.0.0/14",
			sourceIPv4:   ipv4Source,
			sourceIPv6:   ipv6Source,
			destinations: []matchTarget{{matchKindV6CIDR, "2001::/64"}},
			ports:        nil,
			output:       `match="(ip6.dst == 2001::/64) && (ip4.src == $` + ipv4Source + ` || ip6.src == $` + ipv6Source + `) && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
		{
			name:         "ipv6 only no ports, addressset destination",
			internalCIDR: "2002:0:0:1234::/64",
			sourceIPv4:   "",
			sourceIPv6:   ipv6Source,
			destinations: []matchTarget{{matchKindV6AddressSet, "destv6"}},
			ports:        nil,
			output:       `match="(ip6.dst == $destv6) && ip6.src == $` + ipv6Source + ` && inport == \"` + types.JoinSwitchToGWRouterPrefix + types.OVNClusterRouter + `\""`,
		},
	}
	for _, tt := range tests {
		config.IPv4Mode = false
		config.IPv6Mode = false

		t.Run(tt.name, func(t *testing.T) {
			config.Gateway.Mode = "shared"
			if len(tt.sourceIPv4) > 0 {
				config.IPv4Mode = true
			}
			if len(tt.sourceIPv6) > 0 {
				config.IPv6Mode = true
			}
			_, cidr, _ := net.ParseCIDR(tt.internalCIDR)
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: cidr}}
			matchExpression := generateMatch(tt.sourceIPv4, tt.sourceIPv6, tt.destinations, tt.ports)
			if matchExpression != tt.output {
				t.Errorf("matchExpression does not produce the expected output \n\texpected: %s\n\treturned: %s\n", tt.output, matchExpression)
			}

		})
	}

}

func TestEgressFirewallParsing(t *testing.T) {
	tests := []struct {
		name               string
		egressFirewallRule efapi.EgressFirewallRule
		id                 int
		err                bool
		errOutput          string
		output             egressFirewallRule
	}{
		{
			name: "clean parsing no errors",
			egressFirewallRule: efapi.EgressFirewallRule{
				Type: efapi.EgressFirewallRuleAllow,
				To:   efapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
			},
			id:  1,
			err: false,
			output: egressFirewallRule{
				id:     1,
				access: efapi.EgressFirewallRuleAllow,
				to:     destination{cidrSelector: "1.2.3.4/32"},
			},
		},
		{
			name: "attempting to parse an invalid CIDR",
			egressFirewallRule: efapi.EgressFirewallRule{
				Type: efapi.EgressFirewallRuleAllow,
				To:   efapi.EgressFirewallDestination{CIDRSelector: "1.2.3./32"},
			},
			id:        1,
			err:       true,
			errOutput: "invalid CIDR address: 1.2.3./32",
			output:    egressFirewallRule{},
		},
		{
			name: "clean parsing ipv6",
			egressFirewallRule: efapi.EgressFirewallRule{
				Type: efapi.EgressFirewallRuleAllow,
				To:   efapi.EgressFirewallDestination{CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
			},
			id:  2,
			err: false,
			output: egressFirewallRule{
				id:     2,
				access: efapi.EgressFirewallRuleAllow,
				to:     destination{cidrSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := newEgressFirewallRule(tt.egressFirewallRule, tt.id)
			if err != nil && len(tt.errOutput) > 0 {
				if err.Error() != tt.errOutput {
					t.Errorf("test case expected an error but did not throw the expected on \n\texpected: %v\n\treturned: %v\n", tt.errOutput, err.Error())
				}
				return

			}

			if !reflect.DeepEqual(tt.output, *output) {
				t.Errorf("Error parsing egressFirewall rule the output does not match expected output\n\texpected: %v\n\treturned: %v\n",
					output,
					tt.output,
				)
			}
		})
	}

}
