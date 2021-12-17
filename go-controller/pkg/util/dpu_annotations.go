package util

import (
	"encoding/json"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
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
                "pfId": "0",
                "vfId": "3",
                "sandboxId": "35b82dbe2c39768d9874861aee38cf569766d4855b525ae02bff2bfbda73392a"
            }

Annotation: "k8s.ovn.org/dpu.connection-status"
Applied on: Pods
Used for: convey the DPU connection status for a given Pod
Example:
    annotations:
        k8s.ovn.org/dpu.connection-status: |
            {
                "status": "Ready",
                "reason": ""
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

func (scd *DPUConnectionDetails) FromPodAnnotation(podAnnot map[string]string) error {
	if annot, ok := podAnnot[DPUConnectionDetailsAnnot]; ok {
		if err := json.Unmarshal([]byte(annot), scd); err != nil {
			return fmt.Errorf("failed to unmarshal DPUConnectionDetails. %v", err)
		}
		return nil
	}
	return fmt.Errorf("failed to get DPUConnectionDetails, pod annotation \"%s\" does not exist",
		DPUConnectionDetailsAnnot)
}

func (scd *DPUConnectionDetails) SetPodAnnotation(podAnnotator kube.Annotator) error {
	data, err := json.Marshal(scd)
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	err = podAnnotator.Set(DPUConnectionDetailsAnnot, string(data))
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	return nil
}

func (scd *DPUConnectionDetails) AsAnnotation() (map[string]string, error) {
	data, err := json.Marshal(scd)
	if err != nil {
		// We should not get here
		return nil, fmt.Errorf("failed to set annotation. %v", err)
	}
	annot := make(map[string]string)
	annot[DPUConnectionDetailsAnnot] = string(data)
	return annot, nil
}

func (scs *DPUConnectionStatus) FromPodAnnotation(podAnnot map[string]string) error {
	if annot, ok := podAnnot[DPUConnetionStatusAnnot]; ok {
		if err := json.Unmarshal([]byte(annot), scs); err != nil {
			return fmt.Errorf("failed to unmarshal DPUConnectionStatus. %v", err)
		}
		return nil
	}
	return fmt.Errorf("failed to get DPUConnectionStatus pod annotation \"%s\" does not exist",
		DPUConnetionStatusAnnot)
}

func (scs *DPUConnectionStatus) SetPodAnnotation(podAnnotator kube.Annotator) error {
	data, err := json.Marshal(scs)
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	err = podAnnotator.Set(DPUConnetionStatusAnnot, string(data))
	if err != nil {
		// We should not get here
		return fmt.Errorf("failed to set annotation. %v", err)
	}
	return nil
}

func (scs *DPUConnectionStatus) AsAnnotation() (map[string]string, error) {
	data, err := json.Marshal(scs)
	if err != nil {
		// We should not get here
		return nil, fmt.Errorf("failed to set annotation. %v", err)
	}

	annot := make(map[string]string)
	annot[DPUConnetionStatusAnnot] = string(data)
	return annot, nil
}
