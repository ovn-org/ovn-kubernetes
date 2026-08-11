#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
OVN_KUBERNETES_PATH=${OVN_KUBERNETES_PATH:-$(cd "${SCRIPT_DIR}/.." && pwd)}

resolve_dpu_sim_path() {
  if [ -n "${DPU_SIM_PATH:-}" ]; then
    if [ ! -d "${DPU_SIM_PATH}" ]; then
      echo "error: DPU_SIM_PATH does not exist: ${DPU_SIM_PATH}" >&2
      exit 1
    fi
    cd "${DPU_SIM_PATH}" && pwd
    return
  fi

  local candidates=(
    "${OVN_KUBERNETES_PATH}/../dpu-simulator"
    "${OVN_KUBERNETES_PATH}/../../ovn-kubernetes/dpu-simulator"
  )

  local gopath=${GOPATH:-}
  if [ -z "${gopath}" ] && command -v go >/dev/null 2>&1; then
    gopath=$(go env GOPATH 2>/dev/null || true)
  fi
  if [ -n "${gopath}" ]; then
    local entry
    local -a gopath_entries
    IFS=: read -ra gopath_entries <<< "${gopath}"
    for entry in "${gopath_entries[@]}"; do
      candidates+=("${entry}/src/github.com/ovn-kubernetes/dpu-simulator")
    done
  fi

  local path
  for path in "${candidates[@]}"; do
    if [ -d "${path}" ]; then
      cd "${path}" && pwd
      return
    fi
  done

  echo "error: could not locate dpu-simulator checkout" >&2
  echo "set DPU_SIM_PATH to the dpu-simulator repository path" >&2
  exit 1
}

resolve_kind_provider() {
  if [ -n "${KIND_EXPERIMENTAL_PROVIDER:-}" ]; then
    echo "${KIND_EXPERIMENTAL_PROVIDER}"
    return
  fi

  if command -v docker >/dev/null 2>&1; then
    echo "docker"
    return
  fi

  if command -v podman >/dev/null 2>&1; then
    echo "podman"
    return
  fi

  echo "error: could not locate podman or docker" >&2
  echo "set KIND_EXPERIMENTAL_PROVIDER to the Kind provider to use" >&2
  exit 1
}

cleanup_bgp_artifacts() {
  local provider=$1

  if ! command -v "${provider}" >/dev/null 2>&1; then
    return
  fi

  if "${provider}" ps -a --format '{{.Names}}' | grep -Eq '^bgpserver$'; then
    "${provider}" rm -f bgpserver
  fi
  if "${provider}" ps -a --format '{{.Names}}' | grep -Eq '^frr$'; then
    "${provider}" rm -f frr
  fi
  if "${provider}" network ls --format '{{.Name}}' | grep -Eq '^bgpnet$'; then
    "${provider}" network rm bgpnet || true
  fi
}

DPU_SIM_PATH=$(resolve_dpu_sim_path)
KIND_EXPERIMENTAL_PROVIDER=$(resolve_kind_provider)
export KIND_EXPERIMENTAL_PROVIDER
KIND_HELM_OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT:-900}
export KIND_HELM_OVN_TIMEOUT
BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4:-172.29.0.0/16}
BGP_SERVER_NET_SUBNET_IPV6=${BGP_SERVER_NET_SUBNET_IPV6:-fc00:f853:ccd:e797::/64}
export BGP_SERVER_NET_SUBNET_IPV4
export BGP_SERVER_NET_SUBNET_IPV6
DPU_SIM_UPLINK_ENABLE=${DPU_SIM_UPLINK_ENABLE:-true}
DPU_SIM_UPLINK_NETWORK=${DPU_SIM_UPLINK_NETWORK:-dpu-sim-uplink}
DPU_SIM_UPLINK_SUBNET=${DPU_SIM_UPLINK_SUBNET:-172.31.0.0/24}
DPU_SIM_UPLINK_BRIDGE=${DPU_SIM_UPLINK_BRIDGE:-breth-uplink}
# dpusim.UplinkInterfaceIndex: outside the pairs created by the dpu-simulator
# (num_pairs), so the device plugin never advertises the uplink VF as a pod VF.
DPU_SIM_UPLINK_INDEX=200
DPU_SIM_UPLINK_HOST_INTERFACE="eth0-${DPU_SIM_UPLINK_INDEX}"
DPU_SIM_UPLINK_DPU_REPRESENTOR="rep0-${DPU_SIM_UPLINK_INDEX}"
DPU_SIM_CONFIG=${DPU_SIM_CONFIG:-config-kind-ovnk-offload.yaml}
HOST_CLUSTER=${HOST_CLUSTER:-dpu-sim-host}
DPU_CLUSTER=${DPU_CLUSTER:-dpu-sim-dpu}
HOST_KUBECONFIG="${DPU_SIM_PATH}/kubeconfig/${HOST_CLUSTER}.kubeconfig"
DPU_KUBECONFIG="${DPU_SIM_PATH}/kubeconfig/${DPU_CLUSTER}.kubeconfig"
HOST_VALUES="${DPU_SIM_PATH}/kubeconfig/helm-values/${HOST_CLUSTER}-ovn-kubernetes-dpu-host-values.yaml"
HOST_NO_OVERLAY_VALUES="${DPU_SIM_PATH}/kubeconfig/helm-values"
HOST_NO_OVERLAY_VALUES+="/${HOST_CLUSTER}-ovn-kubernetes-dpu-host-no-overlay-values.yaml"
DPU_VALUES="${DPU_SIM_PATH}/kubeconfig/helm-values/${DPU_CLUSTER}-ovn-kubernetes-dpu-values.yaml"
FRR_ENV="${DPU_SIM_PATH}/kubeconfig/helm-values/${DPU_CLUSTER}-frr-k8s.env"

kubectl_host() {
  kubectl --kubeconfig "${HOST_KUBECONFIG}" "$@"
}

kubectl_dpu() {
  kubectl --kubeconfig "${DPU_KUBECONFIG}" "$@"
}

cluster_command_arg() {
  local kubeconfig=$1
  local selector=$2
  local arg_name=$3
  local command

  command=$(kubectl --kubeconfig "${kubeconfig}" -n kube-system get pod \
    -l "${selector}" -o jsonpath='{.items[0].spec.containers[0].command}')
  printf '%s\n' "${command}" | tr '",' '\n' | awk -v prefix="--${arg_name}=" '
    index($0, prefix) == 1 {
      print substr($0, length(prefix) + 1)
      exit
    }
  '
}

host_cluster_cidrs() {
  local net_cidr svc_cidr

  net_cidr=$(cluster_command_arg "${HOST_KUBECONFIG}" "component=kube-controller-manager" "cluster-cidr")
  svc_cidr=$(cluster_command_arg "${HOST_KUBECONFIG}" "component=kube-apiserver" "service-cluster-ip-range")

  if [ -z "${net_cidr}" ] || [ -z "${svc_cidr}" ]; then
    echo "error: could not determine host cluster pod/service CIDRs" >&2
    exit 1
  fi

  echo "${net_cidr} ${svc_cidr}"
}

install_ovnk_host() {
  local cidrs host_net_cidr host_svc_cidr

  cidrs=$(host_cluster_cidrs)
  read -r host_net_cidr host_svc_cidr <<< "${cidrs}"
  echo "Using host cluster pod CIDR ${host_net_cidr}"
  echo "Using host cluster service CIDR ${host_svc_cidr}"
  cat > "${HOST_NO_OVERLAY_VALUES}" <<EOF
global:
  enableEgressIp: false
EOF

  pushd "${OVN_KUBERNETES_PATH}"
  NET_CIDR_IPV4="${host_net_cidr}" \
  SVC_CIDR_IPV4="${host_svc_cidr}" \
    ./contrib/kind-helm.sh \
    --deploy \
    --cluster-name "${HOST_CLUSTER}" \
    --kubeconfig "${HOST_KUBECONFIG}" \
    --dpu-mode host \
    --network-segmentation-enable \
    --dynamic-udn-allocation \
    --multi-network-enable \
    --uplink-enable \
    --route-advertisements-enable \
    --no-overlay-enable \
    --advertise-default-network \
    --extra-values "${HOST_VALUES}" \
    --extra-values "${HOST_NO_OVERLAY_VALUES}"
  popd
}

install_ovnk_dpu() {
  # shellcheck disable=SC1090
  source "${FRR_ENV}"
  export DPU_SIM_GATEWAY_NETWORK
  export DPU_SIM_GATEWAY_SUBNET

  pushd "${OVN_KUBERNETES_PATH}"
  ./contrib/kind-helm.sh \
    --deploy \
    --cluster-name "${DPU_CLUSTER}" \
    --kubeconfig "${DPU_KUBECONFIG}" \
    --dpu-mode dpu \
    --multi-network-enable \
    --network-segmentation-enable \
    --dynamic-udn-allocation \
    --uplink-enable \
    --route-advertisements-enable \
    --no-overlay-enable \
    --advertise-default-network \
    --extra-values "${DPU_VALUES}" \
    --frr-k8s-host-kubeconfig "${FRR_K8S_HOST_KUBECONFIG}" \
    --frr-k8s-remote-kubeconfig "${FRR_K8S_REMOTE_KUBECONFIG}" \
    --frr-k8s-remote-node-map "${FRR_K8S_REMOTE_NODE_MAP}"
  popd
}

wait_for_ovn() {
  kubectl_host wait --for=condition=Ready nodes --all --timeout=25m
  kubectl_dpu wait --for=condition=Ready nodes --all --timeout=25m
  kubectl_host -n ovn-kubernetes wait --for=condition=Ready pods --all --timeout=10m
  kubectl_dpu -n ovn-kubernetes wait --for=condition=Ready pods --all --timeout=10m
  kubectl_dpu -n frr-k8s-system wait --for=condition=Ready pods --all --timeout=10m
}

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
    return
  fi
  sudo "$@"
}

simulated_dpu_mac() {
  local node=$1
  local role=$2
  local index=$3

  python3 -c '
import hashlib
import sys

node, role, index = sys.argv[1], sys.argv[2], int(sys.argv[3])
h = hashlib.sha256((node + "\0" + role).encode()).digest()
print(f"52:54:00:{h[0]:02x}:{h[1]:02x}:{index & 0xff:02x}")
' "${node}" "${role}" "${index}"
}

docker_network_ip() {
  local container=$1
  local network=$2

  "${KIND_EXPERIMENTAL_PROVIDER}" inspect -f \
    "{{ with index .NetworkSettings.Networks \"${network}\" }}{{ .IPAddress }}{{ end }}" \
    "${container}"
}

container_iface_for_ip() {
  local container=$1
  local ip=$2

  "${KIND_EXPERIMENTAL_PROVIDER}" exec "${container}" sh -c \
    "ip -o -4 addr show | awk -v ip='${ip}' '\$4 ~ \"^\" ip \"/\" {print \$2; exit}'"
}

replace_host_with_dpu_node() {
  local host_node=$1
  echo "${host_node/-host-/-dpu-}"
}

configure_external_frr_uplink_peer() {
  local peer_ip=$1

  "${KIND_EXPERIMENTAL_PROVIDER}" exec frr vtysh \
    -c "configure terminal" \
    -c "router bgp 64512" \
    -c "neighbor ${peer_ip} remote-as 64512" \
    -c "address-family ipv4 unicast" \
    -c "neighbor ${peer_ip} activate" \
    -c "neighbor ${peer_ip} route-reflector-client" \
    -c "exit-address-family" \
    -c "end" \
    -c "write memory"
}

configure_dpu_sim_uplink_bridge() {
  if [ "${DPU_SIM_UPLINK_ENABLE}" != true ]; then
    return
  fi

  local prefix subnet_ip subnet_base
  prefix=${DPU_SIM_UPLINK_SUBNET#*/}
  subnet_ip=${DPU_SIM_UPLINK_SUBNET%/*}
  subnet_base=$(echo "${subnet_ip}" | awk -F. '{print $1"."$2"."$3}')

  echo "Configuring reserved DPU Uplink bridge ${DPU_SIM_UPLINK_BRIDGE}"
  if "${KIND_EXPERIMENTAL_PROVIDER}" network inspect "${DPU_SIM_UPLINK_NETWORK}" \
    >/dev/null 2>&1; then
    for container in frr $("${KIND_EXPERIMENTAL_PROVIDER}" ps \
      --format '{{.Names}}' | grep "^${DPU_CLUSTER}-worker"); do
      "${KIND_EXPERIMENTAL_PROVIDER}" network disconnect -f \
        "${DPU_SIM_UPLINK_NETWORK}" "${container}" >/dev/null 2>&1 || true
    done
    "${KIND_EXPERIMENTAL_PROVIDER}" network rm \
      "${DPU_SIM_UPLINK_NETWORK}" >/dev/null 2>&1 || true
  fi
  "${KIND_EXPERIMENTAL_PROVIDER}" network create \
    --subnet="${DPU_SIM_UPLINK_SUBNET}" \
    --driver bridge \
    "${DPU_SIM_UPLINK_NETWORK}"
  "${KIND_EXPERIMENTAL_PROVIDER}" network connect "${DPU_SIM_UPLINK_NETWORK}" frr

  local ordinal=0
  local host_node dpu_node host_pid dpu_pid host_mac dpu_mac host_ip dpu_ip
  local dpu_iface tmp_host tmp_dpu
  for host_node in $(kubectl_host get nodes -l k8s.ovn.org/dpu-host \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
    dpu_node=$(replace_host_with_dpu_node "${host_node}")
    "${KIND_EXPERIMENTAL_PROVIDER}" network connect \
      "${DPU_SIM_UPLINK_NETWORK}" "${dpu_node}"

    host_pid=$("${KIND_EXPERIMENTAL_PROVIDER}" inspect -f '{{.State.Pid}}' "${host_node}")
    dpu_pid=$("${KIND_EXPERIMENTAL_PROVIDER}" inspect -f '{{.State.Pid}}' "${dpu_node}")
    host_mac=$(simulated_dpu_mac "${host_node}" host "${DPU_SIM_UPLINK_INDEX}")
    dpu_mac=$(simulated_dpu_mac "${host_node}" dpu "${DPU_SIM_UPLINK_INDEX}")
    host_ip="${subnet_base}.$((254 - ordinal))"
    dpu_ip=$(docker_network_ip "${dpu_node}" "${DPU_SIM_UPLINK_NETWORK}")
    dpu_iface=$(container_iface_for_ip "${dpu_node}" "${dpu_ip}")
    tmp_host="uh${ordinal}${DPU_SIM_UPLINK_INDEX}"
    tmp_dpu="ud${ordinal}${DPU_SIM_UPLINK_INDEX}"

    run_root nsenter -t "${host_pid}" -n ip link del \
      "${DPU_SIM_UPLINK_HOST_INTERFACE}" >/dev/null 2>&1 || true
    run_root nsenter -t "${dpu_pid}" -n ip link del \
      "${DPU_SIM_UPLINK_DPU_REPRESENTOR}" >/dev/null 2>&1 || true
    run_root ip link del "${tmp_host}" >/dev/null 2>&1 || true
    run_root ip link add "${tmp_host}" type veth peer name "${tmp_dpu}"
    run_root ip link set "${tmp_host}" netns "${host_pid}"
    run_root ip link set "${tmp_dpu}" netns "${dpu_pid}"

    run_root nsenter -t "${host_pid}" -n ip link set "${tmp_host}" \
      name "${DPU_SIM_UPLINK_HOST_INTERFACE}"
    run_root nsenter -t "${host_pid}" -n ip link set \
      "${DPU_SIM_UPLINK_HOST_INTERFACE}" address "${host_mac}"
    run_root nsenter -t "${host_pid}" -n ip addr replace \
      "${host_ip}/${prefix}" dev "${DPU_SIM_UPLINK_HOST_INTERFACE}"
    run_root nsenter -t "${host_pid}" -n ip link set \
      "${DPU_SIM_UPLINK_HOST_INTERFACE}" up

    run_root nsenter -t "${dpu_pid}" -n ip link set "${tmp_dpu}" \
      name "${DPU_SIM_UPLINK_DPU_REPRESENTOR}"
    run_root nsenter -t "${dpu_pid}" -n ip link set \
      "${DPU_SIM_UPLINK_DPU_REPRESENTOR}" address "${dpu_mac}"
    run_root nsenter -t "${dpu_pid}" -n ip link set \
      "${DPU_SIM_UPLINK_DPU_REPRESENTOR}" up

    "${KIND_EXPERIMENTAL_PROVIDER}" exec "${dpu_node}" sh -c "
set -e
ovs-vsctl --if-exists del-br ${DPU_SIM_UPLINK_BRIDGE}
ovs-vsctl --may-exist add-br ${DPU_SIM_UPLINK_BRIDGE}
ip addr flush dev ${dpu_iface}
ovs-vsctl --may-exist add-port ${DPU_SIM_UPLINK_BRIDGE} ${dpu_iface}
ovs-vsctl --may-exist add-port ${DPU_SIM_UPLINK_BRIDGE} ${DPU_SIM_UPLINK_DPU_REPRESENTOR}
ovs-vsctl br-set-external-id ${DPU_SIM_UPLINK_BRIDGE} bridge-uplink ${dpu_iface}
ip link set ${dpu_iface} up
ip link set ${DPU_SIM_UPLINK_BRIDGE} up
ip addr replace ${dpu_ip}/${prefix} dev ${DPU_SIM_UPLINK_BRIDGE}
ip route replace ${DPU_SIM_UPLINK_SUBNET} dev ${DPU_SIM_UPLINK_BRIDGE} src ${dpu_ip}
"

    configure_external_frr_uplink_peer "${dpu_ip}"
    ordinal=$((ordinal + 1))
  done

  echo "DPU Uplink e2e environment:"
  echo "  OVN_TEST_DPU_UPLINK_NETWORK=${DPU_SIM_UPLINK_NETWORK}"
  echo "  OVN_TEST_DPU_UPLINK_HOST_INTERFACE=${DPU_SIM_UPLINK_HOST_INTERFACE}"
  echo "  OVN_TEST_DPU_UPLINK_EXPECTED_BRIDGE=${DPU_SIM_UPLINK_BRIDGE}"
}

if [ ! -x "${DPU_SIM_PATH}/bin/dpu-sim" ]; then
  echo "error: ${DPU_SIM_PATH}/bin/dpu-sim does not exist or is not executable" >&2
  echo "run 'make build' in the dpu-simulator repository first" >&2
  exit 1
fi

echo "Using KIND_EXPERIMENTAL_PROVIDER=${KIND_EXPERIMENTAL_PROVIDER}"
echo "Using KIND_HELM_OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT}"
echo "Using BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4}"

cleanup_bgp_artifacts "${KIND_EXPERIMENTAL_PROVIDER}"

pushd "${DPU_SIM_PATH}"
./bin/dpu-sim \
  --config "${DPU_SIM_CONFIG}" \
  --ovn-kubernetes-path "${OVN_KUBERNETES_PATH}" \
  --ovnk-mode values-only

install_ovnk_host

./bin/dpu-sim ovnk host-access \
  --config "${DPU_SIM_CONFIG}" \
  --cluster "${HOST_CLUSTER}"
./bin/dpu-sim ovnk values \
  --config "${DPU_SIM_CONFIG}" \
  --cluster "${DPU_CLUSTER}" \
  --require-host-credentials

install_ovnk_dpu
wait_for_ovn
configure_dpu_sim_uplink_bridge
popd
