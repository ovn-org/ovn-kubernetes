#!/bin/bash
#set -x
#set -euo pipefail

# This script is the entrypoint for the OVN NB/SB DB HA using corosync/pacemaker.
# Supports version 3 daemonsets only
# Commands ($1 values)
#    run-ovndb Runs NB/SB OVSDB in pacemaker/corosync cluster (v3)
#
# NOTE: The script/image must be compatible with the daemonset.
# This script supports version 3 daemonsets
#    When called, it starts the needed daemons to provide pacemaker/corosync OVN DB.

# ====================
# Environment variables are used to customize operation
# Required:
# K8S_APISERVER - hostname:port (URL)of the real apiserver, not the service address
# OVN_DB_VIP - the virtual IP address to be used by ovn-controller, ovn-northd,
#                 and other OVN client-side utilities to connect to the OVN DB.
#
# Optional:
# OVN_KUBERNETES_NAMESPACE - k8s namespace
# OVN_DAEMONSET_VERSION - version of this script and it currently supports '3'
# K8S_TOKEN - the apiserver token. Automatically detected when running in a pod
# K8S_CACERT - the apiserver CA. Automatically detected when running in a pod
# =========================================
# ovnkube script version (update when script changes - v.x.y)
ovnkube_version="3"

# The daemonset version must be compatible with this script.
ovn_daemonset_version=${OVN_DAEMONSET_VERSION:-"3"}

# hostname is the host's hostname when using host networking,
# This is useful on the master
# otherwise it is the container ID (useful for debugging).
ovn_pod_host=$(hostname)

# in the case where OVN DBs are configured for Active/Standby HA using corosync/pacemaker,
# then ovndb_vip represents the Virtual IP address that frontend's both NB and SB DBs
ovndb_vip=${OVN_DB_VIP:-""}

if [[ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]]
then
  k8s_token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
else
  k8s_token=${K8S_TOKEN}
fi

K8S_CACERT=${K8S_CACERT:-/var/run/secrets/kubernetes.io/serviceaccount/ca.crt}

ovn_kubernetes_namespace=${OVN_KUBERNETES_NAMESPACE:-ovn-kubernetes}

# check that daemonset version is among expected versions
check_ovn_daemonset_version () {
  ok=$1
  for v in ${ok} ; do
    if [[ $v == ${ovn_daemonset_version} ]] ; then
      return 0
    fi
  done
  echo "VERSION MISMATCH expect ${ok}, daemonset is version ${ovn_daemonset_version}"
  exit 1
}

# set the ovnkube_db endpoint for other pods to query the OVN DB IP
set_ovnkube_db_ep () {
  # create a new endpoint for the headless onvkube-db service without selectors
  # using the provided VIP
  kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} apply -f - << EOF
apiVersion: v1
kind: Endpoints
metadata:
  name: ovnkube-db
  namespace: ${ovn_kubernetes_namespace}
subsets:
  - addresses:
      - ip: ${ovndb_vip}
    ports:
    - name: north
      port: 6641
      protocol: TCP
    - name: south
      port: 6642
      protocol: TCP
EOF
    if [[ $? != 0 ]] ; then
        echo "Failed to create endpoint with host ${ovndb_vip} for ovnkube-db service"
        exit 1
    fi
}

cmd=${1}

# =========================================

# Check the ovndb_server pcs resource status. return 2 if
# the status is unknown, return 1 if the status is
# stopped, return 0 if status is started (master or slave)
crm_node_status () {
  retcode=2
  crm stat  | egrep 'Masters|Slaves|Stopped' | awk '$1=$1' > /tmp/crm_stat.$$
  if [[ $? -ne 0 ]] ; then
    echo Command "crm stat" failed
    return $retcode
  fi

  IFS=' ' read -a master_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Slaves/) print $2;}' /tmp/crm_stat.$$)"
  IFS=' ' read -a slave_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Masters/) print $2;}' /tmp/crm_stat.$$)"
  IFS=' ' read -a stopped_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Stopped/) print $2;}' /tmp/crm_stat.$$)"
  started_nodes=("${master_nodes[@]}" "${slave_nodes[@]}")
  if [[ " ${master_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=0
  elif [[ " ${slave_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=0
  elif [[ " ${stopped_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=1
  fi
  rm -rf /tmp/crm_stat.$$
  echo "cluster status $retcode"
  return $retcode
}

config_corosync () {
  # get all the nodes that are participating in hosting the OVN DBs
  cluster_nodes=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get nodes --selector=openvswitch.org/ovnkube-db=true \
    -o=jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null)
  nnodes=$(echo $cluster_nodes |wc -w)
  if [[ $nnodes -lt 2 ]]; then
    echo "at least 2 nodes need to be configured"
    exit 1
  fi
  echo cluster_nodes=$cluster_nodes

  cat << EOF > /etc/corosync/corosync.conf
totem {
  version: 2
  cluster_name: ovn-central-cluster
  transport: udpu
  secauth: off
}
quorum {
  provider: corosync_votequorum
}
logging {
  to_logfile: yes
  logfile: /var/log/corosync/corosync.log
  to_syslog: yes
  timestamp: on
}
nodelist {
`for h in $cluster_nodes; do printf "  node {\n    ring0_addr: $h\n  }\n"; done`
}
EOF
}

service_start() {
  for svc in "$@"; do
    service ${svc} start >> /dev/null 2>&1
  done
}

service_healthy() {
  for svc in "$@"; do
    service ${svc} status | grep "is running" >> /dev/null 2>&1
    if [[ $? -ne 0 ]]; then
      echo "service ${svc} is not started"
      return 1
    fi
  done
  return 0
}

# v3 - Runs ovn NB/SB DB pacemaker/corosync cluster
run-ovndb () {
  check_ovn_daemonset_version "3"
  if [[ ${ovndb_vip} == "" ]] ; then
    echo "Exiting since the Virtual IP to be used for OVN DBs has not been provided"
    return 1
  fi
  trap stop-ovndb TERM

  config_corosync

  service_start pcsd corosync pacemaker
  service_healthy pcsd corosync pacemaker
  if [[ $? -ne 0 ]]; then
    exit 11
  fi

  pcs property set stonith-enabled=false > /dev/null 2>&1
  pcs property set no-quorum-policy=ignore > /dev/null 2>&1
  pcs resource create VirtualIP ocf:heartbeat:IPaddr2 ip=${ovndb_vip} op monitor interval=30s > /dev/null 2>&1
  pcs resource create ovndb_servers ocf:ovn:ovndb-servers master_ip=${ovndb_vip}  \
    inactive_probe_interval=0 ovn_ctl=/usr/share/openvswitch/scripts/ovn-ctl \
    op monitor interval="60s" timeout="50s" op monitor role=Master interval="15s" > /dev/null 2>&1
  pcs resource master ovndb_servers-master ovndb_servers meta notify="true" > /dev/null 2>&1
  pcs constraint order promote ovndb_servers-master then VirtualIP > /dev/null 2>&1
  pcs constraint colocation add VirtualIP with master ovndb_servers-master score=INFINITY > /dev/null 2>&1

  retries=0
  started=0
  while true; do
    # Check resource status on this node
    crm_node_status
    ret=$?

    if [[ $started -eq 0 ]]; then
      if [[ $ret -eq 0 ]]; then
        echo "OVN DB HA cluster started after ${retries} retries"
        started=1

        # now we can create the ovnkube-db endpoint with the ${ovndb_vip} as the IP address. with that
        # the waiting ovnkube-node and ovnkube-master PODs will continue
        set_ovnkube_db_ep
      elif [[ $ret -eq 1 ]]; then
	echo pcs node unstandby ${ovn_pod_host}
        pcs node unstandby ${ovn_pod_host}
      elif [[ "${retries}" -gt 60 ]]; then
        echo "after 60 retries, Corosync/Packemaker OVN DB cluster didn't start"
        exit 11
      fi
    elif [[ $ret -ne 0 ]]; then
      # Service stopped for some reason, reset the retry and restart
      echo "Unknown OVN DB HA cluster status. Attempting to restart the cluster"
      started=0
      retries=0
    fi
    sleep 1
    (( retries += 1 ))
  done
}

# v3 - standby ovn NB/SB DB cluster on this node
# this will gracefully stop the cluster on this
# node and unplumb the VIP if needed
stop-ovndb () {
  echo pcs node standby ${ovn_pod_host}
  pcs node standby ${ovn_pod_host}
  retries=0
  while true; do
    crm_node_status
    ret=$?
    if [[ $ret -ne 0 ]] ; then
      break
    fi
    (( retries += 1 ))
    if [[ "${retries}" -gt 30 ]]; then
      echo "error: OVN DB corosync/pacemaker cluster failed to be stopped gracefully"
      break
    fi
    sleep 1
  done
}


echo "================== ovndb-vip.sh --- version: ${ovnkube_version} ================"

  echo " ==================== command: ${cmd}"

  # display_env

# Start the corosync/pacemaker
# run-ovndb - Runs NB/SB OVSDB in pacemaker/corosync framework (v3)

  case ${cmd} in
    "run-ovndb")
    run-ovndb
    ;;
    *)
    # TODO: add debug commands to capture the health/status of the corosync/pacemaker
	echo "invalid command ${cmd}"
	echo "valid commands: run-ovndb"
	exit 0
  esac

exit 0
