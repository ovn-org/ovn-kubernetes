package util

import (
	"encoding/json"
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
	DPUConnetionStatusAnnot   = "k8s.ovn.org/dpu.connection-status"

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

// MarshalPodDPUConnDetails adds the pod's connection details of the specified network to the corresponding pod annotation.
func MarshalPodDPUConnDetails(annotations map[string]string, dcd *DPUConnectionDetails, annoNadKeyName string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	podDcds[annoNadKeyName] = *dcd

	bytes, err := json.Marshal(podDcds)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcds, err)
	}
	annotations[DPUConnectionDetailsAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnDetails returns dpu connection details for the specified network
func UnmarshalPodDPUConnDetails(annotations map[string]string, annoNadKeyName string) (*DPUConnectionDetails, error) {
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod %s annotation in %v",
			DPUConnectionDetailsAnnot, annotations)
	}

	podDcds, err := UnmarshalPodDPUConnDetailsAllNetworks(annotations)
	if err != nil {
		return nil, err
	}

	dcd, ok := podDcds[annoNadKeyName]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for network %s: %q",
			DPUConnectionDetailsAnnot, annoNadKeyName, ovnAnnotation)
	}
	return &dcd, nil
}

// UnmarshalPodDPUConnStatusAllNetworks returns the DPUConnectionStatus map of all networks from the given Pod annotation
func UnmarshalPodDPUConnStatusAllNetworks(annotations map[string]string) (map[string]DPUConnectionStatus, error) {
	podDcss := make(map[string]DPUConnectionStatus)
	ovnAnnotation, ok := annotations[DPUConnetionStatusAnnot]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podDcss); err != nil {
			// DPU connection status annotation could be in the legacy format
			var legacyScs DPUConnectionStatus
			if err := json.Unmarshal([]byte(ovnAnnotation), &legacyScs); err == nil {
				podDcss[types.DefaultNetworkName] = legacyScs
			} else {
				return nil, fmt.Errorf("failed to unmarshal OVN pod %s annotation %q: %v",
					DPUConnetionStatusAnnot, annotations, err)
			}
		}
	}
	return podDcss, nil
}

// MarshalPodDPUConnStatus adds the pod's connection status of the specified network to the corresponding pod annotation.
func MarshalPodDPUConnStatus(annotations map[string]string, dcs *DPUConnectionStatus, annoNadKeyName string) (map[string]string, error) {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	podDcss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	podDcss[annoNadKeyName] = *dcs
	bytes, err := json.Marshal(podDcss)
	if err != nil {
		return nil, fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcss, err)
	}
	annotations[DPUConnetionStatusAnnot] = string(bytes)
	return annotations, nil
}

// UnmarshalPodDPUConnStatus returns DPU connection status for the specified network
func UnmarshalPodDPUConnStatus(annotations map[string]string, netName string) (*DPUConnectionStatus, error) {
	ovnAnnotation, ok := annotations[DPUConnetionStatusAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podDcss, err := UnmarshalPodDPUConnStatusAllNetworks(annotations)
	if err != nil {
		return nil, err
	}
	dcs, ok := podDcss[netName]
	if !ok {
		return nil, newAnnotationNotSetError("no OVN %s annotation for network %s: %q",
			DPUConnetionStatusAnnot, netName, ovnAnnotation)
	}
	return &dcs, nil
}
