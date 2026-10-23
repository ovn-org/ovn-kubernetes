// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

/*
This Handles DPU related annotations in ovn-kubernetes.

The following annotations are handled:

Annotation: "k8s.ovn.org/dpu.connection-details"
Applied on: Pods
Used for: convey the required information to setup network plubming on DPU for a given Pod
Example:
    annotations:
        k8s.ovn.org/dpu.connection-details: |
            {"default":
				{
                	"pfId": “0”,
                	“vfId”: "3",
                	"sandboxId": "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a"
				}
            }

Annotation: "k8s.ovn.org/dpu.connection-status"
Applied on: Pods
Used for: convey the DPU connection status for a given Pod
Example:
    annotations:
        k8s.ovn.org/dpu.connection-status: |
            {"default":
				{
					"status": “Ready”,
					"reason": ""
				}
			}
*/

const (
	DPUConnectionDetailsAnnot = "k8s.ovn.org/dpu.connection-details"
	DPUConnectionStatusAnnot  = "k8s.ovn.org/dpu.connection-status"

	DPUConnectionStatusReady = "Ready"
	DPUConnectionStatusError = "Error"
)

type DPUConnectionDetails struct {
	PfId         string `json:"pfId"`
	VfId         string `json:"vfId"`
	SandboxId    string `json:"sandboxId"`
	VfNetdevName string `json:"vfNetdevName,omitempty"`
}

type DPUConnectionStatus struct {
	Status string `json:"Status"`
	Reason string `json:"Reason,omitempty"`
}

// UnmarshalPodDPUConnDetailsAllNetworks returns the DPUConnectionDetails map of all networks from the given Pod annotation
func UnmarshalPodDPUConnDetailsAllNetworks(annotations map[string]string) (map[string]DPUConnectionDetails, error) {
	podDcds := make(map[string]DPUConnectionDetails)
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podDcds); err != nil {
			// DPU connection details annotation could be in the legacy format
			var legacyScd DPUConnectionDetails
			if err := json.Unmarshal([]byte(ovnAnnotation), &legacyScd); err == nil {
				podDcds[types.DefaultNetworkName] = legacyScd
			} else {
				return nil, fmt.Errorf("failed to unmarshal OVN pod %s annotation %q: %v",
					DPUConnectionDetailsAnnot, annotations, err)
			}
		}
	}
	return podDcds, nil
}

// MarshalPodDPUConnDetails adds the pod's connection details of the specified NAD to the corresponding pod annotation;
// if dcd is nil, delete the pod's connection details of the specified NAD
func MarshalPodDPUConnDetails(annotations map[string]string, dcd *DPUConnectionDetails, nadKey string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	dc, ok := podDcds[nadKey]
	if dcd != nil {
		if ok && dc == *dcd {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already exists in %v",
				DPUConnectionDetailsAnnot, nadKey, annotations)
		}
		podDcds[nadKey] = *dcd
	} else {
		if !ok {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already removed",
				DPUConnectionDetailsAnnot, nadKey)
		}
		delete(podDcds, nadKey)
	}

	bytes, err := json.Marshal(podDcds)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcds, err)
	}
	annotations[DPUConnectionDetailsAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnDetails returns dpu connection details for the specified NAD
func UnmarshalPodDPUConnDetails(annotations map[string]string, nadKey string) (*DPUConnectionDetails, error) {
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod %s annotation in %v",
			DPUConnectionDetailsAnnot, annotations)
	}

	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	dcd, ok := podDcds[nadKey]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for NAD %s: %q",
			DPUConnectionDetailsAnnot, nadKey, ovnAnnotation)
	}
	return &dcd, nil
}

// UnmarshalPodDPUConnStatusAllNetworks returns the DPUConnectionStatus map of all networks from the given Pod annotation
func UnmarshalPodDPUConnStatusAllNetworks(annotations map[string]string) (map[string]DPUConnectionStatus, error) {
	podDcss := make(map[string]DPUConnectionStatus)
	ovnAnnotation, ok := annotations[DPUConnectionStatusAnnot]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podDcss); err != nil {
			// DPU connection status annotation could be in the legacy format
			var legacyScs DPUConnectionStatus
			if err := json.Unmarshal([]byte(ovnAnnotation), &legacyScs); err == nil {
				podDcss[types.DefaultNetworkName] = legacyScs
			} else {
				return nil, fmt.Errorf("failed to unmarshal OVN pod %s annotation %q: %v",
					DPUConnectionStatusAnnot, annotations, err)
			}
		}
	}
	return podDcss, nil
}

// MarshalPodDPUConnStatus merges the given connection statuses into the pod's
// DPU connection status annotation. Only the keys in statusMap are touched, and
// a nil value deletes that NAD's entry.
func MarshalPodDPUConnStatus(annotations map[string]string, statusMap map[string]*DPUConnectionStatus) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podScss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	changed := false
	for nadKey, scs := range statusMap {
		if scs != nil {
			sc, ok := podScss[nadKey]
			if ok && sc == *scs {
				continue
			}
			podScss[nadKey] = *scs
			changed = true
		} else {
			if _, ok := podScss[nadKey]; !ok {
				continue
			}
			delete(podScss, nadKey)
			changed = true
		}
	}

	if !changed {
		return nil, newAnnotationAlreadySetError("OVN pod %s annotation already up to date",
			DPUConnectionStatusAnnot)
	}

	bytes, err := json.Marshal(podScss)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podScss, err)
	}
	annotations[DPUConnectionStatusAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnStatus returns DPU connection status for the specified NAD
func UnmarshalPodDPUConnStatus(annotations map[string]string, nadKey string) (*DPUConnectionStatus, error) {
	ovnAnnotation, ok := annotations[DPUConnectionStatusAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podScss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	scs, ok := podScss[nadKey]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for NAD %s: %q",
			DPUConnectionStatusAnnot, nadKey, ovnAnnotation)
	}
	return &scs, nil
}

// UpdatePodDPUConnStatusWithRetry updates the DPU connection status annotation
// on the pod, retrying on conflict. Only the keys in statusMap are touched, and
// a nil value deletes that NAD's entry.
func UpdatePodDPUConnStatusWithRetry(ctx context.Context, podLister listers.PodLister, kube kube.Interface, pod *corev1.Pod, statusMap map[string]*DPUConnectionStatus) error {
	updatePodAnnotationNoRollback := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodDPUConnStatus(pod.Annotations, statusMap)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
		ctx,
		podLister,
		kube,
		pod,
		updatePodAnnotationNoRollback,
	)
}

// UpdatePodDPUConnDetailsWithRetry updates the DPU connection details
// annotation on the pod retrying on conflict
func UpdatePodDPUConnDetailsWithRetry(ctx context.Context, podLister listers.PodLister, kube kube.Interface, pod *corev1.Pod, dpuConnDetails *DPUConnectionDetails, nadKey string) error {
	updatePodAnnotationNoRollback := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodDPUConnDetails(pod.Annotations, dpuConnDetails, nadKey)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
		ctx,
		podLister,
		kube,
		pod,
		updatePodAnnotationNoRollback,
	)
}
