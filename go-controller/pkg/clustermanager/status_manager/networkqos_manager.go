package status_manager

import (
	"context"
	"strings"

	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	networkqosapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/applyconfiguration/networkqos/v1"
	networkqosclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/clientset/versioned"
	networkqoslisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/listers/networkqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		messages = append(messages, condition.Message)
	}
	return messages
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *networkQoSManager) updateStatus(networkQoS *networkqosapi.NetworkQoS, applyOpts *metav1.ApplyOptions,
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

	_, err := m.client.K8sV1().NetworkQoSes(networkQoS.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *networkQoSManager) cleanupStatus(networkQoS *networkqosapi.NetworkQoS, applyOpts *metav1.ApplyOptions) error {
	applyObj := networkqosapply.NetworkQoS(networkQoS.Name, networkQoS.Namespace).
		WithStatus(networkqosapply.Status())

	_, err := m.client.K8sV1().NetworkQoSes(networkQoS.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}
