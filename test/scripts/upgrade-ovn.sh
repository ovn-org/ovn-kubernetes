#!/usr/bin/env bash

# always exit on errors
set -ex


export KUBECONFIG=${HOME}/ovn.conf
export OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-f:pr}

kubectl_wait_pods() {
  # Check that everything is fine and running. IPv6 cluster seems to take a little
  # longer to come up, so extend the wait time.
  OVN_TIMEOUT=900s
  if [ "$KIND_IPV6_SUPPORT" == true ]; then
    OVN_TIMEOUT=1400s
  fi
  if ! kubectl wait -n ovn-kubernetes --for=condition=ready pods --all --timeout=${OVN_TIMEOUT} ; then
    echo "some pods in OVN Kubernetes are not running"
    kubectl get pods -A -o wide || true
    kubectl describe po -n ovn-kubernetes
    exit 1
  fi
  if ! kubectl wait -n kube-system --for=condition=ready pods --all --timeout=300s ; then
    echo "some pods in the system are not running"
    kubectl get pods -A -o wide || true
    kubectl describe po -A
    exit 1
  fi
}

kubectl_wait_daemonset(){
  # takes one daemonset and makes sure its desiredNumberScheduled and numberReady are equal
  local retries=0
  local attempts=15
  while true; do
    sleep 30
    run_kubectl get daemonsets.apps $1 -n ovn-kubernetes
    DESIRED_REPLICAS=$(run_kubectl get daemonsets.apps $1 -n ovn-kubernetes -o=jsonpath='{.status.desiredNumberScheduled}')
    READY_REPLICAS=$(run_kubectl get daemonsets.apps $1 -n ovn-kubernetes -o=jsonpath='{.status.numberReady}')
    echo "CURRENT READY REPLICAS: $READY_REPLICAS, CURRENT DESIRED REPLICAS: $DESIRED_REPLICAS for the DaemonSet $1"
    if [[ $READY_REPLICAS -eq $DESIRED_REPLICAS ]]; then
      UP_TO_DATE_REPLICAS=$(run_kubectl get daemonsets.apps ovnkube-node -n ovn-kubernetes  -o=jsonpath='{.status.updatedNumberScheduled}')
      echo "CURRENT UP TO DATE REPLICAS: $UP_TO_DATE_REPLICAS for the Deployment $1"
      if [[ $READY_REPLICAS -eq $UP_TO_DATE_REPLICAS ]]; then
        break
      fi
    fi
    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: daemonset did not succeed, failing"
      exit 1
    fi
  done
  
}

kubectl_wait_deployment(){
  # takes one deployment and makes sure its replicas and readyReplicas are equal
  local retries=0
  local attempts=30
  while true; do
    sleep 30
    run_kubectl get deployments.apps $1 -n ovn-kubernetes 
    DESIRED_REPLICAS=$(run_kubectl get deployments.apps $1 -n ovn-kubernetes -o=jsonpath='{.status.replicas}')
    READY_REPLICAS=$(run_kubectl get deployments.apps $1 -n ovn-kubernetes -o=jsonpath='{.status.readyReplicas}')
    echo "CURRENT READY REPLICAS: $READY_REPLICAS, CURRENT DESIRED REPLICAS: $DESIRED_REPLICAS for the Deployment $1"
    if [[ $READY_REPLICAS -eq $DESIRED_REPLICAS ]]; then
      UP_TO_DATE_REPLICAS=$(run_kubectl get deployments.apps ovnkube-master -n ovn-kubernetes -o=jsonpath='{.status.updatedReplicas}')
      echo "CURRENT UP TO DATE REPLICAS: $UP_TO_DATE_REPLICAS for the Deployment $1"
      if [[ $READY_REPLICAS -eq $UP_TO_DATE_REPLICAS ]]; then
        break
      fi
    fi
    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: deployment did not succeed, failing"
      exit 1
    fi
  done
  
}

run_kubectl() {
  local retries=0
  local attempts=10
  while true; do
    if kubectl "$@"; then
      break
    fi

    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: 'kubectl $*' did not succeed, failing"
      exit 1
    fi
    echo "info: waiting for 'kubectl $*' to succeed..."
    sleep 1
  done
}


install_ovn_image() {
  kind load docker-image "${OVN_IMAGE}" --name "${KIND_CLUSTER_NAME}"
}

kubectl_wait_for_upgrade(){
    # waits until new image is updated into all relevant pods
    count=0
    while [ $count -lt 5 ];
    do
        echo "waiting for ovnkube-master, ovnkube-node, ovnkube-db to have new image, ${OVN_IMAGE}, sleeping 30 seconds"
        sleep 30
        count=$(run_kubectl get pods -n ovn-kubernetes -o=jsonpath='{range .items[*]}{"\n"}{.metadata.name}{":\t"}{range .spec.containers[*]}{.image}{", "}{end}{end}'|grep -c ${OVN_IMAGE})
        echo "Currently count is $count, expected 5"
    done;

}

## This script is responsible to upgrade ovn daemonsets to run new pods with image built from a PR
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn} # Set default values
install_ovn_image
run_kubectl set image daemonsets.apps ovnkube-node ovnkube-node="${OVN_IMAGE}" ovs-metrics-exporter="${OVN_IMAGE}"  ovn-controller="${OVN_IMAGE}" -n ovn-kubernetes
kubectl_wait_daemonset ovnkube-node

run_kubectl get all -n ovn-kubernetes
CURRENT_REPLICAS_OVNKUBE_DB=$(run_kubectl get deploy -n ovn-kubernetes ovnkube-db -o=jsonpath='{.spec.replicas}')
run_kubectl scale deploy -n ovn-kubernetes ovnkube-db --replicas=0
run_kubectl set image deploy  ovnkube-db nb-ovsdb="${OVN_IMAGE}" sb-ovsdb="${OVN_IMAGE}" -n ovn-kubernetes
run_kubectl scale deploy -n ovn-kubernetes ovnkube-db --replicas=$CURRENT_REPLICAS_OVNKUBE_DB
kubectl_wait_deployment ovnkube-db

CURRENT_REPLICAS_OVNKUBE_MASTER=$(run_kubectl get deploy -n ovn-kubernetes ovnkube-master -o=jsonpath='{.spec.replicas}')

# scaling down replica before changing image briefly helps get around an issue seen with KIND
# The issue was sometimes the KIND cluster won't scale down the ovnkube-master pod in time
# and the new pod with the new image would be stuck in "Pending" state
run_kubectl scale deploy -n ovn-kubernetes ovnkube-master --replicas=0

run_kubectl set image deploy  ovnkube-master ovn-northd="${OVN_IMAGE}" nbctl-daemon="${OVN_IMAGE}" ovnkube-master="${OVN_IMAGE}" -n ovn-kubernetes

run_kubectl scale deploy -n ovn-kubernetes ovnkube-master --replicas=$CURRENT_REPLICAS_OVNKUBE_MASTER
kubectl_wait_deployment ovnkube-master
kubectl_wait_for_upgrade

run_kubectl describe ds ovnkube-node -n ovn-kubernetes

run_kubectl describe deployments.apps ovnkube-master -n ovn-kubernetes
