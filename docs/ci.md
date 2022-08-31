# CI Tests

For CI, OVN-Kubernetes runs the
[Kubernetes E2E tests](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
and some [locally defined](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e) tests. 
[GitHub Actions](https://help.github.com/en/actions)
are used to run a subset of the Kubernetes E2E tests on each pull request. The
local workflow that controls the test run is located in
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml).

The following tasks are performed:
- Build OVN-Kubernetes
- Check out the Kubernetes source tree and compiles some dependencies
- Install KIND
- Run a matrix of End-To-End Tests using KIND

The following sections should help you understand (and if needed modify) the set of tests that run and how to run these tests locally.

## Understanding the CI Test Suite

The tests are broken into 2 categories, `shard` tests which execute tests from the Kubernetes E2E test suite and the `control-plane` tests which run locally defined tests.

### Shard tests

The shard tests are broken into a set of shards, which is just a grouping of tests,
and each shard is run in a separate job in parallel. Shards execute the `shard-%` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/Makefile).
The set of shards may change in the future. Below is an example of the shards at time of this writing:
- shard-network
  - All E2E tests that match `[sig-network]`
- shard-conformance
  - All E2E tests that match `[Conformance]|[sig-network]`
- shard-test
  - Single E2E test that matches the name of the test specified with a regex. 
  - When selecting the `shard-test` target, you focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

Shards use the [E2E framework](https://kubernetes.io/blog/2019/03/22/kubernetes-end-to-end-testing-for-everyone/). By selecting a specific shard, you modify ginkgo's `--focus` parameter.

The regex expression for determining which E2E test is run in which shard, as
well as the list of skipped tests is defined in
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh).

### Control-plane tests

In addition to the `shard-%` tests, there is also a `control-plane` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/Makefile).
Below is a description of this target:
- control-plane
  - All locally defined tests by default.
  - You can focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

All local tests are run by `make control-plane`. The local tests are controlled in
[ovn-kubernetes/test/scripts/e2e-cp.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-cp.sh)
and the actual tests are defined in the directory
[ovn-kubernetes/test/e2e/](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e).

### Github CI integration through Github Actions Matrix

Each of these shards and control-plane tests can then be run in a [Github Actions matrix](https://docs.github.com/en/actions/learn-github-actions/managing-complex-workflows#using-a-build-matrix) of:
* HA setup (3 masters and 0 workers) and a non-HA setup (1 master and 2 workers)
* Local Gateway Mode and Shared Gateway Mode. See:
[Enable Node-Local Services Access in Shared Gateway Mode](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/shared_gw_dgp.md)
* IPv4 Only, IPv6 Only and Dualstack
* Disabled SNAT Multiple Gateways or Enabled SNAT Gateways
* Single bridge or two bridges

To reduce the explosion of tests being run in CI, the test cases run are limited
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
and set the environmental variable `K8S_VERSION` to the same value. Also make sure to export a GOPATH which points to your go directory with `export GOPATH=(...)`.

```
K8S_VERSION=v1.24.0
git clone --single-branch --branch $K8S_VERSION https://github.com/kubernetes/kubernetes.git $GOPATH/src/k8s.io/kubernetes/
pushd $GOPATH/src/k8s.io/kubernetes/
make WHAT="test/e2e/e2e.test vendor/github.com/onsi/ginkgo/ginkgo cmd/kubectl"
rm -rf .git

sudo cp _output/local/go/bin/e2e.test /usr/local/bin/.
sudo cp _output/local/go/bin/kubectl /usr/local/bin/kubectl-$K8S_VERSION
sudo ln -s /usr/local/bin/kubectl-$K8S_VERSION /usr/local/bin/kubectl
cp _output/local/go/bin/ginkgo /usr/local/bin/.
popd
```

If you have any failures during the build, verify $PATH has been updated to
point to correct GO version. Also may need to change settings on some of the
generated binaries. For example:

```
chmod +x $GOPATH/src/k8s.io/kubernetes/_output/bin/deepcopy-gen
```

### Export environment variables

Before setting up KIND and before running the actual tests, export essential environment variables.

The environment variables and their values depend on the actual test scenario that you want to run.

Look at the `e2e` action (search for `name: e2e`) in [ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml). Prior to installing kind, set the following environment variables according to your needs:
```
export KIND_CLUSTER_NAME=ovn
export KIND_INSTALL_INGRESS=[true|false]
export KIND_ALLOW_SYSTEM_WRITES=[true|false]
export PARALLEL=[true|false]
export JOB_NAME=(... job name ...)
export OVN_HYBRID_OVERLAY_ENABLE=[true|false]
export OVN_MULTICAST_ENABLE=[true|false]
export OVN_EMPTY_LB_EVENTS=[true|false]
export OVN_HA=[true|false]
export OVN_DISABLE_SNAT_MULTIPLE_GWS=[true|false]
export OVN_GATEWAY_MODE=["local"|"shared"]
export KIND_IPV4_SUPPORT=[true|false]
export KIND_IPV6_SUPPORT=[true|false]
# not required for the OVN Kind installation script, but export this already for later
OVN_SECOND_BRIDGE=[true|false]
```

You can refer to a recent CI run from any pull request in [https://github.com/ovn-org/ovn-kubernetes/actions](https://github.com/ovn-org/ovn-kubernetes/actions) to get a valid set of settings.

As an example for the `control-plane-noHA-local-ipv4-snatGW-1br` job, the settings are at time of this writing:
```
export KIND_CLUSTER_NAME=ovn
export KIND_INSTALL_INGRESS=true
export KIND_ALLOW_SYSTEM_WRITES=true
export PARALLEL=true
export JOB_NAME=control-plane-noHA-local-ipv4-snatGW-1br
export OVN_HYBRID_OVERLAY_ENABLE=true
export OVN_MULTICAST_ENABLE=true
export OVN_EMPTY_LB_EVENTS=true
export OVN_HA=false
export OVN_DISABLE_SNAT_MULTIPLE_GWS=false
export OVN_GATEWAY_MODE="local"
export KIND_IPV4_SUPPORT=true
export KIND_IPV6_SUPPORT=false
# not required for the OVN Kind installation script, but export this already for later
export OVN_SECOND_BRIDGE=false
```

### KIND

Kubernetes in Docker (KIND) is used to deploy Kubernetes locally where a docker
container is created per Kubernetes node. The CI tests run on this Kubernetes
deployment. Therefore, KIND will need to be installed locally.

Generic instructions for installing and running OVNKubernetes with KIND can be found at: 
[https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/kind.md](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/kind.md)

Make sure to set the required environment variables first (see section above). Then, deploy kind:
```
$ pushd
$ ./kind.sh
$ popd
```

### Run Tests

To run the tests locally, run a KIND deployment as described above. The E2E
tests look for the kube config file in a special location, so make a copy:

```
cp ~/ovn.conf ~/.kube/kind-config-kind
```

To run the desired shard, first make sure that the necessary environment variables are exported (see section above). Then, go to the location of your local copy of the `ovn-kubernetes` repository:
```
$ REPO=$GOPATH/src/github.com/ovn-org/ovn-kubernetes
$ cd $REPO
```

#### Running a suite of shards or control-plane tests

Finally, run the the shard that you want to test against (each shard can take 30+ minutes to complete)
```
$ pushd test
# run either
$ make shard-network
# or
$ make shard-conformance
# or
$ GITHUB_WORKSPACE="$REPO" make control-plane
$ popd
```

#### Running a single E2E test

To run a single E2E test instead, target the shard-test action, as follows:

```
$ cd $REPO
$ pushd test
$ make shard-test WHAT="should enforce egress policy allowing traffic to a server in a different namespace based on PodSelector and NamespaceSelector"
$ popd
```

As a reminder, shards use the [E2E framework](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md). The value of `WHAT=` will be used to modify the `--focus` parameter. Individual tests can be retrieved from [https://github.com/kubernetes/kubernetes/tree/master/test/e2e](https://github.com/kubernetes/kubernetes/tree/master/test/e2e). For network tests, one could run:
~~~
grep ginkgo.It $GOPATH/src/k8s.io/kubernetes/test/e2e/network/ -Ri
~~~

For example:
~~~
# grep ginkgo.It $GOPATH/src/k8s.io/kubernetes/test/e2e/network/ -Ri | head -1
/root/go/src/k8s.io/kubernetes/test/e2e/network/conntrack.go:	ginkgo.It("should be able to preserve UDP traffic when server pod cycles for a NodePort service", func() {
# make shard-test WHAT="should enforce policy to allow traffic from pods within server namespace based on PodSelector"
(...)
+ case "$SHARD" in
++ echo should be able to preserve UDP traffic when server pod cycles for a NodePort service
++ sed 's/ /\\s/g'
+ FOCUS='should\sbe\sable\sto\spreserve\sUDP\straffic\swhen\sserver\spod\scycles\sfor\sa\sNodePort\sservice'
+ export KUBERNETES_CONFORMANCE_TEST=y
+ KUBERNETES_CONFORMANCE_TEST=y
+ export KUBE_CONTAINER_RUNTIME=remote
+ KUBE_CONTAINER_RUNTIME=remote
+ export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
+ KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
+ export KUBE_CONTAINER_RUNTIME_NAME=containerd
+ KUBE_CONTAINER_RUNTIME_NAME=containerd
+ export FLAKE_ATTEMPTS=5
+ FLAKE_ATTEMPTS=5
+ export NUM_NODES=20
+ NUM_NODES=20
+ export NUM_WORKER_NODES=3
+ NUM_WORKER_NODES=3
+ ginkgo --nodes=20 '--focus=should\sbe\sable\sto\spreserve\sUDP\straffic\swhen\sserver\spod\scycles\sfor\sa\sNodePort\sservice' '--skip=Networking\sIPerf\sIPv[46]|\[Feature:PerformanceDNS\]|Disruptive|DisruptionController|\[sig-apps\]\sCronJob|\[sig-storage\]|\[Feature:Federation\]|should\shave\sipv4\sand\sipv6\sinternal\snode\sip|should\shave\sipv4\sand\sipv6\snode\spodCIDRs|kube-proxy|should\sset\sTCP\sCLOSE_WAIT\stimeout|should\shave\ssession\saffinity\stimeout\swork|named\sport.+\[Feature:NetworkPolicy\]|\[Feature:SCTP\]|service.kubernetes.io/headless|should\sresolve\sconnection\sreset\sissue\s#74839|sig-api-machinery|\[Feature:NoSNAT\]|Services.+(ESIPP|cleanup\sfinalizer)|configMap\snameserver|ClusterDns\s\[Feature:Example\]|should\sset\sdefault\svalue\son\snew\sIngressClass|should\sprevent\sIngress\screation\sif\smore\sthan\s1\sIngressClass\smarked\sas\sdefault|\[Feature:Networking-IPv6\]|\[Feature:.*DualStack.*\]' --flakeAttempts=5 /usr/local/bin/e2e.test -- --kubeconfig=/root/ovn.conf --provider=local --dump-logs-on-failure=false --report-dir=/root/ovn-kubernetes/test/_artifacts --disable-log-dump=true --num-nodes=3
Running Suite: Kubernetes e2e suite
(...)
• [SLOW TEST:28.091 seconds]
[sig-network] Conntrack
/root/go/src/k8s.io/kubernetes/_output/local/go/src/k8s.io/kubernetes/test/e2e/network/framework.go:23
  should be able to preserve UDP traffic when server pod cycles for a NodePort service
  /root/go/src/k8s.io/kubernetes/_output/local/go/src/k8s.io/kubernetes/test/e2e/network/conntrack.go:128
------------------------------
{"msg":"PASSED [sig-network] Conntrack should be able to preserve UDP traffic when server pod cycles for a NodePort service","total":-1,"completed":1,"skipped":229,"failed":0}
Aug 17 14:46:42.842: INFO: Running AfterSuite actions on all nodes


Aug 17 14:46:15.264: INFO: Running AfterSuite actions on all nodes
Aug 17 14:46:42.885: INFO: Running AfterSuite actions on node 1
Aug 17 14:46:42.885: INFO: Skipping dumping logs from cluster


Ran 1 of 5667 Specs in 30.921 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Flaked | 0 Pending | 5666 Skipped


Ginkgo ran 1 suite in 38.489055861s
Test Suite Passed
~~~

#### Running a control-plane test

All local tests are defined as `control-plane` tests. To run a single `control-plane` test, target the `control-plane` action and append the `WHAT=<test name>` parameter, as follows:

```
$ cd $REPO
$ pushd test
$ make control-plane WHAT="should be able to send multicast UDP traffic between nodes"
$ popd
```

The value of `WHAT=` will be used to modify the `-ginkgo.focus` parameter. Individual tests can be retrieved from this repository under [test/e2e](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e). To see a list of individual tests, one could run:
~~~
grep -R ginkgo.It test/
~~~

For example:
~~~
# grep -R ginkgo.It . | head -1
./e2e/multicast.go:	ginkgo.It("should be able to send multicast UDP traffic between nodes", func() {
# make control-plane WHAT="should be able to send multicast UDP traffic between nodes"
(...)
+ go test -timeout=0 -v . -ginkgo.v -ginkgo.focus 'should\sbe\sable\sto\ssend\smulticast\sUDP\straffic\sbetween\snodes' -ginkgo.flakeAttempts 2 '-ginkgo.skip=recovering from deleting db files while maintain connectivity|Should validate connectivity before and after deleting all the db-pods at once in HA mode|Should be allowed to node local cluster-networked endpoints by nodeport services with externalTrafficPolicy=local|e2e ingress to host-networked pods traffic validation|host to host-networked pods traffic validation' -provider skeleton -kubeconfig /root/ovn.conf --num-nodes=2 --report-dir=/root/ovn-kubernetes/test/_artifacts --report-prefix=control-plane_
I0817 15:26:21.762483 1197731 test_context.go:457] Tolerating taints "node-role.kubernetes.io/control-plane" when considering if nodes are ready
=== RUN   TestE2e
I0817 15:26:21.762635 1197731 e2e_suite_test.go:67] Saving reports to /root/ovn-kubernetes/test/_artifacts
Running Suite: E2e Suite
(...)
• [SLOW TEST:12.332 seconds]
Multicast
/root/ovn-kubernetes/test/e2e/multicast.go:25
  should be able to send multicast UDP traffic between nodes
  /root/ovn-kubernetes/test/e2e/multicast.go:75
------------------------------
SSSSSSSSSSSS
JUnit report was created: /root/ovn-kubernetes/test/_artifacts/junit_control-plane_01.xml

Ran 1 of 60 Specs in 12.333 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Flaked | 0 Pending | 59 Skipped
--- PASS: TestE2e (12.34s)
PASS
ok  	github.com/ovn-org/ovn-kubernetes/test/e2e	12.371s
+ popd
~/ovn-kubernetes/test
~~~

### IPv6 tests

To skip the IPv4 only tests (in a IPv6 only deployment), pass the
`KIND_IPV6_SUPPORT=true` environmental variable to `make`:

```
$ cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes

$ pushd test
$ KIND_IPV6_SUPPORT=true make shard-conformance
$ popd
```

Github CI doesn´t offer IPv6 connectivity, so IPv6 only tests are always
skipped. To run those tests locally, comment out the following line from
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh)

```
# Github CI doesn´t offer IPv6 connectivity, so always skip IPv6 only tests.
SKIPPED_TESTS=$SKIPPED_TESTS$IPV6_ONLY_TESTS
```
