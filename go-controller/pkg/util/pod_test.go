package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"

	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	v1 "k8s.io/api/core/v1"

	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
)

func TestUpdatePodWithAllocationOrRollback(t *testing.T) {
	tests := []struct {
		name             string
		allocateRollback bool
		getPodErr        bool
		allocateErr      bool
		updatePodErr     bool
		expectAllocation bool
		expectRollback   bool
		expectUpdate     bool
		expectErr        bool
	}{
		{
			name:             "normal operation",
			allocateRollback: true,
			expectAllocation: true,
			expectUpdate:     true,
		},
		{
			name:             "pod get fails",
			allocateRollback: true,
			getPodErr:        true,
			expectErr:        true,
		},
		{
			name:             "allocate fails",
			expectAllocation: true,
			allocateRollback: true,
			allocateErr:      true,
			expectErr:        true,
		},
		{
			name:             "update pod fails",
			expectAllocation: true,
			allocateRollback: true,
			updatePodErr:     true,
			expectRollback:   true,
			expectErr:        true,
		},
		{
			name:             "update pod fails no rollback",
			expectAllocation: true,
			updatePodErr:     true,
			expectErr:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			podListerMock := &v1mocks.PodLister{}
			kubeMock := &kubemocks.Interface{}
			podNamespaceLister := &v1mocks.PodNamespaceLister{}

			podListerMock.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)

			var rollbackDone bool
			rollback := func() {
				rollbackDone = true
			}

			pod := &v1.Pod{}

			var allocated bool
			allocate := func(pod *v1.Pod) (*v1.Pod, func(), error) {
				allocated = true
				if tt.allocateErr {
					return pod, rollback, errors.New("Allocate error")
				}
				if tt.allocateRollback {
					return pod, rollback, nil
				}
				return pod, nil, nil
			}

			if tt.getPodErr {
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.New("Get pod error"))
			} else {
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			}

			if tt.updatePodErr {
				kubeMock.On("UpdatePod", pod).Return(errors.New("Update pod error"))
			} else if tt.expectUpdate {
				kubeMock.On("UpdatePod", pod).Return(nil)
			}

			err := UpdatePodWithRetryOrRollback(podListerMock, kubeMock, &v1.Pod{}, allocate)

			if (err != nil) != tt.expectErr {
				t.Errorf("UpdatePodWithAllocationOrRollback() error = %v, expectErr %v", err, tt.expectErr)
			}

			if allocated != tt.expectAllocation {
				t.Errorf("UpdatePodWithAllocationOrRollback() allocated = %v, expectAllocation %v", allocated, tt.expectAllocation)
			}

			if rollbackDone != tt.expectRollback {
				t.Errorf("UpdatePodWithAllocationOrRollback() rollbackDone = %v, expectRollback %v", rollbackDone, tt.expectRollback)
			}
		})
	}
}
