// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// Route source labels for metricRouteImportRoutes.
const (
	RouteSourceBGP = "bgp"
	RouteSourceOVN = "ovn"
)

var metricRouteImportSyncDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "route_import_sync_duration_seconds",
	Help: "Time spent importing BGP routes for a network, covering the netlink route dump, " +
		"the OVN static route scan, the diff and the resulting transaction.",
	Buckets: prometheus.ExponentialBuckets(.001, 2, 16), // 1ms to ~32s
}, []string{"network", "result"})

var metricRouteImportRoutes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "route_import_routes",
	Help: "Routes seen by the most recent import sync for a network. Source 'bgp' is what the " +
		"kernel VRF table holds, source 'ovn' is what the gateway router holds.",
}, []string{"network", "source"})

var metricRouteImportOps = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "route_import_ops_total",
	Help:      "Logical router static routes added or deleted by route import, by network.",
}, []string{"network", "op"})

var metricEVPNPodProgramDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_pod_program_duration_seconds",
	Help:      "Time spent programming the FDB and neighbor entries for a single pod on an EVPN network.",
	Buckets:   prometheus.ExponentialBuckets(.0005, 2, 14), // 0.5ms to ~4s
}, []string{"result"})

var metricEVPNPodEntries = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_pod_entries",
	Help: "Pods on this node with EVPN FDB entries programmed. EVPN route count scales with " +
		"this, not with node count.",
})

var metricEVPNNeighEntries = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_neigh_entries",
	Help:      "Permanent neighbor entries programmed on this node for pods on EVPN networks.",
})

var metricEVPNVTEPReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_vtep_reconcile_duration_seconds",
	Help:      "Time spent reconciling the netlink devices for a VTEP.",
	Buckets:   prometheus.ExponentialBuckets(.005, 2, 14), // 5ms to ~40s
}, []string{"vtep", "result"})

var metricEVPNNetworks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_networks",
	Help:      "EVPN networks this node resolves to a VTEP, as of the last reconcile.",
}, []string{"vtep"})

var metricEVPNVLANsUsed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "evpn_vlans_used",
	Help: "VLAN IDs this node maps on a VTEP's single VXLAN device, one per MAC-VRF and one " +
		"per IP-VRF. The hard limit is 4094. Reported as resolved rather than as programmed, " +
		"so a count that exceeds what the kernel accepted is still visible; pair with " +
		"evpn_vtep_reconcile_duration_seconds{result=\"error\"} to tell the two apart.",
}, []string{"vtep"})

// registerNodeBGPMetrics registers the route import and EVPN node metrics. Called
// from RegisterNodeMetrics.
func registerNodeBGPMetrics() {
	if config.OVNKubernetesFeature.EnableRouteAdvertisements {
		prometheus.MustRegister(metricRouteImportSyncDuration)
		prometheus.MustRegister(metricRouteImportRoutes)
		prometheus.MustRegister(metricRouteImportOps)
	}
	if config.OVNKubernetesFeature.EnableEVPN {
		prometheus.MustRegister(metricEVPNPodProgramDuration)
		prometheus.MustRegister(metricEVPNPodEntries)
		prometheus.MustRegister(metricEVPNNeighEntries)
		prometheus.MustRegister(metricEVPNVTEPReconcileDuration)
		prometheus.MustRegister(metricEVPNNetworks)
		prometheus.MustRegister(metricEVPNVLANsUsed)
	}
}

// RecordRouteImportSync records the duration and outcome of a route import sync
// for a network.
func RecordRouteImportSync(network, result string, duration time.Duration) {
	metricRouteImportSyncDuration.WithLabelValues(network, result).Observe(duration.Seconds())
}

// RecordRouteImportRoutes records how many routes the most recent sync saw on each
// side of the diff.
func RecordRouteImportRoutes(network string, bgp, ovn int) {
	metricRouteImportRoutes.WithLabelValues(network, RouteSourceBGP).Set(float64(bgp))
	metricRouteImportRoutes.WithLabelValues(network, RouteSourceOVN).Set(float64(ovn))
}

// RecordRouteImportOps counts the static routes a sync added and deleted.
func RecordRouteImportOps(network string, adds, deletes int) {
	if adds > 0 {
		metricRouteImportOps.WithLabelValues(network, "add").Add(float64(adds))
	}
	if deletes > 0 {
		metricRouteImportOps.WithLabelValues(network, "delete").Add(float64(deletes))
	}
}

// DeleteRouteImportMetrics removes the timeseries for a network that is no longer
// imported.
func DeleteRouteImportMetrics(network string) {
	labels := prometheus.Labels{"network": network}
	metricRouteImportSyncDuration.DeletePartialMatch(labels)
	metricRouteImportRoutes.DeletePartialMatch(labels)
	metricRouteImportOps.DeletePartialMatch(labels)
}

// RecordEVPNPodProgram records the duration and outcome of programming one pod's
// EVPN FDB and neighbor entries.
func RecordEVPNPodProgram(result string, duration time.Duration) {
	metricEVPNPodProgramDuration.WithLabelValues(result).Observe(duration.Seconds())
}

// RecordEVPNPodEntries records how many pods and neighbor entries this node currently
// programs for EVPN networks.
func RecordEVPNPodEntries(pods, neighbors int) {
	metricEVPNPodEntries.Set(float64(pods))
	metricEVPNNeighEntries.Set(float64(neighbors))
}

// RecordEVPNVTEPReconcile records the duration and outcome of a VTEP device reconcile.
func RecordEVPNVTEPReconcile(vtep, result string, duration time.Duration) {
	metricEVPNVTEPReconcileDuration.WithLabelValues(vtep, result).Observe(duration.Seconds())
}

// RecordEVPNVTEPUsage records how many networks and VLAN IDs a VTEP resolves to.
// VLAN usage is bounded at 4094 by the single VXLAN device.
func RecordEVPNVTEPUsage(vtep string, networks, vlans int) {
	metricEVPNNetworks.WithLabelValues(vtep).Set(float64(networks))
	metricEVPNVLANsUsed.WithLabelValues(vtep).Set(float64(vlans))
}

// DeleteEVPNVTEPMetrics removes the timeseries for a deleted VTEP.
func DeleteEVPNVTEPMetrics(vtep string) {
	labels := prometheus.Labels{"vtep": vtep}
	metricEVPNVTEPReconcileDuration.DeletePartialMatch(labels)
	metricEVPNNetworks.DeletePartialMatch(labels)
	metricEVPNVLANsUsed.DeletePartialMatch(labels)
}
