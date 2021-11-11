# CI Tests

For CI, OVN-Kubernetes runs the
[Kubernetes E2E tests](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
and some locally defined tests. 
[GitHub Actions](https://help.github.com/en/actions)
are used to run a subset of the Kubernetes E2E tests on each pull request. The
local workflow that controls the test run is located in
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml).

The following tasks are performed:
- Builds OVN-Kubernetes
- Checks out the Kubernetes source tree and compiles some dependencies
- Installs KIND
- Runs a matrix of End-To-End Tests using KIND

Below describes how to update the set of tests that run and how to run these
tests locally.

## Updating CI Test Suite

The tests are broken into a set of shards, which is just a grouping of tests,
and each shard is run in a separate job in parallel. Below is an example of
the shards (which may change in the future):
- shard-n-other
  - All E2E tests that match `[sig-network] N` and do NOT have P as their sixth
  letter after the N (i.e. Roughly all `[sig-network] Networking ...` tests.), and all other tests
  that don't match the rule below
- shard-np
  - All E2E tests that match `[sig-network] N` and DO have a P as their sixth
  letter after the N. (i.e. Roughly all `[sig-network] NetworkPolicy ...`
  tests.)
- shard-test
  - Single E2E test that matches the name of the test specified with a regex. See bottom of this document for an example.
- control-plane
  - All locally defined tests.

The regex expression for determining which E2E test is run in which shard, as
well as the list of skipped tests is defined in
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh). The local tests are controlled in
[ovn-kubernetes/test/scripts/e2e-cp.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-cp.sh)
and the actual tests are defined in the directory
[ovn-kubernetes/test/e2e/](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e).

Each of these shards can then be run in a matrix of:
* HA setup (3 masters and 0 workers) and a non-HA setup (1 master and 2 workers)
* Local Gateway Mode and Shared Gateway Mode. See:
[Enable Node-Local Services Access in Shared Gateway Mode](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/shared_gw_dgp.md)
* IPv4 Only and IPv6 Only

To reduce the explosion of tests being run in CI, the test case run are limited
using an `exclude:` statement in 
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml).

## Running CI Locally

This section describes how to run CI tests on a local deployment. This may be
useful for expanding the CI test coverage or testing a private fix before
creating a pull request.

### Download and Build Kubernetes Components

#### Go Version

Older versions of Kubernetes do not build with newer versions of Go,
specifically Kubernetes v1.16.4 doesn't build with Go version 1.13.x. If this is
a version of Kubernetes that needs to be tested with, as a workaround, Go
version 1.12.1 can be downloaded to a local directory and the $PATH variable
updated only where kubernetes is being built. 

```
$ go version
go version go1.13.8 linux/amd64

$ mkdir -p /home/$USER/src/golang/go1-12-1/; cd /home/$USER/src/golang/go1-12-1/
$ wget https://dl.google.com/go/go1.12.1.linux-amd64.tar.gz
$ tar -xzf go1.12.1.linux-amd64.tar.gz
$ PATH=/home/$USER/src/golang/go1-12-1/go/bin:$GOPATH/src/k8s.io/kubernetes/_output/local/bin/linux/amd64:$PATH
```

#### Download and Build Kubernetes Components (E2E Tests, ginkgo, kubectl):

Determine which version of Kubernetes is currently used in CI (See
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml))
and set the environmental variable `K8S_VERSION` to the same value.

```
K8S_VERSION=v1.17.2
git clone --single-branch --branch $K8S_VERSION https://github.com/kubernetes/kubernetes.git $GOPATH/src/k8s.io/kubernetes/
pushd $GOPATH/src/k8s.io/kubernetes/
make WHAT="test/e2e/e2e.test vendor/github.com/onsi/ginkgo/ginkgo cmd/kubectl"
rm -rf .git

cp _output/local/go/bin/e2e.test $GOPATH/bin/.

sudo cp /usr/bin/kubectl /usr/bin/kubectl.bak
sudo cp _output/local/go/bin/kubectl /usr/bin/kubectl-$K8S_VERSION
sudo ln -s /usr/bin/kubectl-$K8S_VERSION /usr/bin/kubectl
popd
```

If you have any failures during the build, verify $PATH has been updated to
point to correct GO version. Also may need to change settings on some of the
generated binaries. For example:

```
chmod +x $GOPATH/src/k8s.io/kubernetes/_output/bin/deepcopy-gen
```

### KIND

Kubernetes in Docker (KIND) is used to deploy Kubernetes locally where a docker
container is created per Kubernetes node. The CI tests run on this Kubernetes
deployment. Therefore, KIND will need to be installed locally.

Instructions for installing and running KIND can be found at: 
https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/kind.md

### Run Tests

To run the tests locally, run a KIND deployment as described above. The E2E
tests look for the kube config file in a special location, so make a copy:

```
cp ~/admin.conf ~/.kube/kind-config-kind
```

To run the desired shard, use the following (each shard can take +30 mins to
run):

```
$ cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes

$ pushd test
$ make shard-n-other
$ make shard-np
$ GITHUB_WORKSPACE=$GOPATH/src/github.com/ovn-org/ovn-kubernetes make control-plane
$ popd
```

To run a single test instead, target the shard-test action, as follows:

```
$ cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes
$ pushd test
$ make shard-test WHAT="should enforce egress policy allowing traffic to a server in a different namespace based on PodSelector and NamespaceSelector"
$ popd
```

To skip the IPv4 only tests (in a IPv6 only deployment), pass the
`KIND_IPV6_SUPPORT=true` environmental variable to `make`:

```
$ cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes

$ pushd test
$ KIND_IPV6_SUPPORT=true make shard-n-other
$ popd
```

Github CI doesn´t offer IPv6 connectivity, so IPv6 only tests are always
skipped. To run those tests locally, comment out the following line from
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh)

```
# Github CI doesn´t offer IPv6 connectivity, so always skip IPv6 only tests.
SKIPPED_TESTS=$SKIPPED_TESTS$IPV6_ONLY_TESTS
```
