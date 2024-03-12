#!/usr/bin/env bash

set -exo pipefail

export SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

set_default_params() {
  # Set default values
  export OVN_HA=${OVN_HA:-false}
  export KIND_NUM_WORKER=${KIND_NUM_WORKER:-1}
  export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  export OVN_IMAGE=${OVN_IMAGE:-'ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:helm'}

  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}
}

usage() {
    echo "usage: kind-helm.sh [--delete]"
    echo "       [ -ha | --ha-enabled ]"
    echo "       [ -wk | --num-workers <num> ]"
    echo "       [ -cn | --cluster-name ]"
    echo "       [ -h ]"
    echo ""
    echo "--delete                            Delete current cluster"
    echo "-ha  | --ha-enabled                 Enable high availability. DEFAULT: HA Disabled."
    echo "-wk  | --num-workers                Number of worker nodes. DEFAULT: 1 worker"
    echo "-cn  | --cluster-name               Configure the kind cluster's name"
    echo "                                    nodes and no HA - 0 worker nodes."

}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            --delete )                          delete
                                                exit
                                                ;;
            -ha | --ha-enabled )                OVN_HA=true
                                                ;;
            -wk | --num-workers )               shift
                                                if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                    echo "Invalid num-workers: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                KIND_NUM_WORKER=$1
                                                ;;
            -cn | --cluster-name )              shift
                                                KIND_CLUSTER_NAME=$1
                                                # Setup KUBECONFIG
                                                set_default_params
                                                ;;
            * )                                 usage
                                                exit 1
        esac
        shift
    done
}

print_params() {
     echo "Using these parameters to deploy KIND + helm"
     echo ""
     echo "KUBECONFIG = $KUBECONFIG"
     echo "KIND_CLUSTER_NAME = $KIND_CLUSTER_NAME"
     echo ""
}

command_exists() {
  cmd="$1"
  command -v ${cmd} >/dev/null 2>&1
}

check_dependencies() {
    for cmd in docker kubectl kind helm go ; do \
         if ! command_exists $cmd ; then
           2&>1 echo "Dependency not met: $cmd"
           exit 1
        fi
    done
}

helm_prereqs() {
    # increate fs.inotify.max_user_watches
    sudo sysctl fs.inotify.max_user_watches=524288
    # increase fs.inotify.max_user_instances
    sudo sysctl fs.inotify.max_user_instances=512
}

docker_disable_ipv6() {
  # Docker disables IPv6 globally inside containers except in the eth0 interface.
  # Kind enables IPv6 globally the containers ONLY for dual-stack and IPv6 deployments.
  # Ovnkube-node tries to move all global addresses from the gateway interface to the
  # bridge interface it creates. This breaks on KIND with IPv4 only deployments, because the new
  # internal bridge has IPv6 disable and can't move the IPv6 from the eth0 interface.
  # We can enable IPv6 always in the container, since the docker setup with IPv4 only
  # is not very common.
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}" | grep -v external-load-balancer)
  for n in $KIND_NODES; do
    docker exec "$n" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
    docker exec "$n" sysctl --ignore net.ipv6.conf.all.forwarding=1
  done
}

build_ovn_image() {
    if [ "${SKIP_OVN_IMAGE_REBUILD}" == "true" ]; then
      echo "Explicitly instructed not to rebuild ovn image: ${OVN_IMAGE}"
      return
    fi

    # Build ovn image
    pushd ${SCRIPT_DIR}/../go-controller
    make
    popd

    # Build ovn kube image
    pushd ${SCRIPT_DIR}/../dist/images
    # Find all built executables, but ignore the 'windows' directory if it exists
    find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
    echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
    docker build -t "${OVN_IMAGE}" -f Dockerfile.fedora .
    popd
}

get_image() {
    local image_and_tag="${1:-$OVN_IMAGE}"  # Use $1 if provided, otherwise use $OVN_IMAGE
    local image="${image_and_tag%%:*}"  # Extract everything before the first colon
    echo "$image"
}

get_tag() {
    local image_and_tag="${1:-$OVN_IMAGE}"  # Use $1 if provided, otherwise use $OVN_IMAGE
    local tag="${image_and_tag##*:}"  # Extract everything after the last colon
    echo "$tag"
}

coredns_patch() {
  dns_server="8.8.8.8"
  # No need for ipv6 nameserver for dual stack, it will ask for
  # A and AAAA records
  if [ "$IP_FAMILY" == "ipv6" ]; then
    dns_server="2001:4860:4860::8888"
  fi

  # Patch CoreDNS to work
  # 1. Github CI doesn´t offer IPv6 connectivity, so CoreDNS should be configured
  # to work in an offline environment:
  # https://github.com/coredns/coredns/issues/2494#issuecomment-457215452
  # 2. Github CI adds following domains to resolv.conf search field:
  # .net.
  # CoreDNS should handle those domains and answer with NXDOMAIN instead of SERVFAIL
  # otherwise pods stops trying to resolve the domain.
  # Get the current config
  original_coredns=$(kubectl get -oyaml -n=kube-system configmap/coredns)
  echo "Original CoreDNS config:"
  echo "${original_coredns}"
  # Patch it
  fixed_coredns=$(
    printf '%s' "${original_coredns}" | sed \
      -e 's/^.*kubernetes cluster\.local/& net/' \
      -e '/^.*upstream$/d' \
      -e '/^.*fallthrough.*$/d' \
      -e 's/^\(.*forward \.\).*$/\1 '"$dns_server"' {/' \
      -e '/^.*loop$/d' \
  )
  echo "Patched CoreDNS config:"
  echo "${fixed_coredns}"
  printf '%s' "${fixed_coredns}" | kubectl apply -f -
}

create_kind_cluster() {
# Fetch the number of worker nodes and HA status from environment variables
    kind_num_worker="${KIND_NUM_WORKER:-1}"  # Default to 1 if not set
    ovn_ha="${OVN_HA:-false}"  # Default to false if not set

    # Start of the kind configuration
    kind_cluster_name=ovn-helm
    cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
EOT

    # Add control-plane nodes based on OVN_HA status
    echo "- role: control-plane" >> /tmp/kind.yaml  # Add a single control-plane node if not HA
    if [ "$ovn_ha" = true ]; then
        for i in {2..3}; do  # Add 3 control-plane nodes for HA
            echo "- role: control-plane" >> /tmp/kind.yaml
        done
    fi

    # Add worker nodes based on KIND_NUM_WORKER
    for i in $(seq 1 $kind_num_worker); do
        echo "- role: worker" >> /tmp/kind.yaml
    done

    # Add networking configuration
    cat <<EOT >> /tmp/kind.yaml
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOT

    kind delete clusters $KIND_CLUSTER_NAME ||:
    kind create cluster --name $KIND_CLUSTER_NAME --config /tmp/kind.yaml
    kind load docker-image --name $KIND_CLUSTER_NAME $OVN_IMAGE

    # When using HA, label controller nodes to host db
    [ "${OVN_HA}" = "false" ] || \
      kubectl label nodes k8s.ovn.org/ovnkube-db=true --overwrite \
        -l node-role.kubernetes.io/control-plane
}

create_ovn_kubernetes() {
    cd ${SCRIPT_DIR}/../helm/ovn-kubernetes

    helm install ovn-kubernetes . -f values.yaml \
        --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
        --set ovnkube-identity.replicas=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l) \
        --set global.image.repository=$(get_image) \
        --set global.image.tag=$(get_tag) \
        --set tags.ovnkube-db-raft=$(if [ "${OVN_HA}" = "true" ]; then echo "true"; else echo "false"; fi) \
        --set tags.ovnkube-db=$(if [ "${OVN_HA}" = "false" ]; then echo "true"; else echo "false"; fi)
}

delete() {
  helm uninstall ovn-kubernetes && sleep 5 ||:
  kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"
}

coredns_patch() {
  dns_server="8.8.8.8"
  # No need for ipv6 nameserver for dual stack, it will ask for 
  # A and AAAA records
  if [ "$IP_FAMILY" == "ipv6" ]; then
    dns_server="2001:4860:4860::8888"
  fi

  # Patch CoreDNS to work
  # 1. Github CI doesn´t offer IPv6 connectivity, so CoreDNS should be configured
  # to work in an offline environment:
  # https://github.com/coredns/coredns/issues/2494#issuecomment-457215452
  # 2. Github CI adds following domains to resolv.conf search field:
  # .net.
  # CoreDNS should handle those domains and answer with NXDOMAIN instead of SERVFAIL
  # otherwise pods stops trying to resolve the domain.
  # Get the current config
  original_coredns=$(kubectl get -oyaml -n=kube-system configmap/coredns)
  echo "Original CoreDNS config:"
  echo "${original_coredns}"
  # Patch it
  fixed_coredns=$(
    printf '%s' "${original_coredns}" | sed \
      -e 's/^.*kubernetes cluster\.local/& net/' \
      -e '/^.*upstream$/d' \
      -e '/^.*fallthrough.*$/d' \
      -e 's/^\(.*forward \.\).*$/\1 '"$dns_server"' {/' \
      -e '/^.*loop$/d' \
  )
  echo "Patched CoreDNS config:"
  echo "${fixed_coredns}"
  printf '%s' "${fixed_coredns}" | kubectl apply -f -
}

# kubectl_wait_pods will set a total timeout of 300s for IPv4 and 480s for IPv6. It will first wait for all
# DaemonSets to complete with kubectl rollout. This command will block until all pods of the DS are actually up.
# Next, it iterates over all pods in ovn-kubernetes namespace and waits for them to post "Ready".
# Last, it will do the same with all pods in the kube-system namespace.
kubectl_wait_pods() {
  local OVN_TIMEOUT=300

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + OVN_TIMEOUT ))

  dss=$(kubectl -n ovn-kubernetes get daemonset --no-headers --sort-by=.metadata.name -o jsonpath='{.items[*].metadata.name}')
  for ds in ${dss}; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to launch all ${ds} pods (timeout ${timeout})..."
    kubectl rollout status daemonset -n ovn-kubernetes ${ds} --timeout ${timeout}s
  done

  pods=$(kubectl -n ovn-kubernetes get pod --no-headers --sort-by=.metadata.name -o jsonpath='{.items[*].metadata.name}')
  for pod in ${pods}; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to create pod ${pod} (timeout ${timeout})..."
    if ! kubectl wait pod ${pod} -n ovn-kubernetes --for condition=Ready --timeout=${timeout}s ; then
        >&2 echo "pod $pod in the ovn-kubernetes namespace is not ready after time out"
        kubectl get pods -A -o wide || true
        exit 1
    fi
  done

  timeout=$(calculate_timeout ${endtime})
  if ! kubectl wait -n kube-system --for=condition=ready pods --all --timeout=${timeout}s ; then
    >&2 echo "some pods in the system are not running"
    kubectl get pods -A -o wide || true
    exit 1
  fi
}

# calculate_timeout takes an absolute endtime in seconds (based on bash script runtime, see
# variable $SECONDS) and calculates a relative timeout value. Should the calculated timeout
# be <= 0, return one second.
calculate_timeout() {
  endtime=$1
  timeout=$(( endtime - SECONDS ))
  if [ ${timeout} -le 0 ]; then
      timeout=1
  fi
  echo ${timeout}
}

check_dependencies
set_default_params
parse_args "$@"
helm_prereqs
build_ovn_image
create_kind_cluster
docker_disable_ipv6
coredns_patch
create_ovn_kubernetes
kubectl_wait_pods
