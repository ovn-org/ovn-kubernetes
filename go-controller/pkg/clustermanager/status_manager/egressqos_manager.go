// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	egressqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/applyconfiguration/egressqos/v1"
	egressqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressqoslisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type egressQoSManager struct {
	lister egressqoslisters.EgressQoSLister
	client egressqosclientset.Interface
}

func newEgressQoSManager(lister egressqoslisters.EgressQoSLister, client egressqosclientset.Interface) *egressQoSManager {
	return &egressQoSManager{
		lister: lister,
		client: client,
	}
}

//lint:ignore U1000 generic interfaces throw false-positives https://github.com/dominikh/go-tools/issues/1440
func (m *egressQoSManager) get(namespace, name string) (*egressqosapi.EgressQoS, error) {
	return m.lister.EgressQoSes(namespace).Get(name)
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressQoSManager) getMessages(egressQoS *egressqosapi.EgressQoS) []string {
	var messages []string
	for _, condition := range egressQoS.Status.Conditions {
		// Extract zone name from condition Type (format: "Ready-In-Zone-zoneName")
		// and format message as "zoneName: message" for consistency with message-based resources
		if strings.HasPrefix(condition.Type, readyInZonePrefix) {
			zoneName := strings.TrimPrefix(condition.Type, readyInZonePrefix)
			messages = append(messages, types.GetZoneStatus(zoneName, condition.Message))
		}
	}
	return messages
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressQoSManager) getManagedFields(egressQoS *egressqosapi.EgressQoS) []metav1.ManagedFieldsEntry {
	return egressQoS.ManagedFields
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressQoSManager) updateStatus(ctx context.Context, egressQoS *egressqosapi.EgressQoS, applyOpts *metav1.ApplyOptions,
	applyEmptyOrFailed bool) error {
	if egressQoS == nil {
		return nil
	}
	newStatus := "EgressQoS Rules applied"
	for _, condition := range egressQoS.Status.Conditions {
		if strings.Contains(condition.Message, types.EgressQoSErrorMsg) {
			newStatus = types.EgressQoSErrorMsg
			break
		}
	}
	if applyEmptyOrFailed && newStatus != types.EgressQoSErrorMsg {
		newStatus = ""
	}

	if egressQoS.Status.Status == newStatus {
		// already set to the same value
		return nil
	}

	applyStatus := egressqosapply.EgressQoSStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}

	applyObj := egressqosapply.EgressQoS(egressQoS.Name, egressQoS.Namespace).
		WithStatus(applyStatus)

	_, err := m.client.K8sV1().EgressQoSes(egressQoS.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressQoSManager) cleanupStatus(ctx context.Context, egressQoS *egressqosapi.EgressQoS, applyOpts *metav1.ApplyOptions) error {
	applyObj := egressqosapply.EgressQoS(egressQoS.Name, egressQoS.Namespace)

	_, err := m.client.K8sV1().EgressQoSes(egressQoS.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}
