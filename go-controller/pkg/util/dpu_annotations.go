package util

import (
	"encoding/json"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
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
func MarshalPodDPUConnDetails(annotations map[string]string, dcd *DPUConnectionDetails, nadName string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	dc, ok := podDcds[nadName]
	if dcd != nil {
		if ok && dc == *dcd {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already exists in %v",
				DPUConnectionDetailsAnnot, nadName, annotations)
		}
		podDcds[nadName] = *dcd
	} else {
		if !ok {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already removed",
				DPUConnectionDetailsAnnot, nadName)
		}
		delete(podDcds, nadName)
	}

	bytes, err := json.Marshal(podDcds)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcds, err)
	}
	annotations[DPUConnectionDetailsAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnDetails returns dpu connection details for the specified NAD
func UnmarshalPodDPUConnDetails(annotations map[string]string, nadName string) (*DPUConnectionDetails, error) {
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod %s annotation in %v",
			DPUConnectionDetailsAnnot, annotations)
	}

	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	dcd, ok := podDcds[nadName]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for network %s: %q",
			DPUConnectionDetailsAnnot, nadName, ovnAnnotation)
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

// MarshalPodDPUConnStatus adds the pod's connection status of the specified NAD to the corresponding pod annotation.
// if scs is nil, delete the pod's connection status of the specified NAD
func MarshalPodDPUConnStatus(annotations map[string]string, scs *DPUConnectionStatus, nadName string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podScss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	sc, ok := podScss[nadName]
	if scs != nil {
		if ok && sc == *scs {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already exists in %v",
				DPUConnectionStatusAnnot, nadName, annotations)
		}
		podScss[nadName] = *scs
	} else {
		if !ok {
			return nil, newAnnotationAlreadySetError("OVN pod %s annotation for NAD %s already removed",
				DPUConnectionStatusAnnot, nadName)
		}
		delete(podScss, nadName)
	}
	bytes, err := json.Marshal(podScss)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podScss, err)
	}
	annotations[DPUConnectionStatusAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnStatus returns DPU connection status for the specified NAD
func UnmarshalPodDPUConnStatus(annotations map[string]string, nadName string) (*DPUConnectionStatus, error) {
	ovnAnnotation, ok := annotations[DPUConnectionStatusAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podScss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	scs, ok := podScss[nadName]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for network %s: %q",
			DPUConnectionStatusAnnot, nadName, ovnAnnotation)
	}
	return &scs, nil
}

// UpdatePodDPUConnStatusWithRetry updates the DPU connection status annotation
// on the pod retrying on conflict
func UpdatePodDPUConnStatusWithRetry(podLister listers.PodLister, kube kube.Interface, pod *v1.Pod, dpuConnStatus *DPUConnectionStatus, nadName string) error {
	updatePodAnnotationNoRollback := func(pod *v1.Pod) (*v1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodDPUConnStatus(pod.Annotations, dpuConnStatus, nadName)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
		podLister,
		kube,
		pod,
		updatePodAnnotationNoRollback,
	)
}

// UpdatePodDPUConnDetailsWithRetry updates the DPU connection details
// annotation on the pod retrying on conflict
func UpdatePodDPUConnDetailsWithRetry(podLister listers.PodLister, kube kube.Interface, pod *v1.Pod, dpuConnDetails *DPUConnectionDetails, nadName string) error {
	updatePodAnnotationNoRollback := func(pod *v1.Pod) (*v1.Pod, func(), error) {
		var err error
		pod.Annotations, err = MarshalPodDPUConnDetails(pod.Annotations, dpuConnDetails, nadName)
		if err != nil {
			return nil, nil, err
		}
		return pod, nil, nil
	}

	return UpdatePodWithRetryOrRollback(
		podLister,
		kube,
		pod,
		updatePodAnnotationNoRollback,
	)
}
