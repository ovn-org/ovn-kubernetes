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
This list is to help notify if there are additions, changes or removals to metrics.

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
