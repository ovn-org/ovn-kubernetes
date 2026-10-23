#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

#set -euo pipefail

# Enable verbose shell output if OVNKUBE_SH_VERBOSE is set to 'true'
if [[ "${OVNKUBE_SH_VERBOSE:-}" == "true" ]]; then
  set -x
fi

# This script is the entrypoint to the image.
# Supports version 1.4.0 daemonsets
#    Keep the daemonset versioning aligned with the ovnkube release versions
# Commands ($1 values)
#    ovs-server     Runs the ovs daemons - ovsdb-server and ovs-switchd (v3)
#    run-ovn-northd Runs ovn-northd as a process does not run nb_ovsdb or sb_ovsdb (v3)
#    ovn-identity   Runs ovnkube-identity (v3)
#    ovn-controller Runs ovn controller (v3)
#    ovn-node       Runs ovnkube in node mode (v3)
#    cleanup-ovn-node   Runs ovnkube to cleanup the node (v3)
#    cleanup-ovs-server Cleanup ovs-server (v3)
#    display        Displays log files
#    display_env    Displays environment variables
#    ovn_debug      Displays ovn/ovs configuration and flows

# NOTE: The script/image must be compatible with the daemonset.
# This script supports version 1.4.0 daemonsets
#      When called, it starts all needed daemons.
# Currently the version here is used to match with the image version
# It must be updated during every release

# ====================
# Environment variables are used to customize operation
# K8S_APISERVER - hostname:port (URL)of the real apiserver, not the service address - v3
# OVN_NET_CIDR - the network cidr - v3
# OVN_SVC_CIDR - the cluster-service-cidr - v3
# OVN_KUBERNETES_NAMESPACE - k8s namespace - v3
# K8S_NODE - hostname of the node - v3
#
# OVN_DAEMONSET_VERSION - version match daemonset and image - v1.4.0
# K8S_TOKEN - the apiserver token. Automatically detected when running in a pod - v3
# K8S_CACERT - the apiserver CA. Automatically detected when running in a pod - v3
# K8S_TOKEN_FILE - the apiserver token file. Automatically detected when running in a pod - v3
# K8S_CACERT_DATA - the apiserver CA data.
# OVN_CONTROLLER_OPTS - the options for ovn-ctl
# OVN_NORTHD_OPTS - the options for the ovn northbound db
# OVN_GATEWAY_MODE - the gateway mode (shared or local) - v3
# OVN_GATEWAY_OPTS - the options for the ovn gateway
# OVN_GATEWAY_ROUTER_SUBNET - the gateway router subnet (shared mode, DPU only) - v3
# OVNKUBE_LOGLEVEL - log level for ovnkube (0..5, default 4) - v3
# OVN_LOGLEVEL_NORTHD - log level (ovn-ctl default: -vconsole:emer -vsyslog:err -vfile:info) - v3
# OVN_LOGLEVEL_NB - log level (ovn-ctl default: -vconsole:off -vfile:info) - v3
# OVN_LOGLEVEL_SB - log level (ovn-ctl default: -vconsole:off -vfile:info) - v3
# OVN_LOGLEVEL_CONTROLLER - log level (ovn-ctl default: -vconsole:off -vfile:info) - v3
# OVN_LOGLEVEL_NBCTLD - log level (ovn-ctl default: -vconsole:off -vfile:info) - v3
# OVNKUBE_LOGFILE_MAXSIZE - log file max size in MB(default 100 MB)
# OVNKUBE_LOGFILE_MAXBACKUPS - log file max backups (default 5)
# OVNKUBE_LOGFILE_MAXAGE - log file max age in days (default 5 days)
# OVNKUBE_LIBOVSDB_CLIENT_LOGFILE - separate log file for libovsdb client (default: do not separate from logfile)
# OVN_ACL_LOGGING_RATE_LIMIT - specify default ACL logging rate limit in messages per second (default: 20)
# OVN_REMOTE_PROBE_INTERVAL - ovn remote probe interval in ms (default 100000)
# OVN_MONITOR_ALL - ovn-controller monitor all data in SB DB
# OVN_OFCTRL_WAIT_BEFORE_CLEAR - ovn-controller wait time in ms before clearing OpenFlow rules during start up
# OVN_ENABLE_LFLOW_CACHE - enable ovn-controller lflow-cache
# OVN_LFLOW_CACHE_LIMIT - maximum number of logical flow cache entries of ovn-controller
# OVN_LFLOW_CACHE_LIMIT_KB - maximum size of the logical flow cache of ovn-controller
# OVN_ADMIN_NETWORK_POLICY_ENABLE - enable admin network policy for ovn-kubernetes
# OVN_EGRESSIP_ENABLE - enable egress IP for ovn-kubernetes
# OVN_EGRESSIP_HEALTHCHECK_PORT - egress IP node check to use grpc on this port (0 ==> dial to port 9 instead)
# OVN_EGRESSFIREWALL_ENABLE - enable egressFirewall for ovn-kubernetes
# OVN_EGRESSQOS_ENABLE - enable egress QoS for ovn-kubernetes
# OVN_EGRESSSERVICE_ENABLE - enable egress Service for ovn-kubernetes
# OVN_UNPRIVILEGED_MODE - execute CNI ovs/netns commands from host (default no)
# OVNKUBE_NODE_MODE - ovnkube node mode of operation, one of: full, dpu, dpu-host (default: full)
# OVNKUBE_NODE_MGMT_PORT_NETDEV - ovnkube node management port netdev.
# OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME - ovnkube node management port device plugin resource
# OVN_ENCAP_IP - encap IP to be used for OVN traffic on the node. mandatory in case ovnkube-node-mode=="dpu"
# OVN_HOST_NETWORK_NAMESPACE - namespace to classify host network traffic for applying network policies
# OVN_DISABLE_FORWARDING - disable forwarding on OVNK controlled interfaces
# OVN_ENABLE_MULTI_EXTERNAL_GATEWAY - enable multi external gateway for ovn-kubernetes
# OVN_ENABLE_OVNKUBE_IDENTITY - enable per node certificate ovn-kubernetes
# OVN_METRICS_CONTROLLER_PORT - metrics port exposed by ovnkube-controller (default 9409)
# OVN_METRICS_MASTER_PORT - deprecated alias for OVN_METRICS_CONTROLLER_PORT
# OVN_METRICS_WORKER_PORT - metrics port which will be exposed by ovnkube-node (default 9410)
# OVN_METRICS_BIND_PORT - port for the OVN metrics server to serve on (default 9476)
# OVN_METRICS_EXPORTER_PORT - ovs-metrics exporter port (default 9310)
# OVN_KUBERNETES_CONNTRACK_ZONE - Conntrack zone number used for openflow rules (default 64000)
# OVN_NORTHD_BACKOFF_INTERVAL - ovn northd backoff interval in ms (default 300)
# OVN_ENABLE_SVC_TEMPLATE_SUPPORT - enable svc template support
# OVN_ENABLE_DNSNAMERESOLVER - enable dns name resolver support
# OVN_ALLOW_ICMP_NETPOL - allow ICMP and ICMPv6 regardless of network policy
# OVN_OBSERV_ENABLE - enable observability for ovnkube

# The argument to the command is the operation to be performed
# ovn-controller ovn-node display display_env ovn_debug
# a cmd must be provided, there is no default
cmd=${1:-""}

bracketify() { case "$1" in *:*) echo "[$1]" ;; *) echo "$1" ;; esac; }

# ovn daemon log levels
ovn_loglevel_northd=${OVN_LOGLEVEL_NORTHD:-"-vconsole:info"}
ovn_loglevel_nb=${OVN_LOGLEVEL_NB:-"-vconsole:info"}
ovn_loglevel_sb=${OVN_LOGLEVEL_SB:-"-vconsole:info"}
ovn_loglevel_controller=${OVN_LOGLEVEL_CONTROLLER:-"-vconsole:info"}

ovnkubelogdir=/var/log/ovn-kubernetes

# logfile rotation parameters
ovnkube_logfile_maxsize=${OVNKUBE_LOGFILE_MAXSIZE:-"100"}
ovnkube_logfile_maxbackups=${OVNKUBE_LOGFILE_MAXBACKUPS:-"5"}
ovnkube_logfile_maxage=${OVNKUBE_LOGFILE_MAXAGE:-"5"}

# logfile for libovsdb client. When not specified, the ovsdb client logs
# are not separated from the "main" --logfile used by ovnkube
ovnkube_libovsdb_client_logfile=${OVNKUBE_LIBOVSDB_CLIENT_LOGFILE:-}

# ovnkube.sh version (Update during each release)
ovnkube_version="1.4.0"

# The daemonset version must be compatible with this script.
# The default when OVN_DAEMONSET_VERSION is not set is version 3
ovn_daemonset_version=${OVN_DAEMONSET_VERSION:-"1.4.0"}

# hostname is the host's hostname when using host networking,
# otherwise it is the container ID (useful for debugging).
ovn_pod_host=${K8S_NODE:-$(hostname)}

# The ovs user id, by default it is going to be root:root
ovs_user_id=${OVS_USER_ID:-""}

# ovs options
ovs_options=${OVS_OPTIONS:-""}

if [[ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]]; then
  k8s_token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
else
  k8s_token=${K8S_TOKEN}
fi

# certs and private keys for k8s and OVN
k8s_cacert=${K8S_CACERT:-/var/run/secrets/kubernetes.io/serviceaccount/ca.crt}

# NB cert material used by ovnkube components to authenticate the Egress IP
# gRPC health-check channel (historical name; no longer used for the OVN NB
# DB connection which is unix-socket only since central mode was removed).
ovn_ca_cert=/ovn-cert/ca-cert.pem
ovn_controller_pk=/ovn-cert/ovncontroller-privkey.pem
ovn_controller_cert=/ovn-cert/ovncontroller-cert.pem
ovn_controller_cname="ovncontroller"

# ovn-northd - /etc/sysconfig/ovn-northd
ovn_northd_opts=${OVN_NORTHD_OPTS:-""}

# ovn-controller
ovn_controller_opts=${OVN_CONTROLLER_OPTS:-""}

# set the log level for ovnkube
ovnkube_loglevel=${OVNKUBE_LOGLEVEL:-4}

# by default it is going to be a shared gateway mode, however this can be overridden to
# any of the other supported gateway modes via the helm chart's `global.gatewayMode` value.
ovn_gateway_mode=${OVN_GATEWAY_MODE:-"shared"}
ovn_gateway_opts=${OVN_GATEWAY_OPTS:-""}
ovn_gateway_router_subnet=${OVN_GATEWAY_ROUTER_SUBNET:-""}

net_cidr=${OVN_NET_CIDR:-10.128.0.0/14/23}
svc_cidr=${OVN_SVC_CIDR:-172.30.0.0/16}
mtu=${OVN_MTU:-1400}
routable_mtu=${OVN_ROUTABLE_MTU:-}

# set metrics endpoint to METRICS_IP. if METRICS_IP not set K8S_NODE_IP or default to 0.0.0.0
metrics_endpoint_ip="${METRICS_IP:-${K8S_NODE_IP:-0.0.0.0}}"
metrics_endpoint_ip=$(bracketify $metrics_endpoint_ip)

# set metrics controller port (OVN_METRICS_MASTER_PORT kept as deprecated alias)
metrics_controller_port=${OVN_METRICS_CONTROLLER_PORT:-${OVN_METRICS_MASTER_PORT:-9409}}

# set metrics worker port
metrics_worker_port=${OVN_METRICS_WORKER_PORT:-9410}

# set metrics bind port
metrics_bind_port=${OVN_METRICS_BIND_PORT:-9476}

# set metrics exporter port
metrics_exporter_port=${OVN_METRICS_EXPORTER_PORT:-9310}

ovn_kubernetes_namespace=${OVN_KUBERNETES_NAMESPACE:-ovn-kubernetes}
# namespace used for classifying host network traffic
ovn_host_network_namespace=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}

# OVN_ENCAP_PORT - GENEVE UDP port (default 6081)
ovn_encap_port=${OVN_ENCAP_PORT:-6081}

ovn_hybrid_overlay_enable=${OVN_HYBRID_OVERLAY_ENABLE:-}
ovn_hybrid_overlay_net_cidr=${OVN_HYBRID_OVERLAY_NET_CIDR:-}
ovn_disable_snat_multiple_gws=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-}
ovn_disable_forwarding=${OVN_DISABLE_FORWARDING:-}
ovn_disable_pkt_mtu_check=${OVN_DISABLE_PKT_MTU_CHECK:-}
ovn_empty_lb_events=${OVN_EMPTY_LB_EVENTS:-}
# OVN_V4_JOIN_SUBNET - v4 join subnet
ovn_v4_join_subnet=${OVN_V4_JOIN_SUBNET:-}
# OVN_V6_JOIN_SUBNET - v6 join subnet
ovn_v6_join_subnet=${OVN_V6_JOIN_SUBNET:-}
# OVN_V4_MASQUERADE_SUBNET - v4 masquerade subnet
ovn_v4_masquerade_subnet=${OVN_V4_MASQUERADE_SUBNET:-}
# OVN_V6_MASQUERADE_SUBNET - v6 masquerade subnet
ovn_v6_masquerade_subnet=${OVN_V6_MASQUERADE_SUBNET:-}
# OVN_V4_TRANSIT_SUBNET - v4 Transit subnet
ovn_v4_transit_subnet=${OVN_V4_TRANSIT_SUBNET:-}
# OVN_V6_TRANSIT_SUBNET - v6 Transit subnet
ovn_v6_transit_subnet=${OVN_V6_TRANSIT_SUBNET:-}
#OVN_REMOTE_PROBE_INTERVAL - ovn remote probe interval in ms (default 100000)
ovn_remote_probe_interval=${OVN_REMOTE_PROBE_INTERVAL:-100000}
#OVN_MONITOR_ALL - ovn-controller monitor all data in SB DB
ovn_monitor_all=${OVN_MONITOR_ALL:-}
#OVN_OFCTRL_WAIT_BEFORE_CLEAR - ovn-controller wait time in ms before clearing OpenFlow rules during start up
ovn_ofctrl_wait_before_clear=${OVN_OFCTRL_WAIT_BEFORE_CLEAR:-}
ovn_enable_lflow_cache=${OVN_ENABLE_LFLOW_CACHE:-}
ovn_lflow_cache_limit=${OVN_LFLOW_CACHE_LIMIT:-}
ovn_lflow_cache_limit_kb=${OVN_LFLOW_CACHE_LIMIT_KB:-}
ovn_multicast_enable=${OVN_MULTICAST_ENABLE:-}
ovn_admin_network_policy_enable=${OVN_ADMIN_NETWORK_POLICY_ENABLE:=false}
#OVN_EGRESSIP_ENABLE - enable egress IP for ovn-kubernetes
ovn_egressip_enable=${OVN_EGRESSIP_ENABLE:-false}
#OVN_EGRESSIP_HEALTHCHECK_PORT - egress IP node check to use grpc on this port
ovn_egress_ip_healthcheck_port=${OVN_EGRESSIP_HEALTHCHECK_PORT:-9107}
#OVN_EGRESSFIREWALL_ENABLE - enable egressFirewall for ovn-kubernetes
ovn_egressfirewall_enable=${OVN_EGRESSFIREWALL_ENABLE:-false}
#OVN_EGRESSQOS_ENABLE - enable egress QoS for ovn-kubernetes
ovn_egressqos_enable=${OVN_EGRESSQOS_ENABLE:-false}
#OVN_EGRESSSERVICE_ENABLE - enable egress Service for ovn-kubernetes
ovn_egressservice_enable=${OVN_EGRESSSERVICE_ENABLE:-false}
#OVN_MULTI_NETWORK_ENABLE - enable multiple network support for ovn-kubernetes
ovn_multi_network_enable=${OVN_MULTI_NETWORK_ENABLE:-false}
#OVN_NETWORK_SEGMENTATION_ENABLE - enable user defined primary networks for ovn-kubernetes
ovn_network_segmentation_enable=${OVN_NETWORK_SEGMENTATION_ENABLE:=false}
#OVN_NETWORK_CONNECT_ENABLE - enable network connect for ovn-kubernetes
ovn_network_connect_enable=${OVN_NETWORK_CONNECT_ENABLE:=false}
#OVN_UPLINK_ENABLE - enable uplink for ovn-kubernetes
ovn_uplink_enable=${OVN_UPLINK_ENABLE:=false}
#OVN_PRE_CONF_UDN_ADDR_ENABLE - enable connecting workloads with custom network configuration to UDNs
ovn_pre_conf_udn_addr_enable=${OVN_PRE_CONF_UDN_ADDR_ENABLE:=false}
#OVN_ROUTE_ADVERTISEMENTS_ENABLE - enable route advertisements for ovn-kubernetes
ovn_route_advertisements_enable=${OVN_ROUTE_ADVERTISEMENTS_ENABLE:=false}
#OVN_EVPN_ENABLE - enable EVPN for ovn-kubernetes
ovn_evpn_enable=${OVN_EVPN_ENABLE:=false}
#OVN_ADVERTISED_UDN_ISOLATION_MODE - pod network isolation between advertised UDN networks.
ovn_advertised_udn_isolation_mode=${OVN_ADVERTISED_UDN_ISOLATION_MODE:=strict}
#OVN_DYNAMIC_UDN_ALLOCATION - dynamic UDN allocation when a node requires it (pod or egress IP)
ovn_enable_dynamic_udn_allocation=${OVN_DYNAMIC_UDN_ALLOCATION}
#OVN_DYNAMIC_UDN_GRACE_PERIOD - period of time before an inactive UDN will be garbage collected
ovn_dynamic_udn_grace_period=${OVN_DYNAMIC_UDN_GRACE_PERIOD:-}
ovn_acl_logging_rate_limit=${OVN_ACL_LOGGING_RATE_LIMIT:-"20"}
ovn_netflow_targets=${OVN_NETFLOW_TARGETS:-}
ovn_sflow_targets=${OVN_SFLOW_TARGETS:-}
ovn_ipfix_targets=${OVN_IPFIX_TARGETS:-}
ovn_ipfix_sampling=${OVN_IPFIX_SAMPLING:-} \
ovn_ipfix_cache_max_flows=${OVN_IPFIX_CACHE_MAX_FLOWS:-} \
ovn_ipfix_cache_active_timeout=${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT:-} \
#OVN_STATELESS_NETPOL_ENABLE - enable stateless network policy for ovn-kubernetes
ovn_stateless_netpol_enable=${OVN_STATELESS_NETPOL_ENABLE:-false}
#OVN_ENABLE_MULTI_EXTERNAL_GATEWAY - enable multi external gateway
ovn_enable_multi_external_gateway=${OVN_ENABLE_MULTI_EXTERNAL_GATEWAY:-false}
#OVN_ENABLE_OVNKUBE_IDENTITY - enable per node cert
ovn_enable_ovnkube_identity=${OVN_ENABLE_OVNKUBE_IDENTITY:-true}
#OVN_ENABLE_PERSISTENT_IPS - enable IPAM for virtualization workloads (KubeVirt persistent IPs)
ovn_enable_persistent_ips=${OVN_ENABLE_PERSISTENT_IPS:-false}
# OVNKUBE_CLUSTER_DEFAULT_NAD - namespace/name of the default cluster wide net-attach-def.
# When unset, ovnkube defaults to <ovn-config-namespace>/default.
ovnkube_cluster_default_nad=${OVNKUBE_CLUSTER_DEFAULT_NAD:-}

# only pass the flag when the env variable is set, otherwise let ovnkube
# resolve the default
ovnkube_cluster_default_nad_flag=
if [[ -n "${ovnkube_cluster_default_nad}" ]]; then
  ovnkube_cluster_default_nad_flag="--cluster-default-nad=${ovnkube_cluster_default_nad}"
fi

# OVNKUBE_NODE_MODE - is the mode which ovnkube node operates
ovnkube_node_mode=${OVNKUBE_NODE_MODE:-"full"}
# OVN_SIMULATE_DPU - use simulated DPU operations instead of SR-IOV/switchdev hardware operations
ovn_simulate_dpu=${OVN_SIMULATE_DPU:-"false"}
# OVN_DPU_HOST_GATEWAY_REPRESENTOR_INTERFACE - the DPU-side representor interface for the host's uplink (PF)
ovn_dpu_host_gateway_representor_interface=${OVN_DPU_HOST_GATEWAY_REPRESENTOR_INTERFACE:-""}
# OVNKUBE_NODE_MGMT_PORT_NETDEV - is the net device to be used for management port
ovnkube_node_mgmt_port_netdev=${OVNKUBE_NODE_MGMT_PORT_NETDEV:-}
# OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME - is the device plugin resource name that has
# allocated interfaces to be used for the management port
ovnkube_node_mgmt_port_dp_resource_name=${OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME:-}
ovnkube_config_duration_enable=${OVNKUBE_CONFIG_DURATION_ENABLE:-false}
ovnkube_metrics_scale_enable=${OVNKUBE_METRICS_SCALE_ENABLE:-false}
# OVN_ENCAP_IP - encap IP to be used for OVN traffic on the node
ovn_encap_ip=${OVN_ENCAP_IP:-}
# OVN_KUBERNETES_CONNTRACK_ZONE - conntrack zone number used for openflow rules (default 64000)
ovn_conntrack_zone=${OVN_KUBERNETES_CONNTRACK_ZONE:-64000}

ovn_ex_gw_network_interface=${OVN_EX_GW_NETWORK_INTERFACE:-}
# OVN_NORTHD_BACKOFF_INTERVAL - northd backoff interval in ms
# defualt is 300; no backoff delay if set to 0
ovn_northd_backoff_interval=${OVN_NORTHD_BACKOFF_INTERVAL:-"300"}
# OVN_ENABLE_SVC_TEMPLATE_SUPPORT - enable svc template support
ovn_enable_svc_template_support=${OVN_ENABLE_SVC_TEMPLATE_SUPPORT:-true}

#OVN_NETWORK_QOS_ENABLE - enable network QoS for ovn-kubernetes
ovn_network_qos_enable=${OVN_NETWORK_QOS_ENABLE:-false}
# OVN_ENABLE_STATUS_METRICS - use metrics instead of per-node status shards
ovn_enable_status_metrics=${OVN_ENABLE_STATUS_METRICS:-false}
# OVN_STATUS_METRICS_PROMETHEUS_URL - Prometheus query URL for CM status rollup
ovn_status_metrics_prometheus_url=${OVN_STATUS_METRICS_PROMETHEUS_URL:-}
# OVN_ENABLE_DNSNAMERESOLVER - enable dns name resolver support
ovn_enable_dnsnameresolver=${OVN_ENABLE_DNSNAMERESOLVER:-false}
# OVN_ALLOW_ICMP_NETPOL - allow ICMP/ICMPv6 with network policy
ovn_allow_icmp_netpol=${OVN_ALLOW_ICMP_NETPOL:-false}
# OVN_OBSERV_ENABLE - enable observability for ovnkube
ovn_observ_enable=${OVN_OBSERV_ENABLE:-false}
# OVN_NOHOSTSUBNET_LABEL - node label indicating nodes managing their own network
ovn_nohostsubnet_label=${OVN_NOHOSTSUBNET_LABEL:-""}
# OVN_DISABLE_REQUESTEDCHASSIS - disable requested-chassis option during pod creation
# should be set to true when dpu nodes are in the cluster
ovn_disable_requestedchassis=${OVN_DISABLE_REQUESTEDCHASSIS:-false}

# external_ids:host-k8s-nodename is set on an Open_vSwitch enabled system if the ovnkube stack
# should function on behalf of a different host than external_ids:hostname. This includes
# all the components that belond in an ovnkube stack (i.e. NB DB, SB DB, ovnkube etc)
# overwrite the K8S_NODE env var with the one found within the OVS metadata in this case
ovn_k8s_node=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:host-k8s-nodename | tr -d '\"')
if [[ ! -z $ovn_k8s_node ]]; then
  echo "host-k8s-nodename is set, overriding K8S_NODE with $ovn_k8s_node"
  K8S_NODE=$ovn_k8s_node
fi

# Determine the ovn rundir.
if [[ -f /usr/bin/ovn-appctl ]]; then
  # ovn-appctl is present. Use new ovn run dir path.
  OVN_RUNDIR=/var/run/ovn
  OVNCTL_PATH=/usr/share/ovn/scripts/ovn-ctl
  OVN_LOGDIR=/var/log/ovn
  OVN_ETCDIR=/etc/ovn
else
  # ovn-appctl is not present. Use openvswitch run dir path.
  OVN_RUNDIR=/var/run/openvswitch
  OVNCTL_PATH=/usr/share/openvswitch/scripts/ovn-ctl
  OVN_LOGDIR=/var/log/openvswitch
  OVN_ETCDIR=/etc/openvswitch
fi

OVS_RUNDIR=/var/run/openvswitch
OVS_LOGDIR=/var/log/openvswitch

# =========================================

setup_ovs_permissions() {
  if [ ${ovs_user_id:-XX} != "XX" ]; then
    chown -R ${ovs_user_id} /etc/openvswitch
    chown -R ${ovs_user_id} ${OVS_RUNDIR}
    chown -R ${ovs_user_id} ${OVS_LOGDIR}
    chown -R ${ovs_user_id} ${OVN_ETCDIR}
    chown -R ${ovs_user_id} ${OVN_RUNDIR}
    chown -R ${ovs_user_id} ${OVN_LOGDIR}
  fi
}

run_as_ovs_user_if_needed() {
  setup_ovs_permissions

  if [ ${ovs_user_id:-XX} != "XX" ]; then
    local uid=$(id -u "${ovs_user_id%:*}")
    local gid=$(id -g "${ovs_user_id%:*}")
    local groups=$(id -G "${ovs_user_id%:*}" | tr ' ' ',')

    setpriv --reuid $uid --regid $gid --groups $groups "$@"
    echo "run as: setpriv --reuid $uid --regid $gid --groups $groups $@"
  else
    "$@"
    echo "run as: $@"
  fi
}

# wait_for_event [attempts=<num>] function_to_call [arguments_to_function]
#
# Processes running inside the container should immediately start, so we
# shouldn't be making 80 attempts (default value). The "attempts=<num>"
# argument will help us in configuring that value.
wait_for_event() {
  retries=0
  sleeper=1
  attempts=80
  if [[ $1 =~ ^attempts= ]]; then
    eval $1
    shift
  fi
  while true; do
    $@
    if [[ $? != 0 ]]; then
      ((retries += 1))
      if [[ "${retries}" -gt ${attempts} ]]; then
        echo "error: $@ did not come up, exiting"
        exit 1
      fi
      echo "info: Waiting for $@ to come up, waiting ${sleeper}s ..."
      sleep ${sleeper}
      sleeper=5
    else
      if [[ "${retries}" != 0 ]]; then
        echo "$@ came up in ${retries} ${sleeper} sec tries"
      fi
      break
    fi
  done
}

# wait_ovnkube_controller_with_node_done - Wait for ovnkube-controller-with-node process to complete
# Checks if the ovnkube-controller-with-node process is running by looking for its PID file.
# If the PID file exists, waits for that process to finish before continuing.
# If the PID file doesnt exist, it means the process has already exited.
wait_ovnkube_controller_with_node_done() {
  local pid_file=${OVN_RUNDIR}/ovnkube-controller-with-node.pid
   if [[ -f ${pid_file} ]]; then
     echo "info: waiting on ovnkube-controller-with-node process to end"
     wait $(cat $pid_file)
     echo "info: done waiting for ovn-controller-with-node to end"
  fi
}

# In IC mode NB/SB run as local unix-socket-only OVSDB containers
# (local-nb-ovsdb / local-sb-ovsdb). Consumers (ovn-northd,
# ovnkube-controller, ovn-controller, ovn-cluster-manager) wait on this
# function via wait_for_event before starting.
local_dbs_ready() {
  for daemon in ovnnb_db ovnsb_db; do
    pidfile=${OVN_RUNDIR}/${daemon}.pid
    if [[ -f ${pidfile} ]]; then
      check_health $daemon $(cat $pidfile)
      if [[ $? == 0 ]]; then
        continue
      fi
    fi
    return 1
  done
  return 0
}

# check that daemonset version is among expected versions
check_ovn_daemonset_version() {
  ok=$1
  for v in ${ok}; do
    if [[ $v == ${ovn_daemonset_version} ]]; then
      return 0
    fi
  done
  echo "VERSION MISMATCH expect ${ok}, daemonset is version ${ovn_daemonset_version}"
  exit 1
}

# OVS must be up before OVN comes up.
# This checks if OVS is up and running
ovs_ready() {
  for daemon in $(echo ovsdb-server ovs-vswitchd); do
    pidfile=${OVS_RUNDIR}/${daemon}.pid
    if [[ -f ${pidfile} ]]; then
      check_health $daemon $(cat $pidfile)
      if [[ $? == 0 ]]; then
        continue
      fi
    fi
    return 1
  done
  return 0
}

# get_bridge_name_for_physnet - Extract OVS bridge name for a given OVN physical network
# Takes an OVN network name for physical networks (physnet) and returns the corresponding
# OVS bridge name from the ovn-bridge-mappings configuration.
# Return empty string if not found.
get_bridge_name_for_physnet() {
      local physnet="$1"
      local mappings
      mappings=$(ovs-vsctl --if-exists get open_vswitch . external_ids:ovn-bridge-mappings)
      # Extract bridge name after physnet: and before next comma (or end)
      # regex matches zero or more non-comma characters
      # cut on colon and return field number 2
      echo "$mappings" | tr -d "\"" | grep -o "$physnet:[^,]*" | cut -d: -f2
}

# Adds drop flows for GARPs on patch port to br-int for specified bridge.
add_garp_drop_flow() {
    local bridge="$1"
    local cookie="0x0305"
    local priority="498"
    # if bridge exists, and the patch port is created, we expect to add at least one flow to a patch port ending in to-br-int.
    # FIXME: can we generate the exact name. Its possible we add these flows to the incorrect port when selecting on substring
    for port_name in $(ovs-vsctl list-ports $bridge); do
        if [[ "$port_name" == *to-br-int ]]; then
            local of_port=$(ovs-vsctl get interface $port_name ofport)
            ovs-ofctl add-flow $bridge "cookie=$cookie,table=0,priority=$priority,in_port=$of_port,arp,arp_op=1,actions=drop" > /dev/null
            break
        fi
    done
}

# Verify that the process is running either by checking for the PID in `ps` output
# or by using `ovs-appctl` utility for the processes that support it.
# $1 is the name of the process
process_ready() {
  case ${1} in
  "ovsdb-server" | "ovs-vswitchd")
    pidfile=${OVS_RUNDIR}/${1}.pid
    ;;
  *)
    pidfile=${OVN_RUNDIR}/${1}.pid
    ;;
  esac

  if [[ -f ${pidfile} ]]; then
    check_health $1 $(cat $pidfile)
    if [[ $? == 0 ]]; then
      return 0
    fi
  fi
  return 1
}

# continuously checks if process is healthy. Exits if process terminates.
# $1 is the name of the process
# $2 is the pid of an another process to kill before exiting
process_healthy() {
  case ${1} in
  "ovsdb-server" | "ovs-vswitchd")
    pid=$(cat ${OVS_RUNDIR}/${1}.pid)
    ;;
  *)
    pid=$(cat ${OVN_RUNDIR}/${1}.pid)
    ;;
  esac

  while true; do
    check_health $1 ${pid}
    if [[ $? != 0 ]]; then
      echo "=============== pid ${pid} terminated ========== "
      # kill the tail -f
      if [[ $2 != "" ]]; then
        kill $2
      fi
      exit 6
    fi
    sleep 15
  done
}

# checks for the health of the process either using `ps` or `ovs-appctl`
# $1 is the name of the process
# $2 is the process pid
check_health() {
  ctl_file=""
  case ${1} in
  "ovnkube" | "ovnkube-cluster-manager" | "ovnkube-controller" | "ovnkube-controller-with-node" | "ovnkube-identity" )
    # just check for presence of pid
    ;;
  "ovnnb_db" | "ovnsb_db")
    ctl_file=${OVN_RUNDIR}/${1}.ctl
    ;;
  "ovn-northd" | "ovn-controller")
    ctl_file=${OVN_RUNDIR}/${1}.${2}.ctl
    ;;
  "ovsdb-server" | "ovs-vswitchd")
    ctl_file=${OVS_RUNDIR}/${1}.${2}.ctl
    ;;
  *)
    echo "Unknown service ${1} specified. Exiting.. "
    exit 1
    ;;
  esac

  if [[ ${ctl_file} == "" ]]; then
    # no control file, so just do the PID check
    pid=${2}
    pidTest=$(ps ax | awk '{ print $1 }' | grep "^${pid:-XX}$")
    if [[ ${pid:-XX} == ${pidTest} ]]; then
      return 0
    fi
  else
    # use ovs-appctl to do the check
    ovs-appctl -t ${ctl_file} version >/dev/null
    if [[ $? == 0 ]]; then
      return 0
    fi
  fi

  return 1
}

get_gw_options() {
  # Build a map from the existing gateway options and overlay any ovn-gw-*
  # values from OVS external_ids. This preserves global options while allowing
  # node-local OVS settings to override/add gateway parameters.
  local opt
  local key
  local value
  declare -A gw_opts_map=()

  for opt in ${ovn_gateway_opts}; do
    if [[ ${opt} == --*=* ]]; then
      gw_opts_map["${opt%%=*}"]="${opt#*=}"
    elif [[ ${opt} == --* ]]; then
      gw_opts_map["${opt}"]=""
    fi
  done

  # get the gateway interface
  value=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-gw-interface | tr -d \")
  if [[ -n ${value} ]]; then
    gw_opts_map["--gateway-interface"]="${value}"
  fi

  # get the gateway nexthop
  value=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-gw-nexthop | tr -d \")
  if [[ -n ${value} ]]; then
    gw_opts_map["--gateway-nexthop"]="${value}"
  fi

  # get the gateway vlanid
  value=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-gw-vlanid | tr -d \")
  if [[ -n ${value} ]]; then
    gw_opts_map["--gateway-vlanid"]="${value}"
  fi

  ovn_gateway_opts=""
  for key in "${!gw_opts_map[@]}"; do
    value="${gw_opts_map["${key}"]}"
    if [[ ${value} == "" ]]; then
      ovn_gateway_opts+="${key} "
    else
      ovn_gateway_opts+="${key}=${value} "
    fi
  done

  # Get gateway options for DPUs
  if [[ ${ovnkube_node_mode} == "dpu" ]]; then
    # this is only required if the DPU and DPU Host are in different subnets
    if [[ ${ovn_gateway_router_subnet} == "" ]]; then
      ovn_gateway_router_subnet=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-gw-router-subnet | tr -d \")
    fi
  fi

}

display_file() {
  if [[ -f $3 ]]; then
    echo "====================== $1 pid "
    cat $2
    echo "====================== $1 log "
    cat $3
    echo " "
  fi
}

# pid and log file for each container
display() {
  echo "==================== display for ${ovn_pod_host}  =================== "
  date
  display_file "local-nb-ovsdb" ${OVN_RUNDIR}/ovnnb_db.pid ${OVN_LOGDIR}/ovsdb-server-nb.log
  display_file "local-sb-ovsdb" ${OVN_RUNDIR}/ovnsb_db.pid ${OVN_LOGDIR}/ovsdb-server-sb.log
  display_file "run-ovn-northd" ${OVN_RUNDIR}/ovn-northd.pid ${OVN_LOGDIR}/ovn-northd.log
  display_file "ovs-vswitchd" ${OVS_RUNDIR}/ovs-vswitchd.pid ${OVS_LOGDIR}/ovs-vswitchd.log
  display_file "ovsdb-server" ${OVS_RUNDIR}/ovsdb-server.pid ${OVS_LOGDIR}/ovsdb-server.log
  display_file "ovn-controller" ${OVN_RUNDIR}/ovn-controller.pid ${OVN_LOGDIR}/ovn-controller.log
  display_file "ovnkube" ${OVN_RUNDIR}/ovnkube.pid ${ovnkubelogdir}/ovnkube.log
}

setup_cni() {
  cp -f /usr/libexec/cni/ovn-k8s-cni-overlay /opt/cni/bin/ovn-k8s-cni-overlay
}

set_northd_probe_interval() {
  # OVN_NORTHD_PROBE_INTERVAL - probe interval of northd for NB and SB DB
  # connections in ms (default 5000)
  northd_probe_interval=${OVN_NORTHD_PROBE_INTERVAL:-5000}

  echo "setting northd probe interval to ${northd_probe_interval} ms"

  output=$(ovn-nbctl --if-exists get NB_GLOBAL . options:northd_probe_interval)
  if [[ $? == 0 ]]; then
    output=$(echo ${output} | tr -d '\"')
    echo "the current value of northd probe interval is ${output} ms"
    if [[ "${output}" != "${northd_probe_interval}" ]]; then
      ovn-nbctl set NB_GLOBAL . options:northd_probe_interval=${northd_probe_interval}
      if [[ $? != 0 ]]; then
        echo "Failed to set northd probe interval to ${northd_probe_interval}. Exiting....."
        exit 13
      fi
      echo "successfully set northd probe interval to ${northd_probe_interval} ms"
    fi
  fi
  return 0
}

ovsdb_cleanup() {
  local db=${1}
  ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl exit >/dev/null 2>&1
  kill $(jobs -p) >/dev/null 2>&1
  exit 0
}

display_version() {
  echo " =================== hostname: ${ovn_pod_host}"
  echo " =================== daemonset version ${ovn_daemonset_version}"
  if [[ -f /root/git_info ]]; then
    disp_ver=$(cat /root/git_info)
    echo " =================== Image built from ovn-kubernetes ${disp_ver}"
    return
  fi
}

display_env() {
  echo OVS_USER_ID ${ovs_user_id}
  echo OVS_OPTIONS ${ovs_options}
  echo OVN_NORTHD_OPTS ${ovn_northd_opts}
  echo OVN_CONTROLLER_OPTS ${ovn_controller_opts}
  echo OVN_LOGLEVEL_CONTROLLER ${ovn_loglevel_controller}
  echo OVN_GATEWAY_MODE ${ovn_gateway_mode}
  echo OVN_GATEWAY_OPTS ${ovn_gateway_opts}
  echo OVN_GATEWAY_ROUTER_SUBNET ${ovn_gateway_router_subnet}
  echo OVN_NET_CIDR ${net_cidr}
  echo OVN_SVC_CIDR ${svc_cidr}
  echo K8S_APISERVER ${K8S_APISERVER}
  echo OVNKUBE_LOGLEVEL ${ovnkube_loglevel}
  echo OVN_DAEMONSET_VERSION ${ovn_daemonset_version}
  echo OVNKUBE_NODE_MODE ${ovnkube_node_mode}
  echo OVN_ENCAP_IP ${ovn_encap_ip}
  echo OVN_KUBERNETES_CONNTRACK_ZONE ${ovn_conntrack_zone}
  echo ovnkube.sh version ${ovnkube_version}
  echo OVN_HOST_NETWORK_NAMESPACE ${ovn_host_network_namespace}
}

ovn_debug() {
  wait_for_event attempts=3 local_dbs_ready

  # get ovs/ovn info from the node for debug purposes (uses local NB/SB unix sockets)
  echo "=========== ovn_debug   hostname: ${ovn_pod_host} ============="
  echo "=========== ovn-nbctl show ============="
  ovn-nbctl show
  echo " "
  echo "=========== ovn-nbctl list ACL ============="
  ovn-nbctl list ACL
  echo " "
  echo "=========== ovn-nbctl list address_set ============="
  ovn-nbctl list address_set
  echo " "
  echo "=========== ovs-vsctl show ============="
  ovs-vsctl show
  echo " "
  echo "=========== ovs-ofctl -O OpenFlow13 dump-ports br-int ============="
  ovs-ofctl -O OpenFlow13 dump-ports br-int
  echo " "
  echo "=========== ovs-ofctl -O OpenFlow13 dump-ports-desc br-int ============="
  ovs-ofctl -O OpenFlow13 dump-ports-desc br-int
  echo " "
  echo "=========== ovs-ofctl dump-flows br-int ============="
  ovs-ofctl dump-flows br-int
  echo " "
  echo "=========== ovn-sbctl show ============="
  ovn-sbctl show
  echo " "
  echo "=========== ovn-sbctl lflow-list ============="
  ovn-sbctl lflow-list
  echo " "
  echo "=========== ovn-sbctl list datapath ============="
  ovn-sbctl list datapath
  echo " "
  echo "=========== ovn-sbctl list port_binding ============="
  ovn-sbctl list port_binding
}

ovs-server() {
  # start ovs ovsdb-server and ovs-vswitchd
  set -euo pipefail

  # if another process is listening on the cni-server socket, wait until it exits
  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    if /usr/share/openvswitch/scripts/ovs-ctl status >/dev/null; then
      echo "warning: Another process is currently managing OVS, waiting 10s ..." 2>&1
      sleep 10 &
      wait
      ((retries += 1))
    else
      break
    fi
    if [[ "${retries}" -gt 60 ]]; then
      echo "error: Another process is currently managing OVS, exiting" 2>&1
      exit 1
    fi
  done
  rm -f ${OVS_RUNDIR}/ovs-vswitchd.pid
  rm -f ${OVS_RUNDIR}/ovsdb-server.pid

  # launch OVS
  function quit() {
    /usr/share/openvswitch/scripts/ovs-ctl stop
    exit 1
  }
  trap quit SIGTERM

  setup_ovs_permissions

  USER_ARGS=""
  if [ ${ovs_user_id:-XX} != "XX" ]; then
    USER_ARGS="--ovs-user=${ovs_user_id}"
  fi

  # OVN-K marks NIC port as transient on startup when plugging it into ovs
  # bridge. This is done so that on ovsdb-server resrtart, the NIC interface is
  # detached from the bridge, which is necessary to restore connectivity
  # through the NIC and make the node healthy. Marking the port as transient
  # works only when we also start ovsdb-server with --delete-transient-ports.
  #
  # Note: once ovnkube is started, it will rewire the NIC port back into the
  # bridge, and move IP configuration as necessary.
  ovs_options="${ovs_options} --delete-transient-ports"

  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovs-vswitchd \
    --system-id=random ${ovs_options} ${USER_ARGS} "$@"

  # Reduce stack size to 2M from default 8M as per below commit on Openvswitch
  # https://github.com/openvswitch/ovs/commit/b82a90e266e1246fe2973db97c95df22558174ea
  # added while troubleshooting on https://bugzilla.redhat.com/show_bug.cgi?id=1572797
  ulimit -s 2048

  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovsdb-server \
    --system-id=random ${ovs_options} ${USER_ARGS} "$@"

  if [[ $(nproc) -gt 32 ]]; then
    echo "Warning: Higher memory allocation by ovs-vswitchd is expected due to high number of n-handler-threads and n-revalidator-threads"
  fi

  tail --follow=name ${OVS_LOGDIR}/ovs-vswitchd.log ${OVS_LOGDIR}/ovsdb-server.log &
  ovs_tail_pid=$!
  sleep 10
  while true; do
    if ! /usr/share/openvswitch/scripts/ovs-ctl status >/dev/null; then
      echo "OVS seems to have crashed, exiting"
      kill ${ovs_tail_pid}
      quit
    fi
    sleep 15
  done
}

cleanup-ovs-server() {
  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server (wait for ovn-node to exit) ======="
  retries=0
  while [[ ${retries} -lt 80 ]]; do
    if [[ ! -e ${OVN_RUNDIR}/ovnkube.pid ]]; then
      break
    fi
    echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server ovn-node still running, wait) ======="
    sleep 1
    ((retries += 1))
  done
  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server (ovs-ctl stop) ======="
  /usr/share/openvswitch/scripts/ovs-ctl stop
}

function memory_trim_on_compaction_supported {
  if [[ $1 == "nbdb" ]]; then
    mem_trim_check=$(ovn-appctl -t ${OVN_RUNDIR}/ovnnb_db.ctl list-commands | grep "memory-trim-on-compaction")
  elif [[ $1 == "sbdb"  ]]; then
    mem_trim_check=$(ovn-appctl -t ${OVN_RUNDIR}/ovnsb_db.ctl list-commands | grep "memory-trim-on-compaction")
  fi
  if [[ ${mem_trim_check} != "" ]]; then
    return $(/bin/true)
  else
    return $(/bin/false)
  fi
}

# v1.0.0 - run nb_ovsdb in a separate container listening only on
# unix sockets
local-nb-ovsdb() {
  trap 'ovsdb_cleanup nb' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovnnb_db.pid

  echo "=============== run nb-ovsdb (unix sockets only) =========="
  run_as_ovs_user_if_needed \
    ${OVNCTL_PATH} run_nb_ovsdb --no-monitor \
    --ovn-nb-log="${ovn_loglevel_nb}" &

  wait_for_event attempts=3 process_ready ovnnb_db
  echo "=============== nb-ovsdb (unix sockets only) ========== RUNNING"

 [[ "local" == "${OVN_GATEWAY_MODE}" &&  "true" == "${OVN_ROUTE_ADVERTISEMENTS_ENABLE}" ]] && {
    ovn-nbctl set NB_Global . options:use_ct_inv_match=false
    echo "=============== nb-ovsdb ========== reconfigured for route advertisements"
  }

  # Let ovn-northd sleep and not use so much CPU
  ovn-nbctl set NB_Global . options:northd-backoff-interval-ms=${ovn_northd_backoff_interval}
  echo "=============== nb-ovsdb ========== reconfigured for northd backoff"

  ovn-nbctl set NB_Global . name=${K8S_NODE}
  ovn-nbctl set NB_Global . options:name=${K8S_NODE}

  [[ "true" == "${ENABLE_IPSEC}" ]] && {
    ovn-nbctl set NB_Global . ipsec=true
    echo "=============== nb-ovsdb ========== reconfigured for ipsec"
  }

  tail --follow=name ${OVN_LOGDIR}/ovsdb-server-nb.log &
  ovn_tail_pid=$!

  process_healthy ovnnb_db ${ovn_tail_pid}
  echo "=============== run nb-ovsdb (unix sockets only) ========== terminated"
}

# v1.0.0 - run sb_ovsdb in a separate container listening only on
# unix sockets
local-sb-ovsdb() {
  trap 'ovsdb_cleanup sb' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovnsb_db.pid

  echo "=============== run sb-ovsdb (unix sockets only) ========== "
  run_as_ovs_user_if_needed \
    ${OVNCTL_PATH} run_sb_ovsdb --no-monitor \
    --ovn-sb-log="${ovn_loglevel_sb}" &

  wait_for_event attempts=3 process_ready ovnsb_db
  echo "=============== sb-ovsdb (unix sockets only) ========== RUNNING"

  tail --follow=name ${OVN_LOGDIR}/ovsdb-server-sb.log &
  ovn_tail_pid=$!

  process_healthy ovnsb_db ${ovn_tail_pid}
  echo "=============== run sb-ovsdb (unix sockets only) ========== terminated"
}

# v1.4.0 - Runs northd.
run-ovn-northd() {
  trap 'ovn-appctl -t ovn-northd exit >/dev/null 2>&1; exit 0' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovn-northd.pid
  rm -f ${OVN_RUNDIR}/ovn-northd.*.ctl

  echo "=============== run-ovn-northd (wait for local NB/SB)"
  wait_for_event local_dbs_ready

  echo "=============== run_ovn_northd =========="
  echo "ovn_northd_opts=${ovn_northd_opts}"
  echo "ovn_loglevel_northd=${ovn_loglevel_northd}"

  # no monitor (and no detach), start northd which connects to the
  # local NB/SB OVSDB unix sockets
  run_as_ovs_user_if_needed \
    ${OVNCTL_PATH} start_northd \
    --no-monitor --ovn-manage-ovsdb=no \
    --ovn-northd-log="${ovn_loglevel_northd}" \
    ${ovn_northd_opts}

  wait_for_event attempts=3 process_ready ovn-northd
  echo "=============== run_ovn_northd ========== RUNNING"

  tail --follow=name ${OVN_LOGDIR}/ovn-northd.log &
  ovn_tail_pid=$!

  process_healthy ovn-northd ${ovn_tail_pid}
  exit 8
}

# v1.4.0 -  run ovnkube-identity
ovnkube-identity() {
    trap 'kill $(jobs -p); exit 0' TERM
    check_ovn_daemonset_version "1.4.0"
    rm -f ${OVN_RUNDIR}/ovnkube-identity.pid

    ovnkube_enable_hybrid_overlay_flag=
    if [[ ${ovn_hybrid_overlay_enable} == "true" ]]; then
      ovnkube_enable_hybrid_overlay_flag="--enable-hybrid-overlay"
    fi

    # extra-allowed-user:
    #   ovnkube-cluster-manager service account - required for multi-homing
    exec /usr/bin/ovnkube-identity  --k8s-apiserver="${K8S_APISERVER}" \
    --webhook-cert-dir="/etc/webhook-cert" \
    ${ovnkube_enable_hybrid_overlay_flag} \
    --extra-allowed-user="system:serviceaccount:${ovn_kubernetes_namespace}:ovnkube-cluster-manager" \
    --loglevel="${ovnkube_loglevel}"

    exit 9
}

# v1.4.0 - run ovnkube --ovnkube-controller
ovnkube-controller() {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovnkube-controller.pid

  echo "=============== ovnkube-controller (wait for local NB/SB) =========="
  wait_for_event local_dbs_ready

  # wait for northd to start
  wait_for_event process_ready ovn-northd

  # wait for ovs-servers to start since ovnkube-controller sets some fields in OVS DB
  echo "=============== ovnkube-controller - (wait for ovs)"
  wait_for_event ovs_ready

  hybrid_overlay_flags=
  if [[ ${ovn_hybrid_overlay_enable} == "true" ]]; then
    hybrid_overlay_flags="--enable-hybrid-overlay"
    if [[ -n "${ovn_hybrid_overlay_net_cidr}" ]]; then
      hybrid_overlay_flags="${hybrid_overlay_flags} --hybrid-overlay-cluster-subnets=${ovn_hybrid_overlay_net_cidr}"
    fi
  fi
  echo "hybrid_overlay_flags=${hybrid_overlay_flags}"

  disable_snat_multiple_gws_flag=
  if [[ ${ovn_disable_snat_multiple_gws} == "true" ]]; then
      disable_snat_multiple_gws_flag="--disable-snat-multiple-gws"
  fi
  echo "disable_snat_multiple_gws_flag=${disable_snat_multiple_gws_flag}"

  ovn_encap_port_flag=
  if [[ -n "${ovn_encap_port}" ]]; then
      ovn_encap_port_flag="--encap-port=${ovn_encap_port}"
  fi
  echo "ovn_encap_port_flag=${ovn_encap_port_flag}"

  disable_pkt_mtu_check_flag=
  if [[ ${ovn_disable_pkt_mtu_check} == "true" ]]; then
      disable_pkt_mtu_check_flag="--disable-pkt-mtu-check"
  fi
  echo "disable_pkt_mtu_check_flag=${disable_pkt_mtu_check_flag}"

  empty_lb_events_flag=
  if [[ ${ovn_empty_lb_events} == "true" ]]; then
      empty_lb_events_flag="--ovn-empty-lb-events"
  fi
  echo "empty_lb_events_flag=${empty_lb_events_flag}"

  ovn_v4_join_subnet_opt=
  if [[ -n ${ovn_v4_join_subnet} ]]; then
      ovn_v4_join_subnet_opt="--gateway-v4-join-subnet=${ovn_v4_join_subnet}"
  fi
  echo "ovn_v4_join_subnet_opt=${ovn_v4_join_subnet_opt}"

  ovn_v6_join_subnet_opt=
  if [[ -n ${ovn_v6_join_subnet} ]]; then
      ovn_v6_join_subnet_opt="--gateway-v6-join-subnet=${ovn_v6_join_subnet}"
  fi
  echo "ovn_v6_join_subnet_opt=${ovn_v6_join_subnet_opt}"

  ovn_v4_masquerade_subnet_opt=
  if [[ -n ${ovn_v4_masquerade_subnet} ]]; then
      ovn_v4_masquerade_subnet_opt="--gateway-v4-masquerade-subnet=${ovn_v4_masquerade_subnet}"
  fi
  echo "ovn_v4_masquerade_subnet_opt=${ovn_v4_masquerade_subnet_opt}"

  ovn_v6_masquerade_subnet_opt=
  if [[ -n ${ovn_v6_masquerade_subnet} ]]; then
      ovn_v6_masquerade_subnet_opt="--gateway-v6-masquerade-subnet=${ovn_v6_masquerade_subnet}"
  fi
  echo "ovn_v6_masquerade_subnet_opt=${ovn_v6_masquerade_subnet_opt}"

  libovsdb_client_logfile_flag=
  if [[ -n ${ovnkube_libovsdb_client_logfile} ]]; then
      libovsdb_client_logfile_flag="--libovsdblogfile ${ovnkube_libovsdb_client_logfile}"
  fi

  ovn_acl_logging_rate_limit_flag=
  if [[ -n ${ovn_acl_logging_rate_limit} ]]; then
      ovn_acl_logging_rate_limit_flag="--acl-logging-rate-limit ${ovn_acl_logging_rate_limit}"
  fi
  echo "ovn_acl_logging_rate_limit_flag=${ovn_acl_logging_rate_limit_flag}"

  multicast_enabled_flag=
  if [[ ${ovn_multicast_enable} == "true" ]]; then
      multicast_enabled_flag="--enable-multicast"
  fi
  echo "multicast_enabled_flag=${multicast_enabled_flag}"

  anp_enabled_flag=
  if [[ ${ovn_admin_network_policy_enable} == "true" ]]; then
      anp_enabled_flag="--enable-admin-network-policy"
  fi
  echo "anp_enabled_flag=${anp_enabled_flag}"

  egressip_enabled_flag=
  if [[ ${ovn_egressip_enable} == "true" ]]; then
      egressip_enabled_flag="--enable-egress-ip"
  fi
  echo "egressip_enabled_flag=${egressip_enabled_flag}"

  egressip_healthcheck_port_flag=
  if [[ -n "${ovn_egress_ip_healthcheck_port}" ]]; then
      egressip_healthcheck_port_flag="--egressip-node-healthcheck-port=${ovn_egress_ip_healthcheck_port}"
  fi
  echo "egressip_healthcheck_port_flag=${egressip_healthcheck_port_flag}"

  egressfirewall_enabled_flag=
  if [[ ${ovn_egressfirewall_enable} == "true" ]]; then
	  egressfirewall_enabled_flag="--enable-egress-firewall"
  fi
  echo "egressfirewall_enabled_flag=${egressfirewall_enabled_flag}"

  egressqos_enabled_flag=
  if [[ ${ovn_egressqos_enable} == "true" ]]; then
	  egressqos_enabled_flag="--enable-egress-qos"
  fi
  echo "egressqos_enabled_flag=${egressqos_enabled_flag}"

  multi_network_enabled_flag=
  if [[ ${ovn_multi_network_enable} == "true" ]]; then
	  multi_network_enabled_flag="--enable-multi-network --enable-multi-networkpolicy"
  fi
  echo "multi_network_enabled_flag=${multi_network_enabled_flag}"

  network_segmentation_enabled_flag=
  if [[ ${ovn_network_segmentation_enable} == "true" ]]; then
	  network_segmentation_enabled_flag="--enable-multi-network --enable-network-segmentation"
  fi
  echo "network_segmentation_enabled_flag=${network_segmentation_enabled_flag}"

  network_connect_enabled_flag=
  if [[ ${ovn_network_connect_enable} == "true" ]]; then
	  network_connect_enabled_flag="--enable-network-connect"
  fi
  echo "network_connect_enabled_flag=${network_connect_enabled_flag}"

  uplink_enabled_flag=
  if [[ ${ovn_uplink_enable} == "true" ]]; then
	  uplink_enabled_flag="--enable-uplink"
  fi
  echo "uplink_enabled_flag=${uplink_enabled_flag}"

  pre_conf_udn_addr_enable_flag=
  if [[ ${ovn_pre_conf_udn_addr_enable} == "true" ]]; then
	  pre_conf_udn_addr_enable_flag="--enable-preconfigured-udn-addresses"
  fi
  echo "pre_conf_udn_addr_enable_flag=${pre_conf_udn_addr_enable_flag}"

  dynamic_udn_allocation_flag=
  if [[ ${ovn_enable_dynamic_udn_allocation} == "true" ]]; then
    dynamic_udn_allocation_flag="--enable-dynamic-udn-allocation"
  fi
  echo "dynamic_udn_allocation_flag=${dynamic_udn_allocation_flag}"

  dynamic_udn_grace_period=
  if [[ -n ${ovn_dynamic_udn_grace_period} ]]; then
    dynamic_udn_grace_period="--udn-deletion-grace-period ${ovn_dynamic_udn_grace_period}"
  fi
  echo "dynamic_udn_grace_period=${dynamic_udn_grace_period}"

  route_advertisements_enabled_flag=
  if [[ ${ovn_route_advertisements_enable} == "true" ]]; then
	  route_advertisements_enabled_flag="--enable-route-advertisements"
  fi
  echo "route_advertisements_enabled_flag=${route_advertisements_enabled_flag}"

  evpn_enabled_flag=
  if [[ ${ovn_evpn_enable} == "true" ]]; then
	  evpn_enabled_flag="--enable-evpn"
  fi
  echo "evpn_enabled_flag=${evpn_enabled_flag}"

  advertised_udn_isolation_flag=
  if [[ -n ${ovn_advertised_udn_isolation_mode} ]]; then
      advertised_udn_isolation_flag="--advertised-udn-isolation-mode=${ovn_advertised_udn_isolation_mode}"
  fi
  echo "advertised_udn_isolation_flag=${advertised_udn_isolation_flag}"

  ovnkube_config_file_flag="--config-file=/run/ovnkube-config/ovnkube.conf"
  echo "ovnkube_config_file_flag=${ovnkube_config_file_flag}"

  egressservice_enabled_flag=
  if [[ ${ovn_egressservice_enable} == "true" ]]; then
	  egressservice_enabled_flag="--enable-egress-service"
  fi
  echo "egressservice_enabled_flag=${egressservice_enabled_flag}"

  ovnkube_controller_metrics_bind_address="${metrics_endpoint_ip}:${metrics_controller_port}"
  echo "ovnkube_controller_metrics_bind_address=${ovnkube_controller_metrics_bind_address}"

  local ovnkube_metrics_tls_opts=""
  if [[ ${OVNKUBE_METRICS_PK} != "" && ${OVNKUBE_METRICS_CERT} != "" ]]; then
    ovnkube_metrics_tls_opts="
        --node-server-privkey ${OVNKUBE_METRICS_PK}
        --node-server-cert ${OVNKUBE_METRICS_CERT}
      "
  fi
  echo "ovnkube_metrics_tls_opts=${ovnkube_metrics_tls_opts}"

  ovnkube_config_duration_enable_flag=
  if [[ ${ovnkube_config_duration_enable} == "true" ]]; then
    ovnkube_config_duration_enable_flag="--metrics-enable-config-duration"
  fi
  echo "ovnkube_config_duration_enable_flag: ${ovnkube_config_duration_enable_flag}"

  ovnkube_enable_multi_external_gateway_flag=
  if [[ ${ovn_enable_multi_external_gateway} == "true" ]]; then
	  ovnkube_enable_multi_external_gateway_flag="--enable-multi-external-gateway"
  fi
  echo "ovnkube_enable_multi_external_gateway_flag=${ovnkube_enable_multi_external_gateway_flag}"

  ovnkube_metrics_scale_enable_flag=
  if [[ ${ovnkube_metrics_scale_enable} == "true" ]]; then
    ovnkube_metrics_scale_enable_flag="--metrics-enable-scale --metrics-enable-pprof"
  fi
  echo "ovnkube_metrics_scale_enable_flag: ${ovnkube_metrics_scale_enable_flag}"

  ovnkube_local_cert_flags=
  if [[ ${ovn_enable_ovnkube_identity} == "true" ]]; then
    bootstrap_kubeconfig="/host-kubernetes/kubelet.conf"
    if [ -f "${bootstrap_kubeconfig}" ]; then
      ovnkube_local_cert_flags="
        --bootstrap-kubeconfig ${bootstrap_kubeconfig}
        --cert-dir /var/run/ovn-kubernetes/certs
      "
    else
      echo "bootstrap kubeconfig file: ${bootstrap_kubeconfig} doesn't exist,
       skipping bootstrap-kubeconfig/cert-dir parameters"
    fi
  fi
  echo "ovnkube_local_cert_flags=${ovnkube_local_cert_flags}"

  ovn_enable_svc_template_support_flag=
  if [[ ${ovn_enable_svc_template_support} == "true" ]]; then
	  ovn_enable_svc_template_support_flag="--enable-svc-template-support"
  fi
  echo "ovn_enable_svc_template_support_flag=${ovn_enable_svc_template_support_flag}"

  network_qos_enabled_flag=
  if [[ ${ovn_network_qos_enable} == "true" ]]; then
      network_qos_enabled_flag="--enable-network-qos"
  fi
  echo "network_qos_enabled_flag=${network_qos_enabled_flag}"

  status_metrics_enabled_flag=
  if [[ ${ovn_enable_status_metrics} == "true" ]]; then
      status_metrics_enabled_flag="--enable-status-metrics"
  fi
  echo "status_metrics_enabled_flag=${status_metrics_enabled_flag}"

  status_metrics_prometheus_url_flag=
  if [[ -n "${ovn_status_metrics_prometheus_url}" ]]; then
      status_metrics_prometheus_url_flag="--metrics-status-prometheus-url=${ovn_status_metrics_prometheus_url}"
  fi
  echo "status_metrics_prometheus_url_flag=${status_metrics_prometheus_url_flag}"

  ovn_enable_dnsnameresolver_flag=
  if [[ ${ovn_enable_dnsnameresolver} == "true" ]]; then
	  ovn_enable_dnsnameresolver_flag="--enable-dns-name-resolver"
  fi
  echo "ovn_enable_dnsnameresolver_flag=${ovn_enable_dnsnameresolver_flag}"

  ovn_allow_icmp_netpol_flag=
  if [[ ${ovn_allow_icmp_netpol} == "true" ]]; then
	  ovn_allow_icmp_netpol_flag="--allow-icmp-network-policy"
  fi
  echo "ovn_allow_icmp_netpol_flag=${ovn_allow_icmp_netpol_flag}"

  ovn_observ_enable_flag=
  if [[ ${ovn_observ_enable} == "true" ]]; then
    ovn_observ_enable_flag="--enable-observability"
  fi
  echo "ovn_observ_enable_flag=${ovn_observ_enable_flag}"


  ovn_stateless_netpol_enable_flag=
  if [[ ${ovn_stateless_netpol_enable} == "true" ]]; then
          ovn_stateless_netpol_enable_flag="--enable-stateless-netpol"
  fi
  echo "ovn_stateless_netpol_enable_flag: ${ovn_stateless_netpol_enable_flag}"

  echo "=============== ovnkube-controller =========="
  /usr/bin/ovnkube --init-ovnkube-controller ${K8S_NODE} \
    ${anp_enabled_flag} \
    ${disable_snat_multiple_gws_flag} \
    ${egressfirewall_enabled_flag} \
    ${egressip_enabled_flag} \
    ${egressip_healthcheck_port_flag} \
    ${egressqos_enabled_flag} \
    ${egressservice_enabled_flag} \
    ${empty_lb_events_flag} \
    ${hybrid_overlay_flags} \
    ${libovsdb_client_logfile_flag} \
    ${multicast_enabled_flag} \
    ${multi_network_enabled_flag} \
    ${network_segmentation_enabled_flag} \
    ${network_connect_enabled_flag} \
    ${uplink_enabled_flag} \
    ${pre_conf_udn_addr_enable_flag} \
    ${route_advertisements_enabled_flag} \
    ${evpn_enabled_flag} \
    ${advertised_udn_isolation_flag} \
    ${ovnkube_config_file_flag} \
    ${ovn_acl_logging_rate_limit_flag} \
    ${ovn_enable_svc_template_support_flag} \
    ${ovn_observ_enable_flag} \
    ${ovnkube_config_duration_enable_flag} \
    ${ovnkube_local_cert_flags} \
    ${ovnkube_enable_multi_external_gateway_flag} \
    ${ovnkube_metrics_scale_enable_flag} \
    ${ovnkube_metrics_tls_opts} \
    ${ovn_encap_port_flag} \
    ${ovn_v4_join_subnet_opt} \
    ${ovn_v4_masquerade_subnet_opt} \
    ${ovn_v6_join_subnet_opt} \
    ${ovn_v6_masquerade_subnet_opt} \
    ${network_qos_enabled_flag} \
    ${status_metrics_enabled_flag} \
    ${status_metrics_prometheus_url_flag} \
    ${ovn_enable_dnsnameresolver_flag} \
    ${dynamic_udn_allocation_flag} \
    ${dynamic_udn_grace_period} \
    ${ovnkube_cluster_default_nad_flag} \
    ${ovn_allow_icmp_netpol_flag} \
    --cluster-subnets ${net_cidr} --k8s-service-cidr=${svc_cidr} \
    --gateway-mode=${ovn_gateway_mode} \
    --host-network-namespace ${ovn_host_network_namespace} \
    --logfile-maxage=${ovnkube_logfile_maxage} \
    --logfile-maxbackups=${ovnkube_logfile_maxbackups} \
    --logfile-maxsize=${ovnkube_logfile_maxsize} \
    --logfile /var/log/ovn-kubernetes/ovnkube-controller.log \
    --loglevel=${ovnkube_loglevel} \
    --metrics-bind-address ${ovnkube_controller_metrics_bind_address} \
    --metrics-enable-pprof \
    --ovn-config-namespace ${ovn_kubernetes_namespace} \
    --pidfile ${OVN_RUNDIR}/ovnkube-controller.pid &

  echo "=============== ovnkube-controller ========== running"
  wait_for_event attempts=3 process_ready ovnkube-controller

  process_healthy ovnkube-controller
  exit 9
}

ovnkube-controller-with-node() {
  # send sig term to background job (ovnkube-node process), remove CNI conf and resume background job until it ends.
  # currently we the process to background, therefore wait until that process removes its pid file on exit.
  # if the pid file doesnt exist, we exit immediately.
  trap 'kill $(jobs -p) ; rm -f /etc/cni/net.d/10-ovn-kubernetes.conf ; wait_ovnkube_controller_with_node_done; exit 0' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovnkube-controller-with-node.pid

  # wait for ovs-servers to start since ovnkube initialization sets some fields in OVS DB
  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
    echo "=============== ovnkube-controller-with-node - (wait for ovs)"
    wait_for_event ovs_ready
  fi

  echo "=============== ovnkube-controller-with-node (wait for local NB/SB) =========="
  wait_for_event local_dbs_ready

  # wait for northd to start
  wait_for_event process_ready ovn-northd

  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
    echo "=============== ovnkube-controller-with-node - (ovn-node  wait for ovn-controller.pid)"
    wait_for_event process_ready ovn-controller
  fi

  # start temp work around
  # remove when https://issues.redhat.com/browse/FDP-1537 is avilable
  if [[ ${ovnkube_node_mode} == "full" && ${ovn_egressip_enable} == "true" ]]; then
    echo "=============== ovnkube-controller-with-node - (add GARP drop flows if external bridge exists)"
    # bridge may not yet exist
    local bridge_name="$(get_bridge_name_for_physnet 'physnet')"
    if [[ "$bridge_name" != "" ]]; then
      echo "=============== ovnkube-controller-with-node - found bridge mapping for physnet: $bridge_name"
      # nothing to do if the external bridge isn't created.
      if ovs-vsctl br-exists $bridge_name; then
        echo "=============== ovnkube-controller-with-node - found bridge $bridge_name"
        add_garp_drop_flow "$bridge_name"
        echo "=============== ovnkube-controller-with-node - (finished adding GARP drop flows)"
      fi
    fi
  fi

  ovn_routable_mtu_flag=
  if [[ -n "${routable_mtu}" ]]; then
    routable_mtu_flag="--routable-mtu ${routable_mtu}"
  fi

  hybrid_overlay_flags=
  if [[ ${ovn_hybrid_overlay_enable} == "true" ]]; then
    hybrid_overlay_flags="--enable-hybrid-overlay"
    if [[ -n "${ovn_hybrid_overlay_net_cidr}" ]]; then
      hybrid_overlay_flags="${hybrid_overlay_flags} --hybrid-overlay-cluster-subnets=${ovn_hybrid_overlay_net_cidr}"
    fi
  fi
  echo "hybrid_overlay_flags=${hybrid_overlay_flags}"

  disable_snat_multiple_gws_flag=
  if [[ ${ovn_disable_snat_multiple_gws} == "true" ]]; then
      disable_snat_multiple_gws_flag="--disable-snat-multiple-gws"
  fi
  echo "disable_snat_multiple_gws_flag=${disable_snat_multiple_gws_flag}"

  disable_forwarding_flag=
  if [[ ${ovn_disable_forwarding} == "true" ]]; then
      disable_forwarding_flag="--disable-forwarding"
  fi

  ovn_encap_port_flag=
  if [[ -n "${ovn_encap_port}" ]]; then
      ovn_encap_port_flag="--encap-port=${ovn_encap_port}"
  fi
  echo "ovn_encap_port_flag=${ovn_encap_port_flag}"

  disable_pkt_mtu_check_flag=
  if [[ ${ovn_disable_pkt_mtu_check} == "true" ]]; then
      disable_pkt_mtu_check_flag="--disable-pkt-mtu-check"
  fi
  echo "disable_pkt_mtu_check_flag=${disable_pkt_mtu_check_flag}"

  empty_lb_events_flag=
  if [[ ${ovn_empty_lb_events} == "true" ]]; then
      empty_lb_events_flag="--ovn-empty-lb-events"
  fi
  echo "empty_lb_events_flag=${empty_lb_events_flag}"

  ovn_v4_join_subnet_opt=
  if [[ -n ${ovn_v4_join_subnet} ]]; then
      ovn_v4_join_subnet_opt="--gateway-v4-join-subnet=${ovn_v4_join_subnet}"
  fi
  echo "ovn_v4_join_subnet_opt=${ovn_v4_join_subnet_opt}"

  ovn_v6_join_subnet_opt=
  if [[ -n ${ovn_v6_join_subnet} ]]; then
      ovn_v6_join_subnet_opt="--gateway-v6-join-subnet=${ovn_v6_join_subnet}"
  fi
  echo "ovn_v6_join_subnet_opt=${ovn_v6_join_subnet_opt}"

  ovn_acl_logging_rate_limit_flag=
  if [[ -n ${ovn_acl_logging_rate_limit} ]]; then
      ovn_acl_logging_rate_limit_flag="--acl-logging-rate-limit ${ovn_acl_logging_rate_limit}"
  fi
  echo "ovn_acl_logging_rate_limit_flag=${ovn_acl_logging_rate_limit_flag}"

  multicast_enabled_flag=
  if [[ ${ovn_multicast_enable} == "true" ]]; then
      multicast_enabled_flag="--enable-multicast"
  fi
  echo "multicast_enabled_flag=${multicast_enabled_flag}"

  egressip_enabled_flag=
  if [[ ${ovn_egressip_enable} == "true" ]]; then
      egressip_enabled_flag="--enable-egress-ip"
  fi
  echo "egressip_enabled_flag=${egressip_enabled_flag}"

  egressip_healthcheck_port_flag=
  if [[ -n "${ovn_egress_ip_healthcheck_port}" ]]; then
      egressip_healthcheck_port_flag="--egressip-node-healthcheck-port=${ovn_egress_ip_healthcheck_port}"
  fi
  echo "egressip_healthcheck_port_flag=${egressip_healthcheck_port_flag}"

  egressfirewall_enabled_flag=
  if [[ ${ovn_egressfirewall_enable} == "true" ]]; then
	  egressfirewall_enabled_flag="--enable-egress-firewall"
  fi
  echo "egressfirewall_enabled_flag=${egressfirewall_enabled_flag}"

  egressqos_enabled_flag=
  if [[ ${ovn_egressqos_enable} == "true" ]]; then
	  egressqos_enabled_flag="--enable-egress-qos"
  fi
  echo "egressqos_enabled_flag=${egressqos_enabled_flag}"

  multi_network_enabled_flag=
  if [[ ${ovn_multi_network_enable} == "true" ]]; then
	  multi_network_enabled_flag="--enable-multi-network --enable-multi-networkpolicy"
  fi
  echo "multi_network_enabled_flag=${multi_network_enabled_flag}"

  network_segmentation_enabled_flag=
  if [[ ${ovn_network_segmentation_enable} == "true" ]]; then
	  network_segmentation_enabled_flag="--enable-multi-network --enable-network-segmentation"
  fi
  echo "network_segmentation_enabled_flag=${network_segmentation_enabled_flag}"

  network_connect_enabled_flag=
  if [[ ${ovn_network_connect_enable} == "true" ]]; then
	  network_connect_enabled_flag="--enable-network-connect"
  fi
  echo "network_connect_enabled_flag=${network_connect_enabled_flag}"

  uplink_enabled_flag=
  if [[ ${ovn_uplink_enable} == "true" ]]; then
	  uplink_enabled_flag="--enable-uplink"
  fi
  echo "uplink_enabled_flag=${uplink_enabled_flag}"

  pre_conf_udn_addr_enable_flag=
  if [[ ${ovn_pre_conf_udn_addr_enable} == "true" ]]; then
	  pre_conf_udn_addr_enable_flag="--enable-preconfigured-udn-addresses"
  fi
  echo "pre_conf_udn_addr_enable_flag=${pre_conf_udn_addr_enable_flag}"

  route_advertisements_enabled_flag=
  if [[ ${ovn_route_advertisements_enable} == "true" ]]; then
	  route_advertisements_enabled_flag="--enable-route-advertisements"
  fi
  echo "route_advertisements_enabled_flag=${route_advertisements_enabled_flag}"

  evpn_enabled_flag=
  if [[ ${ovn_evpn_enable} == "true" ]]; then
	  evpn_enabled_flag="--enable-evpn"
  fi
  echo "evpn_enabled_flag=${evpn_enabled_flag}"

  advertised_udn_isolation_flag=
  if [[ -n ${ovn_advertised_udn_isolation_mode} ]]; then
      advertised_udn_isolation_flag="--advertised-udn-isolation-mode=${ovn_advertised_udn_isolation_mode}"
  fi
  echo "advertised_udn_isolation_flag=${advertised_udn_isolation_flag}"

  ovnkube_config_file_flag="--config-file=/run/ovnkube-config/ovnkube.conf"
  echo "ovnkube_config_file_flag=${ovnkube_config_file_flag}"

  egressservice_enabled_flag=
  if [[ ${ovn_egressservice_enable} == "true" ]]; then
	  egressservice_enabled_flag="--enable-egress-service"
  fi
  echo "egressservice_enabled_flag=${egressservice_enabled_flag}"

  netflow_targets=
  if [[ -n ${ovn_netflow_targets} ]]; then
      netflow_targets="--netflow-targets ${ovn_netflow_targets}"
  fi

  sflow_targets=
  if [[ -n ${ovn_sflow_targets} ]]; then
      sflow_targets="--sflow-targets ${ovn_sflow_targets}"
  fi

  ipfix_targets=
  if [[ -n ${ovn_ipfix_targets} ]]; then
      ipfix_targets="--ipfix-targets ${ovn_ipfix_targets}"
  fi

  ipfix_config=
  if [[ -n ${ovn_ipfix_sampling} ]]; then
      ipfix_config="--ipfix-sampling ${ovn_ipfix_sampling}"
  fi
  if [[ -n ${ovn_ipfix_cache_max_flows} ]]; then
      ipfix_config="${ipfix_config} --ipfix-cache-max-flows ${ovn_ipfix_cache_max_flows}"
  fi
  if [[ -n ${ovn_ipfix_cache_active_timeout} ]]; then
      ipfix_config="${ipfix_config} --ipfix-cache-active-timeout ${ovn_ipfix_cache_active_timeout}"
  fi

  monitor_all=
  if [[ -n ${ovn_monitor_all} ]]; then
     monitor_all="--monitor-all=${ovn_monitor_all}"
  fi

  ofctrl_wait_before_clear=
  if [[ -n ${ovn_ofctrl_wait_before_clear} ]]; then
     ofctrl_wait_before_clear="--ofctrl-wait-before-clear=${ovn_ofctrl_wait_before_clear}"
  fi

  enable_lflow_cache=
  if [[ -n ${ovn_enable_lflow_cache} ]]; then
     enable_lflow_cache="--enable-lflow-cache=${ovn_enable_lflow_cache}"
  fi

  lflow_cache_limit=
  if [[ -n ${ovn_lflow_cache_limit} ]]; then
     lflow_cache_limit="--lflow-cache-limit=${ovn_lflow_cache_limit}"
  fi

  lflow_cache_limit_kb=
  if [[ -n ${ovn_lflow_cache_limit_kb} ]]; then
     lflow_cache_limit_kb="--lflow-cache-limit-kb=${ovn_lflow_cache_limit_kb}"
  fi

  egress_interface=
  if [[ -n ${ovn_ex_gw_network_interface} ]]; then
      egress_interface="--exgw-interface ${ovn_ex_gw_network_interface}"
  fi

  ovn_encap_ip_flag=
  if [[ ${ovn_encap_ip} != "" ]]; then
    ovn_encap_ip_flag="--encap-ip=${ovn_encap_ip}"
  else
    ovn_encap_ip=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-encap-ip)
    if [[ $? == 0 ]]; then
      ovn_encap_ip=$(echo ${ovn_encap_ip} | tr -d '\"')
      if [[ "${ovn_encap_ip}" != "" ]]; then
        ovn_encap_ip_flag="--encap-ip=${ovn_encap_ip}"
      fi
    fi
  fi

  ovnkube_node_mode_flag=
  if [[ ${ovnkube_node_mode} != "" ]]; then
    ovnkube_node_mode_flag="--ovnkube-node-mode=${ovnkube_node_mode}"
    if [[ ${ovnkube_node_mode} == "dpu" ]]; then
      # encap IP is required for dpu, this is either provided via OVN_ENCAP_IP env variable or taken from ovs
      if [[ ${ovn_encap_ip} == "" ]]; then
        echo "ovn encap IP must be provided if \"ovnkube-node-mode\" set to \"dpu\". Exiting..."
        exit 1
      fi
    fi
  fi

  # Get gateway options from OVS for full and dpu modes.
  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
      get_gw_options
  fi

  ovnkube_node_mgmt_port_netdev_flag=
  if [[ ${ovnkube_node_mgmt_port_netdev} != "" ]]; then
    ovnkube_node_mgmt_port_netdev_flag="--ovnkube-node-mgmt-port-netdev=${ovnkube_node_mgmt_port_netdev}"
  fi
  if [[ -n "${ovnkube_node_mgmt_port_dp_resource_name}" ]] ; then
    ovnkube_node_mgmt_port_netdev_flag="--ovnkube-node-mgmt-port-dp-resource-name=${ovnkube_node_mgmt_port_dp_resource_name}"
  fi

  ovn_simulate_dpu_flag=
  if [[ ${ovn_simulate_dpu} == "true" ]]; then
    ovn_simulate_dpu_flag="--simulate-dpu"
  fi

  ovn_dpu_host_gateway_representor_interface_flag=
  if [[ ${ovn_dpu_host_gateway_representor_interface} != "" ]]; then
    ovn_dpu_host_gateway_representor_interface_flag="--dpu-host-gateway-representor-interface=${ovn_dpu_host_gateway_representor_interface}"
  fi

  ovn_unprivileged_flag="--unprivileged-mode"
  if test -z "${OVN_UNPRIVILEGED_MODE+x}" -o "x${OVN_UNPRIVILEGED_MODE}" = xno; then
    ovn_unprivileged_flag=""
  fi

  ovn_metrics_bind_address="${metrics_endpoint_ip}:${metrics_bind_port}"
  metrics_bind_address="${metrics_endpoint_ip}:${metrics_worker_port}"
  echo "ovn_metrics_bind_address=${ovn_metrics_bind_address}"
  echo "metrics_bind_address=${metrics_bind_address}"

  local ovnkube_metrics_tls_opts=""
  if [[ ${OVNKUBE_METRICS_PK} != "" && ${OVNKUBE_METRICS_CERT} != "" ]]; then
    ovnkube_metrics_tls_opts="
        --node-server-privkey ${OVNKUBE_METRICS_PK}
        --node-server-cert ${OVNKUBE_METRICS_CERT}
      "
  fi
  echo "ovnkube_metrics_tls_opts=${ovnkube_metrics_tls_opts}"

  ovnkube_config_duration_enable_flag=
  if [[ ${ovnkube_config_duration_enable} == "true" ]]; then
    ovnkube_config_duration_enable_flag="--metrics-enable-config-duration"
  fi
  echo "ovnkube_config_duration_enable_flag: ${ovnkube_config_duration_enable_flag}"

  ovnkube_enable_multi_external_gateway_flag=
  if [[ ${ovn_enable_multi_external_gateway} == "true" ]]; then
	  ovnkube_enable_multi_external_gateway_flag="--enable-multi-external-gateway"
  fi
  echo "ovnkube_enable_multi_external_gateway_flag=${ovnkube_enable_multi_external_gateway_flag}"

  libovsdb_client_logfile_flag=
  if [[ -n ${ovnkube_libovsdb_client_logfile} ]]; then
      libovsdb_client_logfile_flag="--libovsdblogfile ${ovnkube_libovsdb_client_logfile}"
  fi

  anp_enabled_flag=
  if [[ ${ovn_admin_network_policy_enable} == "true" ]]; then
      anp_enabled_flag="--enable-admin-network-policy"
  fi
  echo "anp_enabled_flag=${anp_enabled_flag}"

  ovn_v4_masquerade_subnet_opt=
  if [[ -n ${ovn_v4_masquerade_subnet} ]]; then
      ovn_v4_masquerade_subnet_opt="--gateway-v4-masquerade-subnet=${ovn_v4_masquerade_subnet}"
  fi
  echo "ovn_v4_masquerade_subnet_opt=${ovn_v4_masquerade_subnet_opt}"

  ovn_v6_masquerade_subnet_opt=
  if [[ -n ${ovn_v6_masquerade_subnet} ]]; then
      ovn_v6_masquerade_subnet_opt="--gateway-v6-masquerade-subnet=${ovn_v6_masquerade_subnet}"
  fi
  echo "ovn_v6_masquerade_subnet_opt=${ovn_v6_masquerade_subnet_opt}"

  ovnkube_metrics_scale_enable_flag=
  if [[ ${ovnkube_metrics_scale_enable} == "true" ]]; then
    ovnkube_metrics_scale_enable_flag="--metrics-enable-scale --metrics-enable-pprof"
  fi
  echo "ovnkube_metrics_scale_enable_flag: ${ovnkube_metrics_scale_enable_flag}"

  ovnkube_local_cert_flags=
  if [[ ${ovn_enable_ovnkube_identity} == "true" ]]; then
    bootstrap_kubeconfig="/host-kubernetes/kubelet.conf"
    if [ -f "${bootstrap_kubeconfig}" ]; then
      ovnkube_local_cert_flags="
        --bootstrap-kubeconfig ${bootstrap_kubeconfig}
        --cert-dir /var/run/ovn-kubernetes/certs
      "
    else
      echo "bootstrap kubeconfig file: ${bootstrap_kubeconfig} doesn't exist,
       skipping bootstrap-kubeconfig/cert-dir parameters"
    fi
  fi
  echo "ovnkube_local_cert_flags=${ovnkube_local_cert_flags}"

  ovn_enable_svc_template_support_flag=
  if [[ ${ovn_enable_svc_template_support} == "true" ]]; then
	  ovn_enable_svc_template_support_flag="--enable-svc-template-support"
  fi
  echo "ovn_enable_svc_template_support_flag=${ovn_enable_svc_template_support_flag}"

  network_qos_enabled_flag=
  if [[ ${ovn_network_qos_enable} == "true" ]]; then
      network_qos_enabled_flag="--enable-network-qos"
  fi
  echo "network_qos_enabled_flag=${network_qos_enabled_flag}"

  status_metrics_enabled_flag=
  if [[ ${ovn_enable_status_metrics} == "true" ]]; then
      status_metrics_enabled_flag="--enable-status-metrics"
  fi
  echo "status_metrics_enabled_flag=${status_metrics_enabled_flag}"

  status_metrics_prometheus_url_flag=
  if [[ -n "${ovn_status_metrics_prometheus_url}" ]]; then
      status_metrics_prometheus_url_flag="--metrics-status-prometheus-url=${ovn_status_metrics_prometheus_url}"
  fi
  echo "status_metrics_prometheus_url_flag=${status_metrics_prometheus_url_flag}"

  ovn_enable_dnsnameresolver_flag=
  if [[ ${ovn_enable_dnsnameresolver} == "true" ]]; then
	  ovn_enable_dnsnameresolver_flag="--enable-dns-name-resolver"
  fi
  echo "ovn_enable_dnsnameresolver_flag=${ovn_enable_dnsnameresolver_flag}"

  ovn_allow_icmp_netpol_flag=
  if [[ ${ovn_allow_icmp_netpol} == "true" ]]; then
	  ovn_allow_icmp_netpol_flag="--allow-icmp-network-policy"
  fi
  echo "ovn_allow_icmp_netpol_flag=${ovn_allow_icmp_netpol_flag}"

  ovn_observ_enable_flag=
  if [[ ${ovn_observ_enable} == "true" ]]; then
    ovn_observ_enable_flag="--enable-observability"
  fi
  echo "ovn_observ_enable_flag=${ovn_observ_enable_flag}"

  ovn_stateless_netpol_enable_flag=
  if [[ ${ovn_stateless_netpol_enable} == "true" ]]; then
          ovn_stateless_netpol_enable_flag="--enable-stateless-netpol"
  fi

  dynamic_udn_allocation_flag=
  if [[ ${ovn_enable_dynamic_udn_allocation} == "true" ]]; then
    dynamic_udn_allocation_flag="--enable-dynamic-udn-allocation"
  fi
  echo "dynamic_udn_allocation_flag=${dynamic_udn_allocation_flag}"

  dynamic_udn_grace_period=
  if [[ -n ${ovn_dynamic_udn_grace_period} ]]; then
    dynamic_udn_grace_period="--udn-deletion-grace-period ${ovn_dynamic_udn_grace_period}"
  fi
  echo "dynamic_udn_grace_period=${dynamic_udn_grace_period}"

  ovn_disable_requestedchassis_flag=
  if [[ ${ovn_disable_requestedchassis} == "true" ]]; then
          ovn_disable_requestedchassis_flag="--disable-requestedchassis"
  fi
  echo "ovn_disable_requestedchassis_flag=${ovn_disable_requestedchassis_flag}"

  # NB client cert material is only consumed by the Egress IP gRPC
  # health-check channel (see pkg/ovn/healthcheck/egressip_healthcheck.go).
  # The OVN NB/SB DB connections themselves are unix-socket only.
  local ssl_opts=""
  [[ "yes" == ${OVN_SSL_ENABLE} ]] && {
    ssl_opts="
        --nb-client-privkey ${ovn_controller_pk}
        --nb-client-cert ${ovn_controller_cert}
        --nb-client-cacert ${ovn_ca_cert}
        --nb-cert-common-name ${ovn_controller_cname}
      "
  }
  echo "ssl_opts=${ssl_opts}"

  # Pass DPU Host cluster access credentials provided via environment variables in case of DPU
  cluster_access_opts=""
  if [[ ${ovnkube_node_mode} == "dpu" ]]; then
    [[ -n ${K8S_APISERVER} ]] && cluster_access_opts+="--k8s-apiserver=${K8S_APISERVER} "
    [[ -n ${K8S_TOKEN} ]] && cluster_access_opts+="--k8s-token=${K8S_TOKEN} "
    [[ -n ${K8S_TOKEN_FILE} ]] && cluster_access_opts+="--k8s-token-file=${K8S_TOKEN_FILE} "
    [[ -n ${K8S_CACERT_DATA} ]] && cluster_access_opts+="--k8s-cacert-data=${K8S_CACERT_DATA} "
    [[ -n ${K8S_CACERT} ]] && cluster_access_opts+="--k8s-cacert=${K8S_CACERT} "
  fi

  echo "=============== ovnkube-controller-with-node --init-ovnkube-controller-with-node=========="
  /usr/bin/ovnkube --init-ovnkube-controller ${K8S_NODE} --init-node ${K8S_NODE} \
    ${anp_enabled_flag} \
    ${disable_forwarding_flag} \
    ${disable_pkt_mtu_check_flag} \
    ${disable_snat_multiple_gws_flag} \
    ${egressfirewall_enabled_flag} \
    ${egress_interface} \
    ${egressip_enabled_flag} \
    ${egressip_healthcheck_port_flag} \
    ${egressqos_enabled_flag} \
    ${egressservice_enabled_flag} \
    ${empty_lb_events_flag} \
    ${enable_lflow_cache} \
    ${hybrid_overlay_flags} \
    ${ipfix_config} \
    ${ipfix_targets} \
    ${libovsdb_client_logfile_flag} \
    ${lflow_cache_limit} \
    ${lflow_cache_limit_kb} \
    ${monitor_all} \
    ${multicast_enabled_flag} \
    ${multi_network_enabled_flag} \
    ${network_segmentation_enabled_flag} \
    ${network_connect_enabled_flag} \
    ${uplink_enabled_flag} \
    ${pre_conf_udn_addr_enable_flag} \
    ${route_advertisements_enabled_flag} \
    ${evpn_enabled_flag} \
    ${advertised_udn_isolation_flag} \
    ${ovnkube_config_file_flag} \
    ${netflow_targets} \
    ${ofctrl_wait_before_clear} \
    ${ovn_acl_logging_rate_limit_flag} \
    ${ovn_enable_svc_template_support_flag} \
    ${ovn_observ_enable_flag} \
    ${ovn_encap_ip_flag} \
    ${ovn_encap_port_flag} \
    ${ovnkube_config_duration_enable_flag} \
    ${ovnkube_local_cert_flags} \
    ${ovnkube_enable_multi_external_gateway_flag} \
    ${ovnkube_metrics_scale_enable_flag} \
    ${ovnkube_metrics_tls_opts} \
    ${ovnkube_node_mgmt_port_netdev_flag} \
    ${ovnkube_node_mode_flag} \
    ${ovn_simulate_dpu_flag} \
    ${ovn_dpu_host_gateway_representor_interface_flag} \
    ${ssl_opts} \
    ${ovn_unprivileged_flag} \
    ${ovn_v4_join_subnet_opt} \
    ${ovn_v4_masquerade_subnet_opt} \
    ${ovn_v6_join_subnet_opt} \
    ${ovn_v6_masquerade_subnet_opt} \
    ${routable_mtu_flag} \
    ${sflow_targets} \
    ${dynamic_udn_allocation_flag} \
    ${dynamic_udn_grace_period} \
    ${ovnkube_cluster_default_nad_flag} \
    ${network_qos_enabled_flag} \
    ${status_metrics_enabled_flag} \
    ${status_metrics_prometheus_url_flag} \
    ${ovn_enable_dnsnameresolver_flag} \
    ${ovn_disable_requestedchassis_flag} \
    ${cluster_access_opts} \
    ${ovn_allow_icmp_netpol_flag} \
    --cluster-subnets ${net_cidr} --k8s-service-cidr=${svc_cidr} \
    --export-ovs-metrics \
    --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts} \
    --gateway-router-subnet=${ovn_gateway_router_subnet} \
    --host-network-namespace ${ovn_host_network_namespace} \
    --inactivity-probe=${ovn_remote_probe_interval} \
    --logfile-maxage=${ovnkube_logfile_maxage} \
    --logfile-maxbackups=${ovnkube_logfile_maxbackups} \
    --logfile-maxsize=${ovnkube_logfile_maxsize} \
    --logfile /var/log/ovn-kubernetes/ovnkube-controller-with-node.log \
    --loglevel=${ovnkube_loglevel} \
    --metrics-bind-address ${metrics_bind_address} \
    --metrics-enable-pprof \
    --mtu=${mtu} \
    --nodeport \
    --ovn-config-namespace ${ovn_kubernetes_namespace} \
    --ovn-metrics-bind-address ${ovn_metrics_bind_address} \
    --pidfile ${OVN_RUNDIR}/ovnkube-controller-with-node.pid &

  wait_for_event attempts=3 process_ready ovnkube-controller-with-node
  if [[ ${ovnkube_node_mode} != "dpu" ]]; then
    setup_cni
  fi
  echo "=============== ovnkube-controller-with-node ========== running"

  process_healthy ovnkube-controller-with-node
  # TODO exit 9 vs 7
  exit 9
}

# run ovnkube --cluster-manager.
ovn-cluster-manager() {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "1.4.0"

  ovn_encap_port_flag=
    if [[ -n "${ovn_encap_port}" ]]; then
      ovn_encap_port_flag="--encap-port=${ovn_encap_port}"
  fi
  echo "ovn_encap_port_flag=${ovn_encap_port_flag}"

  egressip_enabled_flag=
  if [[ ${ovn_egressip_enable} == "true" ]]; then
      egressip_enabled_flag="--enable-egress-ip"
  fi

  egressip_healthcheck_port_flag=
  if [[ -n "${ovn_egress_ip_healthcheck_port}" ]]; then
      egressip_healthcheck_port_flag="--egressip-node-healthcheck-port=${ovn_egress_ip_healthcheck_port}"
  fi
  echo "egressip_flags: ${egressip_enabled_flag}, ${egressip_healthcheck_port_flag}"

  egressservice_enabled_flag=
  if [[ ${ovn_egressservice_enable} == "true" ]]; then
         egressservice_enabled_flag="--enable-egress-service"
  fi
  echo "egressservice_enabled_flag=${egressservice_enabled_flag}"

  anp_enabled_flag=
  if [[ ${ovn_admin_network_policy_enable} == "true" ]]; then
      anp_enabled_flag="--enable-admin-network-policy"
  fi
  echo "anp_enabled_flag=${anp_enabled_flag}"

  egressfirewall_enabled_flag=
  if [[ ${ovn_egressfirewall_enable} == "true" ]]; then
	  egressfirewall_enabled_flag="--enable-egress-firewall"
  fi
  echo "egressfirewall_enabled_flag=${egressfirewall_enabled_flag}"

  egressqos_enabled_flag=
  if [[ ${ovn_egressqos_enable} == "true" ]]; then
	  egressqos_enabled_flag="--enable-egress-qos"
  fi
  echo "egressqos_enabled_flag=${egressqos_enabled_flag}"

  hybrid_overlay_flags=
  if [[ ${ovn_hybrid_overlay_enable} == "true" ]]; then
    hybrid_overlay_flags="--enable-hybrid-overlay"
    if [[ -n "${ovn_hybrid_overlay_net_cidr}" ]]; then
      hybrid_overlay_flags="${hybrid_overlay_flags} --hybrid-overlay-cluster-subnets=${ovn_hybrid_overlay_net_cidr}"
    fi
  fi
  echo "hybrid_overlay_flags: ${hybrid_overlay_flags}"

  ovn_v4_join_subnet_opt=
  if [[ -n ${ovn_v4_join_subnet} ]]; then
      ovn_v4_join_subnet_opt="--gateway-v4-join-subnet=${ovn_v4_join_subnet}"
  fi
  echo "ovn_v4_join_subnet_opt: ${ovn_v4_join_subnet_opt}"

  ovn_v6_join_subnet_opt=
  if [[ -n ${ovn_v6_join_subnet} ]]; then
      ovn_v6_join_subnet_opt="--gateway-v6-join-subnet=${ovn_v6_join_subnet}"
  fi
  echo "ovn_v6_join_subnet_opt: ${ovn_v6_join_subnet_opt}"

   ovn_v4_masquerade_subnet_opt=
  if [[ -n ${ovn_v4_masquerade_subnet} ]]; then
      ovn_v4_masquerade_subnet_opt="--gateway-v4-masquerade-subnet=${ovn_v4_masquerade_subnet}"
  fi
  echo "ovn_v4_masquerade_subnet_opt=${ovn_v4_masquerade_subnet_opt}"

  ovn_v6_masquerade_subnet_opt=
  if [[ -n ${ovn_v6_masquerade_subnet} ]]; then
      ovn_v6_masquerade_subnet_opt="--gateway-v6-masquerade-subnet=${ovn_v6_masquerade_subnet}"
  fi
  echo "ovn_v6_masquerade_subnet_opt=${ovn_v6_masquerade_subnet_opt}"

  ovn_v4_transit_subnet_opt=
  if [[ -n ${ovn_v4_transit_subnet} ]]; then
      ovn_v4_transit_subnet_opt="--cluster-manager-v4-transit-subnet=${ovn_v4_transit_subnet}"
  fi
  echo "ovn_v4_transit_subnet_opt=${ovn_v4_transit_subnet}"

  ovn_v6_transit_subnet_opt=
  if [[ -n ${ovn_v6_transit_subnet} ]]; then
      ovn_v6_transit_subnet_opt="--cluster-manager-v6-transit-subnet=${ovn_v6_transit_subnet}"
  fi
  echo "ovn_v6_transit_subnet_opt=${ovn_v6_transit_subnet}"

  multicast_enabled_flag=
  if [[ ${ovn_multicast_enable} == "true" ]]; then
      multicast_enabled_flag="--enable-multicast"
  fi
  echo "multicast_enabled_flag: ${multicast_enabled_flag}"

  multi_network_enabled_flag=
  if [[ ${ovn_multi_network_enable} == "true" ]]; then
	  multi_network_enabled_flag="--enable-multi-network --enable-multi-networkpolicy"
  fi
  echo "multi_network_enabled_flag: ${multi_network_enabled_flag}"

  network_segmentation_enabled_flag=
  if [[ ${ovn_network_segmentation_enable} == "true" ]]; then
	  network_segmentation_enabled_flag="--enable-multi-network --enable-network-segmentation"
  fi
  echo "network_segmentation_enabled_flag=${network_segmentation_enabled_flag}"

  network_connect_enabled_flag=
  if [[ ${ovn_network_connect_enable} == "true" ]]; then
	  network_connect_enabled_flag="--enable-network-connect"
  fi
  echo "network_connect_enabled_flag=${network_connect_enabled_flag}"

  uplink_enabled_flag=
  if [[ ${ovn_uplink_enable} == "true" ]]; then
	  uplink_enabled_flag="--enable-uplink"
  fi
  echo "uplink_enabled_flag=${uplink_enabled_flag}"

  pre_conf_udn_addr_enable_flag=
  if [[ ${ovn_pre_conf_udn_addr_enable} == "true" ]]; then
	  pre_conf_udn_addr_enable_flag="--enable-preconfigured-udn-addresses"
  fi
  echo "pre_conf_udn_addr_enable_flag=${pre_conf_udn_addr_enable_flag}"

  route_advertisements_enabled_flag=
  if [[ ${ovn_route_advertisements_enable} == "true" ]]; then
	  route_advertisements_enabled_flag="--enable-route-advertisements"
  fi
  echo "route_advertisements_enabled_flag=${route_advertisements_enabled_flag}"

  evpn_enabled_flag=
  if [[ ${ovn_evpn_enable} == "true" ]]; then
	  evpn_enabled_flag="--enable-evpn"
  fi
  echo "evpn_enabled_flag=${evpn_enabled_flag}"

  advertised_udn_isolation_flag=
  if [[ -n ${ovn_advertised_udn_isolation_mode} ]]; then
      advertised_udn_isolation_flag="--advertised-udn-isolation-mode=${ovn_advertised_udn_isolation_mode}"
  fi

  ovnkube_config_file_flag="--config-file=/run/ovnkube-config/ovnkube.conf"
  echo "ovnkube_config_file_flag=${ovnkube_config_file_flag}"

  persistent_ips_enabled_flag=
  if [[ ${ovn_enable_persistent_ips} == "true" ]]; then
	  persistent_ips_enabled_flag="--enable-persistent-ips"
  fi
  echo "persistent_ips_enabled_flag: ${persistent_ips_enabled_flag}"

  ovnkube_cluster_manager_metrics_bind_address="${metrics_endpoint_ip}:9411"
  echo "ovnkube_cluster_manager_metrics_bind_address: ${ovnkube_cluster_manager_metrics_bind_address}"

  local ovnkube_metrics_tls_opts=""
  if [[ ${OVNKUBE_METRICS_PK} != "" && ${OVNKUBE_METRICS_CERT} != "" ]]; then
    ovnkube_metrics_tls_opts="
        --node-server-privkey ${OVNKUBE_METRICS_PK}
        --node-server-cert ${OVNKUBE_METRICS_CERT}
      "
  fi
  echo "ovnkube_metrics_tls_opts: ${ovnkube_metrics_tls_opts}"

  ovnkube_enable_multi_external_gateway_flag=
  if [[ ${ovn_enable_multi_external_gateway} == "true" ]]; then
	  ovnkube_enable_multi_external_gateway_flag="--enable-multi-external-gateway"
  fi
  echo "ovnkube_enable_multi_external_gateway_flag=${ovnkube_enable_multi_external_gateway_flag}"

  empty_lb_events_flag=
  if [[ ${ovn_empty_lb_events} == "true" ]]; then
      empty_lb_events_flag="--ovn-empty-lb-events"
  fi
  echo "empty_lb_events_flag=${empty_lb_events_flag}"

  network_qos_enabled_flag=
  if [[ ${ovn_network_qos_enable} == "true" ]]; then
      network_qos_enabled_flag="--enable-network-qos"
  fi
  echo "network_qos_enabled_flag=${network_qos_enabled_flag}"

  status_metrics_enabled_flag=
  if [[ ${ovn_enable_status_metrics} == "true" ]]; then
      status_metrics_enabled_flag="--enable-status-metrics"
  fi
  echo "status_metrics_enabled_flag=${status_metrics_enabled_flag}"

  status_metrics_prometheus_url_flag=
  if [[ -n "${ovn_status_metrics_prometheus_url}" ]]; then
      status_metrics_prometheus_url_flag="--metrics-status-prometheus-url=${ovn_status_metrics_prometheus_url}"
  fi
  echo "status_metrics_prometheus_url_flag=${status_metrics_prometheus_url_flag}"

  ovn_enable_dnsnameresolver_flag=
  if [[ ${ovn_enable_dnsnameresolver} == "true" ]]; then
	  ovn_enable_dnsnameresolver_flag="--enable-dns-name-resolver"
  fi
  echo "ovn_enable_dnsnameresolver_flag=${ovn_enable_dnsnameresolver_flag}"

  dynamic_udn_allocation_flag=
  if [[ ${ovn_enable_dynamic_udn_allocation} == "true" ]]; then
    dynamic_udn_allocation_flag="--enable-dynamic-udn-allocation"
  fi
  echo "dynamic_udn_allocation_flag=${dynamic_udn_allocation_flag}"

  dynamic_udn_grace_period=
  if [[ -n ${ovn_dynamic_udn_grace_period} ]]; then
    dynamic_udn_grace_period="--udn-deletion-grace-period ${ovn_dynamic_udn_grace_period}"
  fi
  echo "dynamic_udn_grace_period=${dynamic_udn_grace_period}"

  ovn_allow_icmp_netpol_flag=
  if [[ ${ovn_allow_icmp_netpol} == "true" ]]; then
	  ovn_allow_icmp_netpol_flag="--allow-icmp-network-policy"
  fi
  echo "ovn_allow_icmp_netpol_flag=${ovn_allow_icmp_netpol_flag}"

  # NB client cert material is only consumed by the Egress IP gRPC
  # health-check channel (see pkg/ovn/healthcheck/egressip_healthcheck.go).
  # The cluster-manager is the gRPC client; the OVN NB/SB DB connections
  # themselves are unix-socket only.
  local ovn_cluster_manager_ssl_opts=""
  [[ "yes" == ${OVN_SSL_ENABLE} ]] && {
    ovn_cluster_manager_ssl_opts="
        --nb-client-privkey ${ovn_controller_pk}
        --nb-client-cert ${ovn_controller_cert}
        --nb-client-cacert ${ovn_ca_cert}
        --nb-cert-common-name ${ovn_controller_cname}
      "
  }
  echo "ovn_cluster_manager_ssl_opts=${ovn_cluster_manager_ssl_opts}"

  ovnkube_metrics_scale_enable_flag=
  if [[ ${ovnkube_metrics_scale_enable} == "true" ]]; then
    ovnkube_metrics_scale_enable_flag="--metrics-enable-scale"
  fi
  echo "ovnkube_metrics_scale_enable_flag: ${ovnkube_metrics_scale_enable_flag}"

  rm -f ${OVN_RUNDIR}/ovnkube-cluster-manager.pid

  echo "=============== ovn-cluster-manager ========== control plane node only"

  /usr/bin/ovnkube --init-cluster-manager ${K8S_NODE} \
    ${anp_enabled_flag} \
    ${egressfirewall_enabled_flag} \
    ${egressip_enabled_flag} \
    ${egressip_healthcheck_port_flag} \
    ${egressqos_enabled_flag} \
    ${egressservice_enabled_flag} \
    ${empty_lb_events_flag} \
    ${hybrid_overlay_flags} \
    ${multicast_enabled_flag} \
    ${multi_network_enabled_flag} \
    ${network_segmentation_enabled_flag} \
    ${network_connect_enabled_flag} \
    ${uplink_enabled_flag} \
    ${pre_conf_udn_addr_enable_flag} \
    ${route_advertisements_enabled_flag} \
    ${evpn_enabled_flag} \
    ${advertised_udn_isolation_flag} \
    ${ovnkube_config_file_flag} \
    ${persistent_ips_enabled_flag} \
    ${ovnkube_enable_multi_external_gateway_flag} \
    ${ovnkube_metrics_tls_opts} \
    ${ovn_encap_port_flag} \
    ${ovn_cluster_manager_ssl_opts} \
    ${ovn_v4_join_subnet_opt} \
    ${ovn_v4_masquerade_subnet_opt} \
    ${ovn_v6_join_subnet_opt} \
    ${ovn_v6_masquerade_subnet_opt} \
    ${ovn_v4_transit_subnet_opt} \
    ${ovn_v6_transit_subnet_opt} \
    ${network_qos_enabled_flag} \
    ${status_metrics_enabled_flag} \
    ${status_metrics_prometheus_url_flag} \
    ${dynamic_udn_allocation_flag} \
    ${dynamic_udn_grace_period} \
    ${ovnkube_cluster_default_nad_flag} \
    ${ovn_enable_dnsnameresolver_flag} \
    ${ovn_allow_icmp_netpol_flag} \
    ${ovnkube_metrics_scale_enable_flag} \
    --gateway-mode=${ovn_gateway_mode} \
    --cluster-subnets ${net_cidr} --k8s-service-cidr=${svc_cidr} \
    --host-network-namespace ${ovn_host_network_namespace} \
    --logfile-maxage=${ovnkube_logfile_maxage} \
    --logfile-maxbackups=${ovnkube_logfile_maxbackups} \
    --logfile-maxsize=${ovnkube_logfile_maxsize} \
    --logfile /var/log/ovn-kubernetes/ovnkube-cluster-manager.log \
    --loglevel=${ovnkube_loglevel} \
    --metrics-bind-address ${ovnkube_cluster_manager_metrics_bind_address} \
    --metrics-enable-pprof \
    --ovn-config-namespace ${ovn_kubernetes_namespace} \
    --pidfile ${OVN_RUNDIR}/ovnkube-cluster-manager.pid &

  echo "=============== ovn-cluster-manager ========== running"
  wait_for_event attempts=3 process_ready ovnkube-cluster-manager

  process_healthy ovnkube-cluster-manager
  exit 9
}

# ovn-controller - all nodes
ovn-controller() {
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovn-controller.pid

  echo "=============== ovn-controller - (wait for ovs)"
  wait_for_event ovs_ready

  echo "=============== ovn-controller - (wait for local NB/SB)"
  wait_for_event local_dbs_ready

  echo "=============== ovn-controller  start_controller"
  rm -f /var/run/ovn-kubernetes/cni/*
  rm -f ${OVN_RUNDIR}/ovn-controller.*.ctl

  run_as_ovs_user_if_needed \
    ${OVNCTL_PATH} --no-monitor start_controller \
    --ovn-controller-log="${ovn_loglevel_controller}" \
    ${ovn_controller_opts}

  wait_for_event attempts=3 process_ready ovn-controller
  echo "=============== ovn-controller ========== running"

  tail --follow=name ${OVN_LOGDIR}/ovn-controller.log &
  controller_tail_pid=$!

  process_healthy ovn-controller ${controller_tail_pid}
  exit 10
}

# ovn-node - all nodes
ovn-node() {
  trap 'kill $(jobs -p) ; rm -f /etc/cni/net.d/10-ovn-kubernetes.conf ; exit 0' TERM
  check_ovn_daemonset_version "1.4.0"
  rm -f ${OVN_RUNDIR}/ovnkube.pid

  # local_dbs_ready checks for the local NB/SB OVSDB readiness state.
  # This is not available on the DPU host because the DBs run locally on the DPU.
  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
    echo "=============== ovn-node - (wait for ovs)"
    wait_for_event ovs_ready
    echo "=============== ovn-node - (wait for local NB/SB)"
    wait_for_event local_dbs_ready
  fi

  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
    echo "=============== ovn-node - (ovn-node  wait for ovn-controller.pid)"
    wait_for_event process_ready ovn-controller
  fi

  ovn_routable_mtu_flag=
  if [[ -n "${routable_mtu}" ]]; then
	  routable_mtu_flag="--routable-mtu ${routable_mtu}"
  fi

  hybrid_overlay_flags=
  if [[ ${ovn_hybrid_overlay_enable} == "true" ]]; then
    hybrid_overlay_flags="--enable-hybrid-overlay"
    if [[ -n "${ovn_hybrid_overlay_net_cidr}" ]]; then
      hybrid_overlay_flags="${hybrid_overlay_flags} --hybrid-overlay-cluster-subnets=${ovn_hybrid_overlay_net_cidr}"
    fi
  fi

  disable_snat_multiple_gws_flag=
  if [[ ${ovn_disable_snat_multiple_gws} == "true" ]]; then
      disable_snat_multiple_gws_flag="--disable-snat-multiple-gws"
  fi

  ovn_encap_port_flag=
  if [[ -n "${ovn_encap_port}" ]]; then
      ovn_encap_port_flag="--encap-port=${ovn_encap_port}"
  fi
  echo "ovn_encap_port_flag=${ovn_encap_port_flag}"

  disable_forwarding_flag=
  if [[ ${ovn_disable_forwarding} == "true" ]]; then
      disable_forwarding_flag="--disable-forwarding"
  fi

  disable_pkt_mtu_check_flag=
  if [[ ${ovn_disable_pkt_mtu_check} == "true" ]]; then
      disable_pkt_mtu_check_flag="--disable-pkt-mtu-check"
  fi

  multicast_enabled_flag=
  if [[ ${ovn_multicast_enable} == "true" ]]; then
      multicast_enabled_flag="--enable-multicast"
  fi

  anp_enabled_flag=
  if [[ ${ovn_admin_network_policy_enable} == "true" ]]; then
      anp_enabled_flag="--enable-admin-network-policy"
  fi

  egressip_enabled_flag=
  if [[ ${ovn_egressip_enable} == "true" ]]; then
      egressip_enabled_flag="--enable-egress-ip"
  fi

  egressip_healthcheck_port_flag=
  if [[ -n "${ovn_egress_ip_healthcheck_port}" ]]; then
      egressip_healthcheck_port_flag="--egressip-node-healthcheck-port=${ovn_egress_ip_healthcheck_port}"
  fi

  egressservice_enabled_flag=
  if [[ ${ovn_egressservice_enable} == "true" ]]; then
	  egressservice_enabled_flag="--enable-egress-service"
  fi

  multi_network_enabled_flag=
  if [[ ${ovn_multi_network_enable} == "true" ]]; then
	  multi_network_enabled_flag="--enable-multi-network --enable-multi-networkpolicy"
  fi

  network_segmentation_enabled_flag=
  if [[ ${ovn_network_segmentation_enable} == "true" ]]; then
	  network_segmentation_enabled_flag="--enable-multi-network --enable-network-segmentation"
  fi

  network_connect_enabled_flag=
  if [[ ${ovn_network_connect_enable} == "true" ]]; then
	  network_connect_enabled_flag="--enable-network-connect"
  fi
  echo "network_connect_enabled_flag=${network_connect_enabled_flag}"

  uplink_enabled_flag=
  if [[ ${ovn_uplink_enable} == "true" ]]; then
	  uplink_enabled_flag="--enable-uplink"
  fi
  echo "uplink_enabled_flag=${uplink_enabled_flag}"

  pre_conf_udn_addr_enable_flag=
  if [[ ${ovn_pre_conf_udn_addr_enable} == "true" ]]; then
	  pre_conf_udn_addr_enable_flag="--enable-preconfigured-udn-addresses"
  fi

  route_advertisements_enabled_flag=
  if [[ ${ovn_route_advertisements_enable} == "true" ]]; then
	  route_advertisements_enabled_flag="--enable-route-advertisements"
  fi

  evpn_enabled_flag=
  if [[ ${ovn_evpn_enable} == "true" ]]; then
	  evpn_enabled_flag="--enable-evpn"
  fi

  advertised_udn_isolation_flag=
  if [[ -n ${ovn_advertised_udn_isolation_mode} ]]; then
      advertised_udn_isolation_flag="--advertised-udn-isolation-mode=${ovn_advertised_udn_isolation_mode}"
  fi

  ovnkube_config_file_flag="--config-file=/run/ovnkube-config/ovnkube.conf"
  echo "ovnkube_config_file_flag=${ovnkube_config_file_flag}"

  netflow_targets=
  if [[ -n ${ovn_netflow_targets} ]]; then
      netflow_targets="--netflow-targets ${ovn_netflow_targets}"
  fi

  sflow_targets=
  if [[ -n ${ovn_sflow_targets} ]]; then
      sflow_targets="--sflow-targets ${ovn_sflow_targets}"
  fi

  ipfix_targets=
  if [[ -n ${ovn_ipfix_targets} ]]; then
      ipfix_targets="--ipfix-targets ${ovn_ipfix_targets}"
  fi

  ipfix_config=
  if [[ -n ${ovn_ipfix_sampling} ]]; then
      ipfix_config="--ipfix-sampling ${ovn_ipfix_sampling}"
  fi
  if [[ -n ${ovn_ipfix_cache_max_flows} ]]; then
      ipfix_config="${ipfix_config} --ipfix-cache-max-flows ${ovn_ipfix_cache_max_flows}"
  fi
  if [[ -n ${ovn_ipfix_cache_active_timeout} ]]; then
      ipfix_config="${ipfix_config} --ipfix-cache-active-timeout ${ovn_ipfix_cache_active_timeout}"
  fi

  monitor_all=
  if [[ -n ${ovn_monitor_all} ]]; then
     monitor_all="--monitor-all=${ovn_monitor_all}"
  fi

  ofctrl_wait_before_clear=
  if [[ -n ${ovn_ofctrl_wait_before_clear} ]]; then
     ofctrl_wait_before_clear="--ofctrl-wait-before-clear=${ovn_ofctrl_wait_before_clear}"
  fi

  enable_lflow_cache=
  if [[ -n ${ovn_enable_lflow_cache} ]]; then
     enable_lflow_cache="--enable-lflow-cache=${ovn_enable_lflow_cache}"
  fi

  lflow_cache_limit=
  if [[ -n ${ovn_lflow_cache_limit} ]]; then
     lflow_cache_limit="--lflow-cache-limit=${ovn_lflow_cache_limit}"
  fi

  lflow_cache_limit_kb=
  if [[ -n ${ovn_lflow_cache_limit_kb} ]]; then
     lflow_cache_limit_kb="--lflow-cache-limit-kb=${ovn_lflow_cache_limit_kb}"
  fi

  egress_interface=
  if [[ -n ${ovn_ex_gw_network_interface} ]]; then
      egress_interface="--exgw-interface ${ovn_ex_gw_network_interface}"
  fi

  ovn_encap_ip_flag=
  if [[ ${ovn_encap_ip} != "" ]]; then
    ovn_encap_ip_flag="--encap-ip=${ovn_encap_ip}"
  else
    ovn_encap_ip=$(ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-encap-ip)
    if [[ $? == 0 ]]; then
      ovn_encap_ip=$(echo ${ovn_encap_ip} | tr -d '\"')
      if [[ "${ovn_encap_ip}" != "" ]]; then
        ovn_encap_ip_flag="--encap-ip=${ovn_encap_ip}"
      fi
    fi
  fi

  ovnkube_node_mode_flag=
  if [[ ${ovnkube_node_mode} != "" ]]; then
    ovnkube_node_mode_flag="--ovnkube-node-mode=${ovnkube_node_mode}"
    if [[ ${ovnkube_node_mode} == "dpu" ]]; then
      # encap IP is required for dpu, this is either provided via OVN_ENCAP_IP env variable or taken from ovs
      if [[ ${ovn_encap_ip} == "" ]]; then
        echo "ovn encap IP must be provided if \"ovnkube-node-mode\" set to \"dpu\". Exiting..."
        exit 1
      fi
    fi
  fi

  ovnkube_node_mgmt_port_netdev_flag=
  if [[ ${ovnkube_node_mgmt_port_netdev} != "" ]]; then
    ovnkube_node_mgmt_port_netdev_flag="--ovnkube-node-mgmt-port-netdev=${ovnkube_node_mgmt_port_netdev}"
  fi
  if [[ -n "${ovnkube_node_mgmt_port_dp_resource_name}" ]] ; then
    ovnkube_node_mgmt_port_netdev_flag="--ovnkube-node-mgmt-port-dp-resource-name=${ovnkube_node_mgmt_port_dp_resource_name}"
  fi

  ovn_simulate_dpu_flag=
  if [[ ${ovn_simulate_dpu} == "true" ]]; then
    ovn_simulate_dpu_flag="--simulate-dpu"
  fi

  ovn_dpu_host_gateway_representor_interface_flag=
  if [[ ${ovn_dpu_host_gateway_representor_interface} != "" ]]; then
    ovn_dpu_host_gateway_representor_interface_flag="--dpu-host-gateway-representor-interface=${ovn_dpu_host_gateway_representor_interface}"
  fi

  # Get gateway options from OVS for full and dpu modes.
  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
      get_gw_options
  fi

  ovn_unprivileged_flag="--unprivileged-mode"
  if test -z "${OVN_UNPRIVILEGED_MODE+x}" -o "x${OVN_UNPRIVILEGED_MODE}" = xno; then
    ovn_unprivileged_flag=""
  fi

  ovn_metrics_bind_address="${metrics_endpoint_ip}:9476"
  ovnkube_node_metrics_bind_address="${metrics_endpoint_ip}:9410"

  local ovnkube_metrics_tls_opts=""
  if [[ ${OVNKUBE_METRICS_PK} != "" && ${OVNKUBE_METRICS_CERT} != "" ]]; then
    ovnkube_metrics_tls_opts="
        --node-server-privkey ${OVNKUBE_METRICS_PK}
        --node-server-cert ${OVNKUBE_METRICS_CERT}
      "
  fi

  ovnkube_enable_multi_external_gateway_flag=
  if [[ ${ovn_enable_multi_external_gateway} == "true" ]]; then
	  ovnkube_enable_multi_external_gateway_flag="--enable-multi-external-gateway"
  fi
  echo "ovnkube_enable_multi_external_gateway_flag=${ovnkube_enable_multi_external_gateway_flag}"

  ovnkube_node_certs_flags=
  if [[ ${ovn_enable_ovnkube_identity} == "true" ]]; then
     ovnkube_node_certs_flags="
        --bootstrap-kubeconfig /host/etc/kubernetes/kubelet.conf
        --cert-dir /var/run/ovn-kubernetes/certs
     "
  fi
  echo "ovnkube_node_certs_flags=${ovnkube_node_certs_flags}"

  ovn_conntrack_zone_flag=
  if [[ ${ovn_conntrack_zone} != "" ]]; then
     ovn_conntrack_zone_flag="--conntrack-zone=${ovn_conntrack_zone}"
  fi
  echo "ovn_conntrack_zone_flag=${ovn_conntrack_zone_flag}"

  network_qos_enabled_flag=
  if [[ ${ovn_network_qos_enable} == "true" ]]; then
      network_qos_enabled_flag="--enable-network-qos"
  fi
  echo "network_qos_enabled_flag=${network_qos_enabled_flag}"

  status_metrics_enabled_flag=
  if [[ ${ovn_enable_status_metrics} == "true" ]]; then
      status_metrics_enabled_flag="--enable-status-metrics"
  fi
  echo "status_metrics_enabled_flag=${status_metrics_enabled_flag}"

  status_metrics_prometheus_url_flag=
  if [[ -n "${ovn_status_metrics_prometheus_url}" ]]; then
      status_metrics_prometheus_url_flag="--metrics-status-prometheus-url=${ovn_status_metrics_prometheus_url}"
  fi
  echo "status_metrics_prometheus_url_flag=${status_metrics_prometheus_url_flag}"

  ovn_v4_masquerade_subnet_opt=
  if [[ -n ${ovn_v4_masquerade_subnet} ]]; then
      ovn_v4_masquerade_subnet_opt="--gateway-v4-masquerade-subnet=${ovn_v4_masquerade_subnet}"
  fi

  ovn_v6_masquerade_subnet_opt=
  if [[ -n ${ovn_v6_masquerade_subnet} ]]; then
    ovn_v6_masquerade_subnet_opt="--gateway-v6-masquerade-subnet=${ovn_v6_masquerade_subnet}"
  fi

  dynamic_udn_allocation_flag=
  if [[ ${ovn_enable_dynamic_udn_allocation} == "true" ]]; then
    dynamic_udn_allocation_flag="--enable-dynamic-udn-allocation"
  fi
  echo "dynamic_udn_allocation_flag=${dynamic_udn_allocation_flag}"

  dynamic_udn_grace_period=
  if [[ -n ${ovn_dynamic_udn_grace_period} ]]; then
    dynamic_udn_grace_period="--udn-deletion-grace-period ${ovn_dynamic_udn_grace_period}"
  fi
  echo "dynamic_udn_grace_period=${dynamic_udn_grace_period}"

  # NB client cert material is only consumed by the Egress IP gRPC
  # health-check channel (see pkg/ovn/healthcheck/egressip_healthcheck.go).
  # The OVN NB/SB DB connections themselves are unix-socket only.
  local ovn_node_ssl_opts=""
  if [[ ${ovnkube_node_mode} != "dpu-host" ]]; then
      [[ "yes" == ${OVN_SSL_ENABLE} ]] && {
        ovn_node_ssl_opts="
            --nb-client-privkey ${ovn_controller_pk}
            --nb-client-cert ${ovn_controller_cert}
            --nb-client-cacert ${ovn_ca_cert}
            --nb-cert-common-name ${ovn_controller_cname}
          "
      }
  fi

  echo "=============== ovn-node   --init-node"
  /usr/bin/ovnkube --init-node ${K8S_NODE} \
        ${anp_enabled_flag} \
        ${disable_forwarding_flag} \
        ${disable_pkt_mtu_check_flag} \
        ${disable_snat_multiple_gws_flag} \
        ${egress_interface} \
        ${egressip_enabled_flag} \
        ${egressip_healthcheck_port_flag} \
        ${egressservice_enabled_flag} \
        ${enable_lflow_cache} \
        ${hybrid_overlay_flags} \
        ${ipfix_config} \
        ${ipfix_targets} \
        ${lflow_cache_limit} \
        ${lflow_cache_limit_kb} \
        ${monitor_all} \
        ${multicast_enabled_flag} \
        ${multi_network_enabled_flag} \
        ${network_segmentation_enabled_flag} \
        ${network_connect_enabled_flag} \
        ${uplink_enabled_flag} \
        ${pre_conf_udn_addr_enable_flag} \
        ${route_advertisements_enabled_flag} \
        ${evpn_enabled_flag} \
        ${advertised_udn_isolation_flag} \
        ${ovnkube_config_file_flag} \
        ${netflow_targets} \
        ${ofctrl_wait_before_clear} \
        ${ovn_encap_ip_flag} \
        ${ovn_encap_port_flag} \
        ${ovn_conntrack_zone_flag} \
        ${ovnkube_enable_multi_external_gateway_flag} \
        ${ovn_v4_masquerade_subnet_opt} \
        ${ovn_v6_masquerade_subnet_opt} \
        ${ovnkube_metrics_tls_opts} \
        ${ovnkube_node_certs_flags} \
        ${ovnkube_node_mgmt_port_netdev_flag} \
        ${ovnkube_node_mode_flag} \
        ${ovn_simulate_dpu_flag} \
        ${ovn_dpu_host_gateway_representor_interface_flag} \
        ${ovn_node_ssl_opts} \
        ${ovn_unprivileged_flag} \
        ${routable_mtu_flag} \
        ${sflow_targets} \
        ${dynamic_udn_allocation_flag} \
        ${dynamic_udn_grace_period} \
        ${ovnkube_cluster_default_nad_flag} \
        ${network_qos_enabled_flag} \
        ${status_metrics_enabled_flag} \
        ${status_metrics_prometheus_url_flag} \
        --cluster-subnets ${net_cidr} --k8s-service-cidr=${svc_cidr} \
        --export-ovs-metrics \
        --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts} \
        --gateway-router-subnet=${ovn_gateway_router_subnet} \
        --host-network-namespace ${ovn_host_network_namespace} \
        --inactivity-probe=${ovn_remote_probe_interval} \
        --logfile-maxage=${ovnkube_logfile_maxage} \
        --logfile-maxbackups=${ovnkube_logfile_maxbackups} \
        --logfile-maxsize=${ovnkube_logfile_maxsize} \
        --logfile /var/log/ovn-kubernetes/ovnkube.log \
        --loglevel=${ovnkube_loglevel} \
        --metrics-bind-address ${ovnkube_node_metrics_bind_address} \
        --metrics-enable-pprof \
        --mtu=${mtu} \
        --nodeport \
        --ovn-config-namespace ${ovn_kubernetes_namespace} \
        --ovn-metrics-bind-address ${ovn_metrics_bind_address} \
        --pidfile ${OVN_RUNDIR}/ovnkube.pid &

  wait_for_event attempts=3 process_ready ovnkube
  if [[ ${ovnkube_node_mode} != "dpu" ]]; then
    setup_cni
  fi
  echo "=============== ovn-node ========== running"

  process_healthy ovnkube
  exit 7
}

# cleanup-ovn-node - all nodes
cleanup-ovn-node() {
  check_ovn_daemonset_version "1.4.0"

  rm -f /etc/cni/net.d/10-ovn-kubernetes.conf

  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node - (wait for ovn-controller to exit)"
  retries=0
  while [[ ${retries} -lt 80 ]]; do
    process_ready ovn-controller
    if [[ $? != 0 ]]; then
      break
    fi
    echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node - (ovn-controller still running, wait)"
    sleep 1
    ((retries += 1))
  done

  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node --cleanup-node"
  /usr/bin/ovnkube --cleanup-node ${K8S_NODE} --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts} \
    --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${k8s_cacert} \
    --loglevel=${ovnkube_loglevel} \
    --logfile /var/log/ovn-kubernetes/ovnkube.log \
    --ovn-config-namespace ${ovn_kubernetes_namespace}

}

# v1.4.0 - Runs ovn-kube-util in daemon mode to export prometheus metrics related to OVS.
ovs-metrics() {
  check_ovn_daemonset_version "1.4.0"

  echo "=============== ovs-metrics - (wait for ovs_ready)"
  wait_for_event ovs_ready

  ovs_exporter_bind_address="${metrics_endpoint_ip}:${metrics_exporter_port}"
  /usr/bin/ovn-kube-util \
    --loglevel=${ovnkube_loglevel} \
    ovs-exporter \
    --metrics-bind-address ${ovs_exporter_bind_address}

  echo "=============== ovs-metrics with pid ${?} terminated ========== "
  exit 1
}

echo "================== ovnkube.sh --- version: ${ovnkube_version} ================"

echo " ==================== command: ${cmd}"
display_version

# display_env

# Start the requested daemons
# daemons come up in order
# ovs-db-server  - all nodes  -- not done by this script (v3)
# ovs-vswitchd   - all nodes  -- not done by this script (v3)
# run-ovn-northd Runs ovn-northd as a process does not run nb_ovsdb or sb_ovsdb (v3)
# ovn-identity     - control plane only (v3)
# ovn-controller - all nodes (v3)
# ovn-node       - all nodes (v3)
# cleanup-ovn-node - all nodes (v3)

case ${cmd} in
"local-nb-ovsdb")
  local-nb-ovsdb
  ;;
"local-sb-ovsdb")
  local-sb-ovsdb
  ;;
"run-ovn-northd") # zone controller container run-ovn-northd
  run-ovn-northd
  ;;
"ovnkube-identity") # pod ovnkube-identity container ovnkube-identity
  ovnkube-identity
  ;;
"ovnkube-controller") # zone controller container ovnkube-controller
  ovnkube-controller
  ;;
"ovnkube-controller-with-node")
  ovnkube-controller-with-node
  ;;
"ovn-cluster-manager") # control-plane container ovnkube-cluster-manager
  ovn-cluster-manager
  ;;
"ovs-server") # pod ovnkube-node container ovs-daemons
  ovs-server
  ;;
"ovn-controller") # pod ovnkube-node container ovn-controller
  ovn-controller
  ;;
"ovn-node") # pod ovnkube-node container ovn-node
  ovn-node
  ;;
"ovn-northd")
  ovn-northd
  ;;
"display_env")
  display_env
  exit 0
  ;;
"display")
  display
  exit 0
  ;;
"ovn_debug")
  ovn_debug
  exit 0
  ;;
"cleanup-ovs-server")
  cleanup-ovs-server
  ;;
"cleanup-ovn-node")
  cleanup-ovn-node
  ;;
"ovs-metrics")
  ovs-metrics
  ;;
*)
  echo "invalid command ${cmd}"
  echo "valid v3 commands: ovs-server local-nb-ovsdb local-sb-ovsdb run-ovn-northd " \
    "ovnkube-identity ovn-controller ovn-node display_env display ovn_debug cleanup-ovs-server " \
    "cleanup-ovn-node"
  exit 0
  ;;
esac

exit 0
