# KIND Cluster Auto-Scaler

Automatically add worker nodes to existing OVN-Kubernetes KIND clusters.

## Quick Start

```bash
# Set kubeconfig
export KUBECONFIG=~/ovn.conf

# Add 3 worker nodes (sequential)
./contrib/scale-kind-cluster.sh -n 3

# Add 10 worker nodes (parallel batches of 2)
./contrib/scale-kind-cluster.sh -n 10 -p
```

## Usage

```bash
./contrib/scale-kind-cluster.sh -n <num_nodes> [-k <kubeconfig>] [-p]

Options:
  -n    Number of worker nodes to add (required)
  -k    Path to kubeconfig (optional, uses $KUBECONFIG if not set)
  -p    Enable parallel mode (batch size: 2, minimum 2 nodes)
  -h    Show help
```

## Examples

```bash
# Sequential mode (default)
./contrib/scale-kind-cluster.sh -n 5

# Parallel mode
./contrib/scale-kind-cluster.sh -n 10 -p

# Explicit kubeconfig
./contrib/scale-kind-cluster.sh -n 3 -k ~/my-cluster.conf

# Environment variable
export KUBECONFIG=~/ovn.conf
./contrib/scale-kind-cluster.sh -n 5 -p
```

## Features

- **Auto-detects container runtime** - Works with both docker and podman
- **Parallel scaling** - Add multiple nodes simultaneously (batch size: 2)
- **OVN image handling** - Pre-loads images to avoid ImagePullBackOff
- **Network fixes** - Applies eth0→breth0 bridge fix automatically
- **Smart numbering** - Continues from highest existing worker number
- **Worker labeling** - Labels nodes with `node-role.kubernetes.io/worker`

## Modes

**Sequential (default)**
- Adds nodes one at a time
- Lower resource usage
- Recommended for limited resources or troubleshooting

**Parallel (-p flag)**
- Adds nodes in batches of 2
- ~2x faster for large clusters
- Requires at least 2 nodes
- Pre-loads OVN images to prevent race conditions

## Requirements

- Existing KIND cluster with OVN-Kubernetes
- `kubectl` configured with cluster access
- Container runtime: docker or podman
- OVN daemonset image (auto-exported from an existing Ready node if not already present on the host; build with `cd go-controller && make && cd ../dist/images && make fedora` if no existing cluster node is available)

*Times vary based on system resources and image availability*

## How It Works

1. Detects cluster name and container runtime
2. Pre-loads OVN image to host (parallel mode)
3. Creates containers with KIND node image
4. Joins nodes to Kubernetes cluster
5. Loads OVN image into each node
6. Applies network fix (eth0→breth0)
7. Waits for nodes to become Ready
8. Labels nodes as workers

## Scale Down 

To scale down nodes and clean up space, refer to the following sample commands.

```bash
 ## Docker
 kubectl delete node ovn-worker{1..12}
 docker stop ovn-worker{1..12}
 docker rm ovn-worker{1..12}
 # Optionally reclaim disk (removes ALL unused volumes — inspect first):
 # docker volume prune -f

 ## Podman
 kubectl delete node ovn-worker{1..12}
 podman stop ovn-worker{1..12}
 podman rm ovn-worker{1..12}
 # Optionally reclaim disk (removes ALL unused volumes — inspect first):
 # podman volume prune -f
```

## Troubleshooting

**ImagePullBackOff errors**
```bash
# Check if OVN image exists on host
docker images | grep ovn-daemonset
# or
podman images | grep ovn-daemonset

# Build if missing
cd go-controller && make
cd ../dist/images && make fedora
```

**Nodes stuck NotReady**
```bash
# Check OVN pods
kubectl get pods -n ovn-kubernetes -o wide

# Apply manual network fix if needed
# Docker:
OVS=$(docker exec ovn-worker3 crictl ps --name ovs-daemons -q)
docker exec ovn-worker3 crictl exec $OVS ovs-vsctl add-port breth0 eth0

# Podman:
OVS=$(podman exec ovn-worker3 crictl ps --name ovs-daemons -q)
podman exec ovn-worker3 crictl exec $OVS ovs-vsctl add-port breth0 eth0
```

## Notes

- Script uses batch size of 2 for parallel mode (configurable via `BATCH_SIZE` variable)
- Works with single-node clusters (control-plane only)
- Automatically detects whether cluster is on docker or podman
- Skips image loading from existing nodes if image already on host
