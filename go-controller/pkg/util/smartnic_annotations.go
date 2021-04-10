package util

import (
	"encoding/json"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
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

func (scd *SmartNICConnectionDetails) FromPodAnnotation(podAnnot map[string]string) error {
	if annot, ok := podAnnot[SmartNicConnectionDetailsAnnot]; ok {
		if err := json.Unmarshal([]byte(annot), scd); err != nil {
			return fmt.Errorf("failed to unmarshal SmartNICConnectionDetails. %v", err)
		}
		return nil
	}
	return fmt.Errorf("failed to get SmartNICConnectionDetails, pod annotation \"%s\" does not exist",
		SmartNicConnectionDetailsAnnot)
}

func (scd *SmartNICConnectionDetails) SetPodAnnotation(podAnnotator kube.Annotator) error {
	data, err := json.Marshal(scd)
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	err = podAnnotator.Set(SmartNicConnectionDetailsAnnot, string(data))
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	return nil
}

func (scd *SmartNICConnectionDetails) AsAnnotation() (map[string]string, error) {
	data, err := json.Marshal(scd)
	if err != nil {
		// We should not get here
		return nil, fmt.Errorf("failed to set annotation. %v", err)
	}
	annot := make(map[string]string)
	annot[SmartNicConnectionDetailsAnnot] = string(data)
	return annot, nil
}

func (scs *SmartNICConnectionStatus) FromPodAnnotation(podAnnot map[string]string) error {
	if annot, ok := podAnnot[SmartNicConnetionStatusAnnot]; ok {
		if err := json.Unmarshal([]byte(annot), scs); err != nil {
			return fmt.Errorf("failed to unmarshal SmartNICConnectionStatus. %v", err)
		}
		return nil
	}
	return fmt.Errorf("failed to get SmartNICConnectionStatus pod annotation \"%s\" does not exist",
		SmartNicConnetionStatusAnnot)
}

func (scs *SmartNICConnectionStatus) SetPodAnnotation(podAnnotator kube.Annotator) error {
	data, err := json.Marshal(scs)
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	err = podAnnotator.Set(SmartNicConnetionStatusAnnot, string(data))
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	return nil
}

func (scs *SmartNICConnectionStatus) AsAnnotation() (map[string]string, error) {
	data, err := json.Marshal(scs)
	if err != nil {
		// We should not get here
		return nil, fmt.Errorf("failed to set annotation. %v", err)
	}

	annot := make(map[string]string)
	annot[SmartNicConnetionStatusAnnot] = string(data)
	return annot, nil
}
