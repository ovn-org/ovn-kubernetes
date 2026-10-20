# Metrics

OVN-Kubernetes exports Prometheus metrics for its own components and for the
OVN and Open vSwitch services that provide the network data plane. Metrics are
documented by component:

- [OVN-Kubernetes control plane](metrics/ovnkube-control-plane.md) covers
  `ovnkube_clustermanager_*` metrics.
- [OVN-Kubernetes node](metrics/ovnkube-node.md) covers
  `ovnkube_controller_*`, `ovnkube_node_*`, and libovsdb client metrics.
- [OVN](metrics/ovn.md) covers `ovn_controller_*`, `ovn_northd_*`, and
  `ovn_db_*` metrics.
- [Open vSwitch](metrics/ovs.md) covers `ovs_*` metrics.

The catalogs list metrics registered by OVN-Kubernetes source code. A metric
may be absent when its component, feature, or metrics option is disabled.

OVN and OVS coverage metrics are exported as Prometheus gauges even when the
underlying daemon value is cumulative. A daemon restart can reset these
values.

## Change log
This list is to help notify if there are additions, changes or removals to metrics. Latest changes are at the top of this list.

- Remove the Raft-only `ovn_db_cluster_*` metrics because supported OVN NB and SB databases run as standalone instances.
- Add `ovnkube_clustermanager_route_advertisement_condition`, `ovnkube_clustermanager_cluster_user_defined_network_condition`, and `ovnkube_clustermanager_vtep_condition` condition metrics
- Add `transport` label to `ovnkube_clustermanager_cluster_user_defined_networks` to distinguish CUDNs by transport type (Default, EVPN, NoOverlay)
- Add metrics to track logfile size for ovnkube processes - ovnkube_node_logfile_size_bytes and ovnkube_controller_logfile_size_bytes
- Remove ovnkube_controller_ovn_cli_latency_seconds metrics since we have moved most of the OVN DB operations to libovsdb.
- Effect of OVN IC architecture:
  - Move all the metrics from the legacy master subsystem to subsystem "ovnkube-controller". Metrics scraping continues through the controller and cluster-manager pods. See https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3723 for details
  - Move the following metrics from subsystem "master" to subsystem "clustermanager". Therefore, the following metrics are renamed.
    - `ovnkube_master_num_v4_host_subnets` -> `ovnkube_clustermanager_num_v4_host_subnets`
    - `ovnkube_master_num_v6_host_subnets` -> `ovnkube_clustermanager_num_v6_host_subnets`
    - `ovnkube_master_allocated_v4_host_subnets` -> `ovnkube_clustermanager_allocated_v4_host_subnets`
    - `ovnkube_master_allocated_v6_host_subnets` -> `ovnkube_clustermanager_allocated_v6_host_subnets`
    - `ovnkube_master_num_egress_ips` -> `ovnkube_clustermanager_num_egress_ips`
    - `ovnkube_master_egress_ips_node_unreachable_total` -> `ovnkube_clustermanager_egress_ips_node_unreachable_total`
    - `ovnkube_master_egress_ips_rebalance_total` -> `ovnkube_clustermanager_egress_ips_rebalance_total`
- Update description of ovnkube_master_pod_creation_latency_seconds
- Add libovsdb metrics - ovnkube_master_libovsdb_disconnects_total and ovnkube_master_libovsdb_monitors.
- Add ovn_controller_southbound_database_connected metric (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3117).
- Stopwatch metrics now report in seconds instead of milliseconds.
- Rename (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3022):
  - `ovs_vswitchd_interface_link_resets` -> `ovs_vswitchd_interface_resets_total`
  - `ovs_vswitchd_interface_rx_dropped` -> `ovs_vswitchd_interface_rx_dropped_total`
  - `ovs_vswitchd_interface_tx_dropped` -> `ovs_vswitchd_interface_tx_dropped_total`
  - `ovs_vswitchd_interface_rx_errors` -> `ovs_vswitchd_interface_rx_errors_total`
  - `ovs_vswitchd_interface_tx_errors` -> `ovs_vswitchd_interface_tx_errors_total`
  - `ovs_vswitchd_interface_collisions` -> `ovs_vswitchd_interface_collisions_total`
- Remove (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3022):
  - `ovs_vswitchd_dp_if`
  - `ovs_vswitchd_interface_driver_name`
  - `ovs_vswitchd_interface_driver_version`
  - `ovs_vswitchd_interface_firmware_version`
  - `ovs_vswitchd_interface_rx_packets`
  - `ovs_vswitchd_interface_tx_packets`
  - `ovs_vswitchd_interface_rx_bytes`
  - `ovs_vswitchd_interface_tx_bytes`
  - `ovs_vswitchd_interface_rx_frame_err`
  - `ovs_vswitchd_interface_rx_over_err`
  - `ovs_vswitchd_interface_rx_crc_err`
  - `ovs_vswitchd_interface_name`
  - `ovs_vswitchd_interface_duplex`
  - `ovs_vswitchd_interface_type`
  - `ovs_vswitchd_interface_admin_state`
  - `ovs_vswitchd_interface_link_state`
  - `ovs_vswitchd_interface_ifindex`
  - `ovs_vswitchd_interface_link_speed`
  - `ovs_vswitchd_interface_mtu`
  - `ovs_vswitchd_interface_ofport`
  - `ovs_vswitchd_interface_ingress_policing_burst`
  - `ovs_vswitchd_interface_ingress_policing_rate`
- Add `ovnkube_master_network_programming_duration_seconds` and `ovnkube_master_network_programming_ovn_duration_seconds` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2878)
- Remove `ovnkube_master_skipped_nbctl_daemon_total` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2707)
- Add `ovnkube_master_egress_routing_via_host` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/2833)
- Add `ovnkube_resource_retry_failures_total` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3314)
- Add `ovs_vswitchd_interfaces_total` and `ovs_vswitchd_interface_up_wait_seconds_total` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3391)
- Add `ovnkube_controller_admin_network_policies` and `ovnkube_controller_baseline_admin_network_policies` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4239)
- Add `ovnkube_controller_admin_network_policies_db_objects` and `ovnkube_controller_baseline_admin_network_policies_db_objects` (https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4254)
