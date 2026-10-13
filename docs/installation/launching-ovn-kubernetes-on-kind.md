# OVN-Kubernetes KinD Setup

KinD (Kubernetes in Docker) deployment of OVN kubernetes is a fast and easy means to quickly install and test kubernetes with OVN kubernetes CNI. The value proposition is really for developers who want to reproduce an issue or test a fix in an environment that can be brought up locally and within a few minutes.

### Prerequisites

- 20 GB of free space in root file system
- Docker runtime or Podman
- [KinD](https://kubernetes.io/docs/setup/learning-environment/kind/)
   - Installation instructions can be found at https://github.com/kubernetes-sigs/kind#installation-and-usage. 
   - NOTE: The [ovn-kubernetes/contrib/kind.sh](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.sh) and [ovn-kubernetes/contrib/kind.yaml.j2](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.yaml.j2) files provision port 11337. If firewalld is enabled, this port will need to be unblocked:

      ```
      sudo firewall-cmd --permanent --add-port=11337/tcp; sudo firewall-cmd --reload
      ```
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/)
- [Helm](https://helm.sh/docs/intro/install/) v3
- Python 3 and [pipx](https://pipx.pypa.io/stable/installation/)
- jq
- openssl
- openvswitch
- Go 1.23.0 or above
- For podman users: skopeo

For OVN kubernetes KinD deployment, use the `kind.sh` script (a symlink to `kind-helm.sh`, which deploys OVN-Kubernetes via Helm).

First download and build the OVN-Kubernetes repo:

```shell
git clone https://github.com/ovn-kubernetes/ovn-kubernetes.git
cd ovn-kubernetes
```
The `kind.sh` script builds OVN-Kubernetes into a container image. To verify
local changes before building in KIND, run the following:

```
$ pushd go-controller
$ make
$ popd
```

### Run the KIND deployment with docker

Build the image for fedora and launch the KIND Deployment

```
$ pushd dist/images
$ make fedora-image
$ popd

$ pushd contrib
$ export KUBECONFIG=${HOME}/ovn.conf
$ ./kind.sh
$ popd
```

### Run the KIND deployment with podman

To verify local changes, the steps are mostly the same as with docker, except the `fedora` make target:

```
$ pushd dist/images
```

Then Edit the makefile, changing 

```
$ OCI_BIN=podman
```

Then build,

```
$ make fedora-image
$ popd
```

To deploy KIND however, you need to start it as root and then copy root's kube config to use it as non-root:

```
$ pushd contrib
$ sudo ./kind.sh -ep podman
$ mkdir -p ~/.kube
$ sudo cp /root/ovn.conf ~/.kube/kind-config
$ sudo chown $(id -u):$(id -g) ~/.kube/kind-config
$ export KUBECONFIG=~/.kube/kind-config
$ popd
```

**NOTE:** If you installed go via the official path on Linux and have encountered the "go: command not found" issue, you can preserve your environment when doing sudo: `sudo --preserve-env=PATH ./kind.sh -ep podman`

This will launch a KIND deployment. By default, the cluster is named `ovn`.

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
ovn-kubernetes       ovnkube-control-plane-6f44d456df-bv2x8      1/1     Running   0          5h11m
ovn-kubernetes       ovnkube-node-2t6m2                          3/3     Running   0          5h11m
ovn-kubernetes       ovnkube-node-hhsmk                          3/3     Running   0          5h11m
ovn-kubernetes       ovnkube-node-xvqh4                          3/3     Running   0          5h11m
ovn-kubernetes       ovs-node-8r77t                              1/1     Running   0          5h11m
ovn-kubernetes       ovs-node-jl7lh                              1/1     Running   0          5h11m
ovn-kubernetes       ovs-node-s9gxp                              1/1     Running   0          5h11m
```

The `kind.sh` script defaults the cluster to HA disabled. There are numerous
configuration options when deploying. Use `./kind.sh -h` to see the latest options.

```text
[root@ovnkubernetes contrib]# ./kind.sh --help
usage: kind-helm.sh [--delete]
       [ -cf  | --config-file <file> ]
       [ -kt  | --keep-taint ]
       [ -me  | --multicast-enabled ]
       [ -ho  | --hybrid-enabled ]
       [ -el  | --ovn-empty-lb-events ]
       [ -ii  | --install-ingress ]
       [ -mlb | --install-metallb ]
       [ -pl  | --install-cni-plugins ]
       [ -ikv | --install-kubevirt ]
       [ -mne | --multi-network-enable ]
       [ -nse | --network-segmentation-enable ]
       [ -nce | --network-connect-enable ]
       [ -uae | --preconfigured-udn-addresses-enable ]
       [ -rae | --route-advertisements-enable ]
       [ -evpn | --evpn-enable ]
       [-dudn | --dynamic-udn-allocation]
       [-dug | --dynamic-udn-removal-grace-period]
       [-adv | --advertise-default-network]
       [-rud | --routed-udn-isolation-disable]
       [ -nqe | --network-qos-enable ]
       [ -noe | --no-overlay-enable [snat-enabled|managed] ]
       [ -n4  | --no-ipv4 ]
       [ -i6  | --ipv6 ]
       [ -wk  | --num-workers <num> ]
       [ -ov  | --ovn-image <image> ]
       [ -ovr | --ovn-repo <repo> ]
       [ -ovg | --ovn-gitref <ref> ]
       [ -cn  | --cluster-name ]
       [ -mip | --metrics-ip <ip> ]
       [ -mtu <mtu> ]
       [ --enable-coredumps ]
       [ -h ]

--delete                                      Delete current cluster
-cf  | --config-file                          Name of the KIND configuration file
-kt  | --keep-taint                           Do not remove taint components
                                              DEFAULT: Remove taint components
-me  | --multicast-enabled                    Enable multicast. DEFAULT: Disabled
-ho  | --hybrid-enabled                       Enable hybrid overlay. DEFAULT: Disabled
-obs | --observability                        Enable observability. DEFAULT: Disabled
-upm | --unprivileged-mode                    Run ovnkube-node without privileged: true.
                                              Installs the OVS client tools (ovs-vsctl, ovs-ofctl) on the
                                              kind nodes, which the CNI plugin needs in this mode.
                                              DEFAULT: Disabled
-el  | --ovn-empty-lb-events                  Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled
-ii  | --install-ingress                      Flag to install Ingress Components.
                                              DEFAULT: Don't install ingress components.
-mlb | --install-metallb                      Install metallb to test service type LoadBalancer deployments
-pl  | --install-cni-plugins                  Install CNI plugins
-ikv | --install-kubevirt                     Install kubevirt
-mne | --multi-network-enable                 Enable multi networks. DEFAULT: Disabled
-nse | --network-segmentation-enable          Enable network segmentation. DEFAULT: Disabled
-nce | --network-connect-enable               Enable network connect (requires network segmentation). DEFAULT: Disabled
-uae | --preconfigured-udn-addresses-enable   Enable connecting workloads with preconfigured network to user-defined networks. DEFAULT: Disabled
-rae | --route-advertisements-enable          Enable route advertisements
-evpn | --evpn-enable                         Enable EVPN
-dudn | --dynamic-udn-allocation              Enable dynamic UDN allocation. DEFAULT: Disabled
-dug | --dynamic-udn-removal-grace-period     Configure the grace period in seconds for dynamic UDN removal. DEFAULT: 120 seconds
-adv | --advertise-default-network            Applies a RouteAdvertisements configuration to advertise the default network on all nodes
-rud | --routed-udn-isolation-disable         Disable isolation across BGP-advertised UDNs (sets advertised-udn-isolation-mode=loose). DEFAULT: strict.
-nqe | --network-qos-enable                   Enable network QoS. DEFAULT: Disabled
-noe | --no-overlay-enable [snat-enabled|managed] Enable no overlay for the default network. Optional value: 'snat-enabled' to enable SNAT, 'managed' to enable SNAT and managed routing. DEFAULT: disabled.
-ds  | --disable-snat-multiple-gws            Disable SNAT for multiple external gateways. DEFAULT: Enabled
-df  | --disable-forwarding                   Disable forwarding on all interfaces. DEFAULT: Enabled
--disable-ovnkube-identity                    Disable per-node cert and ovnkube-identity webhook. DEFAULT: Enabled
-dgb | --dummy-gateway-bridge                 Use a dummy instead of a real gateway bridge. DEFAULT: Disabled
-gm  | --gateway-mode                         Configure the cluster gateway mode (local|shared). DEFAULT: shared
-n4  | --no-ipv4                              Disable IPv4. DEFAULT: IPv4 Enabled.
-i6  | --ipv6                                 Enable IPv6. DEFAULT: IPv6 Disabled.
-wk  | --num-workers                          Number of worker nodes. DEFAULT: 2 workers
-ov  | --ovn-image                            Use the specified docker image instead of building locally. DEFAULT: local build.
-ovr | --ovn-repo                             Specify the repository to build OVN from
-ovg | --ovn-gitref                           Specify the branch, tag or commit id to build OVN from, it can be a pattern like 'branch-*' it will order results and use the first one
-cn  | --cluster-name                         Configure the kind cluster's name
-mip | --metrics-ip                           IP address to bind metrics endpoints. DEFAULT: K8S_NODE_IP or 0.0.0.0
-mtu                                          Define the overlay mtu. DEFAULT: 1400 (1500 for no-overlay mode)
--enable-coredumps                            Enable coredump collection on kind nodes. DEFAULT: Disabled
-dns | --enable-dnsnameresolver               Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall.
-mps | --multi-pod-subnet                     Use multiple subnets for the default cluster network
--allow-icmp-netpol                           Allows ICMP and ICMPv6 traffic globally, regardless of network policy rules
-ecp | --encap-port                           GENEVE UDP tunnel port.
-dp  | --disable-pkt-mtu-check                Disable checking for packets mtu size. DEFAULT: false
-is  | --ipsec                                Enable IPsec. DEFAULT: false
-sm  | --scale-metrics                        Enable scale metrics. DEFAULT: false
-ehp | --egress-ip-healthcheck-port           TCP port used for gRPC session by egress IP node check. DEFAULT: 9107 (Use "0" for legacy dial to port 9).
-nf  | --netflow-targets                      A comma-separated set of NetFlow collectors to export flow data. DEFAULT: Disabled
-sf  | --sflow-targets                        A comma-separated set of SFlow collectors to export flow data. DEFAULT: Disabled
-if  | --ipfix-targets                        A comma-separated set of IPFIX collectors to export flow data. DEFAULT: Disabled
-ifs | --ipfix-sampling                       Rate at which packets should be sampled and sent to each target collector. DEFAULT: 400
-ifm | --ipfix-cache-max-flows                Maximum number of IPFIX flow records that can be cached at a time. DEFAULT: 0 (disabled)
-ifa | --ipfix-cache-active-timeout           Maximum period in seconds for which an IPFIX flow record is cached. DEFAULT: 60
-lcl | --libovsdb-client-logfile              Separate logs for libovsdb client into provided file. DEFAULT: do not separate.
-eb  | --egress-gw-separate-bridge            The external gateway traffic uses a separate bridge (sets up xgw bridge and eth1).
-lr  | --local-kind-registry                  Configure kind to use a local container registry for images.
-ep  | --experimental-provider                Use an experimental OCI provider such as podman instead of docker.
--deploy                                      Deploy ovn-kubernetes without restarting kind
--add-nodes                                   Adds nodes to an existing cluster. Number of nodes set by --num-workers.
--isolated                                    After cluster creation, remove default route from nodes and publish kind node IPs as /etc/hosts entries for DNS-less isolation.
-ml  | --master-loglevel                      Log level for ovnkube-control-plane/cluster-manager pods (0..5). DEFAULT: 4
-nl  | --node-loglevel                        Log level for ovnkube-node pods (0..5). DEFAULT: 4
-nbl | --ovn-loglevel-nb                      Log level for ovn-nbdb. DEFAULT: '-vconsole:info -vfile:info'
-sbl | --ovn-loglevel-sb                      Log level for ovn-sbdb. DEFAULT: '-vconsole:info -vfile:info'
-ndl | --ovn-loglevel-northd                  Log level for ovn-northd. DEFAULT: '-vconsole:info -vfile:info'
-cl  | --ovn-loglevel-controller              Log level for ovn-controller. DEFAULT: '-vconsole:info'
-dd  | --dns-domain                           Configure a custom dnsDomain for k8s services. DEFAULT: 'cluster.local'
-inf | --num-infra                            Number of infra (tainted, not-ready) kind nodes. DEFAULT: 0
-hns | --host-network-namespace               Namespace used to classify host-network traffic. DEFAULT: 'ovn-host-network'
-prom | --install-prometheus                  Install Prometheus monitoring stack.
-sw  | --allow-system-writes                  Allow the script to write to /etc/hosts and other system files when needed.
-ric | --run-in-container                     Run the script from inside a docker container (adapts kubeconfig API URL).
-kc  | --kubeconfig                           Output kubeconfig path. DEFAULT: $HOME/$KIND_CLUSTER_NAME.conf
-nokvipam | --opt-out-kv-ipam                 Skip installing the KubeVirt IPAM controller (requires --install-kubevirt).
```

As seen above, if you do not specify any options the script will assume the default values.

**Notes / troubleshooting:**

- Issue with /dev/dma_heap: if you get the error `kind "Error: open /dev/dma_heap: permission denied"`, there's a [known issue](https://bugzilla.redhat.com/show_bug.cgi?id=1966158) about it (directory mislabelled with selinux).
Workaround:

```bash
sudo setenforce 0
sudo chcon system\_u:object\_r:device\_t:s0 /dev/dma\_heap/
sudo setenforce 1
```

- If you see errors related to go, you may not have go `$PATH` configured as root. Make sure it is configured, or define it while running `kind.sh`:

```bash
sudo PATH=$PATH:/usr/local/go/bin ./kind.sh -ep podman
```

### Unprivileged ovnkube-node

`./kind.sh -upm` deploys ovnkube-node without `privileged: true`, with only
`NET_ADMIN` added to its capabilities. In this mode ovnkube-node only hands
the CNI plugin, `ovn-k8s-cni-overlay` in `/opt/cni/bin`, the pod's addresses,
and the plugin running on the node creates the veth, plugs it into `br-int`
and waits for the OVN binding itself. For that the plugin runs `ovs-vsctl`
and `ovs-ofctl` from the node's `PATH`, talking to the `ovs-node` pod through
the sockets it exposes in `/var/run/openvswitch`.

The container's own `/proc/sys` is read-only without `privileged: true`, so
the chart mounts the host's `/proc/sys/net` at `/host/proc/sys/net` and
ovnkube-node writes its per-interface sysctls (forwarding, `rp_filter`,
`keep_addr_on_down`) there. The pod runs in the host network namespace, so
these are the same entries, and `NET_ADMIN` is all the kernel requires.

Kubelet probes to primary user defined network pods are not supported in
this mode: the nftables rule that lets kubelet through the host isolation
can only be loaded from the host cgroup namespace. The node reports
`UDNKubeletProbesNotSupported`, and the e2e tests skip their kubelet probe
assertions. See the
[UDN documentation](../features/user-defined-networks/user-defined-networks.md#when-kubelet-probes-are-not-supported)
for the workarounds.

The kind node image does not ship these tools, so the script installs the
`openvswitch-switch` package on every node before deploying OVN-Kubernetes.
Only `-upm` strictly needs it, but the tools are installed for every
deployment because they make debugging OVS from the node easier. The
package's systemd units are masked first, so the node never starts OVS
daemons of its own; only the client binaries are used. Nodes that already
have both binaries are left untouched.

On a non-kind cluster OVS usually runs on the host.

### Usage Notes 

- You can create your own KIND J2 configuration file if the default one is not sufficient

- You can also specify these values as environment variables. Command line parameters will override the environment variables.

- To tear down the KinD cluster when finished, simply run:

   ```shell
   $ ./kind.sh --delete
   ```

## Running OVN-Kubernetes with IPv6 or Dual-stack in KinD

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
$ go get github.com/ovn-kubernetes/ovn-kubernetes; cd $GOPATH/src/github.com/ovn-kubernetes/ovn-kubernetes

$ cd go-controller/
$ make

$ cd ../dist/images/
$ make fedora-image

$ cd ../../contrib/
$ PLATFORM_IPV4_SUPPORT=false PLATFORM_IPV6_SUPPORT=true ./kind.sh
```

Once `kind.sh` completes, setup kube config file:

```
$ cp ~/ovn.conf ~/.kube/config
-- OR --
$ KUBECONFIG=~/ovn.conf
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
Client Version: version.Info{Major:"1", Minor:"28", GitVersion:"v1.17.3", GitCommit:"06ad960bfd03b39c8310aaf92d1e7c12ce618213", GitTreeState:"clean", BuildDate:"2020-02-11T18:14:22Z", GoVersion:"go1.13.6", Compiler:"gc", Platform:"linux/amd64"}
sudo mv /usr/bin/kubectl /usr/bin/kubectl-v1.17.3
sudo ln -s /usr/bin/kubectl-v1.17.3 /usr/bin/kubectl
```

Download and install latest version of `kubectl`:

```
$ K8S_VERSION=v1.36.4
$ curl -LO https://storage.googleapis.com/kubernetes-release/release/$K8S_VERSION/bin/linux/amd64/kubectl
$ chmod +x kubectl
$ sudo mv kubectl /usr/bin/kubectl-$K8S_VERSION
$ sudo rm /usr/bin/kubectl
$ sudo ln -s /usr/bin/kubectl-$K8S_VERSION /usr/bin/kubectl
$ kubectl version --client
Client Version: v1.32.3
Kustomize Version: v5.0.4-0.20230601165947-6ce0bf390ce3
```

### Docker Changes For Dual-stack

For dual-stack, IPv6 needs to be enable in Docker on the host same as
for IPv6 only. See above: [Docker Changes For IPv6](#docker-changes-for-ipv6)

### KinD With IP Dual-stack

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
[1142](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/1142).

To run OVN-Kubernetes with IP dual-stack in a KIND deployment, run:

```
$ go get github.com/ovn-kubernetes/ovn-kubernetes; cd $GOPATH/src/github.com/ovn-kubernetes/ovn-kubernetes

$ cd go-controller/
$ make

$ cd ../dist/images/
$ make fedora-image

$ cd ../../contrib/
$ PLATFORM_IPV4_SUPPORT=true PLATFORM_IPV6_SUPPORT=true K8S_VERSION=v1.36.4 ./kind.sh
```

Once `kind.sh` completes, setup kube config file:

```
$ cp ~/ovn.conf ~/.kube/config
-- OR --
$ KUBECONFIG=~/ovn.conf
```

Once testing is complete, to tear down the KIND deployment:

```
$ kind delete cluster --name ovn
```

### Using specific KinD container image and tag

:warning: Use with caution, as KinD expects this image to have all it needs.

In order to use an image/tag other than the default hardcoded in `kind.sh`, specify
one (or both of) the following variables:

```
$ cd ../../contrib/
$ KIND_IMAGE=example.com/kindest/node K8S_VERSION=v1.36.4 ./kind.sh
```

### Using KinD local registry to deploy non ovn-k containers

A local registry can be made available to the cluster if started with:
```
./kind.sh --local-kind-registry
```
This is useful if you want to make your own local images available to the 
cluster. These images can be pushed, fetched or used 
in manifests using the prefix `localhost:5000`.

### Loading ovn-kubernetes changes without restarting KinD

Sometimes it is useful to update ovn-kubernetes without redeploying the whole 
cluster all over again. For example, when testing the update itself. 
This can be achieve with the "--deploy" flag:

```bash
# Default options will use KinD mechanism to push images directly to the
./kind.sh --deploy

# Using a local registry is an alternative to deploy ovn-kubernetes updates 
# while also being useful to deploy other local images
./kind.sh --deploy --local-kind-registry
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
ovn-kubernetes     ovnkube-control-plane-87fb56d6d-7qmnb     1/1   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-278l9                        2/3   Running  0        107s   172.17.0.3  ovn-worker2
ovn-kubernetes     ovnkube-node-bm7v6                        2/3   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-p4k4t                        2/3   Running  0        107s   172.17.0.4  ovn-worker
ovn-kubernetes     ovs-node-8r77t                            1/1   Running  0        107s   172.17.0.3  ovn-worker2
ovn-kubernetes     ovs-node-jl7lh                            1/1   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovs-node-s9gxp                            1/1   Running  0        107s   172.17.0.4  ovn-worker
```

### Known issues

Some environments (Fedora32,31 on desktop), have problems when the cluster
is deleted directly with KinD `kind delete cluster --name ovn`, it restarts the host.
The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.

You can use the following command to delete the cluster:

`contrib/kind.sh --delete`
