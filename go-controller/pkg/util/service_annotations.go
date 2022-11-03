package util

import (
	"encoding/json"
	"fmt"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	EgressSVCAnnotation     = "k8s.ovn.org/egress-service"
	EgressSVCHostAnnotation = "k8s.ovn.org/egress-service-host"
	EgressSVCLabelPrefix    = "egress-service.k8s.ovn.org"
)

type EgressSVCConfig struct {
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

// ParseEgressSVCAnnotation returns the parsed egress-service annotation.
func ParseEgressSVCAnnotation(annotations map[string]string) (*EgressSVCConfig, error) {
	anno, ok := annotations[EgressSVCAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found", EgressSVCAnnotation)
	}

	cfg := &EgressSVCConfig{}
	if err := json.Unmarshal([]byte(anno), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal egress svc config annotation value %s: %v", anno, err)
	}

	_, err := metav1.LabelSelectorAsSelector(&cfg.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the nodeSelector: %v", err)
	}

	return cfg, nil
}

// HasEgressSVCAnnotation returns true if the service has an egress-service
// config annotation.
func HasEgressSVCAnnotation(svc *kapi.Service) bool {
	_, ok := svc.Annotations[EgressSVCAnnotation]
	return ok
}

// HasEgressSVCHostAnnotation returns true if the service has an egress-service-host
// annotation.
func HasEgressSVCHostAnnotation(svc *kapi.Service) bool {
	_, ok := svc.Annotations[EgressSVCHostAnnotation]
	return ok
}

// GetEgressSVCHost returns the egress-service-host annotation value.
func GetEgressSVCHost(svc *kapi.Service) (string, error) {
	host, ok := svc.Annotations[EgressSVCHostAnnotation]
	if !ok {
		return "", newAnnotationNotSetError("%s annotation not found for service %s/%s", EgressSVCHostAnnotation, svc.Namespace, svc.Name)
	}

	return host, nil
}

// EgressSVCHostChanged returns true if both services have the same
// egress-service-host annotation value.
func EgressSVCHostChanged(oldSVC, newSVC *kapi.Service) bool {
	return oldSVC.Annotations[EgressSVCHostAnnotation] != newSVC.Annotations[EgressSVCHostAnnotation]
}
