#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -ex

export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}
export OVN_IMAGE_FAMILY=${OVN_IMAGE_FAMILY:-fedora}
export OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-${OVN_IMAGE_FAMILY}:pr}
export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

# Stash current replica counts and scale the controller Deployments to 0 so
# their old pods exit before helm re-creates them with the new image.
# DaemonSets are left to roll via their RollingUpdate strategy.
declare -A SAVED_REPLICAS
for d in ovnkube-control-plane; do
  if ! kubectl -n ovn-kubernetes get deployment "$d" >/dev/null 2>&1; then
    continue
  fi
  SAVED_REPLICAS[$d]=$(kubectl -n ovn-kubernetes get deployment "$d" -o=jsonpath='{.spec.replicas}')
  kubectl -n ovn-kubernetes scale deployment "$d" --replicas=0
done

# Let the downscaled pods terminate before helm upgrade re-renders the
# Deployment spec. `.status.replicas` is elided (not 0) once the Deployment
# reaches zero, so waiting on that jsonpath never matches. Wait for the pods
# themselves to be deleted using each Deployment's own selector.
for d in "${!SAVED_REPLICAS[@]}"; do
  selector=$(kubectl -n ovn-kubernetes get deployment "$d" -o json \
    | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' 2>/dev/null) || selector=""
  if [[ -n "$selector" ]]; then
    kubectl -n ovn-kubernetes wait pod -l "$selector" \
      --for=delete --timeout=120s || true
  fi
done

# Pin ovs-node's DaemonSet updateStrategy to OnDelete through the helm
# upgrade. helm rewrites every DS pod template with the new global.image.tag
# (the chart has a single image setting shared by every subchart), which
# otherwise rolls ovs-node concurrently with ovnkube-node. When the ovs
# container on a node restarts, /var/run/openvswitch/db.sock vanishes; the
# still-running old ovnkube-node on that node loses its ovsdb connection,
# sees the eth0-on-breth0 binding disappear, and crashes with "phys port eth0
# ofport changed from 1 to". The DS rollout then stalls because the first new
# ovnkube-node pod can't reach Ready either (same torn-down OVS state).
#
# kind-helm.sh passes OVS_NODE_UPDATE_STRATEGY through as
# `--set ovs-node.updateStrategy=<value>`, so the chart renders the DS with
# OnDelete and helm's reconcile doesn't revert it. OVS keeps running on the
# existing pods; ovnkube-node rolls against live ovsdb state.
export OVS_NODE_UPDATE_STRATEGY=OnDelete

# Run the helm upgrade. contrib/kind-helm.sh --deploy loads the PR image into
# KIND and runs `helm upgrade --install ovn-kubernetes` with current workflow
# env vars (OVN_GATEWAY_MODE, PLATFORM_IPV{4,6}_SUPPORT, ...).
# Chart is re-rendered from the PR branch,
# so chart/value changes land too. Scaled-down Deployments come back up at
# their chart replica count with the new image.
"${SCRIPT_DIR}/../../contrib/kind-helm.sh" --deploy

# Belt-and-braces: if the chart's replicas differ from what was running
# before (or if helm already restored them), make sure they match the
# pre-upgrade count so subsequent e2e doesn't see an unexpected topology.
for d in "${!SAVED_REPLICAS[@]}"; do
  desired=${SAVED_REPLICAS[$d]}
  current=$(kubectl -n ovn-kubernetes get deployment "$d" -o=jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  if [[ -n "$desired" && "$current" != "$desired" ]]; then
    kubectl -n ovn-kubernetes scale deployment "$d" --replicas="$desired"
  fi
  kubectl -n ovn-kubernetes rollout status deployment "$d" --timeout=300s
done

# Verify DaemonSet rollout finished too.
for ds in ovnkube-node ovnkube-single-node-zone; do
  if kubectl -n ovn-kubernetes get daemonset "$ds" >/dev/null 2>&1; then
    kubectl -n ovn-kubernetes rollout status daemonset "$ds" --timeout=600s
  fi
done

# Remove the control-plane taint so e2e shard-conformance workloads can
# schedule on the control-plane node (unchanged from the original script;
# kind-helm.sh's fresh-install path does the same).
KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
if [ "$KIND_REMOVE_TAINT" == true ]; then
  for node in $(kubectl get nodes -l node-role.kubernetes.io/control-plane -o name); do
    kubectl taint node "$node" node-role.kubernetes.io/control-plane:NoSchedule- || true
  done
fi

# Refresh the e2e test binary if the disk copy is stale.
ARCH=""
case $(uname -m) in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
esac
K8S_VERSION="v1.37.0"
E2E_VERSION=$(/usr/local/bin/e2e.test --version)
if [[ "$E2E_VERSION" != "$K8S_VERSION" ]]; then
  echo "found version $E2E_VERSION of e2e binary, need version $K8S_VERSION; downloading"
  curl -LO https://dl.k8s.io/${K8S_VERSION}/kubernetes-test-linux-${ARCH}.tar.gz
  tar xvzf kubernetes-test-linux-${ARCH}.tar.gz
  sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
  sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
  rm kubernetes-test-linux-${ARCH}.tar.gz
fi
