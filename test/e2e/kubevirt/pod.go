package kubevirt

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

func GenerateFakeVirtLauncherPod(namespace, vmName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-launcher-" + vmName,
			Namespace: namespace,
			Labels: map[string]string{
				kubevirtv1.VirtualMachineNameLabel: vmName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "compute",
				Image: "quay.io/nmstate/c10s-nmstate-dev:latest",
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"NET_ADMIN"},
					},
				},
			}},
		},
	}
}
