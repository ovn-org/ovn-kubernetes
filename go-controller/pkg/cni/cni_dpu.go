// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// updatePodDPUConnDetailsWithRetry update the pod annotation with the given connection details for the NAD in
// the PodRequest. If the dpuConnDetails argument is nil, delete the NAD's DPU connection details annotation instead.
func (pr *PodRequest) updatePodDPUConnDetailsWithRetry(kube kube.Interface, podLister corev1listers.PodLister, pod *corev1.Pod, dpuConnDetails *util.DPUConnectionDetails) error {
	err := util.UpdatePodDPUConnDetailsWithRetry(
		pr.ctx,
		podLister,
		kube,
		pod,
		dpuConnDetails,
		pr.nadKey,
	)
	if util.IsAnnotationAlreadySetError(err) {
		return nil
	}

	return err
}

// allocateDPUConnectionDetails allocates the connection details of the request's
// VF; ovnkube-node running on the DPU plumbs the pod's representor from them.
func (pr *PodRequest) allocateDPUConnectionDetails(vfNetdevName string) (*util.DPUConnectionDetails, error) {
	if pr.CNIConf.DeviceID == "" {
		return nil, fmt.Errorf("DeviceID must be set for Pod request with DPU")
	}

	details, err := util.GetDPUOps().ResolveDeviceDetails(pr.CNIConf.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve device details for %s: %v", pr.CNIConf.DeviceID, err)
	}

	return &util.DPUConnectionDetails{
		PfId:         fmt.Sprint(details.PfId),
		VfId:         fmt.Sprint(details.FuncId),
		SandboxId:    pr.SandboxID,
		VfNetdevName: vfNetdevName,
	}, nil
}
