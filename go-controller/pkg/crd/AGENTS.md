# AGENTS.md — CRDs

CRD type definitions and generated clients/informers/listers for OVN-Kubernetes
custom resources. See the parent [`go-controller/AGENTS.md`](../../AGENTS.md)
for build commands and overall conventions.

## Package Layout

Each CRD has its own sub-package with this structure:

```text
<crd-name>/
  v1/
    doc.go                   # Package doc + deepcopy-gen/groupName markers
    register.go              # SchemeBuilder and resource registration
    types.go (or *.go)       # CRD Go type definitions (Spec, Status, List)
    zz_generated.deepcopy.go # Generated — DO NOT EDIT
    apis/
      clientset/             # Generated typed client — DO NOT EDIT
      informers/             # Generated informers — DO NOT EDIT
      listers/               # Generated listers — DO NOT EDIT
      applyconfiguration/    # Generated apply configs — DO NOT EDIT
```

## CRDs

| Sub-package | CRD |
|-------------|-----|
| `adminpolicybasedroute/` | AdminPolicyBasedRoute |
| `clusternetworkconnect/` | ClusterNetworkConnect |
| `egressfirewall/` | EgressFirewall |
| `egressip/` | EgressIP |
| `egressqos/` | EgressQoS |
| `egressservice/` | EgressService |
| `networkqos/` | NetworkQoS |
| `observabilityconfig/` | ObservabilityConfig |
| `routeadvertisements/` | RouteAdvertisements |
| `userdefinednetwork/` | UserDefinedNetwork, ClusterUserDefinedNetwork |
| `vtep/` | VTEP |

Shared types used across multiple CRDs live in `types/`.

## Coding Standards

Follow the Kubernetes [API Conventions](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md)
and [API Changes](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api_changes.md)
guides. Project-specific additions:

- **Never hand-edit generated files** — files under `apis/` and
  `zz_generated.deepcopy.go` are generated. Run `make codegen` from
  `go-controller/` to regenerate after changing types. This also
  regenerates CRD YAML manifests and copies them to
  `helm/ovn-kubernetes/crds/` — commit those too.
- **Markers** — use the correct code-generator markers in `doc.go`
  (`+k8s:deepcopy-gen=package`, `+groupName=k8s.ovn.org`).
- **Backwards compatibility** — CRD API changes must be backwards
  compatible regardless of API maturity. Any breaking change requires a
  new API version with a conversion or migration plan.
- **Validation** — add kubebuilder validation markers on Spec fields
  where applicable (e.g. `+kubebuilder:validation:Enum`,
  `+kubebuilder:validation:Required`, `+kubebuilder:validation:MaxItems`,
  `+kubebuilder:validation:MaxLength`). Use CEL validation rules
  (`+kubebuilder:validation:XValidation`) for cross-field constraints and
  complex invariants that markers alone cannot express.
- **Status subresource** — use the status subresource pattern. Status
  updates should not modify Spec and vice versa.
- **Naming** — follow Kubernetes API conventions: PascalCase for type
  names, camelCase for JSON tags, plural resource names.
- **Group** — all OVN-Kubernetes CRDs belong to the `k8s.ovn.org` API group.
- **Optional fields** — use pointers for optional fields so that zero-values
  are distinguishable from unset (e.g. `*int32`, `*string`). Use the
  `+optional` marker and `omitempty` JSON tag.
- **Defaulting** — if a field has a default value, declare it with
  `+kubebuilder:default=` in the marker. For complex defaulting logic,
  use a defaulting webhook (`MutatingWebhookConfiguration`). Defaults
  must not conflict with validation — a defaulted object must always
  pass validation.
- **Print columns** — add `+kubebuilder:printcolumn` markers on the root
  type so `kubectl get` shows useful information without `-o yaml`.
- **Finalizers** — use finalizers for resources that need cleanup logic
  before deletion. The finalizer string should be scoped to the group
  (e.g. `k8s.ovn.org/<purpose>`). Always remove the finalizer once
  cleanup completes to avoid blocking deletion.
- **Status conditions** — follow the standard `metav1.Condition` type.
  Mark condition fields with `+listType=map`, `+listMapKey=type`, and
  `+patchMergeKey=type` / `+patchStrategy=merge` so that Server-Side
  Apply (SSA) can merge conditions from multiple controllers without
  conflicts.
- **Documentation** — CRD type changes must be accompanied by updating the
  API reference docs in `docs/api-reference/`. Each CRD has a
  `<name>-api-spec.md` file documenting its spec/status fields. New CRDs
  must also be added to `mkdocs.yml` under "API Reference Guide" and to
  `docs/api-reference/introduction.md`.
- **Validation tests** — any new or changed CRD API field must add or update
  CRD integration tests in [`test/crd-integration/`](../../../test/crd-integration/),
  which verify field constraints (defaulting, CEL rules, enums, required fields,
  max items, admission, etc.) are enforced by the API server. These run against
  [`envtest`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest) —
  a real API server and etcd started locally, giving accurate CRD validation
  without a full cluster. Run them with `make test-crd` from `test/`. Prefer
  these over e2e tests in `test/e2e/` for schema validation that needs no live
  pod networking.
