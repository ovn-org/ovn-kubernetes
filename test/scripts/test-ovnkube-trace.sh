#!/usr/bin/env bash
# This script will run a bunch of ovnkube-trace commands to make rudimentary
# testing easier and to provide a bunch of example pods, services and parameters
# for testing. This should help verify that ovnkube-trace still works well when changes
# are made to that binary.
# Note: Some commands are run in double, but this shouldn't do any arm besides
# having this script run for a bit longer.

# always exit on errors
set -e

if [ "${KUBECONFIG}" == "" ]; then
  export KUBECONFIG=${HOME}/ovn.conf
fi
export OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-f:pr}

DIR=$(dirname "${BASH_SOURCE[0]}")

# Get the kubectl or oc binary. Always lean towards using oc if
# that binary can be found.
KUBECTL=""
for cmd in kubectl oc ; do
  if command -v $cmd &> /dev/null; then
      KUBECTL=$(which $cmd)
  fi
done
if [ "$KUBECTL" == "" ]; then
  echo "Cannot find kubectl or oc binary, please install at least one or the other."
  exit 1
fi

LOGLEVEL=0
POST_CLEANUP=true
SETUP=true
RUN_DEST_IP_TESTS=true
OVNKUBE_TRACE=${DIR}/../../go-controller/_output/go/bin/ovnkube-trace
CONTAINER_IMAGE=alpine
EGRESS_IP=172.18.0.10
parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            --skip-post-cleanup )               POST_CLEANUP=false
                                                ;;
            --skip-setup )                      SETUP=false
                                                ;;
            --skip-dst-ip-tests )               RUN_DEST_IP_TESTS=false
                                                ;;
            --loglevel )                          shift
                                                LOGLEVEL=$1
                                                ;;
            --binary )                          shift
                                                OVNKUBE_TRACE=$1
                                                ;;
            --container-image )                 shift
                                                CONTAINER_IMAGE=$1
                                                ;;
            --egress-ip )                       shift
                                                EGRESS_IP=$1
                                                ;;
            --kubectl)                          shift
                                                kubectl_override=$1
                                                ;;
            -h | --help )                       usage
                                                exit
                                                ;;
            * )                                 usage
                                                exit 1
        esac
        shift
    done
    if [ "${kubectl_override}" != "" ]; then
        if command -v ${kubectl_override} &> /dev/null; then
            KUBECTL=$(which ${kubectl_override})
        else
            echo "Cannot find kubectl binary ${kubectl_override}."
            exit 1
        fi
    fi
}

usage() {
    echo "usage: test-ovnkube-trace.sh [--kubectl <binary name>] [--loglevel <level>] [--skip-post-cleanup] [--skip-setup] [--skip-dst-ip-tests]"
    echo "                             [--binary <location of ovnkube-trace binary>] [--container-image <image name>] [--egress-ip <egress IP, default 172.18.0.10>]"
}

# wait for deployment ready replicas count == desired replicas count
# parameters: <namespace> <deployment>
kubectl_wait_deployment(){
  # takes one deployment and makes sure its replicas and readyReplicas are equal
  local retries=0
  local attempts=30
  while true; do
    sleep 10
    run_kubectl get deployments.apps $2 -n $1
    DESIRED_REPLICAS=$(run_kubectl get deployments.apps $2 -n $1 -o=jsonpath='{.status.replicas}')
    READY_REPLICAS=$(run_kubectl get deployments.apps $2 -n $1 -o=jsonpath='{.status.readyReplicas}')
    echo "CURRENT READY REPLICAS: $READY_REPLICAS, CURRENT DESIRED REPLICAS: $DESIRED_REPLICAS for the Deployment $1/$2"
    if [[ $READY_REPLICAS -eq $DESIRED_REPLICAS ]]; then
      break
    fi
    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: deployment did not succeed, failing"
      exit 1
    fi
  done
}

# wrapper around the kubectl command
run_kubectl() {
  local retries=0
  local attempts=10
  while true; do
    if ${KUBECTL} "$@"; then
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

get_worker_nodes() {
  local worker_nodes=$(${KUBECTL} get nodes --show-labels | awk '!/node-role.kubernetes.io\/master=|node-role.kubernetes.io\/control-plane=/ && $1!="NAME" {print $1}')
  echo "${worker_nodes}"
}

# add_scc adds an SCC to the default user of the given namespace.
# If the oc binary cannot be found and/or if this is not OpenShift, simply
# continue without failing. This way, this will work for both OCP and kind.
add_scc() {
  namespace=$1
  scc=$2
  # Simply return if oc cannot be found
  if ! command -v oc &> /dev/null; then
    return
  fi
  oc adm policy add-scc-to-user ${scc} -z default -n ${namespace} 2>/dev/null
}

# Spawn a test deployment named 'pod-test' with 2 replicas in namespace 
# named 'ovn-kubetrace-pod-test'
spawn_deployment() {
  local namespace="ovn-kubetrace-pod-test"
  local name="pod-test"
  local replicas="2"

  file=$(mktemp)
  cat <<EOF >| $file
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
EOF
  run_kubectl apply -f ${file}

  file=$(mktemp)
  cat <<EOF >| $file
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: ${name}
  name: ${name}
  namespace: ${namespace}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      containers:
      - command:
        - sleep
        - "3600"
        image: ${CONTAINER_IMAGE}
        name: ${name}
EOF
  run_kubectl apply -f ${file}
  kubectl_wait_deployment ${namespace} ${name}
}

# Spawn a test deployment named 'pod-test-hostnetwork' with 2 replicas in namespace
# named 'ovn-kubetrace-pod-test-hostnetwork'. The pods of this deployment use hostNetwork=true
spawn_hostnetwork_deployment() {
  local namespace="ovn-kubetrace-pod-test-hostnetwork"
  local name="pod-test-hostnetwork"
  local replicas="2"

  file=$(mktemp)
  cat <<EOF >| $file
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
EOF
  run_kubectl apply -f ${file}
  add_scc ${namespace} hostnetwork

  file=$(mktemp)
  cat <<EOF >| $file
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: ${name}
  name: ${name}
  namespace: ${namespace}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      hostNetwork: true
      containers:
      - command:
        - sleep
        - "3600"
        image: ${CONTAINER_IMAGE}
        name: ${name}
EOF
  run_kubectl apply -f ${file}
  kubectl_wait_deployment ${namespace} ${name}
}

# Spawn a test service named 'service-test' with a test deployment named 'service-test'
# with 1 replica in namespace named 'ovn-kubetrace-service-test'
spawn_service() {
  local namespace="ovn-kubetrace-service-test"
  local name="service-test"
  local replicas="1"

  file=$(mktemp)
  cat <<EOF >| $file
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
EOF
  run_kubectl apply -f ${file}

  file=$(mktemp)
cat <<EOF >| $file
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${namespace}
  labels:
    app: ${name}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      containers:
      - name: ${name}
        image: ${CONTAINER_IMAGE}
        command:
        - sleep
        - "3600"
EOF
  run_kubectl apply -f ${file}
  kubectl_wait_deployment ${namespace} ${name}

file=$(mktemp)
cat <<EOF >| $file
apiVersion: v1
kind: Service
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  type: ClusterIP
  selector:
    app: ${name}
  ports:
    - port: 80
      targetPort: 80
EOF
  run_kubectl apply -f ${file}
}

# Create a test egressip named 'egressip-pod-test' with a test deployment
# with 2 replica in namespace named 'egressip-kubetrace-pod-test'
spawn_egressip() {
  local namespace="egressip-kubetrace-pod-test"
  local name="egressip-pod-test"
  local replicas="2"
  local egressip="$1"

  local worker_nodes=$(get_worker_nodes)
  if [ "${worker_nodes}" == "" ]; then
    echo "Could not find worker nodes for EgressIP. Exiting."
    exit 1
  fi
  for n in ${worker_nodes}; do
    ${KUBECTL} label node/$n k8s.ovn.org/egress-assignable="" --overwrite
  done

  file=$(mktemp)
  cat <<EOF >| $file
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
  labels:
    env: ${namespace}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: ${name}
  name: ${name}
  namespace: ${namespace}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      nodeSelector:
        k8s.ovn.org/egress-assignable: ""
      containers:
      - command:
        - "sleep"
        - "3600"
        image: ${CONTAINER_IMAGE}
        name: ${name}
EOF
  run_kubectl apply -f ${file}
  kubectl_wait_deployment ${namespace} ${name}

  file=$(mktemp)
  cat <<EOF >| ${file}
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: ${name}
spec:
  egressIPs: [ "${egressip}" ]
  namespaceSelector:
    matchLabels:
      env: ${namespace}
EOF
  run_kubectl apply -f ${file}
}

# Clean up all namespaces that we created and remove egress assignable labels
cleanup() {
  echo "Cleaning up namespaces"
  ${KUBECTL} delete ns ovn-kubetrace-pod-test             2>/dev/null || true
  ${KUBECTL} delete ns ovn-kubetrace-pod-test-hostnetwork 2>/dev/null || true
  ${KUBECTL} delete ns ovn-kubetrace-service-test         2>/dev/null || true
  ${KUBECTL} delete ns egressip-kubetrace-pod-test        2>/dev/null || true
  ${KUBECTL} delete egressip egressip-pod-test            2>/dev/null || true
  local worker_nodes=$(get_worker_nodes)
  if [ "${worker_nodes}" == "" ]; then
    echo "Could not find worker nodes for EgressIP. Exiting."
    exit 1
  fi
  for n in ${worker_nodes}; do
    ${KUBECTL} label node/$n k8s.ovn.org/egress-assignable- 2>/dev/null || true
  done
}

# Run ovnkube-trace
ovnkube_trace() {
    local cmd="$OVNKUBE_TRACE $@ -loglevel ${LOGLEVEL}"
    echo -e "\e[1mRunning: ${cmd}\e[0m"
    ${cmd}
}

parse_args "$@"

if [ ! -f ${OVNKUBE_TRACE} ]; then
  echo "ovnkube trace binary not found"
  exit 1
fi

if [ ${SETUP} == true ]; then
  cleanup
  spawn_deployment
  spawn_hostnetwork_deployment
  spawn_service
  spawn_egressip ${EGRESS_IP}
fi

src_pods=()
dst_pods=()
src_pods_hostnetwork=()
dst_pods_hostnetwork=()
egressip_src_pods=()
dst_svc=( "-dst-namespace ovn-kubetrace-service-test -service service-test" )

for namespace in ovn-kubetrace-pod-test; do
  pods=$(${KUBECTL} get pods -o name -n $namespace | sed 's#pod/##')
  for pod in ${pods}; do
    src_pods+=( "-src-namespace ${namespace} -src ${pod}" )
    dst_pods+=( "-dst-namespace ${namespace} -dst ${pod}" )
  done
done

for namespace in egressip-kubetrace-pod-test; do
  pods=$(${KUBECTL} get pods -o name -n $namespace | sed 's#pod/##')
  for pod in ${pods}; do
    egressip_src_pods+=( "-src-namespace ${namespace} -src ${pod}" )
  done
done

for namespace in ovn-kubetrace-pod-test-hostnetwork; do
  pods=$(${KUBECTL} get pods -o name -n $namespace | sed 's#pod/##')
  for pod in ${pods}; do
    src_pods_hostnetwork+=( "-src-namespace ${namespace} -src ${pod}" )
    dst_pods_hostnetwork+=( "-dst-namespace ${namespace} -dst ${pod}" )
  done
done

if [ $RUN_DEST_IP_TESTS == true ]; then
  echo "Run ovnkube-trace from all pod-test pods to 8.8.8.8"
  for ((i = 0; i < ${#src_pods[@]}; i++)); do
    for protocol in tcp udp; do
      ovnkube_trace ${src_pods[$i]} -dst-ip 8.8.8.8 -${protocol}
    done
  done

  echo "Run ovnkube-trace from all egressip-pod-test pods to 8.8.8.8"
  for ((i = 0; i < ${#egressip_src_pods[@]}; i++)); do
    for protocol in tcp udp; do
      ovnkube_trace ${egressip_src_pods[$i]} -dst-ip 8.8.8.8 -${protocol}
    done
  done

  echo "Run ovnkube-trace from a pod-test-hostnetwork pod to 8.8.8.8 - this should fail"
  if ovnkube_trace ${src_pods_hostnetwork[0]} -dst-ip 8.8.8.8 -${protocol}; then
    echo "The result of this test should be a failure condition"
    exit 1
  fi
fi

echo "Run ovnkube-trace from all pod-test pods to all other pod-test pods"
for ((i = 0; i < ${#src_pods[@]}; i++)); do
  for ((j = 0; j < ${#dst_pods[@]}; j++)); do
    for protocol in tcp udp; do
      ovnkube_trace ${src_pods[$i]} ${dst_pods[$j]} -${protocol}
    done
  done
done

echo "Run ovnkube-trace from all pod-test pods to all pod-test-hostnetwork pods"
for ((i = 0; i < ${#src_pods[@]}; i++)); do
  for ((j = 0; j < ${#dst_pods_hostnetwork[@]}; j++)); do
    for protocol in tcp udp; do
      ovnkube_trace ${src_pods[$i]} ${dst_pods_hostnetwork[$j]} -${protocol}
    done
  done
done

echo "Run ovnkube-trace from all pod-test pods to the service-test service via tcp"
for ((i = 0; i < ${#src_pods[@]}; i++)); do
  for ((j = 0; j < ${#dst_svc[@]}; j++)); do
    # udp with services is not a valid option
    for protocol in tcp; do
      ovnkube_trace ${src_pods[$i]} ${dst_svc[$j]} -${protocol}
    done
  done
done

echo "Run ovnkube-trace from a pod-test pod to a service-test service on udp - this should fail"
if ovnkube_trace ${src_pods[0]} ${dst_svc[0]} -udp; then
  echo "The result of this test should be a failure condition"
  exit 1
fi

if [ ${POST_CLEANUP} == true ]; then
  cleanup
fi
