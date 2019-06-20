#!/bin/bash
#set -x
#set -euo pipefail

# This script is the entrypoint to the image.
# Supports version 1, 2, and 3 daemonsets
#    $1 is the daemon to start. In version 2 the daemonset has a container for
#        each daemon. This start script is called with the desired daemon. In
#        version 3 each process has a separate container. Some daemons start
#        more than 1 process. Also, where possible, output is to stdout and
#        The script waits for prerquisite deamons to come up first.
#        The default, with $1 == "" is to start all daemons one after the other
#        waiting for a daemon to start before starting the next (version 1).
# Commands ($1 values)
#    ovs-server     Runs the ovs daemons - ovsdb-server and ovs-switchd (v2, v3)
#    run-ovn-northd Runs ovn-northd as a process does not run nb_ovsdb or sb_ovsdb (v3)
#    nb-ovsdb       Runs nb_ovsdb as a process (no detach or monitor) (v3)
#    sb-ovsdb       Runs sb_ovsdb as a process (no detach or monitor) (v3)
#    ovn-northd     Runs ovn-northd daemon - (v2)
#    ovn-master     Runs ovnkube in master mode (v2, v3)
#    ovn-controller Runs ovn controller (v2, v3)
#    ovn-node       Runs ovnkube in node mode (v2, v3)
#    cleanup-ovn-node   Runs ovnkube to cleanup the node (v3)
#    cleanup-ovs-server Cleanup ovs-server
#
#    display        Displays log files
#    display_env    Displays environment variables
#    ovn_debug      Displays ovn/ovs configuration and flows
#
#    <no argumet>   Runs all daemons - version 1 compatibility

# NOTE: The script/image must be compatible with the daemonset.
# This script supports version 1, 2, 3 daemonsets
#      When called, it starts all needed daemons.

# ====================
# Environment variables are used to customize operation
# K8S_APISERVER - hostname:port (URL)of the real apiserver, not the service address
# OVN_NET_CIDR - the network cidr
# OVN_SVC_CIDR - the cluster-service-cidr
# OVN_KUBERNETES_NAMESPACE - k8s namespace
#
# The following variables are optional and can override internally derived values.
# OVN_NORTH - the full URL to the ovn northdb
# OVN_SOUTH - the full URL to the ovn southdb
#
# Optional:
# OVN_MASTER - whether or not to run the master processes - v1, v2, v3
# OVN_DAEMONSET_VERSION - version match daemonset and image - v2, v3
# K8S_TOKEN - the apiserver token. Automatically detected when running in a pod
# K8S_CACERT - the apiserver CA. Automatically detected when running in a pod
# OVN_CONTROLLER_OPTS - the options for ovn-ctl
# OVN_NORTHD_OPTS - the options for the ovn northbound db
# OVN_GATEWAY_MODE - the gateway mode (shared, spare, local)
# OVN_GATEWAY_OPTS - the options for the ovn gateway
# OVNKUBE_LOGLEVEL - log level for ovnkube (0..5, default 4)
# OVN_LOG_NORTHD - log level (ovn-ctl default: -vconsole:emer -vsyslog:err -vfile:info)
# OVN_LOG_NB - log level (ovn-ctl default: -vconsole:off -vfile:info)
# OVN_LOG_SB - log level (ovn-ctl default: -vconsole:off -vfile:info)
# OVN_LOG_CONTROLLER - log level (ovn-ctl default: -vconsole:off -vfile:info)

# The argument to the command is the operation to be performed
# ovn-northd ovn-master ovn-controller ovn-node display display_env ovn_debug
# default is compatibility mode with version 1 daemonsets
cmd=${1:-"start-ovn"}

# There is a single image for both master nodes and compute nodes
# When OVN_MASTER is true, start the master daemons
ovn_master=${OVN_MASTER:-"false"}

# ovn daemon log levels
ovn_log_northd=${OVN_LOG_NORTHD:-"-vconsole:info"}
ovn_log_nb=${OVN_LOG_NB:-"-vconsole:info"}
ovn_log_sb=${OVN_LOG_SB:-"-vconsole:info"}
ovn_log_controller=${OVN_LOG_CONTROLLER:-"-vconsole:info"}

logdir=/var/log/openvswitch
ovnkubelogdir=/var/log/ovn-kubernetes
logpost=$(date +%F-%T)
ovn_nb_log_file=${logdir}/ovsdb-server-nb-${logpost}.log
ovn_sb_log_file=${logdir}/ovsdb-server-sb-${logpost}.log

# ovnkube.sh version (update when script changes - v.x.y)
ovnkube_version="3"

# The daemonset version must be compatible with this script.
# The default when OVN_DAEMONSET_VERSION is not set is version 1
ovn_daemonset_version=${OVN_DAEMONSET_VERSION:-"1"}

# hostname is the host's hostname when using host networking,
# This is useful on the master node
# otherwise it is the container ID (useful for debugging).
ovn_pod_host=$(hostname)

# The ovs user id, by default it is going to be root:root
ovs_user_id=${OVS_USER_ID:-""}

# ovs options
ovs_options=${OVS_OPTIONS:-""}

if [[ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]]
then
  k8s_token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
else
  k8s_token=${K8S_TOKEN}
fi

K8S_CACERT=${K8S_CACERT:-/var/run/secrets/kubernetes.io/serviceaccount/ca.crt}

# ovn-northd - /etc/sysconfig/ovn-northd
ovn_northd_opts=${OVN_NORTHD_OPTS:-"--db-nb-sock=/var/run/openvswitch/ovnnb_db.sock --db-sb-sock=/var/run/openvswitch/ovnsb_db.sock"}

# ovn-controller
#OVN_CONTROLLER_OPTS=""
ovn_controller_opts=${OVN_CONTROLLER_OPTS:-""}

# set the log level for ovnkube
ovnkube_loglevel=${OVNKUBE_LOGLEVEL:-4}

# by default it is going to be a shared gateway mode, however this can be overridden to any of the other
# two gateway modes that we support using `images/daemonset.sh` tool
ovn_gateway_mode=${OVN_GATEWAY_MODE:-"shared"}
ovn_gateway_opts=${OVN_GATEWAY_OPTS:-""}

net_cidr=${OVN_NET_CIDR:-10.128.0.0/14/23}
svc_cidr=${OVN_SVC_CIDR:-172.30.0.0/16}

ovn_kubernetes_namespace=${OVN_KUBERNETES_NAMESPACE:-ovn-kubernetes}

# host on which ovnkube-db POD is running and this POD contains both
# OVN NB and SB DB running in their own container
ovn_db_host=""

# =========================================

# $1 function to call to test event present, returns 0 (OK) 1 (bad)
# $2 (optional) arg to $1
wait_for_event () {
  retries=0
  sleeper=1
  while true; do
    $1 $2
    if [[ $? != 0 ]] ; then
      (( retries += 1 ))
      if [[ "${retries}" -gt 80 ]]; then
        echo "error: $1 $2 did not come up, exiting"
        exit 1
      fi
      echo "info: Waiting for $1 $2 to come up, waiting ${sleeper}s ..."
      sleep ${sleeper}
      sleeper=5
    else
      if [[ "${retries}" != 0 ]]; then
        echo "$1 $2 came up in ${retries} ${sleeper} sec tries"
      fi
      break
    fi
  done

}

# OVN DBs must be up and initialized before ovn-master and ovn-node PODs can come up
# This waits for ovnkube-db POD to come up
ready_to_start_node () {

  # See if ep is available ...
  ovn_db_host=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get ep -n ${ovn_kubernetes_namespace} ovnkube-db 2>/dev/null | grep 6642 | sed 's/:/ /' | awk '/ovnkube-db/{ print $2 }')
  if [[ ${ovn_db_host} == "" ]] ; then
      return 1
  fi
  get_ovn_db_vars
  ovn-nbctl --db=${ovn_nbdb_test} show > /dev/null 2>&1
  if [[ $? != 0 ]] ; then
      return 1
  fi
  return 0
}
# wait_for_event ready_to_start_node


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

# The ovn vars are based on the IP address of the K8s node
# on which ovnkube-db is running on.
get_ovn_db_vars () {
  # OVN_NORTH and OVN_SOUTH override derived host
  # Currently limited to tcp (ssl is not supported yet)
  ovn_nbdb=${OVN_NORTH:-tcp://${ovn_db_host}:6641}
  ovn_sbdb=${OVN_SOUTH:-tcp://${ovn_db_host}:6642}
  ovn_nbdb_test=$(echo ${ovn_nbdb} | sed 's;//;;')
}

# ovs must be up before ovn comes up
# This waits for ovs to come up
ovs_ready () {
  /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null
  if [[ $? != 0 ]] ; then
    return 1
  fi
  return 0
}
#wait_for_event ovs_ready

# version 1 daemonset compatibility
# ovn must be up and initialized before ovn-controller, init-node, and init-master
# This waits for northd to come up
northd_ready () {
  ovn-nbctl --db=${ovn_nbdb_test} show > /dev/null 2>&1
  if [[ $? != 0 ]] ; then
    return 1
  fi
  return 0
}
#wait_for_event northd_ready


# wait for the pid file to appear (daemon is up)
# verify the pid is running
# $1 is the pid file name
pid_ready () {
  pidfile=/var/run/openvswitch/${1}
  if [[ -f ${pidfile} ]] ; then
    pid=$(cat ${pidfile})
    pidTest=$(ps ax | awk '{ print $1 }' | grep "^${pid:-XX}$")
    if [[ ${pid:-XX} == ${pidTest} ]] ; then
      return 0
    fi
  fi
  return 1
}
#wait_for_event pid_ready $1


# check process health, exit if process terminates
# $1 is the pid file fqn
pid_health () {
  pid=$(cat $1)
  while true; do
    pidTest=$(ps ax | awk '{ print $1 }' | grep "^${pid:-XX}$")
    if [[ ${pid:-XX} != ${pidTest} ]] ; then
      echo "=============== pid ${pid} terminated ========== "
      # kill the tail -f
      kill $2
      exit 6
    fi
    sleep 15
  done
}

display_file () {
    if [[ -f $3 ]]
    then
      echo "====================== $1 pid "
      cat $2
      echo "====================== $1 log "
      cat $3
      echo " "
    fi
}

# pid and log file for each container
display () {
  echo "==================== display for ${ovn_pod_host}  =================== "
  date
  latest=$(ls -t ${logdir}/ovsdb-server-nb-*.log | head -1)
  display_file "nb-ovsdb" /var/run/openvswitch/ovnnb_db.pid ${latest}
  latest=$(ls -t ${logdir}/ovsdb-server-sb-*.log | head -1)
  display_file "sb-ovsdb" /var/run/openvswitch/ovnsb_db.pid ${latest}
  display_file "run-ovn-northd" /var/run/openvswitch/ovn-northd.pid ${logdir}/ovn-northd.log
  display_file "ovn-master" /var/run/openvswitch/ovnkube-master.pid ${ovnkubelogdir}/ovnkube-master.log
  display_file "ovs-vswitchd" /var/run/openvswitch/ovs-vswitchd.pid ${logdir}/ovs-vswitchd.log
  display_file "ovsdb-server" /var/run/openvswitch/ovsdb-server.pid ${logdir}/ovsdb-server.log
  display_file "ovn-controller" /var/run/openvswitch/ovn-controller.pid ${logdir}/ovn-controller.log
  display_file "ovnkube" /var/run/openvswitch/ovnkube.pid ${ovnkubelogdir}/ovnkube.log
}

setup_cni () {
  # Take over network functions on the node
  # rm -f /etc/cni/net.d/*
  cp -f /usr/libexec/cni/ovn-k8s-cni-overlay /host/opt/cni/bin/ovn-k8s-cni-overlay
  if [[ ! -f /host/opt/cni/bin/loopback ]]
  then
    cp -f /usr/libexec/cni/loopback /host/opt/cni/bin/loopback
  fi
}

display_version () {
  echo " =================== hostname: ${ovn_pod_host}"
  echo " =================== daemonset version ${ovn_daemonset_version}"
  if [[ -f /root/.git/HEAD ]]
  then
    commit=$(awk '{ print $1 }' /root/.git/HEAD )
    if [[ ${commit} == "ref:" ]]
    then
      head=$(awk '{ print $2 }' /root/.git/HEAD )
      commit=$(cat /root/.git/${head} )
    else
      head="master"
      commit=$(cat /root/.git/HEAD)
    fi
    echo " =================== Image built from ovn-kubernetes ref: ${head}  commit: ${commit}"
  fi
}

display_env () {
echo OVS_USER_ID ${ovs_user_id}
echo OVS_OPTIONS ${ovs_options}
echo OVN_NORTH ${ovn_nbdb}
echo OVN_NORTHD_OPTS ${ovn_northd_opts}
echo OVN_SOUTH ${ovn_sbdb}
echo OVN_CONTROLLER_OPTS ${ovn_controller_opts}
echo OVN_LOG_CONTROLLER ${ovn_log_controller}
echo OVN_GATEWAY_MODE ${ovn_gateway_mode}
echo OVN_GATEWAY_OPTS ${ovn_gateway_opts}
echo OVN_NET_CIDR ${net_cidr}
echo OVN_SVC_CIDR ${svc_cidr}
echo K8S_APISERVER ${K8S_APISERVER}
echo OVNKUBE_LOGLEVEL ${ovnkube_loglevel}
echo OVN_DAEMONSET_VERSION ${ovn_daemonset_version}
echo ovnkube.sh version ${ovnkube_version}
}

ovn_debug () {
  # get ovn_db_host
  ready_to_start_node
  echo "ovn_nbdb ${ovn_nbdb}   ovn_sbdb ${ovn_sbdb}"
  echo "ovn_nbdb_test ${ovn_nbdb_test}"

  # get ovs/ovn info from the node for debug purposes
  echo "=========== ovn_debug   hostname: ${ovn_pod_host} ============="
  echo "=========== ovn-nbctl show ============="
  echo "=========== ovn-nbctl --db=${ovn_nbdb_test} show ============="
  ovn-nbctl --db=${ovn_nbdb_test} show
  echo " "
  echo "=========== ovn-nbctl list ACL ============="
  ovn-nbctl --db=${ovn_nbdb_test} list ACL
  echo " "
  echo "=========== ovn-nbctl list address_set ============="
  ovn-nbctl --db=${ovn_nbdb_test} list address_set
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
  ovn_sbdb_test=$(echo ${ovn_sbdb} | sed 's;//;;')
  echo "=========== ovn-sbctl --db=${ovn_sbdb_test} show ============="
  ovn-sbctl --db=${ovn_sbdb_test} show
  echo " "
  echo "=========== ovn-sbctl --db=${ovn_sbdb_test} lflow-list ============="
  ovn-sbctl --db=${ovn_sbdb_test} lflow-list
  echo " "
  echo "=========== ovn-sbctl --db=${ovn_sbdb_test} list datapath ============="
  ovn-sbctl --db=${ovn_sbdb_test} list datapath
  echo " "
  echo "=========== ovn-sbctl --db=${ovn_sbdb_test} list port ============="
  ovn-sbctl --db=${ovn_sbdb_test} list port
}

ovs-server () {
  # start ovs ovsdb-server and ovs-vswitchd
  set -euo pipefail

  # if another process is listening on the cni-server socket, wait until it exits
  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    if /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null; then
      echo "warning: Another process is currently managing OVS, waiting 10s ..." 2>&1
      sleep 10 & wait
      (( retries += 1 ))
    else
      break
    fi
    if [[ "${retries}" -gt 60 ]]; then
      echo "error: Another process is currently managing OVS, exiting" 2>&1
      exit 1
    fi
  done
  rm -f /var/run/openvswitch/ovs-vswitchd.pid
  rm -f /var/run/openvswitch/ovsdb-server.pid

  # launch OVS
  function quit {
      /usr/share/openvswitch/scripts/ovs-ctl stop
      exit 1
  }
  trap quit SIGTERM
  [ ${ovs_user_id:-XX} != "XX" ] && set "$@" --ovs-user ${ovs_user_id}
  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovs-vswitchd \
    --system-id=random ${ovs_options} "$@"

  # Restrict the number of pthreads ovs-vswitchd creates to reduce the
  # amount of RSS it uses on hosts with many cores
  # https://bugzilla.redhat.com/show_bug.cgi?id=1571379
  # https://bugzilla.redhat.com/show_bug.cgi?id=1572797
  if [[ `nproc` -gt 12 ]]; then
      ovs-vsctl --no-wait set Open_vSwitch . other_config:n-revalidator-threads=4
      ovs-vsctl --no-wait set Open_vSwitch . other_config:n-handler-threads=10
  fi
  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovsdb-server \
    --system-id=random ${ovs_options} "$@"

  # Ensure GENEVE's UDP port isn't firewalled
  /usr/share/openvswitch/scripts/ovs-ctl --protocol=udp --dport=6081 enable-protocol

  tail --follow=name /var/log/openvswitch/ovs-vswitchd.log /var/log/openvswitch/ovsdb-server.log &
  ovs_tail_pid=$!
  sleep 10
  while true; do
    if ! /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null; then
      echo "OVS seems to have crashed, exiting"
      kill ${ovs_tail_pid}
      quit
    fi
    sleep 15
  done
}

cleanup-ovs-server () {
  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server (wait for ovn-node to exit) ======="
  retries=0
  while [[ ${retries} -lt 80 ]]; do
    if [[ ! -e /var/run/openvswitch/ovnkube.pid ]] ; then
      break
    fi
    echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server ovn-node still running, wait) ======="
    sleep 1
  done
  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovs-server (ovs-ctl stop) ======="
  /usr/share/openvswitch/scripts/ovs-ctl stop
}

# create the ovnkube_db endpoint for other pods to query the OVN DB IP
create_ovnkube_db_ep () {
  # delete any endpoint by name ovnkube-db
  kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    delete ep -n ${ovn_kubernetes_namespace} ovnkube-db 2>/dev/null

  # create a new endpoint for the headless onvkube-db service without selectors
  # using the current host has the endpoint IP
  ovn_db_host=$(getent ahosts $(hostname) | head -1 | awk '{ print $1 }')
  kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} create -f - << EOF
apiVersion: v1
kind: Endpoints
metadata:
  name: ovnkube-db
  namespace: ${ovn_kubernetes_namespace}
subsets:
  - addresses:
      - ip: ${ovn_db_host}
    ports:
    - name: north
      port: 6641
      protocol: TCP
    - name: south
      port: 6642
      protocol: TCP
EOF
    if [[ $? != 0 ]] ; then
        echo "Failed to create endpoint with host ${ovn_db_host} for ovnkube-db service"
        exit 1
    fi
}

# v3 - run nb_ovsdb in a separate container
nb-ovsdb () {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "3"
  rm -f /var/run/openvswitch/ovnnb_db.pid

  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  iptables-rules 6641

  echo "=============== run nb_ovsdb ========== MASTER ONLY"
  echo "ovn_log_nb=${ovn_log_nb} ovn_nb_log_file=${ovn_nb_log_file}"
  /usr/share/openvswitch/scripts/ovn-ctl run_nb_ovsdb --no-monitor \
  --ovn-nb-logfile=${ovn_nb_log_file} --ovn-nb-log="${ovn_log_nb}" &

  wait_for_event pid_ready ovnnb_db.pid
  echo "=============== nb-ovsdb ========== RUNNING"
  sleep 3
  ovn-nbctl set-connection ptcp:6641 -- set connection . inactivity_probe=0

  tail --follow=name ${ovn_nb_log_file} &
  ovn_tail_pid=$!

  pid_health /var/run/openvswitch/ovnnb_db.pid ${ovn_tail_pid}
  echo "=============== run nb_ovsdb ========== terminated"
}

# v3 - run sb_ovsdb in a separate container
sb-ovsdb () {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "3"
  rm -f /var/run/openvswitch/ovnsb_db.pid

  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  iptables-rules 6642

  echo "=============== run sb_ovsdb ========== MASTER ONLY"
  echo "ovn_log_sb=${ovn_log_sb} ovn_sb_log_file=${ovn_sb_log_file}"
  /usr/share/openvswitch/scripts/ovn-ctl run_sb_ovsdb --no-monitor \
  --ovn-sb-logfile=${ovn_sb_log_file} --ovn-sb-log="${ovn_log_sb}" &

  wait_for_event pid_ready ovnsb_db.pid
  echo "=============== sb-ovsdb ========== RUNNING"
  sleep 3
  ovn-sbctl set-connection ptcp:6642 -- set connection . inactivity_probe=0

  # create the ovnkube_db endpoint for other pods to query the OVN DB IP
  create_ovnkube_db_ep

  tail --follow=name ${ovn_sb_log_file} &
  ovn_tail_pid=$!

  pid_health /var/run/openvswitch/ovnsb_db.pid ${ovn_tail_pid}
  echo "=============== run sb_ovsdb ========== terminated"
}


# v3 - Runs northd on master. Does not run nb_ovsdb, and sb_ovsdb
run-ovn-northd () {
  check_ovn_daemonset_version "3"
  rm -f /var/run/openvswitch/ovn-northd.pid

  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  echo "=============== run-ovn-northd (wait for ready_to_start_node)"
  wait_for_event ready_to_start_node

  sleep 1

  echo "=============== run_ovn_northd ========== MASTER ONLY"
  echo "ovn_db_host ${ovn_db_host}"
  echo "ovn_nbdb ${ovn_nbdb}   ovn_sbdb ${ovn_sbdb}"
  echo "ovn_northd_opts=${ovn_northd_opts}"
  echo "ovn_log_northd=${ovn_log_northd}"

  # no monitor (and no detach), start northd which connects to the
  # ovnkube-db service
  ovn_nbdb_i=$(echo ${ovn_nbdb} | sed 's;//;;')
  ovn_sbdb_i=$(echo ${ovn_sbdb} | sed 's;//;;')
  /usr/share/openvswitch/scripts/ovn-ctl start_northd \
    --no-monitor --ovn-manage-ovsdb=no \
    --ovn-northd-nb-db=${ovn_nbdb_i} --ovn-northd-sb-db=${ovn_sbdb_i} \
    --ovn-northd-log="${ovn_log_northd}" \
    ${ovn_northd_opts}

  wait_for_event pid_ready ovn-northd.pid
  echo "=============== run_ovn_northd ========== RUNNING"
  sleep 1

  tail --follow=name /var/log/openvswitch/ovn-northd.log &
  ovn_tail_pid=$!


  pid_health /var/run/openvswitch/ovn-northd.pid ${ovn_tail_pid}
  exit 8
}

# v2 - Runs all 3 northd processes
ovn-northd () {
  check_ovn_daemonset_version "2"
  # this is only run on masters
  if [[ ${ovn_master} = "true" ]]; then
    echo "=============== ovn-northd  ========== MASTER ONLY"
    # Make sure /var/lib/openvswitch exists
    mkdir -p /var/lib/openvswitch
    # ovn-northd - master node only

    echo "OVN_NORTH=${ovn_nbdb}  OVN_SOUTH=${ovn_sbdb} ovn_northd_opts=${ovn_northd_opts}"
    /usr/share/openvswitch/scripts/ovn-ctl start_northd \
      --db-nb-addr=${ovn_nbdb} --db-sb-addr=${ovn_sbdb} \
      ${ovn_northd_opts}
    echo "=============== ovn-northd ========== FAILED"
    cat /var/log/openvswitch/ovn-northd.log
    exit 4
  fi
}

# make sure the specified dport is open
iptables-rules () {
  dport=$1
  iptables -C INPUT -p tcp -m tcp --dport $dport -m conntrack --ctstate NEW -j ACCEPT
  if [[ $? != 0 ]] ; then
    iptables -I INPUT -p tcp -m tcp --dport $dport -m conntrack --ctstate NEW -j ACCEPT
  fi
}

# v2 v3 - run ovnkube --master
ovn-master () {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "2 3"
  rm -f /var/run/openvswitch/ovnkube-master.pid

  echo "=============== ovn-master (wait for ready_to_start_node) ========== MASTER ONLY"
  wait_for_event ready_to_start_node
  echo "ovn_nbdb ${ovn_nbdb}   ovn_sbdb ${ovn_sbdb}"

  # wait for northd to start
  wait_for_event pid_ready ovn-northd.pid
  sleep 5

  # wait for ovs-servers to start since ovn-master sets some fields in OVS DB
  echo "=============== ovn-master - (wait for ovs)"
  wait_for_event ovs_ready

  echo "=============== ovn-master ========== MASTER ONLY"
  /usr/bin/ovnkube \
    --init-master ${ovn_pod_host} \
    --cluster-subnet ${net_cidr} --k8s-service-cidr=${svc_cidr} \
    --nb-address=${ovn_nbdb} --sb-address=${ovn_sbdb} \
    --nodeport \
    --loglevel=${ovnkube_loglevel} \
    --pidfile /var/run/openvswitch/ovnkube-master.pid \
    --logfile /var/log/ovn-kubernetes/ovnkube-master.log &
  echo "=============== ovn-master ========== running"
  wait_for_event pid_ready ovnkube-master.pid
  sleep 1

  tail --follow=name /var/log/ovn-kubernetes/ovnkube-master.log &
  kube_tail_pid=$!

  pid_health /var/run/openvswitch/ovnkube-master.pid ${kube_tail_pid}
  exit 9
}

# ovn-controller - all nodes
ovn-controller () {
  check_ovn_daemonset_version "2 3"
  rm -f /var/run/openvswitch/ovn-controller.pid

  echo "=============== ovn-controller - (wait for ovs)"
  wait_for_event ovs_ready

  echo "=============== ovn-controller - (wait for ready_to_start_node)"
  wait_for_event ready_to_start_node

  echo "ovn_nbdb ${ovn_nbdb}   ovn_sbdb ${ovn_sbdb}"
  echo "ovn_nbdb_test ${ovn_nbdb_test}"

  echo "=============== ovn-controller  start_controller"
  rm -f /var/run/ovn-kubernetes/cni/*
  rm -f /var/run/openvswitch/ovn-controller.*.ctl
  /usr/share/openvswitch/scripts/ovn-ctl --no-monitor start_controller \
    --ovn-controller-log="${ovn_log_controller}" ${ovn_controller_opts}

  wait_for_event pid_ready ovn-controller.pid
  echo "=============== ovn-controller ========== running"

  sleep 4
  tail --follow=name /var/log/openvswitch/ovn-controller.log &
  controller_tail_pid=$!

  pid_health /var/run/openvswitch/ovn-controller.pid ${controller_tail_pid}
  exit 10
}



# ovn-node - all nodes
ovn-node () {
  trap 'kill $(jobs -p) ; rm -f /etc/cni/net.d/10-ovn-kubernetes.conf ; exit 0' TERM
  check_ovn_daemonset_version "2 3"
  rm -f /var/run/openvswitch/ovnkube.pid

  echo "=============== ovn-node - (wait for ovs)"
  wait_for_event ovs_ready

  echo "=============== ovn-node - (wait for ready_to_start_node)"
  wait_for_event ready_to_start_node

  echo "ovn_nbdb ${ovn_nbdb}   ovn_sbdb ${ovn_sbdb}  ovn_nbdb_test ${ovn_nbdb_test}"

  echo "=============== ovn-node - (ovn-node  wait for ovn-controller.pid)"
  wait_for_event pid_ready ovn-controller.pid
  sleep 1

  echo "=============== ovn-node   --init-node"
  # TEMP HACK - WORKAROUND
  # --gateway-mode=local works around a problem that
  # results in loss of network connectivity when docker is
  # restarted or ovs daemonset is deleted.
  # TEMP HACK - WORKAROUND
  /usr/bin/ovnkube --init-node ${K8S_NODE} \
      --cluster-subnet ${net_cidr} --k8s-service-cidr=${svc_cidr} \
      --nb-address=${ovn_nbdb} --sb-address=${ovn_sbdb} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts}  \
      --pidfile /var/run/openvswitch/ovnkube.pid \
      --logfile /var/log/ovn-kubernetes/ovnkube.log &

  wait_for_event pid_ready ovnkube.pid
  setup_cni
  echo "=============== ovn-node ========== running"

  sleep 5
  tail --follow=name /var/log/ovn-kubernetes/ovnkube.log &
  node_tail_pid=$!

  pid_health /var/run/openvswitch/ovnkube.pid ${node_tail_pid}
  exit 7
}

# cleanup-ovn-node - all nodes
cleanup-ovn-node () {
  check_ovn_daemonset_version "3"

  rm -f /etc/cni/net.d/10-ovn-kubernetes.conf

  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node - (wait for ovn-controller to exit)"
  retries=0
  while [[ ${retries} -lt 80 ]]; do
    pid_ready ovn-controller.pid
    if [[ $? != 0 ]] ; then
      break
    fi
    echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node - (ovn-controller still running, wait)"
    sleep 1
  done

  echo "=============== time: $(date +%d-%m-%H:%M:%S:%N) cleanup-ovn-node --cleanup-node"
  /usr/bin/ovnkube --cleanup-node ${K8S_NODE} --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts} \
      --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${K8S_CACERT} \
      --loglevel=${ovnkube_loglevel} \
      --logfile /var/log/ovn-kubernetes/ovnkube.log

}

# version 1 daemonset compatibility
# $1 is "" or "start_ovn"
start_ovn () {
  trap 'kill $(jobs -p); exit 0' TERM
  check_ovn_daemonset_version "1"

# In version 1, ovs is started by a separate daemonset that does not
# use this script.
# ovs is started from ovs_ctl commands not here...
# # start ovsdb-server
# /usr/share/openvswitch/scripts/ovs-ctl \
#   --no-ovs-vswitchd --no-monitor --system-id=random \
#   --ovs-user=${ovs_user_id} \
#   start ${ovs_options}

# # start ovs-vswitchd
# /usr/share/openvswitch/scripts/ovs-ctl \
#   --no-ovsdb-server --no-monitor --system-id=random \
#   --ovs-user=${ovs_user_id} \
#   start ${ovs_options}

  echo "=============== wait for ovs"
  wait_for_event ovs_ready

  get_ovn_db_vars
  # on the master only
  if [[ ${ovn_master} = "true" ]]
  then
    # Make sure /var/lib/openvswitch exists
    mkdir -p /var/lib/openvswitch
    # ovn-northd - master node only
    echo "=============== start ovn-northd ========== MASTER ONLY"
    echo OVN_NORTH=${ovn_nbdb}  OVN_SOUTH=${ovn_sbdb} ovn_northd_opts=${ovn_northd_opts}
    rm -f /var/run/openvswitch/ovn-northd.*.ctl
    /usr/share/openvswitch/scripts/ovn-ctl start_northd --no-monitor \
      --db-nb-addr=${ovn_nbdb} --db-sb-addr=${ovn_sbdb} \
      ${ovn_northd_opts}

    # ovn-master - master node only
    echo "=============== start ovn-master (wait for northbd) ========== MASTER ONLY"
    wait_for_event northd_ready

    echo "=============== start ovn-master ========== MASTER ONLY"
    /usr/bin/ovnkube \
      --init-master ${ovn_pod_host} \
      --cluster-subnet ${net_cidr} --k8s-service-cidr=${svc_cidr} \
      --nb-address=${ovn_nbdb} --sb-address=${ovn_sbdb} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --pidfile /var/run/openvswitch/ovnkube-master.pid \
      --logfile /var/log/ovn-kubernetes/ovnkube-master.log &
  fi

  # ovn-controller - all nodes
  echo "=============== start ovn-controller (wait for northd)"
  wait_for_event northd_ready

  echo "=============== start ovn-controller"
  rm -f /var/run/ovn-kubernetes/cni/*
  /usr/share/openvswitch/scripts/ovn-ctl --no-monitor start_controller \
    --ovn-controller-log="${ovn_log_controller}" ${ovn_controller_opts}

  # ovn-node - all nodes
  echo  "=============== start ovn-node"
  # TEMP HACK - WORKAROUND
  # --gateway-mode=local works around a problem that
  # results in loss of network connectivity when docker is
  # restarted or ovs daemonset is deleted.
  # TEMP HACK - WORKAROUND
  /usr/bin/ovnkube --init-node ${K8S_NODE} \
      --cluster-subnet ${net_cidr} --k8s-service-cidr=${svc_cidr} \
      --nb-address=${ovn_nbdb} --sb-address=${ovn_sbdb} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --gateway-mode=${ovn_gateway_mode} ${ovn_gateway_opts}  \
      --pidfile /var/run/openvswitch/ovnkube.pid \
      --logfile /var/log/ovn-kubernetes/ovnkube.log &

  echo "=============== done starting daemons ================="
}

echo "================== ovnkube.sh --- version: ${ovnkube_version} ================"

  echo " ==================== command: ${cmd}"
  display_version

  # display_env

# Start the requested daemons
# daemons come up in order
# ovs-db-server  - all nodes  -- not done by this script (v2 v3)
# ovs-vswitchd   - all nodes  -- not done by this script (v2 v3)
#  run-ovn-northd Runs ovn-northd as a process does not run nb_ovsdb or sb_ovsdb (v3)
#  nb-ovsdb       Runs nb_ovsdb as a process (no detach or monitor) (v3)
#  sb-ovsdb       Runs sb_ovsdb as a process (no detach or monitor) (v3)
# ovn-northd     - master node only (v2)
# ovn-master     - master node only (v2 v3)
# ovn-controller - all nodes (v2 v3)
# ovn-node       - all nodes (v2 v3)
# cleanup-ovn-node - all nodes (v3)

  case ${cmd} in
    "nb-ovsdb")        # pod ovnkube-db container nb-ovsdb
	nb-ovsdb
    ;;
    "sb-ovsdb")        # pod ovnkube-db container sb-ovsdb
	sb-ovsdb
    ;;
    "run-ovn-northd")  # pod ovnkube-master container run-ovn-northd
	run-ovn-northd
    ;;
    "ovn-master")      # pod ovnkube-master container ovnkube-master
	ovn-master
    ;;
    "ovs-server")      # pod ovnkube-node container ovs-daemons
        ovs-server
    ;;
    "ovn-controller")  # pod ovnkube-node container ovn-controller
	ovn-controller
    ;;
    "ovn-node")        # pod ovnkube-node container ovn-node
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
    # This is being deprecated
    # daemonset version 1 compatibility mode
    "start-ovn")
        setup_cni
	start_ovn
	sleep 10
	display
        # keep the container alive
        while true; do sleep 10; done
    ;;
    *)
	echo "invalid command ${cmd}"
	echo "valid v1 commands: start-ovn display_env display ovn_debug"
	echo "valid v2 commands: ovn-northd ovn-master ovn-controller ovn-node display_env display ovn_debug"
	echo "valid v3 commands: ovs-server nb-ovsdb sb-ovsdb run-ovn-northd ovn-master ovn-controller ovn-node display_env display ovn_debug cleanup-ovs-server cleanup-ovn-node"
	exit 0
  esac

exit 0
