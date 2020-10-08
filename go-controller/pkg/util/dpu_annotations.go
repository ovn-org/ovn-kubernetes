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
            {
                "default": {
                    "pfId":         “0”,
                    “vfId”:         "3",
		    "vfNetdevName": "eth2",
                    "sandboxId":    "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a"
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

// MarshalPodDPUConnDetails returns a JSON-formatted annotation describing the pod's DPU connection details
func MarshalPodDPUConnDetails(pannotations *map[string]string, dcd *DPUConnectionDetails, nadName string) error {
	annotations := *pannotations
	if annotations == nil {
		annotations = make(map[string]string)
		*pannotations = annotations
	}
	podDcds := make(map[string]DPUConnectionDetails)
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if ok {
		// legacy scd annoation are not of different network
		if err := json.Unmarshal([]byte(ovnAnnotation), &podDcds); err != nil {
			var legacyDcd DPUConnectionDetails
			if err := json.Unmarshal([]byte(ovnAnnotation), &legacyDcd); err == nil {
				podDcds = map[string]DPUConnectionDetails{}
				podDcds[types.DefaultNetworkName] = legacyDcd
			} else {
				return fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
					ovnAnnotation, err)
			}
		}
	}
	podDcds[nadName] = *dcd
	bytes, err := json.Marshal(podDcds)
	if err != nil {
		return fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcds, err)
	}
	annotations[DPUConnectionDetailsAnnot] = string(bytes)
	return nil
}

// UnmarshalPodDPUConnDetails returns dpu connection details for the specified network
func UnmarshalPodDPUConnDetails(annotations map[string]string, netName string) (*DPUConnectionDetails, error) {
	ovnAnnotation, ok := annotations[DPUConnectionDetailsAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podDcds := make(map[string]DPUConnectionDetails)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podDcds); err != nil {
		// legacy
		if netName == types.DefaultNetworkName {
			var dcd DPUConnectionDetails
			if err := json.Unmarshal([]byte(ovnAnnotation), &dcd); err == nil {
				return &dcd, nil
			}
		}
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	dcd, ok := podDcds[netName]
	if !ok {
		return nil, fmt.Errorf("no dpu connection details annotation for network %s: %q",
			netName, ovnAnnotation)
	}
	return &dcd, nil
}

// MarshalPodDPUConnStatus returns a JSON-formatted annotation describing the pod's DPU connection status
func MarshalPodDPUConnStatus(pannotations *map[string]string, dcs *DPUConnectionStatus, nadName string) error {
	annotations := *pannotations
	if annotations == nil {
		annotations = make(map[string]string)
		*pannotations = annotations
	}
	podDcds := make(map[string]DPUConnectionStatus)
	ovnAnnotation, ok := annotations[DPUConnetionStatusAnnot]
	if ok {
		// legacy dcd annoation are not of different network
		if err := json.Unmarshal([]byte(ovnAnnotation), &podDcds); err != nil {
			var legacyDcs DPUConnectionStatus
			if err := json.Unmarshal([]byte(ovnAnnotation), &legacyDcs); err == nil {
				podDcds = map[string]DPUConnectionStatus{}
				podDcds[types.DefaultNetworkName] = legacyDcs
			} else {
				return fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
					ovnAnnotation, err)
			}
		}
	}
	podDcds[nadName] = *dcs
	bytes, err := json.Marshal(podDcds)
	if err != nil {
		return fmt.Errorf("failed marshaling pod annotation map %v: %v", podDcds, err)
	}
	annotations[DPUConnetionStatusAnnot] = string(bytes)
	return nil
}

// UnmarshalPodDPUConnStatus returns DPU connection status for the specified network
func UnmarshalPodDPUConnStatus(annotations map[string]string, nadName string) (*DPUConnectionStatus, error) {
	ovnAnnotation, ok := annotations[DPUConnetionStatusAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podDcss := make(map[string]DPUConnectionStatus)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podDcss); err != nil {
		// legacy
		if nadName == types.DefaultNetworkName {
			var dcs DPUConnectionStatus
			if err := json.Unmarshal([]byte(ovnAnnotation), &dcs); err == nil {
				return &dcs, nil
			}
		}
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	dcs, ok := podDcss[nadName]
	if !ok {
		return nil, fmt.Errorf("no dpu connection status annotation for network %s: %q",
			nadName, ovnAnnotation)
	}
	return &dcs, nil
}
