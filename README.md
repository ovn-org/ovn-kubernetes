# How to use Open Virtual Networking with Kubernetes

This document describes how to use Open Virtual Networking with Kubernetes
1.8.0 or later.  This document assumes that you have installed Open
vSwitch by following [INSTALL.rst] or by using the distribution packages
such as .deb or.rpm.

## Setup

OVN provides network virtualization to containers.  In the "overlay" mode,
OVN can create a logical network amongst containers running on multiple hosts.
In this mode, OVN programs the Open vSwitch instances running inside your
hosts.  These hosts can be bare-metal machines or vanilla VMs.

Installing Open vSwitch is out of scope of this documentation.  You can read
[INSTALL.rst] for that.  That documentation inturn links to platform specific
installations.  If you use packages to install OVS, you should install both
OVS and OVN related packages.  You can also read the following quick-start
guide for Ubuntu that installs OVS and OVN from source and packages:
[INSTALL.UBUNTU.md]
The following guide explains setting up an OVN overlay network on Openshift
running on RHEL/Centos/Fedora: [INSTALL.OPENSHIFT.md](docs/INSTALL.OPENSHIFT.md)

On each node, you should also install the 'ovnkube' utility that comes with
this repository. To compile it, you will need golang installed.  You can
install golang with:

```
wget -nv https://dl.google.com/go/go1.9.2.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.9.2.linux-amd64.tar.gz
```

You need to set your GOPATH, clone the ovn-kubernetes repo, compile and
install it.

```
export GOPATH=$HOME/work
mkdir -p $GOPATH/github.com/openvswitch
cd $GOPATH/github.com/openvswitch
git clone https://github.com/openvswitch/ovn-kubernetes
cd ovn-kubernetes/go-controller
make
sudo make install
```

Note that, you need not do the above compilation on every host. You can copy
over the compiled binaries to other hosts.  We will have the option to run
OVN via kubernetes daemonsets soon.

### Kubernetes networking requirements:

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

  This can be achieved by running OVN gateways in the minion nodes. With
  at least one OVN gateway, the pods can reach the k8s central daemons with
  NAT.

### Master node initialization

* Start the central components on a k8s master node.

OVN architecture has a central component which stores your networking intent
in a database.  Start this central component on one of the nodes where you
have started your k8s central daemons and which has an IP address of
$CENTRAL_IP.  (For HA of the central component, please read [HA.md])

Start ovn-northd daemon.  This daemon translates networking intent from k8s
stored in the OVN_Northbound database to logical flows in OVN_Southbound
database.

```
/usr/share/openvswitch/scripts/ovn-ctl start_northd
```

Also start ovn-controller on this node.

```
/usr/share/openvswitch/scripts/ovn-ctl start_controller
```

Now start the ovnkube utility on the master node.

The below command expects the user to provide
* A cluster wide private address range of $CLUSTER_IP_SUBNET
(e.g: 192.168.0.0/16).  The pods are provided IP address from this range.

* $NODE_NAME should be the same as the one used by kubelet.  kubelet by default
uses the hostname.  kubelet allows this name to be overridden with
--hostname-override.

* $SERVICE_IP_SUBNET is the same as the one provided to k8s-apiserver via
--service-cluster-ip-range option. An e.g is 172.16.1.0/24.

```
 nohup sudo ovnkube -k8s-kubeconfig kubeconfig.yaml -net-controller \
 -loglevel=4 \
 -k8s-apiserver="http://$CENTRAL_IP:8080" \
 -logfile="/var/log/openvswitch/ovnkube.log" \
 -init-master=$NODE_NAME -init-node=$NODE_NAME \
 -cluster-subnet="$CLUSTER_IP_SUBNET" \
 -service-cluster-ip-range=$SERVICE_IP_SUBNET \
 -nodeport \
 -k8s-token="$TOKEN" \
 -nb-address="tcp://$CENTRAL_IP:6641" \
 -sb-address="tcp://$CENTRAL_IP:6642" 2>&1 &
```

Note: Make sure to read /var/log/openvswitch/ovnkube.log to see that there were
no obvious errors with argument passing.  Also, you should only pass
"-init-node" argument if there is a kubelet running on the master node too.

If you want to use SSL instead of TCP for OVN databases, please read
[INSTALL.SSL.md].

### Minion node initialization.

On each host, you will need to run the following command once.

The below command expects the user to provide
* A cluster wide private address range of $CLUSTER_IP_SUBNET
(e.g: 192.168.0.0/16).  The pods are provided IP address from this range.
This value should be the same as the one provided to ovnkube in the master
node.

* $NODE_NAME should be the same as the one used by kubelet.  kubelet by default
uses the hostname.  kubelet allows this name to be overridden with
--hostname-override.

* $SERVICE_IP_SUBNET is the same as the one provided to k8s-apiserver via
--service-cluster-ip-range option. An e.g is 172.16.1.0/24.

* If you are using ingress controllers with L7 load-balancing to enter into
the k8s cluster, you can skip providing the '-nodeport' option with the
below command.

```
nohup sudo ovnkube -k8s-kubeconfig kubeconfig.yaml -loglevel=4 \
    -logfile="/var/log/openvswitch/ovnkube.log" \
    -k8s-apiserver="http://$CENTRAL_IP:8080" \
    -init-node="$NODE_NAME"  \
    -nodeport \
    -nb-address="tcp://$CENTRAL_IP:6641" \
    -sb-address="tcp://$CENTRAL_IP:6642" -k8s-token="$TOKEN" \
    -init-gateways \
    -service-cluster-ip-range=$SERVICE_IP_SUBNET \
    -cluster-subnet=$CLUSTER_IP_SUBNET 2>&1 &
```

Note: Make sure to read /var/log/openvswitch/ovnkube.log to see that there were
no obvious errors with argument passing.

Notes on gateway nodes:

* Gateway nodes are needed for North-South connectivity in OVN.
OVN has support for multiple gateway nodes. In the above command,
since '-init-gateways' has been provided as an option, a OVN
gateway will be created on each minion.

* Just providing '-init-gateways', will make OVN choose the
interface in your minion via which the minion's default gateway
is reached.

* If you want to chose the interface for your gateway, you should
provide '-gateway-interface' and '-gateway-nexthop' as options. For e.g:

```
-init-gateways -gateway-interface=enp0s9 -gateway-nexthop="$NEXTHOP"
```

* For both the above cases, ovnkube will create a OVS bridge on top of
your physical interface and move the IP address and route informations
from the physical interface to OVS bridge.  If you are using Ubuntu
and OVS startup scripts are systemd (e.g: there is a file called
/lib/systemd/system/ovsdb-server.service) , you will have to add the
following line to /etc/default/openvswitch

```
OPTIONS=--delete-transient-ports
```

* If you have a spare interface that you want to exclusively use for OVN
gateway on a node, you also need to pass the -gateway-spare-interface option.
Foe e.g:

```
-init-gateways -gateway-interface=enp0s9 -gateway-nexthop="$NEXTHOP" \
    -gateway-spare-interface
```

For more control on the options to ovnkube, please read [config.md]

## Debugging

Please read [debugging.md].

## Vagrant

There is a vagrant available to bring up a simple cluster at [vagrant].

### Overlay mode architecture diagram:

The following digaram represents the internal architecture details
of the overlay mode.

![alt text](https://i.imgur.com/i7sci9O.png "Overlay mode diagram")

## Installing Kubernetes

Installing k8s is out of scope of this documentation.  You can read
[README.K8S.md] for that.  For a quick start, the vagrant in this repo,
does install kubernetes in its simplest form.

[INSTALL.rst]: http://docs.openvswitch.org/en/latest/intro/install
[INSTALL.UBUNTU.md]: docs/INSTALL.UBUNTU.md
[README.K8S.md]: https://github.com/kubernetes/kubernetes/tree/master/docs
[README.RHEL.rst]: https://github.com/openvswitch/ovs/blob/master/rhel/README.RHEL.rst
[debugging.md]: docs/debugging.md
[vagrant]: vagrant/README.md
[INSTALL.SSL.md]: docs/INSTALL.SSL.md
[config.md]: docs/config.md
[HA.md]: docs/ha.md
