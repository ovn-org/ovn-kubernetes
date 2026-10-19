# Developer Documentation

This file aims to have information that is useful to the people contributing to this repo.

## Generating ovsdb bindings using modelgen

In order to generate the latest NBDB and SBDB bindings, we have a tool called `modelgen`
which lives in the libovsdb repo: https://github.com/ovn-kubernetes/libovsdb#modelgen. It is a
[code generator](https://go.dev/blog/generate) that uses `pkg/nbdb/gen.go` and `pkg/sbdb/gen.go`
files to auto-generate the models and additional code like deep-copy methods.

In order to use this tool do the following:

```
$ cd go-controller/
$ make modelgen
curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/${OVN_SCHEMA_VERSION}/ovn-nb.ovsschema -o pkg/nbdb/ovn-nb.ovsschema
curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/${OVN_SCHEMA_VERSION}/ovn-sb.ovsschema -o pkg/sbdb/ovn-sb.ovsschema
hack/update-modelgen.sh
```

If there are new bindings then you should see the changes being generated in the `pkg/nbdb` and
`pkg/sbdb` parts of the repo. Include them and push a commit!

NOTE1: You have to pay attention to the version of the commit hash used to download the modelgen
client. While the client doesn't change too often it can also become outdated causing wrong
generations. So keep in mind to re-install modelgen with latest commits and change the hash
value in the `hack/update-modelgen.sh` file if you find it outdated.

NOTE2: From time to time we always bump our fedora version of OVN used by KinD. But we oftentimes
forget to update the `OVN_SCHEMA_VERSION` in our `Makefile` which is used to download the ovsdb schema.
If that version seems to be outdated, probably best to update that as well and re-generate the schema
bindings.

## Generating CRD yamls using codegen

In order to generate the latest yaml files for a given CRD or to add a new CRD, once
the `types.go` has been created according to sig-apimachinery docs, the developer can run
`make codegen` to be able to generate all the clientgen, listers and informers for the new
CRD along with the deep-copy methods and actual yaml files which get created in `_output/crd`
folder and are copied over to `helm/ovn-kubernetes/crds` to then be used when creating a KinD cluster.

## Generating and Updating Mocks

Mocks for unit tests are auto-generated using [mockery](https://github.com/vektra/mockery)
and configured via `go-controller/.mockery.yaml`. To regenerate all mocks:

```bash
cd go-controller/
make mocksgen
```

### Mock file placement

Mock output locations are determined by `.mockery.yaml` — it is the single
source of truth. The default output is `pkg/testing/mocks/{{ '{{' }}.PackagePath{{ '}}' }}/`,
but packages can override this with a `dir` config entry to place mocks
elsewhere (e.g. `pkg/factory/mocks/`, `pkg/util/mocks/`).

### Adding a new mock

- If the package already has `all: true` in `.mockery.yaml`, new interfaces
  in that package are picked up automatically — no config change needed.
- Otherwise, add an entry for the new interface under the appropriate
  package in `.mockery.yaml`, then run `make mocksgen`.
- Do not run `mockery` manually — the config file is the source of truth.

## Level-Driven Controllers

### Background

OVN-Kubernetes has scale issues with network controllers today. We spin
up 1 network controller per UDN, which incurs heavy cost when handling
resource object events. The cost for example of unmarshaling a node
annotation can be O(n) where n is the number of UDN controllers that are
parsing the object.

To fix this scale problem, the project has started to move to single
controller instances that are aware of all UDNs. Therefore, handling and
parsing of resource objects are done once. A few controllers have
already moved in this direction, and the remaining components that are
within a UDN controller (such as pod, node, namespace) will be migrated
incrementally.
    
Furthermore, as we move to per-resource, multi-network aware
controllers, each controller type needs to be able to get network
information. One obvious way to do this is to add a level-driven controller
for NADs in each main controller.
However, this too has a performance cost, because upon each NAD event,
each controller will need to parse the NAD. Since we already have
networkManager (NAD Controller), it is parsing and updating its cache
with the NAD. In order to solve this problem, NetworkManager has been
extended with a "RegisterNADReconciler" function, which is a callback that
controllers can register with NAD Controller to be informed when a NAD
event happens. Controllers can then query NAD Controller to access its
cache as the source of truth.
    
For example there is commonly used GetActiveNetworkForNamespace, and a
new API, GetPrimaryNADForNamespace is added in #5623, as well as
GetNetInfoForNADKey.
    
Controllers should be using the pkg/controller Reconciler framework to
implement their level-driven controllers, which are fed keys externally
from NAD Controller.

### Guidelines

We prefer level-driven controllers built with `pkg/controller/controller.go`. When adding a new controller:

- Use the shared controller framework rather than bespoke loops or per-network controllers.
- Design controllers to be User Defined Network (UDN) aware: a single controller instance should reconcile objects across all networks instead of spinning up one instance per network.
- If the controller is network-aware, do **not** create a separate NAD Controller, use a Reconciler.
- The network manager is the source of truth for NADs; register a lightweight, non-blocking handler via `RegisterNADReconciler` that will queue keys to the Reconciler.
- RegisterNADReconciler  **before** starting controller workers to avoid missing events during startup.
- For a concrete example, see `go-controller/pkg/ovn/controller/egressfirewall/egressfirewall.go`.
