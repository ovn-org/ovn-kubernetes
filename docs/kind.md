# OVN kubernetes KIND Setup

KIND (Kubernetes in Docker) deployment of OVN kubernetes is a fast and easy means to quickly install and test kubernetes with OVN kubernetes CNI.  The value proposition is really for developers who want to reproduce an issue or test a fix in an environment that can be brought up locally and within a few minutes.

### Prerequisites 

- 20 GB of free space in root file system
- Docker run time
- [KIND]( https://kubernetes.io/docs/setup/learning-environment/kind/ )
   - Installation instructions can be found at https://github.com/kubernetes-sigs/kind#installation-and-usage. 
   - NOTE: The OVN-Kubernetes [ovn-kubernetes/contrib/kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/contrib/kind.sh) and [ovn-kubernetes/contrib/kind.yaml](https://github.com/ovn-org/ovn-kubernetes/blob/master/contrib/kind.yaml) files provision port 11337. If firewalld is enabled, this port will need to be unblocked:

      ```
      sudo firewall-cmd --permanent --add-port=11337/tcp; sudo firewall-cmd --reload
      ```
- [kubectl]( https://kubernetes.io/docs/tasks/tools/install-kubectl/ )
- Python and pip

**NOTE :**  In certain operating systems such as CentOS 8.x, pip2 and pip3 binaries are installed instead of pip. In such situations create a softlink for "pip" that points to "pip2".

### Run the KIND deployment 

For OVN kubernetes KIND deployment, use the `kind.sh` script.

First Download and build the OVN-Kubernetes repo: 

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
$ ./kind.sh
$ popd
```

This will launch a KIND deployment. By default the cluster is named `ovn`.

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

The `kind.sh` script defaults the cluster to HA. There are numerous
configuration when deploying. Use `./kind.sh -h` to see the latest options.

```
./kind.sh --help

usage: kind.sh [[[-cf|--config-file <file>] [-kt|keep-taint] [-ha|--ha-enabled]
                 [-ii|--install-ingress] [-n4|--no-ipv4] [-i6|--ipv6]] | [-h]]

-cf | --config-file          Name of the KIND J2 configuration file.
                             DEFAULT: ./kind.yaml.j2
-kt | --keep-taint           Do not remove taint components.
                             DEFAULT: Remove taint components.
-ha | --ha-enabled           Enable high availability. DEFAULT: HA Disabled.
-ii | --install-ingress      Flag to install Ingress Components.
                             DEFAULT: Don't install ingress components.
-n4 | --no-ipv4              Disable IPv4. DEFAULT: IPv4 Enabled.
-i6 | --ipv6                 Enable IPv6. DEFAULT: IPv6 Disabled.

```
As seen above if you do not specify any options script will assume the default values.

### Usage Notes 

- You can create your own KIND J2 configuration file if the default one is not sufficient

- You can also specify these values as environment variables. Command line parameters will override the environment variables.

- To tear down the KIND cluster when finished simply run 

   ```
   $ ./kind.sh --delete
   ```

## Running OVN-Kubernetes with IPv6 or Dual-stack In KIND

This section describes the configuration needed for IPv6 and dual-stack environments.

## KIND with IPv6

### Docker Changes For IPv6

For KIND clusters using KIND v0.7.0 or older (CI currently is using v0.8.1), to
use IPv6, IPv6 needs to be enable in Docker on the host:

```
$ sudo vi /etc/docker/daemon.json
{
  "ipv6": true
}

$ sudo systemctl reload docker
```

On a CentOS host running Docker version 19.03.6, the above configuration worked.
After the host was rebooted, Docker failed to start. To fix, change
`daemon.json` as follows:

```
$ sudo vi /etc/docker/daemon.json
{
  "ipv6": true,
  "fixed-cidr-v6": "2001:db8:1::/64"
}

$ sudo systemctl reload docker
```

[IPv6](https://github.com/docker/docker.github.io/blob/c0eb65aabe4de94d56bbc20249179f626df5e8c3/engine/userguide/networking/default_network/ipv6.md)
from Docker repo provided the fix. Newer documentation does not include this
change, so change may be dependent on Docker version.

To verify IPv6 is enabled in Docker, run:

```
$ docker run --rm busybox ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
341: eth0@if342: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1500 qdisc noqueue
    link/ether 02:42:ac:11:00:02 brd ff:ff:ff:ff:ff:ff
    inet 172.17.0.2/16 brd 172.17.255.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 2001:db8:1::242:ac11:2/64 scope global flags 02
       valid_lft forever preferred_lft forever
    inet6 fe80::42:acff:fe11:2/64 scope link tentative
       valid_lft forever preferred_lft forever
```

For the eth0 vEth-pair, there should be the two IPv6 entries (global and link
addresses).

### Disable firewalld

Currently, to run OVN-Kubernetes with IPv6 only in a KIND deployment, firewalld
needs to be disabled. To disable:

```
sudo systemctl stop firewalld
```

NOTE: To run with IPv4, firewalld needs to be enabled, so to reenable:

```
sudo systemctl start firewalld
```

If firewalld is enabled during a IPv6 deployment, additional nodes fail to join
the cluster:

```
:
Creating cluster "ovn" ...
 ✓ Ensuring node image (kindest/node:v1.18.2) 🖼
 ✓ Preparing nodes 📦 📦 📦
 ✓ Writing configuration 📜
 ✓ Starting control-plane 🕹️
 ✓ Installing StorageClass 💾
 ✗ Joining worker nodes 🚜
ERROR: failed to create cluster: failed to join node with kubeadm: command "docker exec --privileged ovn-worker kubeadm join --config /kind/kubeadm.conf --ignore-preflight-errors=all --v=6" failed with error: exit status 1
```

And logs show:

```
I0430 16:40:44.590181     579 token.go:215] [discovery] Failed to request cluster-info, will try again: Get https://[2001:db8:1::242:ac11:3]:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s: dial tcp [2001:db8:1::242:ac11:3]:6443: connect: permission denied
Get https://[2001:db8:1::242:ac11:3]:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s: dial tcp [2001:db8:1::242:ac11:3]:6443: connect: permission denied
```

This issue was reported upstream in KIND
[1257](https://github.com/kubernetes-sigs/kind/issues/1257#issuecomment-575984987)
and blamed on firewalld.

### OVN-Kubernetes With IPv6

To run OVN-Kubernetes with IPv6 in a KIND deployment, run:

```
$ go get github.com/ovn-org/ovn-kubernetes; cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes

$ cd go-controller/
$ make

$ cd ../dist/images/
$ make fedora

$ cd ../../contrib/
$ KIND_IPV4_SUPPORT=false KIND_IPV6_SUPPORT=true ./kind.sh
```

Once `kind.sh` completes, setup kube config file:

```
$ cp ~/admin.conf ~/.kube/config
-- OR --
$ KUBECONFIG=~/admin.conf
```

Once testing is complete, to tear down the KIND deployment:

```
$ kind delete cluster --name ovn
```

## KIND with Dual-stack

Currently, IP dual-stack is not fully supported in:
* Kubernetes
* KIND
* OVN-Kubernetes

### Kubernetes And Docker With IP Dual-stack

#### Update kubectl

Kubernetes has some IP dual-stack support but the feature is not complete.
Additional changes are constantly being added. This setup is using the latest
Kubernetes release to test against. Kubernetes is being installed below using
OVN-Kubernetes KIND script, however to test, an equivalent version of `kubectl`
needs to be installed.

First determine what version of `kubectl` is currently being used and save it:

```
$ which kubectl
/usr/bin/kubectl
$ kubectl version --client
Client Version: version.Info{Major:"1", Minor:"17", GitVersion:"v1.17.3", GitCommit:"06ad960bfd03b39c8310aaf92d1e7c12ce618213", GitTreeState:"clean", BuildDate:"2020-02-11T18:14:22Z", GoVersion:"go1.13.6", Compiler:"gc", Platform:"linux/amd64"}
sudo mv /usr/bin/kubectl /usr/bin/kubectl-v1.17.3
sudo ln -s /usr/bin/kubectl-v1.17.3 /usr/bin/kubectl
```

Download and install latest version of `kubectl`:

```
$ K8S_VERSION=v1.18.0
$ curl -LO https://storage.googleapis.com/kubernetes-release/release/$K8S_VERSION/bin/linux/amd64/kubectl
$ chmod +x kubectl
$ sudo mv kubectl /usr/bin/kubectl-v1.18.0
$ sudo rm /usr/bin/kubectl
$ sudo ln -s /usr/bin/kubectl-v1.18.0 /usr/bin/kubectl
$ kubectl version --client
Client Version: version.Info{Major:"1", Minor:"18", GitVersion:"v1.18.0", GitCommit:"9e991415386e4cf155a24b1da15becaa390438d8", GitTreeState:"clean", BuildDate:"2020-03-25T14:58:59Z", GoVersion:"go1.13.8", Compiler:"gc", Platform:"linux/amd64"}
```

### Docker Changes For Dual-stack

For dual-stack, IPv6 needs to be enable in Docker on the host same as
for IPv6 only. See above: [Docker Changes For IPv6](#docker-changes-for-ipv6)

### KIND With IP Dual-stack

IP dual-stack is not currently supported in KIND. There is a PR
([692](https://github.com/kubernetes-sigs/kind/pull/692))
with IP dual-stack changes. Currently using this to test with.

Optionally, save previous version of KIND (if it exists):

```
cp $GOPATH/bin/kind $GOPATH/bin/kind.orig
```

#### Build KIND With Dual-stack Locally

To build locally (if additional needed):

```
go get github.com/kubernetes-sigs/kind; cd $GOPATH/src/github.com/kubernetes-sigs/kind
git pull --no-edit --strategy=ours origin pull/692/head
make clean
make install INSTALL_DIR=$GOPATH/bin
```

### OVN-Kubernetes With IP Dual-stack

For status of IP dual-stack in OVN-Kubernetes, see
[1142](https://github.com/ovn-org/ovn-kubernetes/issues/1142).

To run OVN-Kubernetes with IP dual-stack in a KIND deployment, run:

```
$ go get github.com/ovn-org/ovn-kubernetes; cd $GOPATH/src/github.com/ovn-org/ovn-kubernetes

$ cd go-controller/
$ make

$ cd ../dist/images/
$ make fedora

$ cd ../../contrib/
$ KIND_IPV4_SUPPORT=true KIND_IPV6_SUPPORT=true K8S_VERSION=v1.18.0 ./kind.sh
```

Once `kind.sh` completes, setup kube config file:

```
$ cp ~/admin.conf ~/.kube/config
-- OR --
$ KUBECONFIG=~/admin.conf
```

Once testing is complete, to tear down the KIND deployment:

```
$ kind delete cluster --name ovn
```

### Current Status

This is subject to change because code is being updated constantly. But this is
more a cautionary note that this feature is not completely working at the
moment.

The nodes do not go to ready because the OVN-Kubernetes hasn't setup the network
completely:

```
$ kubectl get nodes
NAME                STATUS     ROLES    AGE   VERSION
ovn-control-plane   NotReady   master   94s   v1.18.0
ovn-worker          NotReady   <none>   61s   v1.18.0
ovn-worker2         NotReady   <none>   62s   v1.18.0

$ kubectl get pods -o wide --all-namespaces
NAMESPACE          NAME                                      READY STATUS   RESTARTS AGE    IP          NODE
kube-system        coredns-66bff467f8-hh4c9                  0/1   Pending  0        2m45s  <none>      <none>
kube-system        coredns-66bff467f8-vwbcj                  0/1   Pending  0        2m45s  <none>      <none>
kube-system        etcd-ovn-control-plane                    1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-apiserver-ovn-control-plane          1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-controller-manager-ovn-control-plane 1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-scheduler-ovn-control-plane          1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
local-path-storage local-path-provisioner-774f7f8fdb-msmd2   0/1   Pending  0        2m45s  <none>      <none>
ovn-kubernetes     ovnkube-db-cf4cc89b7-8d4xq                2/2   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-master-87fb56d6d-7qmnb            3/3   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-278l9                        2/3   Running  0        107s   172.17.0.3  ovn-worker2
ovn-kubernetes     ovnkube-node-bm7v6                        2/3   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-p4k4t                        2/3   Running  0        107s   172.17.0.4  ovn-worker
```

### Known issues

Some environments (Fedora32,31 on desktop), have problems when the cluster
is deleted directly with kind `kind delete cluster --name ovn`, it restarts the host.
The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.

You can use the following command to delete the cluster:

`contrib/kind.sh --delete`