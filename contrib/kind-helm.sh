#!/usr/bin/env bash

set -exo pipefail

# Returns the full directory name of the script
export DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

export OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}

# Source the kind-common file from the same directory where this script is located
source "${DIR}/kind-common"

set_default_params() {

  # Set default values
  export KIND_CONFIG=${KIND_CONFIG:-}
  export KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
  export KIND_INSTALL_METALLB=${KIND_INSTALL_METALLB:-false}
  export KIND_INSTALL_PLUGINS=${KIND_INSTALL_PLUGINS:-false}
  export KIND_INSTALL_KUBEVIRT=${KIND_INSTALL_KUBEVIRT:-false}
  export OVN_HA=${OVN_HA:-false}
  export OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  export OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  export OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  export KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
  export ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-false}
  export OVN_NETWORK_QOS_ENABLE=${OVN_NETWORK_QOS_ENABLE:-false}
  export KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
  export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  export OVN_IMAGE=${OVN_IMAGE:-'ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-ubuntu:helm'}

  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}

  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  export NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  export NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  export SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  export NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  export SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  export JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  export JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  export MASQUERADE_SUBNET_IPV4=${MASQUERADE_SUBNET_IPV4:-169.254.169.0/29}
  export MASQUERADE_SUBNET_IPV6=${MASQUERADE_SUBNET_IPV6:-fd69::/125}
  export TRANSIT_SWITCH_SUBNET_IPV4=${TRANSIT_SWITCH_SUBNET_IPV4:-100.88.0.0/16}
  export TRANSIT_SWITCH_SUBNET_IPV6=${TRANSIT_SWITCH_SUBNET_IPV6:-fd97::/64}
  export METALLB_CLIENT_NET_SUBNET_IPV4=${METALLB_CLIENT_NET_SUBNET_IPV4:-172.22.0.0/16}
  export METALLB_CLIENT_NET_SUBNET_IPV6=${METALLB_CLIENT_NET_SUBNET_IPV6:-fc00:f853:ccd:e792::/64}

  export KIND_NUM_MASTER=1
  if [ "$OVN_HA" == true ]; then
    KIND_NUM_MASTER=3
  fi

  # Hard code ipv4 support until IPv6 is implemented
  export KIND_IPV4_SUPPORT=true
}

usage() {
    echo "usage: kind-helm.sh [--delete]"
    echo "       [ -cf  | --config-file <file> ]"
    echo "       [ -kt  | --keep-taint ]"
    echo "       [ -ha  | --ha-enabled ]"
    echo "       [ -me  | --multicast-enabled ]"
    echo "       [ -ho  | --hybrid-enabled ]"
    echo "       [ -el  | --ovn-empty-lb-events ]"
    echo "       [ -ii  | --install-ingress ]"
    echo "       [ -mlb | --install-metallb ]"
    echo "       [ -pl  | --install-cni-plugins ]"
    echo "       [ -ikv | --install-kubevirt ]"
    echo "       [ -mne | --multi-network-enable ]"
    echo "       [ -nqe | --network-qos-enable ]"
    echo "       [ -wk  | --num-workers <num> ]"
    echo "       [ -cn  | --cluster-name ]"
    echo "       [ -h ]"
    echo ""
    echo "--delete                            Delete current cluster"
    echo "-cf  | --config-file                Name of the KIND configuration file"
    echo "-kt  | --keep-taint                 Do not remove taint components"
    echo "                                    DEFAULT: Remove taint components"
    echo "-me  | --multicast-enabled          Enable multicast. DEFAULT: Disabled"
    echo "-ho  | --hybrid-enabled             Enable hybrid overlay. DEFAULT: Disabled"
    echo "-el  | --ovn-empty-lb-events        Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled"
    echo "-ii  | --install-ingress            Flag to install Ingress Components."
    echo "                                    DEFAULT: Don't install ingress components."
    echo "-mlb | --install-metallb            Install metallb to test service type LoadBalancer deployments"
    echo "-pl  | --install-cni-plugins        Install CNI plugins"
    echo "-ikv | --install-kubevirt           Install kubevirt"
    echo "-mne | --multi-network-enable       Enable multi networks. DEFAULT: Disabled"
    echo "-nqe | --network-qos-enable         Enable network QoS. DEFAULT: Disabled"
    echo "-ha  | --ha-enabled                 Enable high availability. DEFAULT: HA Disabled"
    echo "-wk  | --num-workers                Number of worker nodes. DEFAULT: 2 workers"
    echo "-cn  | --cluster-name               Configure the kind cluster's name"
    echo ""

}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            --delete )                          delete
                                                exit
                                                ;;
            -cf | --config-file )               shift
                                                if test ! -f "$1"; then
                                                    echo "$1 does not  exist"
                                                    usage
                                                    exit 1
                                                fi
                                                KIND_CONFIG=$1
                                                ;;
            -kt | --keep-taint )                KIND_REMOVE_TAINT=false
                                                ;;
            -me | --multicast-enabled)          OVN_MULTICAST_ENABLE=true
                                                ;;
            -ho | --hybrid-enabled )            OVN_HYBRID_OVERLAY_ENABLE=true
                                                ;;
            -el | --ovn-empty-lb-events )       OVN_EMPTY_LB_EVENTS=true
                                                ;;
            -ii | --install-ingress )           KIND_INSTALL_INGRESS=true
                                                ;;
            -mlb | --install-metallb )          KIND_INSTALL_METALLB=true
                                                ;;
            -pl | --install-cni-plugins )       KIND_INSTALL_PLUGINS=true
                                                ;;
            -ikv | --install-kubevirt)          KIND_INSTALL_KUBEVIRT=true
                                                ;;
            -mne | --multi-network-enable )     ENABLE_MULTI_NET=true
                                                ;;
            -nqe | --network-qos-enable )       OVN_NETWORK_QOS_ENABLE=true
                                                ;;
            -ha | --ha-enabled )                OVN_HA=true
                                                KIND_NUM_MASTER=3
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
     echo "KIND_CONFIG_FILE = $KIND_CONFIG"
     echo "KUBECONFIG = $KUBECONFIG"
     echo "KIND_INSTALL_INGRESS = $KIND_INSTALL_INGRESS"
     echo "KIND_INSTALL_METALLB = $KIND_INSTALL_METALLB"
     echo "KIND_INSTALL_PLUGINS = $KIND_INSTALL_PLUGINS"
     echo "KIND_INSTALL_KUBEVIRT = $KIND_INSTALL_KUBEVIRT"
     echo "OVN_HA = $OVN_HA"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "KIND_CLUSTER_NAME = $KIND_CLUSTER_NAME"
     echo "KIND_REMOVE_TAINT = $KIND_REMOVE_TAINT"
     echo "ENABLE_MULTI_NET = $ENABLE_MULTI_NET"
     echo "OVN_NETWORK_QOS_ENABLE = $OVN_NETWORK_QOS_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "KIND_NUM_MASTER = $KIND_NUM_MASTER"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo ""
}

check_dependencies() {
    for cmd in $OCI_BIN kubectl kind helm go ; do \
         if ! command_exists $cmd ; then
           2&>1 echo "Dependency not met: $cmd"
           exit 1
        fi
    done

    # check for currently unsupported features
    [ "${KIND_IPV6_SUPPORT}" == "true" ] && { &>1 echo "Fatal: KIND_IPV6_SUPPORT support not implemented yet"; exit 1; } ||:
    [ "${OVN_ENABLE_INTERCONNECT}" == "true" ] && { &>1 echo "Fatal: OVN_ENABLE_INTERCONNECT support not implemented yet"; exit 1; } ||:
}

helm_prereqs() {
    # increate fs.inotify.max_user_watches
    sudo sysctl fs.inotify.max_user_watches=524288
    # increase fs.inotify.max_user_instances
    sudo sysctl fs.inotify.max_user_instances=512
}

build_ovn_image() {
    if [ "${SKIP_OVN_IMAGE_REBUILD}" == "true" ]; then
      echo "Explicitly instructed not to rebuild ovn image: ${OVN_IMAGE}"
      return
    fi

    # Build ovn image
    pushd ${DIR}/../go-controller
    make
    popd

    # Build ovn kube image
    pushd ${DIR}/../dist/images
    # Find all built executables, but ignore the 'windows' directory if it exists
    find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
    echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
    $OCI_BIN build -t "${OVN_IMAGE}" -f Dockerfile.fedora .
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

create_kind_cluster() {
  [ -n "${KIND_CONFIG}" ] || {
    KIND_CONFIG='/tmp/kind.yaml'

    # Start of the kind configuration
    cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
        authorization-mode: "AlwaysAllow"
EOT
  }

    # Add control-plane nodes based on OVN_HA status. If there are 2 or more worker nodes, use
    # 2 of them them to host databases instead of creating additional control plane nodes.
    if [ "$OVN_HA" == true ] && [ "$KIND_NUM_WORKER" -lt 2 ]; then
        for i in {2..3}; do  # Have 3 control-plane nodes for HA
            echo "- role: control-plane" >> /tmp/kind.yaml
        done
    fi

    # Add worker nodes based on KIND_NUM_WORKER
    for i in $(seq 1 $KIND_NUM_WORKER); do
        echo "- role: worker" >> /tmp/kind.yaml
    done

    # Add networking configuration
    cat <<EOT >> /tmp/kind.yaml
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
  podSubnet: $NET_CIDR_IPV4
  serviceSubnet: $SVC_CIDR_IPV4
EOT

    kind delete clusters $KIND_CLUSTER_NAME ||:
    kind create cluster --name $KIND_CLUSTER_NAME --config "${KIND_CONFIG}" --retain
    kind load docker-image --name $KIND_CLUSTER_NAME $OVN_IMAGE

    # When using HA, label nodes to host db.
    if [ "$OVN_HA" == true ]; then
      kubectl label nodes k8s.ovn.org/ovnkube-db=true --overwrite \
              -l node-role.kubernetes.io/control-plane
      if [ "$KIND_NUM_WORKER" -ge 2 ]; then
        for n in ovn-worker ovn-worker2; do
            # We want OVN HA not Kubernetes HA
            # leverage the kubeadm well-known label node-role.kubernetes.io/control-plane=
            # to choose the nodes where ovn master components will be placed
            kubectl label node "$n" k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
        done
      fi
    fi

    # Remove taint, so control-plane nodes can also schedule regular pods
    if [ "$KIND_REMOVE_TAINT" == true ]; then
      kubectl taint node "$n" node-role.kubernetes.io/master:NoSchedule- \
              -l node-role.kubernetes.io/control-plane ||:
      kubectl taint node "$n" node-role.kubernetes.io/control-plane:NoSchedule- \
              -l node-role.kubernetes.io/control-plane ||:
    fi
}

create_ovn_kubernetes() {
    cd ${DIR}/../helm/ovn-kubernetes

    MASTER_REPLICAS=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l)
    helm install ovn-kubernetes . -f values.yaml \
        --set k8sAPIServer=${API_URL} \
        --set podNetwork="${NET_CIDR_IPV4}/24" \
        --set serviceNetwork=${SVC_CIDR_IPV4} \
        --set ovnkube-identity.replicas=${MASTER_REPLICAS} \
        --set ovnkube-master.replicas=${MASTER_REPLICAS} \
        --set global.image.repository=$(get_image) \
        --set global.image.tag=$(get_tag) \
        --set global.enableAdminNetworkPolicy=true \
        --set global.enableMulticast=$(if [ "${OVN_MULTICAST_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set global.enableMultiNetwork=$(if [ "${ENABLE_MULTI_NET}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set global.enableHybridOverlay=$(if [ "${OVN_HYBRID_OVERLAY_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set global.enableNetworkQos=$(if [ "${OVN_NETWORK_QOS_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set global.emptyLbEvents=$(if [ "${OVN_EMPTY_LB_EVENTS}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set tags.ovnkube-db-raft=$(if [ "${OVN_HA}" == "true" ]; then echo "true"; else echo "false"; fi) \
        --set tags.ovnkube-db=$(if [ "${OVN_HA}" == "false" ]; then echo "true"; else echo "false"; fi)
}

delete() {
  if [ "$KIND_INSTALL_METALLB" == true ]; then
    destroy_metallb
  fi
  helm uninstall ovn-kubernetes && sleep 5 ||:
  kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"
}

install_online_ovn_kubernetes_crds() {
  # NOTE: When you update vendoring versions for the ANP & BANP APIs, we must update the version of the CRD we pull from in the below URL
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
}

check_dependencies
set_default_params
parse_args "$@"
print_params
helm_prereqs
build_ovn_image
create_kind_cluster
detect_apiserver_url
docker_disable_ipv6
coredns_patch
create_ovn_kubernetes

install_online_ovn_kubernetes_crds
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  install_ingress
fi

if [ "$ENABLE_MULTI_NET" == true ]; then
  enable_multi_net
fi

kubectl_wait_pods
sleep_until_pods_settle

if [ "$KIND_INSTALL_METALLB" == true ]; then
  install_metallb
fi
if [ "$KIND_INSTALL_PLUGINS" == true ]; then
  install_plugins
fi
if [ "$KIND_INSTALL_KUBEVIRT" == true ]; then
  install_kubevirt
fi
