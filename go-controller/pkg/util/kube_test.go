package util

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	kubetest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	"github.com/stretchr/testify/assert"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

var (
	nodeA = "node-a"
	nodeB = "node-b"
)

// Go Daddy Class 2 CA
const validCACert string = `-----BEGIN CERTIFICATE-----
MIIEADCCAuigAwIBAgIBADANBgkqhkiG9w0BAQUFADBjMQswCQYDVQQGEwJVUzEh
MB8GA1UEChMYVGhlIEdvIERhZGR5IEdyb3VwLCBJbmMuMTEwLwYDVQQLEyhHbyBE
YWRkeSBDbGFzcyAyIENlcnRpZmljYXRpb24gQXV0aG9yaXR5MB4XDTA0MDYyOTE3
MDYyMFoXDTM0MDYyOTE3MDYyMFowYzELMAkGA1UEBhMCVVMxITAfBgNVBAoTGFRo
ZSBHbyBEYWRkeSBHcm91cCwgSW5jLjExMC8GA1UECxMoR28gRGFkZHkgQ2xhc3Mg
MiBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTCCASAwDQYJKoZIhvcNAQEBBQADggEN
ADCCAQgCggEBAN6d1+pXGEmhW+vXX0iG6r7d/+TvZxz0ZWizV3GgXne77ZtJ6XCA
PVYYYwhv2vLM0D9/AlQiVBDYsoHUwHU9S3/Hd8M+eKsaA7Ugay9qK7HFiH7Eux6w
wdhFJ2+qN1j3hybX2C32qRe3H3I2TqYXP2WYktsqbl2i/ojgC95/5Y0V4evLOtXi
EqITLdiOr18SPaAIBQi2XKVlOARFmR6jYGB0xUGlcmIbYsUfb18aQr4CUWWoriMY
avx4A6lNf4DD+qta/KFApMoZFv6yyO9ecw3ud72a9nmYvLEHZ6IVDd2gWMZEewo+
YihfukEHU1jPEX44dMX4/7VpkI+EdOqXG68CAQOjgcAwgb0wHQYDVR0OBBYEFNLE
sNKR1EwRcbNhyz2h/t2oatTjMIGNBgNVHSMEgYUwgYKAFNLEsNKR1EwRcbNhyz2h
/t2oatTjoWekZTBjMQswCQYDVQQGEwJVUzEhMB8GA1UEChMYVGhlIEdvIERhZGR5
IEdyb3VwLCBJbmMuMTEwLwYDVQQLEyhHbyBEYWRkeSBDbGFzcyAyIENlcnRpZmlj
YXRpb24gQXV0aG9yaXR5ggEAMAwGA1UdEwQFMAMBAf8wDQYJKoZIhvcNAQEFBQAD
ggEBADJL87LKPpH8EsahB4yOd6AzBhRckB4Y9wimPQoZ+YeAEW5p5JYXMP80kWNy
OO7MHAGjHZQopDH2esRU1/blMVgDoszOYtuURXO1v0XJJLXVggKtI3lpjbi2Tc7P
TMozI+gciKqdi0FuFskg5YmezTvacPd+mSYgFFQlq25zheabIZ0KbIIOqPjCDPoQ
HmyW74cNxA9hi63ugyuV+I6ShHI56yDqg+2DzZduCLzrTia2cyvk0/ZM/iZx4mER
dEr/VxqHD3VILs9RaRegAhJhldXRQLIQTO7ErBBDpqWeCtWVYpoNz4iCxTIM5Cuf
ReYNnyicsbkqWletNw+vHX/bvZ8=
-----END CERTIFICATE-----`

func TestNewClientset(t *testing.T) {
	tests := []struct {
		desc        string
		inpConfig   config.KubernetesConfig
		errExpected bool
	}{
		{
			desc: "error: cover code path --> config.KubernetesConfig.Kubeconfig != ``",
			inpConfig: config.KubernetesConfig{
				Kubeconfig: "blah",
			},
			errExpected: true,
		},
		{
			desc: "error: missing token for https",
			inpConfig: config.KubernetesConfig{
				APIServer: "https",
			},
			errExpected: true,
		},
		{
			desc: "error: CAData invalid for https config",
			inpConfig: config.KubernetesConfig{
				CAData:    []byte("testCert"),
				APIServer: "https",
				Token:     "testToken",
			},
			errExpected: true,
		},
		{
			desc: "success: config input valid https",
			inpConfig: config.KubernetesConfig{
				APIServer: "https",
				Token:     "testToken",
				CAData:    []byte(validCACert),
			},
		},
		{
			desc: "success: cover code path --> config.APIServer == http",
			inpConfig: config.KubernetesConfig{
				APIServer: "http",
			},
		},
		{
			desc:        "error: cover code path that assumes client running inside container environment",
			inpConfig:   config.KubernetesConfig{},
			errExpected: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := NewOVNClientset(&tc.inpConfig)
			t.Log(res, e)
			if tc.errExpected {
				assert.Error(t, e)
			} else {
				assert.NotNil(t, res)
			}
		})
	}
}

func TestIsClusterIPSet(t *testing.T) {
	tests := []struct {
		desc   string
		inp    v1.Service
		expOut bool
	}{
		{
			desc: "false: test when ClusterIP set to ClusterIPNone",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP: v1.ClusterIPNone,
				},
			},
			expOut: false,
		},
		{
			desc: "false: test when ClusterIP set to empty string",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP: "",
				},
			},
			expOut: false,
		},
		{
			desc: "true: test when ClusterIP set to NON-empty string",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP: "blah",
				},
			},
			expOut: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := IsClusterIPSet(&tc.inp)
			assert.Equal(t, res, tc.expOut)
		})
	}
}

func TestValidateProtocol(t *testing.T) {
	tests := []struct {
		desc   string
		inp    v1.Protocol
		expOut v1.Protocol
		expErr bool
	}{
		{
			desc: "valid protocol SCTP",
			inp:  v1.ProtocolSCTP,
		},
		{
			desc:   "invalid protocol -> blah",
			inp:    "blah",
			expErr: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			e := ValidateProtocol(tc.inp)
			if tc.expErr {
				assert.Error(t, e)
			} else {
				assert.NoError(t, e)
			}
		})
	}
}

func TestServiceTypeHasClusterIP(t *testing.T) {
	tests := []struct {
		desc   string
		inp    v1.Service
		expOut bool
	}{
		{
			desc: "true: test when Type set to `ClusterIP`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "ClusterIP",
				},
			},
			expOut: true,
		},
		{
			desc: "true: test when Type set to `NodePort`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "NodePort",
				},
			},
			expOut: true,
		},
		{
			desc: "true: test when Type set to `LoadBalancer`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "LoadBalancer",
				},
			},
			expOut: true,
		},
		{
			desc: "false: test when Type set to `loadbalancer`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "loadbalancer",
				},
			},
			expOut: false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := ServiceTypeHasClusterIP(&tc.inp)
			assert.Equal(t, res, tc.expOut)
		})
	}
}

func TestServiceTypeHasNodePort(t *testing.T) {
	tests := []struct {
		desc   string
		inp    v1.Service
		expOut bool
	}{
		{
			desc: "true: test when Type set to `ClusterIP`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "ClusterIP",
				},
			},
			expOut: false,
		},
		{
			desc: "true: test when Type set to `NodePort`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "NodePort",
				},
			},
			expOut: true,
		},
		{
			desc: "true: test when Type set to `LoadBalancer`",
			inp: v1.Service{
				Spec: v1.ServiceSpec{
					Type: "LoadBalancer",
				},
			},
			expOut: true,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := ServiceTypeHasNodePort(&tc.inp)
			assert.Equal(t, res, tc.expOut)
		})
	}
}

func TestGetNodePrimaryIP(t *testing.T) {
	tests := []struct {
		desc   string
		inp    v1.Node
		expErr bool
		expOut string
	}{
		{
			desc: "error: node has neither external nor internal IP",
			inp: v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{Type: v1.NodeHostName, Address: "HN"},
					},
				},
			},
			expErr: true,
			expOut: "HN",
		},
		{
			desc: "success: node's internal IP returned",
			inp: v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{Type: v1.NodeHostName, Address: "HN"},
						{Type: v1.NodeInternalIP, Address: "192.168.1.1"},
						{Type: v1.NodeExternalIP, Address: "90.90.90.90"},
					},
				},
			},
			expOut: "192.168.1.1",
		},
		{
			desc: "success: node's external IP returned",
			inp: v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{Type: v1.NodeHostName, Address: "HN"},
						{Type: v1.NodeExternalIP, Address: "90.90.90.90"},
					},
				},
			},
			expOut: "90.90.90.90",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := GetNodePrimaryIP(&tc.inp)
			t.Log(res, e)
			if tc.expErr {
				assert.Error(t, e)
			} else {
				assert.Equal(t, res, tc.expOut)
			}
		})
	}
}

func Test_GetNodePrimaryIP(t *testing.T) {
	cases := []struct {
		name     string
		nodeInfo *v1.Node
		hostname string
		address  string
		wantErr  bool
	}{
		{
			name:     "non existent Node",
			nodeInfo: makeNodeWithAddresses("", "", ""),
			hostname: "nonexist",
			address:  "",
			wantErr:  true,
		},

		{
			name:     "Node with internal and external address",
			nodeInfo: makeNodeWithAddresses("fakeHost", "192.168.1.1", "90.90.90.90"),
			hostname: "fakeHost",
			address:  "192.168.1.1",
		},
		{
			name:     "Node with internal and external address IPV6",
			nodeInfo: makeNodeWithAddresses("fakeHost", "fd00:1234::1", "2001:db8::2"),
			hostname: "fakeHost",
			address:  "fd00:1234::1",
		},
		{
			name:     "Node with only IPv4 ExternalIP set",
			nodeInfo: makeNodeWithAddresses("fakeHost", "", "90.90.90.90"),
			hostname: "fakeHost",
			address:  "90.90.90.90",
		},

		{
			name:     "Node with only IPv6 ExternalIP set",
			nodeInfo: makeNodeWithAddresses("fakeHost", "", "2001:db8::2"),
			hostname: "fakeHost",
			address:  "2001:db8::2",
		},
	}
	for _, c := range cases {
		client := clientsetfake.NewSimpleClientset(c.nodeInfo)
		node, _ := client.CoreV1().Nodes().Get(context.TODO(), c.hostname, metav1.GetOptions{})
		ip, err := GetNodePrimaryIP(node)
		if err != nil != c.wantErr {
			t.Errorf("Case[%s] Expected error %v got %v", c.name, c.wantErr, err)
		}
		if ip != c.address {
			t.Errorf("Case[%s] Expected IP %q got %q", c.name, c.address, ip)
		}
	}
}

// makeNodeWithAddresses return a node object with the specified parameters
func makeNodeWithAddresses(name, internal, external string) *v1.Node {
	if name == "" {
		return &v1.Node{}
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{},
		},
	}

	if internal != "" {
		node.Status.Addresses = append(node.Status.Addresses,
			v1.NodeAddress{Type: v1.NodeInternalIP, Address: internal},
		)
	}

	if external != "" {
		node.Status.Addresses = append(node.Status.Addresses,
			v1.NodeAddress{Type: v1.NodeExternalIP, Address: external},
		)
	}

	return node
}

// protoPtr takes a Protocol and returns a pointer to it.
func protoPtr(proto v1.Protocol) *v1.Protocol {
	return &proto
}

func TestPodScheduled(t *testing.T) {
	tests := []struct {
		desc      string
		inpPod    v1.Pod
		expResult bool
	}{
		{
			desc:      "Pod is scheduled to a node",
			inpPod:    v1.Pod{Spec: v1.PodSpec{NodeName: "node-1"}},
			expResult: true,
		},
		{
			desc:      "Pod is not scheduled to a node",
			inpPod:    v1.Pod{},
			expResult: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := PodScheduled(&tc.inpPod)
			t.Log(res)
			assert.Equal(t, tc.expResult, res)
		})
	}
}

var (
	testNode   string      = "testNode"
	otherNode              = "otherNode"
	ep1Address string      = "10.244.0.3"
	ep2Address string      = "10.244.0.4"
	ep3Address string      = "10.244.1.3"
	tcpv1      v1.Protocol = v1.ProtocolTCP
	udpv1      v1.Protocol = v1.ProtocolUDP

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

// returns an endpoint slice with three endpoints, two of which belong to the expected local node
// and one belongs to "otherNode"
func getSampleEndpointSlice(service *kapi.Service) *discovery.EndpointSlice {

	epPortHttps := discovery.EndpointPort{
		Name:     &httpsPortName,
		Port:     &httpsPortValue,
		Protocol: &tcpv1,
	}

	epPortCustom := discovery.EndpointPort{
		Name:     &customPortName,
		Port:     &customPortValue,
		Protocol: &udpv1,
	}

	ep1 := discovery.Endpoint{
		Addresses: []string{ep1Address},
		NodeName:  &testNode,
	}
	ep2 := discovery.Endpoint{
		Addresses: []string{ep2Address},
		NodeName:  &testNode,
	}
	nonLocalEndpoint := discovery.Endpoint{
		Addresses: []string{ep3Address},
		NodeName:  &otherNode,
	}

	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name + "ab23",
			Namespace: service.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: service.Name},
		},
		Ports:       []discovery.EndpointPort{epPortHttps, epPortCustom},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   []discovery.Endpoint{ep1, ep2, nonLocalEndpoint},
	}
}

func setEndpointToReady(endpoint *discovery.Endpoint) {
	endpoint.Conditions.Ready = ptr.To(true)
	endpoint.Conditions.Serving = ptr.To(true)
	endpoint.Conditions.Terminating = ptr.To(false)
}

func setEndpointToTerminatingAndServing(endpoint *discovery.Endpoint) {
	endpoint.Conditions.Ready = ptr.To(false)
	endpoint.Conditions.Serving = ptr.To(true)
	endpoint.Conditions.Terminating = ptr.To(true)
}

func setEndpointToTerminatingAndNotServing(endpoint *discovery.Endpoint) {
	endpoint.Conditions.Ready = ptr.To(false)
	endpoint.Conditions.Serving = ptr.To(false)
	endpoint.Conditions.Terminating = ptr.To(true)
}

func setAllEndpointsToTerminatingAndServing(endpointSlice *discovery.EndpointSlice) *discovery.EndpointSlice {
	for i := range endpointSlice.Endpoints {
		setEndpointToTerminatingAndServing(&endpointSlice.Endpoints[i])
	}
	return endpointSlice
}

func setAllEndpointsToTerminatingAndNotServing(endpointSlice *discovery.EndpointSlice) *discovery.EndpointSlice {
	for i := range endpointSlice.Endpoints {
		setEndpointToTerminatingAndNotServing(&endpointSlice.Endpoints[i])
	}
	return endpointSlice
}

func setAllEndpointsToReady(endpointSlice *discovery.EndpointSlice) *discovery.EndpointSlice {
	for i := range endpointSlice.Endpoints {
		setEndpointToReady(&endpointSlice.Endpoints[i])
	}
	return endpointSlice
}

func setEndpointsToAMixOfStatusConditions(endpointSlice *discovery.EndpointSlice) *discovery.EndpointSlice {
	setEndpointToReady(&endpointSlice.Endpoints[0])
	setEndpointToTerminatingAndServing(&endpointSlice.Endpoints[1])
	setEndpointToTerminatingAndNotServing(&endpointSlice.Endpoints[2])
	return endpointSlice
}

func TestGetEndpointAddresses(t *testing.T) {
	service := getSampleService(false)
	var tests = []struct {
		name          string
		endpointSlice *discovery.EndpointSlice
		want          []string
	}{
		{
			"Tests an endpointslice with all ready endpoints",
			setAllEndpointsToReady(getSampleEndpointSlice(service)),
			[]string{ep1Address, ep2Address, ep3Address},
		},
		{
			"Tests an endpointslice with all non-ready, serving, terminating endpoints",
			setAllEndpointsToTerminatingAndServing(getSampleEndpointSlice(service)),
			[]string{ep1Address, ep2Address, ep3Address}, // with no ready endpoints, we fallback to terminating serving endpoints
		},
		{
			"Tests an endpointslice with all non-ready, non-serving, terminating endpoints",
			setAllEndpointsToTerminatingAndNotServing(getSampleEndpointSlice(service)),
			[]string{},
		},
		{
			"Tests an endpointslice with endpoints showing a mix of status conditions",
			setEndpointsToAMixOfStatusConditions(getSampleEndpointSlice(service)),
			[]string{ep1Address}, // only the ready endpoint is included
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := GetEligibleEndpointAddressesFromSlices([]*discovery.EndpointSlice{tt.endpointSlice}, service)
			if !reflect.DeepEqual(answer, tt.want) {
				t.Errorf("got %v, want %v", answer, tt.want)
			}
		})
	}
}

func TestHasLocalHostNetworkEndpoints(t *testing.T) {
	ep1IP := net.ParseIP(ep1Address)
	if ep1IP == nil {
		t.Errorf("error parsing ep1 address %s", ep1Address)
	}
	nodeAddresses := []net.IP{ep1IP}
	var tests = []struct {
		name           string
		localEndpoints sets.Set[string]
		want           bool
	}{
		{
			"Tests with local endpoints that include the node address",
			sets.New(ep1Address, ep2Address),
			true,
		},
		{
			"Tests against a different local endpoint than the node address",
			sets.New(ep2Address),
			false,
		},
		{
			"Tests against no local endpoints",
			sets.New[string](),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := HasLocalHostNetworkEndpoints(tt.localEndpoints, nodeAddresses)
			if !reflect.DeepEqual(answer, tt.want) {
				t.Errorf("got %v, want %v", answer, tt.want)
			}
		})
	}
}

func TestGetEligibleEndpointAddresses(t *testing.T) {
	service := getSampleService(false)
	var tests = []struct {
		name      string
		endpoints []discovery.Endpoint
		node      string
		want      []string
	}{
		{
			"Get all eligible endpoints from an endpointslice with all ready endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeReadyEndpoint(testNode, ep1Address),
				kubetest.MakeReadyEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			"", // get all endpoints
			[]string{ep1Address, ep2Address, ep3Address},
		},
		{
			"Get all eligible local endpoints from an endpointslice with all ready endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeReadyEndpoint(testNode, ep1Address),
				kubetest.MakeReadyEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{ep1Address, ep2Address},
		},
		{
			"Get all eligible local endpoints from an endpointslice with all ready endpoints, all of which are on another node",
			[]discovery.Endpoint{
				kubetest.MakeReadyEndpoint(otherNode, ep1Address),
				kubetest.MakeReadyEndpoint(otherNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{},
		},
		{
			"Get all eligible endpoints from an endpointslice with all non-ready, serving, terminating endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(testNode, ep2Address),
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep3Address),
			},
			"",
			[]string{ep1Address, ep2Address, ep3Address}, // with no ready endpoints, we fallback to terminating serving endpoints
		},
		{
			"Get all eligible local endpoints from an endpointslice with all non-ready, serving, terminating endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(testNode, ep2Address),
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{ep1Address, ep2Address}, // with no ready endpoints, we fallback to terminating serving endpoints
		},
		{
			"Get all eligible local endpoints from an endpointslice with all non-ready, serving, terminating endpoints, all of which are on a different node",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep2Address),
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{},
		},
		{
			"Get all eligible endpoints from an endpointslice with all non-ready, non-serving, terminating endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep2Address),
				kubetest.MakeTerminatingNonServingEndpoint(otherNode, ep3Address),
			},
			"",
			[]string{},
		},
		{
			"Get all eligible local endpoints from an endpointslice with all non-ready, non-serving, terminating endpoints, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep2Address),
				kubetest.MakeTerminatingNonServingEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{},
		},
		{
			"Get all eligible local endpoints from an endpointslice with endpoints showing a mix of status conditions, one of which is on a different node",
			[]discovery.Endpoint{
				kubetest.MakeReadyEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(testNode, ep2Address),
				kubetest.MakeTerminatingNonServingEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{ep1Address}, // only the ready endpoint is included
		},
		{
			"Get all eligible local endpoints from an endpointslice with endpoints showing a mix of status conditions, all of which are on a different node",
			[]discovery.Endpoint{
				kubetest.MakeReadyEndpoint(otherNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(otherNode, ep2Address),
				kubetest.MakeTerminatingNonServingEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{},
		},
		{
			"Get all eligible endpoints from an endpointslice where all local endpoints are serving and terminating and a remote endpoint is ready",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			"",
			[]string{ep3Address}, // fallback to serving&terminating should apply
		},
		{
			"Get all eligible local endpoints from an endpointslice where all local endpoints are serving and terminating and a remote endpoint is ready",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingServingEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{ep1Address, ep2Address}, // fallback to serving&terminating should apply
		},
		{
			"Get all eligible endpoints from an endpointslice where all local endpoints are terminating and not serving and a remote endpoint is ready",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			"",
			[]string{ep3Address},
		},
		{
			"Get all eligible local endpoints from an endpointslice where all local endpoints are terminating and not serving and a remote endpoint is ready",
			[]discovery.Endpoint{
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep1Address),
				kubetest.MakeTerminatingNonServingEndpoint(testNode, ep2Address),
				kubetest.MakeReadyEndpoint(otherNode, ep3Address),
			},
			testNode,
			[]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := getEligibleEndpointAddresses(tt.endpoints, service, tt.node)
			if !reflect.DeepEqual(answer, tt.want) {
				t.Errorf("got %v, want %v", answer, tt.want)
			}
		})
	}
}

func TestDoesEndpointSliceContainEligibleEndpoint(t *testing.T) {
	service := getSampleService(false)
	var tests = []struct {
		name          string
		endpointSlice *discovery.EndpointSlice
		epIP          string
		epPort        int32
		protocol      v1.Protocol
		want          bool
	}{
		{
			"Tests an endpointslice with all ready endpoints",
			setAllEndpointsToReady(getSampleEndpointSlice(service)),
			ep1Address, httpsPortValue, tcpv1,
			true,
		},
		{
			"Tests an endpointslice with all ready endpoints and a port that is not included",
			setAllEndpointsToReady(getSampleEndpointSlice(service)),
			ep1Address, int32(444), tcpv1,
			false,
		},

		{
			"Tests an endpointslice with all non-ready, serving, terminating endpoints",
			setAllEndpointsToTerminatingAndServing(getSampleEndpointSlice(service)),
			ep1Address, customPortValue, udpv1,
			true,
		},
		{
			"Tests an endpointslice with all non-ready, non-serving, terminating endpoints",
			setAllEndpointsToTerminatingAndNotServing(getSampleEndpointSlice(service)),
			ep1Address, customPortValue, udpv1,
			false,
		},
		{
			"Tests an endpointslice with endpoints showing a mix of status conditions",
			setEndpointsToAMixOfStatusConditions(getSampleEndpointSlice(service)),
			ep1Address, customPortValue, udpv1,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := DoesEndpointSliceContainEligibleEndpoint(tt.endpointSlice, tt.epIP, tt.epPort, tt.protocol, service)
			if !reflect.DeepEqual(answer, tt.want) {
				t.Errorf("got %v, want %v", answer, tt.want)
			}
		})
	}
}
