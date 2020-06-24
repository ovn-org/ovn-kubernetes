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

Each of these shards is run in a HA setup and a non-HA setup. The regex
expression for determining which E2E test is run in which shard, as well as the
list of skipped tests is defined in
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh).

The local tests are controlled in
[test/scripts/e2e-cp.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-cp.sh)
and the actual tests are defined in the directory
[ovn-kubernetes/test/e2e/](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e).

## Running CI Locally

This section describes how to run CI tests on a local deployment. This may be
useful for expanding the CI test coverage, or testing a private fix before
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

### Install KIND

Kubernetes in Docker (KIND) is used to deploy Kubernetes locally where a docker
container is created per Kubernetes node. The CI tests run on this Kubernetes
deployment. Therefore, KIND will need to be installed locally.

Installation instructions can be found at:
https://github.com/kubernetes-sigs/kind#installation-and-usage.

NOTE: The OVN-Kubernetes 
[ovn-kubernetes/contrib/kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/contrib/kind.sh)
and
[ovn-kubernetes/contrib/kind.yaml](https://github.com/ovn-org/ovn-kubernetes/blob/master/contrib/kind.yaml)
files provision port 11337. If firewalld is enabled, this port will need to be
unblocked:

```
sudo firewall-cmd --permanent --add-port=11337/tcp; sudo firewall-cmd --reload
```

### Run KIND Deployment from OVN-Kubernetes

To launch the KIND deployment, the `kind.sh` script must be run out of the
OVN-Kubernetes repo. Download and build the OVN-Kubernetes repo:

```
$ go get github.com/ovn-org/ovn-kubernetes; cd GOPATH/src/github.com/ovn-org/ovn-kubernetes
```

The `kind.sh` script builds OVN-Kubernetes into a container image. To verify
local changes before building in KIND, run the following:

```
$ pushd go-controller
$ make
$ popd

$ pushd dist/images
$ make fedora
$ popd
```

Launch the KIND Deployment.

```
$ pushd contrib
$ KUBECONFIG=${HOME}/admin.conf
$ KIND_INSTALL_INGRESS=true ./kind.sh
$ popd
```

This will launch a KIND deployment. The `kind.sh` script defaults the cluster
name to `ovn`. Also by default, the `kind.sh` script defaults the cluster to HA.
Use `KIND_HA=false` to run in non-HA mode.

```
$ kubectl get nodes
NAME                STATUS   ROLES    AGE     VERSION
ovn-control-plane   Ready    master   5h13m   v1.16.4
ovn-worker          Ready    <none>   5h12m   v1.16.4
ovn-worker2         Ready    <none>   5h12m   v1.16.4

$ kubectl get pods --all-namespaces
NAMESPACE            NAME                                        READY   STATUS    RESTARTS   AGE
kube-system          coredns-5644d7b6d9-kw2xc                    1/1     Running   0          5h13m
kube-system          coredns-5644d7b6d9-sd9wh                    1/1     Running   0          5h13m
kube-system          etcd-ovn-control-plane                      1/1     Running   0          5h11m
kube-system          kube-apiserver-ovn-control-plane            1/1     Running   0          5h12m
kube-system          kube-controller-manager-ovn-control-plane   1/1     Running   0          5h12m
kube-system          kube-scheduler-ovn-control-plane            1/1     Running   0          5h11m
local-path-storage   local-path-provisioner-7745554f7f-9r8dz     1/1     Running   0          5h13m
ovn-kubernetes       ovnkube-db-5588bd699c-kb8h7                 2/2     Running   0          5h11m
ovn-kubernetes       ovnkube-master-6f44d456df-bv2x8             3/3     Running   0          5h11m
ovn-kubernetes       ovnkube-node-2t6m2                          3/3     Running   0          5h11m
ovn-kubernetes       ovnkube-node-hhsmk                          3/3     Running   0          5h11m
ovn-kubernetes       ovnkube-node-xvqh4                          3/3     Running   0          5h11m
```

Once the testing is complete, to tear down the KIND deployment:

```
$ kind delete cluster --name ovn
```

### Run Tests

To run the tests locally, run a KIND deployment as described above
(`KIND_HA=false` or `KIND_HA=true` as desired). The E2E tests look for the kube
config file in a special location, so make a copy:

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
