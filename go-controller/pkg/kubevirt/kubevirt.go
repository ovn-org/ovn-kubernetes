package kubevirt

import (
	"context"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	clientset "k8s.io/client-go/kubernetes"
)

const (
	VMLabel                                      = "kubevirt.io/vm"
	NodeNameLabel                                = "kubevirt.io/nodeName"
	IPPoolNameAnnotation                         = "kubevirt.io/ipPoolName"
	AllowPodBridgeNetworkLiveMigrationAnnotation = "kubevirt.io/allow-pod-bridge-network-live-migration"
)

type IPConfig struct {
	PoolName   string
	Annotation string
}

func AllowPodBridgeNetworkLiveMigration(pod *kapi.Pod) bool {
	_, ok := pod.Annotations[AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}

// PodIsLiveMigrationLeftOver return true if there is a another virt-launcher
// pod for this VM but with newer creation time then this pod is a leftover
func PodIsLiveMigrationLeftOver(client clientset.Interface, pod *kapi.Pod) (bool, error) {
	vmPods, err := FindPodsByVMLabel(client, pod)
	if err != nil {
		return false, err
	}

	for _, vmPod := range vmPods {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

// PodOnLiveMigration return true if there are multiple pods referening
// the VM from the pod arg, meaning that the vm is on live migration
func PodOnLiveMigration(client clientset.Interface, pod *kapi.Pod) (bool, error) {
	vmPods, err := FindPodsByVMLabel(client, pod)
	if err != nil {
		return false, err
	}
	return len(vmPods) > 1, nil
}

func FindPodsByVMLabel(client clientset.Interface, pod *kapi.Pod) ([]kapi.Pod, error) {
	vmName, ok := pod.Labels[VMLabel]
	if !ok {
		return []kapi.Pod{}, nil
	}
	matchVMLabel := labels.Set{VMLabel: vmName}
	vmPods, err := client.CoreV1().Pods(pod.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: matchVMLabel.String()})
	if err != nil {
		return []kapi.Pod{}, err
	}
	return vmPods.Items, nil
}

func FindIPConfigByVMLabel(client clientset.Interface, pod *kapi.Pod) (IPConfig, error) {
	vmPods, err := FindPodsByVMLabel(client, pod)
	if err != nil {
		return IPConfig{}, err
	}
	ipConfig := IPConfig{
		PoolName: pod.Spec.NodeName,
	}
	for _, vmPod := range vmPods {
		ipPoolName, ok := vmPod.Annotations[IPPoolNameAnnotation]
		if ok {
			ipConfig.PoolName = ipPoolName
		}
		ipAnnotation, ok := vmPod.Annotations[util.OvnPodAnnotationName]
		if ok {
			ipConfig.Annotation = ipAnnotation
		}
	}
	return ipConfig, nil
}
