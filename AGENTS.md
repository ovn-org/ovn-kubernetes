# AGENTS.md — OVN-Kubernetes

Project context for AI coding agents. See [agents.md](https://agents.md/) for the open standard.

## Project Overview

OVN-Kubernetes is a network plugin written according to CNI Spec that provides
networking for Kubernetes clusters with Open Virtual Network (OVN) and
Open vSwitch (OVS) at its core.

| Feature | Description |
|---------|-------------|
| Pod Networking | Pod-to-pod connectivity via OVN logical switches and routers |
| IPAM | IP address management for pods and networks |
| Services | Kubernetes Services implemented as OVN load balancers |
| Endpoint Slices | Scalable endpoint tracking for services |
| NetworkPolicy | Kubernetes NetworkPolicy enforcement via OVN ACLs |
| AdminNetworkPolicy | Cluster-scoped network policy (ANP/BANP) |
| EgressIP | Source IP control for egress traffic |
| EgressFirewall | Egress traffic filtering rules |
| EgressService | Egress traffic routing through services |
| EgressQoS | QoS marking on egress traffic |
| Multi-Egress Gateway | Egress traffic via multiple gateway nodes |
| User Defined Networks | Multi-network support, network segmentation (UDN) |
| Cluster Network Connect | Connecting isolated User Defined Networks together for controlled inter-UDN connectivity |
| Multi-Homing | Pods attached to multiple networks |
| DPU/SmartNIC Offload | Hardware acceleration via OVS offload |
| Multicast | IGMP snooping and relay via OVN |
| NetworkQoS | DSCP marking and traffic shaping |
| BGP | Route advertisements and peering |
| EVPN | Ethernet VPN integration |
| No-Overlay | Direct pod routing using BGP-learned routes, without encapsulation |
| KubeVirt | VM live migration and persistent IPs support |
| Hybrid Overlay | Mixed Windows/Linux clusters via VXLAN |

## Repository Layout

```text
go-controller/        # Main Go codebase — feature implementations, component code, ovnkube binaries, CRDs, libovsdb models, observability library, hybrid-overlay
test/e2e/             # End-to-end Ginkgo tests covering all features (network policy, egress, UDN, KubeVirt, BGP, etc.)
test/crd-integration/ # CRD integration tests (defaulting, validation, CEL) — run via envtest, no KinD cluster needed
dist/                 # Container images (Dockerfiles) and deployment YAML manifests
helm/                 # Helm charts for deploying ovn-kubernetes
docs/                 # mkdocs source for ovn-kubernetes.io — OKEPs, feature docs, design docs, developer, installation guides
contrib/              # KinD cluster scripts, local deployment helpers (kind.sh), and development tooling
LICENSES/             # License and third-party package licensing information
```

## Build and Test

```bash
cd go-controller/
make build          # Build ovnkube binaries
make lint           # Format and lint Go code (required before PR)
make test           # Run unit tests
```

E2E tests run via CI on KinD clusters. See `test/e2e/` for Ginkgo test suites.
To run E2E locally, first set up a KinD cluster using `contrib/kind.sh`, then run the tests.
See `docs/developer-guide/local_testing_guide.md` for more details.

CRD admission tests (defaulting, validation, CEL rules) belong in `test/crd-integration/`,
not in `test/e2e/`. They run against a real API server via envtest — no cluster needed.
Run with `make test-crd` from `test/`. See
[`go-controller/pkg/crd/AGENTS.md`](go-controller/pkg/crd/AGENTS.md) for when they are
required, and `docs/developer-guide/local_testing_guide.md#crd-integration-tests` for how
to run and add new tests.

Changes under `test/` are exempt from requirements to add unit tests. Do not
request or add unit tests for code in this directory, including test helpers,
image configuration, deployment configuration, and infrastructure providers.
Validate changes with the existing builds and relevant test suites as appropriate,
and preserve coverage of existing scenarios. If a PR also changes product code
outside `test/`, the usual test requirements still apply to those product changes.

## Key Conventions

See `CONTRIBUTING.md` for full details:
- [Commit message guidelines](CONTRIBUTING.md#commit-message-guidelines)
- [Pull request checklist](CONTRIBUTING.md#pull-request-checklist)
- [Sign your commits (DCO)](CONTRIBUTING.md#sign-your-commits)
- [AI guidelines](CONTRIBUTING.md#ai-guidelines)

## OKEPs (Enhancement Proposals)

New features require an OKEP in `docs/okeps/`. See `docs/okeps/okep-4368-template.md` for the
template. OKEPs must have an associated GitHub issue, cover all template sections and update
`mkdocs.yml`.

## Architecture Notes

The OVN-Kubernetes plugin watches the Kubernetes API. It acts on the generated Kubernetes
cluster events by creating and configuring the corresponding OVN logical constructs in the
OVN database for those events. OVN (which is an abstraction on top of Open vSwitch) converts
these logical constructs into logical flows in its database and programs the OpenFlow flows
on the node, which enables networking on a Kubernetes cluster.

See [Architecture](docs/design/architecture.md) for component details (ovnkube-control-plane,
ovnkube-node, ovs-node pods and their containers).

### Gateway Modes

OVN-Kubernetes supports two gateway modes that affect how traffic enters and
leaves the cluster:
- **Local gateway (lgw)** — Traffic leaving the pod to go outside the cluster leaves the OVN stack
  and enters the host networking stack via the management port (mp-X) and then based on routes on
  the host, it either leaves via breth0/primary node NIC or via other interfaces on the host. This
  mode uses nftables to implement a lot of the service traffic and since traffic leaves the OVN/OVS
  datapath, it is not hardware offloadable.
- **Shared gateway (sgw)** — Traffic leaving the pod to go outside the cluster stays within the
  OVN/OVS datapath. It goes from the pod through the OVN logical switch, to the Gateway Router
  (GR), and out via the OVS bridge (breth0) directly. This mode is hardware offloadable to DPUs/SmartNICs.

To determine which mode a cluster is using, check the `k8s.ovn.org/l3-gateway-config`
annotation on any node — the `mode` field will be `"local"` or `"shared"`.
Shared gateway is the default.

## Docs

### Website validation

The website has no unit test suite. Do not request unit, regression, or E2E
tests, or introduce a test framework, for documentation or website infrastructure
changes. This includes documentation content, `mkdocs.yml` navigation/configuration,
website assets/templates, documentation dependencies, and website build/deployment
scripts or workflows, including fixes to website behavior. Validate with the existing
documentation build and manual preview/link checks as appropriate. If a PR also
changes product code, the usual test requirements still apply to those code changes.

### API Reference
```bash
make -C docs/ generate-api-reference  # Generating API reference docs
```
Should be created when new CRD is introduced, or updated upon CRD changes.


