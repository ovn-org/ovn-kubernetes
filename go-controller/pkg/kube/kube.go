package kube

import (
	"encoding/json"
	"k8s.io/klog"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationsOnPod(pod *kapi.Pod, annotations map[string]string) error
	SetAnnotationsOnNode(node *kapi.Node, annotations map[string]interface{}) error
	UpdateNodeStatus(node *kapi.Node) error
	GetAnnotationsOnPod(namespace, name string) (map[string]string, error)
	GetNodes() (*kapi.NodeList, error)
	GetNode(name string) (*kapi.Node, error)
	GetEndpoint(namespace, name string) (*kapi.Endpoints, error)
	CreateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error)
	Events() kv1core.EventInterface
}

// Kube is the structure object upon which the Interface is implemented
type Kube struct {
	KClient kubernetes.Interface
}

// SetAnnotationsOnPod takes the pod object and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnPod(pod *kapi.Pod, annotations map[string]string) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Setting annotations %v on pod %s", annotations, podDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on pod %s: %v", podDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, patchData)
	if err != nil {
		klog.Errorf("Error in setting annotation on pod %s: %v", podDesc, err)
	}
	return err
}

// SetAnnotationsOnNode takes the node object and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNode(node *kapi.Node, annotations map[string]interface{}) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on node %s", annotations, node.Name)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on node %s: %v", node.Name, err)
		return err
	}

	_, err = k.KClient.CoreV1().Nodes().Patch(node.Name, types.MergePatchType, patchData)
	if err != nil {
		klog.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

// UpdateNodeStatus takes the node object and sets the provided update status
func (k *Kube) UpdateNodeStatus(node *kapi.Node) error {
	klog.Infof("Updating status on node %s", node.Name)
	_, err := k.KClient.CoreV1().Nodes().UpdateStatus(node)
	if err != nil {
		klog.Errorf("Error in updating status on node %s: %v", node.Name, err)
	}
	return err
}

// GetAnnotationsOnPod obtains the pod annotations from kubernetes apiserver, given the name and namespace
func (k *Kube) GetAnnotationsOnPod(namespace, name string) (map[string]string, error) {
	pod, err := k.KClient.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod.ObjectMeta.Annotations, nil
}

// GetNodes returns the list of all Node objects from kubernetes
func (k *Kube) GetNodes() (*kapi.NodeList, error) {
	return k.KClient.CoreV1().Nodes().List(metav1.ListOptions{})
}

// GetNode returns the Node resource from kubernetes apiserver, given its name
func (k *Kube) GetNode(name string) (*kapi.Node, error) {
	return k.KClient.CoreV1().Nodes().Get(name, metav1.GetOptions{})
}

// GetEndpoint returns the Endpoints resource
func (k *Kube) GetEndpoint(namespace, name string) (*kapi.Endpoints, error) {
	return k.KClient.CoreV1().Endpoints(namespace).Get(name, metav1.GetOptions{})
}

// CreateEndpoint creates the Endpoints resource
func (k *Kube) CreateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error) {
	return k.KClient.CoreV1().Endpoints(namespace).Create(ep)
}

// Events returns events to use when creating an EventSinkImpl
func (k *Kube) Events() kv1core.EventInterface {
	return k.KClient.CoreV1().Events("")
}
