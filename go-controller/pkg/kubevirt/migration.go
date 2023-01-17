package kubevirt

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
)

const (
	VMLabelKey = "kubevirt.io/vm"
)

func PodIsLiveMigrationLeftOver(client clientset.Interface, pod *corev1.Pod) (bool, error) {
	vmName, ok := OwnsPodAsVirtualMachine(pod)
	if !ok {
		return false, nil
	}
	matchVMLabel := labels.Set{VMLabelKey: vmName}
	vmPods, err := client.CoreV1().Pods(pod.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: matchVMLabel.String()})
	if err != nil {
		return false, err
	}

	// Is there is a another virt-launcher pod for this VM but with newer
	// creation time then this pod is a leftover
	for _, vmPod := range vmPods.Items {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

func OwnsPodAsVirtualMachine(pod *corev1.Pod) (string, bool) {
	vmName, ok := pod.Labels[VMLabelKey]
	return vmName, ok
}
