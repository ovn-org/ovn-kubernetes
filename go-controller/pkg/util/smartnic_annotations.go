package util

import (
	"encoding/json"
	"fmt"
)

/*
This Handles Smart-NIC related annotations in ovn-kubernetes.

The following annotations are handled:

Annotation: "k8s.ovn.org/smartnic.connection-details"
Applied on: Pods
Used for: convey the required information to setup network plubming on Smart-NIC for a given Pod
Example:
    annotations:
        k8s.ovn.org/smartnic.connection-details: |
            {
                "pfId": “0”,
                “vfId”: "3",
                "sandboxId": "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a"
            }

Annotation: "k8s.ovn.org/smartnic.connection-status"
Applied on: Pods
Used for: convey the smart-NIC connection status for a given Pod
Example:
    annotations:
        k8s.ovn.org/smartnic.connection-status: |
            {
                "status": “Ready”,
                "reason": ""
            }
*/

const (
	SmartNicConnectionDetailsAnnot = "k8s.ovn.org/smartnic.connection-details"
	SmartNicConnetionStatusAnnot   = "k8s.ovn.org/smartnic.connection-status"

	SmartNicConnectionStatusReady = "Ready"
	SmartNicConnectionStatusError = "Error"
)

type SmartNICConnectionDetails struct {
	PfId      string `json:"pfId"`
	VfId      string `json:"vfId"`
	SandboxId string `json:"sandboxId"`
}

type SmartNICConnectionStatus struct {
	Status string `json:"Status"`
	Reason string `json:"Reason,omitempty"`
}

// MarshalPodSmartNicConnDetails returns a JSON-formatted annotation describing the pod's smart-nic connection details
func MarshalPodSmartNicConnDetails(pannotations *map[string]string, scd *SmartNICConnectionDetails, netName string) error {
	annotations := *pannotations
	if annotations == nil {
		annotations = make(map[string]string)
		*pannotations = annotations
	}
	podScds := make(map[string]SmartNICConnectionDetails)
	ovnAnnotation, ok := annotations[SmartNicConnectionDetailsAnnot]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podScds); err != nil {
			return fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
				ovnAnnotation, err)
		}
	}
	podScds[netName] = *scd
	bytes, err := json.Marshal(podScds)
	if err != nil {
		return fmt.Errorf("failed marshaling pod annotation map %v: %v", podScds, err)
	}
	annotations[SmartNicConnectionDetailsAnnot] = string(bytes)
	return nil
}

// UnmarshalPodSmartNicConnDetails returns smart-nic connection details for the specified network
func UnmarshalPodSmartNicConnDetails(annotations map[string]string, netName string) (*SmartNICConnectionDetails, error) {
	ovnAnnotation, ok := annotations[SmartNicConnectionDetailsAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podScds := make(map[string]SmartNICConnectionDetails)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podScds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	scd, ok := podScds[netName]
	if !ok {
		return nil, fmt.Errorf("no smart-nic connection details annotation for network %s: %q",
			netName, ovnAnnotation)
	}
	return &scd, nil
}

// MarshalPodSmartNicConnStatus returns a JSON-formatted annotation describing the pod's smart-nic connection status
func MarshalPodSmartNicConnStatus(pannotations *map[string]string, scs *SmartNICConnectionStatus, netName string) error {
	annotations := *pannotations
	if annotations == nil {
		annotations = make(map[string]string)
		*pannotations = annotations
	}
	podScds := make(map[string]SmartNICConnectionStatus)
	ovnAnnotation, ok := annotations[SmartNicConnetionStatusAnnot]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podScds); err != nil {
			return fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
				ovnAnnotation, err)
		}
	}
	podScds[netName] = *scs
	bytes, err := json.Marshal(podScds)
	if err != nil {
		return fmt.Errorf("failed marshaling pod annotation map %v: %v", podScds, err)
	}
	annotations[SmartNicConnetionStatusAnnot] = string(bytes)
	return nil
}

// UnmarshalPodSmartNicConnStatus returns smart-nic connection status for the specified network
func UnmarshalPodSmartNicConnStatus(annotations map[string]string, netName string) (*SmartNICConnectionStatus, error) {
	ovnAnnotation, ok := annotations[SmartNicConnetionStatusAnnot]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podScss := make(map[string]SmartNICConnectionStatus)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podScss); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	scs, ok := podScss[netName]
	if !ok {
		return nil, fmt.Errorf("no smart-nic connection status annotation for network %s: %q",
			netName, ovnAnnotation)
	}
	return &scs, nil
}
