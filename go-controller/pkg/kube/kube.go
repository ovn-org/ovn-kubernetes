package kube

import (
	"fmt"
	"strings"

	"encoding/json"
	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationOnPod(pod *kapi.Pod, key, value string) error
	SetAnnotationsOnPod(pod *kapi.Pod, annotations map[string]string) error
	SetAnnotationOnNode(node *kapi.Node, key, value string) error
	SetAnnotationsOnNode(node *kapi.Node, annotations map[string]string) error
	UpdateNodeStatus(node *kapi.Node) error
	GetAnnotationsOnPod(namespace, name string) (map[string]string, error)
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetPods(namespace string) (*kapi.PodList, error)
	GetPodsByLabels(namespace string, selector labels.Selector) (*kapi.PodList, error)
	GetNodes() (*kapi.NodeList, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
	GetEndpoints(namespace string) (*kapi.EndpointsList, error)
	GetEndpoint(namespace, name string) (*kapi.Endpoints, error)
	CreateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error)
	UpdateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error)
	GetNamespaces() (*kapi.NamespaceList, error)
	Events() kv1core.EventInterface
}

// Kube is the structure object upon which the Interface is implemented
type Kube struct {
	KClient kubernetes.Interface
}

// SetAnnotationOnPod takes the pod object and key/value string pair to set it as an annotation
func (k *Kube) SetAnnotationOnPod(pod *kapi.Pod, key, value string) error {
	logrus.Infof("Setting annotations %s=%s on pod %s", key, value, pod.Name)
	// escape double quotes in the annotation value so it can be sent as a JSON patch
	value = strings.Replace(value, "\"", "\\\"", -1)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		logrus.Errorf("Error in setting annotation on pod %s/%s: %v", pod.Name, pod.Namespace, err)
	}
	return err
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
	logrus.Infof("Setting annotations %v on pod %s", annotations, podDesc)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		logrus.Errorf("Error in setting annotations on pod %s: %v", podDesc, err)
		return err
	}

	_, err = k.KClient.CoreV1().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, patchData)
	if err != nil {
		logrus.Errorf("Error in setting annotation on pod %s: %v", podDesc, err)
	}
	return err
}

// SetAnnotationOnNode takes the node object and key/value string pair to set it as an annotation
func (k *Kube) SetAnnotationOnNode(node *kapi.Node, key, value string) error {
	logrus.Infof("Setting annotations %s=%s on node %s", key, value, node.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.CoreV1().Nodes().Patch(node.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		logrus.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

// SetAnnotationsOnNode takes the node object and map of key/value string pairs to set as annotations
func (k *Kube) SetAnnotationsOnNode(node *kapi.Node, annotations map[string]string) error {
	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]interface{} `json:"metadata"`
	}{
		Metadata: map[string]interface{}{
			"annotations": annotations,
		},
	}

	logrus.Infof("Setting annotations %v on node %s", annotations, node.Name)
	patchData, err = json.Marshal(&patch)
	if err != nil {
		logrus.Errorf("Error in setting annotations on node %s: %v", node.Name, err)
		return err
	}

	_, err = k.KClient.CoreV1().Nodes().Patch(node.Name, types.MergePatchType, patchData)
	if err != nil {
		logrus.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

// UpdateNodeStatus takes the node object and sets the provided update status
func (k *Kube) UpdateNodeStatus(node *kapi.Node) error {
	logrus.Infof("Updating status on node %s", node.Name)
	_, err := k.KClient.CoreV1().Nodes().UpdateStatus(node)
	if err != nil {
		logrus.Errorf("Error in updating status on node %s: %v", node.Name, err)
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

// GetPod obtains the Pod resource from kubernetes apiserver, given the name and namespace
func (k *Kube) GetPod(namespace, name string) (*kapi.Pod, error) {
	return k.KClient.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
}

// GetPods obtains the Pod resource from kubernetes apiserver, given the name and namespace
func (k *Kube) GetPods(namespace string) (*kapi.PodList, error) {
	return k.KClient.CoreV1().Pods(namespace).List(metav1.ListOptions{})
}

// GetPodsByLabels obtains the Pod resources from kubernetes apiserver,
// given the namespace and label
func (k *Kube) GetPodsByLabels(namespace string, selector labels.Selector) (*kapi.PodList, error) {
	options := metav1.ListOptions{}
	options.LabelSelector = selector.String()
	return k.KClient.CoreV1().Pods(namespace).List(options)
}

// GetNodes returns the list of all Node objects from kubernetes
func (k *Kube) GetNodes() (*kapi.NodeList, error) {
	return k.KClient.CoreV1().Nodes().List(metav1.ListOptions{})
}

// GetNode returns the Node resource from kubernetes apiserver, given its name
func (k *Kube) GetNode(name string) (*kapi.Node, error) {
	return k.KClient.CoreV1().Nodes().Get(name, metav1.GetOptions{})
}

// GetService returns the Service resource from kubernetes apiserver, given its name and namespace
func (k *Kube) GetService(namespace, name string) (*kapi.Service, error) {
	return k.KClient.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
}

// GetEndpoints returns all the Endpoint resources from kubernetes
// apiserver, given namespace
func (k *Kube) GetEndpoints(namespace string) (*kapi.EndpointsList, error) {
	return k.KClient.CoreV1().Endpoints(namespace).List(metav1.ListOptions{})
}

// GetEndpoint returns the Endpoints resource
func (k *Kube) GetEndpoint(namespace, name string) (*kapi.Endpoints, error) {
	return k.KClient.CoreV1().Endpoints(namespace).Get(name, metav1.GetOptions{})
}

// CreateEndpoint creates the Endpoints resource
func (k *Kube) CreateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error) {
	return k.KClient.CoreV1().Endpoints(namespace).Create(ep)
}

// UpdateEndpoint updates the Endpoints resource
func (k *Kube) UpdateEndpoint(namespace string, ep *kapi.Endpoints) (*kapi.Endpoints, error) {
	return k.KClient.CoreV1().Endpoints(namespace).Update(ep)
}

// GetNamespaces returns all Namespace resource from kubernetes apiserver
func (k *Kube) GetNamespaces() (*kapi.NamespaceList, error) {
	return k.KClient.CoreV1().Namespaces().List(metav1.ListOptions{})
}

// Events returns events to use when creating an EventSinkImpl
func (k *Kube) Events() kv1core.EventInterface {
	return k.KClient.CoreV1().Events("")
}
