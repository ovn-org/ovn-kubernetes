// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var registerClusterManagerBaseMetrics sync.Once
var registerClusterManagerFunctionalMetrics sync.Once

// MetricClusterManagerLeader identifies whether this instance of ovnkube-cluster-manager is a leader or not
var MetricClusterManagerLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "leader",
	Help:      "Identifies whether the instance of ovnkube-cluster-manager is a leader(1) or not(0).",
})

var MetricClusterManagerReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the cluster manager to get to ready state",
})

var metricV4HostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_v4_host_subnets",
	Help:      "The total number of v4 host subnets possible per network"},
	[]string{
		"network_name",
	},
)

var metricV6HostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_v6_host_subnets",
	Help:      "The total number of v6 host subnets possible per network"},
	[]string{
		"network_name",
	},
)

var metricV4AllocatedHostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "allocated_v4_host_subnets",
	Help:      "The total number of v4 host subnets currently allocated per network"},
	[]string{
		"network_name",
	},
)

var metricV6AllocatedHostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "allocated_v6_host_subnets",
	Help:      "The total number of v6 host subnets currently allocated per network"},
	[]string{
		"network_name",
	},
)

/** EgressIP metrics recorded from cluster-manager begins**/
var metricEgressIPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_egress_ips",
	Help:      "The number of defined egress IP addresses",
})

var metricEgressIPNodeUnreacheableCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "egress_ips_node_unreachable_total",
	Help:      "The total number of times assigned egress IP(s) were unreachable"},
)

var metricEgressIPRebalanceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "egress_ips_rebalance_total",
	Help:      "The total number of times assigned egress IP(s) needed to be moved to a different node"},
)

/** EgressIP metrics recorded from cluster-manager ends**/

var metricUDNCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "user_defined_networks",
	Help:      "The total number of UserDefinedNetworks in the cluster"},
	[]string{
		"role",
		"topology",
	},
)

var metricCUDNCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "cluster_user_defined_networks",
	Help:      "The total number of ClusterUserDefinedNetworks in the cluster"},
	[]string{
		"role",
		"topology",
		"transport",
	},
)

var metricUDNNodesRendered = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Subsystem: types.MetricOvnkubeSubsystemClusterManager,
		Name:      "udn_nodes_rendered",
		Help:      "Number of nodes on which a UserDefinedNetwork (UDN) or ClusterUserDefinedNetwork (CUDN) is currently rendered.",
	},
	[]string{
		"network_name",
	},
)

var metricRouteAdvertisementCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisement_condition",
	Help: "Status condition of RouteAdvertisements resources. " +
		"Value is 1 for the active status, 0 for the inactive status. " +
		"Use the 'condition' and 'status' labels to select.",
}, []string{"name", "condition", "status"})

// RouteAdvertisements metrics are emitted per RouteAdvertisements resource so
// that a slow or churning reconcile can be attributed to a specific resource.
// Cardinality is bounded by the number of RouteAdvertisements in the cluster.
var metricRAReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_reconcile_duration_seconds",
	Help:      "Time spent reconciling a RouteAdvertisements resource.",
	Buckets:   prometheus.ExponentialBuckets(.005, 2, 14), // 5ms to ~40s
}, []string{"name", "result"})

var metricRAFRRConfigurations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_frr_configurations",
	Help: "Number of FRRConfigurations currently generated for a RouteAdvertisements resource. " +
		"Grows with the number of nodes and selected source FRRConfigurations.",
}, []string{"name"})

var metricRAFRRConfigurationWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_frr_configuration_writes_total",
	Help: "Generated FRRConfigurations processed by a RouteAdvertisements reconcile, by operation. " +
		"The 'unchanged' operation counts objects that were compared and found up to date.",
}, []string{"name", "op"})

var metricRAAdvertisedPrefixes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_advertised_prefixes",
	Help:      "Number of distinct prefixes advertised by a RouteAdvertisements resource, by IP family.",
}, []string{"name", "family"})

var metricRANADWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_nad_writes_total",
	Help:      "NetworkAttachmentDefinition annotation updates issued by a RouteAdvertisements reconcile.",
}, []string{"name"})

var metricRANADsListed = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "route_advertisements_nads_listed",
	Help: "Number of NetworkAttachmentDefinitions walked by the most recent RouteAdvertisements " +
		"reconcile. Every reconcile walks all of them.",
})

var metricCUDNCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "cluster_user_defined_network_condition",
	Help: "Status condition of ClusterUserDefinedNetwork resources. " +
		"Value is 1 for the active status, 0 for the inactive status. " +
		"Use the 'condition' and 'status' labels to select.",
}, []string{"name", "condition", "status"})

var metricVTEPCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "vtep_condition",
	Help: "Status condition of VTEP resources. " +
		"Value is 1 for the active status, 0 for the inactive status. " +
		"Use the 'condition' and 'status' labels to select.",
}, []string{"name", "condition", "status"})

// These metrics are being emitted per-network to provide granular data for performance testing
// If performance issues arise due to cardinality, these metrics should be aggregated
var metricUDNUpdateNodeAnnotationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "udn_update_node_annotation_duration_seconds",
	Help:      "Time spent updating node annotations during UDN network allocation.",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 13), // 10ms to ~40s
}, []string{"name"})

var metricUDNNADSyncDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "udn_nad_sync_duration_seconds",
	Help:      "Time spent syncing NetworkAttachmentDefinitions during UDN reconciliation.",
	Buckets:   prometheus.ExponentialBuckets(.005, 2, 14), // 5ms to ~40s
}, []string{"name"})

// RegisterClusterManagerBase registers ovnkube cluster manager base metrics with the Prometheus registry.
// This function should only be called once.
func RegisterClusterManagerBase() {
	registerClusterManagerBaseMetrics.Do(func() {
		prometheus.MustRegister(MetricClusterManagerLeader)
		prometheus.MustRegister(MetricClusterManagerReadyDuration)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: types.MetricOvnkubeNamespace,
				Subsystem: types.MetricOvnkubeSubsystemClusterManager,
				Name:      "build_info",
				Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
					"and go version from which ovnkube was built and when and who built it",
				ConstLabels: prometheus.Labels{
					"version":    "0.0",
					"revision":   config.Commit,
					"branch":     config.Branch,
					"build_user": config.BuildUser,
					"build_date": config.BuildDate,
					"goversion":  runtime.Version(),
				},
			},
			func() float64 { return 1 },
		))
	})
}

// RegisterClusterManagerFunctional is a collection of metrics that help us understand ovnkube-cluster-manager functions. Call once after
// LE is won.
func RegisterClusterManagerFunctional() {
	registerClusterManagerFunctionalMetrics.Do(func() {
		prometheus.MustRegister(metricV4HostSubnetCount)
		prometheus.MustRegister(metricV6HostSubnetCount)
		prometheus.MustRegister(metricV4AllocatedHostSubnetCount)
		prometheus.MustRegister(metricV6AllocatedHostSubnetCount)
		if config.OVNKubernetesFeature.EnableEgressIP {
			prometheus.MustRegister(metricEgressIPNodeUnreacheableCount)
			prometheus.MustRegister(metricEgressIPRebalanceCount)
			prometheus.MustRegister(metricEgressIPCount)
		}
		prometheus.MustRegister(metricUDNCount)
		prometheus.MustRegister(metricCUDNCount)
		prometheus.MustRegister(metricCUDNCondition)
		if config.Metrics.EnableScaleMetrics {
			prometheus.MustRegister(metricUDNUpdateNodeAnnotationDuration)
			prometheus.MustRegister(metricUDNNADSyncDuration)
			registerWorkqueueMetrics(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemClusterManager)
			registerClientGoMetrics()
		}
		if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
			prometheus.MustRegister(metricUDNNodesRendered)
		}
		if config.OVNKubernetesFeature.EnableRouteAdvertisements {
			prometheus.MustRegister(metricRouteAdvertisementCondition)
			prometheus.MustRegister(metricRAReconcileDuration)
			prometheus.MustRegister(metricRAFRRConfigurations)
			prometheus.MustRegister(metricRAFRRConfigurationWrites)
			prometheus.MustRegister(metricRAAdvertisedPrefixes)
			prometheus.MustRegister(metricRANADWrites)
			prometheus.MustRegister(metricRANADsListed)
		}
		if config.OVNKubernetesFeature.EnableEVPN {
			prometheus.MustRegister(metricVTEPCondition)
		}
		if err := prometheus.Register(MetricResourceRetryFailuresCount); err != nil {
			var alreadyRegistered prometheus.AlreadyRegisteredError
			if !errors.As(err, &alreadyRegistered) {
				panic(err)
			}
		}
	})
}

// RecordSubnetUsage records the number of subnets allocated for nodes
func RecordSubnetUsage(v4SubnetsAllocated, v6SubnetsAllocated float64, networkName string) {
	metricV4AllocatedHostSubnetCount.WithLabelValues(networkName).Set(v4SubnetsAllocated)
	metricV6AllocatedHostSubnetCount.WithLabelValues(networkName).Set(v6SubnetsAllocated)
}

// RecordSubnetCount records the number of available subnets per configuration
// for ovn-kubernetes
func RecordSubnetCount(v4SubnetCount, v6SubnetCount float64, networkName string) {
	metricV4HostSubnetCount.WithLabelValues(networkName).Set(v4SubnetCount)
	metricV6HostSubnetCount.WithLabelValues(networkName).Set(v6SubnetCount)
}

// RecordEgressIPReachableNode records how many times EgressIP detected an unuseable node.
func RecordEgressIPUnreachableNode() {
	metricEgressIPNodeUnreacheableCount.Inc()
}

// RecordEgressIPRebalance records how many EgressIPs had to move to a different egress node.
func RecordEgressIPRebalance(count int) {
	metricEgressIPRebalanceCount.Add(float64(count))
}

// RecordEgressIPCount records the total number of Egress IPs.
// This total may include multiple Egress IPs per EgressIP CR.
func RecordEgressIPCount(count float64) {
	metricEgressIPCount.Set(count)
}

// IncrementUDNCount increments the number of UserDefinedNetworks of the given type
func IncrementUDNCount(role, topology string) {
	metricUDNCount.WithLabelValues(role, topology).Inc()
}

// DecrementUDNCount decrements the number of UserDefinedNetworks of the given type
func DecrementUDNCount(role, topology string) {
	metricUDNCount.WithLabelValues(role, topology).Dec()
}

// SetCUDNCount sets the CUDN count for a specific label combination.
func SetCUDNCount(role, topology, transport string, count float64) {
	metricCUDNCount.WithLabelValues(role, topology, transport).Set(count)
}

// DeleteCUDNCount removes a label combination from the CUDN count gauge.
func DeleteCUDNCount(role, topology, transport string) {
	metricCUDNCount.DeleteLabelValues(role, topology, transport)
}

// ResetCUDNCount resets the CUDN count gauge, removing all label combinations.
// Intended for use in tests to ensure metric isolation between test cases.
func ResetCUDNCount() {
	metricCUDNCount.Reset()
}

// SetDynamicUDNNodeCount sets the number of nodes currently active with a CUDN/UDN.
func SetDynamicUDNNodeCount(networkName string, nodeCount float64) {
	metricUDNNodesRendered.WithLabelValues(networkName).Set(nodeCount)
}

// DeleteDynamicUDNNodeCount when CUDN/UDN is deleted.
func DeleteDynamicUDNNodeCount(networkName string) {
	metricUDNNodesRendered.DeleteLabelValues(networkName)
}

// recordCondition emits both status="true" and status="false" timeseries for a
// condition gauge; exactly one is 1, the other is 0.
//
// NOTE: this assumes callers never emit ConditionUnknown. If ConditionUnknown
// is passed, both the status="true" and status="false" timeseries will be 0.
// Should ConditionUnknown become a possible value in the future, a dedicated
// metric (or an additional label value) will be needed to track it.
func recordCondition(metric *prometheus.GaugeVec, name, condition string, status metav1.ConditionStatus) {
	metric.WithLabelValues(name, condition, "true").Set(boolFloat64(status == metav1.ConditionTrue))
	metric.WithLabelValues(name, condition, "false").Set(boolFloat64(status == metav1.ConditionFalse))
}

// RecordRouteAdvertisementCondition records the condition metric for a RouteAdvertisements resource.
func RecordRouteAdvertisementCondition(name, condition string, status metav1.ConditionStatus) {
	recordCondition(metricRouteAdvertisementCondition, name, condition, status)
}

// DeleteRouteAdvertisementCondition removes all condition timeseries for a deleted RouteAdvertisements resource.
func DeleteRouteAdvertisementCondition(name string) {
	metricRouteAdvertisementCondition.DeletePartialMatch(prometheus.Labels{"name": name})
}

// RecordRouteAdvertisementReconcile records the duration and outcome of a
// RouteAdvertisements reconcile.
func RecordRouteAdvertisementReconcile(name, result string, duration time.Duration) {
	metricRAReconcileDuration.WithLabelValues(name, result).Observe(duration.Seconds())
}

// RecordRouteAdvertisementFRRConfigurations records how many FRRConfigurations a
// RouteAdvertisements currently generates.
func RecordRouteAdvertisementFRRConfigurations(name string, count int) {
	metricRAFRRConfigurations.WithLabelValues(name).Set(float64(count))
}

// RecordRouteAdvertisementFRRConfigurationWrite counts a generated FRRConfiguration
// processed by a reconcile. Valid operations are create, update, delete and unchanged.
func RecordRouteAdvertisementFRRConfigurationWrite(name, op string) {
	metricRAFRRConfigurationWrites.WithLabelValues(name, op).Inc()
}

// RecordRouteAdvertisementAdvertisedPrefixes records the number of distinct prefixes
// advertised by a RouteAdvertisements for an IP family.
func RecordRouteAdvertisementAdvertisedPrefixes(name, family string, count int) {
	metricRAAdvertisedPrefixes.WithLabelValues(name, family).Set(float64(count))
}

// RecordRouteAdvertisementNADWrite counts a NAD annotation update issued for a
// RouteAdvertisements.
func RecordRouteAdvertisementNADWrite(name string) {
	metricRANADWrites.WithLabelValues(name).Inc()
}

// RecordRouteAdvertisementNADsListed records how many NADs the most recent
// RouteAdvertisements reconcile walked.
func RecordRouteAdvertisementNADsListed(count int) {
	metricRANADsListed.Set(float64(count))
}

// DeleteRouteAdvertisementMetrics removes all per-resource timeseries for a deleted
// RouteAdvertisements.
func DeleteRouteAdvertisementMetrics(name string) {
	labels := prometheus.Labels{"name": name}
	metricRouteAdvertisementCondition.DeletePartialMatch(labels)
	metricRAReconcileDuration.DeletePartialMatch(labels)
	metricRAFRRConfigurations.DeletePartialMatch(labels)
	metricRAFRRConfigurationWrites.DeletePartialMatch(labels)
	metricRAAdvertisedPrefixes.DeletePartialMatch(labels)
	metricRANADWrites.DeletePartialMatch(labels)
}

// RecordCUDNCondition records the condition metric for a ClusterUserDefinedNetwork resource.
func RecordCUDNCondition(name, condition string, status metav1.ConditionStatus) {
	recordCondition(metricCUDNCondition, name, condition, status)
}

// DeleteCUDNCondition removes all condition timeseries for a deleted ClusterUserDefinedNetwork resource.
func DeleteCUDNCondition(name string) {
	metricCUDNCondition.DeletePartialMatch(prometheus.Labels{"name": name})
}

// RecordVTEPCondition records the condition metric for a VTEP resource.
func RecordVTEPCondition(name, condition string, status metav1.ConditionStatus) {
	recordCondition(metricVTEPCondition, name, condition, status)
}

// DeleteVTEPCondition removes all condition timeseries for a deleted VTEP resource.
func DeleteVTEPCondition(name string) {
	metricVTEPCondition.DeletePartialMatch(prometheus.Labels{"name": name})
}

func boolFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// RecordUDNUpdateNodeAnnotationDuration records duration of updating node annotations during UDN allocation
func RecordUDNUpdateNodeAnnotationDuration(name string, duration float64) {
	metricUDNUpdateNodeAnnotationDuration.WithLabelValues(name).Observe(duration)
}

// RecordUDNNADSyncDuration records duration of syncing NADs during UDN reconciliation
func RecordUDNNADSyncDuration(name string, duration float64) {
	metricUDNNADSyncDuration.WithLabelValues(name).Observe(duration)
}

// CleanupUDNMetrics removes all metric time series associated with a deleted UDN to prevent
// unbounded cardinality growth. Should be called when a UDN or CUDN is fully deleted.
func CleanupUDNMetrics(networkName string) {
	metricUDNUpdateNodeAnnotationDuration.DeleteLabelValues(networkName)
	metricUDNNADSyncDuration.DeleteLabelValues(networkName)
}
