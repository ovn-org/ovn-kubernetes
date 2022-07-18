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

create_ovn_kube_manifests() {
  pushd ../dist/images
  ./daemonset.sh \
    --image="${OVN_IMAGE}" \
    --net-cidr="${NET_CIDR}" \
    --svc-cidr="${SVC_CIDR}" \
    --gateway-mode="${OVN_GATEWAY_MODE}" \
    --hybrid-enabled="${OVN_HYBRID_OVERLAY_ENABLE}" \
    --disable-snat-multiple-gws="${OVN_DISABLE_SNAT_MULTIPLE_GWS}" \
    --disable-pkt-mtu-check="${OVN_DISABLE_PKT_MTU_CHECK}" \
    --ovn-empty-lb-events="${OVN_EMPTY_LB_EVENTS}" \
    --multicast-enabled="${OVN_MULTICAST_ENABLE}" \
    --k8s-apiserver="${API_URL}" \
    --ovn-master-count="${KIND_NUM_MASTER}" \
    --ovn-unprivileged-mode=no \
    --master-loglevel="${MASTER_LOG_LEVEL}" \
    --node-loglevel="${NODE_LOG_LEVEL}" \
    --dbchecker-loglevel="${DBCHECKER_LOG_LEVEL}" \
    --ovn-loglevel-northd="${OVN_LOG_LEVEL_NORTHD}" \
    --ovn-loglevel-nb="${OVN_LOG_LEVEL_NB}" \
    --ovn-loglevel-sb="${OVN_LOG_LEVEL_SB}" \
    --ovn-loglevel-controller="${OVN_LOG_LEVEL_CONTROLLER}" \
    --egress-ip-enable=true \
    --egress-firewall-enable=true \
    --v4-join-subnet="${JOIN_SUBNET_IPV4}" \
    --v6-join-subnet="${JOIN_SUBNET_IPV6}" \
    --ex-gw-network-interface="${OVN_EX_GW_NETWORK_INTERFACE}" \
    --in-upgrade
  popd
}

set_default_ovn_manifest_params() {
  # Set default values
  # kind configs 
  KIND_IPV4_SUPPORT=${KIND_IPV4_SUPPORT:-true}
  KIND_IPV6_SUPPORT=${KIND_IPV6_SUPPORT:-false}
  OVN_HA=${OVN_HA:-false}
  # ovn configs 
  OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-shared}
  OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  OVN_DISABLE_SNAT_MULTIPLE_GWS=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-false}
  OVN_DISABLE_PKT_MTU_CHECK=${OVN_DISABLE_PKT_MTU_CHECK:-false}
  OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  OVN_IMAGE=${OVN_IMAGE:-local}
  MASTER_LOG_LEVEL=${MASTER_LOG_LEVEL:-5}
  NODE_LOG_LEVEL=${NODE_LOG_LEVEL:-5}
  DBCHECKER_LOG_LEVEL=${DBCHECKER_LOG_LEVEL:-5}
  OVN_LOG_LEVEL_NORTHD=${OVN_LOG_LEVEL_NORTHD:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_NB=${OVN_LOG_LEVEL_NB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_SB=${OVN_LOG_LEVEL_SB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_CONTROLLER=${OVN_LOG_LEVEL_CONTROLLER:-"-vconsole:info"}
  OVN_ENABLE_EX_GW_NETWORK_BRIDGE=${OVN_ENABLE_EX_GW_NETWORK_BRIDGE:-false}
  OVN_EX_GW_NETWORK_INTERFACE=""
  if [ "$OVN_ENABLE_EX_GW_NETWORK_BRIDGE" == true ]; then
    OVN_EX_GW_NETWORK_INTERFACE="eth1"
  fi
  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  KIND_NUM_MASTER=1
  if [ "$OVN_HA" == true ]; then
    KIND_NUM_MASTER=3
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-0}
  else
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
  fi
  OVN_HOST_NETWORK_NAMESPACE=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}
  OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}
}

print_ovn_manifest_params() {
     echo "Using these parameters to build upgraded ovn-k manifests"
     echo ""
     echo "KIND_IPV4_SUPPORT = $KIND_IPV4_SUPPORT"
     echo "KIND_IPV6_SUPPORT = $KIND_IPV6_SUPPORT"
     echo "OVN_HA = $OVN_HA"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_DISABLE_SNAT_MULTIPLE_GWS = $OVN_DISABLE_SNAT_MULTIPLE_GWS"
     echo "OVN_DISABLE_PKT_MTU_CHECK = $OVN_DISABLE_PKT_MTU_CHECK"
     echo "OVN_NETFLOW_TARGETS = $OVN_NETFLOW_TARGETS"
     echo "OVN_SFLOW_TARGETS = $OVN_SFLOW_TARGETS"
     echo "OVN_IPFIX_TARGETS = $OVN_IPFIX_TARGETS"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "MASTER_LOG_LEVEL = $MASTER_LOG_LEVEL"
     echo "NODE_LOG_LEVEL = $NODE_LOG_LEVEL"
     echo "DBCHECKER_LOG_LEVEL = $DBCHECKER_LOG_LEVEL"
     echo "OVN_LOG_LEVEL_NORTHD = $OVN_LOG_LEVEL_NORTHD"
     echo "OVN_LOG_LEVEL_NB = $OVN_LOG_LEVEL_NB"
     echo "OVN_LOG_LEVEL_SB = $OVN_LOG_LEVEL_SB"
     echo "OVN_LOG_LEVEL_CONTROLLER = $OVN_LOG_LEVEL_CONTROLLER"
     echo "OVN_HOST_NETWORK_NAMESPACE = $OVN_HOST_NETWORK_NAMESPACE"
     echo "OVN_ENABLE_EX_GW_NETWORK_BRIDGE = $OVN_ENABLE_EX_GW_NETWORK_BRIDGE"
     echo "OVN_EX_GW_NETWORK_INTERFACE = $OVN_EX_GW_NETWORK_INTERFACE"
     echo ""
}

set_cluster_cidr_ip_families() {
  if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == false ]; then
    IP_FAMILY=""
    NET_CIDR=$NET_CIDR_IPV4
    SVC_CIDR=$SVC_CIDR_IPV4
    echo "IPv4 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$KIND_IPV4_SUPPORT" == false ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="ipv6"
    NET_CIDR=$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV6
    echo "IPv6 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="dual"
    NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
    echo "Dual Stack Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  else
    echo "Invalid setup. KIND_IPV4_SUPPORT and/or KIND_IPV6_SUPPORT must be true."
    exit 1
  fi
}

# This script is responsible for upgrading the ovn-kubernetes related resources 
# within a running cluster built from master, to new resources buit from the 
# checked-out branch.  

KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn} # Set default values

# setup env needed for regenerating ovn resources from the checked-out branch
# this will use the new OVN image as well: ovn-daemonset-f:pr
set_default_ovn_manifest_params
print_ovn_manifest_params

# create upgraded OVN manifests from checked-out branch
set_cluster_cidr_ip_families
create_ovn_kube_manifests
install_ovn_image

pushd ../dist/yaml

# install updated k8s configuration for ovn-k (useful in case of ClusterRole updates)
run_kubectl apply -f ovn-setup.yaml

# install updated ovnkube-node daemonset
run_kubectl apply -f ovnkube-node.yaml

kubectl_wait_daemonset ovnkube-node

run_kubectl get all -n ovn-kubernetes
CURRENT_REPLICAS_OVNKUBE_DB=$(run_kubectl get deploy -n ovn-kubernetes ovnkube-db -o=jsonpath='{.spec.replicas}')
run_kubectl scale deploy -n ovn-kubernetes ovnkube-db --replicas=0

# install updated ovnkube-db daemonset
if [ "$OVN_HA" == true ]; then
  run_kubectl apply -f ovnkube-db-raft.yaml
else
  run_kubectl apply -f ovnkube-db.yaml
fi

run_kubectl scale deploy -n ovn-kubernetes ovnkube-db --replicas=$CURRENT_REPLICAS_OVNKUBE_DB
kubectl_wait_deployment ovnkube-db

CURRENT_REPLICAS_OVNKUBE_MASTER=$(run_kubectl get deploy -n ovn-kubernetes ovnkube-master -o=jsonpath='{.spec.replicas}')

# scaling down replica before changing image briefly helps get around an issue seen with KIND
# The issue was sometimes the KIND cluster won't scale down the ovnkube-master pod in time
# and the new pod with the new image would be stuck in "Pending" state
run_kubectl scale deploy -n ovn-kubernetes ovnkube-master --replicas=0

# install updated ovnkube-master deployment
run_kubectl apply -f ovnkube-master.yaml

popd

run_kubectl scale deploy -n ovn-kubernetes ovnkube-master --replicas=$CURRENT_REPLICAS_OVNKUBE_MASTER
kubectl_wait_deployment ovnkube-master
kubectl_wait_for_upgrade

run_kubectl describe ds ovnkube-node -n ovn-kubernetes

run_kubectl describe deployments.apps ovnkube-master -n ovn-kubernetes

KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
MASTER_NODES=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o name)
# We want OVN HA not Kubernetes HA
# leverage the kubeadm well-known label node-role.kubernetes.io/control-plane=
# to choose the nodes where ovn master components will be placed
for node in $MASTER_NODES; do
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    # do not error if it fails to remove the taint
    kubectl taint node "$node" node-role.kubernetes.io/control-plane:NoSchedule- || true
  fi
done

# redownload the e2e test binaries if their version differs
K8S_VERSION="v1.24.0"
E2E_VERSION=$(/usr/local/bin/e2e.test --version)
if [[ "$E2E_VERSION" != "$K8S_VERSION" ]]; then
   echo "found version $E2E_VERSION of e2e binary, need version $K8S_VERSION ; will download it."
   # Install e2e test binary and ginkgo
   curl -L https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/kubernetes-test-linux-amd64.tar.gz -o kubernetes-test-linux-amd64.tar.gz
   tar xvzf kubernetes-test-linux-amd64.tar.gz
   sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
   sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
   rm kubernetes-test-linux-amd64.tar.gz
fi
