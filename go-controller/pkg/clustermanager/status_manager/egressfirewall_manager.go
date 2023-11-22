package status_manager

import (
	"context"
	"strings"

	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressfirewalllisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type egressFirewallManager struct {
	lister egressfirewalllisters.EgressFirewallLister
	client egressfirewallclientset.Interface
}

func newEgressFirewallManager(lister egressfirewalllisters.EgressFirewallLister, client egressfirewallclientset.Interface) *egressFirewallManager {
	return &egressFirewallManager{
		lister: lister,
		client: client,
	}
}

func (m *egressFirewallManager) get(namespace, name string) (*egressfirewallapi.EgressFirewall, error) {
	return m.lister.EgressFirewalls(namespace).Get(name)
}

func (m *egressFirewallManager) getMessages(egressFirewall *egressfirewallapi.EgressFirewall) []string {
	return egressFirewall.Status.Messages
}

func (m *egressFirewallManager) updateStatus(egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions,
	applyEmptyOrFailed bool) error {
	if egressFirewall == nil {
		return nil
	}
	newStatus := "EgressFirewall Rules applied"
	for _, message := range egressFirewall.Status.Messages {
		if strings.Contains(message, types.EgressFirewallErrorMsg) {
			newStatus = types.EgressFirewallErrorMsg
			break
		}
	}
	if applyEmptyOrFailed && newStatus != types.EgressFirewallErrorMsg {
		newStatus = ""
	}

	if egressFirewall.Status.Status == newStatus {
		// already set to the same value
		return nil
	}

	applyStatus := egressfirewallapply.EgressFirewallStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}

	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(applyStatus)

	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}

func (m *egressFirewallManager) cleanupStatus(egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions) error {
	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(egressfirewallapply.EgressFirewallStatus())

	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}
