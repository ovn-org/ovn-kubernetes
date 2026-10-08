# Running e2e tests against an existing cluster

The `kube` infrastructure provider runs the existing Go e2e suite against a
cluster already running OVN-Kubernetes. It does not create or destroy it.

Kind remains the default provider. To select an existing cluster:

```console
cd test/e2e
export KUBECONFIG=/path/to/kubeconfig
export OVN_TEST_INFRA_PROVIDER=kube

go test -timeout=60m -v . \
  -ginkgo.v \
  -ginkgo.fail-on-empty \
  -ginkgo.focus='<focus>'
```

Some scopes read `KUBECONFIG` directly, so set it even when passing the
framework's `-kubeconfig` flag. The credentials must be able to manage the
resources created by the selected specs. Except for DPU uplink runs, suite
startup requires access to list Nodes and valid OVN-Kubernetes
primary-interface annotations. When the Nodes have no shared range, specs that
need an address from it skip. Node commands require get, list, create, and
delete access to Pods plus create access to `pods/exec` in the OVN-Kubernetes
namespace. The cluster must admit a privileged Pod using host PID, host network,
and a host-root `hostPath` volume.

## Dependencies

Core OVN-Kubernetes tests require Kubernetes and a healthy
OVN-Kubernetes CNI installation. External-container specs also require an SSH
host with a container runtime. Node lifecycle and spare-underlay operations
skip because this provider does not own that infrastructure.

The existing cluster must be able to pull the suite's test images, or have them
preloaded or mirrored. Unlike the Kind provider, the Kubernetes provider does
not preload images into the Nodes.

| Variable | Behavior when unset |
| --- | --- |
| `OVN_TEST_OVNK_NAMESPACE` | `ovn-kubernetes` |
| `OVN_TEST_FRRK8S_NAMESPACE` | `frr-k8s-system` |
| `OVN_TEST_EXTERNAL_BRIDGE` | Required by specs that use the gateway bridge |
| `OVN_TEST_PRIMARY_INTERFACE` | Required by specs that use the primary interface |
| `OVN_TEST_NBDB_CONTAINER` | `nb-ovsdb` |
| `OVN_TEST_L3_UDN_MULTI_SUBNET` | Disabled; set to `true` when the deployment supports it |
| `OVN_TEST_CONTAINER_HOST` | External-container specs skip |
| `OVN_TEST_PRIMARY_NETWORK` | Required with a container host; names its runtime network that reaches the Nodes |
| `OVN_TEST_CONTAINER_HOST_USER` | `root` |
| `OVN_TEST_CONTAINER_HOST_PORT` | `22` |
| `OVN_TEST_CONTAINER_HOST_KEY` | Required with a container host; path on the test runner |
| `CONTAINER_RUNTIME` | `docker` |

## External containers

The provider uses the same container engine as Kind, with its commands run over
SSH on `OVN_TEST_CONTAINER_HOST`. The declared runtime must be on that host's
`PATH`, and `OVN_TEST_PRIMARY_NETWORK` must already exist there. The suite
creates and deletes containers and temporary runtime networks on that host.

The primary runtime network must reach the Nodes. The provider verifies that a
Node's OVN-Kubernetes primary-interface annotation belongs to that network and
reads its interface name and MAC from the Node. It does not create the cluster,
attach networks to Nodes, or power Nodes on and off.

## Node commands

The provider executes Node commands through one privileged Pod per Node. Each
Pod uses the host PID and network namespaces and chroots into the host. The
image is borrowed from the OVN-Kubernetes Node Pod already running there, and
the shell Pods are deleted after the suite. Node commands invoke a local
`kubectl` executable, which must be on `PATH`.

This path depends on the kubelet. The provider rejects
`systemctl stop kubelet.service` and `systemctl restart kubelet`; affected test
scopes skip before changing Nodes.

## Remaining portability limits

Known cases that are not portable through the current provider interface
include:

- localnet tests that call `SetupUnderlay` skip because the generic provider
  does not mutate OVS bridges or take ownership of host interfaces;
- the Kube provider skips the static Pod test because it uses local Docker to
  modify Kind Nodes;
- the Kube provider skips Node IP migration because it uses local Docker and
  restarts kubelet;
- DPU host-node deletion skips outside Kind because it requires the Kind DPU
  simulator layout;
- some service and network tests use local `sudo`, network namespaces, routes,
  and iptables on the test runner;
- KubeVirt scopes execute `/tmp/virtctl`; when absent, the suite downloads a
  Linux/amd64 binary there, requiring a writable `/tmp`, outbound access, and a
  compatible test runner unless a suitable binary is already cached;
- node shutdown/startup runs only when the provider implements
  `NodeInfrastructure`;
- tests that assume systemd manages `kubelet.service` do not work on every
  Kubernetes distribution;
- the default report path is still `/tmp/kind/logs`, although it can be
  overridden with `-report-path`.
