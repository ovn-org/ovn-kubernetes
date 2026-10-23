// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	networkqoslisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type networkQoSManager struct {
	lister networkqoslisters.NetworkQoSLister
	client networkqosclientset.Interface
}

func newNetworkQoSManager(lister networkqoslisters.NetworkQoSLister, client networkqosclientset.Interface) *networkQoSManager {
	return &networkQoSManager{
		lister: lister,
		client: client,
	}
}

//lint:ignore U1000 generic interfaces throw false-positives https://github.com/dominikh/go-tools/issues/1440
func (m *networkQoSManager) get(namespace, name string) (*networkqosapi.NetworkQoS, error) {
	return m.lister.NetworkQoSes(namespace).Get(name)
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *networkQoSManager) getMessages(networkQoS *networkqosapi.NetworkQoS) []string {
	var messages []string
	for _, condition := range networkQoS.Status.Conditions {
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
func (m *networkQoSManager) getManagedFields(networkQoS *networkqosapi.NetworkQoS) []metav1.ManagedFieldsEntry {
	return networkQoS.ManagedFields
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *networkQoSManager) updateStatus(ctx context.Context, networkQoS *networkqosapi.NetworkQoS, applyOpts *metav1.ApplyOptions,
	applyEmptyOrFailed bool) error {
	if networkQoS == nil {
		return nil
	}
	newStatus := "NetworkQoS Destinations applied"
	for _, condition := range networkQoS.Status.Conditions {
		if strings.Contains(condition.Message, types.NetworkQoSErrorMsg) {
			newStatus = types.NetworkQoSErrorMsg
			break
		}
	}
	if applyEmptyOrFailed && newStatus != types.NetworkQoSErrorMsg {
		newStatus = ""
	}

	if networkQoS.Status.Status == newStatus {
		// already set to the same value
		return nil
	}

	applyStatus := networkqosapply.Status()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}

	applyObj := networkqosapply.NetworkQoS(networkQoS.Name, networkQoS.Namespace).
		WithStatus(applyStatus)

	_, err := m.client.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *networkQoSManager) cleanupStatus(ctx context.Context, networkQoS *networkqosapi.NetworkQoS, applyOpts *metav1.ApplyOptions) error {
	applyObj := networkqosapply.NetworkQoS(networkQoS.Name, networkQoS.Namespace)

	_, err := m.client.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}
