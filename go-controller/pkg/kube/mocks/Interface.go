// Code generated by mockery v2.14.0. DO NOT EDIT.

package mocks

import (
	adminpolicybasedroutev1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	apicorev1 "k8s.io/api/core/v1"

	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mock "github.com/stretchr/testify/mock"

	v1 "github.com/openshift/api/cloudnetwork/v1"
)

// Interface is an autogenerated mock type for the Interface type
type Interface struct {
	mock.Mock
}

// CreateCloudPrivateIPConfig provides a mock function with given fields: cloudPrivateIPConfig
func (_m *Interface) CreateCloudPrivateIPConfig(cloudPrivateIPConfig *v1.CloudPrivateIPConfig) (*v1.CloudPrivateIPConfig, error) {
	ret := _m.Called(cloudPrivateIPConfig)

	var r0 *v1.CloudPrivateIPConfig
	if rf, ok := ret.Get(0).(func(*v1.CloudPrivateIPConfig) *v1.CloudPrivateIPConfig); ok {
		r0 = rf(cloudPrivateIPConfig)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.CloudPrivateIPConfig)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(*v1.CloudPrivateIPConfig) error); ok {
		r1 = rf(cloudPrivateIPConfig)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// DeleteCloudPrivateIPConfig provides a mock function with given fields: name
func (_m *Interface) DeleteCloudPrivateIPConfig(name string) error {
	ret := _m.Called(name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string) error); ok {
		r0 = rf(name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Events provides a mock function with given fields:
func (_m *Interface) Events() corev1.EventInterface {
	ret := _m.Called()

	var r0 corev1.EventInterface
	if rf, ok := ret.Get(0).(func() corev1.EventInterface); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(corev1.EventInterface)
		}
	}

	return r0
}

// GetAnnotationsOnPod provides a mock function with given fields: namespace, name
func (_m *Interface) GetAnnotationsOnPod(namespace string, name string) (map[string]string, error) {
	ret := _m.Called(namespace, name)

	var r0 map[string]string
	if rf, ok := ret.Get(0).(func(string, string) map[string]string); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(map[string]string)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNamespaces provides a mock function with given fields: labelSelector
func (_m *Interface) GetNamespaces(labelSelector metav1.LabelSelector) (*apicorev1.NamespaceList, error) {
	ret := _m.Called(labelSelector)

	var r0 *apicorev1.NamespaceList
	if rf, ok := ret.Get(0).(func(metav1.LabelSelector) *apicorev1.NamespaceList); ok {
		r0 = rf(labelSelector)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.NamespaceList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(metav1.LabelSelector) error); ok {
		r1 = rf(labelSelector)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNode provides a mock function with given fields: name
func (_m *Interface) GetNode(name string) (*apicorev1.Node, error) {
	ret := _m.Called(name)

	var r0 *apicorev1.Node
	if rf, ok := ret.Get(0).(func(string) *apicorev1.Node); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.Node)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNodes provides a mock function with given fields:
func (_m *Interface) GetNodes() (*apicorev1.NodeList, error) {
	ret := _m.Called()

	var r0 *apicorev1.NodeList
	if rf, ok := ret.Get(0).(func() *apicorev1.NodeList); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.NodeList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPod provides a mock function with given fields: namespace, name
func (_m *Interface) GetPod(namespace string, name string) (*apicorev1.Pod, error) {
	ret := _m.Called(namespace, name)

	var r0 *apicorev1.Pod
	if rf, ok := ret.Get(0).(func(string, string) *apicorev1.Pod); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.Pod)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPods provides a mock function with given fields: namespace, labelSelector
func (_m *Interface) GetPods(namespace string, labelSelector metav1.LabelSelector) (*apicorev1.PodList, error) {
	ret := _m.Called(namespace, labelSelector)

	var r0 *apicorev1.PodList
	if rf, ok := ret.Get(0).(func(string, metav1.LabelSelector) *apicorev1.PodList); ok {
		r0 = rf(namespace, labelSelector)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.PodList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, metav1.LabelSelector) error); ok {
		r1 = rf(namespace, labelSelector)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// PatchNode provides a mock function with given fields: old, new
func (_m *Interface) PatchNode(old *apicorev1.Node, new *apicorev1.Node) error {
	ret := _m.Called(old, new)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Node, *apicorev1.Node) error); ok {
		r0 = rf(old, new)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// RemoveTaintFromNode provides a mock function with given fields: nodeName, taint
func (_m *Interface) RemoveTaintFromNode(nodeName string, taint *apicorev1.Taint) error {
	ret := _m.Called(nodeName, taint)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *apicorev1.Taint) error); ok {
		r0 = rf(nodeName, taint)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnNamespace provides a mock function with given fields: namespaceName, annotations
func (_m *Interface) SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespaceName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, map[string]interface{}) error); ok {
		r0 = rf(namespaceName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnNode provides a mock function with given fields: nodeName, annotations
func (_m *Interface) SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error {
	ret := _m.Called(nodeName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, map[string]interface{}) error); ok {
		r0 = rf(nodeName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnPod provides a mock function with given fields: namespace, podName, annotations
func (_m *Interface) SetAnnotationsOnPod(namespace string, podName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespace, podName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, map[string]interface{}) error); ok {
		r0 = rf(namespace, podName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnService provides a mock function with given fields: namespace, serviceName, annotations
func (_m *Interface) SetAnnotationsOnService(namespace string, serviceName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespace, serviceName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, map[string]interface{}) error); ok {
		r0 = rf(namespace, serviceName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetTaintOnNode provides a mock function with given fields: nodeName, taint
func (_m *Interface) SetTaintOnNode(nodeName string, taint *apicorev1.Taint) error {
	ret := _m.Called(nodeName, taint)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *apicorev1.Taint) error); ok {
		r0 = rf(nodeName, taint)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateCloudPrivateIPConfig provides a mock function with given fields: cloudPrivateIPConfig
func (_m *Interface) UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *v1.CloudPrivateIPConfig) (*v1.CloudPrivateIPConfig, error) {
	ret := _m.Called(cloudPrivateIPConfig)

	var r0 *v1.CloudPrivateIPConfig
	if rf, ok := ret.Get(0).(func(*v1.CloudPrivateIPConfig) *v1.CloudPrivateIPConfig); ok {
		r0 = rf(cloudPrivateIPConfig)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.CloudPrivateIPConfig)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(*v1.CloudPrivateIPConfig) error); ok {
		r1 = rf(cloudPrivateIPConfig)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// UpdateNode provides a mock function with given fields: node
func (_m *Interface) UpdateNode(node *apicorev1.Node) error {
	ret := _m.Called(node)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Node) error); ok {
		r0 = rf(node)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateNodeStatus provides a mock function with given fields: node
func (_m *Interface) UpdateNodeStatus(node *apicorev1.Node) error {
	ret := _m.Called(node)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Node) error); ok {
		r0 = rf(node)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdatePod provides a mock function with given fields: pod
func (_m *Interface) UpdatePod(pod *apicorev1.Pod) error {
	ret := _m.Called(pod)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Pod) error); ok {
		r0 = rf(pod)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateStatusAPBExternalRoute provides a mock function with given fields: route
func (_m *Interface) UpdateStatusAPBExternalRoute(route *adminpolicybasedroutev1.AdminPolicyBasedExternalRoute) error {
	ret := _m.Called(route)

	var r0 error
	if rf, ok := ret.Get(0).(func(*adminpolicybasedroutev1.AdminPolicyBasedExternalRoute) error); ok {
		r0 = rf(route)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

type mockConstructorTestingTNewInterface interface {
	mock.TestingT
	Cleanup(func())
}

// NewInterface creates a new instance of Interface. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewInterface(t mockConstructorTestingTNewInterface) *Interface {
	mock := &Interface{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
