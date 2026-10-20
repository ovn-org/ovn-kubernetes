# OVN metrics

OVN-Kubernetes collects metrics from ovn-controller, ovn-northd, and the OVN
northbound and southbound databases. Only the `coverage/show` and
`stopwatch/show` entries selected by OVN-Kubernetes, along with separately
collected database, build, status, connection, and configuration metrics, are exported.

Coverage values are represented as Prometheus gauges, although the underlying
OVN events are cumulative. Stopwatch duration statistics are gauges expressed
in seconds; only `total_samples` is a count. A failed collection can leave the
previous value in place. An event that has never occurred is reported as zero.

## OVN Database Metrics

The `db_name` label is the database schema name, such as `OVN_Northbound` or
`OVN_Southbound`. The database size family is registered only when the
database files can be found.

All metrics in this section are rooted at `ovn_db_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `nb_schema_version`, `sb_schema_version` | ovsdb-server and OVN schema version information. The value is always `1`. |
| `jsonrpc_server_sessions` | Gauge | `db_name` | Active JSON-RPC server sessions. |
| `ovsdb_monitors` | Gauge | `db_name` | OVSDB monitors running on the server. |
| `db_size_bytes` | Gauge | `db_name` | Size of the database file. |
| `e2e_timestamp` | Gauge | `db_name` | Unix timestamp observed in each database for the end-to-end freshness check. |

## OVN Controller Metrics

All metrics in this section are rooted at `ovn_controller_`.

### Build, Status, and Configuration

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `ovs_lib_version` | OVN and Open vSwitch library version information. The value is always `1`. |
| `southbound_database_connected` | Gauge | None | Whether ovn-controller is connected to the southbound database: `1` for connected and `0` otherwise. |
| `remote_probe_interval_seconds` | Gauge | None | Inactivity probe interval for the southbound database connection. |
| `openflow_probe_interval_seconds` | Gauge | None | Inactivity probe interval for the integration bridge OpenFlow connection. |
| `monitor_all` | Gauge | None | Whether ovn-controller monitors all southbound records: `1` for true and `0` for false. |
| `encap_ip` | Gauge | `ipaddress` | Configured encapsulation IP. The active label set has value `1`. |
| `encap_type` | Gauge | `type` | Configured encapsulation type. The active label set has value `1`. |
| `sb_connection_method` | Gauge | `connection_method` | Configured southbound connection method. The active label set has value `1`. |
| `bridge_mappings` | Gauge | `mapping` | Configured physical-network-to-bridge mappings. The active label set has value `1`. |
| `integration_bridge_openflow_total` | Gauge | None | Point-in-time number of OpenFlow flows on the integration bridge. Despite the `_total` suffix, this is not a counter. |
| `integration_bridge_patch_ports` | Gauge | None | Patch ports connecting the integration bridge to physical or local bridges. |
| `integration_bridge_geneve_ports` | Gauge | None | Geneve ports on the integration bridge. |

The `ovnkube_controller_udn_nbdb_programmed_duration_seconds` histogram is
documented with the other [ovnkube-controller metrics](ovnkube-node.md#scale-metrics).

### Coverage Metrics

| Name | Type | Description |
| --- | --- | --- |
| `lflow_run` | Gauge | Logical-flow translation runs. |
| `rconn_sent` | Gauge | Messages sent on reliable OpenFlow connections. |
| `rconn_queued` | Gauge | Messages queued on reliable OpenFlow connections. |
| `rconn_discarded` | Gauge | Queued messages discarded during reconnection. |
| `rconn_overflow` | Gauge | Messages dropped because the reliable connection queue overflowed. |
| `vconn_open` | Gauge | Attempts to connect to an OpenFlow device. |
| `vconn_sent` | Gauge | Messages sent to OpenFlow devices. |
| `vconn_received` | Gauge | Messages received from OpenFlow devices. |
| `stream_open` | Gauge | Attempts to connect to a remote peer. |
| `txn_success` | Gauge | Successful OVSDB transactions. |
| `txn_error` | Gauge | OVSDB transaction errors. |
| `txn_uncommitted` | Gauge | Uncommitted OVSDB transactions. |
| `txn_unchanged` | Gauge | OVSDB transactions that made no database change. |
| `txn_incomplete` | Gauge | Incomplete OVSDB transactions that required retry. |
| `txn_aborted` | Gauge | Aborted OVSDB transactions. |
| `txn_try_again` | Gauge | OVSDB transactions that failed and requested retry. |
| `netlink_sent` | Gauge | Netlink messages sent to the kernel. |
| `netlink_received` | Gauge | Netlink messages received from the kernel. |
| `netlink_recv_jumbo` | Gauge | Netlink messages larger than the allocated receive buffer. |
| `netlink_overflow` | Gauge | Netlink messages dropped because the daemon buffer overflowed. |
| `packet_in` | Gauge | Packet-ins handled from ovs-vswitchd. |
| `packet_in_drop` | Gauge | Packet-ins dropped because of resource constraints. This aggregates selected pinctrl drop events. |

### Stopwatch Metrics

For every base name below, OVN-Kubernetes exports these six gauge families:

- `<base>_total_samples`
- `<base>_maximum`
- `<base>_minimum`
- `<base>_95th_percentile`
- `<base>_short_term_avg`
- `<base>_long_term_avg`

`total_samples` is a sample count. The other five values are durations in
seconds.

| Base name | Measured operation |
| --- | --- |
| `bfd_run` | BFD processing loop |
| `flow_installation` | Flow installation |
| `if_status_mgr_run` | Interface status manager run loop |
| `if_status_mgr_update` | Interface status manager update |
| `flow_generation` | Flow generation |
| `pinctrl_run` | Pin controller run loop |
| `ofctrl_seqno_run` | OpenFlow controller sequence-number run loop |
| `patch_run` | Patch-port processing loop |
| `ct_zone_commit` | Connection-tracking zone commit |

## OVN Northd Metrics

All metrics in this section are rooted at `ovn_northd_`.

### Build and Status

Status and connection metrics use `-1` when the value cannot be collected or
parsed.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `ovs_lib_version` | OVN and Open vSwitch library version information. The value is always `1`. |
| `status` | Gauge | None | Instance status: `0` standby, `1` active, `2` paused, and `-1` unknown. |
| `nb_connection_status` | Gauge | None | Northbound connection: `0` disconnected, `1` connected, and `-1` unknown. |
| `sb_connection_status` | Gauge | None | Southbound connection: `0` disconnected, `1` connected, and `-1` unknown. |
| `northd_probe_interval` | Gauge | None | Maximum idle time, in milliseconds, before an inactivity probe is sent to the OVN databases. |

### Coverage Metrics

| Name | Type | Description |
| --- | --- | --- |
| `pstream_open` | Gauge | Passive connections opened for remote peers. |
| `stream_open` | Gauge | Attempts to connect to a remote peer. |
| `txn_success` | Gauge | Successful OVSDB transactions. |
| `txn_error` | Gauge | OVSDB transaction errors. |
| `txn_uncommitted` | Gauge | Uncommitted OVSDB transactions. |
| `txn_unchanged` | Gauge | OVSDB transactions that made no database change. |
| `txn_incomplete` | Gauge | Incomplete OVSDB transactions that required retry. |
| `txn_aborted` | Gauge | Aborted OVSDB transactions. |
| `txn_try_again` | Gauge | OVSDB transactions that failed and requested retry. |

### Stopwatch Metrics

For every base name below, OVN-Kubernetes exports these six gauge families:

- `<base>_total_samples`
- `<base>_maximum`
- `<base>_minimum`
- `<base>_95th_percentile`
- `<base>_short_term_avg`
- `<base>_long_term_avg`

`total_samples` is a sample count. The other five values are durations in
seconds.

| Base name | Measured operation |
| --- | --- |
| `ovnnb_db_run` | Northbound database processing |
| `build_flows_ctx` | Flow-build context processing |
| `ovn_northd_loop` | Main ovn-northd loop |
| `build_lflows` | Logical-flow construction |
| `lflows_lbs` | Load-balancer logical-flow processing |
| `clear_lflows_ctx` | Logical-flow context cleanup |
| `lflows_ports` | Port logical-flow processing |
| `lflows_dp_groups` | Datapath-group logical-flow processing |
| `lflows_datapaths` | Datapath logical-flow processing |
| `lflows_igmp` | IGMP logical-flow processing |
| `ovnsb_db_run` | Southbound database processing |
