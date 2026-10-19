# OVN-Kubernetes KinD Setup

KinD (Kubernetes in Docker) deployment of OVN kubernetes is a fast and easy means to quickly install and test kubernetes with OVN kubernetes CNI. The value proposition is really for developers who want to reproduce an issue or test a fix in an environment that can be brought up locally and within a few minutes.

### Prerequisites

- A Linux host or Linux virtual machine. The script reads Linux sysctls and
  `/proc`, and OVN-Kubernetes requires a kernel with `CONFIG_NFT_FIB_INET`
  enabled. Running the script directly on macOS is not supported.
- 20 GB of free space on the filesystem used by the container runtime
- Docker or Podman
- [KinD](https://kubernetes.io/docs/setup/learning-environment/kind/)
  version 0.27.0 or later
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/)
- [Helm](https://helm.sh/docs/intro/install/) v3 or later
- Python 3, `pipx`, and `jinjanate`. If `jinjanate` is missing, the script installs it
  with [pipx](https://pipx.pypa.io/stable/installation/).
- git
- curl
- awk
- jq
- make
- GNU coreutils, including `timeout`
- skopeo when Podman is installed
- OpenSSL 3 when enabling IPsec

For OVN kubernetes KinD deployment, use the `kind.sh` script (a symlink to `kind-helm.sh`, which deploys OVN-Kubernetes via Helm).

First download the OVN-Kubernetes repository:

```shell
git clone https://github.com/ovn-kubernetes/ovn-kubernetes.git
cd ovn-kubernetes
```
The `kind.sh` script builds OVN-Kubernetes into a container image. On Linux,
you can optionally build the Go binaries first to verify local changes. This
requires Go 1.26.3 or later, as specified by `go-controller/go.mod`:

```
pushd go-controller
make
popd
```

### Run the KinD deployment with Docker

Run the script from the repository root. It builds the Fedora image as part of
the deployment:

```shell
export KUBECONFIG="${HOME}/ovn.conf"
pushd contrib
./kind.sh
popd
```

### Optional: Run the KinD deployment with Podman

Podman is not required when you use Docker. If you prefer Podman, run the script
as root with Podman, then copy root's kubeconfig for use by your regular user.
The script passes Podman to the image build; do not edit the Makefile:

```shell
pushd contrib
sudo ./kind.sh -ep podman
mkdir -p ~/.kube
sudo cp /root/ovn.conf ~/.kube/kind-config
sudo chown "$(id -u):$(id -g)" ~/.kube/kind-config
export KUBECONFIG=~/.kube/kind-config
popd
```

This will launch a KinD deployment. By default, the cluster is named `ovn`.

Example output from an older Kubernetes release:

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

The `kind.sh` script defaults to one control-plane node, two worker nodes, and
HA disabled. Run the following command for the current options and defaults:

```text
$ contrib/kind.sh --help
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
       [ -ue | --uplink-enable ]
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
       [ --dpu-mode <none|host|dpu> ]
       [ -f | --extra-values <file> ]
       [ --frr-k8s-remote-kubeconfig <file> ]
       [ --frr-k8s-host-kubeconfig <file> ]
       [ --frr-k8s-remote-node-map <host=dpu[,host=dpu...]> ]
       [ --bgp-net-name <name> ]
       [ --bgp-server-name <name> ]
       [ --frr-container-name <name> ]
       [ --bgp-server-net-subnet-ipv4 <subnet> ]
       [ --bgp-server-net-subnet-ipv6 <subnet> ]
       [ --bgp-server-host-port <port> ]
       [ --enable-coredumps ]
       [ -ub | --uplink-bridge ]
       [ -h ]

--delete                                      Delete current cluster
-cf  | --config-file                          Name of the KIND configuration file
-kt  | --keep-taint                           Do not remove taint components
                                              DEFAULT: Remove taint components
-me  | --multicast-enabled                    Enable multicast. DEFAULT: Disabled
-ho  | --hybrid-enabled                       Enable hybrid overlay. DEFAULT: Disabled
-obs | --observability                        Enable observability. DEFAULT: Disabled
-el  | --ovn-empty-lb-events                  Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled
-ii  | --install-ingress                      Flag to install Ingress Components.
                                              DEFAULT: Don't install ingress components.
-mlb | --install-metallb                      Install metallb to test service type LoadBalancer deployments
-pl  | --install-cni-plugins                  Install CNI plugins
-ikv | --install-kubevirt                     Install kubevirt
-mne | --multi-network-enable                 Enable multi networks. DEFAULT: Disabled
-nse | --network-segmentation-enable          Enable network segmentation. DEFAULT: Disabled
-nce | --network-connect-enable               Enable network connect (requires network segmentation). DEFAULT: Disabled
-ue  | --uplink-enable                        Enable uplink (requires network segmentation). DEFAULT: Disabled
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
-ub  | --uplink-bridge                        Create an unmanaged OVS bridge for Uplink testing on each KIND node.
-lr  | --local-kind-registry                  Configure kind to use a local container registry for images.
-ep  | --experimental-provider                Use an experimental OCI provider such as podman instead of docker.
--deploy                                      Deploy ovn-kubernetes without restarting kind
--add-nodes                                   Adds nodes to an existing cluster. Number of nodes set by --num-workers.
--isolated                                    After cluster creation, remove default route from nodes and publish kind node IPs as /etc/hosts entries for DNS-less isolation.
-ml  | --master-loglevel                      Log level for ovnkube-control-plane pods (0..5). DEFAULT: 4
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
--dpu-mode                                    Deploy OVN-Kubernetes for DPU simulator mode: none, host, or dpu. DEFAULT: none
-f | --extra-values                           Extra Helm values file appended after the base values file. May be repeated.
--frr-k8s-remote-kubeconfig                   Kubeconfig used by DPU-cluster FRR-K8S pods to watch the host cluster API.
--frr-k8s-host-kubeconfig                     Kubeconfig used by this script to write FRR-K8S resources to the host cluster API.
--frr-k8s-remote-node-map                     Comma-separated host-node=dpu-node pairs for remote FRR-K8S node-name mapping.
--bgp-net-name                                Name of the docker network hosting the external BGP server. DEFAULT: bgpnet
--bgp-server-name                             Name of the external BGP server container. DEFAULT: bgpserver
--frr-container-name                          Name of the external FRR router container. DEFAULT: frr
--bgp-server-net-subnet-ipv4                  IPv4 subnet of the external BGP server network. DEFAULT: 172.26.0.0/16
--bgp-server-net-subnet-ipv6                  IPv6 subnet of the external BGP server network. DEFAULT: fc00:f853:ccd:e796::/64
--bgp-server-host-port                        Host port published by the external BGP server container. DEFAULT: 18080
-nokvipam | --opt-out-kv-ipam                 Skip installing the KubeVirt IPAM controller (requires --install-kubevirt).

```

**Notes / troubleshooting:**

- Issue with /dev/dma_heap: if you get the error `kind "Error: open /dev/dma_heap: permission denied"`, there's a [known issue](https://bugzilla.redhat.com/show_bug.cgi?id=1966158) about it (directory mislabelled with selinux).
Workaround:

```bash
sudo setenforce 0
sudo chcon system\_u:object\_r:device\_t:s0 /dev/dma\_heap/
sudo setenforce 1
```

### Usage Notes

- You can create your own KinD J2 configuration file if the default one is not sufficient

- You can also specify these values as environment variables. Command line parameters will override the environment variables.

- To tear down a Docker KinD cluster from the repository root, run:

   ```shell
   contrib/kind.sh --delete
   ```

- For a cluster created with Podman and `sudo`, use the same provider and privileges:

   ```shell
   sudo contrib/kind.sh -ep podman --delete
   ```

## Running OVN-Kubernetes with IPv6 or Dual-stack in KinD

This section describes the configuration needed for IPv6 and dual-stack environments.
KinD supports both IP families; `contrib/kind-common.sh` sets the KinD `ipFamily`
and OVN-Kubernetes network ranges from the two `PLATFORM_IPV*_SUPPORT` variables.
These settings apply to either provider. For Podman, pass them through `sudo env`
when running `./kind.sh -ep podman`, then follow the
[Podman kubeconfig steps](#optional-run-the-kind-deployment-with-podman).
For IPv6 on Linux, verify that the kernel exposes IPv6 support, IPv6 is enabled,
and forwarding is enabled before creating the cluster:

```shell
test -f /proc/net/if_inet6
sysctl net.ipv6.conf.all.disable_ipv6
sysctl net.ipv6.conf.all.forwarding
```

`net.ipv6.conf.all.disable_ipv6` must be `0`, and
`net.ipv6.conf.all.forwarding` must be `1`. Set these values before running the
script, or pass `--allow-system-writes` to allow the script to set them. See the
[KinD networking configuration](https://kind.sigs.k8s.io/docs/user/configuration/#ip-family)
for additional host requirements.

## KinD with IPv6

From the repository root, run:

```shell
export KUBECONFIG="${HOME}/ovn.conf"
pushd contrib
PLATFORM_IPV4_SUPPORT=false PLATFORM_IPV6_SUPPORT=true ./kind.sh
popd
```

After creation, check the nodes and OVN-Kubernetes pods:

```shell
kubectl get nodes
kubectl get pods -n ovn-kubernetes -o wide
```

### IPv6 host networking troubleshooting with Docker

If worker nodes fail to join an IPv6 cluster backed by Docker, inspect the
KinD network and export the cluster logs before changing host settings:

```shell
docker network inspect kind
kind export logs --name ovn ./kind-logs
```

The KinD network should show `"EnableIPv6": true`. On older Docker and KinD setups,
the Docker **default bridge** also needed IPv6 enabled. This is a separate
network from KinD's own bridge, so the following is a legacy workaround, not
a general prerequisite for current KinD. On those hosts, `/etc/docker/daemon.json`
used settings like:

```json
{
  "ipv6": true,
  "fixed-cidr-v6": "fd00:1234:5678:1::/64"
}
```

Choose a subnet that does not overlap existing networks, then restart Docker
after editing its configuration. The `fixed-cidr-v6` setting allocates addresses
on Docker's default bridge; on a host where this workaround applies, verify it
with `docker run --rm busybox ip -6 addr show eth0`. The output should include
a global or unique-local IPv6 address as well as a link-local `fe80::` address.
See [Docker's IPv6 documentation](https://docs.docker.com/engine/daemon/ipv6/)
for the current daemon settings. The original guide recorded a Docker 19.03.6
host that failed to restart after enabling IPv6 without `fixed-cidr-v6`.

An older KinD issue also showed `Joining worker nodes` failing when `firewalld`
blocked traffic between IPv6 node containers. The kubeadm log contained
`dial tcp [<node IPv6 address>]:6443: connect: permission denied`.
This points to host networking during the worker join, before
OVN-Kubernetes pods start.
If you see that symptom, check the host firewall and the
[KinD firewalld guidance](https://kind.sigs.k8s.io/docs/user/known-issues/#firewalld)
before changing firewall rules. The old workaround stopped `firewalld` with
`sudo systemctl stop firewalld`; restore it with `sudo systemctl start firewalld`
after troubleshooting. See [KinD issue 1257](https://github.com/kubernetes-sigs/kind/issues/1257#issuecomment-575984987)
for the original report.

## KinD with Dual-stack

From the repository root, run:

```shell
export KUBECONFIG="${HOME}/ovn.conf"
pushd contrib
PLATFORM_IPV4_SUPPORT=true PLATFORM_IPV6_SUPPORT=true ./kind.sh
popd
```

Check the result with `kubectl get nodes` and `kubectl get pods -A -o wide`.
For a service being tested, `kubectl get service SERVICE_NAME -o yaml` shows
its `ipFamilies` and `clusterIPs`. Check the client with `kubectl version --client`.
If it is outside the supported minor-version skew for the cluster, use a
compatible client built in the
[local testing guide](../developer-guide/local_testing_guide.md) rather than
replacing a system-wide `kubectl` binary. KinD supports dual-stack clusters
with KinD 0.11+ and Kubernetes 1.20+; the former experimental patch to KinD
is no longer needed.

If nodes remain `NotReady` or pods remain `Pending`, inspect pod events and
the OVN-Kubernetes pods before assuming dual-stack support is missing:

```shell
kubectl describe nodes
kubectl get pods -A -o wide
kubectl describe pod -n ovn-kubernetes POD_NAME
```

Replace `POD_NAME` with the name of a pod that is not ready.

The `NotReady` output from the former Kubernetes v1.18 example described an
incomplete historical setup, not the expected state of current clusters.
When finished, run `contrib/kind.sh --delete` from the repository root.

## Advanced KinD options

### Using specific KinD container image and tag

:warning: Use with caution, as KinD expects this image to have all it needs.

In order to use an image/tag other than the default hardcoded in `kind.sh`, specify
one (or both of) the following variables:

```
$ cd contrib
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

### Known issues

Some environments (Fedora32,31 on desktop), have problems when the cluster
is deleted directly with KinD `kind delete cluster --name ovn`, it restarts the host.
The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.

You can use the following command to delete the cluster:

`contrib/kind.sh --delete`
