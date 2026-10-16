#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Staged OVN-Kubernetes upgrade for the kind cluster created by
# contrib/kind-helm.sh, modelled on how production GitOps upgrades work: a new
# chart version and image tag are handed to helm, and every component rolls
# according to its own update strategy. No pods are scaled down or deleted by
# hand, and nothing is re-provisioned. The only kind-specific step is loading
# the image into the nodes, which stands in for pulling it from a registry.
#
# The supported upgrade order is nodes first, then the control plane (see
# docs/design/upgrades.md). ovnkube-identity belongs to the node stage: it is a
# DaemonSet on the control-plane nodes and rolls together with ovnkube-node.
# Two things are needed to test the half-upgraded cluster that order produces:
#  - hold back the control plane: `kubectl rollout pause` on the Deployment.
#    helm updates the pod template, the Deployment controller does not act on
#    it until `rollout resume`.
#  - hold back one node: before the upgrade, load the OLD image into that node
#    under the NEW image's tag. The DaemonSet rolls everywhere with the new
#    tag, but on that node the kubelet finds the tag already present
#    (imagePullPolicy IfNotPresent) and starts the old code. The next stage
#    loads the real new image into the node and recreates its pods. The
#    DaemonSet keeps its RollingUpdate strategy and its status is truthful, so
#    `rollout status` works unchanged; only the image ID differs per node.
#
# UPGRADE_STAGE:
#   all                 - single helm upgrade, everything rolls
#   nodes-first         - ovnkube-identity and every node but one roll to the
#                         new image, one node and the control plane stay on
#                         the old image
#   control-plane-last  - roll the held-back node, then resume the control plane
#
# Run e2e between the two stages to exercise both skews the order produces,
# new nodes against an old control plane and new nodes against an old node,
# and again after the second stage.
#
# Env:
#   OVN_IMAGE           image to upgrade to (default ovn-daemonset-fedora:pr)
#   KIND_CLUSTER_NAME   kind cluster to load the image into (default ovn)
#   HELM_RELEASE        helm release name used at install (default ovn-kubernetes)
#   HOLDBACK_NODE       node kept on the old image in nodes-first (default: the
#                       alphabetically last worker; empty to hold none back).
#                       Must be the same value in both stages.
#   PIN_OVS_NODE        keep ovs-node on OnDelete so OVS does not restart under a
#                       running ovnkube-node (default true, see below)

set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}
OVN_IMAGE_FAMILY=${OVN_IMAGE_FAMILY:-fedora}
OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-${OVN_IMAGE_FAMILY}:pr}
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
HELM_RELEASE=${HELM_RELEASE:-ovn-kubernetes}
PIN_OVS_NODE=${PIN_OVS_NODE:-true}
NODE_WAIT_TIMEOUT=${NODE_WAIT_TIMEOUT:-600}

NAMESPACE=ovn-kubernetes
CONTROL_PLANE_DEPLOYMENT=ovnkube-control-plane
NODE_DAEMONSETS="ovnkube-node ovnkube-single-node-zone"
# Rolls with the nodes but only runs on control-plane nodes, so it takes no
# part in holding a worker back.
IDENTITY_DAEMONSET=ovnkube-identity

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
CHART_DIR="${SCRIPT_DIR}/../../helm/ovn-kubernetes"

log() { echo ">>> $*"; }

exists() { kubectl -n "$NAMESPACE" get "$1" "$2" >/dev/null 2>&1; }

# Selector of a workload as a kubectl -l argument.
selector_of() {
  kubectl -n "$NAMESPACE" get "$1" "$2" -o json \
    | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")'
}

# Unique set of images used by the running pods of a workload. An optional
# third argument is a field selector restricting the pods, e.g. by node.
# Terminating pods are ignored: `rollout status` returns once the new pods are
# available, while the replaced ones may still be shutting down, and a
# Deployment's old ReplicaSet pod would otherwise show up next to the new one.
running_images() {
  kubectl -n "$NAMESPACE" get pod -l "$(selector_of "$1" "$2")" ${3:+--field-selector "$3"} -o json \
    | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .spec.containers[].image' | sort -u
}

# Unique set of image IDs reported by the running containers of a workload,
# optionally restricted by a field selector. Two builds loaded under the same
# tag are told apart by ID, not by name.
running_image_ids() {
  kubectl -n "$NAMESPACE" get pod -l "$(selector_of "$1" "$2")" ${3:+--field-selector "$3"} -o json \
    | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .status.containerStatuses[]?.imageID' | sort -u
}

# Nodes of the kind cluster, one per line.
cluster_nodes() {
  kind get nodes --name "$KIND_CLUSTER_NAME"
}

# Load an image into the given nodes (comma separated), or all nodes when empty.
load_image() {
  local image=$1 nodes=${2:-}
  kind load docker-image "$image" --name "$KIND_CLUSTER_NAME" ${nodes:+--nodes "$nodes"}
}

# Host-side reference of the image the nodes currently run. The cluster may
# have been installed from an image the host knows under the same name, or
# under the name without the localhost/ prefix that kind-helm adds.
host_ref_of() {
  local ref
  for ref in "$1" "${1#localhost/}"; do
    if docker image inspect "$ref" >/dev/null 2>&1; then
      echo "$ref"
      return
    fi
  done
  echo "image ${1} running on the nodes is not present on the host, cannot hold a node back" >&2
  exit 1
}

# Load OLD_IMAGE into NODE under the tag of OVN_IMAGE, without touching the
# other nodes or the host's view of OVN_IMAGE afterwards.
load_old_image_as_new() {
  local old=$1 node=$2 real
  real=$(docker image inspect "$OVN_IMAGE" -f '{{.Id}}')
  docker tag "$old" "$OVN_IMAGE"
  load_image "$OVN_IMAGE" "$node"
  docker tag "$real" "$OVN_IMAGE"
}

# Worker kept on the old image during nodes-first. Defaults to the
# alphabetically last node without the control-plane role, so a multi-node
# cluster always exercises new nodes talking to an old one. Empty when the
# cluster has a single node or HOLDBACK_NODE is explicitly set to "".
holdback_node() {
  if [[ -n "${HOLDBACK_NODE+x}" ]]; then
    echo "$HOLDBACK_NODE"
    return
  fi
  kubectl get nodes -o json \
    | jq -r '[.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null) | .metadata.name] | sort | last // empty'
}

node_daemonsets() {
  for ds in $NODE_DAEMONSETS; do
    exists daemonset "$ds" && echo "$ds"
  done
}

node_running_images() {
  for ds in $(node_daemonsets); do running_images daemonset "$ds"; done | sort -u
}

wait_nodes() {
  for ds in $(node_daemonsets); do
    kubectl -n "$NAMESPACE" rollout status daemonset "$ds" --timeout="${NODE_WAIT_TIMEOUT}s"
  done
}

# Wait until every node DaemonSet has a Ready, non-terminating pod on NODE.
# `rollout status` is not enough after deleting a node's pods: the DaemonSet
# template is unchanged, so its status can still read complete from before
# the deletion, and a missing replacement pod is invisible to an image check
# that only looks at the pods that exist.
wait_node_pods() {
  local node=$1 ds deadline ready
  deadline=$((SECONDS + NODE_WAIT_TIMEOUT))
  for ds in $(node_daemonsets); do
    until ready=$(kubectl -n "$NAMESPACE" get pod -l "$(selector_of daemonset "$ds")" \
          --field-selector "spec.nodeName=${node}" -o json \
        | jq -r '[.items[] | select(.metadata.deletionTimestamp == null)
                  | select(.status.conditions[]? | select(.type == "Ready" and .status == "True"))] | length') \
        && [[ "$ready" == "1" ]]; do
      if (( SECONDS >= deadline )); then
        echo "${ds}: no Ready pod on ${node} after ${NODE_WAIT_TIMEOUT}s" >&2
        kubectl -n "$NAMESPACE" get pod -l "$(selector_of daemonset "$ds")" -o wide >&2 || true
        exit 1
      fi
      sleep 5
    done
  done
}

wait_identity() {
  exists daemonset "$IDENTITY_DAEMONSET" || return 0
  kubectl -n "$NAMESPACE" rollout status daemonset "$IDENTITY_DAEMONSET" --timeout="${NODE_WAIT_TIMEOUT}s"
}

expect_identity_image() {
  exists daemonset "$IDENTITY_DAEMONSET" || return 0
  expect_images "ovnkube-identity pods" "$(running_images daemonset "$IDENTITY_DAEMONSET")" "$1"
}

wait_control_plane() {
  exists deployment "$CONTROL_PLANE_DEPLOYMENT" || return 0
  kubectl -n "$NAMESPACE" rollout status deployment "$CONTROL_PLANE_DEPLOYMENT" --timeout=300s
}

expect_images() {
  # expect_images <what> <actual> <expected>
  if [[ "$2" != "$3" ]]; then
    echo "$1 run image(s) [$2], expected [$3]" >&2
    exit 1
  fi
}

# The upgrade itself: make the image available to the nodes and hand the new
# chart and tag to helm. --reset-then-reuse-values takes the new chart's
# defaults and layers the values the release was installed with on top, the
# same way GitOps keeps pointing at the same values files while bumping the
# chart version.
# helm_upgrade [nodes]: load the image into the given nodes (default all) and
# run the chart upgrade.
helm_upgrade() {
  local nodes=${1:-}
  log "loading ${OVN_IMAGE} into kind cluster ${KIND_CLUSTER_NAME}${nodes:+ nodes ${nodes}}"
  load_image "$OVN_IMAGE" "$nodes"

  local extra=()
  if [[ "$PIN_OVS_NODE" == true ]]; then
    # The chart has one image tag for every DaemonSet, so ovs-node would roll
    # together with ovnkube-node. Restarting OVS under a running ovnkube-node
    # removes /var/run/openvswitch/db.sock and the breth0 port state; the old
    # ovnkube-node crashes and the new one cannot become Ready against the
    # torn-down OVS, so the rollout stalls. OVS is upgraded separately.
    extra+=(--set ovs-node.updateStrategy=OnDelete)
  fi

  log "helm upgrade ${HELM_RELEASE} to ${OVN_IMAGE}"
  helm upgrade "$HELM_RELEASE" "$CHART_DIR" --reset-then-reuse-values \
    --set "global.image.repository=${OVN_IMAGE%:*}" \
    --set "global.image.tag=${OVN_IMAGE##*:}" \
    ${extra[@]+"${extra[@]}"}
}

stage_all() {
  helm_upgrade
  wait_control_plane
  wait_identity
  wait_nodes
  expect_identity_image "$OVN_IMAGE"
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
  if exists deployment "$CONTROL_PLANE_DEPLOYMENT"; then
    expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$OVN_IMAGE"
  fi
}

stage_nodes_first() {
  local old_cp="" old_nodes holdback others
  old_nodes=$(node_running_images)
  holdback=$(holdback_node)
  if exists deployment "$CONTROL_PLANE_DEPLOYMENT"; then
    old_cp=$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")
    kubectl -n "$NAMESPACE" rollout pause deployment "$CONTROL_PLANE_DEPLOYMENT"
  fi
  if [[ -n "$holdback" ]]; then
    log "holding node ${holdback} back: loading ${old_nodes} there as ${OVN_IMAGE}"
    load_old_image_as_new "$(host_ref_of "$old_nodes")" "$holdback"
    others=$(cluster_nodes | grep -v -x "$holdback" | paste -sd,)
    helm_upgrade "$others"
  else
    helm_upgrade
  fi
  wait_identity
  wait_nodes
  expect_identity_image "$OVN_IMAGE"
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
  if [[ -n "$holdback" ]]; then
    local ds on off
    for ds in $(node_daemonsets); do
      on=$(running_image_ids daemonset "$ds" "spec.nodeName=${holdback}")
      off=$(running_image_ids daemonset "$ds" "spec.nodeName!=${holdback}")
      if [[ -z "$on" || -z "$off" || "$on" == "$off" || $(wc -l <<<"$off") -ne 1 ]]; then
        echo "${ds}: expected ${holdback} to run a different image ID than the other nodes; on=[${on}] off=[${off}]" >&2
        exit 1
      fi
    done
    log "nodes on ${OVN_IMAGE} except ${holdback}, which runs ${old_nodes} under that tag"
  else
    log "all nodes on ${OVN_IMAGE}"
  fi
  if [[ -n "$old_cp" ]]; then
    expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$old_cp"
    log "control plane still on ${old_cp}"
  fi
}

stage_control_plane_last() {
  local holdback ds ids
  holdback=$(holdback_node)
  if [[ -n "$holdback" ]]; then
    # Finish the node roll first: the control plane may assume every node runs
    # the new release when it starts. Load the real image over the stand-in
    # tag and recreate the node pods there; the DaemonSet template is
    # unchanged, so only a pod deletion makes the kubelet pick the new image.
    log "loading the real ${OVN_IMAGE} into ${holdback}"
    load_image "$OVN_IMAGE" "$holdback"
    for ds in $(node_daemonsets); do
      kubectl -n "$NAMESPACE" delete pod -l "$(selector_of daemonset "$ds")" \
        --field-selector "spec.nodeName=${holdback}" --timeout="${NODE_WAIT_TIMEOUT}s"
    done
    wait_node_pods "$holdback"
    wait_nodes
    for ds in $(node_daemonsets); do
      ids=$(running_image_ids daemonset "$ds")
      if [[ $(wc -l <<<"$ids") -ne 1 ]]; then
        echo "${ds}: expected one image ID on every node after rolling ${holdback}, got [${ids}]" >&2
        exit 1
      fi
    done
  fi
  expect_identity_image "$OVN_IMAGE"
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
  exists deployment "$CONTROL_PLANE_DEPLOYMENT" || { log "no ${CONTROL_PLANE_DEPLOYMENT}, nodes done"; return; }
  kubectl -n "$NAMESPACE" rollout resume deployment "$CONTROL_PLANE_DEPLOYMENT"
  wait_control_plane
  expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$OVN_IMAGE"
}

UPGRADE_STAGE=${1:-${UPGRADE_STAGE:-}}
case "$UPGRADE_STAGE" in
  all)                stage_all ;;
  nodes-first)        stage_nodes_first ;;
  control-plane-last) stage_control_plane_last ;;
  "")
    echo "usage: $0 <all|nodes-first|control-plane-last>" >&2
    exit 2 ;;
  *)
    echo "unknown UPGRADE_STAGE '${UPGRADE_STAGE}': all, nodes-first, control-plane-last" >&2
    exit 2 ;;
esac
