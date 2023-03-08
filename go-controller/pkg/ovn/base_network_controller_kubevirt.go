package ovn

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

func (bnc *BaseNetworkController) ensureNetworkInfoForVM(origPod *corev1.Pod) error {
	if !kubevirt.PodIsLiveMigratable(origPod) {
		return nil
	}
	vmNetworkInfo, err := kubevirt.FindNetworkInfo(bnc.watchFactory, origPod)
	if err != nil {
		return err
	}
	resultErr := retry.RetryOnConflict(util.OvnConflictBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := bnc.watchFactory.GetPod(origPod.Namespace, origPod.Name)
		if err != nil {
			return err
		}

		cpod := pod.DeepCopy()
		_, ok := cpod.Labels[kubevirt.OriginalSwitchNameLabel]
		if !ok {
			cpod.Labels[kubevirt.OriginalSwitchNameLabel] = vmNetworkInfo.OriginalSwitchName
		}
		if vmNetworkInfo.Status != "" {
			cpod.Annotations[util.OvnPodAnnotationName] = vmNetworkInfo.Status
		}
		return bnc.kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update labels and annotations on pod %s/%s: %v", origPod.Namespace, origPod.Name, resultErr)
	}

	// There is nothing to check
	if vmNetworkInfo.Status == "" {
		return nil
	}
	// Wait until informers cache get updated so we don't depend on conflict
	// mechanism at next pod annotations update
	return wait.ExponentialBackoff(util.OvnConflictBackoff, func() (bool, error) {
		pod, err := bnc.watchFactory.GetPod(origPod.Namespace, origPod.Name)
		if err != nil {
			return false, err
		}
		currentNetworkInfoStatus, ok := pod.Annotations[util.OvnPodAnnotationName]
		if !ok || currentNetworkInfoStatus != vmNetworkInfo.Status {
			return false, err
		}
		originalSwitchName, ok := pod.Labels[kubevirt.OriginalSwitchNameLabel]
		if !ok || originalSwitchName != vmNetworkInfo.OriginalSwitchName {
			return false, err
		}
		return true, nil
	})
}
