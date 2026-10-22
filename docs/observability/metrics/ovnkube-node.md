# OVN-Kubernetes node metrics

This page documents metrics produced by ovnkube-controller, ovnkube-node, and
their libovsdb clients. Metrics collected from the OVN and OVS daemons are
listed separately in the [OVN](ovn.md) and [Open vSwitch](ovs.md) catalogs.

## OVN-Kubernetes Controller Metrics

All metrics in this section are rooted at `ovnkube_controller_`.

### Lifecycle and Resource Processing

Consecutive pending update events for the same resource key are coalesced only
when the incoming update keeps the UID unchanged and neither update changes a
handler's filter membership. Add and delete callbacks remain separate and retain
their per-key order. The resource counter and latency histograms below measure
events dispatched after coalescing; the latency histograms time the handler pass
for each dispatched event. The coalesced event counter counts incoming updates
merged into a pending update.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `revision`, `branch`, `build_user`, `build_date`, `goversion` | Build information. The value is always `1`. |
| `ready_duration_seconds` | Gauge | None | Time for ovnkube-controller to become ready. |
| `sync_duration_seconds` | Gauge | `resource_name` | Time to complete initial synchronization and set up handlers for a resource. |
| `resource_update_total` | Counter | `name`, `event` | Resource add, update, and delete events dispatched after per-key queue coalescing. |
| `resource_update_coalesced_total` | Counter | `name` | Queued update events merged into another callback for the same resource key. |
| `resource_add_latency_seconds` | Histogram | None | Time to process all handlers for a delivered add callback. |
| `resource_update_latency_seconds` | Histogram | None | Time to process all handlers for a delivered update callback. |
| `resource_delete_latency_seconds` | Histogram | None | Time to process all handlers for a delivered delete callback. |
| `logfile_size_bytes` | Gauge | `logfile_name` | Size of the configured ovnkube-controller log file. No series is produced when file logging is not configured. |

### Pod and Service Programming

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `pod_creation_latency_seconds` | Histogram | None | Time from pod scheduling until logical switch port configuration completes. User-defined network pods are not currently recorded. |
| `pod_first_seen_lsp_created_duration_seconds` | Histogram | None | Time from first observing a pod until its logical switch port is created. |
| `pod_lsp_created_port_binding_duration_seconds` | Histogram | None | Time from logical switch port creation until the port binding is observed. |
| `pod_port_binding_port_binding_chassis_duration_seconds` | Histogram | None | Time from observing the port binding until its chassis assignment is observed. |
| `pod_port_binding_chassis_port_binding_up_duration_seconds` | Histogram | None | Time from chassis assignment until the port binding is observed as up. |
| `requeue_service_total` | Counter | None | Service reconciliations requeued after failing to synchronize with OVN. |
| `sync_service_total` | Counter | None | Service synchronizations with OVN load balancers. |
| `sync_service_latency_seconds` | Histogram | None | Time to synchronize a service with OVN load balancers. |

### OVN State and Configuration

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `nb_e2e_timestamp` | Gauge | None | Unix timestamp written by this controller to the northbound database. |
| `sb_e2e_timestamp` | Gauge | None | Unix timestamp observed in the southbound database. |
| `ipsec_enabled` | Gauge | None | Whether IPsec is enabled: `1` for enabled and `0` otherwise. |
| `egress_routing_via_host` | Gauge | None | Gateway mode: `0` for shared, `1` for local, and `2` for an invalid mode. |
| `num_egress_firewalls` | Gauge | None | Number of egress firewall policies. |
| `num_egress_firewall_rules` | Gauge | None | Number of egress firewall rules. |

### Admin Network Policy

The rule collectors are available when the AdminNetworkPolicy controller is
running. Database object series are updated when AdminNetworkPolicy is
enabled.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `admin_network_policies` | Gauge | None | Number of AdminNetworkPolicy resources. |
| `baseline_admin_network_policies` | Gauge | None | Number of BaselineAdminNetworkPolicy resources. |
| `admin_network_policies_rules` | Gauge | `direction`, `action` | Number of rules across AdminNetworkPolicy resources. Actions are `Allow`, `Deny`, and `Pass`. |
| `baseline_admin_network_policies_rules` | Gauge | `direction`, `action` | Number of rules in the BaselineAdminNetworkPolicy. Actions are `Allow` and `Deny`. |
| `admin_network_policies_db_objects` | Gauge | `table_name` | OVN northbound database objects owned by the AdminNetworkPolicy controller. |
| `baseline_admin_network_policies_db_objects` | Gauge | `table_name` | OVN northbound database objects owned by the BaselineAdminNetworkPolicy controller. |

### Configuration Duration Recorder

These metrics are registered only when
`--metrics-enable-config-duration` is enabled. They report an upper bound:
unrelated work and the slowest node may increase a measurement.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `network_programming_duration_seconds` | Histogram | `kind` | End-to-end time to apply an add, update, or delete for a Kubernetes resource kind. The value includes OVN time when it is measured. |
| `network_programming_ovn_duration_seconds` | Histogram | None | Time for OVN to apply measured configuration to all relevant nodes. |

### Scale Metrics

These metrics are registered only when `--metrics-enable-scale` is enabled.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `egress_ips_assign_latency_seconds` | Histogram | None | Time to assign an egress IP in the OVN northbound database. |
| `egress_ips_unassign_latency_seconds` | Histogram | None | Time to unassign an egress IP from the OVN northbound database. |
| `network_policy_event_latency_seconds` | Histogram | `event` | Time to handle a complete network policy create or delete event. |
| `network_policy_local_pod_event_latency_seconds` | Histogram | `event` | Time to handle a local pod add or delete for network policy. |
| `network_policy_peer_namespace_event_latency_seconds` | Histogram | `event` | Time to handle a peer namespace add or delete for network policy. |
| `pod_selector_address_set_pod_event_latency_seconds` | Histogram | `event` | Time to handle a peer pod add or delete for a pod-selector address set. |
| `pod_selector_address_set_namespace_event_latency_seconds` | Histogram | `event` | Time to handle a peer namespace add or delete for a pod-selector address set. |
| `pod_event_latency_seconds` | Histogram | `event` | Time to handle a pod add, update, or delete. |
| `udn_nbdb_programmed_duration_seconds` | Histogram | `topology` | Time to program the northbound database while initializing a UDN, grouped by topology. |
| `workqueue_depth` | Gauge | `name` | Current workqueue depth. |
| `workqueue_adds_total` | Counter | `name` | Items added to a workqueue. |
| `workqueue_queue_duration_seconds` | Histogram | `name` | Time an item remains queued before processing. |
| `workqueue_work_duration_seconds` | Histogram | `name` | Time spent processing an item. |
| `workqueue_unfinished_work_seconds` | Gauge | `name` | Total in-progress work not yet observed by the work-duration metric. |
| `workqueue_longest_running_processor_seconds` | Gauge | `name` | Runtime of the longest-running workqueue processor. |
| `workqueue_retries_total` | Counter | `name` | Workqueue retries. |

### NetworkQoS

NetworkQoS metrics are registered by the NetworkQoS controller package.
Duration values use milliseconds, as indicated by the metric names.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `num_network_qoses` | Gauge | `network` | Number of NetworkQoS resources per network. |
| `nqos_ovn_operation_duration_ms` | Histogram | `operation` | Time spent on an OVN operation during NetworkQoS reconciliation. |
| `nqos_creation_duration_ms` | Histogram | `network` | Time spent reconciling a NetworkQoS event. |
| `nqos_deletion_duration_ms` | Histogram | `network` | Time spent reconciling a pod event for NetworkQoS. The historical metric name is retained for compatibility. |
| `nqos_ns_reconcile_duration_ms` | Histogram | `network` | Time spent applying a namespace change to related NetworkQoS pods. |
| `nqos_status_patch_duration_ms` | Histogram | `network` | Time spent patching NetworkQoS status. |

## OVN-Kubernetes Node Metrics

All metrics in this section are rooted at `ovnkube_node_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `revision`, `branch`, `build_user`, `build_date`, `goversion` | Build information. The value is always `1`. |
| `ready_duration_seconds` | Gauge | None | Time for ovnkube-node to become ready. |
| `cni_request_duration_seconds` | Histogram | `command`, `err` | Duration of CNI server requests, grouped by command and error status. |
| `nodeport_enabled` | Gauge | None | Whether NodePort is enabled on the node: `1` for enabled and `0` otherwise. |
| `logfile_size_bytes` | Gauge | `logfile_name` | Size of the configured ovnkube log file. No series is produced when file logging is not configured. |

## libovsdb Client Metrics

These metrics describe the OVN northbound and southbound clients used by
ovnkube-controller. The `primary_model` constant label distinguishes the
client model. Its values are `OVN_Northbound` and `OVN_Southbound`.

All metrics in this section are rooted at `ovnkube_master_libovsdb_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `update_messages_total` | Counter | `primary_model`, `database` | Monitor update messages processed, grouped by database. |
| `table_updates_total` | Counter | `primary_model`, `database`, `table` | Monitor updates processed, grouped by database and table. |
| `disconnects_total` | Counter | `primary_model` | libovsdb client disconnects. |
| `monitors` | Gauge | `primary_model` | Running libovsdb monitors. |

## Shared OVN-Kubernetes Metrics

All metrics in this section are rooted at `ovnkube_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `resource_retry_failures_total` | Counter | None | Resources that reached the maximum reconciliation retry limit and are no longer processed. This family is shared by OVN-Kubernetes components. |
