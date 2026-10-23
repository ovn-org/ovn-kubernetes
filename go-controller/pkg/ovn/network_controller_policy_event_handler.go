// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// newNetpolRetryFramework builds and returns a retry framework for the input resource
// type and assigns all ovnkube-controller-specific function attributes in the returned struct;
// these functions will then be called by the retry logic in the retry package when
// WatchResource() is called.
// newNetpolRetryFramework takes as input a resource type (required)
// and the following optional parameters: a namespace and a label filter for the
// shared informer, a sync function to process all objects of this type at startup,
// and resource-specific extra parameters (used now for network-policy-dependant types).
// newNetpolRetryFramework is also called directly by the watchers that are
// dynamically created when a network policy is added:
// LocalPodSelectorType
func (bnc *BaseNetworkController) newNetpolRetryFramework(
	objectType reflect.Type,
	syncFunc func([]interface{}) error,
	extraParameters interface{},
	stopChan <-chan struct{}) *retry.RetryFramework {
	eventHandler := &networkControllerPolicyEventHandler{
		objType:         objectType,
		watchFactory:    bnc.watchFactory,
		bnc:             bnc,
		extraParameters: extraParameters, // in use by network policy dynamic watchers
		syncFunc:        syncFunc,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          hasPolicyResourceAnUpdateFunc(objectType),
		NeedsUpdateDuringRetry: needsPolicyResourceUpdateDuringRetry(objectType),
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return retry.NewRetryFramework(
		bnc.GetNetworkName()+"/netPolController",
		stopChan,
		bnc.wg,
		bnc.watchFactory,
		resourceHandler,
	)
}

// event handlers handles policy related events
type networkControllerPolicyEventHandler struct {
	retry.DefaultEventHandler
	watchFactory    *factory.WatchFactory
	objType         reflect.Type
	bnc             *BaseNetworkController
	extraParameters interface{}
	syncFunc        func([]interface{}) error
}

func (h *networkControllerPolicyEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// AreResourcesEqual returns true if, given two objects of a known resource type, the update logic for this resource
// type considers them equal and therefore no update is needed. It returns false when the two objects are not considered
// equal and an update needs be executed. This is regardless of how the update is carried out (whether with a dedicated update
// function or with a delete on the old obj followed by an add on the new obj).
func (h *networkControllerPolicyEventHandler) AreResourcesEqual(_, _ interface{}) (bool, error) {
	// switch based on type
	switch h.objType {
	case factory.LocalPodSelectorType: //
		// For these types, there was no old vs new obj comparison in the original update code,
		// so pretend they're always different so that the update code gets executed
		return false, nil
	}

	return false, fmt.Errorf("no object comparison for type %s", h.objType)
}

// GetInternalCacheEntry returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (h *networkControllerPolicyEventHandler) GetInternalCacheEntry(_ interface{}) interface{} {
	return nil
}

// GetResourceFromInformerCache returns the latest state of the object, given an object key and its type.
// from the informers cache.
func (h *networkControllerPolicyEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var namespace, name string
	var err error

	namespace, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.LocalPodSelectorType:
		obj, err = h.watchFactory.GetPod(namespace, name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}

// AddResource adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation.
// Given an object to add and a boolean specifying if the function was executed from iterateRetryResources
func (h *networkControllerPolicyEventHandler) AddResource(_ context.Context, obj interface{}, _ bool) error {
	switch h.objType {
	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.bnc.handleLocalPodSelectorAddFunc(extraParameters.np,
			obj)

	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
}

func hasPolicyResourceAnUpdateFunc(objType reflect.Type) bool {
	switch objType {
	case factory.LocalPodSelectorType:
		return true
	}
	return false
}

// UpdateResource updates the specified object in the cluster to its version in newObj according to its
// type and returns the error, if any, yielded during the object update.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (h *networkControllerPolicyEventHandler) UpdateResource(_ context.Context, _, newObj interface{}, _ bool) error {
	switch h.objType {
	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.bnc.handleLocalPodSelectorAddFunc(extraParameters.np,
			newObj)
	}
	return fmt.Errorf("no update function for object type %s", h.objType)
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods and network policies.
func (h *networkControllerPolicyEventHandler) DeleteResource(_ context.Context, obj, _ interface{}) error {
	switch h.objType {
	case factory.LocalPodSelectorType:
		extraParameters := h.extraParameters.(*NetworkPolicyExtraParameters)
		return h.bnc.handleLocalPodSelectorDelFunc(extraParameters.np,
			obj)

	default:
		return fmt.Errorf("object type %s not supported", h.objType)
	}
}

func (h *networkControllerPolicyEventHandler) SyncFunc(_ context.Context, objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.LocalPodSelectorType:
			syncFunc = nil

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// IsObjectInTerminalState returns true if the given object is a in terminal state.
// This is used now for pods that are either in a PodSucceeded or in a PodFailed state.
func (h *networkControllerPolicyEventHandler) IsObjectInTerminalState(obj interface{}) bool {
	switch h.objType {
	case factory.LocalPodSelectorType:
		pod := obj.(*corev1.Pod)
		return util.PodCompleted(pod)

	default:
		return false
	}
}

func needsPolicyResourceUpdateDuringRetry(_ reflect.Type) bool {
	return false
}
