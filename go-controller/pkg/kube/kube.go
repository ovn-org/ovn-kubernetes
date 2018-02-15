package kube

import (
	"fmt"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationOnPod(pod *kapi.Pod, key, value string) error
	SetAnnotationOnNode(node *kapi.Node, key, value string) error
	GetAnnotationsOnPod(namespace, name string) (map[string]string, error)
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetPods(namespace string) (*kapi.PodList, error)
	GetPodsByLabels(namespace string, selector labels.Selector) (*kapi.PodList, error)
	GetNodes() (*kapi.NodeList, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
}

// Kube is the structure object upon which the Interface is implemented
type Kube struct {
	KClient kubernetes.Interface
}

// SetAnnotationOnPod takes the pod object and key/value string pair to set it as an annotation
func (k *Kube) SetAnnotationOnPod(pod *kapi.Pod, key, value string) error {
	logrus.Infof("Setting annotations %s=%s on pod %s", key, value, pod.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		logrus.Errorf("Error in setting annotation on pod %s/%s: %v", pod.Name, pod.Namespace, err)
	}
	return err
}

// SetAnnotationOnNode takes the node object and key/value string pair to set it as an annotation
func (k *Kube) SetAnnotationOnNode(node *kapi.Node, key, value string) error {
	logrus.Infof("Setting annotations %s=%s on node %s", key, value, node.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Nodes().Patch(node.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		logrus.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

// GetAnnotationsOnPod obtains the pod annotations from kubernetes apiserver, given the name and namespace
func (k *Kube) GetAnnotationsOnPod(namespace, name string) (map[string]string, error) {
	pod, err := k.KClient.Core().Pods(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod.ObjectMeta.Annotations, nil
}

// GetPod obtains the Pod resource from kubernetes apiserver, given the name and namespace
func (k *Kube) GetPod(namespace, name string) (*kapi.Pod, error) {
	return k.KClient.Core().Pods(namespace).Get(name, metav1.GetOptions{})
}

// GetPods obtains the Pod resource from kubernetes apiserver, given the name and namespace
func (k *Kube) GetPods(namespace string) (*kapi.PodList, error) {
	return k.KClient.Core().Pods(namespace).List(metav1.ListOptions{})
}

// GetPodsByLabels obtains the Pod resources from kubernetes apiserver,
// given the namespace and label
func (k *Kube) GetPodsByLabels(namespace string, selector labels.Selector) (*kapi.PodList, error) {
	options := metav1.ListOptions{}
	options.LabelSelector = selector.String()
	return k.KClient.Core().Pods(namespace).List(options)
}

// GetNodes returns the list of all Node objects from kubernetes
func (k *Kube) GetNodes() (*kapi.NodeList, error) {
	return k.KClient.Core().Nodes().List(metav1.ListOptions{})
}

// GetNode returns the Node resource from kubernetes apiserver, given its name
func (k *Kube) GetNode(name string) (*kapi.Node, error) {
	return k.KClient.Core().Nodes().Get(name, metav1.GetOptions{})
}

// GetService returns the Service resource from kubernetes apiserver, given its name and namespace
func (k *Kube) GetService(namespace, name string) (*kapi.Service, error) {
	return k.KClient.Core().Services(namespace).Get(name, metav1.GetOptions{})
}
