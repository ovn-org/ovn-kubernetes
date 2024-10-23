package util

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	annotatorMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestL3GatewayConfig_MarshalJSON(t *testing.T) {
	vlanid := uint(1024)
	tests := []struct {
		desc       string
		inpL3GwCfg *L3GatewayConfig
		expOutput  []byte
	}{
		{
			desc:       "test empty config i.e gateway mode disabled by default",
			inpL3GwCfg: &L3GatewayConfig{},
			expOutput:  []byte(`{"mode":""}`),
		},
		{
			desc: "test gateway mode set to local and verify that node-port-enable is set to false by default",
			inpL3GwCfg: &L3GatewayConfig{
				Mode: config.GatewayModeLocal,
			},
			expOutput: []byte(`{"mode":"local","node-port-enable":"false"}`),
		},
		{
			desc: "test VLANID not nil",
			inpL3GwCfg: &L3GatewayConfig{
				Mode:   config.GatewayModeShared,
				VLANID: &vlanid,
			},
			expOutput: []byte(`{"mode":"shared","node-port-enable":"false","vlan-id":"1024"}`),
		},
		{
			desc: "test single IP address and single next hop path",
			inpL3GwCfg: &L3GatewayConfig{
				Mode:        config.GatewayModeLocal,
				VLANID:      &vlanid,
				IPAddresses: []*net.IPNet{ovntest.MustParseIPNet("192.168.1.10/24")},
				NextHops:    []net.IP{ovntest.MustParseIP("192.168.1.1")},
			},
			expOutput: []byte(`{"mode":"local","ip-addresses":["192.168.1.10/24"],"ip-address":"192.168.1.10/24","next-hops":["192.168.1.1"],"next-hop":"192.168.1.1","node-port-enable":"false","vlan-id":"1024"}`),
		},
		{
			desc: "test multiple IP address and multiple next hop paths",
			inpL3GwCfg: &L3GatewayConfig{
				Mode:        config.GatewayModeLocal,
				VLANID:      &vlanid,
				BridgeID:    "BRIDGE-ID",
				InterfaceID: "INTERFACE-ID",
				MACAddress:  ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses: []*net.IPNet{
					ovntest.MustParseIPNet("192.168.1.10/24"),
					ovntest.MustParseIPNet("fd01::1234/64"),
				},
				NextHops: []net.IP{
					ovntest.MustParseIP("192.168.1.1"),
					ovntest.MustParseIP("fd01::1"),
				},
			},
			expOutput: []byte(`{"mode":"local","bridge-id":"BRIDGE-ID","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-addresses":["192.168.1.10/24","fd01::1234/64"],"next-hops":["192.168.1.1","fd01::1"],"node-port-enable":"false","vlan-id":"1024"}`),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := tc.inpL3GwCfg.MarshalJSON()
			t.Log(string(res), e)
			assert.True(t, reflect.DeepEqual(res, tc.expOutput))
		})
	}
}

func TestL3GatewayConfig_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		desc       string
		expOut     L3GatewayConfig
		inputParam []byte
		errAssert  bool
		errMatch   error
	}{
		{
			desc:       "error: test bad input causing json Unmarshal error",
			errAssert:  true,
			inputParam: []byte(`{`),
		},
		{
			desc:       "success: test gateway mode disabled path",
			inputParam: []byte(`{"mode":""}`),
			expOut: L3GatewayConfig{
				Mode:           "",
				NodePortEnable: false,
			},
		},
		{
			desc:       "error: test unsupported gateway mode",
			inputParam: []byte(`{"mode":"blah"}`),
			errMatch:   fmt.Errorf("bad 'mode' value"),
		},
		{
			desc:       "error: test bad VLANID input",
			inputParam: []byte(`{"mode":"shared","vlan-id":"A"}`),
			errMatch:   fmt.Errorf("bad 'vlan-id' value"),
		},
		{
			desc:       "success: test valid VLANID input",
			inputParam: []byte(`{"mode":"shared","vlan-id":"223"}`),
			expOut: L3GatewayConfig{
				Mode:           "shared",
				NodePortEnable: false,
				VLANID:         &[]uint{223}[0],
			},
		},
		{
			desc:       "success: test host gateway bridge parsing",
			inputParam: []byte(`{"mode":"shared","exgw-interface-id":"breth0_ovn-control-plane"}`),
			expOut: L3GatewayConfig{
				Mode:                "shared",
				EgressGWInterfaceID: "breth0_ovn-control-plane",
			},
		},
		{
			desc:       "test bad MAC address value",
			inputParam: []byte(`{"mode":"local","mac-address":"BADMAC"}`),
			errMatch:   fmt.Errorf("bad 'mac-address' value"),
		},
		{
			desc:       "test bad 'IP address' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1/24"}`),
			errMatch:   fmt.Errorf("bad 'ip-address' value"),
		},
		{
			desc:       "test valid 'IP address' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.5/24"}`),
			expOut: L3GatewayConfig{
				Mode:        "local",
				MACAddress:  ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses: ovntest.MustParseIPNets("192.168.1.5/24"),
				NextHops:    []net.IP{},
			},
		},
		{
			desc:       "test bad 'IP addresses' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-addresses":["192.168.1/24","fd01::1234/64"]}`),
			errMatch:   fmt.Errorf("bad 'ip-addresses' value"),
		},
		{
			desc:       "test valid 'IP addresses' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-addresses":["192.168.1.5/24","fd01::1234/64"]}`),
			expOut: L3GatewayConfig{
				Mode:        "local",
				MACAddress:  ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses: ovntest.MustParseIPNets("192.168.1.5/24", "fd01::1234/64"),
				NextHops:    []net.IP{},
			},
		},
		{
			desc:       "test bad 'next-hops' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.5/24", "next-hops":["192.168.1.","fd01::1"]}`),
			errMatch:   fmt.Errorf("bad 'next-hops' value"),
		},
		{
			desc:       "test valid 'next-hops' value",
			inputParam: []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.5/24", "next-hops":["192.168.1.1","fd01::1"]}`),
			expOut: L3GatewayConfig{
				Mode:        "local",
				MACAddress:  ovntest.MustParseMAC("11:22:33:44:55:66"),
				IPAddresses: ovntest.MustParseIPNets("192.168.1.5/24"),
				NextHops: []net.IP{
					ovntest.MustParseIP("192.168.1.1"),
					ovntest.MustParseIP("fd01::1"),
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			l3GwCfg := L3GatewayConfig{}
			e := l3GwCfg.UnmarshalJSON(tc.inputParam)
			if tc.errAssert {
				t.Log(e)
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				t.Log(l3GwCfg)
				assert.Equal(t, l3GwCfg, tc.expOut)
			}
		})
	}
}

func TestSetL3GatewayConfig(t *testing.T) {
	mockAnnotator := new(annotatorMock.Annotator)

	tests := []struct {
		desc                   string
		inpNodeAnnotator       kube.Annotator
		inputL3GwCfg           L3GatewayConfig
		errExpected            bool
		onRetArgsAnnotatorList []ovntest.TestifyMockHelper
	}{
		{
			desc:             "success: empty L3GatewayConfig applied should pass",
			inpNodeAnnotator: mockAnnotator,
			inputL3GwCfg:     L3GatewayConfig{},
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:             "test error path when setting gateway annotation",
			inpNodeAnnotator: mockAnnotator,
			inputL3GwCfg:     L3GatewayConfig{},
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
		{
			desc:             "success: apply empty Chassis id",
			inpNodeAnnotator: mockAnnotator,
			inputL3GwCfg: L3GatewayConfig{
				ChassisID: " ",
			},
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:             "test error path when applying Chassis ID",
			inpNodeAnnotator: mockAnnotator,
			inputL3GwCfg: L3GatewayConfig{
				ChassisID: "testid",
			},
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "interface{}"}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.onRetArgsAnnotatorList {
				call := mockAnnotator.On(item.OnCallMethodName)
				for range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.Anything)
				}

				for _, e := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, e)
				}
				call.Once()
			}
			e := SetL3GatewayConfig(tc.inpNodeAnnotator, &tc.inputL3GwCfg)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			}
			mockAnnotator.AssertExpectations(t)
		})
	}
}

func TestParseNodeL3GatewayAnnotation(t *testing.T) {
	tests := []struct {
		desc      string
		inpNode   *v1.Node
		errAssert bool
		errMatch  error
	}{
		{
			desc:      "error: annotation not found for node",
			inpNode:   &v1.Node{},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation not found for node", OvnNodeL3GatewayConfig),
		},
		{
			desc: "error: fail to unmarshal l3 gateway config annotations",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac_address":"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("failed to unmarshal l3 gateway config annotation"),
		},
		{
			desc: "error: annotation for network not found",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"nondefault":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation for %s network not found", OvnNodeL3GatewayConfig, ovnDefaultNetworkGateway),
		},
		{
			desc: "error: nod chassis ID annotation not found",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation not found", OvnNodeChassisID),
		},
		{
			desc: "success: parse completed",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
						"k8s.ovn.org/node-chassis-id":   "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeL3GatewayAnnotation(tc.inpNode)
			if tc.errAssert {
				t.Log(e)
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				assert.NotNil(t, cfg)
			}
		})
	}
}

func TestNodeL3GatewayAnnotationChanged(t *testing.T) {
	tests := []struct {
		desc    string
		oldNode *v1.Node
		newNode *v1.Node
		result  bool
	}{
		{
			desc: "true: annotation changed",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			result: true,
		},
		{
			desc: "true: annotation's node IP field changed",
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.3/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			result: true,
		},
		{
			desc: "false: annotation didn't change",
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			result: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			result := NodeL3GatewayAnnotationChanged(tc.oldNode, tc.newNode)
			assert.Equal(t, tc.result, result)
		})
	}
}

func TestParseNodeManagementPortMACAddresses(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     v1.Node
		errExpected bool
		expOutput   bool
		netName     string
	}{
		{
			desc:      "mac address annotation not found for node, however, does not return error",
			inpNode:   v1.Node{},
			expOutput: false,
		},
		{
			desc: "success: parse mac address for given netName",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:e5\",\"blue\":\"d6:bc:85:32:30:fb\",\"red\":\"4a:ea:1d:8d:8f:8c\"}"},
				},
			},
			expOutput: true,
			netName:   types.DefaultNetworkName,
		},
		{
			desc: "error: parse mac address error",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:\",\"blue\":\"1\",\"red\":\"2\"}"},
				},
			},
			errExpected: true,
			netName:     types.DefaultNetworkName,
		},
		{
			desc: "error: parse mac address error since value of secondary network is invalid",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:\",\"blue\":\"1\",\"red\":\"2\"}"},
				},
			},
			errExpected: true,
			netName:     "blue",
		},
		{
			desc: "error: parse mac address error since network doesn't exist on the annotation",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:\",\"blue\":\"1\",\"red\":\"2\"}"},
				},
			},
			errExpected: true,
			netName:     "yello",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeManagementPortMACAddresses(&tc.inpNode, tc.netName)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
				assert.Nil(t, cfg)
			}
			if tc.expOutput {
				assert.NotNil(t, cfg)
			}
		})
	}
}

func TestParseNodeGatewayRouterLRPAddr(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     v1.Node
		errExpected bool
		expOutput   bool
	}{
		{
			desc:      "Gateway router LPR IP address annotation not found for node, however, does not return error",
			inpNode:   v1.Node{},
			expOutput: false,
		},
		{
			desc: "success: Gateway router parse LPR IP address",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddr": `{"ipv4":"100.64.0.5/16"}`},
				},
			},
			expOutput: true,
		},
		{
			desc: "success: Gateway router parse LPR IP address dual stack",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddr": `{"ipv4":"100.64.0.5/16", "ipv6":"fd:98::/64"}`},
				},
			},
			expOutput: true,
		},
		{
			desc: "error: Gateway router parse LPR IP address error",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddr": `{"ipv4":"100.64.0.5"}`},
				},
			},
			errExpected: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeGatewayRouterLRPAddr(&tc.inpNode)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
				assert.Nil(t, cfg)
			}
			if tc.expOutput {
				assert.NotNil(t, cfg)
			}
		})
	}
}

func TestParseNodeGatewayRouterJoinAddrs(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     v1.Node
		netName     string
		errExpected bool
		expOutput   bool
	}{
		{
			desc:      "Gateway router LPR IP address annotation not found for node, however, does not return error",
			inpNode:   v1.Node{},
			expOutput: false,
		},
		{
			desc: "success: Gateway router parse LPR IP address",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":{"ipv4":"100.64.0.4/16"}}`},
				},
			},
			netName:   types.DefaultNetworkName,
			expOutput: true,
		},
		{
			desc: "success: Gateway router parse LPR IP address dual stack",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":{"ipv4":"100.64.0.5/16","ipv6":"fd:98::/64"}}`},
				},
			},
			netName:   types.DefaultNetworkName,
			expOutput: true,
		},
		{
			desc: "success: Gateway router parse LPR IP address dual stack for the right network name",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":{"ipv4":"100.64.0.5/16","ipv6":"fd:98::/64"},"l3-network":{"ipv4":"100.65.0.5/16","ipv6":"fd:99::/64"}}`},
				},
			},
			netName:   "l3-network",
			expOutput: true,
		},
		{
			desc: "error: Gateway router parse LPR IP address dual stack cannot find the requested network name",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":{"ipv4":"100.64.0.5/16","ipv6":"fd:98::/64"},"l3-network":{"ipv4":"100.65.0.5/16","ipv6":"fd:99::/64"}}`},
				},
			},
			netName:     "l2-network",
			errExpected: true,
		},
		{
			desc: "error: Gateway router parse LPR IP address error",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":{"ipv4":"100.64.0.5"}}`},
				},
			},
			netName:     types.DefaultNetworkName,
			errExpected: true,
		},
	}
	config.IPv4Mode = true
	config.IPv6Mode = true
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeGatewayRouterJoinAddrs(&tc.inpNode, tc.netName)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
				assert.Nil(t, cfg)
			}
			if tc.expOutput {
				assert.NotNil(t, cfg)
			}
		})
	}
}

func TestCreateNodeGatewayRouterLRPAddrsAnnotation(t *testing.T) {
	tests := []struct {
		desc            string
		inpDefSubnetIps []*net.IPNet
		outExp          map[string]string
		errExp          bool
	}{
		{
			desc:            "success path, valid default subnets",
			inpDefSubnetIps: ovntest.MustParseIPNets("192.168.1.12/24"),
			outExp: map[string]string{
				"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": "{\"default\":{\"ipv4\":\"192.168.1.12/24\"}}",
			},
		},
		{
			desc:            "success path, valid default dualstack subnets",
			inpDefSubnetIps: ovntest.MustParseIPNets("192.168.1.12/24", "fd:98::5/64"),
			outExp: map[string]string{
				"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": "{\"default\":{\"ipv4\":\"192.168.1.12/24\",\"ipv6\":\"fd:98::5/64\"}}",
			},
		},
		{
			desc:   "success path, inpDefSubnetIps is nil",
			outExp: map[string]string{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := UpdateNodeGatewayRouterLRPAddrsAnnotation(nil, tc.inpDefSubnetIps, types.DefaultNetworkName)
			t.Log(res, err)
			if tc.errExp {
				assert.NotNil(t, err)
			} else {
				assert.True(t, reflect.DeepEqual(res, tc.outExp))
			}
		})
	}
}

func TestSetGatewayMTUSupport(t *testing.T) {
	mockAnnotator := new(annotatorMock.Annotator)

	tests := []struct {
		desc                   string
		inpNodeAnnotator       kube.Annotator
		inputSet               bool
		errExpected            bool
		onRetArgsAnnotatorList []ovntest.TestifyMockHelper
	}{
		{
			desc:             "success: set true should delete annotation on node",
			inpNodeAnnotator: mockAnnotator,
			inputSet:         true,
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Delete", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil}},
			},
		},
		{
			desc:             "success: set false should create annotation with value 'false'",
			inpNodeAnnotator: mockAnnotator,
			inputSet:         false,
			onRetArgsAnnotatorList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Set", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{nil}},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.onRetArgsAnnotatorList {
				call := mockAnnotator.On(item.OnCallMethodName)
				for range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.Anything)
				}

				for _, e := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, e)
				}
				call.Once()
			}
			e := SetGatewayMTUSupport(tc.inpNodeAnnotator, tc.inputSet)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			}
			mockAnnotator.AssertExpectations(t)
		})
	}
}

func TestParseNodeGatewayMTUSupport(t *testing.T) {
	tests := []struct {
		desc    string
		inpNode *v1.Node
		res     bool
	}{
		{
			desc:    "annotation not found for node and true",
			inpNode: &v1.Node{},
			res:     true,
		},
		{
			desc: "parse completed and true",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/gateway-mtu-support": "true",
					},
				},
			},
			res: true,
		},
		{
			desc: "parse completed and false",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/gateway-mtu-support": "false",
					},
				},
			},
			res: false,
		},
		{
			desc: "parse invalid value completed and true",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/gateway-mtu-support": "tru",
					},
				},
			},
			res: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := ParseNodeGatewayMTUSupport(tc.inpNode)
			assert.Equal(t, tc.res, res)
		})
	}
}

func TestGetNetworkID(t *testing.T) {
	tests := []struct {
		desc              string
		nodes             []*corev1.Node
		netInfo           NetInfo
		expectedError     error
		expectedNetworkID int
	}{
		{
			desc:              "with no nodes should return and error and invalid network ID",
			netInfo:           newDummyNetInfo("rednamespace", "bluenet"),
			expectedError:     fmt.Errorf("missing network id for network 'bluenet'"),
			expectedNetworkID: InvalidID,
		},
		{
			desc: "with bad network ID annotations should return and error and invalid network ID",
			nodes: []*corev1.Node{
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							"k8s.ovn.org/network-ids": "not a map",
						},
					},
				},
			},
			netInfo:           newDummyNetInfo("rednamespace", "bluenet"),
			expectedError:     fmt.Errorf("could not parse"),
			expectedNetworkID: InvalidID,
		},
		{
			desc: "with multiple networks annotation should return expected network ID and no error",
			nodes: []*corev1.Node{
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							"k8s.ovn.org/network-ids": `{"rednet": "5"}`,
						},
					},
				},
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							"k8s.ovn.org/network-ids": `{"yellownet": "6", "bluenet": "3"}`,
						},
					},
				},
			},
			netInfo:           newDummyNetInfo("rednamespace", "bluenet"),
			expectedNetworkID: 3,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			obtainedNetworkID, obtainedError := GetNetworkID(tc.nodes, tc.netInfo)
			if tc.expectedError != nil {
				assert.Contains(t, obtainedError.Error(), tc.expectedError.Error())
			} else {
				assert.NoError(t, obtainedError)
			}
			assert.Equal(t, obtainedNetworkID, tc.expectedNetworkID)
		})
	}
}

func TestParseUDNLayer2NodeGRLRPTunnelIDs(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     *v1.Node
		inpNetName  string
		res         int
		errExpected bool
	}{
		{
			desc:       "annotation not found for node and invalidID",
			inpNode:    &v1.Node{},
			inpNetName: "rednet",
			res:        -1,
		},
		{
			desc: "parse completed and validID",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `{"rednet":"5"}`,
					},
				},
			},
			inpNetName:  "rednet",
			errExpected: false,
			res:         5,
		},
		{
			desc: "parse completed and invalid value",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `blah`,
					},
				},
			},
			errExpected: true,
			inpNetName:  "rednet",
			res:         -1,
		},
		{
			desc: "multiple networks; parse completed and validID",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `{"rednet":"5", "bluenet":"8"}`,
					},
				},
			},
			inpNetName:  "bluenet",
			errExpected: false,
			res:         8,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := ParseUDNLayer2NodeGRLRPTunnelIDs(tc.inpNode, tc.inpNetName)
			if tc.errExpected {
				t.Log(err)
				assert.Error(t, err)
			}
			assert.Equal(t, tc.res, res)
		})
	}
}
