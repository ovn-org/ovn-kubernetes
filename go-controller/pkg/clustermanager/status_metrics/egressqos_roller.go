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

	egressqosapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/applyconfiguration/egressqos/v1"
	egressqosclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressqoslisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/listers/egressqos/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	egressQoSSuccessStatus = "EgressQoS Rules applied"
	fullMetricEgressQoS    = types.MetricOvnkubeNamespace + "_" + metrics.MetricEgressQoSSyncSucceeded
)

type egressQoSRoller struct {
	lister egressqoslisters.EgressQoSLister
	client egressqosclientset.Interface
}

// NewEgressQoSRoller creates a Prometheus-backed rollup for EgressQoS.
func NewEgressQoSRoller(lister egressqoslisters.EgressQoSLister, client egressqosclientset.Interface) ResourceRoller {
	return &egressQoSRoller{lister: lister, client: client}
}

func (r *egressQoSRoller) Name() string       { return "egressqos" }
func (r *egressQoSRoller) MetricName() string { return fullMetricEgressQoS }

func (r *egressQoSRoller) ListKeys() ([]ResourceKey, error) {
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

func (r *egressQoSRoller) RelevantNodes(_ ResourceKey, zones sets.Set[string]) (sets.Set[string], error) {
	return zones.Clone(), nil
}

func (r *egressQoSRoller) PatchSummary(key ResourceKey, outcome AggregateOutcome, failingNodes []string) error {
	eq, err := r.lister.EgressQoSes(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}

	var newStatus string
	switch outcome {
	case OutcomeSuccess:
		newStatus = egressQoSSuccessStatus
	case OutcomeFailure:
		newStatus = types.EgressQoSErrorMsg
		if msg := FormatFailingNodesMessage(failingNodes); msg != "" {
			newStatus = fmt.Sprintf("%s (%s)", types.EgressQoSErrorMsg, msg)
		}
	case OutcomeIncomplete:
		if strings.Contains(eq.Status.Status, types.EgressQoSErrorMsg) {
			newStatus = eq.Status.Status
		} else {
			newStatus = ""
		}
	}

	if eq.Status.Status == newStatus {
		return nil
	}

	applyStatus := egressqosapply.EgressQoSStatus()
	if newStatus != "" {
		applyStatus.WithStatus(newStatus)
	}
	applyObj := egressqosapply.EgressQoS(eq.Name, eq.Namespace).WithStatus(applyStatus)
	_, err = r.client.K8sV1().EgressQoSes(eq.Namespace).ApplyStatus(context.TODO(), applyObj, metav1.ApplyOptions{
		Force:        true,
		FieldManager: clusterManagerName,
	})
	return err
}
