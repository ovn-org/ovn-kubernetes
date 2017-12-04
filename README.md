# How to Use Open Virtual Networking With Kubernetes

This document describes how to use Open Virtual Networking with Kubernetes
1.3.0 or later.  This document assumes that you have installed Open
vSwitch by following [INSTALL.rst] or by using the distribution packages
such as .deb or.rpm.

## Setup

OVN provides network virtualization to containers.  OVN's integration with
containers currently works in two modes - the "underlay" mode or the "overlay"
mode.

In the "underlay" mode, one can create logical networks and can have
containers running inside VMs, standalone VMs (without having any containers
running inside them) and physical machines connected to the same logical
network.  In this mode, OVN programs the Open vSwitch instances running in
hypervisors with containers running inside the VMs.

In the "overlay" mode, OVN can create a logical network amongst containers
running on multiple hosts.  In this mode, OVN programs the Open vSwitch
instances running inside your VMs or directly on bare metal hosts.

For both the modes to work, a user has to install and start Open vSwitch in
each VM/host that he plans to run his containers.

Installing Open vSwitch is out of scope of this documentation.  You can read
[INSTALL.rst] for that.  That documentation inturn links to platform specific
installations.  If you use packages to install OVS, you should install both
OVS and OVN related packages.  You can also read the following quick-start
quide for Ubuntu that installs OVS and OVN from source:  [INSTALL.UBUNTU.md]

## The "overlay" mode

Kubernetes networking requirements:

OVN is a network virtualization solution.  It can create logical switches and
routers.  One can then interconnect these switches and routers to create any
network topologies.  Kubernetes (k8s) in its default mode has the following
networking requirements.

1. All containers should be able to communicate with each other over IP
address.

  OVN is capable of having overlapping IP addresses (as part of different
  network topologies).  But with the k8s requirement that all containers
  should be able to talk over IP address, it makes sense to have a simple
  network topology with one logical switch created per k8s node and then
  inter-connect all those logical switches with a single logical router.
  Any containers created in these nodes get connected to the logical switch
  associated with that node as a logical port.

2. All nodes should be able to communicate with the containers running inside
that node over IP address.

  k8s has this requirement for health-checking.  To achieve this goal, we will
  create an additional logical port for the node (represented by a OVS
  internal interface).

3. All containers should be able to communicate with the k8s daemons running
in the master node.

  This is a tricky requirement when OVN is run in the "overlay" mode.  To
  achieve this goal, we create a logical switch for the master node and also
  create a logical port for the same node (represented by a OVS internal
  interface and a private IP address).  We then use the logical port's IP
  address as the IP address via which the nodes can reach k8s central daemons.


### System Initialization

OVN in "overlay" mode needs a minimum Open vSwitch version of 2.6.  It needs
Open vSwitch version 2.7 for multi-gateway support.

* Start the central components.

OVN architecture has a central component which stores your networking intent
in a database.  Start this central component on the node where you intend to
start your k8s central daemons and which has an IP address of $CENTRAL_IP.

Start ovn-northd daemon.  This daemon translates networking intent from k8s
stored in the OVN_Northbound database to logical flows in OVN_Southbound
database.

```
/usr/share/openvswitch/scripts/ovn-ctl start_northd
```

In Open vSwitch 2.7, you need to additionally run the following commands
to open up TCP ports to access the OVN databases.

```
ovn-nbctl set-connection ptcp:6641
ovn-sbctl set-connection ptcp:6642
```

If you want to use SSL instead of TCP for OVN databases, please read
[INSTALL.SSL.md].

### One time setup.

On each host, you will need to run the following command once.  (You need to
run it again if your OVS database gets cleared.  It is harmless to run it
again in any case.)

$LOCAL_IP in the below command is the IP address via which other hosts can
reach this host.  This acts as your local tunnel endpoint.

$ENCAP_TYPE is the type of tunnel that you would like to use for overlay
networking.  The options are "geneve" or "stt".  (Please note that your kernel
should have support for your chosen $ENCAP_TYPE.  Both geneve and stt are part
of the Open vSwitch kernel module that is compiled from this repo.  If you use
the Open vSwitch kernel module from upstream Linux, you will need a minumum
kernel version of 3.18 for geneve.  There is no stt support in upstream Linux.
You can verify whether you have the support in your kernel by doing a `lsmod |
grep $ENCAP_TYPE`.)

```
ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="tcp:$CENTRAL_IP:6642" \
  external_ids:ovn-nb="tcp:$CENTRAL_IP:6641" \
  external_ids:ovn-encap-ip=$LOCAL_IP \
  external_ids:ovn-encap-type="$ENCAP_TYPE"
```

In addition, each Open vSwitch instance in an OVN deployment needs a unique,
persistent identifier, called the "system-id".  If you install OVS from
distribution packaging for Open vSwitch (e.g. .deb or .rpm packages), or if
you use the ovs-ctl utility included with Open vSwitch or the startup
scripts that come with Open vSwitch, it automatically configures a system-id.
If you start Open vSwitch manually, you should set one up yourself.

For example:

```
id_file=/etc/openvswitch/system-id.conf
test -e $id_file || uuidgen > $id_file
ovs-vsctl set Open_vSwitch . external_ids:system-id=$(cat $id_file)
```

And finally, start the ovn-controller.  (You need to run the below command on
every boot)

```
/usr/share/openvswitch/scripts/ovn-ctl start_controller
```

### k8s master node initialization.

Note: The init scripts used below use fair defaults.  If you are an advanced
user, please read [config.md] for more fine grained control.

Set the k8s API server address in the Open vSwitch database for the
initialization scripts (and later daemons) to pick from.

```
ovs-vsctl set Open_vSwitch . external_ids:k8s-api-server="127.0.0.1:8080"
```

Clone this repository and install the executables.

```
git clone https://github.com/openvswitch/ovn-kubernetes
cd ovn-kubernetes
pip install .
```

On the master node, with a unique node name of $NODE_NAME, for a cluster wide
private address range of $CLUSTER_IP_SUBNET and a smaller subnet for the
logical switch created in the master node of $MASTER_SWITCH_SUBNET, run:

```
ovn-k8s-overlay master-init \
  --cluster-ip-subnet=$CLUSTER_IP_SUBNET \
  --master-switch-subnet="$MASTER_SWITCH_SUBNET" \
  --node-name="$NODE_NAME"
```

Note: $NODE_NAME should be the same as the one used by kubelet.
kubelet by default uses the hostname.  kubelet allows this name to be be
overridden with --hostname-override.

An example is:

```
ovn-k8s-overlay master-init \
  --cluster-ip-subnet="192.168.0.0/16" \
  --master-switch-subnet="192.168.1.0/24" \
  --node-name="kube-master"
```

The above command will create a cluster wide logical router, a connected
logical switch for the master node and a logical port and a OVS internal
interface named "k8s-$NODE_NAME" with an IP address via which other nodes
should be eventually able to reach the daemons running on this node.  This
IP address will be referred in the future as $K8S_API_SERVER_IP.

### k8s minion node initializations.

Set the k8s API server address in the Open vSwitch database for the
initialization scripts (and later daemons) to pick from. For insecure
connections:

```
ovs-vsctl set Open_vSwitch . \
  external_ids:k8s-api-server="$K8S_API_SERVER_IP:8080"
```

For secure connections $API_TOKEN should be provided.  In case of self-signed
certificates $CA_CRT should be present in /etc/openvswitch/k8s-ca.crt or
stored in the OVSDB.  (Please note that if no protocol is specified in
external_ids:k8s-api-server, insecure http connection will be used.)

```
ovs-vsctl set Open_vSwitch . \
  external_ids:k8s-api-server="https://$K8S_API_SERVER_IP" \
  external_ids:k8s-ca-certificate="$CA_CRT" \
  external_ids:k8s-api-token="$API_TOKEN"
```

Clone this repository and install the executables.

```
git clone https://github.com/openvswitch/ovn-kubernetes
cd ovn-kubernetes
pip install .
```

On the minion node, with a unique node name of $NODE_NAME, for a cluster wide
private address range of $CLUSTER_IP_SUBNET and a smaller subnet for the
logical switch created in the minion node of $MINION_SWITCH_SUBNET, run:

```
ovn-k8s-overlay minion-init \
  --cluster-ip-subnet="$CLUSTER_IP_SUBNET" \
  --minion-switch-subnet="$MINION_SWITCH_SUBNET" \
  --node-name="$NODE_NAME"
```

Note: $NODE_NAME should be the same as the one used by kubelet.
kubelet by default uses the hostname.  kubelet allows this name to be be
overridden with --hostname-override.

An example is:

```
ovn-k8s-overlay minion-init \
  --cluster-ip-subnet="192.168.0.0/16" \
  --minion-switch-subnet="192.168.2.0/24" \
  --node-name="kube-minion1"
```

Note on Windows: ```ovn-k8s-overlay``` cannot be invoked directly and has to
be called via the python interpreter. The following is an example ran from an
elevated powershell prompt.
```
python (Get-Command ovn-k8s-overlay).Source minion-init \
  --cluster-ip-subnet="$CLUSTER_IP_SUBNET" \
  --minion-switch-subnet="$MINION_SWITCH_SUBNET" \
  --node-name="$NODE_NAME"
```

An example is:
```
python (Get-Command ovn-k8s-overlay).Source minion-init \
  --cluster-ip-subnet="192.168.0.0/16" \
  --minion-switch-subnet="192.168.2.0/24" \
  --node-name="kube-minwin1"
```

### k8s gateway node initialization

Gateway nodes are needed for North-South connectivity.  OVN has support
for multiple gateway nodes.

On any minions (or separate nodes, which would be the case for a DPDK
based OVN gateway), you need to initialize the gateway node.

Set the k8s API server address in the Open vSwitch database for the
initialization scripts (and later daemons) to pick from.

```
ovs-vsctl set Open_vSwitch . \
  external_ids:k8s-api-server="$K8S_API_SERVER_IP:8080"
```

Clone this repository and install the executables.

```
git clone https://github.com/openvswitch/ovn-kubernetes
cd ovn-kubernetes
pip install .
```

You have two options with respect to choosing the interface via which
North-South connectivity happens.

In the first option, you can choose a dedicated physical interface that is only
used for North-South connectivity for the cluster.  This means that you cannot
use this interface for any management traffic (like ssh).  If you choose
"eth1" as that physical interface, with an IP address of $PHYSICAL_IP and a
external gateway of $EXTERNAL_GATEWAY, run the following command on the
designated gateway node with a unique name of $NODE_NAME.

```
ovn-k8s-overlay gateway-init \
  --cluster-ip-subnet="$CLUSTER_IP_SUBNET" \
  --physical-interface eth1 \
  --physical-ip "$PHYSICAL_IP" \
  --node-name="$NODE_NAME" \
  --default-gw "$EXTERNAL_GATEWAY"
```

An example is:

```
ovn-k8s-overlay gateway-init \
  --cluster-ip-subnet="192.168.0.0/16" \
  --physical-interface eth1 \
  --physical-ip 10.33.74.138/24 \
  --node-name="kube-minion2" \
  --default-gw 10.33.74.253
```

The second option is to share a single network interface for both your
management traffic (e.g ssh) as well as the cluster's North-South traffic.  To
do this, you need to attach your physical interface to a OVS bridge and move
its IP address and routes to that bridge.  For e.g., if 'eth0' is your primary
network interface with IP address of "$PHYSICAL_IP", you create a bridge called
'breth0', add 'eth0' as a port of that bridge and then move "$PHYSICAL_IP" to
'breth0'. You also need to move the routing table entries to 'breth0'.  The
following helper script does the same

```
ovn-k8s-util nics-to-bridge eth0
```

After the above move, you will have to restart any dhclient sessions with
'breth0'.  You can do this on every bootup more natively by following the
instructions of [README.RHEL.rst] or [openvswitch-switch.README.Debian].

You then run your gateway initialization script with the following options.

```
ovn-k8s-overlay gateway-init \
  --cluster-ip-subnet="$CLUSTER_IP_SUBNET" \
  --bridge-interface breth0 \
  --physical-ip "$PHYSICAL_IP" \
  --node-name="$NODE_NAME" \
  --default-gw "$EXTERNAL_GATEWAY"
```

Since you share a NIC for both mgmt and North-South connectivity, you will
have to start a separate daemon to de-multiplex the traffic.

```
ovn-k8s-gateway-helper --physical-bridge=breth0 --physical-interface=eth0 \
    --pidfile --detach
```

In case of multiple gateways, when the traffic is originated from the
pods, you can pin the pod subnet traffic to go out of a particular
gateway.  For e.g., if you want the pods belonging to subnet 192.168.1.0/24
and 192.168.2.0/24 to go out of gateway1, when you initialize gateway1, you
can provide --rampout-ip-subnets="192.168.1.0/24,192.168.2.0/24" option to
the 'gateway-init' command.

### Watchers on master node

Once the above initializations are done, you can start your Kubernetes daemons
on the master node and minions.

You then start a watcher on the master node to listen for Kubernetes events.
This watcher is responsible to create logical ports and load-balancer entries.

```
ovn-k8s-watcher \
  --overlay \
  --pidfile \
  --log-file \
  -vfile:info \
  -vconsole:emer \
  --detach
```

### Overlay mode architecture diagram:

The following digaram represents the internal architecture details
of the overlay mode.

![alt text](https://i.imgur.com/i7sci9O.png "Overlay mode diagram")

## The "underlay" mode

TBA

## Installing Kubernetes

Installing k8s is out of scope of this documentation.  You can read
[README.K8S.md] for that.  This repo does provide a quick start guide here:
[INSTALL.K8S.md]

## Debugging

Please read [debugging.md].

## Vagrant

There is a vagrant available to bring up a simple cluster at [vagrant].

[INSTALL.rst]: http://docs.openvswitch.org/en/latest/intro/install
[INSTALL.UBUNTU.md]: docs/INSTALL.UBUNTU.md
[README.K8S.md]: https://github.com/kubernetes/kubernetes/tree/master/docs
[INSTALL.K8S.md]: docs/INSTALL.K8S.md
[README.RHEL.rst]: https://github.com/openvswitch/ovs/blob/master/rhel/README.RHEL.rst
[openvswitch-switch.README.Debian]: https://github.com/openvswitch/ovs/blob/master/debian/openvswitch-switch.README.Debian
[debugging.md]: docs/debugging.md
[vagrant]: vagrant/README.md
[INSTALL.SSL.md]: docs/INSTALL.SSL.md
[config.md]: docs/config.md
