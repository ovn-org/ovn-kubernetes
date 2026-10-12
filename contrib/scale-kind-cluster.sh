#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -eEo pipefail

# OVN-Kubernetes KIND Cluster Auto-Scaler
# Automatically adds worker nodes to existing KIND cluster with all necessary fixes

# Error handler
trap 'echo "Error on line $LINENO. Exit code: $?"' ERR

usage() {
    echo "Usage: $0 -n <num_nodes> [-k <kubeconfig>] [-p]"
    echo ""
    echo "Options:"
    echo "  -n    Number of new worker nodes to add (required)"
    echo "  -k    Path to kubeconfig file (optional, defaults to \$KUBECONFIG env var)"
    echo "  -p    Enable parallel mode (only valid if nodes >= 2)"
    echo "  -h    Show this help message"
    echo ""
    echo "Examples:"
    echo "  export KUBECONFIG=~/ovn.conf"
    echo "  $0 -n 2                    # Sequential mode (default)"
    echo "  $0 -n 5 -p                 # Parallel mode"
    echo "  $0 -n 3 -k ~/ovn.conf      # Explicit kubeconfig"
    echo "  $0 -n 10 -k ~/ovn.conf -p  # Parallel with explicit kubeconfig"
    exit 1
}

# Initialize variables
KUBECONFIG_FILE=""
NUM_NODES=""
PARALLEL_MODE=false
CONTAINER_RUNTIME=""

# Parse arguments
while getopts "k:n:ph" opt; do
    case $opt in
        k) KUBECONFIG_FILE="$OPTARG" ;;
        n) NUM_NODES="$OPTARG" ;;
        p) PARALLEL_MODE=true ;;
        h) usage ;;
        *) usage ;;
    esac
done

# Validate number of nodes
if [ -z "$NUM_NODES" ]; then
    echo "Error: -n <num_nodes> is required"
    echo ""
    usage
fi

if ! [[ "$NUM_NODES" =~ ^[0-9]+$ ]] || [ "$NUM_NODES" -lt 1 ]; then
    echo "Error: Number of nodes must be a positive integer"
    exit 1
fi

# Validate parallel mode
if [ "$PARALLEL_MODE" = true ] && [ "$NUM_NODES" -lt 2 ]; then
    echo "Error: Parallel mode (-p) requires at least 2 nodes"
    echo "  You requested: $NUM_NODES nodes"
    echo "  Minimum for parallel: 2 nodes"
    echo ""
    echo "Suggestion: Remove -p flag or increase node count to at least 2"
    exit 1
fi

# Handle kubeconfig
if [ -z "$KUBECONFIG_FILE" ]; then
    # Try to use KUBECONFIG environment variable
    if [ -z "$KUBECONFIG" ]; then
        echo "Error: No kubeconfig specified"
        echo ""
        echo "Please either:"
        echo "  1. Export KUBECONFIG environment variable:"
        echo "     export KUBECONFIG=~/ovn.conf"
        echo "  2. Or use -k flag:"
        echo "     $0 -n $NUM_NODES -k ~/ovn.conf"
        exit 1
    fi
    KUBECONFIG_FILE="$KUBECONFIG"
    echo "Using KUBECONFIG from environment: $KUBECONFIG_FILE"
else
    # Explicit -k flag provided
    if [ ! -f "$KUBECONFIG_FILE" ]; then
        echo "Error: Kubeconfig file not found: $KUBECONFIG_FILE"
        exit 1
    fi
    export KUBECONFIG="$KUBECONFIG_FILE"
fi

# Verify kubeconfig file exists
if [ ! -f "$KUBECONFIG_FILE" ]; then
    echo "Error: Kubeconfig file not found: $KUBECONFIG_FILE"
    exit 1
fi

echo "=================================================="
echo "OVN-Kubernetes KIND Cluster Auto-Scaler"
echo "=================================================="
echo "Kubeconfig: $KUBECONFIG_FILE"
echo "Nodes to add: $NUM_NODES"
echo ""

# Detect cluster name from current context
CURRENT_CONTEXT=$(kubectl config current-context 2>/dev/null || true)
if [ -z "$CURRENT_CONTEXT" ]; then
    echo "Error: Could not detect current context from kubeconfig"
    exit 1
fi
CLUSTER_FULL_NAME=$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"${CURRENT_CONTEXT}\")].context.cluster}" 2>/dev/null)
CLUSTER_NAME=$(echo "$CLUSTER_FULL_NAME" | sed 's/^kind-//')
if [ -z "$CLUSTER_NAME" ]; then
    echo "Error: Could not detect cluster name from current context"
    exit 1
fi
echo "Detected cluster: $CLUSTER_NAME"

# Get control plane container name
CONTROL_PLANE="${CLUSTER_NAME}-control-plane"

# Detect which container runtime the KIND cluster is using
echo "Detecting container runtime..."

# Check if control plane exists in docker
if command -v docker &> /dev/null; then
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTROL_PLANE}$"; then
        CONTAINER_RUNTIME="docker"
    fi
fi

# Check if control plane exists in podman (if not found in docker)
if [ -z "$CONTAINER_RUNTIME" ] && command -v podman &> /dev/null; then
    if podman ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTROL_PLANE}$"; then
        CONTAINER_RUNTIME="podman"
    fi
fi

# If still not found, error out
if [ -z "$CONTAINER_RUNTIME" ]; then
    echo "Error: Control plane container '$CONTROL_PLANE' not found in docker or podman"
    echo ""
    echo "Available runtimes:"
    command -v docker &> /dev/null && echo "  - docker (installed, but no cluster found)"
    command -v podman &> /dev/null && echo "  - podman (installed, but no cluster found)"
    echo ""
    echo "Please ensure:"
    echo "  1. KIND cluster is running"
    echo "  2. Kubeconfig points to the correct cluster"
    echo "  3. Container runtime (docker/podman) has the cluster containers"
    exit 1
fi

echo "Container runtime: $CONTAINER_RUNTIME"
echo ""

# Get highest existing worker number
EXISTING_WORKERS=$(kubectl get nodes --no-headers 2>/dev/null | awk '{print $1}' | grep -oE 'worker[0-9]+$' | grep -oE '[0-9]+$' | sort -n | tail -1 || true)
EXISTING_WORKERS=${EXISTING_WORKERS:-0}

# Handle special cases
if [ -z "$EXISTING_WORKERS" ] || [ "$EXISTING_WORKERS" = "0" ]; then
    # No numbered workers found, check if base "worker" (without number) exists
    if kubectl get nodes --no-headers | grep -q "^${CLUSTER_NAME}-worker "; then
        # Base worker exists (e.g., ovn-worker), next should be worker2 to match KIND convention
        EXISTING_WORKERS=1
        echo "Found base worker node (${CLUSTER_NAME}-worker), starting from worker2"
    else
        # No workers at all, start from worker1
        EXISTING_WORKERS=0
        echo "No worker nodes found, starting from worker1"
    fi
else
    echo "Highest worker number: $EXISTING_WORKERS"
fi
echo ""

# Get join command
echo "[1/7] Generating kubeadm join token..."
JOIN_COMMAND=$($CONTAINER_RUNTIME exec "$CONTROL_PLANE" kubeadm token create --print-join-command 2>/dev/null || true)
if [ -z "$JOIN_COMMAND" ]; then
    echo "Error: Failed to generate join command"
    exit 1
fi
echo " Join token generated"

# Get KIND node image
NODE_IMAGE=$($CONTAINER_RUNTIME inspect "$CONTROL_PLANE" --format '{{.Config.Image}}')
echo "Using KIND image: $NODE_IMAGE"

# Get cluster network
NETWORK=$($CONTAINER_RUNTIME inspect "$CONTROL_PLANE" --format '{{range .NetworkSettings.Networks}}{{.NetworkID}}{{end}}' | head -c 12)
echo "Cluster network: $NETWORK"
echo ""

# Get OVN image name
OVN_IMAGE=$(kubectl get ds -n ovn-kubernetes ovs-node -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
if [ -z "$OVN_IMAGE" ]; then
    echo "Warning: Could not detect OVN image, will attempt to use localhost/ovn-daemonset-fedora:dev"
    OVN_IMAGE="localhost/ovn-daemonset-fedora:dev"
fi
echo "OVN image: $OVN_IMAGE"
echo ""

# Pre-load OVN image to host (critical for parallel mode to avoid race conditions)
echo "Checking if OVN image is available on $CONTAINER_RUNTIME host..."
if $CONTAINER_RUNTIME image inspect "$OVN_IMAGE" >/dev/null 2>&1; then
    echo "OVN image found on host"
else
    echo "OVN image not found on host, loading from existing node..."

    # Find an existing ready worker node first, fallback to control-plane
    EXISTING_NODE=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2 == "Ready" && $1 ~ /worker/ {print $1; exit}')

    # If no workers exist, try control-plane
    if [ -z "$EXISTING_NODE" ]; then
        EXISTING_NODE=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2 == "Ready" && $1 ~ /control-plane/ {print $1; exit}')
        if [ -n "$EXISTING_NODE" ]; then
            echo "No workers found, using control-plane node: $EXISTING_NODE"
        fi
    fi

    if [ -n "$EXISTING_NODE" ]; then
        echo "Exporting image from $EXISTING_NODE..."
        REMOTE_TMP="/var/ovn-image-$$.tar"
        HOST_TMP=$(mktemp /tmp/ovn-image-host.XXXXXX.tar)

        # Export from existing node
        if $CONTAINER_RUNTIME exec "$EXISTING_NODE" ctr -n k8s.io images export "$REMOTE_TMP" "$OVN_IMAGE" >/dev/null 2>&1; then
            # Copy to host
            if $CONTAINER_RUNTIME cp "${EXISTING_NODE}:${REMOTE_TMP}" "$HOST_TMP" >/dev/null 2>&1; then
                # Load into host
                if $CONTAINER_RUNTIME load -i "$HOST_TMP" >/dev/null 2>&1; then
                    echo "OVN image loaded to $CONTAINER_RUNTIME host"
                else
                    echo "Warning: Failed to load image to host"
                    echo "  Image loading may fail for new nodes"
                fi
                # Cleanup host temp file
                rm -f "$HOST_TMP"
            else
                echo "Warning: Failed to copy image from node"
            fi
            # Cleanup node temp file
            $CONTAINER_RUNTIME exec "$EXISTING_NODE" rm -f "$REMOTE_TMP" >/dev/null 2>&1
        else
            echo "Warning: Failed to export image from $EXISTING_NODE"
        fi
    else
        echo "Warning: No ready nodes found to export image from"
        echo "  You may need to build the image first:"
        echo "    cd go-controller && make && cd ../dist/images && make fedora"
    fi
fi
echo ""

# Function to add a single worker node
add_worker_node() {
    local WORKER_NUM=$1
    local NODE_NAME="${CLUSTER_NAME}-worker${WORKER_NUM}"

    echo "=================================================="
    echo "Adding worker node: $NODE_NAME"
    echo "=================================================="

    # Skip if node is already registered in Kubernetes
    if kubectl get node "$NODE_NAME" &>/dev/null; then
        echo "Node $NODE_NAME already exists and is registered in Kubernetes. Skipping."
        return 0
    fi

    # Step 1: Create container
    echo "[2/7] Creating container..."

    # Remove orphaned container if it exists without a corresponding K8s node
    if $CONTAINER_RUNTIME ps -a --format '{{.Names}}' | grep -q "^${NODE_NAME}$"; then
        echo "Orphaned container $NODE_NAME found (not in K8s), removing it first..."
        $CONTAINER_RUNTIME rm -f "$NODE_NAME" >/dev/null 2>&1
    fi

    # Create container with error output visible
    if ! $CONTAINER_RUNTIME run -d --privileged --name "$NODE_NAME" \
        --hostname "$NODE_NAME" \
        --network "$NETWORK" \
        --tmpfs /tmp --tmpfs /run \
        -v /var -v /lib/modules:/lib/modules:ro \
        "$NODE_IMAGE" >/dev/null; then
        echo "Failed to create container $NODE_NAME"
        echo "Error: Container runtime command failed. Check $CONTAINER_RUNTIME daemon and network."
        return 1
    fi
    echo "Container created: $NODE_NAME"

    # Wait for container to be ready
    sleep 3

    # Step 2: Join cluster
    echo "[3/7] Joining Kubernetes cluster..."
    if ! $CONTAINER_RUNTIME exec "$NODE_NAME" bash -c "$JOIN_COMMAND --ignore-preflight-errors=SystemVerification" >/dev/null 2>&1; then
        echo "Failed to join cluster. Node may already be joined or token expired."
        return 1
    fi
    echo "Joined cluster"

    # Step 3: Load OVN image into node
    echo "[4/7] Loading OVN image..."
    if $CONTAINER_RUNTIME save "$OVN_IMAGE" | $CONTAINER_RUNTIME exec -i "$NODE_NAME" ctr -n k8s.io images import - >/dev/null 2>&1; then
        echo "OVN image loaded into node"
    else
        echo "Warning: Failed to load OVN image, pods may experience ImagePullBackOff"
    fi

    # Step 4: Wait for OVN pods to start
    echo "[5/7] Waiting for OVN pods to initialize..."
    local MAX_WAIT=60
    local COUNT=0
    while [ $COUNT -lt $MAX_WAIT ]; do
        if kubectl get pod -n ovn-kubernetes --field-selector spec.nodeName="$NODE_NAME" 2>/dev/null | grep -q ovs-node; then
            break
        fi
        sleep 2
        COUNT=$((COUNT + 2))
    done

    if [ $COUNT -ge $MAX_WAIT ]; then
        echo "OVN pods not found after ${MAX_WAIT}s, continuing anyway..."
    else
        echo "OVN pods started"
    fi

    # Step 5: Critical fix - Add eth0 to breth0 bridge
    echo "[6/7] Applying network fix (eth0 → breth0)..."

    local MAX_RETRY=60
    local RETRY=0
    local OVS_CONTAINER=""
    while [ $RETRY -lt $MAX_RETRY ]; do
        OVS_CONTAINER=$($CONTAINER_RUNTIME exec "$NODE_NAME" crictl ps --name ovs-daemons -q 2>/dev/null || true)
        if [ -n "$OVS_CONTAINER" ]; then
            # Check if port already exists (OVN may add it automatically)
            if $CONTAINER_RUNTIME exec "$NODE_NAME" crictl exec "$OVS_CONTAINER" ovs-vsctl list-ports breth0 2>/dev/null | grep -q "^eth0$"; then
                echo "Network fix already applied (eth0 already on breth0)"
                break
            fi

            # Try to add port
            if $CONTAINER_RUNTIME exec "$NODE_NAME" crictl exec "$OVS_CONTAINER" ovs-vsctl add-port breth0 eth0 2>/dev/null; then
                echo "Network fix applied (eth0 added to breth0)"
                break
            fi
        fi
        sleep 3
        RETRY=$((RETRY + 3))
    done

    if [ $RETRY -ge $MAX_RETRY ]; then
        echo "Could not apply network fix (OVS container not ready after ${MAX_RETRY}s)"
        echo " Manual fix: OVS=\$($CONTAINER_RUNTIME exec $NODE_NAME crictl ps --name ovs-daemons -q); $CONTAINER_RUNTIME exec $NODE_NAME crictl exec \$OVS ovs-vsctl add-port breth0 eth0"
    fi

    # Step 6: Wait for node to become Ready
    echo "[7/7] Waiting for node to become Ready..."
    MAX_WAIT=120
    COUNT=0
    local NODE_STATUS=""
    while [ $COUNT -lt $MAX_WAIT ]; do
        NODE_STATUS=$(kubectl get node "$NODE_NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        if [ "$NODE_STATUS" = "True" ]; then
            echo "Node is Ready"
            break
        fi
        sleep 3
        COUNT=$((COUNT + 3))
    done

    if [ "$NODE_STATUS" != "True" ]; then
        echo "Node not Ready after ${MAX_WAIT}s (current status: $(kubectl get node "$NODE_NAME" --no-headers 2>/dev/null | awk '{print $2}' || echo 'Unknown'))"
        return 1
    fi

    # Label node as worker
    echo "Labeling node as worker..."
    if kubectl label node "$NODE_NAME" node-role.kubernetes.io/worker="" --overwrite >/dev/null 2>&1; then
        echo "Node labeled as worker"
    else
        echo "Failed to label node (node may already be labeled)"
    fi

    echo ""
}

# Wrapper function for parallel execution
add_worker_node_parallel() {
    local WORKER_NUM=$1
    local LOG_FILE="/tmp/scale-worker-${WORKER_NUM}.log"
    local EXIT_CODE=0

    # Run add_worker_node and capture output
    add_worker_node "$WORKER_NUM" > "$LOG_FILE" 2>&1 || EXIT_CODE=$?

    # Show output
    cat "$LOG_FILE"
    rm -f "$LOG_FILE"

    return "$EXIT_CODE"
}

# Add all requested worker nodes
START_NUM=$((EXISTING_WORKERS + 1))
END_NUM=$((EXISTING_WORKERS + NUM_NODES))
BATCH_SIZE=2

echo "=================================================="
echo "Node Addition Plan"
echo "=================================================="
echo "Total nodes to add: $NUM_NODES"
echo "Starting from: worker${START_NUM}"
echo "Ending at: worker${END_NUM}"

TOTAL_FAILED=0

if [ "$PARALLEL_MODE" = false ]; then
    echo "Mode: Sequential"
    echo ""

    # Sequential execution
    for ((i=START_NUM; i<=END_NUM; i++)); do
        add_worker_node "$i" || { echo "Warning: Failed to add ${CLUSTER_NAME}-worker${i}"; TOTAL_FAILED=$((TOTAL_FAILED + 1)); }
    done
else
    echo "Mode: Parallel"
    echo ""

    # Parallel execution in batches
    CURRENT=$START_NUM
    BATCH_NUM=1

    while [ $CURRENT -le $END_NUM ]; do
        BATCH_END=$((CURRENT + BATCH_SIZE - 1))
        if [ $BATCH_END -gt $END_NUM ]; then
            BATCH_END=$END_NUM
        fi

        BATCH_COUNT=$((BATCH_END - CURRENT + 1))
        echo "=================================================="
        echo "Batch $BATCH_NUM: Adding $BATCH_COUNT node(s) in parallel"
        echo " Nodes: worker${CURRENT} to worker${BATCH_END}"
        echo "=================================================="
        echo ""

        # Start nodes in parallel
        PIDS=()
        for ((i=CURRENT; i<=BATCH_END; i++)); do
            add_worker_node_parallel "$i" &
            PIDS+=($!)
        done

        # Wait for all nodes in this batch
        echo "Waiting for batch $BATCH_NUM to complete..."
        FAILED=0
        for PID in "${PIDS[@]}"; do
            if ! wait "$PID"; then
                FAILED=$((FAILED + 1))
            fi
        done

        if [ $FAILED -gt 0 ]; then
            echo "Warning: $FAILED node(s) in batch $BATCH_NUM had errors"
            TOTAL_FAILED=$((TOTAL_FAILED + FAILED))
        else
            echo "Batch $BATCH_NUM completed successfully"
        fi
        echo ""

        CURRENT=$((BATCH_END + 1))
        BATCH_NUM=$((BATCH_NUM + 1))
    done
fi

echo "=================================================="
echo "Scale-up Complete!"
echo "=================================================="
echo ""
echo "Cluster nodes:"
kubectl get nodes -o wide
echo ""
echo "Newly added workers:"
for ((i=START_NUM; i<=END_NUM; i++)); do
    NODE_NAME="${CLUSTER_NAME}-worker${i}"
    kubectl get node "$NODE_NAME" --no-headers 2>/dev/null || echo "$NODE_NAME - Not found"
done
echo ""
echo "All done! Added $NUM_NODES new worker node(s) to cluster $CLUSTER_NAME"
if [ "$TOTAL_FAILED" -gt 0 ]; then
    echo "Completed with $TOTAL_FAILED failure(s). See warnings above."
    exit 1
fi
