package util

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"net"
	"reflect"
	"testing"
)
import "fmt"

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
				Mode:   config.GatewayModeLocal,
				VLANID: &vlanid,
			},
			expOutput: []byte(`{"mode":"local","node-port-enable":"false","vlan-id":"1024"}`),
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
			expOutput: []byte(`{"mode":"local","interface-id":"INTERFACE-ID","mac-address":"11:22:33:44:55:66","ip-addresses":["192.168.1.10/24","fd01::1234/64"],"next-hops":["192.168.1.1","fd01::1"],"node-port-enable":"false","vlan-id":"1024"}`),
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
		desc        string
		expOut      L3GatewayConfig
		inputParam  []byte
		errExpected bool
	}{
		{
			desc:        "error: test bad input causing json Unmarshal error",
			errExpected: true,
			inputParam:  []byte(`{`),
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
			desc:        "error: test unsupported gateway mode",
			inputParam:  []byte(`{"mode":"blah"}`),
			errExpected: true,
		},
		{
			desc:        "error: test bad VLANID input",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","vlan-id":"A"}`),
		},

		{
			desc:        "test bad MAC address value",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","mac-address":"BADMAC"}`),
		},
		{
			desc:        "test bad 'IP address' value",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1/24"}`),
		},
		{
			desc:        "test bad 'IP addresses' value",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-addresses":["192.168.1/24","fd01::1234/64"]}`),
		},
		{
			desc:        "test bad 'next-hop value",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.5/24","next-hop":"192.168.11"}`),
		},
		{
			desc:        "test bad 'next-hops' value",
			errExpected: true,
			inputParam:  []byte(`{"mode":"local","mac-address":"11:22:33:44:55:66","ip-address":"192.168.1.5/24", "next-hops":["192.168.1.","fd01::1"]}`),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			l3GwCfg := L3GatewayConfig{}
			e := l3GwCfg.UnmarshalJSON(tc.inputParam)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			} else {
				t.Log(l3GwCfg)
				assert.Equal(t, l3GwCfg, tc.expOut)
			}
		})
	}
}

func TestSetL3GatewayConfig(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{})
	k := &kube.Kube{KClient: fakeClient}
	testAnnotator := kube.NewNodeAnnotator(k, &v1.Node{})

	tests := []struct {
		desc             string
		inpNodeAnnotator kube.Annotator
		inputL3GwCfg     L3GatewayConfig
		errExpected      bool
	}{
		{
			desc:             "success: empty L3GatewayConfig applied should pass",
			inpNodeAnnotator: testAnnotator,
			inputL3GwCfg:     L3GatewayConfig{},
		},
		{
			desc:             "success: apply empty Chassis id ",
			inpNodeAnnotator: testAnnotator,
			inputL3GwCfg: L3GatewayConfig{
				ChassisID: " ",
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			e := SetL3GatewayConfig(tc.inpNodeAnnotator, &tc.inputL3GwCfg)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			}
		})
	}
}

func TestParseNodeL3GatewayAnnotation(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     *v1.Node
		errExpected bool
	}{
		{
			desc:        "error: annotation not found for node",
			inpNode:     &v1.Node{},
			errExpected: true,
		},
		{
			desc: "error: fail to unmarshal l3 gateway config annotations",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac_address":"}}`},
				},
			},
			errExpected: true,
		},
		{
			desc: "error: annotation for network not found",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"nondefault":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`},
				},
			},
			errExpected: true,
		},
		{
			desc: "error: nod chassis ID annotation not found",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`},
				},
			},
			errExpected: true,
		},
		{
			desc: "success: parse completed",
			inpNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`,
						"k8s.ovn.org/node-chassis-id":   "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeL3GatewayAnnotation(tc.inpNode)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
				assert.Nil(t, cfg)
			} else {
				assert.NotNil(t, cfg)
			}
		})
	}
}

func TestParseNodeManagementPortMACAddress(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     v1.Node
		errExpected bool
		expOutput   bool
	}{
		{
			desc:      "mac address annotation not found for node, however, does not return error",
			inpNode:   v1.Node{},
			expOutput: false,
		},
		{
			desc: "success: parse mac address",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-address": "96:8f:e8:25:a2:e5"},
				},
			},
			expOutput: true,
		},
		{
			desc: "error: parse mac address error",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/node-mgmt-port-mac-address": "96:8f:e8:25:a2:"},
				},
			},
			errExpected: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cfg, e := ParseNodeManagementPortMACAddress(&tc.inpNode)
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
