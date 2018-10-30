#!/bin/bash
#set -x
#set -euo pipefail

# This script is the entrypoint to the image.
# Supports version 2 and version 1 daemonsets
#    $1 is the daemon to start. In version 2 the daemonset has a container for
#        each daemon. This start script is called with the desired daemon.
#        The script waits of prerquisite deamons to come up first.
#        The default, with $1 == "" is to start all daemons one after the other
#        waiting for a daemon to start before starting the next.
# Commands ($1 values)
#    ovs-server     Runs the ovs daemons - ovsdb-server and ovs-switchd
#    ovn-northd     Runs ovn-northd daemon
#    ovn-master     Runs ovnkube in master mode
#    ovn-controller Runs ovn controller
#    ovn-node       Runs ovnkube in node mode
#    display        Displays log files
#    display_env    Displays environment variables
#    ovn_debug      Displays ovn/ovs configuration and flows
#    <no argumet>   Runs all daemons - version 1 compatibility

# NOTE: The script/image must be compatible with the daemonset.
# This script supports version 1 daemonsets
#      When called, it starts all needed daemons.

# ====================
# Environment variables are used to customize operation
#
# The following variables are REQUIRED:
# K8S_APISERVER - hostname:port of the real apiserver, not the service address
# OVN_NET_CIDR - the network cidr
# OVN_SVC_CIDR - the cluster-service-cidr
# OVN_NORTH - the full URL to the ovn northdb
# OVN_SOUTH - the full URL to the ovn southdb
#
# Optional:
# OVN_MASTER - whether or not to run the master processes
# K8S_TOKEN - the apiserver token. Automatically detected when running in a pod
# K8S_CACERT - the apiserver CA. Automatically detected when running in a pod
# OVN_CONTROLLER_OPTS - the options for ovn-ctl
# OVN_NORTHD_OPTS - the options for the ovn northbound db
# OVNKUBE_LOGLEVEL - log level for ovnkube (0..5, default 4)

# The argument to the command is the operation to be performed
# ovn-northd ovn-master ovn-controller ovn-node display display_env ovn_debug
# default is compatibility mode with version 1 daemonsets
cmd=${1:-"start-ovn"}

# There is a single image for both master nodes and compute nodes
# When OVN_MASTER is true, start the master daemons
# in addition to the node daemons
ovn_master=${OVN_MASTER:-"false"}

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
ovn_controller_opts=${OVN_CONTROLLER_OPTS:-"--ovn-controller-log=-vconsole:emer"}

# set the log level for ovnkube
ovnkube_loglevel=${OVNKUBE_LOGLEVEL:-4}

# =========================================

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
wait_for_northdb () {

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

display () {
  echo ${ovn_host}
  date
  if [[ ${ovn_master} = "true" ]]
  then
    echo "==================== ovnkube-master =================== "
    echo "====================== ovnnb_db.pid"
    if [[ -f /var/run/openvswitch/ovnnb_db.pid ]]
    then
      cat /var/run/openvswitch/ovnnb_db.pid
    fi
    echo " "
    echo "====================== ovnsb_db.pid"
    if [[ -f /var/run/openvswitch/ovnsb_db.pid ]]
    then
      cat /var/run/openvswitch/ovnsb_db.pid
    fi
    echo " "
    echo "====================== ovs-vswitchd.pid"
    if [[ -f /var/run/openvswitch/ovs-vswitchd.pid ]]
    then
      cat /var/run/openvswitch/ovs-vswitchd.pid
    fi
    echo " "
    echo "====================== ovsdb-server.pid"
    if [[ -f /var/run/openvswitch/ovsdb-server.pid ]]
    then
      cat /var/run/openvswitch/ovsdb-server.pid
    fi
    echo " "
    echo "====================== ovsdb-server-nb.log"
    if [[ -f /var/log/openvswitch/ovsdb-server-nb.log ]]
    then
      cat /var/log/openvswitch/ovsdb-server-nb.log
    fi
    echo " "
    echo "====================== ovsdb-server-sb.log "
    if [[ -f /var/log/openvswitch/ovsdb-server-sb.log ]]
    then
      cat /var/log/openvswitch/ovsdb-server-sb.log
    fi
    echo " "
    echo "====================== ovn-northd.pid"
    if [[ -f /var/run/openvswitch/ovn-northd.pid ]]
    then
      cat /var/run/openvswitch/ovn-northd.pid
    fi
    echo " "
    echo "====================== ovn-northd.log"
    if [[ -f /var/log/openvswitch/ovn-northd.log ]]
    then
      cat /var/log/openvswitch/ovn-northd.log
    fi
    echo " "
    echo "====================== ovnkube-master.pid"
    if [[ -f /var/run/openvswitch/ovnkube-master.pid ]]
    then
      cat /var/run/openvswitch/ovnkube-master.pid
    fi
    echo " "
    echo "====================== ovnkube-master.log"
    if [[ -f /var/log/openvswitch/ovnkube-master.log ]]
    then
      cat /var/log/openvswitch/ovnkube-master.log
    fi
  fi
  echo " "
  echo "==================== ovnkube =================== "
  echo "====================== ovn-controller.pid"
  if [[ -f /var/run/openvswitch/ovn-controller.pid ]]
  then
    cat /var/run/openvswitch/ovn-controller.pid
  fi
  echo " "
  echo "====================== ovn-controller.log"
  if [[ -f /var/log/openvswitch/ovn-controller.log ]]
  then
    cat /var/log/openvswitch/ovn-controller.log
  fi
  echo " "
  echo "====================== ovnkube.pid"
  if [[ -f /var/run/openvswitch/ovnkube.pid ]]
  then
    cat /var/run/openvswitch/ovnkube.pid
  fi
  echo " "
  echo "====================== ovnkube.log"
  if [[ -f /var/log/openvswitch/ovnkube.log ]]
  then
    cat /var/log/openvswitch/ovnkube.log
  fi
  echo " "
  echo "====================== ovn-k8s-cni-overlay.log"
  if [[ -f /var/log/openvswitch/ovn-k8s-cni-overlay.log ]]
  then
    cat /var/log/openvswitch/ovn-k8s-cni-overlay.log
  fi
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
  echo "==================== hostname: ${ovn_host} "
  echo "==================== compatible with version 1  and version 2 daemonsets"
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
    echo "Image built from ovn-kubernetes ref: ${head}  commit: ${commit}"
  fi
}

display_env () {
echo OVS_USER_ID ${ovs_user_id}   OVS_OPTIONS ${ovs_options}
echo OVN_NORTH ${OVN_NORTH}       OVN_NORTHD_OPTS ${ovn_northd_opts}
echo OVN_SOUTH ${OVN_SOUTH}
echo OVN_CONTROLLER_OPTS ${ovn_controller_opts}
echo OVN_NET_CIDR ${OVN_NET_CIDR}
echo OVN_SVC_CIDR ${OVN_SVC_CIDR}
echo K8S_APISERVER ${K8S_APISERVER}
echo OVNKUBE_LOGLEVEL ${ovnkube_loglevel}
}

ovn_debug () {
  # get ovs/ovn info from the node for debug purposes
  echo "=========== ovn_debug ============="
  if [[ ${ovn_master} = "true" ]]
  then
    echo "=========== MASTER NODE ==========="
  fi
  display_version
  echo " "
  echo "=========== ovn-nbctl show ============="
  ovn-nbctl show
  echo " "
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
  if [[ ${ovn_master} = "true" ]]
  then
    echo " "
    echo "=========== MASTER NODE ==========="
    echo " "
    echo "=========== ovn-sbctl lflow-list ============="
    ovn-sbctl lflow-list
    echo " "
    echo "=========== ovn-sbctl list datapath ============="
    ovn-sbctl list datapath
    echo " "
    echo "=========== ovn-sbctl list port ============="
    ovn-sbctl list port
  fi
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
      exit 0
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
}

ovn-northd () {
  # this is only run on masters
  if [[ ${ovn_master} = "true" ]]; then
    echo "=============== start ovn-northd  ========== MASTER ONLY"
    # Make sure /var/lib/openvswitch exists
    mkdir -p /var/lib/openvswitch
    # ovn-northd - master node only
    echo "=============== start ovn-northd ========== MASTER ONLY"
    echo OVN_NORTH=${OVN_NORTH}  OVN_SOUTH==${OVN_SOUTH} ovn_northd_opts=${ovn_northd_opts}
    /usr/share/openvswitch/scripts/ovn-ctl start_northd \
      --db-nb-addr=${OVN_NORTH} --db-sb-addr=${OVN_SOUTH} \
      ${ovn_northd_opts}
  fi
}

ovn-master () {
  # this is only run on masters
  if [[ ${ovn_master} = "true" ]]; then
    # ovn-master - master node only
    echo "=============== start ovn-master (wait for ovs) ========== MASTER ONLY"
    wait_for_ovs

    echo "=============== start ovn-master (wait for northdb) ========== MASTER ONLY"
    wait_for_northdb

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
}

ovn-controller () {
  # ovn-controller - all nodes
  echo "=============== start ovn-controller (wait for ovs)"
  wait_for_ovs

  echo "=============== start ovn-controller - (wait for northdb)"
  wait_for_northdb

  echo "=============== start ovn-controller"
  rm -f /var/run/ovn-kubernetes/*
  /usr/share/openvswitch/scripts/ovn-ctl --no-monitor start_controller \
    ${ovn_controller_opts}
}


ovn-node () {
  # ovn-node - all nodes
  echo "=============== start ovn-node (wait for ovs)"
  wait_for_ovs

  echo "=============== start ovn-node - (wait for northdb)"
  wait_for_northdb

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
}

# version 1 daemonset compatibility
# $1 is "" or "start_ovn"
start_ovn () {

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
    /usr/share/openvswitch/scripts/ovn-ctl start_northd \
      --db-nb-addr=${OVN_NORTH} --db-sb-addr=${OVN_SOUTH} \
      ${ovn_northd_opts}

    # ovn-master - master node only
    echo "=============== start ovn-master (wait for northbd) ========== MASTER ONLY"
    wait_for_northdb
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
  echo "=============== start ovn-controller (wait for northdb)"
  wait_for_northdb
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

echo "================== ovnkube.sh ================"

  echo " ==================== command: ${cmd}"
  echo " ==================== hostname: ${ovn_host} "


# Start the requested daemons
# daemons come up in order
# ovs-db-server  - all nodes  -- not done by this script
# ovs-vswitchd   - all nodes  -- not done by this script
# ovn-northd     - master node only
# ovn-master     - master node only
# ovn-controller - all nodes
# ovn-node       - all nodes

  case ${cmd} in
    "ovs-server")
        display_version
        display_env
        ovs-server
    ;;
    "ovn-northd")
        display_version
        display_env
	ovn-northd
    ;;
    "ovn-master")
        display_version
        display_env
	ovn-master
    ;;
    "ovn-controller")
        display_version
        display_env
	ovn-controller
    ;;
    "ovn-node")
        display_version
        display_env
        setup_cni
	ovn-node
	sleep 10
	display
    ;;
    "display_env")
	display_version
	display_env
	exit 0
    ;;
    "display")
	display_version
	display
	exit 0
    ;;
    "ovn_debug")
	display_version
	ovn_debug
	exit 0
    ;;
    # This is being deprecated
    # daemonset version 1 compatibility mode
    "start-ovn")
        display_version
        display_env
        setup_cni
	start_ovn
	sleep 10
	display
    ;;
    *)
	echo "invalid command ${cmd}"
	echo "valid commands: ovs-server ovn-northd ovn-master ovn-controller ovn-node display_env display ovn_debug start-ovn"
	exit 0
  esac

# keep the container alive
while true; do sleep 10; done

exit 0
