#!/bin/bash
#set -euo pipefail

verify-ovsdb-raft() {
  check_ovn_daemonset_version "3"

  if [[ ${ovn_db_host} == "" ]]; then
    echo "failed to retrieve the IP address of the host $(hostname). Exiting..."
    exit 1
  fi

  replicas=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get statefulset -n ${ovn_kubernetes_namespace} ovnkube-db -o=jsonpath='{.spec.replicas}')
  if [[ ${replicas} -lt 3 || $((${replicas} % 2)) -eq 0 ]]; then
    echo "at least 3 nodes need to be configured, and it must be odd number of nodes"
    exit 1
  fi
}

bracketify() { case "$1" in *:*) echo "[$1]" ;; *) echo "$1" ;; esac }

# checks if a db pod is part of a current cluster
db_part_of_cluster() {
  local pod=${1}
  local db=${2}
  local port=${3}
  echo "Checking if ${pod} is part of cluster"
  init_ip=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get pod -n ${ovn_kubernetes_namespace} ${pod} -o=jsonpath='{.status.podIP}')
  if [[ $? != 0 ]]; then
    echo "Unable to get ${pod} ip "
    return 1
  fi
  echo "Found ${pod} ip: $init_ip"
  init_ip=$(bracketify $init_ip)
  target=$(ovn-${db}ctl --timeout=5 --db=${transport}:${init_ip}:${port} ${ovndb_ctl_ssl_opts} \
               --data=bare --no-headings --columns=target list connection 2>&1)
  if [[ "x${target}" != "xp${transport}:${port}${ovn_raft_conn_ip_url_suffix}" ]]; then
    echo "Unable to check correct target ${target} "
    return 1
  fi

  echo "${pod} is part of cluster"
  return 0
}

# Checks if cluster has already been initialized.
# If not it returns false and sets init_ip to ovnkube-db-0
cluster_exists() {
  # See if ep is available ...
  local db=${1}
  local port=${2}

  db_pods=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get pod -n ${ovn_kubernetes_namespace} -o=jsonpath='{.items[*].metadata.name}' | egrep -o 'ovnkube-db[^ ]+')

  for db_pod in $db_pods; do
    if db_part_of_cluster $db_pod $db $port; then
      echo "${db_pod} is part of current cluster with ip: ${init_ip}!"
      return 0
    fi
  done

  # if we get here  there is no cluster, set init_ip and get out
  init_ip="$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get pod -n ${ovn_kubernetes_namespace} ovnkube-db-0 -o=jsonpath='{.status.podIP}')"
  if [[ $? != 0 ]]; then
    return 1
  fi

  init_ip=$(bracketify $init_ip)
  return 1
}

check_ovnkube_db_ep() {
  local dbaddr=${1}
  local dbport=${2}

  # TODO: Right now only checks for NB ovsdb instances
  echo "======= checking [${dbaddr}]:${dbport} OVSDB instance ==============="
  ovsdb-client ${ovndb_ctl_ssl_opts} list-dbs ${transport}:[${dbaddr}]:${dbport} >/dev/null
  if [[ $? != 0 ]]; then
    return 1
  fi
  return 0
}

check_and_apply_ovnkube_db_ep() {
  local port=${1}

  # return if ovn db service endpoint already exists
  result=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
      get ep -n ${ovn_kubernetes_namespace} ovnkube-db 2>&1)
  test $? -eq 0 && return
  if ! echo ${result} | grep -q "NotFound"; then
      echo "Failed to find ovnkube-db endpoint: ${result}, Exiting..."
      exit 12
  fi
  # Get IPs of all ovnkube-db PODs
  ips=()
  for ((i = 0; i < ${replicas}; i++)); do
    ip=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
      get pod -n ${ovn_kubernetes_namespace} ovnkube-db-${i} -o=jsonpath='{.status.podIP}')
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

    for ip in ${ips[@]}; do
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

change_election_timer_wait() {
  local db=${1}
  local election_timer=${2}
  local result=""

  while true; do
    result=$(ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/change-election-timer ${database} ${election_timer} 2>&1)
    test $? -eq 0 && break
    echo "ovs-appctl: $result"
    if ! echo $result|grep -q "change pending"; then
      echo "Failed to set election timer ${election_timer}. Exiting..."
      exit 11
    fi
    sleep 1
  done
}

# election timer can only be at most doubled each time, and it can only be set on the leader
set_election_timer() {
  local db=${1}
  local election_timer=${2}
  local current_election_timer

  echo "setting election timer for ${database} to ${election_timer} ms"

  current_election_timer=$(ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/status ${database} |
    grep "Election timer" | sed "s/.*:[[:space:]]//")
  if [[ -z "${current_election_timer}" ]]; then
    echo "Failed to get current election timer value. Exiting..."
    exit 11
  fi

  while [[ ${current_election_timer} != ${election_timer} ]]; do
    max_election_timer=$((${current_election_timer} * 2))
    if [[ ${election_timer} -le ${max_election_timer} ]]; then
      change_election_timer_wait $db $election_timer
      return 0
    else
      change_election_timer_wait $db $max_election_timer
      current_election_timer=${max_election_timer}
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
set_connection() {
  local db=${1}
  local port=${2}
  local target
  local output

  # this call will fail on non-leader node since we are using unix socket and no --no-leader-only option.
  output=$(ovn-${db}ctl --data=bare --no-headings --columns=target,inactivity_probe list connection)
  if [[ $? == 0 ]]; then
    # this instance is a leader, check if we need to make any changes
    echo "found the current value of target and inactivity probe to be ${output}"
    target=$(echo "${output}" | awk 'ORS=","')
    if [[ "${target}" != "p${transport}:${port}${ovn_raft_conn_ip_url_suffix},0," ]]; then
      ovn-${db}ctl --inactivity-probe=0 set-connection p${transport}:${port}${ovn_raft_conn_ip_url_suffix}
      if [[ $? != 0 ]]; then
        echo "Failed to set connection and disable inactivity probe. Exiting...."
        exit 12
      fi
      echo "added port ${port} to connection table and disabled inactivity probe"
    fi
  fi
  return 0
}

set_northd_probe_interval() {
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

ovsdb_cleanup() {
  local db=${1}
  ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl exit >/dev/null 2>&1
  kill $(jobs -p) >/dev/null 2>&1
  exit 0
}

# v3 - create nb_ovsdb/sb_ovsdb cluster in a separate container
ovsdb-raft() {
  local db=${1}
  local port=${2}
  local raft_port=${3}
  local election_timer=${4}
  local initialize="false"

  ovn_db_pidfile=${OVN_RUNDIR}/ovn${db}_db.pid
  eval ovn_loglevel_db=\$ovn_loglevel_${db}
  ovn_db_file=${OVN_ETCDIR}/ovn${db}_db.db

  trap 'ovsdb_cleanup ${db}' TERM
  rm -f ${ovn_db_pidfile}

  verify-ovsdb-raft
  echo "=============== run ${db}-ovsdb-raft pod ${POD_NAME} =========="

  if [[ ! -e ${ovn_db_file} ]] || ovsdb-tool db-is-standalone ${ovn_db_file}; then
    initialize="true"
  fi

  local db_ssl_opts=""
  if [[ ${db} == "nb" ]]; then
    database="OVN_Northbound"
    [[ "yes" == ${OVN_SSL_ENABLE} ]] && {
      db_ssl_opts="
            --ovn-nb-db-ssl-key=${ovn_nb_pk}
            --ovn-nb-db-ssl-cert=${ovn_nb_cert}
            --ovn-nb-db-ssl-ca-cert=${ovn_ca_cert}
      "
    }
  else
    database="OVN_Southbound"
    [[ "yes" == ${OVN_SSL_ENABLE} ]] && {
      db_ssl_opts="
            --ovn-sb-db-ssl-key=${ovn_sb_pk}
            --ovn-sb-db-ssl-cert=${ovn_sb_cert}
            --ovn-sb-db-ssl-ca-cert=${ovn_ca_cert}
      "
    }
  fi

  # 'ovn-${db}ctl set-connection' called in ovsdb-raft() defaults to IPv4.
  # For IPv6, overwrite with ":[::]"
  ovn_raft_conn_ip_url_suffix=""
  if [[ "${ovn_db_host}" == *":"* ]]; then
    ovn_raft_conn_ip_url_suffix=":[::]"
  fi

  if [[ "${initialize}" == "true" ]]; then
    # check to see if a cluster already exists. If it does, just join it.
    counter=0
    cluster_found=false
    while [ $counter -lt 5 ]; do
      if cluster_exists ${db} ${port}; then
        cluster_found=true
        break
      fi
      sleep 1
      counter=$((counter+1))
    done

    if ${cluster_found}; then
      echo "Cluster already exists for DB: ${db}"
      run_as_ovs_user_if_needed \
      ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
      --db-${db}-cluster-local-addr=$(bracketify ${ovn_db_host}) --db-${db}-cluster-remote-addr=${init_ip} \
      --db-${db}-cluster-local-port=${raft_port} --db-${db}-cluster-remote-port=${raft_port} \
      --db-${db}-cluster-local-proto=${transport} --db-${db}-cluster-remote-proto=${transport} \
      ${db_ssl_opts} \
      --ovn-${db}-log="${ovn_loglevel_db}" &
    else
      # either we need to initialize a new cluster or wait for db-0 to create it
      if [[ "${POD_NAME}" == "ovnkube-db-0" ]]; then
        echo "Cluster does not exist for DB: ${db}, creating new raft cluster"
        run_as_ovs_user_if_needed \
        ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
        --db-${db}-cluster-local-addr=$(bracketify ${ovn_db_host}) \
        --db-${db}-cluster-local-port=${raft_port} \
        --db-${db}-cluster-local-proto=${transport} \
        ${db_ssl_opts} \
        --ovn-${db}-log="${ovn_loglevel_db}" &
      else
        echo "Cluster does not exist for DB: ${db}, waiting for ovnkube-db-0 pod to create it"
        # all non pod-0 pods will be blocked here till connection is set
        wait_for_event cluster_exists ${db} ${port}
        run_as_ovs_user_if_needed \
        ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
        --db-${db}-cluster-local-addr=$(bracketify ${ovn_db_host}) --db-${db}-cluster-remote-addr=${init_ip} \
        --db-${db}-cluster-local-port=${raft_port} --db-${db}-cluster-remote-port=${raft_port} \
        --db-${db}-cluster-local-proto=${transport} --db-${db}-cluster-remote-proto=${transport} \
        ${db_ssl_opts} \
        --ovn-${db}-log="${ovn_loglevel_db}" &
      fi
    fi
  else
    # in this case the cluster was already previously joined
    # so we simply start without specifying the remote addr because
    #  a) we can't reliably know who the raft leader is
    #  b) we don't need to pass that information since it's already
    #     in the db
    run_as_ovs_user_if_needed \
    ${OVNCTL_PATH} run_${db}_ovsdb --no-monitor \
    --db-${db}-cluster-local-addr=[${ovn_db_host}] \
    --db-${db}-cluster-local-port=${raft_port} \
    --db-${db}-cluster-local-proto=${transport} \
    ${db_ssl_opts} \
    --ovn-${db}-log="${ovn_loglevel_db}" &
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
    # post raft create work has to be done only once and in ovnkube-db-0 while it is still
    # a single-node cluster, additional protection against the case when pod-0 isn't a leader
    # is needed in the cases of sudden pod-0 initialization logic restarts
    current_raft_role=$(ovs-appctl -t ${OVN_RUNDIR}/ovn${db}_db.ctl cluster/status ${database} 2>&1 | grep "^Role")
    if [[ -z "${current_raft_role}" ]]; then
      echo "Failed to get current raft role value. Exiting..."
      exit 11
    fi
    if echo $current_raft_role | grep -q -i leader; then
      # set the election timer value before other servers join the cluster
      set_election_timer ${db} ${election_timer}
      if [[ ${db} == "nb" ]]; then
        set_northd_probe_interval
      fi
      # set the connection and disable inactivity probe, this deletes the old connection if any
      # this will unblock pod-1 and pod-2 waiters
      set_connection ${db} ${port}
    fi
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
