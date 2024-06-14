# Release Documentation

## Overview

Each new release of OVN-Kubernetes is defined with a "version" that represents the Git tag of a release, such as v1.0.0. This contains the following:

* Source Code
* Binaries
  * `ovnkube`: which is our main single all-in-one binary executable used to launch the ovnkube control plane and data plane pods in a kubernetes deployment
  * `ovn-k8s-cni-overlay`: is the cni executable to be placed in /opt/cni/bin (or another directory in which kubernetes will look for the plugin) so that it can be invoked for each pod event by kubernetes
  * `hybrid-overlay-node`
  * `ovn-kube-util`: contains the Utils for ovn-kubernetes
  * `ovndbchecker`
  * `ovnkube-trace`: is the binary that contains ovnkube-trace which is an abstraction used to invoke OVN/OVS packet tracing utils
  * `ovnkube-identity`: is the executable that is invoked to run ovn-kubernetes identity manager, which includes the admission webhook and the CertificateSigningRequest approver
* `ovnkube` API configuration 
* scripts used to deploy OVN-Kubernetes including helm charts
* Images for [fedora](https://github.com/ovn-org/ovn-kubernetes/pkgs/container/ovn-kubernetes%2Fovn-kube-f) and [ubuntu](https://github.com/ovn-org/ovn-kubernetes/pkgs/container/ovn-kubernetes%2Fovn-kube-u)

## Release Planning

* OVN-Kubernetes projects uses [milestones](https://github.com/ovn-org/ovn-kubernetes/milestones) to track our release planning
* All PRs and Issues must be tagged with the correct milestone so that it get's included in the release planning
* Please check our [roadmap](https://github.com/orgs/ovn-org/projects/5/views/4) for more details on our release tracking process

## Release Cadence

* We will do two major releases each year. Ex: 1.0.0 at June 2024 and 1.1.0 at Dec 2024
* At a given time we will maintain only two active major releases
  (So when 1.2.0 is released we will stop maintaining and backporting fixes into 1.0.0)
* For a supported major release we will continue to do backports for backfixes and offer
  support. A minor patch release will be done at a cadence of 4 weeks for every major release.
  Ex. Once 1.0.0 is release, we will do 1.0.1, 1.0.2, 1.0.3 etc. This also depends on a case-by-case
  basis based on demands from end-users and backport statuses.

## Release Process

* You can find our current releases [here](https://github.com/ovn-org/ovn-kubernetes/releases).
* Every major release cut will be preceded by an alpha prerelease and beta prerelease.
* See [sample release PR](https://github.com/ovn-org/ovn-kubernetes/pull/4333) which
  will become the head commit for a given release.
* Branch will be cut on the day of release once the release PR merges.

## BackPort Request

* If a PR needs to be backported to an older release that should be requested
  by adding the `needs-backport` label.
* Reach out to the maintainers on slack or by tagging them directly on the PR.
