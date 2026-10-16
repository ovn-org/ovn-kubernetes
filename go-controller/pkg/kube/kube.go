// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimssclientset "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpcloudnetworkclientset "github.com/openshift/client-go/cloudnetwork/clientset/versioned"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"

	adminpolicybasedrouteclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	egressfirewall "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
	egressqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressserviceclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
)

// InterfaceOVN represents the exported methods for dealing with getting/setting
// kubernetes and OVN resources
type InterfaceOVN interface {
	Interface
	UpdateEgressFirewall(egressfirewall *egressfirewall.EgressFirewall) error
	UpdateEgressIP(eIP *egressipv1.EgressIP) error
	PatchEgressIP(name string, patchData []byte) error
	GetEgressIP(name string) (*egressipv1.EgressIP, error)
	GetEgressIPs() ([]*egressipv1.EgressIP, error)
	GetEgressFirewalls() ([]*egressfirewall.EgressFirewall, error)
	CreateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error)
	UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error)
	DeleteCloudPrivateIPConfig(name string) error
	UpdateEgressServiceStatus(namespace, name, host string) error
	UpdateIPAMClaimIPs(updatedIPAMClaim *ipamclaimsapi.IPAMClaim) error
}

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationsOnPod(namespace, podName string, annotations map[string]interface{}) error
	SetAnnotationsOnService(namespace, serviceName string, annotations map[string]interface{}) error
	SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error
	SetAnnotationsOnNodeWithFieldManager(nodeName string, annotations map[string]interface{}, fieldManager string) error
	SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error
	SetLabelsOnNode(nodeName string, labels map[string]interface{}) error
	GetNode(name string) (*corev1.Node, error)
	PatchNode(old, new *corev1.Node) error
	PatchNodeStatus(old, new *corev1.Node) error
	PatchNodeStatusAnnotations(oldNode, newNode *corev1.Node) error
	PatchPodStatusAnnotations(oldPod, newPod *corev1.Pod) error
	// GetNodeForWindows should only be used for windows hybrid overlay binary and never in linux code
	GetNodeForWindows(name string) (*corev1.Node, error)
	GetNodesForWindows() ([]*corev1.Node, error)
	Events() kv1core.EventInterface
}

// Kube works with kube client only
// Implements Interface
type Kube struct {
	KClient kubernetes.Interface
}

// KubeOVN works with all kube and ovn resources
// Implements InterfaceOVN
type KubeOVN struct {
	Kube
	ANPClient            anpclientset.Interface
	EIPClient            egressipclientset.Interface
	EgressFirewallClient egressfirewallclientset.Interface
	CloudNetworkClient   ocpcloudnetworkclientset.Interface
	EgressServiceClient  egressserviceclientset.Interface
	APBRouteClient       adminpolicybasedrouteclientset.Interface
	EgressQoSClient      egressqosclientset.Interface
	IPAMClaimsClient     ipamclaimssclientset.Interface
	NADClient            nadclientset.Interface
	NetworkQoSClient     networkqosclientset.Interface
}

// SetAnnotationsOnPod takes the pod object and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnPod(namespace, podName string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	podDesc := namespace + "/" + podName
	klog.Infof("Setting annotations %v on pod %s", annotations, podDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on pod %s: %v", podDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on pod %s: %v", podDesc, err)
	}
	return err
}

type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func escapeJSONPatchPathKey(key string) string {
	key = strings.ReplaceAll(key, "~", "~0")
	return strings.ReplaceAll(key, "/", "~1")
}

// PatchPodStatusAnnotations patches only pod annotations through the status
// subresource using compare-and-retry semantics on the old pod state.
//
// The patch always includes a pod UID test so a same-name replacement can
// never receive annotations computed for the old pod. There are also two
// annotation concurrency cases to handle:
//  1. The annotation key already exists on the old pod. In that case we can use a
//     narrow JSON patch "test" on that specific key so we only retry if another
//     writer changed the same annotation.
//  2. The annotation key does not exist on the old pod. In that case a per-key
//     "test" cannot protect us because two stale writers could both issue an
//     unconditional "add" and the last one would win. If the annotations map
//     exists, compare that map instead of the Pod resourceVersion. This ignores
//     unrelated kubelet status updates while still detecting annotation writers.
//     If the map itself is absent, use a resourceVersion guard because JSON Patch
//     cannot test a missing parent before creating it.
//
// Real informer/API pods always have a resourceVersion. If a synthetic caller
// passes an object without one, we skip that extra guard and fall back to the
// narrower per-key tests that are available.
func (k *Kube) PatchPodStatusAnnotations(oldPod, newPod *corev1.Pod) error {
	if oldPod.Namespace != newPod.Namespace || oldPod.Name != newPod.Name {
		return fmt.Errorf("cannot patch annotations for different pods %s/%s and %s/%s",
			oldPod.Namespace, oldPod.Name, newPod.Namespace, newPod.Name)
	}

	changedKeys := make(map[string]struct{})
	for key, oldValue := range oldPod.Annotations {
		if newValue, ok := newPod.Annotations[key]; !ok || oldValue != newValue {
			changedKeys[key] = struct{}{}
		}
	}
	for key, newValue := range newPod.Annotations {
		if oldValue, ok := oldPod.Annotations[key]; !ok || oldValue != newValue {
			changedKeys[key] = struct{}{}
		}
	}
	if len(changedKeys) == 0 {
		return nil
	}

	keys := make([]string, 0, len(changedKeys))
	for key := range changedKeys {
		keys = append(keys, key)
	}

	ops := []jsonPatchOp{}
	if oldPod.UID != "" {
		ops = append(ops, jsonPatchOp{
			Op:    "test",
			Path:  "/metadata/uid",
			Value: string(oldPod.UID),
		})
	}
	requiresResourceVersionGuard := false
	requiresAnnotationsGuard := false
	if len(oldPod.Annotations) == 0 {
		ops = append(ops, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{},
		})
		requiresResourceVersionGuard = true
	}
	for _, key := range keys {
		path := "/metadata/annotations/" + escapeJSONPatchPathKey(key)
		oldValue, oldOK := oldPod.Annotations[key]
		newValue, newOK := newPod.Annotations[key]
		if oldOK {
			ops = append(ops, jsonPatchOp{
				Op:    "test",
				Path:  path,
				Value: oldValue,
			})
		} else if newOK {
			if len(oldPod.Annotations) == 0 {
				requiresResourceVersionGuard = true
			} else {
				requiresAnnotationsGuard = true
			}
		}
		switch {
		case newOK && oldOK:
			ops = append(ops, jsonPatchOp{
				Op:    "replace",
				Path:  path,
				Value: newValue,
			})
		case newOK:
			ops = append(ops, jsonPatchOp{
				Op:    "add",
				Path:  path,
				Value: newValue,
			})
		default:
			ops = append(ops, jsonPatchOp{
				Op:   "remove",
				Path: path,
			})
		}
	}
	if requiresAnnotationsGuard {
		ops = append([]jsonPatchOp{{
			Op:    "test",
			Path:  "/metadata/annotations",
			Value: oldPod.Annotations,
		}}, ops...)
	} else if requiresResourceVersionGuard && oldPod.ResourceVersion != "" {
		ops = append([]jsonPatchOp{{
			Op:    "test",
			Path:  "/metadata/resourceVersion",
			Value: oldPod.ResourceVersion,
		}}, ops...)
	}

	patchData, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("failed to marshal annotation patch for pod %s/%s: %w", oldPod.Namespace, oldPod.Name, err)
	}

	podDesc := oldPod.Namespace + "/" + oldPod.Name
	klog.Infof("Patching annotations on pod %s", podDesc)
	_, err = k.KClient.CoreV1().Pods(oldPod.Namespace).Patch(
		context.TODO(),
		oldPod.Name,
		types.JSONPatchType,
		patchData,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		klog.Errorf("Error in patching annotations on pod %s: %v", podDesc, err)
	}
	return err
}

// SetAnnotationsOnNode takes the node name and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error {
	return k.SetAnnotationsOnNodeWithFieldManager(nodeName, annotations, "")
}

// SetAnnotationsOnNodeWithFieldManager is like SetAnnotationsOnNode but allows
// specifying a field manager for the patch operation. If fieldManager is empty,
// the server default is used.
func (k *Kube) SetAnnotationsOnNodeWithFieldManager(nodeName string, annotations map[string]interface{}, fieldManager string) error {
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on node %s", annotations, nodeName)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on node %s: %v", nodeName, err)
		return err
	}

	patchOpts := metav1.PatchOptions{}
	if fieldManager != "" {
		patchOpts.FieldManager = fieldManager
	}

	_, err = k.KClient.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.StrategicMergePatchType, patchData, patchOpts, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on node %s: %v", nodeName, err)
	}
	return err
}

// SetAnnotationsOnNamespace takes the namespace name and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on namespace %s", annotations, namespaceName)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on namespace %s: %v", namespaceName, err)
		return err
	}

	_, err = k.KClient.CoreV1().Namespaces().Patch(context.TODO(), namespaceName, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on namespace %s: %v", namespaceName, err)
	}
	return err
}

// SetAnnotationsOnService takes a service namespace and name and a map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnService(namespace, name string, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	serviceDesc := namespace + "/" + name
	klog.Infof("Setting annotations %v on service %s", annotations, serviceDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on service %s: %v", serviceDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Services(namespace).Patch(context.TODO(), name, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("Error in setting annotation on service %s: %v", serviceDesc, err)
	}
	return err
}

// SetLabelsOnNode takes the node name and map of key/value string pairs to set as labels
func (k *Kube) SetLabelsOnNode(nodeName string, labels map[string]interface{}) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"labels": labels,
		},
	}

	klog.V(4).Infof("Setting labels %v on node %s", labels, nodeName)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting labels on node %s: %v", nodeName, err)
		return err
	}

	_, err = k.KClient.CoreV1().Nodes().PatchStatus(context.TODO(), nodeName, patchData)
	return err
}

// PatchNode patches the old node object with the changes provided in the new node object.
func (k *Kube) PatchNode(old, new *corev1.Node) error {
	oldNodeObjectJson, err := json.Marshal(old)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", old.Name, err)
		return err
	}

	newNodeObjectJson, err := json.Marshal(new)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", new.Name, err)
		return err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNodeObjectJson, newNodeObjectJson, corev1.Node{})
	if err != nil {
		klog.Errorf("Unable to patch node %s: %v", old.Name, err)
		return err
	}

	if _, err = k.KClient.CoreV1().Nodes().Patch(context.TODO(), old.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		klog.Errorf("Unable to patch node %s: %v", old.Name, err)
		return err
	}

	return nil
}

// getNodeAPITimeout bounds Kubernetes API server reads for a single node so a
// slow or unresponsive apiserver cannot block a caller indefinitely.
const getNodeAPITimeout = 30 * time.Second

// GetNode returns the Node resource from the kubernetes apiserver, given its name.
func (k *Kube) GetNode(name string) (*corev1.Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), getNodeAPITimeout)
	defer cancel()
	return k.KClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

// PatchNodeStatus patches the node status subresource using a strategic merge
// patch computed from the old and new node objects. Only the fields that differ
// between old and new are sent to the API server, so informer-trimmed fields
// that are identical in both objects do not overwrite the server state.
//
// ResourceVersion from the new object is included in the patch to enable
// optimistic conflict detection: the API server rejects the patch with 409
// if the server's current resourceVersion differs, preventing silent
// overwrites from stale data.
func (k *Kube) PatchNodeStatus(old, new *corev1.Node) error {
	// Clear ResourceVersion from old so it appears in the computed diff.
	// This makes the patch carry new's resourceVersion, enabling the API
	// server to reject stale updates with 409 Conflict.
	oldForPatch := old.DeepCopy()
	oldForPatch.ResourceVersion = ""

	oldNodeObjectJson, err := json.Marshal(oldForPatch)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", old.Name, err)
		return err
	}

	newNodeObjectJson, err := json.Marshal(new)
	if err != nil {
		klog.Errorf("Unable to marshal node %s: %v", new.Name, err)
		return err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNodeObjectJson, newNodeObjectJson, corev1.Node{})
	if err != nil {
		klog.Errorf("Unable to create patch for node %s: %v", old.Name, err)
		return err
	}

	if _, err = k.KClient.CoreV1().Nodes().Patch(context.TODO(), old.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "status"); err != nil {
		klog.Errorf("Unable to patch node status %s: %v", old.Name, err)
		return err
	}

	return nil
}

// PatchNodeStatusAnnotations patches only node annotations through the status
// subresource using compare-and-retry semantics on the old node state. This
// avoids sending the full node status (which may have fields trimmed by the
// informer transform) back to the API server.
//
// There are two concurrency cases to handle:
//  1. The annotation key already exists on the old node. In that case we can use a
//     narrow JSON patch "test" on that specific key so we only retry if another
//     writer changed the same annotation.
//  2. The annotation key does not exist on the old node. In that case a per-key
//     "test" cannot protect us because two stale writers could both issue an
//     unconditional "add" and the last one would win. For that create case we add
//     a resourceVersion guard so only one writer based on that node snapshot can
//     create the missing key; losers will retry from the latest node state and
//     recompute a merged annotation value.
//
// Real informer/API nodes always have a resourceVersion. If a synthetic caller
// passes an object without one, we skip that extra guard and fall back to the
// narrower per-key tests that are available.
func (k *Kube) PatchNodeStatusAnnotations(oldNode, newNode *corev1.Node) error {
	if oldNode.Name != newNode.Name {
		return fmt.Errorf("cannot patch annotations for different nodes %s and %s",
			oldNode.Name, newNode.Name)
	}

	changedKeys := make(map[string]struct{})
	for key, oldValue := range oldNode.Annotations {
		if newValue, ok := newNode.Annotations[key]; !ok || oldValue != newValue {
			changedKeys[key] = struct{}{}
		}
	}
	for key, newValue := range newNode.Annotations {
		if oldValue, ok := oldNode.Annotations[key]; !ok || oldValue != newValue {
			changedKeys[key] = struct{}{}
		}
	}
	if len(changedKeys) == 0 {
		return nil
	}

	keys := make([]string, 0, len(changedKeys))
	for key := range changedKeys {
		keys = append(keys, key)
	}

	ops := []jsonPatchOp{}
	requiresResourceVersionGuard := false
	if len(oldNode.Annotations) == 0 {
		ops = append(ops, jsonPatchOp{
			Op:    "add",
			Path:  "/metadata/annotations",
			Value: map[string]string{},
		})
		requiresResourceVersionGuard = true
	}
	for _, key := range keys {
		path := "/metadata/annotations/" + escapeJSONPatchPathKey(key)
		oldValue, oldOK := oldNode.Annotations[key]
		newValue, newOK := newNode.Annotations[key]
		if oldOK {
			ops = append(ops, jsonPatchOp{
				Op:    "test",
				Path:  path,
				Value: oldValue,
			})
		} else if newOK {
			requiresResourceVersionGuard = true
		}
		switch {
		case newOK && oldOK:
			ops = append(ops, jsonPatchOp{
				Op:    "replace",
				Path:  path,
				Value: newValue,
			})
		case newOK:
			ops = append(ops, jsonPatchOp{
				Op:    "add",
				Path:  path,
				Value: newValue,
			})
		default:
			ops = append(ops, jsonPatchOp{
				Op:   "remove",
				Path: path,
			})
		}
	}
	if requiresResourceVersionGuard && oldNode.ResourceVersion != "" {
		ops = append([]jsonPatchOp{{
			Op:    "test",
			Path:  "/metadata/resourceVersion",
			Value: oldNode.ResourceVersion,
		}}, ops...)
	}

	patchData, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("failed to marshal annotation patch for node %s: %w", oldNode.Name, err)
	}

	klog.Infof("Patching annotations on node %s", oldNode.Name)
	_, err = k.KClient.CoreV1().Nodes().Patch(
		context.TODO(),
		oldNode.Name,
		types.JSONPatchType,
		patchData,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		klog.Errorf("Error in patching annotations on node %s: %v", oldNode.Name, err)
	}
	return err
}

// GetNodesForWindows returns the list of all Node objects from kubernetes. Only used by windows binary.
func (k *Kube) GetNodesForWindows() ([]*corev1.Node, error) {
	list := []*corev1.Node{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.KClient.CoreV1().Nodes().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*corev1.Node))
		return nil
	})
	return list, err
}

// GetNodeForWindows returns the Node resource from kubernetes apiserver, given its name. Only used by windows binary.
func (k *Kube) GetNodeForWindows(name string) (*corev1.Node, error) {
	return k.KClient.CoreV1().Nodes().Get(context.TODO(), name, metav1.GetOptions{})
}

// Events returns events to use when creating an EventSinkImpl
func (k *Kube) Events() kv1core.EventInterface {
	return k.KClient.CoreV1().Events("")
}

// UpdateEgressFirewall updates the EgressFirewall with the provided EgressFirewall data
func (k *KubeOVN) UpdateEgressFirewall(egressfirewall *egressfirewall.EgressFirewall) error {
	klog.Infof("Updating status on EgressFirewall %s in namespace %s", egressfirewall.Name, egressfirewall.Namespace)
	_, err := k.EgressFirewallClient.K8sV1().EgressFirewalls(egressfirewall.Namespace).Update(context.TODO(), egressfirewall, metav1.UpdateOptions{})
	return err
}

// UpdateEgressIP updates the EgressIP with the provided EgressIP data
func (k *KubeOVN) UpdateEgressIP(eIP *egressipv1.EgressIP) error {
	klog.Infof("Updating status on EgressIP %s status %v", eIP.Name, eIP.Status)
	_, err := k.EIPClient.K8sV1().EgressIPs().Update(context.TODO(), eIP, metav1.UpdateOptions{})
	return err
}

func (k *KubeOVN) PatchEgressIP(name string, patchData []byte) error {
	_, err := k.EIPClient.K8sV1().EgressIPs().Patch(context.TODO(), name, types.JSONPatchType, patchData, metav1.PatchOptions{})
	return err
}

// GetEgressIP returns the EgressIP object from kubernetes
func (k *KubeOVN) GetEgressIP(name string) (*egressipv1.EgressIP, error) {
	return k.EIPClient.K8sV1().EgressIPs().Get(context.TODO(), name, metav1.GetOptions{})
}

// GetEgressIPs returns the list of all EgressIP objects from kubernetes
func (k *KubeOVN) GetEgressIPs() ([]*egressipv1.EgressIP, error) {
	list := []*egressipv1.EgressIP{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.EIPClient.K8sV1().EgressIPs().List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*egressipv1.EgressIP))
		return nil
	})
	return list, err
}

// GetEgressFirewalls returns the list of all EgressFirewall objects from kubernetes
func (k *KubeOVN) GetEgressFirewalls() ([]*egressfirewall.EgressFirewall, error) {
	list := []*egressfirewall.EgressFirewall{}
	err := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return k.EgressFirewallClient.K8sV1().EgressFirewalls(metav1.NamespaceAll).List(ctx, opts)
	}).EachListItem(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
	}, func(obj runtime.Object) error {
		list = append(list, obj.(*egressfirewall.EgressFirewall))
		return nil
	})
	return list, err
}

func (k *KubeOVN) CreateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error) {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Create(context.TODO(), cloudPrivateIPConfig, metav1.CreateOptions{})
}

func (k *KubeOVN) UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *ocpcloudnetworkapi.CloudPrivateIPConfig) (*ocpcloudnetworkapi.CloudPrivateIPConfig, error) {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Update(context.TODO(), cloudPrivateIPConfig, metav1.UpdateOptions{})
}

func (k *KubeOVN) DeleteCloudPrivateIPConfig(name string) error {
	return k.CloudNetworkClient.CloudV1().CloudPrivateIPConfigs().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (k *KubeOVN) UpdateEgressServiceStatus(namespace, name, host string) error {
	es, err := k.EgressServiceClient.K8sV1().EgressServices(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	es.Status.Host = host

	_, err = k.EgressServiceClient.K8sV1().EgressServices(es.Namespace).UpdateStatus(context.TODO(), es, metav1.UpdateOptions{})
	return err
}

func (k *KubeOVN) UpdateIPAMClaimIPs(updatedIPAMClaim *ipamclaimsapi.IPAMClaim) error {
	_, err := k.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(updatedIPAMClaim.Namespace).UpdateStatus(context.TODO(), updatedIPAMClaim, metav1.UpdateOptions{})
	return err
}

// SetAnnotationsOnNAD takes a NAD namespace and name and a map of key/value string pairs to set as annotations
func (k *KubeOVN) SetAnnotationsOnNAD(namespace, name string, annotations map[string]string, fieldManager string) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	patchData, err = json.Marshal(&patch)
	if err != nil {
		return err
	}

	patchOptions := metav1.PatchOptions{}
	if fieldManager != "" {
		patchOptions.FieldManager = fieldManager
	}

	_, err = k.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Patch(context.Background(), name, types.MergePatchType, patchData, patchOptions)
	return err
}
