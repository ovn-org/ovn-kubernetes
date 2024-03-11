#!/usr/bin/env bash

set -exo pipefail

export SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

set_default_params() {
  # Set default values
  export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}
}

usage() {
    echo "usage: kind-helm.sh [--delete]"
    echo "       [ -cn | --cluster-name ]"
    echo "       [ -h ]"
    echo ""
}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
           --delete )                          delete
                                               exit
                                               ;;
            -cn | --cluster-name )             shift
                                               KIND_CLUSTER_NAME=$1
                                               set_default_params
                                               ;;
            * )                                usage
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
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    docker exec "$n" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
    docker exec "$n" sysctl --ignore net.ipv6.conf.all.forwarding=1
  done
}

build_ovn_image() {
    pushd ${SCRIPT_DIR}/../dist/images && \
    make ubuntu && \
    popd

    docker tag ovn-kube-u:latest ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:helm
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
    # create a cluster, with 1 controller and 1 worker node
    kind_cluster_name=ovn-helm
    cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOT
    kind delete clusters $KIND_CLUSTER_NAME ||:
    kind create cluster --name $KIND_CLUSTER_NAME --config /tmp/kind.yaml
    kind load docker-image --name $KIND_CLUSTER_NAME ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:helm
}

create_ovn_kubernetes() {
    cd ${SCRIPT_DIR}/../helm/ovn-kubernetes
    helm install ovn-kubernetes . -f values.yaml \
        --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
        --set ovnkube-identity.replicas=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l) \
        --set global.image.repository=ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u --set global.image.tag=helm
}

delete() {
  helm uninstall ovn-kubernetes
  sleep 5
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
