package kube

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Annotator represents the exported methods for handling node annotations
// Implementations should enforce thread safety on the declared methods
type Annotator interface {
	Set(key string, value interface{}) error
	Delete(key string)
	Run() error
}

type nodeAnnotator struct {
	kube     Interface
	nodeName string

	changes map[string]interface{}
	sync.Mutex
}

// NewNodeAnnotator returns a new annotator for Node objects
func NewNodeAnnotator(kube Interface, nodeName string) Annotator {
	return &nodeAnnotator{
		kube:     kube,
		nodeName: nodeName,
		changes:  make(map[string]interface{}),
	}
}

func (na *nodeAnnotator) Set(key string, val interface{}) error {
	na.Lock()
	defer na.Unlock()

	if val == nil {
		na.changes[key] = nil
		return nil
	}

	// Annotations must be either a valid string value or nil; coerce
	// any non-empty values to string
	if reflect.TypeOf(val).Kind() == reflect.String {
		na.changes[key] = val.(string)
	} else {
		bytes, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("failed to marshal %q value %v to string: %v", key, val, err)
		}
		na.changes[key] = string(bytes)
	}

	return nil
}

func (na *nodeAnnotator) Delete(key string) {
	na.Lock()
	defer na.Unlock()
	na.changes[key] = nil
}

func (na *nodeAnnotator) Run() error {
	na.Lock()
	defer na.Unlock()
	if len(na.changes) == 0 {
		return nil
	}

	return na.kube.SetAnnotationsOnNode(na.nodeName, na.changes)
}

// NewPodAnnotator returns a new annotator for Pod objects
func NewPodAnnotator(kube Interface, podName string, namespace string) Annotator {
	return &podAnnotator{
		kube:      kube,
		podName:   podName,
		namespace: namespace,
		changes:   make(map[string]interface{}),
	}
}

type podAnnotator struct {
	kube      Interface
	podName   string
	namespace string

	changes map[string]interface{}
	sync.Mutex
}

func (pa *podAnnotator) Set(key string, val interface{}) error {
	pa.Lock()
	defer pa.Unlock()

	if val == nil {
		pa.changes[key] = nil
		return nil
	}

	// Annotations must be either a valid string value or nil; coerce
	// any non-empty values to string
	if reflect.TypeOf(val).Kind() == reflect.String {
		pa.changes[key] = val.(string)
	} else {
		bytes, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("failed to marshal %q value %v to string: %v", key, val, err)
		}
		pa.changes[key] = string(bytes)
	}

	return nil
}

func (pa *podAnnotator) Delete(key string) {
	pa.Lock()
	defer pa.Unlock()
	pa.changes[key] = nil
}

func (pa *podAnnotator) Run() error {
	pa.Lock()
	defer pa.Unlock()

	if len(pa.changes) == 0 {
		return nil
	}

	return pa.kube.SetAnnotationsOnPod(pa.namespace, pa.podName, pa.changes)
}

// NewNamespaceAnnotator returns a new annotator for Namespace objects
func NewNamespaceAnnotator(kube Interface, namespaceName string) Annotator {
	return &namespaceAnnotator{
		kube:          kube,
		namespaceName: namespaceName,
		changes:       make(map[string]interface{}),
	}
}

type namespaceAnnotator struct {
	kube          Interface
	namespaceName string

	changes map[string]interface{}
	sync.Mutex
}

func (na *namespaceAnnotator) Set(key string, val interface{}) error {
	na.Lock()
	defer na.Unlock()

	if val == nil {
		na.changes[key] = nil
		return nil
	}

	// Annotations must be either a valid string value or nil; coerce
	// any non-empty values to string
	if reflect.TypeOf(val).Kind() == reflect.String {
		na.changes[key] = val.(string)
	} else {
		bytes, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("failed to marshal %q value %v to string: %v", key, val, err)
		}
		na.changes[key] = string(bytes)
	}

	return nil
}

func (na *namespaceAnnotator) Delete(key string) {
	na.Lock()
	defer na.Unlock()
	na.changes[key] = nil
}

func (na *namespaceAnnotator) Run() error {
	na.Lock()
	defer na.Unlock()
	if len(na.changes) == 0 {
		return nil
	}

	return na.kube.SetAnnotationsOnNamespace(na.namespaceName, na.changes)
}
