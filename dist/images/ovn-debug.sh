#!/bin/bash
#set -x
set -euo pipefail

# This script helps debug the container.

# Determine the ovn rundir.
if [[ -f /usr/bin/ovn-appctl ]] ; then
	# ovn-appctl is present. Use new ovn run dir path.
	OVN_RUNDIR=/var/run/ovn
	OVNCTL_PATH=/usr/share/ovn/scripts/ovn-ctl
	OVN_LOGDIR=/var/log/ovn
else
	# ovn-appctl is not present. Use openvswitch run dir path.
	OVN_RUNDIR=/var/run/openvswitch
	OVNCTL_PATH=/usr/share/openvswitch/scripts/ovn-ctl
	OVN_LOGDIR=/var/log/openvswitch
fi

# ====================
# Environment variables are used to customize operation

# There is a single image for both master node and compute node
# setup. When OVN_MASTER is true, start the master daemons
# in addition to the node daemons
ovn_master=${OVN_MASTER:-"false"}

# hostname is the host's hostname when using host networking,
# otherwise it is the container ID (useful for debugging).
ovn_host=$(hostname)

# Cluster's internal network cidr
ovn_cidr=${OVN_CIDR:-"10.128.0.0/14"}

# Used to test for ovn-northd coming up
ovn_nbdb=${OVN_TEST_NDB:-"tcp:10.19.188.22:6641"}

# ovn-northd - /etc/sysconfig/ovn-northd
ovn_northd_opts=${OVN_NORTHD_OPTS:-"--db-nb-sock=${OVN_RUNDIR}/ovnnb_db.sock --db-sb-sock=${OVN_RUNDIR}/ovnsb_db.sock"}

# ovn-controller
#OVN_CONTROLLER_OPTS="--ovn-controller-log=-vconsole:emer --vsyslog:err -vfile:info"
ovn_controller_opts=${OVN_CONTROLLER_OPTS:-"--ovn-controller-log=-vconsole:emer"}

# set the default values for the daemon and the command to be run for that daemon.
daemon=${1:-"all"}
cmd=${2:-"check"}


# =========================================

# Master must be up before the nodes can come up.
# This waits for northd to come up
wait_for_northdb () {
  # Wait for ovn-northd to come up
  trap 'kill $(jobs -p); exit 0' TERM
  retries=0
  while true; do
    # northd is up when this works
    out=$(ovn-nbctl --db${ovn_nbdb} show >/dev/null)
    if [[ ${out} -ne 0 ]] ; then
      echo "info: Waiting for ovn-northd to come up, waiting 10s ..." 2>&1
      sleep 10 & wait
      (( retries += 1 ))
    else
      break
    fi
    if [[ "${retries}" -gt 40 ]]; then
      echo "error: ovn-northd did not come up, exiting" 2>&1
      exit 1
    else
      echo "ovn-northd came up in ${retries} 10sec tries"
    fi
  done
}


# Master control of ovn daemons
# daemons come up in order
# ovn-northd - master node only
# ovn-master - master node only
# ovn-controller - all nodes
# ovn-node - all nodes

# All services have start | stop | reload | check | logs

ovn-northd () {
  case $1 in
  "start") echo "ovn-northd - START"
	 if [ ! -f ${OVN_RUNDIR}/ovn-northd.pid ] ; then
	   ${OVNCTL_PATH} start_northd ${ovn_northd_opts}
	 else
	   echo "ovn-northd already running"
	 fi
	 ;;
  "stop") echo "ovn-northd - STOP"
	 if [ -f ${OVN_RUNDIR}/ovn-northd.pid ] ; then
	   ${OVNCTL_PATH} stop_northd
	 else
	   echo "ovn-northd already stopped"
	 fi
	 ;;
  "reload") echo "ovn-northd - RELOAD"
	 ;;
  "check")
	 if [ -f ${OVN_RUNDIR}/ovn-northd.pid ] ; then
	   echo "ovn-northd  - running"
	 else
	   echo "ovn-northd  - stopped"
	 fi
	 return 0
	 ;;
  "logs") echo "ovn-northd - LOGS"
	 echo "============ ovsdb-server-nb.log ======================"
	 cat ${OVN_LOGDIR}/ovsdb-server-nb.log
	 echo "============ ovsdb-server-sb.log ======================"
	 cat ${OVN_LOGDIR}/ovsdb-server-sb.log
	 echo "============ ovs-northd.log ==========================="
	 cat ${OVN_LOGDIR}/ovn-northd.log
	 ;;
  "debug") echo "ovn-northd - DEBUG"
	 if [ -f ${OVN_RUNDIR}/ovn-northd.pid ] ; then
	   echo -n "ovnnb_db.pid:   "
	   cat ${OVN_RUNDIR}/ovnnb_db.pid
	   echo -n "ovnsb_db.pid:   "
	   cat ${OVN_RUNDIR}/ovnsb_db.pid
	   echo -n "ovn-northd.pid: "
	   cat ${OVN_RUNDIR}/ovn-northd.pid
	 fi
	 echo "============ ovn-northd processes ====================="
	 ps ax | grep -e ovnnb_db -e ovnsb_db -e ovn-northd | grep -v color=auto
	 ;;
  *) echo "ovn-northd - unknown arg $1" ; return 1 ;;
  esac
}

ovn-master () {
  case $1 in
  "start") echo "ovn-master - START"
	 if [ ! -f ${OVN_RUNDIR}/ovnkube-master.pid ] ; then
	 /usr/bin/ovnkube \
           --cluster-subnets "${ovn_cidr}" \
           --init-master ${ovn_host} \
	   --pidfile ${OVN_RUNDIR}/ovnkube-master.pid \
	   --logfile /var/log/ovn-kubernetes/ovnkube-master.log &
	 fi
	 ;;
  "stop") echo "ovn-master - STOP"
	 if [ -f ${OVN_RUNDIR}/ovnkube-master.pid ] ; then
	   echo "STOP ovn-master"
	   kill `cat ${OVN_RUNDIR}/ovnkube-master.pid`
	 else
	   echo "ovn-master already stopped"
	 fi
	 ;;
  "reload") echo "ovn-master - RELOAD"
	 ;;
  "check")
	 if [ -f ${OVN_RUNDIR}/ovnkube-master.pid ] ; then
	   echo "ovn-master  - running"
	 else
	   echo "ovn-master  - stopped"
	 fi
	 ;;
  "logs") echo "ovn-master - LOGS"
	 echo "============ ovnkube-master.log ======================="
	 cat /var/log/ovn-kubernetes/ovnkube-master.log
	 ;;
  "debug") echo "ovn-master - DEBUG"
	if [ -f ${OVN_RUNDIR}/ovnkube-master.pid ] ; then
	 echo -n "ovn-master pid: "
	 cat ${OVN_RUNDIR}/ovnkube-master.pid
	fi
	 echo "============ ovn-master processes ====================="
	 ;;
  *) echo "ovn-master - unknown arg $1" ; return 1 ;;
  esac
}

ovn-controller () {
  case $1 in
  "start") echo "ovn-controller - START"
	 if [ ! -f ${OVN_RUNDIR}/ovn-controller.pid ] ; then
	 ${OVNCTL_PATH} --no-monitor \
          start_controller ${ovn_controller_opts}
	 else
	   echo "ovn-controller already running"
	 fi
	 ;;
  "stop") echo "ovn-controller - STOP"
	 if [ -f ${OVN_RUNDIR}/ovn-controller.pid ] ; then
	  ${OVNCTL_PATH} stop_controller
	 else
	   echo "ovn-controller already stopped"
	 fi
	 ;;
  "reload") # echo "ovn-controller - RELOAD"
	 ;;
  "check")
	 if [ -f ${OVN_RUNDIR}/ovn-controller.pid ] ; then
	   echo "ovn-controller  - running"
	 else
	   echo "ovn-controller  - stopped"
	 fi
	 ;;
  "logs") echo "ovn-controller - LOGS"
	 echo "============ ovn-controller.log ======================="
	 cat ${OVN_LOGDIR}/ovn-controller.log
	 ;;
  "debug") echo "ovn-controller - DEBUG"
	 echo "============ ovn-controller processes ================="
	 cat ${OVN_RUNDIR}/ovn-controller.pid
	 ;;
  *) echo "ovn-controller - unknown arg $1" ; return 1 ;;
  esac
}

ovn-node () {
  case $1 in
  "start") echo "ovn-node - START"
	 /usr/bin/ovnkube \
        --cluster-subnets "${ovn_cidr}" \
        --init-node "${ovn_host}" &
	 ;;
  "stop") echo "ovn-node - STOP"
	 ;;
  "reload") echo "ovn-node - RELOAD"
	 ;;
  "check") echo "ovn-node - CHECK"
	 ;;
  "logs") echo "ovn-node - LOGS"
	 ;;
  "debug") echo "ovn-node - DEBUG"
	 ;;
  *) echo "ovn-node - unknown arg $1" ; return 1 ;;
  esac
}

all () {
  case $1 in
  "start") echo "all - START"
	 ;;
  "stop") echo "all - STOP"
	 ;;
  "reload") echo "all - RELOAD"
	 ;;
  "check") echo "all - CHECK"
	 ovn-northd $1
	 ovn-master $1
	 ovn-controller $1
	 ovn-node $1
	 ;;
  "logs") echo "all - LOGS"
	 ovn-northd $1
	 ovn-master $1
	 ovn-controller $1
	 ovn-node $1
	 ;;
  "debug") echo "all - DEBUG"
	 ovn-northd $1
	 ovn-master $1
	 ovn-controller $1
	 ovn-node $1
	 ;;
  *) echo "all - unknown arg $1" ; return 1 ;;
  esac
}

echo ================== ovn-debug.sh ================

echo $0: daemon: ${daemon}  command: ${cmd}
case ${daemon} in
"ovn-northd") echo "${daemon} - ${cmd}" ; ovn-northd ${cmd} ;;
"ovn-master") echo "${daemon} - ${cmd}" ; ovn-master ${cmd} ;;
"ovn-controller") echo "${daemon} - ${cmd}" ; ovn-controller ${cmd} ;;
"ovn-node") echo "${daemon} - ${cmd}" ; ovn-node ${cmd} ;;
"all") echo "${daemon} - ${cmd}" ; all ${cmd} ;;
*) echo "[all|ovn-northd|ovn-master|ovn-controller|ovn-node] [start|stop|reload|check|logs|debug]" ;;
esac

exit 0
