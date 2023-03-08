package kubevirt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kvv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// PodIsLiveMigratable will return true if the pod belongs
// to kubevirt and should use the live migration features
func PodIsLiveMigratable(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[kvv1.AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}

// FindVMRelatedPods will return pods belong to the same vm annotated at pod
func FindVMRelatedPods(client *factory.WatchFactory, pod *corev1.Pod) ([]*corev1.Pod, error) {
	vmName, ok := pod.Labels[kvv1.VirtualMachineNameLabel]
	if !ok {
		return []*corev1.Pod{}, nil
	}
	vmPods, err := client.GetPodsBySelector(pod.Namespace, metav1.LabelSelector{MatchLabels: map[string]string{kvv1.VirtualMachineNameLabel: vmName}})
	if err != nil {
		return []*corev1.Pod{}, err
	}
	return vmPods, nil
}

// FindNetworkInfo will return the original switch name and the OVN pod
// annotation from any other pod annotated with the same VM as pod
func FindNetworkInfo(client *factory.WatchFactory, pod *corev1.Pod) (NetworkInfo, error) {
	vmPods, err := FindVMRelatedPods(client, pod)
	if err != nil {
		return NetworkInfo{}, fmt.Errorf("failed finding related pods for pod %s/%s when looking for network info: %v", pod.Namespace, pod.Name, err)
	}
	networkInfo := NetworkInfo{
		OriginalSwitchName: pod.Spec.NodeName,
	}
	for _, vmPod := range vmPods {
		if vmPod.Name == pod.Name {
			continue
		}
		originalSwitchName, ok := vmPod.Labels[OriginalSwitchNameLabel]
		if ok {
			networkInfo.OriginalSwitchName = originalSwitchName
		}
		status, ok := vmPod.Annotations[util.OvnPodAnnotationName]
		if ok {
			networkInfo.Status = status
		}
	}
	return networkInfo, nil
}

// PodIsLiveMigrationLeftOver return true if there are other pods related to
// to it and any of them has newer creation timestamp.
func PodIsLiveMigrationLeftOver(client *factory.WatchFactory, pod *corev1.Pod) (bool, error) {
	vmPods, err := FindVMRelatedPods(client, pod)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking live migration left overs: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

func PodMatchesExternalIDs(pod *corev1.Pod, externalIDs map[string]string) bool {
	return len(externalIDs) > 1 && externalIDs[NamespaceExternalIDKey] == pod.Namespace && externalIDs[kvv1.VirtualMachineNameLabel] == pod.Labels[kvv1.VirtualMachineNameLabel]
}
