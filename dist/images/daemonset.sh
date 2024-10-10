#!/bin/bash
#set -x

#Always exit on errors
set -e

install_jinjanator_renderer() {
  # ensure jinjanator renderer installed
  pip install wheel --user
  pip freeze | grep jinjanator || pip install "jinjanator[yaml]" --user
  export PATH=~/.local/bin:$PATH
}

# The script renders j2 templates into yaml files in ../yaml/

# ensure jinjanator renderer installed
if ! command -v jinjanate >/dev/null 2>&1 ; then
  if ! command -v pip >/dev/null 2>&1 ; then
    echo "Dependency not met: 'jinjanator' not installed and cannot install with 'pip'"
    exit 1
  fi
  echo "'jinjanate' not found, installing with 'pip'"
  install_jinjanator_renderer
fi

OVN_OUTPUT_DIR=""
OVN_IMAGE=""
OVN_IMAGE_PULL_POLICY=""
OVN_NET_CIDR=""
OVN_SVC_CIDR=""
OVN_K8S_APISERVER=""
OVN_GATEWAY_MODE=""
OVN_GATEWAY_OPTS=""
OVN_DUMMY_GATEWAY_BRIDGE=""
OVN_DB_REPLICAS=""
OVN_MTU=""
OVN_SSL_ENABLE=""
OVN_UNPRIVILEGED_MODE=""
MASTER_LOGLEVEL=""
NODE_LOGLEVEL=""
DBCHECKER_LOGLEVEL=""
OVN_LOGLEVEL_NORTHD=""
OVN_LOGLEVEL_NB=""
OVN_LOGLEVEL_SB=""
OVN_LOGLEVEL_CONTROLLER=""
OVN_LOGLEVEL_NBCTLD=""
OVNKUBE_LOGFILE_MAXSIZE=""
OVNKUBE_LOGFILE_MAXBACKUPS=""
OVNKUBE_LOGFILE_MAXAGE=""
OVNKUBE_LIBOVSDB_CLIENT_LOGFILE=""
OVN_ACL_LOGGING_RATE_LIMIT=""
OVN_MASTER_COUNT=""
OVN_REMOTE_PROBE_INTERVAL=""
OVN_MONITOR_ALL=""
OVN_OFCTRL_WAIT_BEFORE_CLEAR=""
OVN_ENABLE_LFLOW_CACHE=""
OVN_LFLOW_CACHE_LIMIT=""
OVN_LFLOW_CACHE_LIMIT_KB=""
OVN_HYBRID_OVERLAY_ENABLE=""
OVN_DISABLE_SNAT_MULTIPLE_GWS=""
OVN_DISABLE_FORWARDING=""
OVN_DISABLE_PKT_MTU_CHECK=""
OVN_EMPTY_LB_EVENTS=""
OVN_MULTICAST_ENABLE=""
OVN_ADMIN_NETWORK_POLICY_ENABLE=""
OVN_EGRESSIP_ENABLE=
OVN_EGRESSIP_HEALTHCHECK_PORT=
OVN_EGRESSFIREWALL_ENABLE=
OVN_EGRESSQOS_ENABLE=
OVN_EGRESSSERVICE_ENABLE=
OVN_DISABLE_OVN_IFACE_ID_VER="false"
OVN_MULTI_NETWORK_ENABLE=
OVN_V4_JOIN_SUBNET=""
OVN_V6_JOIN_SUBNET=""
OVN_V4_MASQUERADE_SUBNET=""
OVN_V6_MASQUERADE_SUBNET=""
OVN_V4_TRANSIT_SWITCH_SUBNET=""
OVN_V6_TRANSIT_SWITCH_SUBNET=""
OVN_NETFLOW_TARGETS=""
OVN_SFLOW_TARGETS=""
OVN_IPFIX_TARGETS=""
OVN_IPFIX_SAMPLING=""
OVN_IPFIX_CACHE_MAX_FLOWS=""
OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=""
OVN_HOST_NETWORK_NAMESPACE=""
OVN_EX_GW_NETWORK_INTERFACE=""
OVNKUBE_NODE_MGMT_PORT_NETDEV=""
OVNKUBE_CONFIG_DURATION_ENABLE=
OVNKUBE_METRICS_SCALE_ENABLE=
OVN_STATELESS_NETPOL_ENABLE="false"
OVN_ENABLE_INTERCONNECT=
OVN_ENABLE_OVNKUBE_IDENTITY="true"
OVN_ENABLE_PERSISTENT_IPS=
OVN_ENABLE_SVC_TEMPLATE_SUPPORT="true"
OVN_NOHOSTSUBNET_LABEL=""
OVN_DISABLE_REQUESTEDCHASSIS="false"
# IN_UPGRADE is true only if called by upgrade-ovn.sh during the upgrade test,
# it will render only the parts in ovn-setup.yaml related to RBAC permissions.
IN_UPGRADE=
# northd-backoff-interval, in ms
OVN_NORTHD_BACKOFF_INTERVAL=

# Parse parameters given as arguments to this script.
while [ "$1" != "" ]; do
  PARAM=$(echo $1 | awk -F= '{print $1}')
  VALUE=$(echo $1 | cut -d= -f2-)
  case $PARAM in
  --output-directory)
    OVN_OUTPUT_DIR=$VALUE
    ;;
  --image)
    OVN_IMAGE=$VALUE
    ;;
  --ovnkube-image)
    OVNKUBE_IMAGE=$VALUE
    ;;
  --image-pull-policy)
    OVN_IMAGE_PULL_POLICY=$VALUE
    ;;
  --gateway-mode)
    OVN_GATEWAY_MODE=$VALUE
    ;;
  --gateway-options)
    OVN_GATEWAY_OPTS=$VALUE
    ;;
  --dummy-gateway-bridge)
    OVN_DUMMY_GATEWAY_BRIDGE=$VALUE
    ;;
  --enable-ipsec)
    ENABLE_IPSEC=$VALUE
    ;;
  --ovn-monitor-all)
    OVN_MONITOR_ALL=$VALUE
    ;;
  --ovn-ofctrl-wait-before-clear)
    OVN_OFCTRL_WAIT_BEFORE_CLEAR=$VALUE
    ;;
  --ovn-enable-lflow-cache)
    OVN_ENABLE_LFLOW_CACHE=$VALUE
    ;;
  --ovn-lflow-cache-limit)
    OVN_LFLOW_CACHE_LIMIT=$VALUE
    ;;
  --ovn-lflow-cache-limit-kb)
    OVN_LFLOW_CACHE_LIMIT_KB=$VALUE
    ;;
  --net-cidr)
    OVN_NET_CIDR=$VALUE
    ;;
  --svc-cidr)
    OVN_SVC_CIDR=$VALUE
    ;;
  --k8s-apiserver)
    OVN_K8S_APISERVER=$VALUE
    ;;
  --db-replicas)
    OVN_DB_REPLICAS=$VALUE
    ;;
  --mtu)
    OVN_MTU=$VALUE
    ;;
  --ovn-unprivileged-mode)
    OVN_UNPRIVILEGED_MODE=$VALUE
    ;;
  --master-loglevel)
    MASTER_LOGLEVEL=$VALUE
    ;;
  --node-loglevel)
    NODE_LOGLEVEL=$VALUE
    ;;
  --dbchecker-loglevel)
    DBCHECKER_LOGLEVEL=$VALUE
    ;;
  --ovn-loglevel-northd)
    OVN_LOGLEVEL_NORTHD=$VALUE
    ;;
  --ovn-loglevel-nb)
    OVN_LOGLEVEL_NB=$VALUE
    ;;
  --ovn-loglevel-sb)
    OVN_LOGLEVEL_SB=$VALUE
    ;;
  --ovn-loglevel-controller)
    OVN_LOGLEVEL_CONTROLLER=$VALUE
    ;;
  --ovnkube-logfile-maxsize)
    OVNKUBE_LOGFILE_MAXSIZE=$VALUE
    ;;
  --ovnkube-logfile-maxbackups)
    OVNKUBE_LOGFILE_MAXBACKUPS=$VALUE
    ;;
  --ovnkube-logfile-maxage)
    OVNKUBE_LOGFILE_MAXAGE=$VALUE
    ;;
  --ovnkube-libovsdb-client-logfile)
    OVNKUBE_LIBOVSDB_CLIENT_LOGFILE=$VALUE
    ;;
  --acl-logging-rate-limit)
    OVN_ACL_LOGGING_RATE_LIMIT=$VALUE
    ;;
  --ssl)
    OVN_SSL_ENABLE="yes"
    ;;
  --ovn_nb_raft_election_timer)
    OVN_NB_RAFT_ELECTION_TIMER=$VALUE
    ;;
  --ovn_sb_raft_election_timer)
    OVN_SB_RAFT_ELECTION_TIMER=$VALUE
    ;;
  --ovn-master-count)
    OVN_MASTER_COUNT=$VALUE
    ;;
  --ovn-nb-port)
    OVN_NB_PORT=$VALUE
    ;;
  --ovn-sb-port)
    OVN_SB_PORT=$VALUE
    ;;
  --ovn-nb-raft-port)
    OVN_NB_RAFT_PORT=$VALUE
    ;;
  --ovn-sb-raft-port)
    OVN_SB_RAFT_PORT=$VALUE
    ;;
  --hybrid-enabled)
    OVN_HYBRID_OVERLAY_ENABLE=$VALUE
    ;;
  --disable-snat-multiple-gws)
    OVN_DISABLE_SNAT_MULTIPLE_GWS=$VALUE
    ;;
  --disable-forwarding)
    OVN_DISABLE_FORWARDING=$VALUE
    ;;
  --ovn-encap-port)
    OVN_ENCAP_PORT=$VALUE
    ;;
  --disable-pkt-mtu-check)
    OVN_DISABLE_PKT_MTU_CHECK=$VALUE
    ;;
  --ovn-empty-lb-events)
    OVN_EMPTY_LB_EVENTS=$VALUE
    ;;
  --multicast-enabled)
    OVN_MULTICAST_ENABLE=$VALUE
    ;;
  --admin-network-policy-enable)
    OVN_ADMIN_NETWORK_POLICY_ENABLE=$VALUE
    ;;
  --egress-ip-enable)
    OVN_EGRESSIP_ENABLE=$VALUE
    ;;
  --egress-ip-healthcheck-port)
    OVN_EGRESSIP_HEALTHCHECK_PORT=$VALUE
    ;;
  --disabe-ovn-iface-id-ver)
    OVN_DISABLE_OVN_IFACE_ID_VER=$VALUE
    ;;
  --egress-firewall-enable)
    OVN_EGRESSFIREWALL_ENABLE=$VALUE
    ;;
  --egress-qos-enable)
    OVN_EGRESSQOS_ENABLE=$VALUE
    ;;
  --multi-network-enable)
    OVN_MULTI_NETWORK_ENABLE=$VALUE
    ;;
  --egress-service-enable)
    OVN_EGRESSSERVICE_ENABLE=$VALUE
    ;;
  --v4-join-subnet)
    OVN_V4_JOIN_SUBNET=$VALUE
    ;;
  --v6-join-subnet)
    OVN_V6_JOIN_SUBNET=$VALUE
    ;;
  --v4-masquerade-subnet)
    OVN_V4_MASQUERADE_SUBNET=$VALUE
    ;;
  --v6-masquerade-subnet)
    OVN_V6_MASQUERADE_SUBNET=$VALUE
    ;;
  --v4-transit-switch-subnet)
    OVN_V4_TRANSIT_SWITCH_SUBNET=$VALUE
    ;; 
  --v6-transit-switch-subnet)
    OVN_V6_TRANSIT_SWITCH_SUBNET=$VALUE
    ;; 
  --netflow-targets)
    OVN_NETFLOW_TARGETS=$VALUE
    ;;
  --sflow-targets)
    OVN_SFLOW_TARGETS=$VALUE
    ;;
  --ipfix-targets)
    OVN_IPFIX_TARGETS=$VALUE
    ;;
  --ipfix-sampling)
    OVN_IPFIX_SAMPLING=$VALUE
    ;;
  --ipfix-cache-max-flows)
    OVN_IPFIX_CACHE_MAX_FLOWS=$VALUE
    ;;
  --ipfix-cache-active-timeout)
    OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=$VALUE
    ;;
  --host-network-namespace)
    OVN_HOST_NETWORK_NAMESPACE=$VALUE
    ;;
  --ex-gw-network-interface)
    OVN_EX_GW_NETWORK_INTERFACE=$VALUE
    ;;
  --ovnkube-node-mgmt-port-netdev)
    OVNKUBE_NODE_MGMT_PORT_NETDEV=$VALUE
    ;;
  --ovnkube-node-mgmt-port-dp-resource-name)
    OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME=$VALUE
    ;;
  --ovnkube-config-duration-enable)
    OVNKUBE_CONFIG_DURATION_ENABLE=$VALUE
    ;;
  --ovnkube-metrics-scale-enable)
    OVNKUBE_METRICS_SCALE_ENABLE=$VALUE
    ;;
  --in-upgrade)
    IN_UPGRADE=true
    ;;
  --stateless-netpol-enable)
    OVN_STATELESS_NETPOL_ENABLE=$VALUE
    ;;
  --compact-mode)
    COMPACT_MODE=$VALUE
    ;;
  --enable-interconnect)
    OVN_ENABLE_INTERCONNECT=$VALUE
    ;;
  --enable-multi-external-gateway)
    OVN_ENABLE_MULTI_EXTERNAL_GATEWAY=$VALUE
    ;;
  --enable-ovnkube-identity)
    OVN_ENABLE_OVNKUBE_IDENTITY=$VALUE
    ;;
  --ovn-northd-backoff-interval)
    OVN_NORTHD_BACKOFF_INTERVAL=$VALUE
    ;;
  --enable-persistent-ips)
    OVN_ENABLE_PERSISTENT_IPS=$VALUE
    ;;
  --enable-svc-template-support)
    OVN_ENABLE_SVC_TEMPLATE_SUPPORT=$VALUE
    ;;
  --no-hostsubnet-label)
    OVN_NOHOSTSUBNET_LABEL=$VALUE
    ;;
  --ovn_disable_requestedchassis)
    OVN_DISABLE_REQUESTEDCHASSIS=$value
    ;;
  *)
    echo "WARNING: unknown parameter \"$PARAM\""
    exit 1
    ;;
  esac
  shift
done

# Create the daemonsets with the desired image
# They are expanded into daemonsets in the specified
# output directory.
if [ -z ${OVN_OUTPUT_DIR} ] ; then
  output_dir="../yaml"
else
  output_dir=${OVN_OUTPUT_DIR}
  if [ ! -d ${OVN_OUTPUT_DIR} ]; then
    mkdir $output_dir
  fi
fi
echo "output_dir: $output_dir"

image=${OVN_IMAGE:-"docker.io/ovnkube/ovn-daemonset:latest"}
echo "image: ${image}"

ovnkube_image=${OVNKUBE_IMAGE:-${image}}
echo "ovnkube_image: ${ovnkube_image}"

image_pull_policy=${OVN_IMAGE_PULL_POLICY:-"IfNotPresent"}
echo "imagePullPolicy: ${image_pull_policy}"

ovn_gateway_mode=${OVN_GATEWAY_MODE}
echo "ovn_gateway_mode: ${ovn_gateway_mode}"

ovn_gateway_opts=${OVN_GATEWAY_OPTS}
echo "ovn_gateway_opts: ${ovn_gateway_opts}"

ovn_dummy_gateway_bridge=${OVN_DUMMY_GATEWAY_BRIDGE}
echo "ovn_dummy_gateway_bridge: ${ovn_dummy_gateway_bridge}"

enable_ipsec=${ENABLE_IPSEC:-false}
echo "enable_ipsec: ${enable_ipsec}"

ovn_db_replicas=${OVN_DB_REPLICAS:-3}
echo "ovn_db_replicas: ${ovn_db_replicas}"
ovn_db_minAvailable=$(((${ovn_db_replicas} + 1) / 2))
echo "ovn_db_minAvailable: ${ovn_db_minAvailable}"
master_loglevel=${MASTER_LOGLEVEL:-"4"}
echo "master_loglevel: ${master_loglevel}"
node_loglevel=${NODE_LOGLEVEL:-"4"}
echo "node_loglevel: ${node_loglevel}"
db_checker_loglevel=${DBCHECKER_LOGLEVEL:-"4"}
echo "db_checker_loglevel: ${db_checker_loglevel}"
ovn_loglevel_northd=${OVN_LOGLEVEL_NORTHD:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_northd: ${ovn_loglevel_northd}"
ovn_loglevel_nb=${OVN_LOGLEVEL_NB:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_nb: ${ovn_loglevel_nb}"
ovn_loglevel_sb=${OVN_LOGLEVEL_SB:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_sb: ${ovn_loglevel_sb}"
ovn_loglevel_controller=${OVN_LOGLEVEL_CONTROLLER:-"-vconsole:info"}
echo "ovn_loglevel_controller: ${ovn_loglevel_controller}"
ovnkube_logfile_maxsize=${OVNKUBE_LOGFILE_MAXSIZE:-"100"}
echo "ovnkube_logfile_maxsize: ${ovnkube_logfile_maxsize}"
ovnkube_logfile_maxbackups=${OVNKUBE_LOGFILE_MAXBACKUPS:-"5"}
echo "ovnkube_logfile_maxbackups: ${ovnkube_logfile_maxbackups}"
ovnkube_logfile_maxage=${OVNKUBE_LOGFILE_MAXAGE:-"5"}
echo "ovnkube_logfile_maxage: ${ovnkube_logfile_maxage}"
ovnkube_libovsdb_client_logfile=${OVNKUBE_LIBOVSDB_CLIENT_LOGFILE}
echo "ovnkube_libovsdb_client_logfile: ${ovnkube_libovsdb_client_logfile}"
ovn_acl_logging_rate_limit=${OVN_ACL_LOGGING_RATE_LIMIT:-"20"}
echo "ovn_acl_logging_rate_limit: ${ovn_acl_logging_rate_limit}"
ovn_hybrid_overlay_enable=${OVN_HYBRID_OVERLAY_ENABLE}
echo "ovn_hybrid_overlay_enable: ${ovn_hybrid_overlay_enable}"
ovn_admin_network_policy_enable=${OVN_ADMIN_NETWORK_POLICY_ENABLE}
echo "ovn_admin_network_policy_enable: ${ovn_admin_network_policy_enable}"
ovn_egress_ip_enable=${OVN_EGRESSIP_ENABLE}
echo "ovn_egress_ip_enable: ${ovn_egress_ip_enable}"
ovn_egress_ip_healthcheck_port=${OVN_EGRESSIP_HEALTHCHECK_PORT}
echo "ovn_egress_ip_healthcheck_port: ${ovn_egress_ip_healthcheck_port}"
ovn_egress_firewall_enable=${OVN_EGRESSFIREWALL_ENABLE}
echo "ovn_egress_firewall_enable: ${ovn_egress_firewall_enable}"
ovn_egress_qos_enable=${OVN_EGRESSQOS_ENABLE}
echo "ovn_egress_qos_enable: ${ovn_egress_qos_enable}"
ovn_egress_service_enable=${OVN_EGRESSSERVICE_ENABLE}
echo "ovn_egress_service_enable: ${ovn_egress_service_enable}"
ovn_disable_ovn_iface_id_ver=${OVN_DISABLE_OVN_IFACE_ID_VER}
echo "ovn_disable_ovn_iface_id_ver: ${ovn_disable_ovn_iface_id_ver}"
ovn_multi_network_enable=${OVN_MULTI_NETWORK_ENABLE}
echo "ovn_multi_network_enable: ${ovn_multi_network_enable}"
ovn_hybrid_overlay_net_cidr=${OVN_HYBRID_OVERLAY_NET_CIDR}
echo "ovn_hybrid_overlay_net_cidr: ${ovn_hybrid_overlay_net_cidr}"
ovn_disable_snat_multiple_gws=${OVN_DISABLE_SNAT_MULTIPLE_GWS}
echo "ovn_disable_snat_multiple_gws: ${ovn_disable_snat_multiple_gws}"
ovn_disable_forwarding=${OVN_DISABLE_FORWARDING}
echo "ovn_disable_forwarding: ${ovn_disable_forwarding}"
ovn_encap_port=${OVN_ENCAP_PORT}
echo "ovn_encap_port: ${ovn_encap_port}"
ovn_disable_pkt_mtu_check=${OVN_DISABLE_PKT_MTU_CHECK}
echo "ovn_disable_pkt_mtu_check: ${ovn_disable_pkt_mtu_check}"
ovn_empty_lb_events=${OVN_EMPTY_LB_EVENTS}
echo "ovn_empty_lb_events: ${ovn_empty_lb_events}"
ovn_ssl_en=${OVN_SSL_ENABLE:-"no"}
echo "ovn_ssl_enable: ${ovn_ssl_en}"
ovn_unprivileged_mode=${OVN_UNPRIVILEGED_MODE:-"no"}
echo "ovn_unprivileged_mode: ${ovn_unprivileged_mode}"
ovn_nb_raft_election_timer=${OVN_NB_RAFT_ELECTION_TIMER:-1000}
echo "ovn_nb_raft_election_timer: ${ovn_nb_raft_election_timer}"
ovn_sb_raft_election_timer=${OVN_SB_RAFT_ELECTION_TIMER:-1000}
echo "ovn_sb_raft_election_timer: ${ovn_sb_raft_election_timer}"
ovn_master_count=${OVN_MASTER_COUNT:-"1"}
echo "ovn_master_count: ${ovn_master_count}"
ovn_remote_probe_interval=${OVN_REMOTE_PROBE_INTERVAL:-"100000"}
echo "ovn_remote_probe_interval: ${ovn_remote_probe_interval}"
ovn_monitor_all=${OVN_MONITOR_ALL}
echo "ovn_monitor_all: ${ovn_monitor_all}"
ovn_ofctrl_wait_before_clear=${OVN_OFCTRL_WAIT_BEFORE_CLEAR}
echo "ovn_ofctrl_wait_before_clear: ${ovn_ofctrl_wait_before_clear}"
ovn_enable_lflow_cache=${OVN_ENABLE_LFLOW_CACHE}
echo "ovn_enable_lflow_cache: ${ovn_enable_lflow_cache}"
ovn_lflow_cache_limit=${OVN_LFLOW_CACHE_LIMIT}
echo "ovn_lflow_cache_limit: ${ovn_lflow_cache_limit}"
ovn_lflow_cache_limit_kb=${OVN_LFLOW_CACHE_LIMIT_KB}
echo "ovn_lflow_cache_limit_kb: ${ovn_lflow_cache_limit_kb}"
ovn_nb_port=${OVN_NB_PORT:-6641}
echo "ovn_nb_port: ${ovn_nb_port}"
ovn_sb_port=${OVN_SB_PORT:-6642}
echo "ovn_sb_port: ${ovn_sb_port}"
ovn_nb_raft_port=${OVN_NB_RAFT_PORT:-6643}
echo "ovn_nb_raft_port: ${ovn_nb_raft_port}"
ovn_sb_raft_port=${OVN_SB_RAFT_PORT:-6644}
echo "ovn_sb_raft_port: ${ovn_sb_raft_port}"
ovn_multicast_enable=${OVN_MULTICAST_ENABLE}
echo "ovn_multicast_enable: ${ovn_multicast_enable}"
ovn_v4_join_subnet=${OVN_V4_JOIN_SUBNET}
echo "ovn_v4_join_subnet: ${ovn_v4_join_subnet}"
ovn_v6_join_subnet=${OVN_V6_JOIN_SUBNET}
echo "ovn_v6_join_subnet: ${ovn_v6_join_subnet}"
ovn_v4_masquerade_subnet=${OVN_V4_MASQUERADE_SUBNET}
echo "ovn_v4_masquerade_subnet: ${ovn_v4_masquerade_subnet}"
ovn_v6_masquerade_subnet=${OVN_V6_MASQUERADE_SUBNET}
echo "ovn_v6_masquerade_subnet: ${ovn_v6_masquerade_subnet}"
ovn_v4_transit_switch_subnet=${OVN_V4_TRANSIT_SWITCH_SUBNET}
echo "ovn_v4_transit_switch_subnet: ${ovn_v4_transit_switch_subnet}"
ovn_v6_transit_switch_subnet=${OVN_V6_TRANSIT_SWITCH_SUBNET}
echo "ovn_v6_transit_switch_subnet: ${ovn_v6_transit_switch_subnet}"
ovn_netflow_targets=${OVN_NETFLOW_TARGETS}
echo "ovn_netflow_targets: ${ovn_netflow_targets}"
ovn_sflow_targets=${OVN_SFLOW_TARGETS}
echo "ovn_sflow_targets: ${ovn_sflow_targets}"
ovn_ipfix_targets=${OVN_IPFIX_TARGETS}
echo "ovn_ipfix_targets: ${ovn_ipfix_targets}"
ovn_ipfix_sampling=${OVN_IPFIX_SAMPLING}
echo "ovn_ipfix_sampling: ${ovn_ipfix_sampling}"
ovn_ipfix_cache_max_flows=${OVN_IPFIX_CACHE_MAX_FLOWS}
echo "ovn_ipfix_cache_max_flows: ${ovn_ipfix_cache_max_flows}"
ovn_ipfix_cache_active_timeout=${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT}
echo "ovn_ipfix_cache_active_timeout: ${ovn_ipfix_cache_active_timeout}"
ovn_ex_gw_networking_interface=${OVN_EX_GW_NETWORK_INTERFACE}
echo "ovn_ex_gw_networking_interface: ${ovn_ex_gw_networking_interface}"
ovnkube_node_mgmt_port_netdev=${OVNKUBE_NODE_MGMT_PORT_NETDEV}
echo "ovnkube_node_mgmt_port_netdev: ${ovnkube_node_mgmt_port_netdev}"
ovnkube_config_duration_enable=${OVNKUBE_CONFIG_DURATION_ENABLE}
echo "ovnkube_config_duration_enable: ${ovnkube_config_duration_enable}"
ovnkube_metrics_scale_enable=${OVNKUBE_METRICS_SCALE_ENABLE}
echo "ovnkube_metrics_scale_enable: ${ovnkube_metrics_scale_enable}"
ovn_stateless_netpol_enable=${OVN_STATELESS_NETPOL_ENABLE}
echo "ovn_stateless_netpol_enable: ${ovn_stateless_netpol_enable}"
ovnkube_compact_mode_enable=${COMPACT_MODE:-"false"}
echo "ovnkube_compact_mode_enable: ${ovnkube_compact_mode_enable}"
ovn_enable_interconnect=${OVN_ENABLE_INTERCONNECT}
echo "ovn_enable_interconnect: ${ovn_enable_interconnect}"
ovn_enable_multi_external_gateway=${OVN_ENABLE_MULTI_EXTERNAL_GATEWAY}
echo "ovn_enable_multi_external_gateway: ${ovn_enable_multi_external_gateway}"

ovn_enable_ovnkube_identity=${OVN_ENABLE_OVNKUBE_IDENTITY}
echo "ovn_enable_ovnkube_identity: ${ovn_enable_ovnkube_identity}"

ovn_northd_backoff_interval=${OVN_NORTHD_BACKOFF_INTERVAL}
echo "ovn_northd_backoff_interval: ${ovn_northd_backoff_interval}"

ovn_enable_persistent_ips=${OVN_ENABLE_PERSISTENT_IPS}
echo "ovn_enable_persistent_ips: ${ovn_enable_persistent_ips}"

ovn_enable_svc_template_support=${OVN_ENABLE_SVC_TEMPLATE_SUPPORT}
echo "ovn_enable_svc_template_support: ${ovn_enable_svc_template_support}"

ovn_nohostsubnet_label=${OVN_NOHOSTSUBNET_LABEL}
echo "ovn_nohostsubnet_label: ${ovn_nohostsubnet_label}"

ovn_disable_requestedchassis=${OVN_DISABLE_REQUESTEDCHASSIS}
echo "ovn_disable_requestedchassis: ${ovn_disable_requestedchassis}"

ovn_image=${ovnkube_image} \
  ovnkube_compact_mode_enable=${ovnkube_compact_mode_enable} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovn_dummy_gateway_bridge=${ovn_dummy_gateway_bridge} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_forwarding=${ovn_disable_forwarding} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_v4_masquerade_subnet=${ovn_v4_masquerade_subnet} \
  ovn_v6_masquerade_subnet=${ovn_v6_masquerade_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_remote_probe_interval=${ovn_remote_probe_interval} \
  ovn_monitor_all=${ovn_monitor_all} \
  ovn_ofctrl_wait_before_clear=${ovn_ofctrl_wait_before_clear} \
  ovn_enable_lflow_cache=${ovn_enable_lflow_cache} \
  ovn_lflow_cache_limit=${ovn_lflow_cache_limit} \
  ovn_lflow_cache_limit_kb=${ovn_lflow_cache_limit_kb} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovn_disable_ovn_iface_id_ver=${ovn_disable_ovn_iface_id_ver} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovnkube_app_name=ovnkube-node \
  jinjanate ../templates/ovnkube-node.yaml.j2 -o ${output_dir}/ovnkube-node.yaml

ovn_image=${ovnkube_image} \
  ovnkube_compact_mode_enable=${ovnkube_compact_mode_enable} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovn_dummy_gateway_bridge=${ovn_dummy_gateway_bridge} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_forwarding=${ovn_disable_forwarding} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_v4_masquerade_subnet=${ovn_v4_masquerade_subnet} \
  ovn_v6_masquerade_subnet=${ovn_v6_masquerade_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_remote_probe_interval=${ovn_remote_probe_interval} \
  ovn_monitor_all=${ovn_monitor_all} \
  ovn_ofctrl_wait_before_clear=${ovn_ofctrl_wait_before_clear} \
  ovn_enable_lflow_cache=${ovn_enable_lflow_cache} \
  ovn_lflow_cache_limit=${ovn_lflow_cache_limit} \
  ovn_lflow_cache_limit_kb=${ovn_lflow_cache_limit_kb} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovn_disable_ovn_iface_id_ver=${ovn_disable_ovn_iface_id_ver} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovnkube_app_name=ovnkube-node-dpu \
  jinjanate ../templates/ovnkube-node.yaml.j2 -o ${output_dir}/ovnkube-node-dpu.yaml

# ovnkube node for dpu-host daemonset
# TODO: we probably dont need all of these when running on dpu host
ovn_image=${image} \
  ovnkube_compact_mode_enable=${ovnkube_compact_mode_enable} \
  ovn_image_pull_policy=${image_pull_policy} \
  kind=${KIND} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovn_dummy_gateway_bridge=${ovn_dummy_gateway_bridge} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_forwarding=${ovn_disable_forwarding} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_v4_masquerade_subnet=${ovn_v4_masquerade_subnet} \
  ovn_v6_masquerade_subnet=${ovn_v6_masquerade_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovnkube_app_name=ovnkube-node-dpu-host \
  jinjanate ../templates/ovnkube-node.yaml.j2 -o ${output_dir}/ovnkube-node-dpu-host.yaml

ovn_image=${ovnkube_image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_loglevel_northd=${ovn_loglevel_northd} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovnkube_libovsdb_client_logfile=${ovnkube_libovsdb_client_logfile} \
  ovnkube_config_duration_enable=${ovnkube_config_duration_enable} \
  ovnkube_metrics_scale_enable=${ovnkube_metrics_scale_enable} \
  ovn_acl_logging_rate_limit=${ovn_acl_logging_rate_limit} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_forwarding=${ovn_disable_forwarding} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_empty_lb_events=${ovn_empty_lb_events} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_v4_masquerade_subnet=${ovn_v4_masquerade_subnet} \
  ovn_v6_masquerade_subnet=${ovn_v6_masquerade_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_egress_firewall_enable=${ovn_egress_firewall_enable} \
  ovn_egress_qos_enable=${ovn_egress_qos_enable} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_master_count=${ovn_master_count} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovn_dummy_gateway_bridge=${ovn_dummy_gateway_bridge} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovn_stateless_netpol_enable=${ovn_netpol_acl_enable} \
  ovnkube_compact_mode_enable=${ovnkube_compact_mode_enable} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovn_enable_persistent_ips=${ovn_enable_persistent_ips} \
  ovn_enable_svc_template_support=${ovn_enable_svc_template_support} \
  ovn_nohostsubnet_label=${ovn_nohostsubnet_label} \
  ovn_disable_requestedchassis=${ovn_disable_requestedchassis} \
  jinjanate ../templates/ovnkube-master.yaml.j2 -o ${output_dir}/ovnkube-master.yaml

ovn_image=${ovnkube_image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_loglevel_northd=${ovn_loglevel_northd} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovnkube_config_duration_enable=${ovnkube_config_duration_enable} \
  ovnkube_metrics_scale_enable=${ovnkube_metrics_scale_enable} \
  ovn_acl_logging_rate_limit=${ovn_acl_logging_rate_limit} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_empty_lb_events=${ovn_empty_lb_events} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_v4_masquerade_subnet=${ovn_v4_masquerade_subnet} \
  ovn_v6_masquerade_subnet=${ovn_v6_masquerade_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_egress_firewall_enable=${ovn_egress_firewall_enable} \
  ovn_egress_qos_enable=${ovn_egress_qos_enable} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_master_count=${ovn_master_count} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovn_v4_transit_switch_subnet=${ovn_v4_transit_switch_subnet} \
  ovn_v6_transit_switch_subnet=${ovn_v6_transit_switch_subnet} \
  ovn_enable_persistent_ips=${ovn_enable_persistent_ips} \
  jinjanate ../templates/ovnkube-control-plane.yaml.j2 -o ${output_dir}/ovnkube-control-plane.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_loglevel_nb=${ovn_loglevel_nb} \
  ovn_loglevel_sb=${ovn_loglevel_sb} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_nb_port=${ovn_nb_port} \
  ovn_sb_port=${ovn_sb_port} \
  enable_ipsec=${enable_ipsec} \
  ovn_northd_backoff_interval=${ovn_northd_backoff_interval} \
  jinjanate ../templates/ovnkube-db.yaml.j2 -o ${output_dir}/ovnkube-db.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_db_replicas=${ovn_db_replicas} \
  ovn_db_minAvailable=${ovn_db_minAvailable} \
  ovn_loglevel_nb=${ovn_loglevel_nb} ovn_loglevel_sb=${ovn_loglevel_sb} \
  ovn_dbchecker_loglevel=${db_checker_loglevel} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_nb_raft_election_timer=${ovn_nb_raft_election_timer} \
  ovn_sb_raft_election_timer=${ovn_sb_raft_election_timer} \
  ovn_nb_port=${ovn_nb_port} \
  ovn_sb_port=${ovn_sb_port} \
  ovn_nb_raft_port=${ovn_nb_raft_port} \
  ovn_sb_raft_port=${ovn_sb_raft_port} \
  enable_ipsec=${enable_ipsec} \
  ovn_northd_backoff_interval=${ovn_northd_backoff_interval} \
  jinjanate ../templates/ovnkube-db-raft.yaml.j2 -o ${output_dir}/ovnkube-db-raft.yaml

ovn_image=${ovnkube_image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovnkube_local_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovnkube_libovsdb_client_logfile=${ovnkube_libovsdb_client_logfile} \
  ovnkube_config_duration_enable=${ovnkube_config_duration_enable} \
  ovnkube_metrics_scale_enable=${ovnkube_metrics_scale_enable} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_forwarding=${ovn_disable_forwarding} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_egress_firewall_enable=${ovn_egress_firewall_enable} \
  ovn_egress_qos_enable=${ovn_egress_qos_enable} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_remote_probe_interval=${ovn_remote_probe_interval} \
  ovn_monitor_all=${ovn_monitor_all} \
  ovn_ofctrl_wait_before_clear=${ovn_ofctrl_wait_before_clear} \
  ovn_enable_lflow_cache=${ovn_enable_lflow_cache} \
  ovn_lflow_cache_limit=${ovn_lflow_cache_limit} \
  ovn_lflow_cache_limit_kb=${ovn_lflow_cache_limit_kb} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovn_disable_ovn_iface_id_ver=${ovn_disable_ovn_iface_id_ver} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_loglevel_northd=${ovn_loglevel_northd} \
  ovn_loglevel_nbctld=${ovn_loglevel_nbctld} \
  ovn_acl_logging_rate_limit=${ovn_acl_logging_rate_limit} \
  ovn_empty_lb_events=${ovn_empty_lb_events} \
  ovn_loglevel_nb=${ovn_loglevel_nb} ovn_loglevel_sb=${ovn_loglevel_sb} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovn_northd_backoff_interval=${ovn_northd_backoff_interval} \
  ovn_enable_persistent_ips=${ovn_enable_persistent_ips} \
  ovn_enable_svc_template_support=${ovn_enable_svc_template_support} \
  jinjanate ../templates/ovnkube-single-node-zone.yaml.j2 -o ${output_dir}/ovnkube-single-node-zone.yaml

ovn_image=${ovnkube_image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovnkube_local_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovnkube_libovsdb_client_logfile=${ovnkube_libovsdb_client_logfile} \
  ovnkube_config_duration_enable=${ovnkube_config_duration_enable} \
  ovnkube_metrics_scale_enable=${ovnkube_metrics_scale_enable} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_encap_port=${ovn_encap_port} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_admin_network_policy_enable=${ovn_admin_network_policy_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_ip_healthcheck_port=${ovn_egress_ip_healthcheck_port} \
  ovn_egress_service_enable=${ovn_egress_service_enable} \
  ovn_egress_firewall_enable=${ovn_egress_firewall_enable} \
  ovn_egress_qos_enable=${ovn_egress_qos_enable} \
  ovn_multi_network_enable=${ovn_multi_network_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_remote_probe_interval=${ovn_remote_probe_interval} \
  ovn_monitor_all=${ovn_monitor_all} \
  ovn_ofctrl_wait_before_clear=${ovn_ofctrl_wait_before_clear} \
  ovn_enable_lflow_cache=${ovn_enable_lflow_cache} \
  ovn_lflow_cache_limit=${ovn_lflow_cache_limit} \
  ovn_lflow_cache_limit_kb=${ovn_lflow_cache_limit_kb} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovn_disable_ovn_iface_id_ver=${ovn_disable_ovn_iface_id_ver} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_loglevel_northd=${ovn_loglevel_northd} \
  ovn_loglevel_nbctld=${ovn_loglevel_nbctld} \
  ovn_acl_logging_rate_limit=${ovn_acl_logging_rate_limit} \
  ovn_empty_lb_events=${ovn_empty_lb_events} \
  ovn_loglevel_nb=${ovn_loglevel_nb} ovn_loglevel_sb=${ovn_loglevel_sb} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  ovn_enable_multi_external_gateway=${ovn_enable_multi_external_gateway} \
  ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  ovn_northd_backoff_interval=${ovn_enable_backoff_interval} \
  ovn_enable_persistent_ips=${ovn_enable_persistent_ips} \
  ovn_enable_svc_template_support=${ovn_enable_svc_template_support} \
  jinjanate ../templates/ovnkube-zone-controller.yaml.j2 -o ${output_dir}/ovnkube-zone-controller.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  jinjanate ../templates/ovs-node.yaml.j2 -o ${output_dir}/ovs-node.yaml

ovnkube_certs_dir="/tmp/ovnkube-certs"
ovnkube_webhook_name="ovnkube-webhook"
mkdir -p ${ovnkube_certs_dir}
path_prefix="${ovnkube_certs_dir}/${ovnkube_webhook_name}"

if [ "${ovn_enable_ovnkube_identity}" = "true" ]; then
  # Create self signed CA and webhook cert
  # NOTE: The CA and certificate are not renewed after they expire, this should only be used in development environments
  openssl req -x509 -newkey rsa:4096 -nodes -keyout "${path_prefix}-ca.key" -out "${path_prefix}-ca.crt" -days 400 -subj "/CN=self-signed-ca"
  openssl req -newkey rsa:4096 -nodes -keyout "${path_prefix}.key" -out "${path_prefix}.csr" -subj "/CN=localhost"
  openssl x509 -req -in "${path_prefix}.csr" -CA "${path_prefix}-ca.crt" -CAkey "${path_prefix}-ca.key" -extfile <(printf "subjectAltName=DNS:localhost") -CAcreateserial -out "${path_prefix}.crt" -days 365
fi

ovn_image=${ovnkube_image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_master_count=${ovn_master_count} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_enable_interconnect=${ovn_enable_interconnect} \
  webhook_ca_bundle=$(cat "${path_prefix}-ca.crt" | base64 -w0) \
  webhook_key=$(cat "${path_prefix}.key" | base64 -w0) \
  webhook_cert=$(cat "${path_prefix}.crt" | base64 -w0) \
  ovn_enable_multi_node_zone=${ovn_enable_multi_node_zone} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  jinjanate ../templates/ovnkube-identity.yaml.j2 -o ${output_dir}/ovnkube-identity.yaml

if ${enable_ipsec}; then
  ovn_image=${image} \
    jinjanate ../templates/ovn-ipsec.yaml.j2 -o ${output_dir}/ovn-ipsec.yaml
fi

# ovn-setup.yaml
net_cidr=${OVN_NET_CIDR:-"10.128.0.0/14/23"}
svc_cidr=${OVN_SVC_CIDR:-"172.30.0.0/16"}
k8s_apiserver=${OVN_K8S_APISERVER:-"10.0.2.16:6443"}
mtu=${OVN_MTU:-1400}
host_network_namespace=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}
in_upgrade=${IN_UPGRADE:-false}
echo "net_cidr: ${net_cidr}"
echo "svc_cidr: ${svc_cidr}"
echo "k8s_apiserver: ${k8s_apiserver}"
echo "mtu: ${mtu}"
echo "host_network_namespace: ${host_network_namespace}"
echo "in_upgrade: ${in_upgrade}"

net_cidr=${net_cidr} svc_cidr=${svc_cidr} \
  mtu_value=${mtu} k8s_apiserver=${k8s_apiserver} \
  host_network_namespace=${host_network_namespace} \
  in_upgrade=${in_upgrade} \
  jinjanate ../templates/ovn-setup.yaml.j2 -o ${output_dir}/ovn-setup.yaml

ovn_enable_interconnect=${ovn_enable_interconnect} \
ovn_enable_ovnkube_identity=${ovn_enable_ovnkube_identity} \
  jinjanate ../templates/rbac-ovnkube-node.yaml.j2 -o ${output_dir}/rbac-ovnkube-node.yaml

cp ../templates/rbac-ovnkube-identity.yaml.j2 ${output_dir}/rbac-ovnkube-identity.yaml
cp ../templates/rbac-ovnkube-master.yaml.j2 ${output_dir}/rbac-ovnkube-master.yaml
cp ../templates/rbac-ovnkube-db.yaml.j2 ${output_dir}/rbac-ovnkube-db.yaml
cp ../templates/rbac-ovnkube-cluster-manager.yaml.j2 ${output_dir}/rbac-ovnkube-cluster-manager.yaml
cp ../templates/ovnkube-monitor.yaml.j2 ${output_dir}/ovnkube-monitor.yaml
cp ../templates/k8s.ovn.org_egressfirewalls.yaml.j2 ${output_dir}/k8s.ovn.org_egressfirewalls.yaml
cp ../templates/k8s.ovn.org_egressips.yaml.j2 ${output_dir}/k8s.ovn.org_egressips.yaml
cp ../templates/k8s.ovn.org_egressqoses.yaml.j2 ${output_dir}/k8s.ovn.org_egressqoses.yaml
cp ../templates/k8s.ovn.org_egressservices.yaml.j2 ${output_dir}/k8s.ovn.org_egressservices.yaml
cp ../templates/k8s.ovn.org_adminpolicybasedexternalroutes.yaml.j2 ${output_dir}/k8s.ovn.org_adminpolicybasedexternalroutes.yaml

exit 0
