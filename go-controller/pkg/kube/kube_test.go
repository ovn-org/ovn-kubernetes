package kube

import (
	"fmt"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	mock_egressfirewall_iface "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/mocks"
	mock_egressfirewall_k8s_v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/typed/egressfirewall/v1/mocks"
	egressv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	mock_eipclient_iface "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/mocks"
	mock_eip_k8s_v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/typed/egressip/v1/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_clientgo_iface "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/kubernetes"
	mock_core_v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/kubernetes/typed/core/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestKube_SetAnnotationsOnPod(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockPodIface := new(mock_core_v1.PodInterface)
	k := &Kube{KClient: mockIface}
	pod := &v1.Pod{}
	annotations := make(map[string]string)
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			expectedErr:            true,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Pods", []string{"string"}, []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnPod")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Pods", []string{"string"}, []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockPodIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			e := k.SetAnnotationsOnPod(pod, annotations)

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockPodIface.AssertExpectations(t)

		})
	}
}

func TestKube_SetAnnotationsOnNode(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNodeIface := new(mock_core_v1.NodeInterface)
	k := &Kube{KClient: mockIface}
	node := &v1.Node{}
	annotations := make(map[string]interface{})
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			expectedErr:            true,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{"string"}, []interface{}{mockNodeIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnNode")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{"string"}, []interface{}{mockNodeIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockNodeIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			e := k.SetAnnotationsOnNode(node, annotations)

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNodeIface.AssertExpectations(t)

		})
	}
}

func TestKube_SetAnnotationsOnNamespace(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNameSpaceIface := new(mock_core_v1.NamespaceInterface)
	k := &Kube{KClient: mockIface}
	namespace := &v1.Namespace{}
	annotations := make(map[string]string)
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			expectedErr:            true,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Namespaces", []string{"string"}, []interface{}{mockNameSpaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnNamespacces")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Namespaces", []string{"string"}, []interface{}{mockNameSpaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Patch", []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockNameSpaceIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			e := k.SetAnnotationsOnNamespace(namespace, annotations)

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNameSpaceIface.AssertExpectations(t)

		})
	}
}

func TestKube_UpdateEgressFirewall(t *testing.T) {
	mockIface := new(mock_egressfirewall_iface.Interface)
	mockK8sIface := new(mock_egressfirewall_k8s_v1.K8sV1Interface)
	mockEgressfirewallIface := new(mock_egressfirewall_k8s_v1.EgressFirewallInterface)

	k := &Kube{EgressFirewallClient: mockIface}
	tests := []struct {
		desc                        string
		errorMatch                  error
		onRetMockInterfaceArgs      *ovntest.TestifyMockHelper
		onRetMockK8sArgs            *ovntest.TestifyMockHelper
		onRetmockfirewallGetterArgs *ovntest.TestifyMockHelper
		onRetMockEgressFirewallArgs *ovntest.TestifyMockHelper
	}{
		{
			desc:                        "Negative test code path",
			errorMatch:                  fmt.Errorf("error in updating status on EgressFirewall"),
			onRetMockInterfaceArgs:      &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:            &ovntest.TestifyMockHelper{"EgressFirewalls", []string{"string"}, []interface{}{mockEgressfirewallIface}},
			onRetMockEgressFirewallArgs: &ovntest.TestifyMockHelper{"Update", []string{"*context.emptyCtx", "*v1.EgressFirewall", "v1.UpdateOptions"}, []interface{}{nil, fmt.Errorf("error in updating status on EgressFirewall")}},
		},
		{
			desc:                        "Positive test code path",
			onRetMockInterfaceArgs:      &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:            &ovntest.TestifyMockHelper{"EgressFirewalls", []string{"string"}, []interface{}{mockEgressfirewallIface}},
			onRetMockEgressFirewallArgs: &ovntest.TestifyMockHelper{"Update", []string{"*context.emptyCtx", "*v1.EgressFirewall", "v1.UpdateOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockK8sArgs != nil {
				mockK8sCall := mockK8sIface.On(tc.onRetMockK8sArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockK8sArgs.OnCallMethodArgType {
					mockK8sCall.Arguments = append(mockK8sCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockK8sArgs.RetArgList {
					mockK8sCall.ReturnArguments = append(mockK8sCall.ReturnArguments, elem)
				}
				mockK8sCall.Once()
			}

			if tc.onRetMockEgressFirewallArgs != nil {
				mockEgressFirewallCall := mockEgressfirewallIface.On(tc.onRetMockEgressFirewallArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEgressFirewallArgs.OnCallMethodArgType {
					mockEgressFirewallCall.Arguments = append(mockEgressFirewallCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEgressFirewallArgs.RetArgList {
					mockEgressFirewallCall.ReturnArguments = append(mockEgressFirewallCall.ReturnArguments, elem)
				}
				mockEgressFirewallCall.Once()
			}

			e := k.UpdateEgressFirewall(&egressfirewall.EgressFirewall{})
			if tc.errorMatch != nil {
				assert.Contains(t, e.Error(), tc.errorMatch.Error())
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockK8sIface.AssertExpectations(t)
			mockEgressfirewallIface.AssertExpectations(t)

		})
	}
}

func TestKube_UpdateEgressIP(t *testing.T) {
	mockIface := new(mock_eipclient_iface.Interface)
	mockK8sIface := new(mock_eip_k8s_v1.K8sV1Interface)
	mockEgressIPIface := new(mock_eip_k8s_v1.EgressIPInterface)

	k := &Kube{EIPClient: mockIface}
	tests := []struct {
		desc                   string
		errorMatch             error
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockK8sArgs       *ovntest.TestifyMockHelper
		onRetMockEgressIPArgs  *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			errorMatch:             fmt.Errorf("error in updating status on EgressIP"),
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{"EgressIPs", []string{}, []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{"Update", []string{"*context.emptyCtx", "*v1.EgressIP", "v1.UpdateOptions"}, []interface{}{nil, fmt.Errorf("error in updating status on EgressIP")}},
		},
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{"EgressIPs", []string{}, []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{"Update", []string{"*context.emptyCtx", "*v1.EgressIP", "v1.UpdateOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockK8sArgs != nil {
				mockK8sCall := mockK8sIface.On(tc.onRetMockK8sArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockK8sArgs.OnCallMethodArgType {
					mockK8sCall.Arguments = append(mockK8sCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockK8sArgs.RetArgList {
					mockK8sCall.ReturnArguments = append(mockK8sCall.ReturnArguments, elem)
				}
				mockK8sCall.Once()
			}

			if tc.onRetMockEgressIPArgs != nil {
				mockEgressIPCall := mockEgressIPIface.On(tc.onRetMockEgressIPArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEgressIPArgs.OnCallMethodArgType {
					mockEgressIPCall.Arguments = append(mockEgressIPCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEgressIPArgs.RetArgList {
					mockEgressIPCall.ReturnArguments = append(mockEgressIPCall.ReturnArguments, elem)
				}
				mockEgressIPCall.Once()
			}

			e := k.UpdateEgressIP(&egressv1.EgressIP{})

			if tc.errorMatch != nil {
				assert.Contains(t, e.Error(), tc.errorMatch.Error())
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockK8sIface.AssertExpectations(t)
			mockEgressIPIface.AssertExpectations(t)

		})
	}
}

func TestKube_UpdateNodeStatus(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNodeIface := new(mock_core_v1.NodeInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		errorMatch             error
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockNodeArgs      *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			errorMatch:             fmt.Errorf("error in updating status on node"),
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{"string"}, []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{"UpdateStatus", []string{"*context.emptyCtx", "*v1.Node", "UpdateOptions"}, []interface{}{nil, fmt.Errorf("error in updating status on node")}},
		},
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{}, []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{"UpdateStatus", []string{"*context.emptyCtx", "*v1.Node", "UpdateOptions"}, []interface{}{&v1.Node{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockNodeArgs != nil {
				mockPodCall := mockNodeIface.On(tc.onRetMockNodeArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockNodeArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockNodeArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			e := k.UpdateNodeStatus(&v1.Node{})

			if tc.errorMatch != nil {
				assert.Contains(t, e.Error(), tc.errorMatch.Error())
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNodeIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetAnnotationsOnPod(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockPodIface := new(mock_core_v1.PodInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			expectedErr:            true,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Pods", []string{"string"}, []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Get", []string{"*context.emptyCtx", "string", "v1.GetOptions"}, []interface{}{nil, fmt.Errorf("mock Error: Failed to run GetAnnotationsOnPod")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Pods", []string{"string"}, []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"Get", []string{"*context.emptyCtx", "string", "v1.GetOptions"}, []interface{}{&v1.Pod{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockPodIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			_, e := k.GetAnnotationsOnPod("namespace", "name")

			if tc.expectedErr {
				assert.Error(t, e)
			} else {
				assert.Nil(t, e)
			}

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockPodIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetNamespaces(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNamespaceIface := new(mock_core_v1.NamespaceInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Namespaces", []string{"string"}, []interface{}{mockNamespaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"List", []string{"*context.emptyCtx", "ListOptions"}, []interface{}{&v1.NamespaceList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockNamespaceIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			_, e := k.GetNamespaces(metav1.LabelSelector{})

			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNamespaceIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetPods(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockPodIface := new(mock_core_v1.PodInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockPodArgs       *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Pods", []string{"string"}, []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{"List", []string{"*context.emptyCtx", "ListOptions"}, []interface{}{&v1.PodList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockPodArgs != nil {
				mockPodCall := mockPodIface.On(tc.onRetMockPodArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockPodArgs.OnCallMethodArgType {
					mockPodCall.Arguments = append(mockPodCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockPodArgs.RetArgList {
					mockPodCall.ReturnArguments = append(mockPodCall.ReturnArguments, ret)
				}
				mockPodCall.Once()
			}

			_, e := k.GetPods("namespace", metav1.LabelSelector{})

			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockPodIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetNodes(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNodeIface := new(mock_core_v1.NodeInterface)

	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockNodeArgs      *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{"string"}, []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{"List", []string{"*context.emptyCtx", "v1.ListOptions"}, []interface{}{&v1.NodeList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockNodeArgs != nil {
				mockEndpointCall := mockNodeIface.On(tc.onRetMockNodeArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockNodeArgs.OnCallMethodArgType {
					mockEndpointCall.Arguments = append(mockEndpointCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockNodeArgs.RetArgList {
					mockEndpointCall.ReturnArguments = append(mockEndpointCall.ReturnArguments, elem)
				}
				mockEndpointCall.Once()
			}

			_, e := k.GetNodes()
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNodeIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetNode(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockNodeIface := new(mock_core_v1.NodeInterface)

	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockNodeArgs      *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Nodes", []string{"string"}, []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{"Get", []string{"*context.emptyCtx", "string", "v1.GetOptions"}, []interface{}{&v1.Node{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockNodeArgs != nil {
				mockEndpointCall := mockNodeIface.On(tc.onRetMockNodeArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockNodeArgs.OnCallMethodArgType {
					mockEndpointCall.Arguments = append(mockEndpointCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockNodeArgs.RetArgList {
					mockEndpointCall.ReturnArguments = append(mockEndpointCall.ReturnArguments, elem)
				}
				mockEndpointCall.Once()
			}

			_, e := k.GetNode("name")
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockNodeIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetEgressIP(t *testing.T) {
	mockIface := new(mock_eipclient_iface.Interface)
	mockK8sIface := new(mock_eip_k8s_v1.K8sV1Interface)
	mockEgressIPIface := new(mock_eip_k8s_v1.EgressIPInterface)

	k := &Kube{EIPClient: mockIface}
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockK8sArgs       *ovntest.TestifyMockHelper
		onRetMockEgressIPArgs  *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{"EgressIPs", []string{}, []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{"Get", []string{"*context.emptyCtx", "string", "v1.GetOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockK8sArgs != nil {
				mockK8sCall := mockK8sIface.On(tc.onRetMockK8sArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockK8sArgs.OnCallMethodArgType {
					mockK8sCall.Arguments = append(mockK8sCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockK8sArgs.RetArgList {
					mockK8sCall.ReturnArguments = append(mockK8sCall.ReturnArguments, elem)
				}
				mockK8sCall.Once()
			}

			if tc.onRetMockEgressIPArgs != nil {
				mockEgressIPCall := mockEgressIPIface.On(tc.onRetMockEgressIPArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEgressIPArgs.OnCallMethodArgType {
					mockEgressIPCall.Arguments = append(mockEgressIPCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEgressIPArgs.RetArgList {
					mockEgressIPCall.ReturnArguments = append(mockEgressIPCall.ReturnArguments, elem)
				}
				mockEgressIPCall.Once()
			}

			_, e := k.GetEgressIP("string")
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockK8sIface.AssertExpectations(t)
			mockEgressIPIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetEgressIPs(t *testing.T) {
	mockIface := new(mock_eipclient_iface.Interface)
	mockK8sIface := new(mock_eip_k8s_v1.K8sV1Interface)
	mockEgressIPIface := new(mock_eip_k8s_v1.EgressIPInterface)

	k := &Kube{EIPClient: mockIface}
	tests := []struct {
		desc                   string
		expectedErr            bool
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockK8sArgs       *ovntest.TestifyMockHelper
		onRetMockEgressIPArgs  *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"K8sV1", []string{}, []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{"EgressIPs", []string{}, []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{"List", []string{"*context.emptyCtx", "v1.ListOptions"}, []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockK8sArgs != nil {
				mockK8sCall := mockK8sIface.On(tc.onRetMockK8sArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockK8sArgs.OnCallMethodArgType {
					mockK8sCall.Arguments = append(mockK8sCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockK8sArgs.RetArgList {
					mockK8sCall.ReturnArguments = append(mockK8sCall.ReturnArguments, elem)
				}
				mockK8sCall.Once()
			}

			if tc.onRetMockEgressIPArgs != nil {
				mockEgressIPCall := mockEgressIPIface.On(tc.onRetMockEgressIPArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEgressIPArgs.OnCallMethodArgType {
					mockEgressIPCall.Arguments = append(mockEgressIPCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEgressIPArgs.RetArgList {
					mockEgressIPCall.ReturnArguments = append(mockEgressIPCall.ReturnArguments, elem)
				}
				mockEgressIPCall.Once()
			}

			_, e := k.GetEgressIPs()
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockK8sIface.AssertExpectations(t)
			mockEgressIPIface.AssertExpectations(t)

		})
	}
}

func TestKube_GetEndpoint(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockEndpointIface := new(mock_core_v1.EndpointsInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockEndpointArgs  *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Endpoints", []string{"string"}, []interface{}{mockEndpointIface}},
			onRetMockEndpointArgs:  &ovntest.TestifyMockHelper{"Get", []string{"*context.emptyCtx", "string", "v1.GetOptions"}, []interface{}{&v1.Endpoints{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockEndpointArgs != nil {
				mockEndpointCall := mockEndpointIface.On(tc.onRetMockEndpointArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEndpointArgs.OnCallMethodArgType {
					mockEndpointCall.Arguments = append(mockEndpointCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEndpointArgs.RetArgList {
					mockEndpointCall.ReturnArguments = append(mockEndpointCall.ReturnArguments, elem)
				}
				mockEndpointCall.Once()
			}

			_, e := k.GetEndpoint("namespace", "name")
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockEndpointIface.AssertExpectations(t)

		})
	}
}

func TestKube_CreateEndpoint(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	mockEndpointIface := new(mock_core_v1.EndpointsInterface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
		onRetMockEndpointArgs  *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Endpoints", []string{"string"}, []interface{}{mockEndpointIface}},
			onRetMockEndpointArgs:  &ovntest.TestifyMockHelper{"Create", []string{"*context.emptyCtx", "*v1.Endpoints", "CreateOptions"}, []interface{}{&v1.Endpoints{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			if tc.onRetMockEndpointArgs != nil {
				mockEndpointCall := mockEndpointIface.On(tc.onRetMockEndpointArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockEndpointArgs.OnCallMethodArgType {
					mockEndpointCall.Arguments = append(mockEndpointCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockEndpointArgs.RetArgList {
					mockEndpointCall.ReturnArguments = append(mockEndpointCall.ReturnArguments, elem)
				}
				mockEndpointCall.Once()
			}

			_, e := k.CreateEndpoint("namespace", &v1.Endpoints{})
			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)
			mockEndpointIface.AssertExpectations(t)

		})
	}
}

func TestKube_Events(t *testing.T) {
	mockIface := new(mock_clientgo_iface.Interface)
	mockCoreIface := new(mock_core_v1.CoreV1Interface)
	k := &Kube{KClient: mockIface}
	tests := []struct {
		desc                   string
		onRetMockInterfaceArgs *ovntest.TestifyMockHelper
		onRetMockCoreArgs      *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{"CoreV1", []string{}, []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{"Events", []string{"string"}, []interface{}{nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				mockiFaceCall := mockIface.On(tc.onRetMockInterfaceArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockInterfaceArgs.OnCallMethodArgType {
					mockiFaceCall.Arguments = append(mockiFaceCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range tc.onRetMockInterfaceArgs.RetArgList {
					mockiFaceCall.ReturnArguments = append(mockiFaceCall.ReturnArguments, ret)
				}
				mockiFaceCall.Once()
			}

			if tc.onRetMockCoreArgs != nil {
				mockCoreCall := mockCoreIface.On(tc.onRetMockCoreArgs.OnCallMethodName)
				for _, arg := range tc.onRetMockCoreArgs.OnCallMethodArgType {
					mockCoreCall.Arguments = append(mockCoreCall.Arguments, mock.AnythingOfType(arg))
				}
				for _, elem := range tc.onRetMockCoreArgs.RetArgList {
					mockCoreCall.ReturnArguments = append(mockCoreCall.ReturnArguments, elem)
				}
				mockCoreCall.Once()
			}

			e := k.Events()

			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)

		})
	}
}
