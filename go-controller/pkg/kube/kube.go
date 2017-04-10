package kube

import (
	"fmt"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kapi "k8s.io/client-go/pkg/api/v1"
)

type KubeInterface interface {
	SetAnnotationOnPod(pod *kapi.Pod, key, value string) error
	SetAnnotationOnNode(node *kapi.Node, key, value string) error
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetNodes() (*kapi.NodeList, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
}

type Kube struct {
	KClient kubernetes.Interface
}

func (k *Kube) SetAnnotationOnPod(pod *kapi.Pod, key, value string) error {
	glog.Infof("Setting annotations %s=%s on pod %s", key, value, pod.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Pods(pod.Namespace).Patch(pod.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		glog.Errorf("Error in setting annotation on pod %s/%s: %v", pod.Name, pod.Namespace, err)
	}
	return err
}

func (k *Kube) SetAnnotationOnNode(node *kapi.Node, key, value string) error {
	glog.Infof("Setting annotations %s=%s on node %s", key, value, node.Name)
	patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, key, value)
	_, err := k.KClient.Core().Nodes().Patch(node.Name, types.MergePatchType, []byte(patchData))
	if err != nil {
		glog.Errorf("Error in setting annotation on node %s: %v", node.Name, err)
	}
	return err
}

func (k *Kube) GetPod(namespace, name string) (*kapi.Pod, error) {
	return k.KClient.Core().Pods(namespace).Get(name, metav1.GetOptions{})
}

func (k *Kube) GetNodes() (*kapi.NodeList, error) {
	return k.KClient.Core().Nodes().List(metav1.ListOptions{})
}

func (k *Kube) GetNode(name string) (*kapi.Node, error) {
	return k.KClient.Core().Nodes().Get(name, metav1.GetOptions{})
}

func (k *Kube) GetService(namespace, name string) (*kapi.Service, error) {
	return k.KClient.Core().Services(namespace).Get(name, metav1.GetOptions{})
}
