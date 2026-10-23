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

	networkqosapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	networkqoslisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	networkQoSSuccessStatus = "NetworkQoS Destinations applied"
	fullMetricNetworkQoS    = types.MetricOvnkubeNamespace + "_" + metrics.MetricNetworkQoSSyncSucceeded
)

type networkQoSRoller struct {
	lister         networkqoslisters.NetworkQoSLister
	nodeLister     corelisters.NodeLister
	client         networkqosclientset.Interface
	networkManager networkmanager.Interface
}

// NewNetworkQoSRoller creates a Prometheus-backed rollup for NetworkQoS.
func NewNetworkQoSRoller(
	lister networkqoslisters.NetworkQoSLister,
	nodeLister corelisters.NodeLister,
	client networkqosclientset.Interface,
	networkManager networkmanager.Interface,
) ResourceRoller {
	return &networkQoSRoller{
		lister:         lister,
		nodeLister:     nodeLister,
		client:         client,
		networkManager: networkManager,
	}
}

func (r *networkQoSRoller) Name() string       { return "networkqos" }
func (r *networkQoSRoller) MetricName() string { return fullMetricNetworkQoS }

func (r *networkQoSRoller) ListKeys() ([]ResourceKey, error) {
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

func (r *networkQoSRoller) RelevantNodes(_ ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	// StatusManager today uses all zone_tracker nodes. When NetworkQoS is
	// network-scoped with dynamic UDN, further narrowing can reuse
	// NodeHasNetwork similarly to EgressFirewall; keep the full zone set for now.
	_ = r.nodeLister
	_ = r.networkManager
	return zones.Clone(), nil
}

func (r *networkQoSRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error {
	nqos, err := r.lister.NetworkQoSes(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}

	var newStatus string
	switch outcome {
	case OutcomeSuccess:
		newStatus = networkQoSSuccessStatus
	case OutcomeFailure:
		newStatus = types.NetworkQoSErrorMsg
		if msg := FormatFailingNodesMessage(failingNodes); msg != "" {
			newStatus = fmt.Sprintf("%s (%s)", types.NetworkQoSErrorMsg, msg)
		}
	case OutcomeIncomplete:
		if strings.Contains(nqos.Status.Status, types.NetworkQoSErrorMsg) {
			newStatus = nqos.Status.Status
		} else {
			newStatus = ""
		}
	}

	if nqos.Status.Status == newStatus {
		return nil
	}

	applyStatus := networkqosapply.Status()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}
	applyObj := networkqosapply.NetworkQoS(nqos.Name, nqos.Namespace).WithStatus(applyStatus)
	_, err = r.client.K8sV1alpha1().NetworkQoSes(nqos.Namespace).ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{
		Force:        true,
		FieldManager: clusterManagerName,
	})
	return err
}
