package kubevirt

import (
	corev1 "k8s.io/api/core/v1"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

// IsPodLiveMigratable will return true if the pod belongs
// to kubevirt and should use the live migration features
func IsPodLiveMigratable(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}
