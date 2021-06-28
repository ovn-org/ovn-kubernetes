package services

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

func TestServiceNeedsIdling(t *testing.T) {
	config.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
	}()

	tests := []struct {
		name        string
		annotations map[string]string
		needsIdling bool
	}{
		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"foo": "bar",
			},
			needsIdling: false,
		},
		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"idling.alpha.openshift.io/idled-at": "2021-03-25T21:58:54Z",
			},
			needsIdling: true,
		},

		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"k8s.ovn.org/idled-at": "2021-03-25T21:58:54Z",
			},
			needsIdling: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if svcNeedsIdling(tt.annotations) != tt.needsIdling {
				t.Errorf("needs Idling does not match")
			}
		})
	}

}
