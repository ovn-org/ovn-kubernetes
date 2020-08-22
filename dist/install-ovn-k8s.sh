#!/bin/bash -ex
# shellcheck disable=SC2016

SCRIPTS_DIR=$(dirname "${BASH_SOURCE[0]}")

function wait_for {
  # Execute in a subshell to prevent local variable override during recursion
  (
    local total_attempts=$1; shift
    local cmdstr=$*
    local sleep_time=2
    echo -e "\n[wait_for] Waiting for cmd to return success: ${cmdstr}"
    # shellcheck disable=SC2034
    for attempt in $(seq "${total_attempts}"); do
      echo "[wait_for] Attempt ${attempt}/${total_attempts%.*} for: ${cmdstr}"
      # shellcheck disable=SC2015
      eval "${cmdstr}" && echo "[wait_for] OK: ${cmdstr}" && return 0 || true
      sleep "${sleep_time}"
    done
    echo "[wait_for] ERROR: Failed after max attempts: ${cmdstr}"
    return 1
  )
}

# Create OVN namespace, service accounts, ovnkube-db headless service, configmap, and policies
kubectl create -f ${SCRIPTS_DIR}/yaml/ovn-setup.yaml
wait_for 5 'test $(kubectl get svc -n ovn-kubernetes | grep ovnkube-db -c ) -eq 1'


# Run ovnkube-db daemonset.
kubectl create -f ${SCRIPTS_DIR}/yaml/ovnkube-db.yaml
wait_for 60 'test $(kubectl get pods -n ovn-kubernetes | grep -e "ovnkube-db" | grep "Running" -c) -eq 1'


# Run ovnkube-master daemonset.
kubectl create -f ${SCRIPTS_DIR}/yaml/ovnkube-master.yaml
wait_for 60 'test $(kubectl get pods -n ovn-kubernetes | grep -e "ovnkube-master" | grep "Running" -c) -eq 1'


# Run ovnkube daemonsets for nodes, maybe more than 1 ovnkube-node pods since there would be 1 ovnkube-node
# pod on each K8s node
kubectl create -f ${SCRIPTS_DIR}/yaml/ovnkube-node.yaml
wait_for 60 'test $(kubectl get pods -n ovn-kubernetes | grep -e "ovnkube-node" | grep "Running" -c) -ge 1'


kubectl get pods -n ovn-kubernetes
