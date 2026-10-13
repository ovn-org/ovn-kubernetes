# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

if [ "${BASH_SOURCE[0]}" -ef "$0" ]
then
    >&2 echo 'This file contains bash helper functions used by kind-helm.sh'
    >&2 echo 'and is meant to be sourced upon invocation.'
    >&2 echo 'Please source this script, do not execute it!'
    exit 1
fi

ARCH=""
case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64"   ;;
esac

# Directory for coredump collection (used by setup_coredumps and collect_coredump_binaries)
readonly COREDUMP_DIR="/tmp/kind/logs/coredumps"

if_error_exit() {
    ###########################################################################
    # Description:                                                            #
    # Validate if previous command failed and show an error msg (if provided) #
    #                                                                         #
    # Arguments:                                                              #
    #   $1 - error message if not provided, it will just exit                 #
    ###########################################################################
    if [ "$?" != "0" ]; then
        if [ -n "$1" ]; then
            RED="\e[31m"
            ENDCOLOR="\e[0m"
            echo -e "[ ${RED}FAILED${ENDCOLOR} ] ${1}"
        fi
        exit 1
    fi
}

replace_in_file_or_exit() {
  local file="$1"
  local needle="$2"
  local replacement="$3"
  local escaped_needle escaped_replacement

  escaped_needle=$(escape_sed_replacement "$needle")
  escaped_replacement=$(escape_sed_replacement "$replacement")

  if ! grep -Fq -- "$needle" "$file"; then
    echo "Expected to replace pattern in ${file}, but it was not found: ${needle}" >&2
    exit 1
  fi

  if ! sed -i "s|${escaped_needle}|${escaped_replacement}|g" "$file"; then
    echo "Failed to update ${file}" >&2
    exit 1
  fi

  if grep -Fq -- "$needle" "$file"; then
    echo "Pattern still present in ${file} after replacement: ${needle}" >&2
    exit 1
  fi
}

escape_sed_replacement() {
  printf '%s' "$1" | sed 's/[\\|&]/\\&/g'
}

set_common_default_params() {
  # KIND/cluster params
  KIND_CREATE=${KIND_CREATE:-true}
  KIND_IMAGE=${KIND_IMAGE:-kindest/node}
  KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  K8S_VERSION=${K8S_VERSION:-v1.36.4}
  KIND_SETTLE_DURATION=${KIND_SETTLE_DURATION:-30}
  KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind.yaml.j2}
  KIND_LOCAL_REGISTRY=${KIND_LOCAL_REGISTRY:-false}
  KIND_LOCAL_REGISTRY_NAME=${KIND_LOCAL_REGISTRY_NAME:-kind-registry}
  KIND_LOCAL_REGISTRY_PORT=${KIND_LOCAL_REGISTRY_PORT:-5000}
  KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
  KIND_INSTALL_METALLB=${KIND_INSTALL_METALLB:-false}
  KIND_INSTALL_PLUGINS=${KIND_INSTALL_PLUGINS:-false}
  KIND_INSTALL_KUBEVIRT=${KIND_INSTALL_KUBEVIRT:-false}
  KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
  OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}
  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}
  # Scrub any existing kubeconfigs at the path
  if [ "${KIND_CREATE}" == true ]; then
    rm -f "${KUBECONFIG}"
  fi

  # Image/source code params
  OVN_IMAGE=${OVN_IMAGE:-local}
  OVN_IMAGE_FAMILY=${OVN_IMAGE_FAMILY:-fedora}
  OVN_REPO=${OVN_REPO:-""}
  OVN_GITREF=${OVN_GITREF:-""}
  # Pods to force-delete on --deploy so they respawn with the kind-loaded image.
  # ovs-node excluded: restarting it under live ovnkube-node pods breaks the cluster.
  OVN_DEPLOY_PODS=${OVN_DEPLOY_PODS:-"ovnkube-identity ovnkube-control-plane ovnkube-node"}
  case "${OVN_IMAGE_FAMILY}" in
    fedora|ubuntu)
      ;;
    *)
      echo "Unsupported OVN image family: ${OVN_IMAGE_FAMILY}"
      exit 1
      ;;
  esac

  # Subnet params
  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  MASQUERADE_SUBNET_IPV4=${MASQUERADE_SUBNET_IPV4:-169.254.0.0/17}
  MASQUERADE_SUBNET_IPV6=${MASQUERADE_SUBNET_IPV6:-fd69::/112}
  NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  MULTI_POD_SUBNET=${MULTI_POD_SUBNET:-false}
  if [ "$MULTI_POD_SUBNET" == true ]; then
    NET_CIDR_IPV4="10.243.0.0/23/24,10.244.0.0/16"
    NET_CIDR_IPV6="fd00:10:243::/63/64,fd00:10:244::/48"
  fi
  NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  TRANSIT_SUBNET_IPV4=${TRANSIT_SUBNET_IPV4:-100.88.0.0/16}
  TRANSIT_SUBNET_IPV6=${TRANSIT_SUBNET_IPV6:-fd97::/64}
  METALLB_CLIENT_NET_SUBNET_IPV4=${METALLB_CLIENT_NET_SUBNET_IPV4:-172.22.0.0/16}
  METALLB_CLIENT_NET_SUBNET_IPV6=${METALLB_CLIENT_NET_SUBNET_IPV6:-fc00:f853:ccd:e792::/64}
  PLATFORM_IPV4_SUPPORT=${PLATFORM_IPV4_SUPPORT:-true}
  PLATFORM_IPV6_SUPPORT=${PLATFORM_IPV6_SUPPORT:-false}

  # Feature params
  OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-shared}
  OVN_SECOND_BRIDGE=${OVN_SECOND_BRIDGE:-false}
  OVN_UPLINK_BRIDGE=${OVN_UPLINK_BRIDGE:-false}
  OVN_UPLINK_BRIDGE_NAME=${OVN_UPLINK_BRIDGE_NAME:-ovsbr1}
  OVN_UPLINK_NETWORK_NAME=${OVN_UPLINK_NETWORK_NAME:-uplink}
  OVN_UPLINK_NETWORK_IPV4=${OVN_UPLINK_NETWORK_IPV4:-172.28.0.0/16}
  OVN_UPLINK_NETWORK_IPV6=${OVN_UPLINK_NETWORK_IPV6:-fc00:f853:ccd:e800::/64}
  OVN_DISABLE_SNAT_MULTIPLE_GWS=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-false}
  OVN_DISABLE_FORWARDING=${OVN_DISABLE_FORWARDING:-false}
  OVN_UNPRIVILEGED_MODE=${OVN_UNPRIVILEGED_MODE:-false}
  ADVERTISE_DEFAULT_NETWORK=${ADVERTISE_DEFAULT_NETWORK:-false}
  ADVERTISED_UDN_ISOLATION_MODE=${ADVERTISED_UDN_ISOLATION_MODE:-strict}
  BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4:-172.26.0.0/16}
  BGP_SERVER_NET_SUBNET_IPV6=${BGP_SERVER_NET_SUBNET_IPV6:-fc00:f853:ccd:e796::/64}
  BGP_SERVER_HOST_PORT=${BGP_SERVER_HOST_PORT:-18080}
  # Names of the external BGP scaffolding: the docker network hosting the
  # external server, the server container and the external FRR router
  # container. Overridable so that multiple cluster environments (e.g. a kind
  # cluster and the dpu-sim clusters) can each own a separate trio on the same
  # machine instead of fighting over a machine-global singleton.
  BGP_NET_NAME=${BGP_NET_NAME:-bgpnet}
  BGP_SERVER_NAME=${BGP_SERVER_NAME:-bgpserver}
  FRR_CONTAINER_NAME=${FRR_CONTAINER_NAME:-frr}
  OVN_OBSERV_ENABLE=${OVN_OBSERV_ENABLE:-false}
  OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  OVN_NETWORK_QOS_ENABLE=${OVN_NETWORK_QOS_ENABLE:-false}
  OVN_ENABLE_DNSNAMERESOLVER=${OVN_ENABLE_DNSNAMERESOLVER:-false}
  ENABLE_COREDUMPS=${ENABLE_COREDUMPS:-false}
  METRICS_IP=${METRICS_IP:-""}
  OVN_ALLOW_ICMP_NETPOL=${OVN_ALLOW_ICMP_NETPOL:-false}
  OVN_ENCAP_PORT=${OVN_ENCAP_PORT:-""}
  OVN_DISABLE_PKT_MTU_CHECK=${OVN_DISABLE_PKT_MTU_CHECK:-false}
  ENABLE_IPSEC=${ENABLE_IPSEC:-false}
  OVN_METRICS_SCALE_ENABLE=${OVN_METRICS_SCALE_ENABLE:-false}
  OVN_EGRESSIP_HEALTHCHECK_PORT=${OVN_EGRESSIP_HEALTHCHECK_PORT:-""}
  OVN_NETFLOW_TARGETS=${OVN_NETFLOW_TARGETS:-""}
  OVN_SFLOW_TARGETS=${OVN_SFLOW_TARGETS:-""}
  OVN_IPFIX_TARGETS=${OVN_IPFIX_TARGETS:-""}
  OVN_IPFIX_SAMPLING=${OVN_IPFIX_SAMPLING:-""}
  OVN_IPFIX_CACHE_MAX_FLOWS=${OVN_IPFIX_CACHE_MAX_FLOWS:-""}
  OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT:-""}
  LIBOVSDB_CLIENT_LOGFILE=${LIBOVSDB_CLIENT_LOGFILE:-""}
  OVN_ISOLATED=${OVN_ISOLATED:-false}
  KIND_ADD_NODES=${KIND_ADD_NODES:-false}
  # Log levels
  MASTER_LOG_LEVEL=${MASTER_LOG_LEVEL:-4}
  NODE_LOG_LEVEL=${NODE_LOG_LEVEL:-4}
  OVN_LOG_LEVEL_NB=${OVN_LOG_LEVEL_NB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_SB=${OVN_LOG_LEVEL_SB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_NORTHD=${OVN_LOG_LEVEL_NORTHD:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_CONTROLLER=${OVN_LOG_LEVEL_CONTROLLER:-"-vconsole:info"}
  # Cluster config
  KIND_DNS_DOMAIN=${KIND_DNS_DOMAIN:-cluster.local}
  KIND_NUM_INFRA=${KIND_NUM_INFRA:-0}
  OVN_HOST_NETWORK_NAMESPACE=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}
  # Script behavior
  KIND_INSTALL_PROMETHEUS=${KIND_INSTALL_PROMETHEUS:-false}
  KIND_ALLOW_SYSTEM_WRITES=${KIND_ALLOW_SYSTEM_WRITES:-false}
  RUN_IN_CONTAINER=${RUN_IN_CONTAINER:-false}
  OVN_DUMMY_GATEWAY_BRIDGE=${OVN_DUMMY_GATEWAY_BRIDGE:-false}
  OVN_GATEWAY_OPTS=${OVN_GATEWAY_OPTS:-""}
  if [ "$OVN_ISOLATED" == true ]; then
    OVN_GATEWAY_OPTS="--gateway-interface=eth0"
  fi
  if [ "$OVN_DUMMY_GATEWAY_BRIDGE" == true ]; then
    OVN_GATEWAY_OPTS="--allow-no-uplink --gateway-interface=br-ex"
  fi

  KIND_NUM_MASTER=1
  KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}

  ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-false}
  ENABLE_NETWORK_SEGMENTATION=${ENABLE_NETWORK_SEGMENTATION:-false}
  if [ "$ENABLE_NETWORK_SEGMENTATION" == true ] && [ "$ENABLE_MULTI_NET" != true ]; then
    echo "Network segmentation (UDN) requires multi-network to be enabled (-mne)"
    exit 1
  fi

  ENABLE_NETWORK_CONNECT=${ENABLE_NETWORK_CONNECT:-false}
  if [[ $ENABLE_NETWORK_CONNECT == true && $ENABLE_NETWORK_SEGMENTATION != true ]]; then
    echo "Network connect requires network-segmentation to be enabled (-nse)"
    exit 1
  fi

  ENABLE_UPLINK=${ENABLE_UPLINK:-false}
  if [[ $ENABLE_UPLINK == true && $ENABLE_NETWORK_SEGMENTATION != true ]]; then
    echo "Uplink requires network-segmentation to be enabled (-nse)"
    exit 1
  fi

  DYNAMIC_UDN_ALLOCATION=${DYNAMIC_UDN_ALLOCATION:-false}
  if [[ $DYNAMIC_UDN_ALLOCATION == true && $ENABLE_NETWORK_SEGMENTATION != true ]]; then
      echo "Dynamic UDN allocation requires network-segmentation to be enabled (-nse)"
      exit 1
  fi
  DYNAMIC_UDN_GRACE_PERIOD=${DYNAMIC_UDN_GRACE_PERIOD:-120s}

  ENABLE_PRE_CONF_UDN_ADDR=${ENABLE_PRE_CONF_UDN_ADDR:-false}
  if [[ $ENABLE_PRE_CONF_UDN_ADDR == true && $ENABLE_NETWORK_SEGMENTATION != true ]]; then
    echo "Preconfigured UDN addresses requires network-segmentation to be enabled (-nse)"
    exit 1
  fi
  ENABLE_ROUTE_ADVERTISEMENTS=${ENABLE_ROUTE_ADVERTISEMENTS:-false}
  if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ] && [ "$ENABLE_MULTI_NET" != true ]; then
    echo "Route advertisements requires multi-network to be enabled (-mne)"
    exit 1
  fi

  ENABLE_EVPN=${ENABLE_EVPN:-false}
  if [ "$ENABLE_EVPN" == true ] && [ "$ENABLE_ROUTE_ADVERTISEMENTS" != true ]; then
    echo "EVPN requires Route advertisements to be enabled (-rae)"
    exit 1
  fi
  if [ "$ENABLE_EVPN" == true ] && [ "$OVN_GATEWAY_MODE" != "local" ]; then
    echo "EVPN requires local gateway mode (-gm local)"
    exit 1
  fi
  if [ "$OVN_UPLINK_BRIDGE" == true ] && [ "${DPU_MODE:-none}" != "none" ]; then
    echo "Uplink bridge provisioning is supported only for regular KIND deployments"
    exit 1
  fi
  

  ENABLE_NO_OVERLAY=${ENABLE_NO_OVERLAY:-false}
  ENABLE_NO_OVERLAY_OUTBOUND_SNAT=${ENABLE_NO_OVERLAY_OUTBOUND_SNAT:-false}
  ENABLE_NO_OVERLAY_MANAGED_ROUTING=${ENABLE_NO_OVERLAY_MANAGED_ROUTING:-false}
  if [ "$ENABLE_NO_OVERLAY" == true ] && [ "$ENABLE_ROUTE_ADVERTISEMENTS" != true ]; then
    echo "No-overlay mode requires route advertisement to be enabled (-rae)"
    exit 1
  fi
  if [ "$ENABLE_NO_OVERLAY" == true ] && [ "$ADVERTISE_DEFAULT_NETWORK" != true ] && [ "$ENABLE_NO_OVERLAY_MANAGED_ROUTING" != true ]; then
    echo "No-overlay mode requires advertise the default network (-adv) in unmanaged routing mode"
    exit 1
  fi
  if [ "$ENABLE_NO_OVERLAY" == true ]; then
    # Set default MTU for no-overlay mode (1500) if not already set
    OVN_MTU=${OVN_MTU:-1500}
  else
    # Set default MTU for overlay mode (1400) if not already set
    OVN_MTU=${OVN_MTU:-1400}
  fi
  if [ "$ENABLE_NO_OVERLAY_OUTBOUND_SNAT" == true ] && [ "$ENABLE_NO_OVERLAY" != true ]; then
    echo "No-overlay outbound SNAT can only be enabled when no-overlay mode is enabled (-noe)"
    exit 1
  fi
  if [ "$ENABLE_NO_OVERLAY_MANAGED_ROUTING" == true ] && [ "$ENABLE_NO_OVERLAY" != true ]; then
    echo "No-overlay managed routing can only be enabled when no-overlay mode is enabled (-noe)"
    exit 1
  fi
}

set_ovn_image() {
  local ovn_image_repo="ovn-daemonset-${OVN_IMAGE_FAMILY}"
  if [ "${KIND_LOCAL_REGISTRY:-false}" == true ]; then
    OVN_IMAGE="localhost:5000/${ovn_image_repo}:latest"
  else
    OVN_IMAGE="localhost/${ovn_image_repo}:dev"
  fi
}

create_local_registry() {
    # create registry container unless it already exists
    if [ "$($OCI_BIN inspect -f '{{.State.Running}}' "${KIND_LOCAL_REGISTRY_NAME}" 2>/dev/null || true)" != 'true' ]; then
      $OCI_BIN run \
        -d --restart=always -p "127.0.0.1:${KIND_LOCAL_REGISTRY_PORT}:5000" --name "${KIND_LOCAL_REGISTRY_NAME}" \
        registry:2
    fi
}

connect_local_registry() {
    # connect the registry to the cluster network if not already connected
    if [ "$($OCI_BIN inspect -f='{{json .NetworkSettings.Networks.kind}}' "${KIND_LOCAL_REGISTRY_NAME}")" = 'null' ]; then
      $OCI_BIN network connect "kind" "${KIND_LOCAL_REGISTRY_NAME}"
    fi

    # Reference docs for local registry:
    # - https://kind.sigs.k8s.io/docs/user/local-registry/
    # - https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${KIND_LOCAL_REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

    # Configure containerd hosts.toml for the local registry (containerd 2.x)
    local registry_dir="/etc/containerd/certs.d/localhost:${KIND_LOCAL_REGISTRY_PORT}"
    for node in $(kind get nodes --name "${KIND_CLUSTER_NAME}"); do
        $OCI_BIN exec "${node}" mkdir -p "${registry_dir}"
        $OCI_BIN exec "${node}" sh -c \
            "printf '[host.\"http://${KIND_LOCAL_REGISTRY_NAME}:${KIND_LOCAL_REGISTRY_PORT}\"]\n' > ${registry_dir}/hosts.toml"
    done
}

build_ovn_image() {
  local build_target="${OVN_IMAGE_FAMILY}-image"
  local push_args=""
  if [ "$OCI_BIN" == "podman" ]; then
    # docker doesn't perform tls check by default only podman does, hence we need to disable it for podman.
    push_args="--tls-verify=false"
  fi

  if [ "$OVN_IMAGE" == local ]; then
    set_ovn_image

    # Build image
    make -C ${DIR}/../dist/images IMAGE="${OVN_IMAGE}" OVN_REPO="${OVN_REPO}" OVN_GITREF="${OVN_GITREF}" OCI_BIN="${OCI_BIN}" "${build_target}"

    # store in local registry
    if [ "$KIND_LOCAL_REGISTRY" == true ];then
      echo "Pushing built image to local $OCI_BIN registry"
      $OCI_BIN push $push_args "$OVN_IMAGE"
    fi
  # We should push to local registry if image is not remote
  elif [[ -n "${OVN_IMAGE}" && "${KIND_LOCAL_REGISTRY}" == true && "${OVN_IMAGE}" != */* ]]; then
    local local_registry_ovn_image="localhost:5000/${OVN_IMAGE}"
    $OCI_BIN tag "$OVN_IMAGE" $local_registry_ovn_image
    OVN_IMAGE=$local_registry_ovn_image
    $OCI_BIN push $push_args "$OVN_IMAGE"
  fi
}

run_kubectl() {
  # In deploy mode, KUBECONFIG points at an existing cluster and must not be
  # merged with `kind export kubeconfig`; repeated merges can duplicate entries.
  # In container mode, export would also overwrite reachable container IPs with localhost.
  if [ "$RUN_IN_CONTAINER" != true ] && [ "${KIND_CREATE:-true}" == true ]; then
    kind export kubeconfig --name ${KIND_CLUSTER_NAME}
  fi

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

setup_kubectl_bin() {
    ###########################################################################
    # Description:                                                            #
    # setup kubectl for querying the cluster                                  #
    #                                                                         #
    # Arguments:                                                              #
    #   $1 - error message if not provided, it will just exit                 #
    ###########################################################################
    if [ ! -d "./bin" ]
    then
        mkdir -p ./bin
        if_error_exit "Failed to create bin dir!"
    fi

    if [[ "$OSTYPE" == "linux-gnu" ]]; then
        OS_TYPE="linux"
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        OS_TYPE="darwin"
    fi

    pushd ./bin
       if [ ! -f ./kubectl ]; then
           curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/${OS_TYPE}/${ARCH}/kubectl"
           if_error_exit "Failed to download kubectl failed!"
       fi
    popd

    chmod +x ./bin/kubectl
    export PATH=${PATH}:$(pwd)/bin
}

command_exists() {
  cmd="$1"
  command -v ${cmd} >/dev/null 2>&1
}

detect_apiserver_url() {
  # Detect API_URL used for in-cluster communication
  #
  # This will return apiserver address in format https://<node-name>:<port>
  DNS_NAME_URL=$(kind get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" | grep server | awk '{ print $2 }')
  # cut https:// from the URL
  CP_NODE=${DNS_NAME_URL#*//}
  # cut port from the URL
  CP_NODE=${CP_NODE%:*}
  # find node IP address in the kind network
  if [ "$PLATFORM_IPV4_SUPPORT" == false ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    NODE_IP="[$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.GlobalIPv6Address}}' $CP_NODE)]"
  else
    NODE_IP=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$CP_NODE")
  fi
  # replace node name with node IP address
  API_URL=${DNS_NAME_URL/$CP_NODE/$NODE_IP}
}

docker_disable_ipv6() {
  # Docker disables IPv6 globally inside containers except in the eth0 interface.
  # Kind enables IPv6 globally the containers ONLY for dual-stack and IPv6 deployments.
  # Ovnkube-node tries to move all global addresses from the gateway interface to the
  # bridge interface it creates. This breaks on KIND with IPv4 only deployments, because the new
  # internal bridge has IPv6 disable and can't move the IPv6 from the eth0 interface.
  # We can enable IPv6 always in the container, since the docker setup with IPv4 only
  # is not very common.
  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    $OCI_BIN exec "$n" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
    $OCI_BIN exec "$n" sysctl --ignore net.ipv6.conf.all.forwarding=1
  done
}

docker_create_second_interface() {
  echo "adding second interfaces to nodes"

  # Create the network as dual stack, regardless of the type of the deployment. Ignore if already exists.
  "$OCI_BIN" network create --ipv6 --driver=bridge xgw --subnet=172.19.0.0/16 --subnet=fc00:f853:ccd:e798::/64 || true

  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    "$OCI_BIN" network connect xgw "$n"
  done
}

kind_uplink_network_value() {
  local container=$1
  local field=$2

  "$OCI_BIN" inspect "$container" | jq -r \
    --arg network "${OVN_UPLINK_NETWORK_NAME}" \
    --arg field "$field" \
    '.[0].NetworkSettings.Networks[$network][$field] // empty'
}

oci_network_subnets() {
  local network=$1

  "$OCI_BIN" network inspect "$network" | jq -r '
    .[0] as $network |
    [
      $network.IPAM.Config[]?.Subnet,
      $network.subnets[]?.subnet
    ] | map(select(. != null and . != "")) | .[]
  '
}

validate_uplink_network_subnets() {
  local network=$1
  local existing_subnets subnet missing_subnets unexpected_subnets

  if ! existing_subnets=$(oci_network_subnets "$network" | sort -u); then
    echo "failed to inspect OCI network ${network}" >&2
    exit 1
  fi

  for subnet in "${OVN_UPLINK_NETWORK_IPV4}" "${OVN_UPLINK_NETWORK_IPV6}"; do
    if ! printf '%s\n' "$existing_subnets" | grep -Fxq "$subnet"; then
      missing_subnets="${missing_subnets} ${subnet}"
    fi
  done

  while IFS= read -r subnet; do
    case "$subnet" in
    ""|"${OVN_UPLINK_NETWORK_IPV4}"|"${OVN_UPLINK_NETWORK_IPV6}")
      ;;
    *)
      unexpected_subnets="${unexpected_subnets} ${subnet}"
      ;;
    esac
  done <<< "$existing_subnets"

  if [ -n "${missing_subnets}" ] || [ -n "${unexpected_subnets}" ]; then
    echo "OCI network ${network} subnets do not match expected Uplink subnets" >&2
    echo "Expected subnets: ${OVN_UPLINK_NETWORK_IPV4} ${OVN_UPLINK_NETWORK_IPV6}" >&2
    echo "Existing subnets: ${existing_subnets}" >&2
    exit 1
  fi
}

ensure_uplink_network() {
  if "$OCI_BIN" network inspect "${OVN_UPLINK_NETWORK_NAME}" >/dev/null 2>&1; then
    validate_uplink_network_subnets "${OVN_UPLINK_NETWORK_NAME}"
    return
  fi

  "$OCI_BIN" network create --ipv6 --driver=bridge "${OVN_UPLINK_NETWORK_NAME}" \
    --subnet="${OVN_UPLINK_NETWORK_IPV4}" \
    --subnet="${OVN_UPLINK_NETWORK_IPV6}"
}

docker_create_uplink_interface() {
  echo "adding Uplink interfaces to nodes"

  ensure_uplink_network

  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    if [ -z "$(kind_uplink_network_value "$n" IPAddress)" ]; then
      "$OCI_BIN" network connect "${OVN_UPLINK_NETWORK_NAME}" "$n"
    fi
  done
}

disable_bridge_netfilter() {
  echo "disabling bridge netfilter for KIND container bridge networks"

  local key current
  [ -e /proc/sys/net/bridge/bridge-nf-call-iptables ] || sudo modprobe br_netfilter || true
  for key in bridge-nf-call-iptables bridge-nf-call-ip6tables; do
    current=$(cat "/proc/sys/net/bridge/${key}" 2>/dev/null) || continue
    if [ "${current}" != 0 ]; then
      sudo sysctl -w "net.bridge.${key}=0"
    fi
  done
}

find_kind_uplink_interface() {
  local node=$1
  local ip iface

  ip=$(kind_uplink_network_value "$node" IPAddress)
  if [ -n "$ip" ]; then
    iface=$("$OCI_BIN" exec "$node" sh -c \
      "ip -o -4 addr show | awk -v ip='${ip}' '\$4 ~ \"^\" ip \"/\" {print \$2; exit}'")
    if [ -n "$iface" ]; then
      echo "$iface"
      return 0
    fi
  fi

  ip=$(kind_uplink_network_value "$node" GlobalIPv6Address)
  if [ -n "$ip" ]; then
    iface=$("$OCI_BIN" exec "$node" sh -c \
      "ip -o -6 addr show | awk -v ip='${ip}' '\$4 ~ \"^\" ip \"/\" {print \$2; exit}'")
    if [ -n "$iface" ]; then
      echo "$iface"
      return 0
    fi
  fi

  echo "failed to find node interface for network ${OVN_UPLINK_NETWORK_NAME} on ${node}" >&2
  return 1
}

configure_kind_uplink_bridge() {
  echo "configuring unmanaged Uplink bridge ${OVN_UPLINK_BRIDGE_NAME}"

  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    local iface pod
    iface=$(find_kind_uplink_interface "$n")
    pod=$(kubectl -n ovn-kubernetes get pod \
      -l app=ovnkube-node \
      --field-selector "spec.nodeName=${n}" \
      -o jsonpath='{.items[0].metadata.name}')
    if [ -z "$pod" ]; then
      echo "failed to find ovnkube-node pod on ${n}" >&2
      return 1
    fi
    local existing_iface
    existing_iface=$(kubectl -n ovn-kubernetes exec "$pod" -c ovn-controller -- \
      ovs-vsctl br-get-external-id "${OVN_UPLINK_BRIDGE_NAME}" bridge-uplink 2>/dev/null || true)
    if [ -n "$existing_iface" ]; then
      iface=$existing_iface
    fi
    kubectl -n ovn-kubernetes exec "$pod" -c ovn-controller -- \
      ovs-vsctl --may-exist add-br "${OVN_UPLINK_BRIDGE_NAME}"
    kubectl -n ovn-kubernetes exec "$pod" -c ovn-controller -- \
      ovs-vsctl --may-exist add-port "${OVN_UPLINK_BRIDGE_NAME}" "$iface"
    kubectl -n ovn-kubernetes exec "$pod" -c ovn-controller -- \
      ovs-vsctl br-set-external-id "${OVN_UPLINK_BRIDGE_NAME}" bridge-uplink "$iface"
    "$OCI_BIN" exec "$n" sh -c 'bridge=$1; iface=$2; ip link set dev "$bridge" up;
      for addr in $(ip -o -4 addr show dev "$iface" scope global | awk "{print \$4}"); do
        ip addr add "$addr" dev "$bridge" 2>/dev/null || true;
        ip addr del "$addr" dev "$iface" 2>/dev/null || true;
      done;
      for addr in $(ip -o -6 addr show dev "$iface" scope global | awk "{print \$4}"); do
        ip addr add "$addr" dev "$bridge" 2>/dev/null || true;
        ip addr del "$addr" dev "$iface" 2>/dev/null || true;
      done' sh "${OVN_UPLINK_BRIDGE_NAME}" "$iface"
    echo "Uplink nodeConfig: node=${n} hostInterfaceName=${OVN_UPLINK_BRIDGE_NAME} bridge=${OVN_UPLINK_BRIDGE_NAME} bridgeUplink=${iface}"
  done
}

configure_frr_uplink_receive_config() {
  local frr_uplink_ipv4=$1
  local frr_uplink_ipv6=$2
  local receive_file
  local kubectl_cmd=(kubectl)

  if [ -z "${frr_uplink_ipv4}" ] && [ -z "${frr_uplink_ipv6}" ]; then
    echo "external FRR has no IP address on ${OVN_UPLINK_NETWORK_NAME}" >&2
    return 1
  fi
  if frr_k8s_remote_enabled; then
    kubectl_cmd=(kubectl --kubeconfig "$(frr_k8s_host_kubeconfig)")
  fi

  receive_file=$(mktemp)
  {
    echo "apiVersion: frrk8s.metallb.io/v1beta1"
    echo "kind: FRRConfiguration"
    echo "metadata:"
    echo "  name: uplink-receive-all"
    echo "  labels:"
    echo "    name: uplink-receive-all"
    echo "spec:"
    echo "  bgp:"
    echo "    routers:"
    echo "    - asn: 64512"
    echo "      neighbors:"
    if [ -n "${frr_uplink_ipv4}" ]; then
      echo "      - address: ${frr_uplink_ipv4}"
      echo "        asn: 64512"
      echo "        disableMP: true"
      echo "        toReceive:"
      echo "          allowed:"
      echo "            mode: filtered"
      echo "            prefixes:"
      echo "            - prefix: ${BGP_SERVER_NET_SUBNET_IPV4}"
    fi
    if [ "$PLATFORM_IPV6_SUPPORT" == true ] && [ -n "${frr_uplink_ipv6}" ]; then
      echo "      - address: ${frr_uplink_ipv6}"
      echo "        asn: 64512"
      echo "        disableMP: true"
      echo "        toReceive:"
      echo "          allowed:"
      echo "            mode: filtered"
      echo "            prefixes:"
      echo "            - prefix: ${BGP_SERVER_NET_SUBNET_IPV6}"
    fi
  } > "${receive_file}"

  "${kubectl_cmd[@]}" apply -n frr-k8s-system -f "${receive_file}"
  rm -f "${receive_file}"
  echo "Uplink FRRConfiguration selector: name=uplink-receive-all"
}

configure_frr_uplink_peers() {
  if [ "$OVN_UPLINK_BRIDGE" != true ] ||
     [ "$ENABLE_ROUTE_ADVERTISEMENTS" != true ] ||
     [ "$ENABLE_NO_OVERLAY_MANAGED_ROUTING" == true ] ||
     [ "${DPU_MODE:-none}" == "host" ]; then
    return
  fi

  echo "configuring external FRR for Uplink network ${OVN_UPLINK_NETWORK_NAME}"
  "$OCI_BIN" network connect "${OVN_UPLINK_NETWORK_NAME}" "${FRR_CONTAINER_NAME}" || true

  local attempts=0
  while ! "$OCI_BIN" exec "${FRR_CONTAINER_NAME}" vtysh -c "show daemons" >/dev/null 2>&1; do
    if (( ++attempts > 30 )); then
      echo "error: FRR daemons did not become ready for Uplink peering" >&2
      return 1
    fi
    sleep 1
  done

  local frr_uplink_ipv4 frr_uplink_ipv6
  frr_uplink_ipv4=$(kind_uplink_network_value "${FRR_CONTAINER_NAME}" IPAddress)
  frr_uplink_ipv6=$(kind_uplink_network_value "${FRR_CONTAINER_NAME}" GlobalIPv6Address)

  local ipv4_neighbors=()
  local ipv6_neighbors=()
  local node_ip
  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    node_ip=$(kind_uplink_network_value "$n" IPAddress)
    if [ -n "${node_ip}" ]; then
      ipv4_neighbors+=("${node_ip}")
    fi
    if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
      node_ip=$(kind_uplink_network_value "$n" GlobalIPv6Address)
      if [ -n "${node_ip}" ]; then
        ipv6_neighbors+=("${node_ip}")
      fi
    fi
  done

  local vtysh_cmds=(-c "configure terminal" -c "router bgp 64512")
  for node_ip in "${ipv4_neighbors[@]}" "${ipv6_neighbors[@]}"; do
    vtysh_cmds+=(-c "neighbor ${node_ip} remote-as 64512")
  done
  if ((${#ipv4_neighbors[@]} > 0)); then
    vtysh_cmds+=(-c "address-family ipv4 unicast")
    for node_ip in "${ipv4_neighbors[@]}"; do
      vtysh_cmds+=(-c "neighbor ${node_ip} activate")
      vtysh_cmds+=(-c "neighbor ${node_ip} route-reflector-client")
    done
    vtysh_cmds+=(-c "exit-address-family")
  fi
  if ((${#ipv6_neighbors[@]} > 0)); then
    vtysh_cmds+=(-c "address-family ipv6 unicast")
    for node_ip in "${ipv6_neighbors[@]}"; do
      vtysh_cmds+=(-c "neighbor ${node_ip} activate")
      vtysh_cmds+=(-c "neighbor ${node_ip} route-reflector-client")
    done
    vtysh_cmds+=(-c "exit-address-family")
  fi
  vtysh_cmds+=(-c "end" -c "write memory")
  "$OCI_BIN" exec "${FRR_CONTAINER_NAME}" vtysh "${vtysh_cmds[@]}"

  "$OCI_BIN" exec "${FRR_CONTAINER_NAME}" ip route delete default || true
  "$OCI_BIN" exec "${FRR_CONTAINER_NAME}" ip -6 route delete default || true
  configure_frr_uplink_receive_config "${frr_uplink_ipv4}" "${frr_uplink_ipv6}"
}

check_ipv6() {
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Collect additional IPv6 data on test environment
    ERROR_FOUND=false
    TMPVAR=$(sysctl net.ipv6.conf.all.forwarding | awk '{print $3}')
    echo "net.ipv6.conf.all.forwarding is equal to $TMPVAR"
    if [ "$TMPVAR" != 1 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.forwarding=1
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.forwarding=1' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    TMPVAR=$(sysctl net.ipv6.conf.all.disable_ipv6 | awk '{print $3}')
    echo "net.ipv6.conf.all.disable_ipv6 is equal to $TMPVAR"
    if [ "$TMPVAR" != 0 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    if [ -f /proc/net/if_inet6 ]; then
      echo "/proc/net/if_inet6 exists so IPv6 supported in kernel."
    else
      echo "/proc/net/if_inet6 does not exists so no IPv6 support found! Compile the kernel!!"
      ERROR_FOUND=true
    fi
    if "$ERROR_FOUND"; then
      exit 2
    fi
  fi
}

set_cluster_cidr_ip_families() {
# kind only allows single subnet for pod network, while ovn-kubernetes supports multiple subnets.
# So we pick the first subnet from the provided list for kind configuration and store it in KIND_CIDR.
# remove host subnet mask info for kind configuration (when the subnet is set as 10.0.0.0/16/14)
  KIND_CIDR_IPV4=$(echo "${NET_CIDR_IPV4}"| cut -d',' -f1 | cut -d'/' -f1,2 )
  KIND_CIDR_IPV6=$(echo "${NET_CIDR_IPV6}"| cut -d',' -f1 | cut -d'/' -f1,2 )
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == false ]; then
    IP_FAMILY=""
    KIND_CIDR=$KIND_CIDR_IPV4
    NET_CIDR=$NET_CIDR_IPV4
    SVC_CIDR=$SVC_CIDR_IPV4
    echo "IPv4 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == false ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="ipv6"
    KIND_CIDR=$KIND_CIDR_IPV6
    NET_CIDR=$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV6
    echo "IPv6 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="dual"
    KIND_CIDR=$KIND_CIDR_IPV4,$KIND_CIDR_IPV6
    NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
    echo "Dual Stack Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  else
    echo "Invalid setup. PLATFORM_IPV4_SUPPORT and/or PLATFORM_IPV6_SUPPORT must be true."
    exit 1
  fi
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

install_ingress() {
  run_kubectl apply -f "${DIR}/ingress/mandatory.yaml"
  run_kubectl apply -f "${DIR}/ingress/service-nodeport.yaml"
}

METALLB_DIR="/tmp/metallb"

align_metallb_pool_with_ip_family() {
  # In single-stack jobs, dev-env can provide dual-stack addresses, which
  # causes withdraw/announce churn and flaky connectivity checks.
  if [ "$PLATFORM_IPV4_SUPPORT" == "$PLATFORM_IPV6_SUPPORT" ]; then
    return
  fi

  local pool_json filtered_addresses_json matched_count
  pool_json=$(kubectl -n metallb-system get ipaddresspool dev-env-bgp -o json 2>/dev/null || true)
  if [ -z "${pool_json}" ]; then
    echo "Failed to read dev-env-bgp pool"
    kubectl -n metallb-system get ipaddresspool dev-env-bgp -o yaml || true
    exit 1
  fi

  filtered_addresses_json=$(echo "${pool_json}" | jq -c \
    --arg ipv4 "${PLATFORM_IPV4_SUPPORT}" \
    --arg ipv6 "${PLATFORM_IPV6_SUPPORT}" '
    .spec.addresses
    | if $ipv6 == "true" and $ipv4 != "true" then map(select(test(":")))
      elif $ipv4 == "true" and $ipv6 != "true" then map(select(test(":") | not))
      else .
      end
  ')
  matched_count=$(echo "${filtered_addresses_json}" | jq 'length')
  if [ "${matched_count}" -eq 0 ]; then
    echo "Failed to derive family-matching addresses from dev-env-bgp pool: $(echo "${pool_json}" | jq -c '.spec.addresses')"
    exit 1
  fi
  kubectl -n metallb-system patch ipaddresspool dev-env-bgp --type='merge' \
    -p "{\"spec\":{\"addresses\":${filtered_addresses_json}}}"
}

install_metallb() {
  local metallb_version=v0.16.1
  local metallb_upstream_frr_image=quay.io/frrouting/frr:10.5.3
  mkdir -p /tmp/metallb
  local builddir
  builddir=$(mktemp -d "${METALLB_DIR}/XXXXXX")

  pushd "${builddir}"
  git clone https://github.com/metallb/metallb.git
  cd metallb
  git checkout $metallb_version

  # kindest/node image v1.32+ that we use is only compatible with kind v0.27+
  # when using 'kind load' command however metallb builds and uses older
  # incompatible kind version patch it so that it uses our own kind install
  # instead of their build
  replace_in_file_or_exit \
    tasks.py \
    'kind_path = os.path.join(build_path, "kind")' \
    'kind_path = "kind"'

  # MetalLB v0.16.1 manifests reference FRR 10.5.3. CI uses
  # FRR_DEPLOYED_IMAGE for BGP tests, so replace that exact upstream value in
  # the MetalLB manifests. Matching the exact upstream value is intentional
  # because if a future MetalLB release changes its FRR image, this script
  # should fail instead of silently leaving MetalLB on an unexpected FRR version.
  replace_in_file_or_exit \
    config/frr/speaker-patch.yaml \
    "${metallb_upstream_frr_image}" \
    "${FRR_DEPLOYED_IMAGE}"
  replace_in_file_or_exit \
    config/manifests/metallb-frr.yaml \
    "${metallb_upstream_frr_image}" \
    "${FRR_DEPLOYED_IMAGE}"
  replace_in_file_or_exit \
    charts/metallb/values.yaml \
    "tag: ${metallb_upstream_frr_image##*:}" \
    "tag: ${FRR_DEPLOYED_IMAGE##*:}"

  pip install -r dev-env/requirements.txt

  local ip_family ipv6_network
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="dual"
    ipv6_network="--ipv6 --subnet=${METALLB_CLIENT_NET_SUBNET_IPV6}"
  elif  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="ipv6"
    ipv6_network="--ipv6 --subnet=${METALLB_CLIENT_NET_SUBNET_IPV6}"
  else
    ip_family="ipv4"
    ipv6_network=""
  fi
  # The dev-env BGP backend is selected with -b. MetalLB v0.16.1 can default
  # that path to frr-k8s, which runs FRR through in-cluster frr-k8s resources
  # instead of the standalone dev-env container named frr. The service and
  # network-segmentation e2e setup below still connects clientnet to that frr
  # container and relies on its external routes. Keep -b frr explicit so this
  # Kubernetes 1.36 compatibility update only changes the MetalLB release and
  # does not also change the BGP datapath used by those tests.
  #
  # TODO: Move this path to frr-k8s in a separate change after validating the
  # service and network-segmentation e2e setup against the in-cluster frr-k8s
  # backend.
  # Override GOBIN until https://github.com/metallb/metallb/issues/2218 is fixed.
  GOBIN="" inv dev-env -n ovn -b frr -p bgp -i "${ip_family}"

  align_metallb_pool_with_ip_family

  $OCI_BIN network rm -f clientnet
  $OCI_BIN network create --subnet="${METALLB_CLIENT_NET_SUBNET_IPV4}" ${ipv6_network} --driver bridge clientnet
  $OCI_BIN network connect clientnet frr
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Enable IPv6 forwarding in FRR
    $OCI_BIN exec frr sysctl -w net.ipv6.conf.all.forwarding=1
  fi
  # Note: this image let's us use it also for creating load balancer backends that can send big packets
  $OCI_BIN rm -f lbclient
  $OCI_BIN run  --cap-add NET_ADMIN --user 0  -d --network clientnet  --rm  --name lbclient  quay.io/itssurya/dev-images:metallb-lbservice
  popd
  delete_metallb_dir

  # The metallb commit https://github.com/metallb/metallb/commit/1a8e52c393d40efd17f28491616f6f9f7790a522
  # removes control plane node from acting as a bgp speaker for service routes.
  # Hence remove node.kubernetes.io/exclude-from-external-load-balancers label from control-plane nodes
  # so that they are also available for advertising bgp routes which are needed for ovnkube's service
  # specific e2e tests.
  MASTER_NODES=$(kind_get_nodes | sort | head -n "${KIND_NUM_MASTER}")
  for n in $MASTER_NODES; do
    kubectl label node "$n" node.kubernetes.io/exclude-from-external-load-balancers-
  done

  kind_network_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' frr)
  echo "FRR kind network IPv4: ${kind_network_v4}"
  kind_network_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.GlobalIPv6Address}}' frr)
  echo "FRR kind network IPv6: ${kind_network_v6}"
  local client_network_v4 client_network_v6
  client_network_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.clientnet.IPAddress}}' frr)
  echo "FRR client network IPv4: ${client_network_v4}"
  client_network_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.clientnet.GlobalIPv6Address}}' frr)
  echo "FRR client network IPv6: ${client_network_v6}"

  local client_subnets
  client_subnets=$($OCI_BIN network inspect clientnet -f '{{range .IPAM.Config}}{{.Subnet}}#{{end}}')
  echo "${client_subnets}"
  local client_subnets_v4 client_subnets_v6
  client_subnets_v4=$(echo "${client_subnets}" | cut -d '#' -f 1)
  echo "client subnet IPv4: ${client_subnets_v4}"
  client_subnets_v6=$(echo "${client_subnets}" | cut -d '#' -f 2)
  echo "client subnet IPv6: ${client_subnets_v6}"

  KIND_NODES=$(kind_get_nodes)
  for n in ${KIND_NODES}; do
    if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
        $OCI_BIN exec "${n}" ip route add "${client_subnets_v4}" via "${kind_network_v4}"
    fi
    if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
        $OCI_BIN exec "${n}" ip -6 route add "${client_subnets_v6}" via "${kind_network_v6}"
    fi
  done

  # for now, we only run one test with metalLB load balancer for which this
  # one svcVIP (192.168.10.0/fc00:f853:ccd:e799::) is more than enough since at a time we will only
  # have one load balancer service
  if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
    $OCI_BIN exec lbclient ip route add 192.168.10.0 via "${client_network_v4}" dev eth0
  fi
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    $OCI_BIN exec lbclient ip -6 route add fc00:f853:ccd:e799:: via "${client_network_v6}" dev eth0
  fi
  sleep 30
}

install_plugins() {
  # pinned so CI doesn't chase upstream master; the dhcp plugin needs
  # >= v1.6.0 (cniVersion 1.1.0 support)
  git clone --depth 1 -b v1.9.1 https://github.com/containernetworking/plugins.git
  pushd plugins
  CGO_ENABLED=0 ./build_linux.sh
  KIND_NODES=$(kind_get_nodes)
  # Opted for not overwritting the existing plugins
  for node in $KIND_NODES; do
    for plugin in bandwidth bridge dhcp dummy firewall host-device ipvlan macvlan sbr static tuning vlan vrf; do
      $OCI_BIN cp ./bin/$plugin $node:/opt/cni/bin/
    done
  done
  popd
  rm -rf plugins
}

destroy_metallb() {
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^lbclient$'; then
      $OCI_BIN stop lbclient
  fi
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^frr$'; then
      $OCI_BIN stop frr
  fi
  if $OCI_BIN network ls --format '{{.Name}}' | grep -q '^clientnet$'; then
      $OCI_BIN network rm clientnet
  fi
  delete_metallb_dir
}

delete_metallb_dir() {
  if ! [ -d "${METALLB_DIR}" ]; then
      return
  fi

  # The build directory will contain read only directories after building. Files cannot be deleted, even by the owner.
  # Therefore, set all dirs to u+rwx.
  find "${METALLB_DIR}" -type d -exec chmod u+rwx "{}" \;
  rm -rf "${METALLB_DIR}"
}

wait_for_ovn_daemonset() {
  local ds=$1
  local required=${2:-true}
  local endtime=$3

  if ! kubectl -n ovn-kubernetes get daemonset "${ds}" >/dev/null 2>&1; then
    if [ "${required}" = true ]; then
      echo "DaemonSet ${ds} was not found in namespace ovn-kubernetes" >&2
      return 1
    fi
    return 0
  fi

  local timeout
  timeout=$(calculate_timeout ${endtime})
  echo "Waiting for k8s to launch all ${ds} pods (timeout ${timeout})..."
  # `kubectl rollout status` errors on DaemonSets with updateStrategy=OnDelete
  # (upgrade-ovn.sh sets ovs-node to OnDelete so helm upgrade doesn't roll
  # OVS out from under still-running ovnkube-node pods). For OnDelete DSes
  # we can't observe rollout progress; just wait for pods to be Ready.
  local strategy
  strategy=$(kubectl -n ovn-kubernetes get daemonset ${ds} \
    -o=jsonpath='{.spec.updateStrategy.type}' 2>/dev/null)
  if [ "${strategy}" = "OnDelete" ]; then
    kubectl wait -n ovn-kubernetes --for=condition=ready pods \
      -l app=${ds} --timeout=${timeout}s
  else
    kubectl rollout status daemonset -n ovn-kubernetes ${ds} --timeout ${timeout}s
  fi
}

# kubectl_wait_pods will set a total timeout of 300s for IPv4 and 480s for IPv6,
# unless KIND_HELM_OVN_TIMEOUT is set. It will first wait for all DaemonSets to
# complete with kubectl rollout. This command will block until all pods of the
# DS are actually up. Next, it waits for ovnkube-control-plane pods to post
# "Ready" when that deployment is part of the mode. If the DNS name resolver
# replaced the CoreDNS image, it then waits for that rollout to finish. Last,
# it waits for every non-terminating pod in the kube-system namespace to be
# Ready.
kubectl_wait_pods() {
  # IPv6 cluster seems to take a little longer to come up, so extend the wait time.
  OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT:-300}
  if ! [[ "${OVN_TIMEOUT}" =~ ^[0-9]+$ ]]; then
    echo "Invalid KIND_HELM_OVN_TIMEOUT: ${OVN_TIMEOUT}"
    exit 1
  fi
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT:-480}
  fi

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + OVN_TIMEOUT ))

  case "${DPU_MODE:-none}" in
    dpu)
      wait_for_ovn_daemonset ovnkube-node-dpu true ${endtime}
      ;;
    host)
      wait_for_ovn_daemonset ovnkube-node-dpu-host true ${endtime}
      wait_for_ovn_daemonset ovnkube-node false ${endtime}
      wait_for_ovn_daemonset ovs-node false ${endtime}
      ;;
    *)
      wait_for_ovn_daemonset ovnkube-node true ${endtime}
      wait_for_ovn_daemonset ovs-node true ${endtime}
      ;;
  esac

  if [ "${DPU_MODE:-none}" != "dpu" ]; then
    local name
    name=ovnkube-control-plane
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to create ${name} pods (timeout ${timeout})..."
    kubectl wait pods -n ovn-kubernetes -l name=${name} --for condition=Ready --timeout=${timeout}s
  fi

  restart_dpu_sim_multus_after_ovnk

  if [ "${OVN_ENABLE_DNSNAMERESOLVER:-false}" == true ]; then
    # Make sure the custom CoreDNS image rollout completed before checking the
    # kube-system pods, so the check below covers the new replicas.
    timeout=$(calculate_timeout "${endtime}")
    echo "Waiting for the CoreDNS deployment rollout (timeout ${timeout})..."
    kubectl -n kube-system rollout status deployment/coredns --timeout "${timeout}s"
  fi

  if ! kubectl_wait_namespace_pods_ready kube-system ${endtime}; then
    echo "some pods in the system are not running"
    kubectl get pods -A -o wide || true
    exit 1
  fi
}

# kubectl_wait_namespace_pods_ready waits until every non-terminating pod in the
# namespace is Ready, giving up at the absolute endtime (in $SECONDS, see
# calculate_timeout). `kubectl wait --all` is not used because it resolves the
# pod list once and keeps waiting for a pod deleted mid-wait (e.g. a terminating
# CoreDNS replica) until its timeout expires; instead the current pods are
# checked once per poll.
kubectl_wait_namespace_pods_ready() {
  local namespace=$1
  local endtime=$2
  local pods

  echo "Waiting for pods in ${namespace} to become ready (timeout $(calculate_timeout ${endtime}))..."
  while true; do
    pods=$(kubectl get pods -n ${namespace} -o go-template \
      --template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}')
    if [ -n "${pods}" ] && \
      kubectl wait -n ${namespace} --for=condition=ready --timeout=0 pod ${pods} >/dev/null 2>&1; then
      return 0
    fi
    if [ $(( endtime - SECONDS )) -le 0 ]; then
      return 1
    fi
    sleep 2
  done
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

sleep_until_pods_settle() {
  echo "Pods are all up, allowing things settle for ${KIND_SETTLE_DURATION} seconds..."
  sleep ${KIND_SETTLE_DURATION}
}

is_nested_virt_enabled() {
    local kvm_nested="unknown"
    if [ -f "/sys/module/kvm_intel/parameters/nested" ]; then
        kvm_nested=$( cat /sys/module/kvm_intel/parameters/nested )
    elif [ -f "/sys/module/kvm_amd/parameters/nested" ]; then
        kvm_nested=$( cat /sys/module/kvm_amd/parameters/nested )
    fi
    [ "$kvm_nested" == "1" ] || [ "$kvm_nested" == "Y" ] || [ "$kvm_nested" == "y" ]
}

install_kubevirt() {
    # possible values:
    # stable - install newest stable (default)
    # vX.Y.Z - install specific stable (i.e v1.3.1)
    # nightly - install newest nightly
    # nightly tag - install specific nightly (i.e 20240910)
    # KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-"stable"}

    KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-"v1.8.4"}

    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name"); do
        $OCI_BIN exec -t $node bash -c "echo 'fs.inotify.max_user_watches=1048576' >> /etc/sysctl.conf"
        $OCI_BIN exec -t $node bash -c "echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.conf"
        $OCI_BIN exec -i $node bash -c "sysctl -p /etc/sysctl.conf"
        if [[ "${node}" =~ worker ]]; then
            kubectl label nodes $node node-role.kubernetes.io/worker="" --overwrite=true
        fi
    done

    if [ "$(kubectl get kubevirts -n kubevirt kubevirt -ojsonpath='{.status.phase}')" != "Deployed" ]; then
      local kubevirt_release_url=$(get_kubevirt_release_url "$KUBEVIRT_VERSION")
      echo "Deploying Kubevirt from $kubevirt_release_url"
      # Server-side apply avoids kubectl.kubernetes.io/last-applied-configuration.
      # That annotation duplicates the KubeVirt CRD and exceeds the 256KiB annotation limit.
      kubectl apply --server-side --force-conflicts -f "${kubevirt_release_url}/kubevirt-operator.yaml"
      kubectl apply --server-side --force-conflicts -f "${kubevirt_release_url}/kubevirt-cr.yaml"
      if ! is_nested_virt_enabled; then
        kubectl -n kubevirt patch kubevirt kubevirt --type=merge --patch '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'
      fi
    fi

    kubectl -n kubevirt patch kubevirt kubevirt --type=json --patch '[
        {"op":"add","path":"/spec/configuration/virtualMachineOptions","value":{}},
        {"op":"add","path":"/spec/configuration/virtualMachineOptions/disableSerialConsoleLog","value":{}},
        {"op":"add","path":"/spec/configuration/developerConfiguration","value":{"featureGates":[]}},
        {"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"NetworkBindingPlugins"},
        {"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"DynamicPodInterfaceNaming"},
        {"op":"add","path":"/spec/configuration/network","value":{}},
        {"op":"add","path":"/spec/configuration/network/binding","value":{"l2bridge":{"domainAttachmentType":"managedTap","migration":{}}}}
    ]'

    if ! kubectl wait -n kubevirt kv kubevirt --for condition=Available --timeout 15m; then
        kubectl get pod -n kubevirt -l || true
        kubectl describe pod -n kubevirt -l || true
        for p in $(kubectl get pod -n kubevirt -l -o name |sed "s#pod/##"); do
            kubectl logs -p --all-containers=true -n kubevirt $p || true
            kubectl logs --all-containers=true -n kubevirt $p || true
        done
    fi
}

install_cert_manager() {
  local cert_manager_version="v1.14.4"
  echo "Installing cert-manager ..."
  manifest="https://github.com/cert-manager/cert-manager/releases/download/${cert_manager_version}/cert-manager.yaml"
  run_kubectl apply -f "$manifest"
}

install_kubevirt_ipam_controller() {
  echo "Installing KubeVirt IPAM controller manager ..."
  manifest="https://github.com/kubevirt/ipam-extensions/releases/download/v0.3.1/install.yaml"
  run_kubectl apply -f "$manifest"
  kubectl wait -n kubevirt-ipam-controller-system deployment kubevirt-ipam-controller-manager --for condition=Available --timeout 2m
}

install_multus() {
  local version="v4.1.3"
  local image="ghcr.io/k8snetworkplumbingwg/multus-cni:${version}"
  echo "Installing multus-cni $version daemonset ..."
  # Pull the image once on the host and load it into every kind node, instead
  # of having each node pull ~180MB from ghcr.io independently. The daemonset
  # pins the image tag and has no imagePullPolicy, so the kubelet default
  # (IfNotPresent) uses the preloaded image.
  if [ "$KIND_LOCAL_REGISTRY" != true ]; then
    "$OCI_BIN" pull "$image"
    install_image "$image"
  fi
  # The upstream 50Mi memory limit gets the thin plugin OOM-killed on newer
  # (7.0+) kernels, so bump it.
  wget -qO- "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${version}/deployments/multus-daemonset.yml" |\
    sed -e "s|multus-cni:snapshot|multus-cni:${version}|g" \
        -e 's|memory: "50Mi"|memory: "200Mi"|g' |\
    run_kubectl apply -f -
}

install_mpolicy_crd() {
  echo "Installing multi-network-policy CRD ..."
  mpolicy_manifest="https://raw.githubusercontent.com/k8snetworkplumbingwg/multi-networkpolicy/refs/tags/v1.0.1/scheme.yml"
  run_kubectl apply -f "$mpolicy_manifest"
}

install_ipamclaim_crd() {
  echo "Installing IPAMClaim CRD ..."
  ipamclaims_manifest="https://raw.githubusercontent.com/k8snetworkplumbingwg/ipamclaims/v0.5.1-alpha/artifacts/k8s.cni.cncf.io_ipamclaims.yaml"
  run_kubectl apply -f "$ipamclaims_manifest"
}

docker_create_second_disconnected_interface() {
  echo "adding second interfaces to nodes"
  local bridge_name="${1:-xgw}"
  echo "bridge: $bridge_name"

  if [ "${OCI_BIN}" = "podman" ]; then
    # docker and podman do different things with the --internal parameter:
    # - docker installs iptables rules to drop traffic on a different subnet
    #   than the bridge and we don't want that.
    # - podman does not set the bridge as default gateway and we want that.
    # So we need it with podman but not with docker. Neither allows us to create
    # a bridge network without IPAM which would be ideal, so perhaps the best
    # option would be a manual setup.
    local podman_params="--internal"
  fi

  # Create the network without subnets; ignore if already exists.
  "$OCI_BIN" network create --driver=bridge ${podman_params-} "$bridge_name" || true

  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    "$OCI_BIN" network connect "$bridge_name" "$n" || true
  done
}

enable_multi_net() {
  if [ "${DPU_MODE:-none}" == "none" ]; then
    install_multus
  else
    echo "Skipping multus-cni installation; DPU simulator mode expects Multus to be installed by dpu-simulator"
  fi
  install_mpolicy_crd
  install_ipamclaim_crd
  docker_create_second_disconnected_interface "underlay"  # localnet scenarios require an extra interface
}

kind_get_nodes() {
  kind get nodes --name "${KIND_CLUSTER_NAME}" | grep -v external-load-balancer
}

set_dnsnameresolver_images() {
  if [ "$KIND_LOCAL_REGISTRY" == true ];then
    COREDNS_WITH_OCP_DNSNAMERESOLVER="localhost:5000/coredns-with-ocp-dnsnameresolver:latest"
    DNSNAMERESOLVER_OPERATOR="localhost:5000/dnsnameresolver-operator:latest"
  else
    COREDNS_WITH_OCP_DNSNAMERESOLVER="localhost/coredns-with-ocp-dnsnameresolver:dev"
    DNSNAMERESOLVER_OPERATOR="localhost/dnsnameresolver-operator:dev"
  fi
}

# build_image accepts three arguments. The first argument is the absolute path to the directory
# which contains the Dockerfile. The second argument is the image name along with the tag. The
# third argument is the name of the Dockerfile to use for building the image. 
build_image() {
  pushd ${1}
  $OCI_BIN build -t "${2}" -f ${3} .

  # store in local registry
  if [ "$KIND_LOCAL_REGISTRY" == true ];then
    echo "Pushing built image (${2}) to local $OCI_BIN registry"
    $OCI_BIN push "${2}"
  fi
  popd
}

build_dnsnameresolver_images() {
  set_dnsnameresolver_images
  rm -rf /tmp/coredns-ocp-dnsnameresolver
  git clone https://github.com/openshift/coredns-ocp-dnsnameresolver.git /tmp/coredns-ocp-dnsnameresolver
  pushd /tmp/coredns-ocp-dnsnameresolver
  git checkout release-4.22
  popd
 
  build_image /tmp/coredns-ocp-dnsnameresolver ${COREDNS_WITH_OCP_DNSNAMERESOLVER} Dockerfile.upstream

  build_image /tmp/coredns-ocp-dnsnameresolver/operator ${DNSNAMERESOLVER_OPERATOR} Dockerfile
}

check_common_dependencies() {
  if ! command_exists curl ; then
    echo "Dependency not met: Command not found 'curl'"
    exit 1
  fi

  if ! command_exists kubectl ; then
    echo "'kubectl' not found, installing"
    setup_kubectl_bin
  fi

  if ! command_exists kind ; then
    echo "Dependency not met: Command not found 'kind'"
    exit 1
  fi

  local kind_min="0.27.0"
  local kind_cur
  kind_cur=$(kind version -q)
  if [ "$(echo -e "$kind_min\n$kind_cur" | sort -V | head -1)" != "$kind_min" ]; then
    echo "Dependency not met: expected kind version >= $kind_min but have $kind_cur"
    exit 1
  fi

  if ! command_exists jq ; then
    echo "Dependency not met: Command not found 'jq'"
    exit 1
  fi

  if ! command_exists awk ; then
    echo "Dependency not met: Command not found 'awk'"
    exit 1
  fi

  if ! command_exists jinjanate ; then
    if ! command_exists pipx ; then
      echo "Dependency not met: 'jinjanator' not installed and cannot install with 'pipx'"
      exit 1
    fi
    echo "'jinjanate' not found, installing with 'pipx'"
    install_jinjanator_renderer
  fi

  if ! command_exists docker && ! command_exists podman; then
    echo "Dependency not met: Neither docker nor podman found"
    exit 1
  fi

  if command_exists podman && ! command_exists skopeo; then
    echo "Dependency not met: skopeo not installed. Run the following command to install it: 'sudo dnf install skopeo'"
    exit 1
  fi
}

install_jinjanator_renderer() {
  # ensure jinjanator renderer installed
  pipx install jinjanator[yaml]
  pipx ensurepath --force >/dev/null
  export PATH=~/.local/bin:$PATH
}

install_ovn_image() {
  install_image "${OVN_IMAGE}"
}

# Force-respawn OVN pods so their controllers pick up the kind-loaded image
# on --deploy, where helm sees no spec diff (OVN_IMAGE tag is fixed).
refresh_ovn_pods() {
  for pod in ${OVN_DEPLOY_PODS}; do
    kubectl delete pod -n ovn-kubernetes -l name="${pod}" --ignore-not-found
  done
}

# install_image accepts the image name along with the tag as an argument and installs it.
install_image() {
  # If local registry is being used push image there for consumption by kind cluster
  if [ "$KIND_LOCAL_REGISTRY" == true ]; then
    echo "${1} should already be avaliable in local registry, not loading"
  else
    # Write the archive under a unique temporary directory per invocation:
    # podman save refuses to write to an existing file, and concurrent or
    # aborted runs must not clash on a shared archive path. The subshell
    # scopes the cleanup trap to this function call.
    (
      archive_dir=$(mktemp -d)
      trap 'rm -rf "${archive_dir}"' EXIT
      archive="${archive_dir}/image.tar"
      if [ "$OCI_BIN" == "podman" ]; then
        # podman: cf https://github.com/kubernetes-sigs/kind/issues/2027
        podman save -o "${archive}" "${1}"
        kind load image-archive "${archive}" --name "${KIND_CLUSTER_NAME}"
      else
        # docker: with the containerd image store (default since Docker 27), an
        # archive of a pulled multi-arch image references every platform but only
        # contains the host platform's blobs, and the "ctr images import
        # --all-platforms" that kind runs fails on the missing digests. Save the
        # host platform only (docker save --platform, Docker 28+) and load the
        # archive; older docker lacks the flag and its classic image store
        # produces single-platform archives anyway, so fall back to kind load.
        if docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" -o "${archive}" "${1}" 2>/dev/null; then
          kind load image-archive "${archive}" --name "${KIND_CLUSTER_NAME}"
        else
          kind load docker-image "${1}" --name "${KIND_CLUSTER_NAME}"
        fi
      fi
    )
  fi
}

install_dnsnameresolver_images() {
  install_image ${COREDNS_WITH_OCP_DNSNAMERESOLVER}
  install_image ${DNSNAMERESOLVER_OPERATOR}
}

install_dnsnameresolver_operator() {
  pushd /tmp/coredns-ocp-dnsnameresolver/operator
  
  # Before installing DNSNameResolver operator, update the args so that the operator
  # is configured with the correct values.
  sed -i -e 's/^\(.*--coredns-namespace=\).*/\1kube-system/' \
    -e 's/^\(.*--coredns-service-name=\).*/\1kube-dns/' \
    -e 's/^\(.*--dns-name-resolver-namespace=\).*/\1ovn-kubernetes/' \
    -e 's/^\(.*--coredns-port=\).*/\153/' config/default/manager_dnsnameresolver_patch.yaml

  make install CONTROLLER_TOOLS_VERSION=v0.19.0
  make deploy IMG=${DNSNAMERESOLVER_OPERATOR} CONTROLLER_TOOLS_VERSION=v0.19.0
  popd
}

update_clusterrole_coredns() {
  original_clusterrole=$(kubectl get clusterrole system:coredns -oyaml)
  echo "Original CoreDNS clusterrole:"
  echo "${original_clusterrole}"
  additional_permissions=$(printf '%s' '
- apiGroups:
  - network.openshift.io
  resources:
  - dnsnameresolvers
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - network.openshift.io
  resources:
  - dnsnameresolvers/status
  verbs:
  - update
  - get
  - patch
')
  updated_clusterrole="${original_clusterrole}${additional_permissions}"
  echo "Patched CoreDNS clusterrole:"
  echo "${updated_clusterrole}"
  printf '%s' "${updated_clusterrole}" | kubectl apply -f -
}

add_ocp_dnsnameresolver_to_coredns_config() {
  original_corefile=$(kubectl get -n=kube-system configmap/coredns -o=jsonpath="{.data['Corefile']}")
  if ! grep -wq "ocp_dnsnameresolver" <<< ${original_corefile}; then
    echo "Original CoreDNS Corefile:"
    echo "${original_corefile}"
    updated_corefile=$(
      printf '%s' "${original_corefile}" | sed -e 's/^\(.*\)\(forward.*\)/\1ocp_dnsnameresolver {\n\1   namespaces ovn-kubernetes\n\1}\n\1\2/'
    )
    echo "Patched CoreDNS Corefile:"
    echo "${updated_corefile}"
    printf '%s' "${updated_corefile}" > /tmp/Corefile.json
    updated_coredns=$(kubectl create configmap coredns -n=kube-system --from-file=Corefile=/tmp/Corefile.json -oyaml --dry-run=client)
    echo "Patched CoreDNS config:"
    echo "${updated_coredns}"
    printf '%s' "${updated_coredns}" | kubectl apply -f -
  fi
}

update_coredns_deployment_image() {
  kubectl -n=kube-system set image deploy/coredns coredns=${COREDNS_WITH_OCP_DNSNAMERESOLVER}
}

# kubectl_wait_dnsnameresolver_pods will set a total timeout of 60s and wait for the pods
# related to the dns name resolver feature to become "Ready".
kubectl_wait_dnsnameresolver_pods() {
  TIMEOUT=60

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + TIMEOUT ))

  timeout=$(calculate_timeout ${endtime})
  echo "Waiting for pods in dnsnameresolver-operator namespace to become ready (timeout ${timeout})..."
  kubectl wait -n dnsnameresolver-operator --for=condition=ready pods --all --timeout=${timeout}s
}

get_kubevirt_release_url() {
    local VERSION="$1"

    local kubevirt_version
    local kubevirt_release_url

    if [[ "$VERSION" == "stable" ]]; then
        kubevirt_version=$(curl -sL https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt)
        kubevirt_release_url="https://github.com/kubevirt/kubevirt/releases/download/${kubevirt_version}"
    elif [[ "$VERSION" == v* ]]; then
        kubevirt_version="$VERSION"
        kubevirt_release_url="https://github.com/kubevirt/kubevirt/releases/download/${kubevirt_version}"
    elif [[ "$VERSION" == "nightly" ]]; then
        kubevirt_version=$(curl -sL https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/latest)
        kubevirt_release_url="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${kubevirt_version}"
    elif [[ "$VERSION" =~ ^[0-9]{8}$ ]]; then
        kubevirt_version="$VERSION"
        kubevirt_release_url="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${kubevirt_version}"
    else
        echo "Unsupported KUBEVIRT_VERSION value $VERSION (use either stable, vX.Y.Z, nightly or nightly tag)"
        exit 1
    fi

    echo "$kubevirt_release_url"
}

readonly FRR_K8S_VERSION=v0.0.0-20260603082256-b43efcb206be
readonly FRR_K8S_GIT_REF=b43efcb206be
readonly FRR_K8S_OVNK_BGP_PATCH="${DIR}/frr-k8s/patches/0001-Improvements-to-the-demo.patch"
readonly FRR_K8S_UPSTREAM_FRR_IMAGE=quay.io/frrouting/frr:10.4.3
readonly FRR_DEPLOYED_IMAGE=quay.io/frrouting/frr:10.6.0
# Override to test newer FRR builds in the in-cluster frr-k8s daemonset
# without changing the pinned frr-k8s release.
FRR_K8S_FRR_IMAGE=${FRR_K8S_FRR_IMAGE:-${FRR_DEPLOYED_IMAGE}}
readonly FRR_TMP_DIR=$(mktemp -d -u)

clone_frr() {
  [ -d "$FRR_TMP_DIR" ] || {
    mkdir -p "$FRR_TMP_DIR" && trap 'rm -rf $FRR_TMP_DIR' EXIT
    pushd "$FRR_TMP_DIR" || exit 1
    git clone --no-tags --single-branch --branch main https://github.com/metallb/frr-k8s
    pushd frr-k8s
    git checkout --detach "$FRR_K8S_GIT_REF"
    popd

    pushd frr-k8s
    if ! git apply "${FRR_K8S_OVNK_BGP_PATCH}"; then
      echo "Failed to apply ${FRR_K8S_OVNK_BGP_PATCH} to frr-k8s ${FRR_K8S_GIT_REF}; refresh the patch." >&2
      exit 1
    fi

    # The local OVN-K demo patch is refreshed against FRR_K8S_GIT_REF and keeps
    # the upstream demo image unchanged. Replace that exact pinned image with
    # the FRR version configured by this script.
    replace_in_file_or_exit \
      hack/demo/demo.sh \
      "${FRR_K8S_UPSTREAM_FRR_IMAGE}" \
      "${FRR_DEPLOYED_IMAGE}"

    popd

    popd || exit 1
  }
}

deploy_frr_external_container() {
  echo "Deploying FRR external container ..."
  clone_frr
 
  pushd "$FRR_TMP_DIR" || exit 1
  run_kubectl apply -f frr-k8s/charts/frr-k8s/charts/crds/templates/
  popd || exit 1
 
  # apply the demo which will deploy an external FRR container that the cluster
  # can peer with acting as BGP (reflector) external gateway
  pushd "${FRR_TMP_DIR}"/frr-k8s/hack/demo || exit 1
  # Add the configured BGP server network prefixes to the demo FRR config.
  # The carried FRR-k8s patch already renders neighbors as route reflector
  # clients.
  grep -q 'network '"${BGP_SERVER_NET_SUBNET_IPV4}" frr/frr.conf.tmpl || \
    sed -i '/address-family ipv4 unicast/a \ \ network '"${BGP_SERVER_NET_SUBNET_IPV4}"'' frr/frr.conf.tmpl

  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Check if IPv6 address-family section exists
    if ! grep -q 'address-family ipv6 unicast' frr/frr.conf.tmpl; then
      # Add IPv6 address-family section if it doesn't exist
      sed -i '/exit-address-family/a \ \
  address-family ipv6 unicast\
    network '"${BGP_SERVER_NET_SUBNET_IPV6}"'\
  exit-address-family' frr/frr.conf.tmpl
    else
      # Add network to existing IPv6 section
      sed -i '/address-family ipv6 unicast/a \ \ network '"${BGP_SERVER_NET_SUBNET_IPV6}"'' frr/frr.conf.tmpl
    fi
  fi
  if [ "${OCI_BIN}" == "podman" ]; then
    # frr-k8s' demo script prefers docker when both docker and podman are
    # installed. Force its podman path, and avoid its host-network fallback
    # because podman cannot later attach a host-network container to the BGP
    # server network.
    replace_in_file_or_exit \
      ./demo.sh \
      'CLI=docker' \
      'CLI=podman'
    replace_in_file_or_exit \
      ./demo.sh \
      'CLI_BR_NET_BY_SUBNET_FN="docker_get_br_net_by_subnet"' \
      'CLI_BR_NET_BY_SUBNET_FN="podman_get_br_net_by_subnet"'
    sed -i '/^pushd \.\/frr\/ && {/i\
if [ "$CLI" = "podman" ]; then\
    NETWORK=${FRR_K8S_DEMO_NETWORK:-kind}\
fi\
' ./demo.sh
  fi
  # The vendored demo patch reads the external FRR container name from
  # FRR_CONTAINER_NAME.
  FRR_CONTAINER_NAME="${FRR_CONTAINER_NAME}" ./demo.sh
  popd || exit 1
  if frr_k8s_remote_enabled && [ -n "${DPU_SIM_GATEWAY_NETWORK:-}" ]; then
    configure_dpu_sim_frr_gateway_peers
  fi
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Enable IPv6 forwarding in FRR
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" sysctl -w net.ipv6.conf.all.forwarding=1
    # Enable keep_addr_on_down to preserve IPv6 addresses during VRF enslavement.
    # Without this, IPv6 global addresses are removed when interfaces are moved to a VRF,
    # causing FRR/zebra to fail creating FIB nexthop groups ("no fib nhg" bug).
    # See: https://docs.kernel.org/networking/vrf.html (section 4: Enslave L3 interfaces)
    #      https://github.com/FRRouting/frr/issues/1666
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" sysctl -w net.ipv6.conf.all.keep_addr_on_down=1
  fi

  if [ "$ENABLE_EVPN" == true ]; then
    echo "Configuring global EVPN BGP on external FRR (advertise-all-vni + neighbor activation)..."
    # Enable l2vpn evpn address-family, activate all neighbors, and advertise-all-vni.
    # Neighbors are already configured by demo.sh; extract them from the running config.
    # This is cluster-level infrastructure shared across all EVPN tests; configured once
    # at install time so individual tests don't need to manage it.
    # Wait for FRR daemons to be ready ("Not all daemons are up, cannot write config").
    local attempts=0 daemon_status
    while ! daemon_status=$($OCI_BIN exec "${FRR_CONTAINER_NAME}" vtysh -c "show daemons" 2>&1); do
      if (( ++attempts > 30 )); then
        echo "error: FRR daemons did not become ready after 30 attempts"
        echo "last daemon status: $daemon_status"
        exit 1
      fi
      sleep 1
    done
    local bgp_neighbors vtysh_cmds
    bgp_neighbors=$($OCI_BIN exec "${FRR_CONTAINER_NAME}" vtysh -c "show running-config" | grep "^ neighbor.*remote-as" | awk '{print $2}')
    vtysh_cmds=(-c "configure terminal" -c "router bgp 64512" -c "address-family l2vpn evpn")
    for neighbor in $bgp_neighbors; do
      vtysh_cmds+=(-c "neighbor $neighbor activate")
      vtysh_cmds+=(-c "neighbor $neighbor route-reflector-client")
    done
    vtysh_cmds+=(-c "advertise-all-vni" -c "exit-address-family" -c "end" -c "write memory")
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" vtysh "${vtysh_cmds[@]}"
    echo "Global EVPN BGP config complete on external FRR"
  fi
}

deploy_bgp_external_server() {
  # We create an external docker container that acts as the server (or client) outside the cluster
  # in the e2e tests that levergae router advertisements.
  # This container will be connected to the frr container deployed above to simulate a realistic
  # network topology
  # -----------------               ------------------                         ---------------------
  # |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
  # |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
  # |    server     |               |                |                         |    ovn-worker     |   (client pod advertised
  # -----------------               ------------------                         ---------------------    using RouteAdvertisements
  #                                                                            |    ovn-worker2    |    from default pod network)
  #                                                                            ---------------------
  local ip_family ipv6_network
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="dual"
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  elif  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="ipv6"
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  else
    ip_family="ipv4"
    ipv6_network=""
  fi
  $OCI_BIN rm -f "${BGP_SERVER_NAME}"
  $OCI_BIN network rm -f "${BGP_NET_NAME}"
  $OCI_BIN network create --subnet="${BGP_SERVER_NET_SUBNET_IPV4}" ${ipv6_network} --driver bridge "${BGP_NET_NAME}"
  $OCI_BIN network connect "${BGP_NET_NAME}" "${FRR_CONTAINER_NAME}"
  $OCI_BIN run  --cap-add NET_ADMIN --user 0  -d --network "${BGP_NET_NAME}"  --rm  --name "${BGP_SERVER_NAME}" -p "${BGP_SERVER_HOST_PORT}:8080"  registry.k8s.io/e2e-test-images/agnhost:2.45 netexec
  # let's make the bgp external server have its default route towards FRR router so that we don't need to add routes during tests back to the pods in the
  # cluster for return traffic
  local bgp_network_frr_v4 bgp_network_frr_v6 kind_network_frr_v4 kind_network_frr_v6
  bgp_network_frr_v4=$($OCI_BIN inspect -f "{{(index .NetworkSettings.Networks \"${BGP_NET_NAME}\").IPAddress}}" "${FRR_CONTAINER_NAME}")
  echo "FRR bgp network IPv4: ${bgp_network_frr_v4}"
  $OCI_BIN exec "${BGP_SERVER_NAME}" ip route replace default via "$bgp_network_frr_v4"
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ] ; then
    bgp_network_frr_v6=$($OCI_BIN inspect -f "{{(index .NetworkSettings.Networks \"${BGP_NET_NAME}\").GlobalIPv6Address}}" "${FRR_CONTAINER_NAME}")
    echo "FRR bgp network IPv6: ${bgp_network_frr_v6}"
    $OCI_BIN exec "${BGP_SERVER_NAME}" ip -6 route replace default via "$bgp_network_frr_v6"
  fi
  if [ "$ADVERTISED_UDN_ISOLATION_MODE" == "loose" ]; then
    kind_network_frr_v4=$($OCI_BIN inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${FRR_CONTAINER_NAME}")
    echo "FRR kind network IPv4: ${kind_network_frr_v4}"
    # If UDN isolation is in loose disabled, we need to set the default gateway for the nodes in the cluster
    # to the FRR router so that cross-UDN traffic can be routed back to the pods in the cluster in the loose mode.
    echo "Setting default gateway for nodes in the cluster to FRR router IPv4: ${kind_network_frr_v4}"
    set_nodes_default_gw "$kind_network_frr_v4"
    if  [ "$PLATFORM_IPV6_SUPPORT" == true ] ; then
      kind_network_frr_v6=$($OCI_BIN inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.GlobalIPv6Address}}{{end}}' "${FRR_CONTAINER_NAME}")
      echo "FRR kind network IPv6: ${kind_network_frr_v6}"
      set_nodes_default_gw "$kind_network_frr_v6"
    fi
  else
    # disable the default route to make sure the container only routes accross
    # directly connected or learnt networks (doing this at the very end since
    # docker changes the routing table when a new network is connected)
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" ip route delete default
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" ip route
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" ip -6 route delete default
    $OCI_BIN exec "${FRR_CONTAINER_NAME}" ip -6 route
  fi
}

set_nodes_default_gw() {
  local gw="$1"
  local ip_cmd="ip"
  local route_cmd="route replace default via"

  # Check if $gw is IPv6 (contains ':')
  if [[ "$gw" == *:* ]]; then
    ip_cmd="ip -6"
  fi

  KIND_NODES=$(kind_get_nodes)
  for node in $KIND_NODES; do
    $OCI_BIN exec "$node" $ip_cmd $route_cmd "$gw"
  done
}

destroy_bgp() {
  if $OCI_BIN ps --format '{{.Names}}' | grep -Fxq -- "${BGP_SERVER_NAME}"; then
      $OCI_BIN stop "${BGP_SERVER_NAME}"
  fi
  if $OCI_BIN ps --format '{{.Names}}' | grep -Fxq -- "${FRR_CONTAINER_NAME}"; then
      $OCI_BIN stop "${FRR_CONTAINER_NAME}"
  fi
  if $OCI_BIN network ls --format '{{.Name}}' | grep -Fxq -- "${BGP_NET_NAME}"; then
      $OCI_BIN network rm "${BGP_NET_NAME}"
  fi
}

install_frr_k8s_crds() {
  echo "Installing frr-k8s CRDs ..."
  clone_frr
  kubectl apply -f "${FRR_TMP_DIR}"/frr-k8s/config/crd/bases/
}

install_frr_k8s() {
  local bgp_port=${1:-0}
  echo "Installing frr-k8s ..."
  clone_frr

  # apply frr-k8s

  # The all-in-one manifest is only consumed here (deploy_frr_external_container
  # uses CRDs and the demo scripts, not this manifest), so the fix lives here
  # rather than in clone_frr.
  #
  # gcr.io/kubebuilder/kube-rbac-proxy is unavailable after Google's Container
  # Registry shutdown (https://cloud.google.com/container-registry/docs/deprecations/container-registry-deprecation).
  # Upstream bug: https://github.com/metallb/metallb/issues/2619
  # Use the same image from the Kubernetes community registry instead.
  # REVERT ME: when https://github.com/metallb/metallb/issues/2619 is fixed
  sed -i 's|gcr.io/kubebuilder/kube-rbac-proxy|registry.k8s.io/kubebuilder/kube-rbac-proxy|g' \
    "${FRR_TMP_DIR}"/frr-k8s/config/all-in-one/frr-k8s.yaml

  # This BGP e2e setup uses two FRR containers:
  # 1. The external FRR test router from hack/demo/demo.sh. clone_frr()
  #    patches that image to FRR_DEPLOYED_IMAGE.
  # 2. The in-cluster frr-k8s daemonset from config/all-in-one/frr-k8s.yaml.
  #    Patch that manifest here because clone_frr() does not update it.
  #
  # In regular PR e2e jobs where nobody sets a custom FRR_K8S_FRR_IMAGE
  # environment variable, both containers use FRR_DEPLOYED_IMAGE.
  # FRR_K8S_FRR_IMAGE remains overrideable for tests that intentionally need a
  # different in-cluster daemonset image.
  #
  # Match the manifest's current upstream image exactly. If the pinned frr-k8s
  # manifest changes its FRR image, replace_in_file_or_exit fails and this
  # override must be reviewed before it changes what CI deploys.
  replace_in_file_or_exit \
    "${FRR_TMP_DIR}"/frr-k8s/config/all-in-one/frr-k8s.yaml \
    "${FRR_K8S_UPSTREAM_FRR_IMAGE}" \
    "${FRR_K8S_FRR_IMAGE}"

  if [ "${bgp_port}" -ne 0 ]; then
    local frr_yaml="${FRR_TMP_DIR}/frr-k8s/config/all-in-one/frr-k8s.yaml"
    grep -q 'bgpd_options=.*-p 0' "$frr_yaml" || {
      echo "expected bgpd_options with '-p 0' not found in $frr_yaml"
      exit 1
    }
    sed -i "s/bgpd_options=\"\(.*\)-p 0\(.*\)\"/bgpd_options=\"\1-p ${bgp_port}\2\"/g" "$frr_yaml"
    grep -q "bgpd_options=.*-p ${bgp_port}" "$frr_yaml" || {
      echo "failed to patch bgpd_options to use port ${bgp_port}"
      exit 1
    }
  fi
  install_frr_k8s_host_api_crds
  kubectl apply -f "${FRR_TMP_DIR}"/frr-k8s/config/all-in-one/frr-k8s.yaml
  create_frr_k8s_remote_kubeconfig_secret
  configure_frr_k8s_remote_daemonsets
}

wait_for_frr_k8s() {
  # The pinned frr-k8s all-in-one manifest runs the webhook-serving helper as
  # deployment/frr-k8s-statuscleaner. Wait for it before tests create
  # FRRConfiguration resources so admission does not race the install.
  if kubectl -n frr-k8s-system get deployment frr-k8s-statuscleaner >/dev/null 2>&1; then
    kubectl wait -n frr-k8s-system deployment frr-k8s-statuscleaner --for condition=Available --timeout 2m
  fi
  if kubectl -n frr-k8s-system get daemonset frr-k8s-daemon >/dev/null 2>&1; then
    kubectl rollout status -n frr-k8s-system daemonset frr-k8s-daemon --timeout 2m
    return
  fi
  local ds found=false
  for ds in $(kubectl -n frr-k8s-system get daemonset -l dpu-sim.ovn.org/frr-remote=true -o name); do
    found=true
    kubectl rollout status -n frr-k8s-system "${ds}" --timeout 2m
  done
  if [ "${found}" != true ]; then
    echo "No local or remote FRR-K8S daemonsets were found in namespace frr-k8s-system" >&2
    return 1
  fi
}

apply_frr_k8s_receive_config() {
  # apply a BGP peer configration with the external gateway that does not
  # exchange routes
  pushd "${FRR_TMP_DIR}"/frr-k8s/hack/demo/configs || exit 1
  sed 's/mode: all/mode: filtered/g' receive_all.yaml > receive_filtered.yaml
  if frr_k8s_remote_enabled && [ -n "${DPU_SIM_GATEWAY_NETWORK:-}" ]; then
    configure_dpu_sim_frr_receive_config receive_filtered.yaml
  fi
  # Allow receiving the bgp external server's prefix
  sed -i '/mode: filtered/a\            prefixes:\n            - prefix: '"${BGP_SERVER_NET_SUBNET_IPV4}"'' receive_filtered.yaml
  # If IPv6 is enabled, add the IPv6 prefix as well
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Find all line numbers where the IPv4 prefix is defined
    IPv6_LINE="            - prefix: ${BGP_SERVER_NET_SUBNET_IPV6}"
    # Process each occurrence of the IPv4 prefix in reverse order to avoid line number shifting
    for LINE_NUM in $(grep -n "prefix: ${BGP_SERVER_NET_SUBNET_IPV4}" receive_filtered.yaml | cut -d ':' -f 1 | sort -rn); do
      # Insert the IPv6 prefix after each IPv4 prefix line
      sed -i "${LINE_NUM}a\\${IPv6_LINE}" receive_filtered.yaml
    done
  fi
  
  # frr-k8s webhook is declaring readiness before its endpoint is serving.
  # Let's do our own probing. Also will print logs in case of failure so we get
  # insights on why this is hapenning. In remote mode the host API does not use
  # the DPU-cluster webhook service, so skip this probe.
  local r
  r=0
  if ! frr_k8s_remote_enabled; then
    timeout 60s bash -x <<EOF || r=$?
echo "Attempting to reach frr-k8s webhook"
kind export kubeconfig --name ovn
while true; do
CLUSTER_IP=\$(kubectl get svc -n frr-k8s-system frr-k8s-webhook-service -o jsonpath='{.spec.clusterIP}')
# Wrap IPv6 addresses in brackets for URL syntax
[[ \${CLUSTER_IP} =~ : ]] && CLUSTER_IP="[\${CLUSTER_IP}]"
$OCI_BIN exec ovn-control-plane curl -ksS --connect-timeout 0.1 https://\${CLUSTER_IP}
[ \$? -eq 0 ] && exit 0
echo "Couldn't reach frr-k8s webhook, trying in 1s..."
sleep 1s
done
EOF
  fi
  echo "r=$r"
  if [ "$r" -ne "0" ]; then
    kubectl describe pod -n frr-k8s-system -l app=frr-k8s-webhook-server
    kubectl logs -n frr-k8s-system -l app=frr-k8s-webhook-server
  fi

  local kubectl_cmd=(kubectl)
  if frr_k8s_remote_enabled; then
    kubectl_cmd=(kubectl --kubeconfig "$(frr_k8s_host_kubeconfig)")
  fi
  "${kubectl_cmd[@]}" apply -n frr-k8s-system -f receive_filtered.yaml
  popd || exit 1
}

configure_frr_k8s_routes() {
  # Add routes for pod networks dynamically into the github runner for return traffic to pass back
  if [ "$ADVERTISE_DEFAULT_NETWORK" = "true" ]; then
    echo "Adding routes for Kubernetes pod networks..."
    NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
    echo "Found nodes: $NODES"
    for node in $NODES; do
      # Get the addresses
      node_ips=$(kubectl get node $node -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
      # Get subnet information
      subnet_json=$(kubectl get node $node -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-subnets}')
      
      if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
        # Extract IPv4 address (first address)
        node_ipv4=$(echo "$node_ips" | awk '{print $1}')
        ipv4_subnet=$(echo "$subnet_json" | jq -r '.default[0]')
        
        # Add IPv4 route
        if [ -n "$ipv4_subnet" ] && [ -n "$node_ipv4" ]; then
          echo "Adding IPv4 route for $node ($node_ipv4): $ipv4_subnet"
          sudo ip route replace $ipv4_subnet via $node_ipv4
        fi
      fi

      # Add IPv6 route if enabled
      if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
        # Extract IPv6 address (second address, if present)
        node_ipv6=$(echo "$node_ips" | awk '{print $2}')
        ipv6_subnet=$(echo "$subnet_json" | jq -r '.default[1] // empty')
        
        if [ -n "$ipv6_subnet" ] && [ -n "$node_ipv6" ]; then
          echo "Adding IPv6 route for $node ($node_ipv6): $ipv6_subnet"
          sudo ip -6 route replace $ipv6_subnet via $node_ipv6
        fi
      fi
    done
  fi

  rm -rf "${FRR_TMP_DIR}"
}

configure_frr_k8s() {
  apply_frr_k8s_receive_config
  configure_frr_k8s_routes
}

setup_coredumps() {
  # Setup core dump collection
  #
  # Core dumps will be saved on the HOST at $COREDUMP_DIR (not inside containers)
  # because kernel.core_pattern is a kernel-level setting shared across all containers.
  #
  # - Using a pipe instead of a file path avoids needing to mount
  #   $COREDUMP_DIR into every container that might crash
  # - The pipe executes in the host's namespace, so $COREDUMP_DIR
  #   automatically refers to the host path
  #
  # Location: COREDUMP_DIR is under /tmp/kind/logs to ensure coredumps are exported in CI
  # Use container exec to avoid asking for root permissions

  mkdir -p "$COREDUMP_DIR"
  ulimit -c unlimited
  for node in $(kind get nodes --name "${KIND_CLUSTER_NAME}"); do
    # Core dump filename pattern variables:
    #   %P - global PID
    #   %e - executable filename
    #   %h - hostname (container hostname)
    #   %s - signal number that caused dump
    ${OCI_BIN} exec "$node" sysctl -w kernel.core_pattern="|/bin/dd of=${COREDUMP_DIR}/core.%P.%e.%h.%s bs=1M status=none"
  done
}

wait_for_coredumps() {
  # Wait for any in-progress coredump writes to complete
  # The kernel pipes coredumps to dd processes, which can take 30+ seconds for large Go binaries
  #
  # Challenge: Go's crash handling (printing stack traces for all goroutines) takes
  # several seconds BEFORE it calls abort() and the kernel starts the coredump.
  # So we can't just check for dd processes - we need to wait for potential crashes
  # to fully materialize.

  local max_wait=120  # Maximum wait time in seconds
  local initial_wait=15  # Initial wait for Go crash handling to complete
  local waited=0

  if [ ! -d "$COREDUMP_DIR" ]; then
    return 0
  fi

  # Record initial coredump count
  local initial_count
  initial_count=$(find "$COREDUMP_DIR" -maxdepth 1 -name "core.*" -type f 2>/dev/null | wc -l || echo 0)
  echo "Checking for in-progress coredump writes (initial count: $initial_count)..."

  # Initial wait: Go's crash handling (printing goroutine stack traces) can take
  # 10+ seconds before abort() is called and the kernel starts the coredump
  echo "Waiting ${initial_wait}s for any pending crash handling to complete..."
  sleep "$initial_wait"
  waited=$initial_wait

  while [ $waited -lt $max_wait ]; do
    # Check for dd processes writing to the coredump directory
    local dd_procs
    dd_procs=$(pgrep -f "dd of=${COREDUMP_DIR}" 2>/dev/null || true)

    # Check current coredump count
    local current_count
    current_count=$(find "$COREDUMP_DIR" -maxdepth 1 -name "core.*" -type f 2>/dev/null | wc -l || echo 0)

    if [ -z "$dd_procs" ]; then
      # No dd processes running
      if [ "$current_count" -gt "$initial_count" ]; then
        echo "New coredumps detected (initial: $initial_count, current: $current_count) after ${waited}s"
      fi
      echo "No coredump writes in progress after ${waited}s"
      return 0
    fi

    echo "Waiting for coredump writes... (${waited}s, dd PIDs: $dd_procs, coredumps: $current_count)"
    sleep 5
    waited=$((waited + 5))
  done

  echo "Warning: Timed out waiting for coredump writes after ${max_wait}s"
}

export_logs() {
  # Export kind logs and collect coredump binaries
  # Usage: export_logs [logs_dir]
  # Default logs_dir: /tmp/kind/logs

  local logs_dir="${1:-/tmp/kind/logs}"

  mkdir -p "$logs_dir"

  # Wait for any in-progress coredump writes to complete before exporting
  wait_for_coredumps

  kind export logs --name "${KIND_CLUSTER_NAME}" --verbosity 4 "$logs_dir"
  collect_coredump_binaries
}

# Helper function to try extracting a binary from a container
# Used by collect_coredump_binaries()
try_extract_binary() {
  local node=$1
  local container_id=$2
  local exe=$3
  local binary_dir=$4

  # Get container's PID to access its rootfs via /proc/<pid>/root
  local pid
  pid=$(${OCI_BIN} exec "$node" crictl inspect "$container_id" 2>/dev/null | jq -r '.info.pid // empty')
  if [ -z "$pid" ] || [ "$pid" = "null" ] || [ "$pid" = "0" ]; then
    return 1
  fi

  # Common paths where binaries might be located
  local binary_paths=("/usr/bin" "/bin" "/usr/sbin" "/sbin" "/usr/libexec/cni" "/usr/lib/frr")

  for path in "${binary_paths[@]}"; do
    local full_path="/proc/${pid}/root${path}/${exe}"
    if ${OCI_BIN} exec "$node" test -f "$full_path" 2>/dev/null; then
      if ${OCI_BIN} exec "$node" cat "$full_path" > "${binary_dir}/${exe}" 2>/dev/null && [ -s "${binary_dir}/${exe}" ]; then
        echo "    Collected binary: ${exe} from container $container_id (pid $pid)"
        return 0
      fi
    fi
  done
  rm -f "${binary_dir}/${exe}" 2>/dev/null
  return 1
}

# Save the container image metadata for a collected binary
# Used by collect_coredump_binaries() to record which image the binary came from
# Extracts:
#   - OVN image reference from container metadata (crictl inspect)
#   - Fedora base image digest from /root/fedora_base_image (embedded at build time)
collect_container_image_info() {
  local node=$1
  local container_id=$2
  local exe=$3
  local binary_dir=$4

  local inspect_out
  inspect_out=$(${OCI_BIN} exec "$node" crictl inspect "$container_id" 2>/dev/null) || true

  local image_ref
  image_ref=$(echo "$inspect_out" | jq -r '.status.imageRef // empty')
  if [ -n "$image_ref" ]; then
    echo "${exe}: ${image_ref}" >> "${binary_dir}/image-info.txt"
    echo "  Image ref: ${image_ref}"
  fi

  # Extract the Fedora base image digest embedded during docker build
  # (only needs to be collected once since all containers share the same base)
  if [ ! -f "${binary_dir}/fedora-base-image.txt" ]; then
    local pid
    pid=$(echo "$inspect_out" | jq -r '.info.pid // empty')
    if [ -n "$pid" ] && [ "$pid" != "null" ] && [ "$pid" != "0" ]; then
      local digest
      digest=$(${OCI_BIN} exec "$node" cat "/proc/${pid}/root/root/fedora_base_image" 2>/dev/null) || true
      if [ -n "$digest" ]; then
        echo "$digest" > "${binary_dir}/fedora-base-image.txt"
        echo "  Fedora base image: ${digest}"
      fi
    fi
  fi
}

collect_coredump_binaries() {
  # Collect binaries that caused coredumps for post-mortem debugging
  # Parses coredump filenames (core.%P.%e.%h.%s) to identify executables
  # Binaries run inside pod containers, so we use crictl to access them

  local binary_dir="${COREDUMP_DIR}/binaries"

  if [ ! -d "$COREDUMP_DIR" ]; then
    echo "No coredump directory found, skipping binary collection"
    return 0
  fi

  local coredumps
  coredumps=$(find "$COREDUMP_DIR" -maxdepth 1 -name "core.*" -type f 2>/dev/null)
  if [ -z "$coredumps" ]; then
    echo "No coredumps found, skipping binary collection"
    return 0
  fi

  mkdir -p "$binary_dir"

  # Get all KIND nodes
  local nodes
  nodes=$(kind get nodes --name "${KIND_CLUSTER_NAME}" 2>/dev/null)
  if [ -z "$nodes" ]; then
    echo "Warning: No KIND nodes available, cannot collect binaries"
    return 0
  fi

  # Process each coredump: extract exe name (%e, field 3)
  # Filename format: core.%P.%e.%h.%s (see setup_coredumps)
  for coredump in $coredumps; do
    local filename
    filename=$(basename "$coredump")
    local exe
    exe=$(echo "$filename" | cut -d. -f3)

    echo "Processing coredump: $filename (exe=$exe)"

    # Skip if we already collected this binary
    if [ -f "${binary_dir}/${exe}" ]; then
      echo "  Binary $exe already collected, skipping"
      continue
    fi

    local found=false

    # Search all containers on all nodes for the binary
    for node in $nodes; do
      local containers
      containers=$(${OCI_BIN} exec "$node" crictl ps -q 2>/dev/null) || true
      for container_id in $containers; do
        if try_extract_binary "$node" "$container_id" "$exe" "$binary_dir"; then
          echo "  Collected $exe from container $container_id on node $node"
          collect_container_image_info "$node" "$container_id" "$exe" "$binary_dir"
          found=true
          break 2
        fi
      done
    done

    # Fallback: binary running directly on KIND node (not in container)
    if [ "$found" = false ]; then
      for node in $nodes; do
        local bin_path
        bin_path=$(${OCI_BIN} exec "$node" which "$exe" 2>/dev/null) || true
        if [ -n "$bin_path" ]; then
          echo "  Collected $exe from node $node at $bin_path"
          ${OCI_BIN} cp "${node}:${bin_path}" "${binary_dir}/${exe}" && found=true || true
          break
        fi
      done
    fi

    if [ "$found" = false ]; then
      echo "  WARNING: Could not find binary '$exe'"
    fi
  done

  echo "Binary collection complete:"
  ls -la "$binary_dir" 2>/dev/null || true
}

# Some environments (Fedora32,31 on desktop), have problems when the cluster
# is deleted directly with kind `kind delete cluster --name ovn`, it restarts the host.
# The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
# with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.
delete() {
  OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}

  if [ "$KIND_INSTALL_METALLB" == true ]; then
    destroy_metallb
  fi
  if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
    destroy_bgp
  fi
  timeout 5 kubectl --kubeconfig "${KUBECONFIG}" delete namespace ovn-kubernetes || true
  sleep 5
  kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"
}

create_kind_cluster() {
  # Output of the jinjanate command
  KIND_CONFIG_LCL=${DIR}/kind-${KIND_CLUSTER_NAME}.yaml

  ovn_ip_family=${IP_FAMILY} \
  net_cidr="${KIND_CIDR}" \
  svc_cidr=${SVC_CIDR} \
  dns_domain=${KIND_DNS_DOMAIN} \
  ovn_num_worker=${KIND_NUM_WORKER} \
  kind_num_infra=${KIND_NUM_INFRA} \
  cluster_log_level=${KIND_CLUSTER_LOGLEVEL:-4} \
  jinjanate "${KIND_CONFIG}" -o "${KIND_CONFIG_LCL}"

  # Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
  if kind get clusters | grep "${KIND_CLUSTER_NAME}"; then
    delete
  fi

  if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
    create_local_registry
  fi

  kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config=${KIND_CONFIG_LCL} --retain

  cat "${KUBECONFIG}"
}

remove_no_schedule_taint() {
  KIND_NODES=$(kind_get_nodes | sort)
  for n in $KIND_NODES; do
    # do not error if it fails to remove the taint
    kubectl taint node "$n" node-role.kubernetes.io/control-plane:NoSchedule- || true
  done
}

OPENSSL=""
set_openssl_binary() {
  for s in openssl openssl3; do
      if ! command_exists "${s}" ; then
          continue
      fi
      if [ "$(${s} version | awk -F '[ |.]' '{print $2}')" == "3" ]; then
          OPENSSL="${s}"
          echo "Found OpenSSL version 3 in binary ${OPENSSL}"
          break
      fi
  done
  if [ "${OPENSSL}" == "" ] ; then
    echo "Dependency not met: Cannot find openssl version 3 (searched for openssl and openssl3)"
    exit 1
  fi
}

scale_kind_cluster() {
  echo "Scaling cluster to ${KIND_NUM_WORKER} workers..."
  rm -rf /tmp/kindscaler
  # change this to https://github.com/lobuhi/kindscaler once PR https://github.com/lobuhi/kindscaler/pull/1 is accepted
  git clone https://github.com/trozet/kindscaler /tmp/kindscaler
  /tmp/kindscaler/kindscaler.sh ${KIND_CLUSTER_NAME} -r worker -c ${KIND_NUM_WORKER}
  if [ "$OVN_IMAGE" == local ]; then
    set_ovn_image
  fi
  install_ovn_image
  if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    set_dnsnameresolver_images
    install_dnsnameresolver_images
  fi
}

# install_ipsec sets up IPsec once the ovn-ipsec DaemonSet is deployed by the helm chart
# (tags.ovn-ipsec=true). It creates a CA, publishes it as the ovn-kubernetes/signer-ca
# ConfigMap, and signs any pending CSRs with that CA until the DaemonSet rolls out.
# Make sure to run this at the very end of the setup process.
install_ipsec() {
  # Create the CA (stored inside the signer-ca ConfigMap) that the IPsec pods use to sign their certificates
  ca_dir=$(mktemp -d)
  pushd "${ca_dir}"
  ${OPENSSL} genrsa -out ca-bundle.key 4096
  ${OPENSSL} req -x509 -new -nodes -key ca-bundle.key -sha256 -days 10240 -out ca-bundle.crt \
      -subj "/C=CA/ST=Arctica/L=Northpole/O=Acme Inc/OU=DevOps/CN=www.example.com/emailAddress=dev@www.example.com"
  kubectl create configmap -n ovn-kubernetes signer-ca --from-file ca-bundle.crt \
      --dry-run=client -o yaml | kubectl apply -f -

  # For ca. 5 minutes max (60 * 5 seconds + overhead) ...
  success=false
  for i in {1..60}; do
    # ... try to get all CSRs and sign them
    csrs=$(kubectl get csr -o go-template='{{range .items}}{{if not .status}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}')
    for csr in ${csrs}; do
      kubectl get csr "${csr}" -o jsonpath='{.spec.request}' | base64 --decode | \
          sed -n '/BEGIN CERTIFICATE REQUEST/,$p' > "${csr}"
      ${OPENSSL} x509 -req -in ${csr} -CA ca-bundle.crt -CAkey ca-bundle.key -CAcreateserial -out "${csr}.crt" -days 3650  \
          -sha256 -extensions v3_req -copy_extensions copy
      kubectl get csr "${csr}" -o json | \
          jq '.status.certificate = "'$(base64 "${csr}.crt" | tr -d '\n')'"' | \
          kubectl replace --raw /apis/certificates.k8s.io/v1/certificatesigningrequests/${csr}/status -f -
    done

    # ... and then check if the ovn-ipsec DaemonSet rolled out completely (wait for 5 seconds)
    if kubectl rollout status daemonset -n ovn-kubernetes ovn-ipsec --timeout 5s; then
        echo "All IPsec pods rolled out successfully"
        success=true
        break
    fi
    echo "IPsec pods did not roll out successfully yet"
  done
  popd
  rm -Rf "${ca_dir}"

  if ! ${success}; then
      echo "IPsec pods did not roll out successfully"
      exit 1
  fi
}

# run_script_in_container should be used when the script is run nested in a container
# and makes sure the control-plane node is reachable by substituting 127.0.0.1
# with the control-plane container's IP.
run_script_in_container() {
  local master_ip
  if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
    master_ip=$("$OCI_BIN" inspect -f '{{ (index .NetworkSettings.Networks "kind").IPAddress }}' "${KIND_CLUSTER_NAME}-control-plane")
    sed -i -- "s/server: .*/server: https:\/\/$master_ip:6443/g" "$KUBECONFIG"
  else
    master_ip=$("$OCI_BIN" inspect -f '{{ (index .NetworkSettings.Networks "kind").GlobalIPv6Address }}' "${KIND_CLUSTER_NAME}-control-plane")
    sed -i -- "s/server: .*/server: https:\/\/[$master_ip]:6443/g" "$KUBECONFIG"
  fi
  chmod a+r "$KUBECONFIG"
}

# fixup_kubeconfig_names ensures kind clusters are named based off provided
# values by removing the 'kind-' prefix from context/user/cluster names.
fixup_kubeconfig_names() {
  sed -i -- "s/user: kind-.*/user: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/name: kind-.*/name: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/cluster: kind-.*/cluster: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/current-context: .*/current-context: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
}

remove_default_route() {
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    "$OCI_BIN" exec "$n" ip route delete default
  done
}

add_dns_hostnames() {
  local entries=""
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    local v4 v6
    v4=$("$OCI_BIN" container inspect -f '{{ .NetworkSettings.Networks.kind.IPAddress }}' "$n")
    v6=$("$OCI_BIN" container inspect -f '{{ .NetworkSettings.Networks.kind.GlobalIPv6Address }}' "$n")
    [ -n "$v4" ] && entries+=$(printf '%s %s\n' "$v4" "$n")$'\n'
    [ -n "$v6" ] && entries+=$(printf '%s %s\n' "$v6" "$n")$'\n'
  done
  for n in $KIND_NODES; do
    printf '%s' "$entries" | "$OCI_BIN" exec -i "$n" bash -c 'cat >> /etc/hosts'
  done
}
