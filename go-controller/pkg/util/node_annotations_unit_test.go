// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	annotatorMock "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
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
				assert.Equal(t, tc.expOut, l3GwCfg)
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
				require.Error(t, e)
			}
			mockAnnotator.AssertExpectations(t)
		})
	}
}

func TestParseNodeL3GatewayAnnotation(t *testing.T) {
	tests := []struct {
		desc      string
		inpNode   *corev1.Node
		errAssert bool
		errMatch  error
	}{
		{
			desc:      "error: annotation not found for node",
			inpNode:   &corev1.Node{},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation not found for node", OvnNodeL3GatewayConfig),
		},
		{
			desc: "error: fail to unmarshal l3 gateway config annotations",
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac_address":"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("failed to unmarshal l3 gateway config annotation"),
		},
		{
			desc: "error: annotation for network not found",
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"nondefault":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation for %s network not found", OvnNodeL3GatewayConfig, ovnDefaultNetworkGateway),
		},
		{
			desc: "error: nod chassis ID annotation not found",
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`},
				},
			},
			errAssert: true,
			errMatch:  fmt.Errorf("%s annotation not found", OvnNodeChassisID),
		},
		{
			desc: "success: parse completed",
			inpNode: &corev1.Node{
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
		oldNode *corev1.Node
		newNode *corev1.Node
		result  bool
	}{
		{
			desc: "true: annotation changed",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			newNode: &corev1.Node{
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
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.3/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			oldNode: &corev1.Node{
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
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.255.33.2/24", "next-hop":"169.255.33.1"}}`,
					},
				},
			},
			oldNode: &corev1.Node{
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
		inpNode     corev1.Node
		errExpected bool
		expOutput   bool
		netName     string
	}{
		{
			desc:      "mac address annotation not found for node, however, does not return error",
			inpNode:   corev1.Node{},
			expOutput: false,
		},
		{
			desc: "success: parse mac address for given netName",
			inpNode: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:e5\",\"blue\":\"d6:bc:85:32:30:fb\",\"red\":\"4a:ea:1d:8d:8f:8c\"}"},
				},
			},
			expOutput: true,
			netName:   types.DefaultNetworkName,
		},
		{
			desc: "error: parse mac address error",
			inpNode: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:\",\"blue\":\"1\",\"red\":\"2\"}"},
				},
			},
			errExpected: true,
			netName:     types.DefaultNetworkName,
		},
		{
			desc: "error: parse mac address error since value of secondary network is invalid",
			inpNode: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-addresses": "{\"default\":\"96:8f:e8:25:a2:\",\"blue\":\"1\",\"red\":\"2\"}"},
				},
			},
			errExpected: true,
			netName:     "blue",
		},
		{
			desc: "error: parse mac address error since network doesn't exist on the annotation",
			inpNode: corev1.Node{
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
				require.Error(t, e)
				assert.Nil(t, cfg)
			}
			if tc.expOutput {
				assert.NotNil(t, cfg)
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
				require.Error(t, e)
			}
			mockAnnotator.AssertExpectations(t)
		})
	}
}

func TestParseNodeGatewayMTUSupport(t *testing.T) {
	tests := []struct {
		desc    string
		inpNode *corev1.Node
		res     bool
	}{
		{
			desc:    "annotation not found for node and true",
			inpNode: &corev1.Node{},
			res:     true,
		},
		{
			desc: "parse completed and true",
			inpNode: &corev1.Node{
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
			inpNode: &corev1.Node{
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
			inpNode: &corev1.Node{
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

func TestGetNodeID(t *testing.T) {
	nodeWithID := func(id string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node1",
				Annotations: map[string]string{"k8s.ovn.org/node-id": id},
			},
		}
	}

	tests := []struct {
		desc        string
		inpNode     *corev1.Node
		res         int
		errExpected bool
	}{
		{
			desc:        "annotation not set",
			inpNode:     &corev1.Node{},
			res:         InvalidNodeID,
			errExpected: true,
		},
		{
			desc:    "valid id",
			inpNode: nodeWithID("5"),
			res:     5,
		},
		{
			desc:    "highest valid id",
			inpNode: nodeWithID(fmt.Sprintf("%d", types.MaxNodeIDs)),
			res:     types.MaxNodeIDs,
		},
		{
			desc:        "not an integer",
			inpNode:     nodeWithID("blah"),
			res:         InvalidNodeID,
			errExpected: true,
		},
		{
			desc:        "negative id",
			inpNode:     nodeWithID("-2"),
			res:         InvalidNodeID,
			errExpected: true,
		},
		{
			desc:        "id above the maximum",
			inpNode:     nodeWithID(fmt.Sprintf("%d", types.MaxNodeIDs+1)),
			res:         InvalidNodeID,
			errExpected: true,
		},
		{
			desc:        "id large enough to exhaust the allocator bitmap",
			inpNode:     nodeWithID("1099511627776"),
			res:         InvalidNodeID,
			errExpected: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := GetNodeID(tc.inpNode)
			if tc.errExpected {
				t.Log(err)
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.res, res)
		})
	}
}

func TestParseUDNLayer2NodeGRLRPTunnelIDs(t *testing.T) {
	tests := []struct {
		desc string
		// notSetExpected additionally requires the error to report the
		// annotation as unset, so that callers allocate a replacement.
		notSetExpected bool
		inpNode        *corev1.Node
		inpNetName     string
		res            int
		errExpected    bool
	}{
		{
			desc:       "annotation not found for node and invalidID",
			inpNode:    &corev1.Node{},
			inpNetName: "rednet",
			res:        -1,
		},
		{
			desc: "parse completed and validID",
			inpNode: &corev1.Node{
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
			inpNode: &corev1.Node{
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
			inpNode: &corev1.Node{
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
		{
			desc:           "negative ID that is not InvalidID",
			notSetExpected: true,
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `{"rednet":"-2"}`,
					},
				},
			},
			inpNetName:  "rednet",
			errExpected: true,
			res:         -1,
		},
		{
			desc:           "zero is the reserved NoTunnelID and read as unset",
			notSetExpected: true,
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `{"rednet":"0"}`,
					},
				},
			},
			inpNetName:  "rednet",
			errExpected: true,
			res:         -1,
		},
		{
			desc:           "ID at the top of the allocator range",
			notSetExpected: true,
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": fmt.Sprintf(`{"rednet":"%d"}`, types.MaxLogicalPortTunnelKey),
					},
				},
			},
			inpNetName:  "rednet",
			errExpected: true,
			res:         -1,
		},
		{
			desc:           "ID large enough to exhaust the allocator bitmap",
			notSetExpected: true,
			inpNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": `{"rednet":"1099511627776"}`,
					},
				},
			},
			inpNetName:  "rednet",
			errExpected: true,
			res:         -1,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := ParseUDNLayer2NodeGRLRPTunnelIDs(tc.inpNode, tc.inpNetName)
			if tc.errExpected {
				t.Log(err)
				require.Error(t, err)
			}
			if tc.notSetExpected {
				require.True(t, IsAnnotationNotSetError(err))
			}
			assert.Equal(t, tc.res, res)
			// callers choose between reading the ID and allocating one based on
			// HasUDNLayer2NodeGRLRPTunnelID, so it has to agree with the parse
			assert.Equal(t, err == nil, HasUDNLayer2NodeGRLRPTunnelID(tc.inpNode, tc.inpNetName))
		})
	}
}

func TestNodeDontSNATSubnetAnnotationChanged(t *testing.T) {
	tests := []struct {
		desc    string
		oldNode *corev1.Node
		newNode *corev1.Node
		result  bool
	}{
		{
			desc: "annotation added",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24"]`,
					},
				},
			},
			result: true,
		},
		{
			desc: "annotation removed",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24"]`,
					},
				},
			},
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			result: true,
		},
		{
			desc: "annotation value changed",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24"]`,
					},
				},
			},
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["10.0.0.0/16"]`,
					},
				},
			},
			result: true,
		},
		{
			desc: "false: annotation unchanged",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24"]`,
					},
				},
			},
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24"]`,
					},
				},
			},
			result: false,
		},
		{
			desc: "annotation absent in both",
			oldNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			newNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			result: false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			result := NodeDontSNATSubnetAnnotationChanged(tc.oldNode, tc.newNode)
			assert.Equal(t, tc.result, result)
		})
	}
}

func TestCloudEgressIPConfigAnnotationChanged(t *testing.T) {
	v4Only := `[{"interface":"eth0","ifaddr":{"ipv4":"192.168.126.12/24"},"capacity":{}}]`
	dualStack := `[{"interface":"eth0","ifaddr":{"ipv4":"192.168.126.12/24","ipv6":"fc00:f853:ccd:e793::12/64"},"capacity":{}}]`

	tests := []struct {
		desc    string
		oldNode *corev1.Node
		newNode *corev1.Node
		result  bool
	}{
		{
			desc:    "annotation added",
			oldNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			newNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: v4Only,
			}}},
			result: true,
		},
		{
			desc: "annotation removed",
			oldNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: v4Only,
			}}},
			newNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			result:  true,
		},
		{
			desc: "annotation value changed (IPv6 subnet added)",
			oldNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: v4Only,
			}}},
			newNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: dualStack,
			}}},
			result: true,
		},
		{
			desc: "false: annotation unchanged",
			oldNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: v4Only,
			}}},
			newNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				cloudEgressIPConfigAnnotationKey: v4Only,
			}}},
			result: false,
		},
		{
			desc:    "false: annotation absent in both",
			oldNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			newNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			result:  false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			assert.Equal(t, tc.result, CloudEgressIPConfigAnnotationChanged(tc.oldNode, tc.newNode))
		})
	}
}

func TestParseNodeDontSNATSubnetsList(t *testing.T) {
	tests := []struct {
		desc        string
		node        *corev1.Node
		expected    []string
		expectError bool
	}{
		{
			desc: "no annotation present",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: map[string]string{},
				},
			},
			expected:    []string{},
			expectError: false,
		},
		{
			desc: "valid annotation list with IPv4 and IPv6 CIDRs",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node2",
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `["192.168.1.0/24", "fd00::/64", "10.0.0.0/16"]`,
					},
				},
			},
			expected:    []string{"192.168.1.0/24", "fd00::/64", "10.0.0.0/16"},
			expectError: false,
		},
		{
			desc: "invalid annotation value (not JSON)",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node3",
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `not-a-json`,
					},
				},
			},
			expected:    nil,
			expectError: true,
		},
		{
			desc: "empty JSON array annotation",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node4",
					Annotations: map[string]string{
						OvnNodeDontSNATSubnets: `[]`,
					},
				},
			},
			expected:    []string{},
			expectError: false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			result, err := ParseNodeDontSNATSubnetsList(tc.node)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestParseNodeManagementPortAnnotation(t *testing.T) {
	tests := []struct {
		desc           string
		node           *corev1.Node
		expectedOutput NetworkDeviceDetailsMap
		expectError    error
	}{
		{
			desc: "if management port annotation is nil",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: nil,
				},
			},
			expectedOutput: nil,
			expectError:    newAnnotationNotSetError("%s annotation not found for node %q", OvnNodeManagementPort, "my-node"),
		},
		{
			desc: "if management port annotation has no fields set",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: map[string]string{},
				},
			},
			expectedOutput: nil,
			expectError:    newAnnotationNotSetError("%s annotation not found for node %q", OvnNodeManagementPort, "my-node"),
		},
		{
			desc: "if management port annotation only has default network device information",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port": `{"default":{"DeviceId":"0000:01:00.6","PfId":0,"FuncId":4}}`},
				},
			},
			expectedOutput: NetworkDeviceDetailsMap{"default": &NetworkDeviceDetails{DeviceId: "0000:01:00.6", PfId: 0, FuncId: 4}},
			expectError:    nil,
		},
		{
			desc: "if management port annotation only has legacy default network device information",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port": `{"PfId":0,"FuncId":4}`},
				},
			},
			expectedOutput: NetworkDeviceDetailsMap{"default": &NetworkDeviceDetails{PfId: 0, FuncId: 4}},
			expectError:    nil,
		},
		{
			desc: "if management port annotation has device information for two different networks",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port": `{"default":{"DeviceId":"0000:01:00.6","PfId":0,"FuncId":4}, "bluenet":{"DeviceId":"0000:01:00.8","PfId":1,"FuncId":6}}`},
				},
			},
			expectedOutput: NetworkDeviceDetailsMap{"default": &NetworkDeviceDetails{DeviceId: "0000:01:00.6", PfId: 0, FuncId: 4}, "bluenet": &NetworkDeviceDetails{DeviceId: "0000:01:00.8", PfId: 1, FuncId: 6}},
			expectError:    nil,
		},
		{
			desc: "if management port annotation has malformed device information",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port": `{"default":{}`},
				},
			},
			expectedOutput: nil,
			expectError:    fmt.Errorf("failed to unmarshal management port annotation {\"default\":{} for node \"\": unexpected end of JSON input"),
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			mpDetails, err := ParseNodeManagementPortAnnotation(tc.node)
			t.Log(mpDetails, err)
			if tc.expectError != nil {
				require.Error(t, err)
				assert.EqualError(t, err, tc.expectError.Error())
			} else {
				require.NoError(t, err)
				assert.True(t, reflect.DeepEqual(mpDetails, tc.expectedOutput))
			}
		})
	}
}

func TestIsNodeAnnotationPatchRetryable(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "conflict error is retryable",
			err:      apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node1", fmt.Errorf("conflict")),
			expected: true,
		},
		{
			name:     "invalid error is retryable (JSON Patch test failure)",
			err:      apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "Node"}, "node1", field.ErrorList{field.Invalid(field.NewPath("metadata"), nil, "test failed")}),
			expected: true,
		},
		{
			name:     "plain error is not retryable",
			err:      fmt.Errorf("plain error"),
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, IsNodeAnnotationPatchRetryable(tc.err))
		})
	}
}
