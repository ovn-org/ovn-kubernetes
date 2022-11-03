package util

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseEgressSVCAnnotation(t *testing.T) {
	tests := []struct {
		desc        string
		annotations map[string]string
		errMatch    error
	}{
		{
			desc: "valid empty annotation should work",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{}",
			},
		},
		{
			desc: "valid annotation with matchLabels in nodeSelector should work",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{\"nodeSelector\":{\"matchLabels\":{\"happy\": \"true\"}}}",
			},
		},
		{
			desc: "valid annotation with matchLabels in nodeSelector should work",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{\"nodeSelector\":{\"matchExpressions\":[{\"key\": \"happy\",\"operator\": \"In\",\"values\":[\"true\"]}]}}",
			},
		},
		{
			desc:        "missing annotation should fail",
			annotations: nil,
			errMatch:    fmt.Errorf("k8s.ovn.org/egress-service annotation not found"),
		},
		{
			desc: "invalid annotation should fail",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{&&}",
			},
			errMatch: fmt.Errorf("failed to unmarshal egress svc config"),
		},
		{
			desc: "invalid matchLabels should fail",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{\"nodeSelector\":{\"matchLabels\":{\"$hould\": \"F@il\"}}}",
			},
			errMatch: fmt.Errorf("failed to parse the nodeSelector"),
		},
		{
			desc: "invalid matchExpressions should fail",
			annotations: map[string]string{
				"k8s.ovn.org/egress-service": "{\"nodeSelector\":{\"matchExpressions\":[{\"key\": \"sad\",\"operator\": \"rainy\",\"values\":[\"true\"]}]}}",
			},
			errMatch: fmt.Errorf("failed to parse the nodeSelector"),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := ParseEgressSVCAnnotation(tc.annotations)
			t.Log(res)
			if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, e)
				assert.NotNil(t, res)
			}
		})
	}
}

func TestHasEgressServiceAnnotation(t *testing.T) {
	tests := []struct {
		desc     string
		svc      *kapi.Service
		expected bool
	}{
		{
			desc: "a service with the annotation should be considered an egress service",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service": "{}",
					},
				},
			},
			expected: true,
		},
		{
			desc: "a service without the proper annotation should not be considered an egress service",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"unrelated": "value",
					},
				},
			},
			expected: false,
		},
		{
			desc:     "a service without annotations should not be considered an egress service",
			svc:      &kapi.Service{},
			expected: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := HasEgressSVCAnnotation(tc.svc)
			t.Log(res)
			assert.Equal(t, res, tc.expected)
		})
	}
}

func TestHasEgressSVCHostAnnotation(t *testing.T) {
	tests := []struct {
		desc     string
		svc      *kapi.Service
		expected bool
	}{
		{
			desc: "a service with the annotation should be considered as one that has a host",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			expected: true,
		},
		{
			desc: "a service without the proper annotation should not be considered as having a host",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"unrelated": "value",
					},
				},
			},
			expected: false,
		},
		{
			desc:     "a service without annotations should not be considered as having a host",
			svc:      &kapi.Service{},
			expected: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := HasEgressSVCHostAnnotation(tc.svc)
			t.Log(res)
			assert.Equal(t, res, tc.expected)
		})
	}
}

func TestGetEgressSVCHost(t *testing.T) {
	tests := []struct {
		desc     string
		svc      *kapi.Service
		expected string
		errMatch error
	}{
		{
			desc: "a service with the annotation should be considered as one that has a host",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			expected: "dummy",
		},
		{
			desc: "a service without the proper annotation should not return a host",
			svc: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"unrelated": "value",
					},
				},
			},
			errMatch: fmt.Errorf("annotation not found for service"),
		},
		{
			desc:     "a service without annotations should not return a host",
			svc:      &kapi.Service{},
			errMatch: fmt.Errorf("annotation not found for service"),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := GetEgressSVCHost(tc.svc)
			t.Log(res)
			if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				assert.Nil(t, e)
				assert.NotNil(t, res)
			}
		})
	}
}

func TestEgressSVCHostChanged(t *testing.T) {
	tests := []struct {
		desc     string
		svc1     *kapi.Service
		svc2     *kapi.Service
		expected bool
	}{
		{
			desc: "same host should be false",
			svc1: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			svc2: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			expected: false,
		},
		{
			desc: "different host should be true",
			svc1: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			svc2: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy2",
					},
				},
			},
			expected: true,
		},
		{
			desc: "host and empty should be considered a change",
			svc1: &kapi.Service{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/egress-service-host": "dummy",
					},
				},
			},
			svc2:     &kapi.Service{},
			expected: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := EgressSVCHostChanged(tc.svc1, tc.svc2)
			t.Log(res)
			assert.Equal(t, res, tc.expected)
		})
	}
}
