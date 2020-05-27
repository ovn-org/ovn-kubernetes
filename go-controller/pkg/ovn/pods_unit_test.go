package ovn

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	mock_k8s_io_client_go_kubernetes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/kubernetes"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	k8s_io_apimachinery_pkg_apis_meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net"

	//"net"
	"testing"
)

type onCallReturnArgs struct {
	onCallMethodName    string
	onCallMethodArgType []string
	retArgList          []interface{}
	fnCallTimes         int
}

func getMockController() *Controller {
	stopChan := make(chan struct{})
	mockK8SIface := new(mock_k8s_io_client_go_kubernetes.Interface)
	mockK8SIface.On("NetworkingV1").Return().On("CoreV1").Return()
	mockWatchFactory, err := factory.NewWatchFactory(mockK8SIface, stopChan)
	if err != nil {
		return nil
	}
	mockController := NewOvnController(mockK8SIface, mockWatchFactory, stopChan)
	return mockController
}

func getMockWatchFactory(iface kubernetes.Interface) *factory.WatchFactory {
	mockK8SIface := new(mock_k8s_io_client_go_kubernetes.Interface)
	stopChan := make(chan struct{})
	mockWatchFactory, err := factory.NewWatchFactory(mockK8SIface, stopChan)
	if err != nil {
		return nil
	}
	return mockWatchFactory
}

func TestPodLogicalPortName(t *testing.T) {
	tests := []struct {
		desc           string
		inputPod       v1.Pod
		matchResult    bool
		expectedResult string
	}{
		{
			desc: "postive case",
			inputPod: v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Namespace: "ovn-kubernetes",
					Name:      "mypod",
				},
			},
			matchResult:    true,
			expectedResult: "ovn-kubernetes_mypod",
		},
		{
			desc: "postive case: no name provided",
			inputPod: v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Namespace: "ovn-kubernetes",
				},
			},
			matchResult:    true,
			expectedResult: "ovn-kubernetes_",
		},
		{
			desc: "postive case: no namespace provided",
			inputPod: v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Name: "mypod",
				},
			},
			matchResult:    true,
			expectedResult: "_mypod",
		},
		{
			desc: "negative case, ensure namespace is appended",
			inputPod: v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Namespace: "ovn-kubernetes",
					Name:      "mypod",
				},
			},
			matchResult:    false,
			expectedResult: "mypod",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := podLogicalPortName(&tc.inputPod)
			t.Log(res)
			t.Log(tc.expectedResult)
			if tc.matchResult {
				assert.Equal(t, res, tc.expectedResult)
			} else {
				assert.NotEqual(t, res, tc.expectedResult)
			}
		})
	}
}

func TestGetPodAddresses(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecUtilRunSvc := new(utilmocks.ExecUtilRunSvc)
	// below is defined in ovs.go
	util.RunCmdExecSvcInst = mockExecUtilRunSvc
	tests := []struct {
		desc                    string
		inpPortName             string
		expectedErr             bool
		onRetArgsIface          *onCallReturnArgs
		onRetArgsExecUtilRunSvc []onCallReturnArgs
	}{
		{
			desc:           "positive, valid portname",
			inpPortName:    "valid",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", nil}, 8},
			onRetArgsExecUtilRunSvc: []onCallReturnArgs{
				//{"SetExec", []string{"*mock_k8s_io_utils_exec.Interface"}, []interface{}{nil}, 1},
				{"RunWithEnvVars", []string{"string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{"22:e9:ac:f4:00:04 10.244.0.3\n[dynamic]", "", nil}, 1},
			},
		},
		{
			desc:        "negative, invalid portname",
			inpPortName: "invalid",
			expectedErr: true,
			//onRetArgsIface: &onCallReturnArgs{"RunCmd", []string{"kexec.Cmd", "string", "[]string"}, []interface{}{[]byte(""), []byte(""), nil}, 1},
			onRetArgsIface: &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", nil}, 8},
			onRetArgsExecUtilRunSvc: []onCallReturnArgs{
				{"RunWithEnvVars", []string{"kexec.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("Error while obtaining pod addressess")}, 1},
				//{"SetExec", []string{"*mocks.Interface"}, []interface{}{nil}, 1},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetArgsIface != nil {
				call := mockKexecIface.On(tc.onRetArgsIface.onCallMethodName)
				for _, arg := range tc.onRetArgsIface.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsIface.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Times(tc.onRetArgsIface.fnCallTimes)
			}

			for _, elem := range tc.onRetArgsExecUtilRunSvc {
				call := mockExecUtilRunSvc.On(elem.onCallMethodName)
				for range elem.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.Anything)
				}
				for _, ret := range elem.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Times(elem.fnCallTimes)
			}

			util.SetExec(mockKexecIface)
			hw, ip, done, e := getPodAddresses(tc.inpPortName)
			t.Log(hw, ip, done, e)
			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.NoError(t, e)
			}

			mockExecUtilRunSvc.AssertExpectations(t)
		})
	}
}

func TestWaitForPodAddresses(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecUtilRunSvc := new(utilmocks.ExecUtilRunSvc)
	// below is defined in ovs.go
	util.RunCmdExecSvcInst = mockExecUtilRunSvc
	tests := []struct {
		desc                    string
		inpPortName             string
		expectedErr             bool
		onRetArgsIface          *onCallReturnArgs
		onRetArgsExecUtilRunSvc []onCallReturnArgs
	}{
		{
			desc:           "positive, valid portname",
			inpPortName:    "valid",
			expectedErr:    false,
			onRetArgsIface: &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", nil}, 8},
			onRetArgsExecUtilRunSvc: []onCallReturnArgs{
				{"RunWithEnvVars", []string{"kexec.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{"22:e9:ac:f4:00:04 10.244.0.3\n[dynamic]", "", nil}, 1},
				//{"SetExec", []string{"*mocks.Interface"}, []interface{}{nil}, 1},
			},
		},
		{
			desc:        "negative, invalid portname",
			inpPortName: "invalid",
			expectedErr: true,
			//onRetArgsIface: &onCallReturnArgs{"RunCmd", []string{"kexec.Cmd", "string", "[]string"}, []interface{}{[]byte(""), []byte(""), nil}, 1},
			onRetArgsIface: &onCallReturnArgs{"LookPath", []string{"string"}, []interface{}{"", nil}, 8},
			onRetArgsExecUtilRunSvc: []onCallReturnArgs{
				{"RunWithEnvVars", []string{"kexec.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, []interface{}{"", "", fmt.Errorf("Error while obtaining pod addressess")}, 1},
				//{"SetExec", []string{"*mocks.Interface"}, []interface{}{nil}, 1},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetArgsIface != nil {
				call := mockKexecIface.On(tc.onRetArgsIface.onCallMethodName)
				for _, arg := range tc.onRetArgsIface.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetArgsIface.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Times(tc.onRetArgsIface.fnCallTimes)
			}

			for _, elem := range tc.onRetArgsExecUtilRunSvc {
				call := mockExecUtilRunSvc.On(elem.onCallMethodName)
				for range elem.onCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.Anything)
				}
				for _, ret := range elem.retArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Times(elem.fnCallTimes)
			}

			util.SetExec(mockKexecIface)
			hw, ip, e := waitForPodAddresses(tc.inpPortName)
			t.Log(hw, ip, e)
			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.NoError(t, e)
			}

			mockExecUtilRunSvc.AssertExpectations(t)
		})
	}
}

func TestGetRoutesGatewayIP(t *testing.T) {
	tests := []struct {
		desc           string
		inputPod       v1.Pod
		inputGwIpnet   string
		inputHybGwIp   net.IP
		matchResult    bool
		expectedResult string
	}{
		{
			desc: "postive case, pod with text annotation",
			inputPod: v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Namespace:   "ovn-kubernetes",
					Name:        "mypod",
					Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": "ovn-kubernetes/macvlan-conf-1, ovn-kubernetes/macvlan-conf-2"},
				},
			},
			inputGwIpnet:   "10.0.0.1/24",
			inputHybGwIp:   []byte("100.64.0.1"),
			matchResult:    true,
			expectedResult: "ovn-kubernetes_mypod",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ip, ipnet, e := net.ParseCIDR(tc.inputGwIpnet)
			if e != nil {
				t.FailNow()
			}
			podRoute, ip, e := getRoutesGatewayIP(&tc.inputPod, ipnet, ip)
			t.Log(podRoute, ip, e)
			t.Log(tc.expectedResult)

		})
	}
}

/*
func TestController_SyncPods(t *testing.T) {
	controller := getMockController()
	tests := []struct {
		desc 		string
		input 		[]interface{}
	}{
		{
			desc: "single pod invocation",
			input: 	[]interface{}{v1.Pod{
				ObjectMeta: k8s_io_apimachinery_pkg_apis_meta_v1.ObjectMeta{
					Namespace: "ovn-kubernetes",
					Name:      "mypod",
					Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks":"ovn-kubernetes/macvlan-conf-1, ovn-kubernetes/macvlan-conf-2"},
				},
			}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T){
			controller.syncPods(tc.input)
		})
	}
}*/
