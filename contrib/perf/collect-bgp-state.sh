#!/bin/sh
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Capture the BGP quantities that cannot reasonably be Prometheus series, and
# write them next to the kube-burner output for the run.
#
# Invoked from a kube-burner job hook. Never fails the run: a missing CRD or an
# unreachable FRR container is reported as null rather than a non-zero exit, so
# a reporting gap cannot turn a passing benchmark into a red lane.
#
# POSIX sh, not bash, and no pipefail. kube-burner does not execute the file:
# util.RunShellCmd reads it and pipes the contents to "/bin/sh -s -", so the
# shebang is ignored and on Ubuntu runners /bin/sh is dash. A bashism here
# fails the whole run rather than just the hook.
#
# Usage: collect-bgp-state.sh <label>

set -u

LABEL="${1:-unlabelled}"
OUT_DIR="${BGP_STATE_DIR:-.}"
OUT="${OUT_DIR}/bgp-state-${LABEL}.json"
FRR_CONTAINER="${FRR_CONTAINER_NAME:-frr}"
SETTLE_TIMEOUT="${BGP_STATE_SETTLE_TIMEOUT:-120}"
SETTLE_INTERVAL="${BGP_STATE_SETTLE_INTERVAL:-5}"

mkdir -p "${OUT_DIR}"

# FRRNodeState.status.runningConfig holds the whole rendered FRR configuration
# as one string, per node. It grows with VRF and neighbour count and the
# default etcd object size limit is 1.5MB, so its size is the measurement that
# bounds how many advertised networks a cluster can carry.
frrnodestate_sizes() {
  kubectl get frrnodestates -o json 2>/dev/null \
    | jq -c '[.items[] | {node: .metadata.name,
                          bytes: (.status.runningConfig // "" | length),
                          reload: .status.lastReloadResult}]' 2>/dev/null \
    || echo 'null'
}

# generated = RAs x nodes x matching source FRRConfigurations
generated_frrconfigs() {
  kubectl get frrconfigurations -A -l k8s.ovn.org/route-advertisements -o json 2>/dev/null \
    | jq -c '{count: (.items | length),
              total_bytes: ([.items[] | tojson | length] | add // 0),
              max_bytes:   ([.items[] | tojson | length] | max // 0)}' 2>/dev/null \
    || echo 'null'
}

# one per (node, peer, VRF); the cardinality is multiplicative.
#
# Broken down by status and by peer because the first two CI runs both showed
# twice as many objects as nodes with only half established, while the peer
# itself reported every session up and no failures. The counts alone cannot
# say whether that is a second configured neighbour that never comes up, a
# per-address-family artifact, or stale objects, so record enough to tell.
bgpsessionstates() {
  kubectl get bgpsessionstates -A -o json 2>/dev/null \
    | jq -c '{count: (.items | length),
              established: ([.items[] | select(.status.bgpStatus == "Established")] | length),
              byStatus: ([.items[] | .status.bgpStatus // "unknown"] | group_by(.)
                         | map({key: .[0], value: length}) | from_entries),
              byPeer:   ([.items[] | .metadata.labels["frrk8s.metallb.io/peer"] // "none"] | group_by(.)
                         | map({key: .[0], value: length}) | from_entries),
              byVRF:    ([.items[] | .metadata.labels["frrk8s.metallb.io/vrf"] // "none"] | group_by(.)
                         | map({key: (.[0] | if . == "" then "default" else . end), value: length}) | from_entries),
              nodes:    ([.items[] | .metadata.labels["frrk8s.metallb.io/node"]] | unique | length)}' 2>/dev/null \
    || echo 'null'
}

# How many neighbours the source configurations actually ask each node to peer
# with. If this is 2 then the half-established session count is the harness
# configuring a second neighbour, not an ovn-kubernetes scale property.
source_neighbors() {
  kubectl get frrconfigurations -A -o json 2>/dev/null \
    | jq -c '[.items[] | select(.metadata.labels["k8s.ovn.org/route-advertisements"] == null)
              | {name: .metadata.name,
                 routers: [.spec.bgp.routers[]? | {vrf: (.vrf // "default"),
                                                   neighbors: [.neighbors[]?.address]}]}]' 2>/dev/null \
    || echo 'null'
}

routeadvertisements() {
  kubectl get routeadvertisements -o json 2>/dev/null \
    | jq -c '{count: (.items | length),
              accepted: ([.items[] | select(.status.conditions[]? | select(.type=="Accepted" and .status=="True"))] | length)}' 2>/dev/null \
    || echo 'null'
}

# Nothing inside the cluster observes the peer's table. Reduced to the counts
# that matter: the full vtysh output is per-peer and would dominate the
# artifact once the ladder reaches hundreds of sessions.
peer_bgp_summary() {
  if command -v docker >/dev/null 2>&1 && docker inspect "${FRR_CONTAINER}" >/dev/null 2>&1; then
    docker exec "${FRR_CONTAINER}" vtysh -c "show bgp summary json" 2>/dev/null \
      | jq -c '{ribCount: .ipv4Unicast.ribCount,
                peerCount: .ipv4Unicast.peerCount,
                failedPeers: .ipv4Unicast.failedPeers,
                prefixesReceivedFromNodes: ([.ipv4Unicast.peers[]?.pfxRcd] | add // 0),
                prefixesSentToNodes: ([.ipv4Unicast.peers[]?.pfxSnt] | add // 0)}' 2>/dev/null \
      || echo 'null'
  else
    echo 'null'
  fi
}

# kube-burner runs beforeCleanup straight after object creation and before
# jobPause, so without this the snapshot is taken mid-convergence. The first CI
# run proved it: the peer had received 54 of 120 advertised prefixes and
# runningConfig was a fifth of its settled size.
#
# Settle on the two quantities that move last and need nothing but kubectl: the
# generated FRRConfiguration count and the total runningConfig size. Both have
# to hold still for two consecutive polls. A timeout only means the snapshot is
# taken anyway, matching this script's contract of never failing the run.
settle_signature() {
  printf '%s/%s' \
    "$(kubectl get frrconfigurations -A -l k8s.ovn.org/route-advertisements \
         -o json 2>/dev/null | jq -r '.items | length' 2>/dev/null || echo x)" \
    "$(kubectl get frrnodestates -o json 2>/dev/null \
         | jq -r '[.items[].status.runningConfig // "" | length] | add // 0' 2>/dev/null || echo x)"
}

wait_for_settle() {
  elapsed=0
  previous=""
  stable=0
  while [ "${elapsed}" -lt "${SETTLE_TIMEOUT}" ]; do
    current="$(settle_signature)"
    if [ "${current}" = "${previous}" ]; then
      stable=$((stable + 1))
      if [ "${stable}" -ge 2 ]; then
        echo "settled after ${elapsed}s at ${current}"
        return 0
      fi
    else
      stable=0
    fi
    previous="${current}"
    sleep "${SETTLE_INTERVAL}"
    elapsed=$((elapsed + SETTLE_INTERVAL))
  done
  echo "did not settle within ${SETTLE_TIMEOUT}s, snapshotting anyway at ${previous}"
  return 0
}

wait_for_settle

jq -n \
  --arg label "${LABEL}" \
  --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson frrnodestates "$(frrnodestate_sizes)" \
  --argjson generated "$(generated_frrconfigs)" \
  --argjson sessions "$(bgpsessionstates)" \
  --argjson ras "$(routeadvertisements)" \
  --argjson peer "$(peer_bgp_summary)" \
  --argjson srcneigh "$(source_neighbors)" \
  '{label: $label, timestamp: $ts,
    routeAdvertisements: $ras,
    generatedFRRConfigurations: $generated,
    frrNodeStates: $frrnodestates,
    bgpSessionStates: $sessions,
    sourceFRRConfigurations: $srcneigh,
    peerBGPSummary: $peer}' \
  > "${OUT}" 2>/dev/null || echo '{}' > "${OUT}"

echo "wrote ${OUT}"
exit 0
