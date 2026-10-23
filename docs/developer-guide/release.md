# Release Documentation

## Overview

Each new release of OVN-Kubernetes is defined with a "version" that represents the Git tag of a release, such as v1.0.0. This contains the following:

* Source Code
* Binaries
    * `ovnkube`: which is our main single all-in-one binary executable used to launch the ovnkube control plane and data plane pods in a kubernetes deployment
    * `ovn-k8s-cni-overlay`: is the cni executable to be placed in /opt/cni/bin (or another directory in which kubernetes will look for the plugin) so that it can be invoked for each pod event by kubernetes
    * `hybrid-overlay-node`
    * `ovn-kube-util`: contains the Utils for ovn-kubernetes
    * `ovnkube-trace`: is the binary that contains ovnkube-trace which is an abstraction used to invoke OVN/OVS packet tracing utils
    * `ovnkube-identity`: is the executable that is invoked to run ovn-kubernetes identity manager, which includes the admission webhook and the CertificateSigningRequest approver
* `ovnkube` API configuration 
* scripts used to deploy OVN-Kubernetes including helm charts
* Images for [fedora](https://github.com/ovn-kubernetes/ovn-kubernetes/pkgs/container/ovn-kubernetes%2Fovn-kube-fedora) and [ubuntu](https://github.com/ovn-kubernetes/ovn-kubernetes/pkgs/container/ovn-kubernetes%2Fovn-kube-ubuntu)

## Release Planning

* OVN-Kubernetes uses [milestones](https://github.com/ovn-kubernetes/ovn-kubernetes/milestones) to track release planning
* All feature PRs and OKEPs **must** be tagged with the milestone corresponding to their target release before merging,
  so that they are included in release planning and release notes generation
* Bug fix PRs should also be tagged with the milestone of the release they target
* If you are unsure which release a feature or fix will land in, tag it under the
  [Unplanned Backlog](https://github.com/ovn-kubernetes/ovn-kubernetes/milestone/9) milestone — a maintainer
  will triage and assign it to the correct release
* Please check our [roadmap](https://github.com/orgs/ovn-kubernetes/projects/5/views/4) for more details on our release tracking process

## Release Cadence

* We will do three major releases each year. Ex: 1.x.0 in April 2026 and 1.y.0 in August 2026 and 1.z.0 in December 2026
* The release timings have been fixed roughly to be 3 or so months after a major Kubernetes release (allowing time for that to stabilize and for us to consume that kube release before we cut our own release)
* At a given time we will maintain only two active major releases
  (So when 1.2.0 is released we will stop maintaining and backporting fixes into 1.0.0)
* For a supported major release we will continue to do backports for backfixes and offer
  support. A minor patch release will be done at a cadence of 4 weeks for every major release.
  Ex. Once 1.0.0 is released, we will do 1.0.1, 1.0.2, 1.0.3 etc. This also depends on a case-by-case
  basis based on demands from end-users and backport statuses.

## Release Process

* You can find our current releases [here](https://github.com/ovn-kubernetes/ovn-kubernetes/releases).
* Every major release cut will be preceded by an alpha prerelease and beta prerelease.
* See [sample release PR](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4333) which
  will become the head commit for a given release.
* Branch will be cut on the day of release once the release PR merges.
* CI-CD for the release branch will be added to the [GitHub Workflow](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/test.yml#L6).

### Release Notes

* Every PR must include a `release-note` fenced code block in its description (enforced by CodeRabbit).
  The block must contain either a short user-facing description of the change or the single word `NONE`
  if the PR has no user-facing impact (e.g. tests, refactors, CI-only changes):
  ````
  ```release-note
  Added support for X feature in Y component.
  ```
  ````
  or for non-user-facing changes:
  ````
  ```release-note
  NONE
  ```
  ````
* Release notes are generated from the `release-note` blocks of all PRs merged into a release. Any PR with
  a meaningful (non-`NONE`) `release-note` block will be included in the release notes.

## Backport Request

* If a PR needs to be backported to an older release that should be requested
  by adding the `needs-backport` label and noting the target releases on the PR.
* Reach out to the maintainers on slack or by tagging them directly on the PR or come to the community meetings to discuss this.

### Preparing a backport

1. Create a branch from the target `release-X.Y` branch.
2. Cherry-pick the original commits with `git cherry-pick -x <commit>` so each
   backport commit records its source.
3. Resolve any conflicts, keeping the fix appropriate for the release branch.
   When conflicts occur, we recommend keeping a `Conflicts:` section in the
   final commit message. List the affected files and briefly explain why each
   conflict occurred and how it was resolved. For example:

    ```text
    Conflicts:
        go-controller/pkg/ovn/example.go
            The release branch lacks the helper introduced by a later
            refactor. Applied the fix to the existing inline logic.
    ```

4. Run the relevant tests on the release branch, including coverage of any
   conflict resolutions.
5. Open a PR targeting `release-X.Y`, link the original PR, and describe any
   adaptations and the testing performed. Prepare a separate PR for each target
   release.

## Information on Past Releases

* [v1.0.0](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/release-1.0) - [release-notes](https://github.com/ovn-kubernetes/ovn-kubernetes/releases/tag/v1.0.0) - not maintained anymore.
* [v1.1.0](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/release-1.1) - [release-notes](https://github.com/ovn-kubernetes/ovn-kubernetes/releases/tag/v1.1.0) - not maintained anymore.
* [v1.2.0](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/release-1.2) - [release-notes](https://github.com/ovn-kubernetes/ovn-kubernetes/releases/tag/v1.2.0) - not maintained anymore.
* [v1.3.0](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/release-1.3) - [release-notes](https://github.com/ovn-kubernetes/ovn-kubernetes/releases/tag/v1.3.0) - actively maintained
* [v1.4.0](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/release-1.4) - [release-notes](https://github.com/ovn-kubernetes/ovn-kubernetes/releases/tag/v1.4.0) - actively maintained
