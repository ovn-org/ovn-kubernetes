#!/bin/bash
#set -x

#Always exit on errors
set -e

install_j2_renderer() {
  # ensure j2 renderer installed
  pip install wheel --user
  pip freeze | grep j2cli || pip install j2cli[yaml] --user
  export PATH=~/.local/bin:$PATH
}

# The script renders j2 templates into yaml files in ../yaml/

# ensure j2 renderer installed
if ! command -v j2 >/dev/null 2>&1 ; then 
  if ! command -v pip >/dev/null 2>&1 ; then 
    echo "Dependency not met: 'j2' not installed and cannot install with 'pip'"
    exit 1
  fi
  echo "'j2' not found, installing with 'pip'"
  install_j2_renderer
fi 

OVN_OUTPUT_DIR=""
OVN_IMAGE=""
OVN_IMAGE_PULL_POLICY=""
OVN_NET_CIDR=""
OVN_SVC_DIDR=""
OVN_K8S_APISERVER=""
OVN_GATEWAY_MODE=""
OVN_GATEWAY_OPTS=""
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
OVN_DISABLE_PKT_MTU_CHECK=""
OVN_EMPTY_LB_EVENTS=""
OVN_MULTICAST_ENABLE=""
OVN_EGRESSIP_ENABLE=
OVN_EGRESSFIREWALL_ENABLE=
OVN_EGRESSQOS_ENABLE=
OVN_DISABLE_OVN_IFACE_ID_VER="false"
OVN_V4_JOIN_SUBNET=""
OVN_V6_JOIN_SUBNET=""
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
# IN_UPGRADE is true only if called by upgrade-ovn.sh during the upgrade test,
# it will render only the parts in ovn-setup.yaml related to RBAC permissions.
IN_UPGRADE=

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
  --image-pull-policy)
    OVN_IMAGE_PULL_POLICY=$VALUE
    ;;
  --gateway-mode)
    OVN_GATEWAY_MODE=$VALUE
    ;;
  --gateway-options)
    OVN_GATEWAY_OPTS=$VALUE
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
  --disable-pkt-mtu-check)
    OVN_DISABLE_PKT_MTU_CHECK=$VALUE
    ;;
  --ovn-empty-lb-events)
    OVN_EMPTY_LB_EVENTS=$VALUE
    ;;
  --multicast-enabled)
    OVN_MULTICAST_ENABLE=$VALUE
    ;;
  --egress-ip-enable)
    OVN_EGRESSIP_ENABLE=$VALUE
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
  --v4-join-subnet)
    OVN_V4_JOIN_SUBNET=$VALUE
    ;;
  --v6-join-subnet)
    OVN_V6_JOIN_SUBNET=$VALUE
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
  --ovnkube-config-duration-enable)
    OVNKUBE_CONFIG_DURATION_ENABLE=$VALUE
    ;;
  --in-upgrade)
    IN_UPGRADE=true
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

image_pull_policy=${OVN_IMAGE_PULL_POLICY:-"IfNotPresent"}
echo "imagePullPolicy: ${image_pull_policy}"

ovn_gateway_mode=${OVN_GATEWAY_MODE}
echo "ovn_gateway_mode: ${ovn_gateway_mode}"

ovn_gateway_opts=${OVN_GATEWAY_OPTS}
echo "ovn_gateway_opts: ${ovn_gateway_opts}"

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
ovn_acl_logging_rate_limit=${OVN_ACL_LOGGING_RATE_LIMIT:-"20"}
echo "ovn_acl_logging_rate_limit: ${ovn_acl_logging_rate_limit}"
ovn_hybrid_overlay_enable=${OVN_HYBRID_OVERLAY_ENABLE}
echo "ovn_hybrid_overlay_enable: ${ovn_hybrid_overlay_enable}"
ovn_egress_ip_enable=${OVN_EGRESSIP_ENABLE}
echo "ovn_egress_ip_enable: ${ovn_egress_ip_enable}"
ovn_egress_firewall_enable=${OVN_EGRESSFIREWALL_ENABLE}
echo "ovn_egress_firewall_enable: ${ovn_egress_firewall_enable}"
ovn_egress_qos_enable=${OVN_EGRESSQOS_ENABLE}
echo "ovn_egress_qos_enable: ${ovn_egress_qos_enable}"
ovn_disable_ovn_iface_id_ver=${OVN_DISABLE_OVN_IFACE_ID_VER}
echo "ovn_disable_ovn_iface_id_ver: ${ovn_disable_ovn_iface_id_ver}"
ovn_hybrid_overlay_net_cidr=${OVN_HYBRID_OVERLAY_NET_CIDR}
echo "ovn_hybrid_overlay_net_cidr: ${ovn_hybrid_overlay_net_cidr}"
ovn_disable_snat_multiple_gws=${OVN_DISABLE_SNAT_MULTIPLE_GWS}
echo "ovn_disable_snat_multiple_gws: ${ovn_disable_snat_multiple_gws}"
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

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
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
  ovnkube_app_name=ovnkube-node \
  j2 ../templates/ovnkube-node.yaml.j2 -o ${output_dir}/ovnkube-node.yaml

# ovnkube node for dpu-host daemonset
# TODO: we probably dont need all of these when running on dpu host
ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  kind=${KIND} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_gateway_opts=${ovn_gateway_opts} \
  ovnkube_node_loglevel=${node_loglevel} \
  ovn_loglevel_controller=${ovn_loglevel_controller} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_netflow_targets=${ovn_netflow_targets} \
  ovn_sflow_targets=${ovn_sflow_targets} \
  ovn_ipfix_targets=${ovn_ipfix_targets} \
  ovn_ipfix_sampling=${ovn_ipfix_sampling} \
  ovn_ipfix_cache_max_flows=${ovn_ipfix_cache_max_flows} \
  ovn_ipfix_cache_active_timeout=${ovn_ipfix_cache_active_timeout} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  ovnkube_node_mgmt_port_netdev=${ovnkube_node_mgmt_port_netdev} \
  ovnkube_app_name=ovnkube-node-dpu-host \
  j2 ../templates/ovnkube-node.yaml.j2 -o ${output_dir}/ovnkube-node-dpu-host.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovnkube_master_loglevel=${master_loglevel} \
  ovn_loglevel_northd=${ovn_loglevel_northd} \
  ovnkube_logfile_maxsize=${ovnkube_logfile_maxsize} \
  ovnkube_logfile_maxbackups=${ovnkube_logfile_maxbackups} \
  ovnkube_logfile_maxage=${ovnkube_logfile_maxage} \
  ovnkube_config_duration_enable=${ovnkube_config_duration_enable} \
  ovn_acl_logging_rate_limit=${ovn_acl_logging_rate_limit} \
  ovn_hybrid_overlay_net_cidr=${ovn_hybrid_overlay_net_cidr} \
  ovn_hybrid_overlay_enable=${ovn_hybrid_overlay_enable} \
  ovn_disable_snat_multiple_gws=${ovn_disable_snat_multiple_gws} \
  ovn_disable_pkt_mtu_check=${ovn_disable_pkt_mtu_check} \
  ovn_empty_lb_events=${ovn_empty_lb_events} \
  ovn_v4_join_subnet=${ovn_v4_join_subnet} \
  ovn_v6_join_subnet=${ovn_v6_join_subnet} \
  ovn_multicast_enable=${ovn_multicast_enable} \
  ovn_egress_ip_enable=${ovn_egress_ip_enable} \
  ovn_egress_firewall_enable=${ovn_egress_firewall_enable} \
  ovn_egress_qos_enable=${ovn_egress_qos_enable} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_master_count=${ovn_master_count} \
  ovn_gateway_mode=${ovn_gateway_mode} \
  ovn_ex_gw_networking_interface=${ovn_ex_gw_networking_interface} \
  j2 ../templates/ovnkube-master.yaml.j2 -o ${output_dir}/ovnkube-master.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_loglevel_nb=${ovn_loglevel_nb} \
  ovn_loglevel_sb=${ovn_loglevel_sb} \
  ovn_ssl_en=${ovn_ssl_en} \
  ovn_nb_port=${ovn_nb_port} \
  ovn_sb_port=${ovn_sb_port} \
  j2 ../templates/ovnkube-db.yaml.j2 -o ${output_dir}/ovnkube-db.yaml

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
  j2 ../templates/ovnkube-db-raft.yaml.j2 -o ${output_dir}/ovnkube-db-raft.yaml

ovn_image=${image} \
  ovn_image_pull_policy=${image_pull_policy} \
  ovn_unprivileged_mode=${ovn_unprivileged_mode} \
  j2 ../templates/ovs-node.yaml.j2 -o ${output_dir}/ovs-node.yaml

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
  j2 ../templates/ovn-setup.yaml.j2 -o ${output_dir}/ovn-setup.yaml

cp ../templates/ovnkube-monitor.yaml.j2 ${output_dir}/ovnkube-monitor.yaml
cp ../templates/k8s.ovn.org_egressfirewalls.yaml.j2 ${output_dir}/k8s.ovn.org_egressfirewalls.yaml
cp ../templates/k8s.ovn.org_egressips.yaml.j2 ${output_dir}/k8s.ovn.org_egressips.yaml
cp ../templates/k8s.ovn.org_egressqoses.yaml.j2 ${output_dir}/k8s.ovn.org_egressqoses.yaml

exit 0
