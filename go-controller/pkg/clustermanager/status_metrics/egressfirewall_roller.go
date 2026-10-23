// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"

	egressfirewallapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressfirewalllisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	egressFirewallSuccessStatus = "EgressFirewall Rules applied"
	fullMetricEgressFirewall    = types.MetricOvnkubeNamespace + "_" + metrics.MetricEgressFirewallSyncSucceeded
)

type egressFirewallRoller struct {
	lister         egressfirewalllisters.EgressFirewallLister
	nodeLister     corelisters.NodeLister
	client         egressfirewallclientset.Interface
	networkManager networkmanager.Interface
}

// NewEgressFirewallRoller creates a Prometheus-backed rollup for EgressFirewall.
func NewEgressFirewallRoller(
	lister egressfirewalllisters.EgressFirewallLister,
	nodeLister corelisters.NodeLister,
	client egressfirewallclientset.Interface,
	networkManager networkmanager.Interface,
) ResourceRoller {
	return &egressFirewallRoller{
		lister:         lister,
		nodeLister:     nodeLister,
		client:         client,
		networkManager: networkManager,
	}
}

func (r *egressFirewallRoller) Name() string { return "egressfirewall" }

func (r *egressFirewallRoller) MetricName() string { return fullMetricEgressFirewall }

func (r *egressFirewallRoller) ListKeys() ([]ResourceKey, error) {
	objs, err := r.lister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	keys := make([]ResourceKey, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, ResourceKey{Namespace: o.Namespace, Name: o.Name})
	}
	return keys, nil
}

func (r *egressFirewallRoller) RelevantNodes(key ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	activeNetwork, err := r.networkManager.GetActiveNetworkForNamespace(key.Namespace)
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

	nodes, err := r.nodeLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	relevantZones := sets.New[string]()
	for _, node := range nodes {
		if !r.networkManager.NodeHasNetwork(node.Name, activeNetwork.GetNetworkName()) {
			continue
		}
		if zones.Has(node.Name) {
			relevantZones.Insert(node.Name)
		}
	}
	return relevantZones, nil
}

func (r *egressFirewallRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error {
	ef, err := r.lister.EgressFirewalls(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}

	var newStatus string
	switch outcome {
	case OutcomeSuccess:
		newStatus = egressFirewallSuccessStatus
	case OutcomeFailure:
		newStatus = types.EgressFirewallErrorMsg
		if msg := FormatFailingNodesMessage(failingNodes); msg != "" {
			newStatus = fmt.Sprintf("%s (%s)", types.EgressFirewallErrorMsg, msg)
		}
	case OutcomeIncomplete:
		// Same as legacy applyEmptyOrFailed: clear success, keep failure if present.
		if strings.Contains(ef.Status.Status, types.EgressFirewallErrorMsg) {
			newStatus = ef.Status.Status
		} else {
			newStatus = ""
		}
	}

	if ef.Status.Status == newStatus {
		return nil
	}

	applyStatus := egressfirewallapply.EgressFirewallStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}
	applyObj := egressfirewallapply.EgressFirewall(ef.Name, ef.Namespace).WithStatus(applyStatus)
	_, err = r.client.K8sV1().EgressFirewalls(ef.Namespace).ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{
		Force:        true,
		FieldManager: clusterManagerName,
	})
	return err
}
