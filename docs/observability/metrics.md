# Metrics
## OVN-Kubernetes master
This includes a description of a selective set of metrics and to explore the exhausted set, see `go-controller/pkg/metrics/master.go`
### Configuration duration recorder
#### Setup
Enabled by default with the `kind.sh` (in directory `$ROOT/contrib`) [Kind](https://kind.sigs.k8s.io/) setup script.
Disabled by default for binary ovnkube-master and enabled with flag `--metrics-enable-config-duration`.
#### High-level description
This set of metrics gives a result for the upper bound duration which means, it has taken at most this amount of seconds to apply the configuration to all nodes. It does not represent the exact accurate time to apply only this configuration.
Measurement accuracy can be impacted by other parallel processing that might be occurring while the measurement is in progress therefore, the accuracy of the measurements should only indicate upper bound duration to roll out configuration changes.
#### Metrics
| Name | Prometheus type | Description  |
|--|--|--|
|ovnkube_master_network_programming_duration_seconds | Histogram | The duration to apply network configuration for a kind (e.g. pod, service, networkpolicy). Configuration includes add, update and delete events for kinds. This includes OVN-Kubernetes master and OVN duration.
|ovnkube_master_network_programming_ovn_duration_seconds| Histogram  | The duration for OVN to apply network configuration for a kind (e.g. pod, service, networkpolicy).

## Change log
This list is to help notify if there are additions, changes or removals to metrics. Latest changes are at the top of this list.

- Add metrics to track logfile size for ovnkube processes - ovnkube_node_logfile_size_bytes and ovnkube_controller_logfile_size_bytes
- Remove ovnkube_controller_ovn_cli_latency_seconds metrics since we have moved most of the OVN DB operations to libovsdb.
- Effect of OVN IC architecture:
  - Move all the metrics from subsystem "ovnkube-master" to subsystem "ovnkube-controller". The non-IC and IC deployments will each continue to have their ovnkube-master and ovnkube-controller containers running inside the ovnkube-master and ovnkube-controller pods. The metrics scraping should work seemlessly. See https://github.com/ovn-org/ovn-kubernetes/pull/3723 for details
  - Move the following metrics from subsystem "master" to subsystem "clustermanager". Therefore, the follow metrics are renamed.
    - `ovnkube_master_num_v4_host_subnets` -> `ovnkube_clustermanager_num_v4_host_subnets`
    - `ovnkube_master_num_v6_host_subnets` -> `ovnkube_clustermanager_num_v6_host_subnets`
    - `ovnkube_master_allocated_v4_host_subnets` -> `ovnkube_clustermanager_allocated_v4_host_subnets`
    - `ovnkube_master_allocated_v6_host_subnets` -> `ovnkube_clustermanager_allocated_v6_host_subnets`
    - `ovnkube_master_num_egress_ips` -> `ovnkube_clustermanager_num_egress_ips`
    - `ovnkube_master_egress_ips_node_unreachable_total` -> `ovnkube_clustermanager_egress_ips_node_unreachable_total`
    - `ovnkube_master_egress_ips_rebalance_total` -> `ovnkube_clustermanager_egress_ips_rebalance_total`
- Update description of ovnkube_master_pod_creation_latency_seconds
- Add libovsdb metrics - ovnkube_master_libovsdb_disconnects_total and ovnkube_master_libovsdb_monitors.
- Add ovn_controller_southbound_database_connected metric (https://github.com/ovn-org/ovn-kubernetes/pull/3117).
- Stopwatch metrics now report in seconds instead of milliseconds.
- Rename (https://github.com/ovn-org/ovn-kubernetes/pull/3022):
  - `ovs_vswitchd_interface_link_resets` -> `ovs_vswitchd_interface_resets_total`
  - `ovs_vswitchd_interface_rx_dropped` -> `ovs_vswitchd_interface_rx_dropped_total`
  - `ovs_vswitchd_interface_tx_dropped` -> `ovs_vswitchd_interface_tx_dropped_total`
  - `ovs_vswitchd_interface_rx_errors` -> `ovs_vswitchd_interface_rx_errors_total`
  - `ovs_vswitchd_interface_tx_errors` -> `ovs_vswitchd_interface_tx_errors_total`
  - `ovs_vswitchd_interface_collisions` -> `ovs_vswitchd_interface_collisions_total`
- Remove (https://github.com/ovn-org/ovn-kubernetes/pull/3022):
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
- Add `ovnkube_master_network_programming_duration_seconds` and `ovnkube_master_network_programming_ovn_duration_seconds` (https://github.com/ovn-org/ovn-kubernetes/pull/2878)
- Remove `ovnkube_master_skipped_nbctl_daemon_total` (https://github.com/ovn-org/ovn-kubernetes/pull/2707)
- Add `ovnkube_master_egress_routing_via_host` (https://github.com/ovn-org/ovn-kubernetes/pull/2833)
- Add `ovnkube_resource_retry_failures_total` (https://github.com/ovn-org/ovn-kubernetes/pull/3314)
- Add `ovs_vswitchd_interfaces_total` and `ovs_vswitchd_interface_up_wait_seconds_total` (https://github.com/ovn-org/ovn-kubernetes/pull/3391)
- Add `ovnkube_controller_admin_network_policies` and `ovnkube_controller_baseline_admin_network_policies` (https://github.com/ovn-org/ovn-kubernetes/pull/4239)
- Add `ovnkube_controller_admin_network_policies_db_objects` and `ovnkube_controller_baseline_admin_network_policies_db_objects` (https://github.com/ovn-org/ovn-kubernetes/pull/4254)
