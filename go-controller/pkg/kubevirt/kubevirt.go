package kubevirt

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func OwnsPod(ctx context.Context, kclient kubernetes.Interface, namespace, name string) (bool, error) {
	pod, err := kclient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	_, ok := pod.Labels["kubevirt.io/vm"]
	return ok, nil
}
