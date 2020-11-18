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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Pods", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnPod")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Pods", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockPodArgs != nil {
				ovntest.ProcessMockFn(&mockPodIface.Mock, *tc.onRetMockPodArgs)
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
		onRetMockNodeArgs      *ovntest.TestifyMockHelper
	}{
		{
			desc:                   "Negative test code path",
			expectedErr:            true,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnNode")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockNodeArgs != nil {
				ovntest.ProcessMockFn(&mockNodeIface.Mock, *tc.onRetMockNodeArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Namespaces", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNameSpaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("mock Error: Failed to run SetAnnotationsOnNamespacces")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Namespaces", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNameSpaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Patch", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "types.PatchType", "[]uint8", "v1.PatchOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockPodArgs != nil {
				ovntest.ProcessMockFn(&mockNameSpaceIface.Mock, *tc.onRetMockPodArgs)
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
			onRetMockInterfaceArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:            &ovntest.TestifyMockHelper{OnCallMethodName: "EgressFirewalls", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockEgressfirewallIface}},
			onRetMockEgressFirewallArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "Update", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.EgressFirewall", "v1.UpdateOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("error in updating status on EgressFirewall")}},
		},
		{
			desc:                        "Positive test code path",
			onRetMockInterfaceArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:            &ovntest.TestifyMockHelper{OnCallMethodName: "EgressFirewalls", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockEgressfirewallIface}},
			onRetMockEgressFirewallArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "Update", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.EgressFirewall", "v1.UpdateOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockK8sArgs != nil {
				ovntest.ProcessMockFn(&mockK8sIface.Mock, *tc.onRetMockK8sArgs)
			}

			if tc.onRetMockEgressFirewallArgs != nil {
				ovntest.ProcessMockFn(&mockEgressfirewallIface.Mock, *tc.onRetMockEgressFirewallArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "EgressIPs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "Update", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.EgressIP", "v1.UpdateOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("error in updating status on EgressIP")}},
		},
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "EgressIPs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "Update", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.EgressIP", "v1.UpdateOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockK8sArgs != nil {
				ovntest.ProcessMockFn(&mockK8sIface.Mock, *tc.onRetMockK8sArgs)
			}

			if tc.onRetMockEgressIPArgs != nil {
				ovntest.ProcessMockFn(&mockEgressIPIface.Mock, *tc.onRetMockEgressIPArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "UpdateStatus", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.Node", "UpdateOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("error in updating status on node")}},
		},
		{
			desc:                   "Positive test code path",
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "UpdateStatus", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.Node", "UpdateOptions"}, RetArgList: []interface{}{&v1.Node{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockNodeArgs != nil {
				ovntest.ProcessMockFn(&mockNodeIface.Mock, *tc.onRetMockNodeArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Pods", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Get", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "v1.GetOptions"}, RetArgList: []interface{}{nil, fmt.Errorf("mock Error: Failed to run GetAnnotationsOnPod")}},
		},
		{
			desc:                   "Positive test code path",
			expectedErr:            false,
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Pods", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "Get", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "v1.GetOptions"}, RetArgList: []interface{}{&v1.Pod{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockPodArgs != nil {
				ovntest.ProcessMockFn(&mockPodIface.Mock, *tc.onRetMockPodArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Namespaces", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNamespaceIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "List", OnCallMethodArgType: []string{"*context.emptyCtx", "ListOptions"}, RetArgList: []interface{}{&v1.NamespaceList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockPodArgs != nil {
				ovntest.ProcessMockFn(&mockNamespaceIface.Mock, *tc.onRetMockPodArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Pods", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockPodIface}},
			onRetMockPodArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "List", OnCallMethodArgType: []string{"*context.emptyCtx", "ListOptions"}, RetArgList: []interface{}{&v1.PodList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockPodArgs != nil {
				ovntest.ProcessMockFn(&mockPodIface.Mock, *tc.onRetMockPodArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "List", OnCallMethodArgType: []string{"*context.emptyCtx", "v1.ListOptions"}, RetArgList: []interface{}{&v1.NodeList{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockNodeArgs != nil {
				ovntest.ProcessMockFn(&mockNodeIface.Mock, *tc.onRetMockNodeArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Nodes", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockNodeIface}},
			onRetMockNodeArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Get", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "v1.GetOptions"}, RetArgList: []interface{}{&v1.Node{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockNodeArgs != nil {
				ovntest.ProcessMockFn(&mockNodeIface.Mock, *tc.onRetMockNodeArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "EgressIPs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "Get", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "v1.GetOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockK8sArgs != nil {
				ovntest.ProcessMockFn(&mockK8sIface.Mock, *tc.onRetMockK8sArgs)
			}

			if tc.onRetMockEgressIPArgs != nil {
				ovntest.ProcessMockFn(&mockEgressIPIface.Mock, *tc.onRetMockEgressIPArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "K8sV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockK8sIface}},
			onRetMockK8sArgs:       &ovntest.TestifyMockHelper{OnCallMethodName: "EgressIPs", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockEgressIPIface}},
			onRetMockEgressIPArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "List", OnCallMethodArgType: []string{"*context.emptyCtx", "v1.ListOptions"}, RetArgList: []interface{}{nil, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockK8sArgs != nil {
				ovntest.ProcessMockFn(&mockK8sIface.Mock, *tc.onRetMockK8sArgs)
			}

			if tc.onRetMockEgressIPArgs != nil {
				ovntest.ProcessMockFn(&mockEgressIPIface.Mock, *tc.onRetMockEgressIPArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Endpoints", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockEndpointIface}},
			onRetMockEndpointArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "Get", OnCallMethodArgType: []string{"*context.emptyCtx", "string", "v1.GetOptions"}, RetArgList: []interface{}{&v1.Endpoints{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockEndpointArgs != nil {
				ovntest.ProcessMockFn(&mockEndpointIface.Mock, *tc.onRetMockEndpointArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Endpoints", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockEndpointIface}},
			onRetMockEndpointArgs:  &ovntest.TestifyMockHelper{OnCallMethodName: "Create", OnCallMethodArgType: []string{"*context.emptyCtx", "*v1.Endpoints", "CreateOptions"}, RetArgList: []interface{}{&v1.Endpoints{}, nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			if tc.onRetMockEndpointArgs != nil {
				ovntest.ProcessMockFn(&mockEndpointIface.Mock, *tc.onRetMockEndpointArgs)
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
			onRetMockInterfaceArgs: &ovntest.TestifyMockHelper{OnCallMethodName: "CoreV1", OnCallMethodArgType: []string{}, RetArgList: []interface{}{mockCoreIface}},
			onRetMockCoreArgs:      &ovntest.TestifyMockHelper{OnCallMethodName: "Events", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil}},
		},
	}
	for i, tc := range tests {

		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {

			if tc.onRetMockInterfaceArgs != nil {
				ovntest.ProcessMockFn(&mockIface.Mock, *tc.onRetMockInterfaceArgs)
			}

			if tc.onRetMockCoreArgs != nil {
				ovntest.ProcessMockFn(&mockCoreIface.Mock, *tc.onRetMockCoreArgs)
			}

			e := k.Events()

			assert.Nil(t, e)

			mockIface.AssertExpectations(t)
			mockCoreIface.AssertExpectations(t)

		})
	}
}
