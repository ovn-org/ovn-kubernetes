#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# frr-k8s helpers for the kind clusters: clone the pinned frr-k8s release and
# install, wait for and configure it in the cluster.
#
# This file is sourced by kind-common.sh for the kind.sh/kind-helm.sh flows,
# and is also a standalone entrypoint for downstream CI test frameworks that manage
# the cluster themselves:
#
#   setup-frr-k8s.sh install-crds        install only the frr-k8s CRDs
#   setup-frr-k8s.sh install [bgp_port]  install the pinned frr-k8s release
#   setup-frr-k8s.sh wait                wait for frr-k8s to be ready
#   setup-frr-k8s.sh configure           apply the BGP receive config and the
#                                        return routes for advertised networks
#
# Standalone invocations operate on an existing cluster (KUBECONFIG must point
# at it) and honor the same environment knobs as the kind flows, notably:
# KIND_CLUSTER_NAME, PLATFORM_IPV4_SUPPORT, PLATFORM_IPV6_SUPPORT,
# BGP_SERVER_NET_SUBNET_IPV4/IPV6, ADVERTISE_DEFAULT_NETWORK,
# FRR_K8S_FRR_IMAGE and FRR_TMP_DIR. Export the same FRR_TMP_DIR to this
# script and to setup-bgp-peer.sh to share one frr-k8s checkout across
# separate invocations.

# Include guard: kind-common.sh sources both setup scripts, setup-bgp-peer.sh
# sources setup-frr-k8s.sh, and standalone invocations source kind-common.sh
# back, so every file in the cycle is reached more than once. Sourcing this
# file a second time would fail on the readonly assignments below; return
# early instead and make re-sourcing a no-op.
if [ -n "${__SETUP_FRR_K8S_SH_SOURCED:-}" ]; then
  return 0
fi
__SETUP_FRR_K8S_SH_SOURCED=1

SETUP_FRR_K8S_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# DPU-sim remote mode is optional; consumers that want it source
# kind-dpu-sim-lib.sh, which overrides this stub.
declare -F frr_k8s_remote_enabled >/dev/null || frr_k8s_remote_enabled() { false; }

readonly FRR_K8S_VERSION=v0.0.0-20260603082256-b43efcb206be
readonly FRR_K8S_GIT_REF=b43efcb206be
readonly FRR_K8S_OVNK_BGP_PATCH="${SETUP_FRR_K8S_DIR}/frr-k8s/patches/0001-Improvements-to-the-demo.patch"
readonly FRR_K8S_UPSTREAM_FRR_IMAGE=quay.io/frrouting/frr:10.4.3
readonly FRR_DEPLOYED_IMAGE=quay.io/frrouting/frr:10.6.2
# Override to test newer FRR builds in the in-cluster frr-k8s daemonset
# without changing the pinned frr-k8s release.
FRR_K8S_FRR_IMAGE=${FRR_K8S_FRR_IMAGE:-${FRR_DEPLOYED_IMAGE}}
# Override to use a mirrored FRR image for the external demo container.
FRR_EXTERNAL_DEMO_IMAGE=${FRR_EXTERNAL_DEMO_IMAGE:-${FRR_DEPLOYED_IMAGE}}
readonly FRR_EXTERNAL_DEMO_IMAGE
# A caller-provided FRR_TMP_DIR is shared across invocations (e.g. deploy the
# external peer in one process and configure frr-k8s in another) and left for
# the caller to clean up; otherwise use a private one removed on exit.
if [ -n "${FRR_TMP_DIR:-}" ]; then
  FRR_TMP_DIR_CALLER_OWNED=true
else
  FRR_TMP_DIR=$(mktemp -d)
  FRR_TMP_DIR_CALLER_OWNED=false
fi
readonly FRR_TMP_DIR FRR_TMP_DIR_CALLER_OWNED

clone_frr_k8s() {
  # A caller-owned FRR_TMP_DIR may exist but be empty, so check for the clone itself.
  [ -d "$FRR_TMP_DIR/frr-k8s" ] || {
    mkdir -p "$FRR_TMP_DIR"
    [ "$FRR_TMP_DIR_CALLER_OWNED" == true ] || trap 'rm -rf $FRR_TMP_DIR' EXIT
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
      "${FRR_EXTERNAL_DEMO_IMAGE}"

    popd

    popd || exit 1
  }
}

install_frr_k8s_crds() {
  echo "Installing frr-k8s CRDs ..."
  clone_frr_k8s
  kubectl apply -f "${FRR_TMP_DIR}"/frr-k8s/config/crd/bases/
}

install_frr_k8s() {
  local bgp_port=${1:-0}
  if [[ ! "$bgp_port" =~ ^[0-9]+$ ]] || (( 10#$bgp_port > 65535 )); then
    echo "invalid BGP port: ${bgp_port}; expected 0 or 1-65535" >&2
    return 2
  fi
  echo "Installing frr-k8s ..."
  clone_frr_k8s

  # apply frr-k8s

  # The all-in-one manifest is only consumed here (deploy_frr_external_container
  # uses CRDs and the demo scripts, not this manifest), so the fix lives here
  # rather than in clone_frr_k8s.
  #
  # gcr.io/kubebuilder/kube-rbac-proxy is unavailable after Google's Container
  # Registry shutdown (https://cloud.google.com/container-registry/docs/deprecations/container-registry-deprecation).
  # Upstream bug: https://github.com/metallb/metallb/issues/2619
  # Use the same image from the Kubernetes community registry instead.
  # REVERT ME: when https://github.com/metallb/metallb/issues/2619 is fixed
  sed -i 's|gcr.io/kubebuilder/kube-rbac-proxy|registry.k8s.io/kubebuilder/kube-rbac-proxy|g' \
    "${FRR_TMP_DIR}"/frr-k8s/config/all-in-one/frr-k8s.yaml

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
kind export kubeconfig --name ${KIND_CLUSTER_NAME}
while true; do
CLUSTER_IP=\$(kubectl get svc -n frr-k8s-system frr-k8s-webhook-service -o jsonpath='{.spec.clusterIP}')
# Wrap IPv6 addresses in brackets for URL syntax
[[ \${CLUSTER_IP} =~ : ]] && CLUSTER_IP="[\${CLUSTER_IP}]"
$OCI_BIN exec ${KIND_CLUSTER_NAME}-control-plane curl -ksS --connect-timeout 0.1 https://\${CLUSTER_IP}
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
  local NODES node node_ips subnet_json node_ipv4 ipv4_subnet node_ipv6 ipv6_subnet
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

  [ "$FRR_TMP_DIR_CALLER_OWNED" == true ] || rm -rf "${FRR_TMP_DIR}"
}

configure_frr_k8s() {
  clone_frr_k8s
  apply_frr_k8s_receive_config
  configure_frr_k8s_routes
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  set -eo pipefail
  # Operate on an existing cluster; kind-common's defaults would otherwise
  # scrub KUBECONFIG, and route advertisements imply multi-network there.
  KIND_CREATE=${KIND_CREATE:-false}
  ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-true}
  DIR="${SETUP_FRR_K8S_DIR}"
  source "${DIR}/kind-common.sh"
  source "${DIR}/kind-dpu-sim-lib.sh"
  set_common_default_params

  case "${1:-}" in
    install-crds) install_frr_k8s_crds ;;
    install)      install_frr_k8s "${2:-0}" ;;
    wait)         wait_for_frr_k8s ;;
    configure)    configure_frr_k8s ;;
    *)
      echo "Usage: $0 install-crds | install [bgp_port] | wait | configure" >&2
      exit 2
      ;;
  esac
fi
