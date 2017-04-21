package kube

import (
	"fmt"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kapi "k8s.io/client-go/pkg/api/v1"
)

// Interface represents the exported methods for dealing with getting/setting
// kubernetes resources
type Interface interface {
	SetAnnotationOnPod(pod *kapi.Pod, key, value string) error
	SetAnnotationOnNode(node *kapi.Node, key, value string) error
	GetPod(namespace, name string) (*kapi.Pod, error)
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
	glog.Infof("Setting annotations %s=%s on pod %s", key, value, pod.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		glog.Errorf("Error in setting annotation on pod %s/%s: %v", pod.Name, pod.Namespace, err)
	}
	return err
}

// SetAnnotationOnNode takes the node object and key/value string pair to set it as an annotation
func (k *Kube) SetAnnotationOnNode(node *kapi.Node, key, value string) error {
	glog.Infof("Setting annotations %s=%s on node %s", key, value, node.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Nodes().Patch(node.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		glog.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

// GetPod obtains the Pod resource from kubernetes apiserver, given the name and namespace
func (k *Kube) GetPod(namespace, name string) (*kapi.Pod, error) {
	return k.KClient.Core().Pods(namespace).Get(name, metav1.GetOptions{})
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
