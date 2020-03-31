#!/bin/bash
#set -euo pipefail

verify-ovsdb-raft () {
  check_ovn_daemonset_version "3"

  replicas=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get statefulset -n ${ovn_kubernetes_namespace} ovnkube-db -o=jsonpath='{.spec.replicas}')
  if [[ ${replicas} -lt 3 || $((${replicas} % 2)) -eq 0 ]]; then
    echo "at least 3 nodes need to be configured, and it must be odd number of nodes"
    exit 1
  fi
}

# OVN DB must be up in the first DB node
# This waits for ovnkube-db-0 POD to come up
ready_to_join_cluster () {
  # See if ep is available ...
  db=${1}
  port=${2}

  init_ip="$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get pod -n ${ovn_kubernetes_namespace} ovnkube-db-0 -o=jsonpath='{.status.podIP}')"
  if [[ $? != 0 ]]; then
    return 1
  fi
  target=$(ovn-${db}ctl --db=tcp:${init_ip}:${port} --data=bare --no-headings --columns=target list connection 2>/dev/null)
  if [[ "${target}" != "ptcp:${port}" ]] ; then
    return 1
  fi
  return 0
}

check_ovnkube_db_ep () {
  local dbaddr=${1}
  local dbport=${2}

  # TODO: Right now only checks for NB ovsdb instances
  echo "======= checking ${dbaddr}:${dbport} OVSDB instance ==============="
  ovsdb-client list-dbs tcp:${dbaddr}:${dbport} > /dev/null 2>&1
  if [[ $? != 0 ]] ; then
      return 1
  fi
  return 0
}

check_and_apply_ovnkube_db_ep () {
  local port=${1}

  # Get IPs of all ovnkube-db PODs
  ips=()
  for (( i=0; i<${replicas}; i++ )); do
    ip=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
      get pod -n ${ovn_kubernetes_namespace} ovnkube-db-${i} -o=jsonpath='{.status.podIP}' 2>/dev/null)
    if [[ ${ip} == "" ]]; then
      break
    fi
    ips+=(${ip})
  done

  if [[ ${i} -eq ${replicas} ]]; then
    # Number of POD IPs is same as number of statefulset replicas. Now, if the number of ovnkube-db endpoints
    # is 0, then we are applying the endpoint for the first time. So, we need to make sure that each of the
    # pod IP responds to the `ovsdb-client list-dbs` call before we set the endpoint. If they don't, retry several
    # times and then give up.

    # Get the current set of ovnkube-db endpoints, if any
    IFS=" " read -a old_ips <<< "$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
      get ep -n ${ovn_kubernetes_namespace} ovnkube-db -o=jsonpath='{range .subsets[0].addresses[*]}{.ip}{" "}' 2>/dev/null)"
    if [[ ${#old_ips[@]} -ne 0 ]]; then
      return
    fi

    for ip in ${ips[@]} ; do
      wait_for_event attempts=10 check_ovnkube_db_ep ${ip} ${port}
    done
    set_ovnkube_db_ep ${ips[@]}
  else
    # ideally shouldn't happen
    echo "Not all the pods in the statefulset are up. Expecting ${replicas} pods, but found ${i} pods."
    echo "Exiting...."
    exit 10
  fi
}

# election timer can only be at most doubled each time, and it can only be set on the leader
set_election_timer () {
  local election_timer=${1}
  local current_election_timer

  echo "setting election timer for ${database} to ${election_timer} ms"

  current_election_timer=$(ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/status ${database} 2>/dev/null \
    | grep "Election" | sed "s/.*:[[:space:]]//")
  if [[ -z "${current_election_timer}" ]]; then
    echo "Failed to get current election timer value. Exiting..."
    exit 11
  fi

  while [[ ${current_election_timer} != ${election_timer} ]]; do
    max_electinon_timer=$((${current_election_timer} * 2))
    if [[ ${election_timer} -le ${max_electinon_timer} ]]; then
      ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/change-election-timer ${database} ${election_timer}
      if [[ $? != 0 ]]; then
        echo "Failed to set election timer ${election_timer}. Exiting..."
        exit 11
      fi
      return 0
    else
      ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/change-election-timer ${database} ${max_electinon_timer}
      if [[ $? != 0 ]]; then
        echo "Failed to set election timer ${max_election_timer}. Exiting..."
        exit 11
      fi
      current_election_timer=${max_electinon_timer}
    fi
  done
  return 0
}

# set_connection() will be called for ovnkube-db-0 pod when :
#    1. it is first started or
#    2. it restarts after the initial start has failed or
#    3. subsequent restarts during the lifetime of the pod
#
# In the first and second case, the pod is a one-node cluster and hence a leader. In the third case,
# the pod is a part of mutli-pods cluster and may not be a leader and the connection information should
# have already been set, so we don't care.
set_connection () {
  local port=${1}
  local target
  local output

  # this call will fail on non-leader node since we are using unix socket and no --no-leader-only option.
  output=$(ovn-${db}ctl --data=bare --no-headings --columns=target,inactivity_probe list connection 2>/dev/null)
  if [[ $? == 0 ]]; then
    # this instance is a leader, check if we need to make any changes
    echo "found the current value of target and inactivity probe to be ${output}"
    target=$(echo "${output}" | awk 'ORS=","')
    if [[ "${target}" != "ptcp:${port},0," ]]; then
      ovn-${db}ctl --inactivity-probe=0 set-connection ptcp:${port}
      if [[ $? != 0 ]]; then
        echo "Failed to set connection and disable inactivity probe. Exiting...."
        exit 12
      fi
      echo "added port ${port} to connection table and disabled inactivity probe"
    fi
  fi
  return 0
}

set_northd_probe_interval () {
  # OVN_NORTHD_PROBE_INTERVAL - probe interval of northd for NB and SB DB
  # connections in ms (default 5000)
  northd_probe_interval=${OVN_NORTHD_PROBE_INTERVAL:-5000}

  echo "setting northd probe interval to ${northd_probe_interval} ms"

  # this call will fail on non-leader node since we are using unix socket and no --no-leader-only option.
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

# v3 - create nb_ovsdb/sb_ovsdb cluster in a separate container
ovsdb-raft () {
  trap 'kill $(jobs -p); exit 0' TERM

  local db=${1}
  local port=${2}
  local raft_port=${3}
  local election_timer=${4}
  local initialize="false"

  ovn_db_pidfile=${OVN_RUNDIR}/ovn${db}_db.pid
  eval ovn_log_db=\$ovn_log_${db}
  ovn_db_file=${OVN_ETCDIR}/ovn${db}_db.db
  database="OVN_Northbound"
  if [[ ${db} == "sb" ]]; then
    database="OVN_Southbound"
  fi

  rm -f ${ovn_db_pidfile}

  verify-ovsdb-raft
  local_ip=$(getent ahostsv4 $(hostname) | grep -v "^127\." | head -1 | awk '{ print $1 }')
  if [[ ${local_ip} == "" ]] ; then
      echo "failed to retrieve the IP address of the host $(hostname). Exiting..."
      exit 1
  fi
  echo "=============== run ${db}-ovsdb-raft pod ${POD_NAME} =========="

  if [[ ! -e ${ovn_db_file} ]] || ovsdb-tool db-is-standalone ${ovn_db_file} ; then
    initialize="true"
  fi
  if [[ "${POD_NAME}" == "ovnkube-db-0" ]]; then
    run_as_ovs_user_if_needed \
      ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
      --db-${db}-cluster-local-addr=${local_ip} \
      --db-${db}-cluster-local-port=${raft_port} \
      --ovn-${db}-log="${ovn_log_db}" &
  else
    # join the remote cluster node if the DB is not created
    if [[ "${initialize}" == "true" ]]; then
      wait_for_event ready_to_join_cluster ${db} ${port}
    fi
    run_as_ovs_user_if_needed \
      ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
      --db-${db}-cluster-local-addr=${local_ip} --db-${db}-cluster-remote-addr=${init_ip} \
      --db-${db}-cluster-local-port=${raft_port} --db-${db}-cluster-remote-port=${raft_port} \
      --ovn-${db}-log="${ovn_log_db}" &
  fi

  # Following command waits for the database on server to enter a `connected` state
  # -- Waits until a database with the given name has been added to server. Then, if database
  # is clustered, additionally waits until it has joined and connected to its cluster.
  echo "waiting for ${database} to join and connect to the cluster."
  /usr/bin/ovsdb-client -t 120 wait unix:${OVN_RUNDIR}/ovn${db}_db.sock ${database} connected
  if [[ $? != 0 ]]; then
    echo "the ${database} has not yet joined and connected to its cluster. Exiting..."
    exit 1
  fi
  echo "=============== ${db}-ovsdb-raft ========== RUNNING"

  if [[ "${POD_NAME}" == "ovnkube-db-0" ]]; then
    # set the election timer value before other servers join the cluster and it can
    # only be set on the leader so we must do this in ovnkube-db-0 when it is still
    # a single-node cluster
    set_election_timer ${election_timer}
    if [[ ${db} == "nb" ]]; then
      set_northd_probe_interval
    fi
    # set the connection and disable inactivity probe, this deletes the old connection if any
    set_connection ${port}
  fi

  last_node_index=$(expr ${replicas} - 1)
  # Create endpoints only if all ovnkube-db pods have started and are running. We do this
  # from the last pod of the statefulset.
  if [[ ${db} == "nb" && "${POD_NAME}" == "ovnkube-db-"${last_node_index} ]]; then
    check_and_apply_ovnkube_db_ep ${port}
  fi

  tail --follow=name ${OVN_LOGDIR}/ovsdb-server-${db}.log &
  ovn_tail_pid=$!

  process_healthy ovn${db}_db ${ovn_tail_pid}
  echo "=============== run ${db}_ovsdb-raft ========== terminated"
}

# v3 - Runs ovn-kube-util in daemon mode to export prometheus metrics related to OVN clustered db
db-raft-metrics() {
  check_ovn_daemonset_version "3"

  echo "=============== db-raft-metrics - (wait for ready_to_start_node)"
  wait_for_event ready_to_start_node

  ovndb_exporter_bind_address=${OVNDB_EXPORTER_BIND_ADDRESS:-"0.0.0.0:9476"}

  /usr/bin/ovn-kube-util \
    --loglevel=${ovnkube_loglevel} \
    ovn-db-exporter \
    --metrics-bind-address ${ovndb_exporter_bind_address}

  echo "=============== db-raft-metrics with pid ${?} terminated ========== "
  exit 1
}
