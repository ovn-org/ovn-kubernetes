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
#
# The following variables are REQUIRED:
# K8S_APISERVER - hostname:port (URL)of the real apiserver, not the service address
# OVN_NET_CIDR - the network cidr
# OVN_SVC_CIDR - the cluster-service-cidr
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
# OVNKUBE_LOGLEVEL - log level for ovnkube (0..5, default 4)
# OVN_LOG_NORTHD - log level (ovn-ctl default: -vconsole:emer -vsyslog:err -vfile:info)
# OVN_LOG_NB - log level (ovn-ctl default: -vconsole:off -vfile:info)
# OVN_LOG_SB - log level (ovn-ctl default: -vconsole:off -vfile:info)

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

logdir=/var/log/openvswitch
logpost=$(date +%F-%T)
ovn_nb_log_file=${logdir}/ovsdb-server-nb-${logpost}.log
ovn_sb_log_file=${logdir}/ovsdb-server-sb-${logpost}.log

# ovnkube.sh version (update when script changes - v.x.y)
ovnkube_version="3"

# The daemonset version must be compatible with this script.
# The default when OVN_DAEMONSET_VERSION is not set is version 1
ovn_daemonset_version=${OVN_DAEMONSET_VERSION:-"1"}

# hostname is the host's hostname when using host networking,
# otherwise it is the container ID (useful for debugging).
ovn_host=$(hostname)

# The ovs user id
ovs_user_id=${OVS_USER_ID:-root:root}

# ovs options
ovs_options=${OVS_OPTIONS:-""}

if [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]
then
  k8s_token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
else
  k8s_token=${K8S_TOKEN}
fi

K8S_CACERT=${K8S_CACERT:-/var/run/secrets/kubernetes.io/serviceaccount/ca.crt}

# ovn-northd - /etc/sysconfig/ovn-northd
ovn_northd_opts=${OVN_NORTHD_OPTS:-"--db-nb-sock=/var/run/openvswitch/ovnnb_db.sock --db-sb-sock=/var/run/openvswitch/ovnsb_db.sock"}

# ovn-controller
#OVN_CONTROLLER_OPTS="--ovn-controller-log=-vconsole:emer --vsyslog:err -vfile:info"
ovn_controller_opts=${OVN_CONTROLLER_OPTS:-"--ovn-controller-log=-vconsole:info"}

# set the log level for ovnkube
ovnkube_loglevel=${OVNKUBE_LOGLEVEL:-4}



# =========================================

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

# ovs must be up before ovn comes up
# This waits for ovs to come up
wait_for_ovs () {

  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null
    if [[ $? != 0 ]] ; then
      (( retries += 1 ))
      if [[ "${retries}" -gt 40 ]]; then
        echo "error: ovs did not come up, exiting"
        exit 1
      fi
      echo "info: Waiting for ovs to come up, waiting 10s ..."
      sleep 10
    else
      if [[ "${retries}" != 0 ]]; then
        echo "ovs came up in ${retries} 10sec tries"
      fi
      break
    fi
  done
}


# ovn must be up before ovnkube --master
# This waits for northd to come up
wait_for_northd () {

  ovn_nbdb_test=$(echo ${OVN_NORTH} | sed 's;//;;')
  # Wait for ovn-northd to come up
  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    # northd is up when this works
    ovn-nbctl --db=${ovn_nbdb_test} show > /dev/null 2>&1
    if [[ $? != 0 ]] ; then
      ovn-nbctl show > /dev/null 2>&1
      if [[ $? != 0 ]] ; then
        (( retries += 1 ))
        if [[ "${retries}" -gt 40 ]]; then
          echo "error: ovn-northd did not come up, exiting"
          exit 1
        fi
        echo "info: Waiting for ovn-northd to come up, waiting 10s ..."
        sleep 10
      else
        if [[ "${retries}" != 0 ]]; then
          echo "ovn-northd came up in ${retries} 10sec tries"
        fi
        break
      fi
    else
      if [[ "${retries}" != 0 ]]; then
        echo "ovn-northd came up in ${retries} 10sec tries"
      fi
      break
    fi
  done
}

# wait for the pid file to appear (daemon is up)
# $1 is the pid file fqn
wait_for_pid () {
  pidfile=${1}
  retries=0
  while true ; do
    if [[ -f ${pidfile} ]] ; then
      if [[ "${retries}" != 0 ]]; then
        echo "Pid file appeared after ${retries} 10sec tries"
      fi
      break
    fi
    (( retries += 1 ))
    if [[ "${retries}" -gt 40 ]]; then
      echo "error: pid file did not appear in 400 seconds, exiting"
      exit 1
    fi
    echo "info: waiting 10s for pid file to appear"
    sleep 10
  done
  return 0
}

# check process health, exit if process terminates
# $1 is the pid file fqn
pid_health () {
  pid=$(cat $1)
  while true; do
    pidTest=$(ps ax | grep "^${pid:-XX}" | awk '{ print $1 }')
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
  echo "==================== display for ${ovn_host}  =================== "
  date
  latest=$(ls -t ${logdir}/ovsdb-server-nb-*.log | head -1)
  display_file "nb-ovsdb" /var/run/openvswitch/ovnnb_db.pid ${latest}
  latest=$(ls -t ${logdir}/ovsdb-server-sb-*.log | head -1)
  display_file "sb-ovsdb" /var/run/openvswitch/ovnsb_db.pid ${latest}
  display_file "run-ovn-northd" /var/run/openvswitch/ovn-northd.pid ${logdir}/ovn-northd.log
  display_file "ovn-master" /var/run/openvswitch/ovnkube-master.pid ${logdir}/ovnkube-master.log
  display_file "ovs-vswitchd" /var/run/openvswitch/ovs-vswitchd.pid ${logdir}/ovs-vswitchd.log
  display_file "ovsdb-server" /var/run/openvswitch/ovsdb-server.pid ${logdir}/ovsdb-server.log
  display_file "ovn-controller" /var/run/openvswitch/ovn-controller.pid ${logdir}/ovn-controller.log
  display_file "ovnkube" /var/run/openvswitch/ovnkube.pid ${logdir}/ovnkube.log
}

setup_cni () {
  # Take over network functions on the node
  # rm -Rf /etc/cni/net.d/*
  cp -f /usr/libexec/cni/ovn-k8s-cni-overlay /host/opt/cni/bin/ovn-k8s-cni-overlay
  if [[ ! -f /host/opt/cni/bin/loopback ]]
  then
    cp -f /usr/libexec/cni/loopback /host/opt/cni/bin/loopback
  fi
}

display_version () {
  echo " =================== hostname: ${ovn_host}"
  echo " =================== daemonset version ${ovn_daemonset_version}"
  if [[ -f /root/.git/HEAD ]]
  then
    commit=$(gawk '{ print $1 }' /root/.git/HEAD )
    if [[ ${commit} == "ref:" ]]
    then
      head=$(gawk '{ print $2 }' /root/.git/HEAD )
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
echo OVN_NORTH ${OVN_NORTH}
echo OVN_NORTHD_OPTS ${ovn_northd_opts}
echo OVN_SOUTH ${OVN_SOUTH}
echo OVN_CONTROLLER_OPTS ${ovn_controller_opts}
echo OVN_NET_CIDR ${OVN_NET_CIDR}
echo OVN_SVC_CIDR ${OVN_SVC_CIDR}
echo K8S_APISERVER ${K8S_APISERVER}
echo OVNKUBE_LOGLEVEL ${ovnkube_loglevel}
echo OVN_DAEMONSET_VERSION ${ovn_daemonset_version}
echo ovnkube.sh version ${ovnkube_version}
}

ovn_debug () {
  # get ovs/ovn info from the node for debug purposes
  echo "=========== ovn_debug   hostname: ${ovn_host} ============="
  echo "=========== ovn-nbctl show ============="
  ovn_nbdb_test=$(echo ${OVN_NORTH} | sed 's;//;;')
  echo "=========== ovn-nbctl --db=${ovn_nbdb_test} show ============="
  ovn-nbctl --db=${ovn_nbdb_test} show
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
  echo "=========== MASTER NODE ==========="
  echo "=========== ovn-sbctl show ============="
  ovn_sbdb_test=$(echo ${OVN_SOUTH} | sed 's;//;;')
  echo "=========== ovn-sbctl --db=${ovn_sbdb_test} show ============="
  ovn-sbctl --db=${ovn_sbdb_test} show
  echo " "
  echo "=========== ovn-sbctl lflow-list ============="
  ovn-sbctl lflow-list
  echo " "
  echo "=========== ovn-sbctl list datapath ============="
  ovn-sbctl list datapath
  echo " "
  echo "=========== ovn-sbctl list port ============="
  ovn-sbctl list port
}

ovs-server () {
  # start ovs ovsdb-server and ovs-vswitchd
  set -euo pipefail

  # if another process is listening on the cni-server socket, wait until it exits
  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    if /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null; then
      echo "warning: Another process is currently managing OVS, waiting 15s ..." 2>&1
      sleep 15 & wait
      (( retries += 1 ))
    else
      break
    fi
    if [[ "${retries}" -gt 40 ]]; then
      echo "error: Another process is currently managing OVS, exiting" 2>&1
      exit 1
    fi
  done

  # launch OVS
  function quit {
      /usr/share/openvswitch/scripts/ovs-ctl stop
      exit 1
  }
  trap quit SIGTERM
  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovs-vswitchd \
    --system-id=random --ovs-user=${ovs_user_id} ${ovs_options}

  # Restrict the number of pthreads ovs-vswitchd creates to reduce the
  # amount of RSS it uses on hosts with many cores
  # https://bugzilla.redhat.com/show_bug.cgi?id=1571379
  # https://bugzilla.redhat.com/show_bug.cgi?id=1572797
  if [[ `nproc` -gt 12 ]]; then
      ovs-vsctl --no-wait set Open_vSwitch . other_config:n-revalidator-threads=4
      ovs-vsctl --no-wait set Open_vSwitch . other_config:n-handler-threads=10
  fi
  /usr/share/openvswitch/scripts/ovs-ctl start --no-ovsdb-server \
    --system-id=random --ovs-user=${ovs_user_id} ${ovs_options}

  # Ensure GENEVE's UDP port isn't firewalled
  /usr/share/openvswitch/scripts/ovs-ctl --protocol=udp --dport=6081 enable-protocol

  tail --follow=name /var/log/openvswitch/ovs-vswitchd.log /var/log/openvswitch/ovsdb-server.log &
  ovs_tail_pid=$!
  sleep 20
  while true; do
    if ! /usr/share/openvswitch/scripts/ovs-ctl status &>/dev/null; then
      echo "OVS seems to have crashed, exiting"
      kill ${ovs_tail_pid}
      quit
    fi
    sleep 15
  done
}

# v3 - run nb_ovsdb in a separate container
nb-ovsdb () {
  check_ovn_daemonset_version "3"
  # this is only run on masters in a separate container
  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  echo "=============== run nb_ovsdb ========== MASTER ONLY"
  echo "ovn_log_nb=${ovn_log_nb} ovn_nb_log_file=${ovn_nb_log_file}"
  /usr/share/openvswitch/scripts/ovn-ctl run_nb_ovsdb --no-monitor \
  --ovn-nb-logfile=${ovn_nb_log_file} --ovn-nb-log="${ovn_log_nb}"
  echo "=============== run nb_ovsdb ========== terminated"
}

# v3 - run sb_ovsdb in a separate container
sb-ovsdb () {
  check_ovn_daemonset_version "3"
  # this is only run on masters in a separate container
  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  echo "=============== run sb_ovsdb ========== MASTER ONLY"
  echo "ovn_log_sb=${ovn_log_sb} ovn_sb_log_file=${ovn_sb_log_file}"
  /usr/share/openvswitch/scripts/ovn-ctl run_sb_ovsdb --no-monitor \
  --ovn-sb-logfile=${ovn_sb_log_file} --ovn-sb-log="${ovn_log_sb}"
  echo "=============== run sb_ovsdb ========== terminated"
}


# v3 - Runs northd. Does not run nb_ovsdb, and sb_ovsdb
run-ovn-northd () {
  check_ovn_daemonset_version "3"
  # this is only run on masters
  # Make sure /var/lib/openvswitch exists
  mkdir -p /var/lib/openvswitch

  echo "=============== ovnkube-master (wait for ovs) ========== MASTER ONLY"
  wait_for_ovs

  echo "=============== run_ovn_northd ========== MASTER ONLY"
  echo "OVN_NORTH=${OVN_NORTH}  OVN_SOUTH==${OVN_SOUTH}"
  echo "ovn_northd_opts=${ovn_northd_opts}"
  echo "ovn_log_northd=${ovn_log_northd}"
  # no monitor (and no detach), start nb_ovsdb and sb_ovsdb in separate containers
  /usr/share/openvswitch/scripts/ovn-ctl start_northd \
    --no-monitor --ovn-manage-ovsdb=no \
    --ovn-northd-log=${ovn_log_northd} \
    --db-nb-addr=${OVN_NORTH} --db-sb-addr=${OVN_SOUTH} \
    ${ovn_northd_opts}
  echo "=============== run_ovn_northd ========== RUNNING"

  tail --follow=name /var/log/openvswitch/ovn-northd.log &
  ovn_tail_pid=$!

  wait_for_pid /var/run/openvswitch/ovn-northd.pid

  pid_health /var/run/openvswitch/ovn-northd.pid ${ovn_tail_pid}
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
    echo "=============== ovn-northd ========== MASTER ONLY"
    echo "OVN_NORTH=${OVN_NORTH}  OVN_SOUTH==${OVN_SOUTH} ovn_northd_opts=${ovn_northd_opts}"
    /usr/share/openvswitch/scripts/ovn-ctl start_northd \
      --db-nb-addr=${OVN_NORTH} --db-sb-addr=${OVN_SOUTH} \
      ${ovn_northd_opts}
    echo "=============== ovn-northd ========== FAILED"
    cat /var/log/openvswitch/ovn-northd.log
    exit 4
  fi
}

# v2 v3 - run ovnkube --master
ovn-master () {
  check_ovn_daemonset_version "2 3"
  # this is only run on masters
  # ovn-master - master node only
  echo "=============== ovn-master (wait for ovs) ========== MASTER ONLY"
  wait_for_ovs

  echo "=============== ovn-master (wait for northd) ========== MASTER ONLY"
  wait_for_northd

  echo "=============== ovn-master ========== MASTER ONLY"
  /usr/bin/ovnkube \
    --init-master ${ovn_host} --net-controller \
    --cluster-subnet ${OVN_NET_CIDR} --service-cluster-ip-range=${OVN_SVC_CIDR} \
    --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${K8S_CACERT} \
    --nb-address=${OVN_NORTH} --sb-address=${OVN_SOUTH} \
    --nodeport \
    --loglevel=${ovnkube_loglevel} \
    --pidfile /var/run/openvswitch/ovnkube-master.pid \
    --logfile /var/log/openvswitch/ovnkube-master.log &
  echo "=============== ovn-master ========== running"

  sleep 10
  tail --follow=name /var/log/openvswitch/ovnkube-master.log &
  kube_tail_pid=$!

  wait_for_pid /var/run/openvswitch/ovnkube-master.pid

  pid_health /var/run/openvswitch/ovnkube-master.pid ${kube_tail_pid}
}

ovn-controller () {
  check_ovn_daemonset_version "2 3"
  # ovn-controller - all nodes
  echo "=============== ovn-controller - (wait for ovs)"
  wait_for_ovs

  echo "=============== ovn-controller"
  rm -f /var/run/ovn-kubernetes/cni/*
  rm -f /var/run/openvswitch/ovn-controller.*.ctl
  /usr/share/openvswitch/scripts/ovn-ctl --no-monitor start_controller \
    ${ovn_controller_opts}
  echo "=============== ovn-controller ========== running"
  #/var/run/openvswitch/ovn-controller.pid /var/log/openvswitch/ovn-controller.log

  sleep 10
  tail --follow=name /var/log/openvswitch/ovn-controller.log &
  controller_tail_pid=$!

  wait_for_pid /var/run/openvswitch/ovn-controller.pid

  pid_health /var/run/openvswitch/ovn-controller.pid ${controller_tail_pid}
}


ovn-node () {
  check_ovn_daemonset_version "2 3"
  # ovn-node - all nodes
  echo "=============== ovn-node - (wait for ovs)"
  wait_for_ovs

  echo "=============== ovn-node - (wait for northd)"
  wait_for_northd

  echo "=============== ovn-node"
  # TEMP HACK - WORKAROUND
  # --init-gateways --gateway-localnet works around a problem that
  # results in loss of network connectivity when docker is
  # restarted or ovs daemonset is deleted.
  # TEMP HACK - WORKAROUND
  /usr/bin/ovnkube --init-node ${K8S_NODE} \
      --cluster-subnet ${OVN_NET_CIDR} --service-cluster-ip-range=${OVN_SVC_CIDR} \
      --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${K8S_CACERT} \
      --nb-address=${OVN_NORTH} --sb-address=${OVN_SOUTH} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --init-gateways --gateway-localnet \
      --pidfile /var/run/openvswitch/ovnkube.pid \
      --logfile /var/log/openvswitch/ovnkube.log &
  echo "=============== ovn-node ========== running"
  sleep 10
  tail --follow=name /var/log/openvswitch/ovnkube.log &
  node_tail_pid=$!

  wait_for_pid /var/run/openvswitch/ovnkube.pid

  pid_health /var/run/openvswitch/ovnkube.pid ${node_tail_pid}
}

# version 1 daemonset compatibility
# $1 is "" or "start_ovn"
start_ovn () {
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
  wait_for_ovs

  # on the master only
  if [[ ${ovn_master} = "true" ]]
  then
    # Make sure /var/lib/openvswitch exists
    mkdir -p /var/lib/openvswitch
    # ovn-northd - master node only
    echo "=============== start ovn-northd ========== MASTER ONLY"
    echo OVN_NORTH=${OVN_NORTH}  OVN_SOUTH==${OVN_SOUTH} ovn_northd_opts=${ovn_northd_opts}
    rm -f /var/run/openvswitch/ovn-northd.*.ctl
    /usr/share/openvswitch/scripts/ovn-ctl start_northd --no-monitor \
      --db-nb-addr=${OVN_NORTH} --db-sb-addr=${OVN_SOUTH} \
      ${ovn_northd_opts}

    # ovn-master - master node only
    echo "=============== start ovn-master (wait for northbd) ========== MASTER ONLY"
    wait_for_northd
    echo "=============== start ovn-master ========== MASTER ONLY"
    /usr/bin/ovnkube \
      --init-master ${ovn_host} --net-controller \
      --cluster-subnet ${OVN_NET_CIDR} --service-cluster-ip-range=${OVN_SVC_CIDR} \
      --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${K8S_CACERT} \
      --nb-address=${OVN_NORTH} --sb-address=${OVN_SOUTH} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --pidfile /var/run/openvswitch/ovnkube-master.pid \
      --logfile /var/log/openvswitch/ovnkube-master.log &
  fi

  # ovn-controller - all nodes
  echo "=============== start ovn-controller (wait for northd)"
  wait_for_northd
  echo "=============== start ovn-controller"
  rm -f /var/run/ovn-kubernetes/cni/*
  /usr/share/openvswitch/scripts/ovn-ctl --no-monitor start_controller \
    ${ovn_controller_opts}

  # ovn-node - all nodes
  echo  "=============== start ovn-node"
  # TEMP HACK - WORKAROUND
  # --init-gateways --gateway-localnet works around a problem that
  # results in loss of network connectivity when docker is
  # restarted or ovs daemonset is deleted.
  # TEMP HACK - WORKAROUND
  /usr/bin/ovnkube --init-node ${K8S_NODE} \
      --cluster-subnet ${OVN_NET_CIDR} --service-cluster-ip-range=${OVN_SVC_CIDR} \
      --k8s-token=${k8s_token} --k8s-apiserver=${K8S_APISERVER} --k8s-cacert=${K8S_CACERT} \
      --nb-address=${OVN_NORTH} --sb-address=${OVN_SOUTH} \
      --nodeport \
      --loglevel=${ovnkube_loglevel} \
      --init-gateways --gateway-localnet \
      --pidfile /var/run/openvswitch/ovnkube.pid \
      --logfile /var/log/openvswitch/ovnkube.log &

  echo "=============== done starting daemons ================="
}

echo "================== ovnkube.sh --- version: ${ovnkube_version} ================"

  echo " ==================== command: ${cmd}"
  display_version
  display_env


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

  case ${cmd} in
    "ovs-server")
        ovs-server
    ;;
    "nb-ovsdb")
	nb-ovsdb
    ;;
    "sb-ovsdb")
	sb-ovsdb
    ;;
    "run-ovn-northd")
	run-ovn-northd
    ;;
    "ovn-northd")
	ovn-northd
    ;;
    "ovn-master")
	ovn-master
    ;;
    "ovn-controller")
	ovn-controller
    ;;
    "ovn-node")
        setup_cni
	ovn-node
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
	echo "valid commands (v1): start-ovn display_env display ovn_debug"
	echo "valid commands (v2): ovn-northd ovn-master ovn-controller ovn-node display_env display ovn_debug"
	echo "valid commands (v3): ovs-server nb-ovsdb sb-ovsdb run-ovn-northd ovn-master ovn-controller ovn-node display_env display ovn_debug"
	exit 0
  esac

exit 0
