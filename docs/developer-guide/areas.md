# Project Areas

ovn-kubernetes organises its codebase into **areas** — focused domains that are
owned by designated **Area Maintainers**. Each area has a clear scope of files
defined in [`CODEOWNERS`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/CODEOWNERS)
and a set of maintainers and reviewers responsible for its health.

For the full governance details — roles, responsibilities, appointment and
removal process — see [Area Maintainers](../governance/GOVERNANCE.md#area-maintainers)
in the governance docs.

## How Areas Work

* Every area is declared as a section in `CODEOWNERS` with a header comment
  identifying the Area Maintainer(s):
  ```
  # Virtualization (Area Maintainer: @user)
  ```
* GitHub automatically assigns reviewers from the listed owners when a PR
  touches files matching the area's patterns.
* Area Maintainers can merge PRs that **exclusively** touch files within their
  area by commenting `/area-maintainer-approved` on the PR. The merge bot
  (`.github/workflows/area-merge.yml`) verifies file scope, CI status, and
  authorization before merging.
* PRs that touch files across multiple areas require a repo Maintainer to merge.

## Merge Bot (ovn-kubernetes-merge-bot)

The area merge workflow uses a dedicated GitHub App called
**ovn-kubernetes-merge-bot** to perform merges. This is required because the
repository's branch protection rules restrict who can push to `master`. The
app is added to the branch protection allow-list, enabling it to merge PRs on
behalf of area maintainers once all checks pass.

**Configuration:**

| Item | Location |
|---|---|
| GitHub App | Installed on the `ovn-kubernetes` org ([app settings](https://github.com/organizations/ovn-kubernetes/settings/apps/ovn-kubernetes-merge-bot)) |
| Client ID | Repository variable: `OVN_KUBERNETES_MERGE_BOT` |
| Private key | Repository secret: `OVN_KUBERNETES_MERGE_BOT` |
| Branch protection | `master` — app listed under "Restrict who can push to matching branches" |

The workflow generates a short-lived installation token via
[`actions/create-github-app-token`](https://github.com/actions/create-github-app-token)
at the start of each run. No long-lived access tokens are used during workflow execution.

## Current Areas

### Virtualization

| | |
|---|---|
| **Scope** | KubeVirt integration, live migration, multi-homing, localnet |
| **Area Maintainer** | [@maiqueb](https://github.com/maiqueb) |
| **Reviewers** | [@qinqon](https://github.com/qinqon), [@ormergi](https://github.com/ormergi) |

**Files:**

| Category | Paths |
|---|---|
| E2E tests | `/test/e2e/kubevirt.go`, `/test/e2e/kubevirt/`, `/test/e2e/multihoming.go`, `/test/e2e/multihoming_utils.go`, `/test/e2e/multihoming_external_router_utils.go`, `/test/e2e/network_segmentation_localnet.go`, `/test/e2e/localnet-underlay.go`, `/test/e2e/network_segmentation_preconfigured_layer2.go` |
| Unit tests | `/go-controller/pkg/ovn/kubevirt_test.go`, `/go-controller/pkg/ovn/multihoming_test.go`, `/go-controller/pkg/ovn/multipolicy_test.go`, `/go-controller/pkg/ovn/layer2_user_defined_network_controller_test.go`, `/go-controller/pkg/util/multi_network_test.go`, `/test/crd-integration/testscenario/cudn/valid-scenarios-localnet.go`, `/test/crd-integration/testscenario/cudn/invalid-scenarios-localnet-*.go` |
| Production code | `/go-controller/pkg/kubevirt/`, `/go-controller/pkg/util/arp.go`, `/go-controller/pkg/util/ndp/` |
| Docs | `/docs/features/live-migration.md`, `/docs/features/multiple-networks/multi-homing.md`, `/docs/features/multiple-networks/multi-network-policies.md` |

## Adding a New Area

1. Open a PR proposing the new area — the PR must be approved by the repo
   Maintainers (see [Governance](../governance/GOVERNANCE.md#area-maintainers)).
2. Add a new section to `CODEOWNERS` with the file patterns and the proposed
   Area Maintainer in the section header comment.
3. Add an entry to this document describing the area's scope, maintainer(s),
   and reviewers.
4. Once merged, the Area Maintainer can begin using `/area-maintainer-approved`
   to merge qualifying PRs.
