// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// AggregateOutcome is the derived coarse health of a resource across nodes.
type AggregateOutcome string

const (
	// OutcomeSuccess means every relevant node reported sync_succeeded == 1.
	OutcomeSuccess AggregateOutcome = "success"
	// OutcomeFailure means at least one relevant node reported sync_succeeded == 0.
	OutcomeFailure AggregateOutcome = "failure"
	// OutcomeIncomplete means the relevant set is empty or at least one relevant
	// node has no series yet. Do not patch a success outcome in this case.
	OutcomeIncomplete AggregateOutcome = "incomplete"
)

const maxFailingNodesInSummary = 5

// AggregateFromSamples derives the aggregate outcome for a resource from
// Prometheus samples, considering only nodes in relevantNodes.
func AggregateFromSamples(samples []Sample, relevantNodes sets.Set[string]) (AggregateOutcome, []string) {
	if relevantNodes.Len() == 0 {
		return OutcomeIncomplete, nil
	}

	byNode := map[string]float64{}
	for _, s := range samples {
		node := s.Labels["node"]
		if node == "" || !relevantNodes.Has(node) {
			continue
		}
		byNode[node] = s.Value
	}

	var failing []string
	for node := range relevantNodes {
		val, ok := byNode[node]
		if !ok {
			return OutcomeIncomplete, nil
		}
		if val == 0 {
			failing = append(failing, node)
		}
	}
	if len(failing) > 0 {
		sort.Strings(failing)
		return OutcomeFailure, failing
	}
	return OutcomeSuccess, nil
}

// FormatFailingNodesMessage returns up to maxFailingNodesInSummary failing node
// names sorted lexicographically, suitable for inclusion in a status message.
func FormatFailingNodesMessage(failingNodes []string) string {
	if len(failingNodes) == 0 {
		return ""
	}
	sorted := append([]string(nil), failingNodes...)
	sort.Strings(sorted)
	if len(sorted) > maxFailingNodesInSummary {
		sorted = sorted[:maxFailingNodesInSummary]
	}
	return fmt.Sprintf("failing nodes: %s", strings.Join(sorted, ", "))
}

// BuildSyncSucceededQuery builds a PromQL selector for a sync_succeeded metric.
func BuildSyncSucceededQuery(metricName string, namespace, name string) string {
	var b strings.Builder
	b.WriteString(metricName)
	b.WriteString("{")
	first := true
	if namespace != "" {
		b.WriteString(fmt.Sprintf(`namespace="%s"`, namespace))
		first = false
	}
	if name != "" {
		if !first {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf(`name="%s"`, name))
	}
	b.WriteString("}")
	return b.String()
}
