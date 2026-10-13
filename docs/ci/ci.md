# CI Tests

For CI, OVN-Kubernetes runs the
[Kubernetes E2E tests](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
and some [locally defined](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/test/e2e) tests. 
[GitHub Actions](https://help.github.com/en/actions)
are used to run a subset of the Kubernetes E2E tests on each pull request. The
local workflow that controls the test run is located in
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/test.yml).

The following tasks are performed:

- Build OVN-Kubernetes
- Check out the Kubernetes source tree and compiles some dependencies
- Install KIND
- Run a matrix of End-To-End Tests using KIND
- Ensure that documentation builds successfully

The full matrix of e2e tests found [here](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/test.yml)
are also run periodically (twice daily) using an OVN-Kubernetes build based on the currently merged code base.

The following sections should help you understand (and if needed modify) the set of tests that run and how to run these
tests locally.

## CI fails: what do I do?

Some tests are known to be flaky, see [`kind/ci-flake` issues.](https://github.com/ovn-kubernetes/ovn-kubernetes/issues?q=is%3Aissue%20state%3Aopen%20label%3Akind%2Fci-flake)
At the end of your failed test run, you will see something like:

```
Summarizing 1 Failure:
  [FAIL] e2e egress firewall policy validation with external containers [It] Should validate the egress firewall policy functionality for allowed IP
  /home/runner/work/ovn-kubernetes/ovn-kubernetes/test/e2e/egress_firewall.go:130
```
then search for "e2e egress firewall policy validation" in the open issues. 

If you find an issue that matches your failure, update the issue with your job link. 
If the issue doesn't exist, it either means the failure is introduced in your PR or it is a new flake. 
Try to run the same test locally multiple times, and if doesn't fail, report a new flake.
Reporting a new flake is fairly straightforward, but you can use already open issues as an example.
It may also be useful sometimes to search through the closed issues to see if the flake was reported
previously and (not really) fixed, then reopening it with the new job failure.

Only after following these steps ^, you can comment `/retest-failed` on your PR to trigger a retest of the failed tests.
A rocket emoji reaction on your comment should apper when the retest is triggered.

Before running this command, please reference existing or newly opened issues to justify the retest request.

## Understanding the CI Test Suite

The tests are broken into 2 categories, `shard` tests which execute tests from the Kubernetes E2E test suite and the
`control-plane` tests which run locally defined tests.

### Shard tests

The shard tests are broken into a set of shards, which is just a grouping of tests,
and each shard is run in a separate job in parallel. Shards execute the `shard-%` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/test/Makefile).
The set of shards may change in the future. Below is an example of the shards at time of this writing:

- shard-network
  - All E2E tests that match `[sig-network]`
- shard-conformance
  - All E2E tests that match `[Conformance]|[sig-network]`
- shard-test
  - Single E2E test that matches the name of the test specified with a regex. 
  - When selecting the `shard-test` target, you focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

Shards use the [E2E framework](https://kubernetes.io/blog/2019/03/22/kubernetes-end-to-end-testing-for-everyone/). By
selecting a specific shard, you modify ginkgo's `--focus` parameter.

The regex expression for determining which E2E test is run in which shard, as
well as the list of skipped tests is defined in
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh).

### Control-plane tests

In addition to the `shard-%` tests, there is also a `control-plane` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/test/Makefile).
Below is a description of this target:

- control-plane
  - All locally defined tests by default.
  - You can focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

All local tests are run by `make control-plane`. The local tests are controlled in
[ovn-kubernetes/test/scripts/e2e-cp.sh](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/test/scripts/e2e-cp.sh)
and the actual tests are defined in the directory
[ovn-kubernetes/test/e2e/](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/test/e2e).

#### Node IP migration tests

The node IP migration tests are part of the control-plane tests but due to their impact they cannot be run concurrently
with other tests and they are disabled when running `make control-plane`.
Instead, they must explicitly be requested with `make -C test control-plane WHAT="Node IP address migration"`.

### Github CI integration through Github Actions Matrix

Each of these shards and control-plane tests can then be run in a [Github Actions matrix](https://docs.github.com/en/actions/learn-github-actions/managing-complex-workflows#using-a-build-matrix) of:
* Local Gateway Mode and Shared Gateway Mode. See [Gateway Modes](../design/gateway-modes.md).
* IPv4 Only, IPv6 Only and Dualstack
* Disabled SNAT Multiple Gateways or Enabled SNAT Gateways
* Single bridge or two bridges

To reduce the explosion of tests being run in CI, the test cases run are limited
using an `exclude:` statement in 
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/test.yml).

# CRD Integration Tests

Tests from
[`test/crd-integration/`](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/test/crd-integration)
verify **CRD admission behaviour** against a real kube-apiserver via
[envtest](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest), with
no Kind cluster required.  Because they are lightweight, they run as a step in
the `Test-PR` unit-test job rather than in a dedicated lane.  See the
[Local Testing Guide](../developer-guide/local_testing_guide.md#crd-integration-tests)
for prerequisites, how to run them, and how to add new tests.

## What belongs here

These tests are the intended home for **CRD admission behaviour** that can
be verified without live pod networking:

- **Defaulting** — fields with `+kubebuilder:default` markers are filled in
  correctly when absent from a user's YAML
- **Validation** — `+kubebuilder:validation` constraints reject invalid objects
- **CEL rules** — `+kubebuilder:validation:XValidation` expressions behave as
  specified

Do **not** add these to `test/e2e/` or `test/e2e/testscenario/`.  The E2E
suite requires a full Kind cluster; CRD admission tests do not.  Adding them
to the E2E suite makes them slower, harder to run locally, and harder to
iterate on.  Schema-only tests currently in
[`test/e2e/testscenario/`](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/test/e2e/testscenario)
should be migrated here over time (tracked in
[ovn-kubernetes#6932](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6932)).

# Conformance Tests

We have a conformance test suit that can be invoked using the `make conformance` command.
Currently we run the `TestNetworkPolicyV2Conformance` tests there. The actual tests are
defined in https://github.com/kubernetes-sigs/network-policy-api/tree/master/conformance
and then invoked from this repo. Any changes to the tests first have to be submitted
upstream to `network-policy-api` repo and then brought downstream into the ovn-kubernetes repo
through version bump.

# Documentation Build Check

To catch any potential documentation build breakages which would prevent any docs changes
from being deployed to our GitHub Pages [site](https://github.com/ovn-kubernetes/ovn-kubernetes). The build check will produce the
html docs and will be available in the job artifacts for review. There is a link printed
in the job run logs inside the step "Upload Artifact". Download and unzip that locally 
to view the resulting docs after they are built to see what would be deployed to github
pages.
