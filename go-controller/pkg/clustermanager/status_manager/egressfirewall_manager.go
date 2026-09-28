// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"

	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressfirewalllisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type egressFirewallManager struct {
	lister         egressfirewalllisters.EgressFirewallLister
	nodeLister     corelisters.NodeLister
	client         egressfirewallclientset.Interface
	networkManager networkmanager.Interface
}

// getRelevantZones is consumed through a generic interface assertion in the shared status manager.
var _ relevantZoneProvider[egressfirewallapi.EgressFirewall] = (*egressFirewallManager)(nil)

func newEgressFirewallManager(lister egressfirewalllisters.EgressFirewallLister, nodeLister corelisters.NodeLister, client egressfirewallclientset.Interface, networkManager networkmanager.Interface) *egressFirewallManager {
	return &egressFirewallManager{
		lister:         lister,
		nodeLister:     nodeLister,
		client:         client,
		networkManager: networkManager,
	}
}

//lint:ignore U1000 generic interfaces throw false-positives https://github.com/dominikh/go-tools/issues/1440
func (m *egressFirewallManager) get(namespace, name string) (*egressfirewallapi.EgressFirewall, error) {
	return m.lister.EgressFirewalls(namespace).Get(name)
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) getMessages(egressFirewall *egressfirewallapi.EgressFirewall) []string {
	return egressFirewall.Status.Messages
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) getManagedFields(egressFirewall *egressfirewallapi.EgressFirewall) []metav1.ManagedFieldsEntry {
	return egressFirewall.ManagedFields
}

func (m *egressFirewallManager) getRelevantZones(egressFirewall *egressfirewallapi.EgressFirewall, zones sets.Set[string]) (sets.Set[string], error) {
	activeNetwork, err := m.networkManager.GetActiveNetworkForNamespace(egressFirewall.Namespace)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) {
			return nil, err
		}
		return nil, err
	}
	if activeNetwork == nil {
		return sets.New[string](), nil
	}
	if activeNetwork.IsDefault() {
		return zones.Clone(), nil
	}

	nodes, err := m.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	relevantZones := sets.New[string]()
	for _, node := range nodes {
		if !m.networkManager.NodeHasNetwork(node.Name, activeNetwork.GetNetworkName()) {
			continue
		}
		if zones.Has(node.Name) {
			relevantZones.Insert(node.Name)
		}
	}
	return relevantZones, nil
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) updateStatus(ctx context.Context, egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions,
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

	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}

//lint:ignore U1000 generic interfaces throw false-positives
func (m *egressFirewallManager) cleanupStatus(ctx context.Context, egressFirewall *egressfirewallapi.EgressFirewall, applyOpts *metav1.ApplyOptions) error {
	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace)
	_, err := m.client.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(ctx, applyObj, *applyOpts)
	return err
}
