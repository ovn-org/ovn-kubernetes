# OVS Hardware Offload

The OVS software based solution is CPU intensive, affecting system performance
and preventing fully utilizing available bandwidth. OVS 2.8 and above support
new feature called OVS Hardware Offload which improves performance significantly. 
This feature allows to offload the OVS data-plane to the NIC while maintaining 
OVS control-plane unmodified. It is using SR-IOV technology with VF representor
host net-device. The VF representor plays the same role as TAP devices
in Para-Virtual (PV) setup. A packet sent through the VF representor on the host
arrives to the VF, and a packet sent through the VF is received by its representor.

## Supported Ethernet controllers

The following manufacturers are known to work:

- Mellanox ConnectX-5 NIC

## Prerequisites

- Linux Kernel TBD (require connection tracking offload - https://lwn.net/Articles/793082/)
- Open vSwitch TBD (require connection tracking offload - https://mail.openvswitch.org/pipermail/ovs-dev/2019-July/360376.html)
- iproute >= 4.12
- sriov-device-plugin
- multus-cni

## Worker Node SR-IOV Configuration

In order to enable Open vSwitch hardware offloading, the following steps
are required. Please make sure you have root privileges to run the commands
below.

Check the Number of VF Supported on the NIC

```
cat /sys/class/net/enp3s0f0/device/sriov_totalvfs
8
```

Create the VFs

```
echo '4' > /sys/class/net/enp3s0f0/device/sriov_numvfs
```

Verfiy the VFs are created

```
ip link show enp3s0f0
8: enp3s0f0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP mode DEFAULT qlen 1000
   link/ether a0:36:9f:8f:3f:b8 brd ff:ff:ff:ff:ff:ff
   vf 0 MAC 00:00:00:00:00:00, spoof checking on, link-state auto
   vf 1 MAC 00:00:00:00:00:00, spoof checking on, link-state auto
   vf 2 MAC 00:00:00:00:00:00, spoof checking on, link-state auto
   vf 3 MAC 00:00:00:00:00:00, spoof checking on, link-state auto
```

Setup the PF to be up

```
ip link set enp3s0f0 up
```

Unbind the VFs from the driver

```
echo 0000:03:00.2 > /sys/bus/pci/drivers/mlx5_core/unbind
echo 0000:03:00.3 > /sys/bus/pci/drivers/mlx5_core/unbind
echo 0000:03:00.4 > /sys/bus/pci/drivers/mlx5_core/unbind
echo 0000:03:00.5 > /sys/bus/pci/drivers/mlx5_core/unbind
```

Configure SR-IOV VFs to switchdev mode

```
devlink dev eswitch set pci/0000:03:00.0 mode switchdev
ethtool -K enp3s0f0 hw-tc-offload on
```

Bind the VFs to the driver

```
echo 0000:03:00.2 > /sys/bus/pci/drivers/mlx5_core/bind
echo 0000:03:00.3 > /sys/bus/pci/drivers/mlx5_core/bind
echo 0000:03:00.4 > /sys/bus/pci/drivers/mlx5_core/bind
echo 0000:03:00.5 > /sys/bus/pci/drivers/mlx5_core/bind
```

Set hw-offload=true restart Open vSwitch

```
systemctl enable openvswitch.service
ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
systemctl restart openvswitch.service
```

## Worker Node SR-IOV network device plugin configuration

This plugin creates device plugin endpoints based on the configurations given in file `/etc/pcidp/config.json`.
This configuration file is in json format as shown below:

```json
{
    "resourceList": [
         {
            "resourceName": "cx5_sriov_switchdev",
            "selectors": {
                "vendors": ["15b3"],
                "devices": ["1018"]
            }
        }
    ]
}
```

Deploy SR-IOV network device plugin as daemonset see https://github.com/intel/sriov-network-device-plugin

## Worker Node Multus CNI configuration

Multus Config
```json
{
  "name": "multus-cni-network",
  "type": "multus",
  "clusterNetwork": "default",
  "defaultNetworks":[],
  "kubeconfig": "/etc/kubernetes/node-kubeconfig.yaml"
}
```

Deploy multus CNI as daemonset see https://github.com/intel/multus-cni

Create NetworkAttachementDefinition CRD with OVN CNI config

```yaml
Kubernetes Network CRD Spec:
apiVersion: "k8s.cni.cncf.io/v1"
kind: NetworkAttachmentDefinition
metadata:
  name: default
  annotations:
    k8s.v1.cni.cncf.io/resourceName: mellanox.com/cx5_sriov_switchdev
spec:
  Config: '{"cniVersion":"0.3.1","name":"ovn-kubernetes","type":"ovn-k8s-cni-overlay","ipam":{},"dns":{}}'
```

## Deploy POD with OVS hardware-offload

Create POD spec and

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ovs-offload-pod1
  annotations:
    v1.multus-cni.io/default-network: default
spec:
  containers:
  - name: appcntr1
    image: centos/tools
    resources:
      requests:
        mellanox.com/cx5_sriov_switchdev: '1'
      limits:
        mellanox.com/cx5_sriov_switchdev: '1'
```

## Verify Hardware-Offload is working

Lookup VF representor, in this example it is e5a1c8fcef0f327

```
$ ip link show enp3s0f0
6: enp3s0f0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq master ovs-system state UP mode DEFAULT group default qlen 1000
   link/ether ec:0d:9a:46:9e:84 brd ff:ff:ff:ff:ff:ff
   vf 0 MAC 00:00:00:00:00:00, spoof checking off, link-state enable, trust off, query_rss off
   vf 1 MAC 00:00:00:00:00:00, spoof checking off, link-state enable, trust off, query_rss off
   vf 2 MAC 00:00:00:00:00:00, spoof checking off, link-state enable, trust off, query_rss off
   vf 3 MAC fa:16:3e:b9:b8:ce, vlan 57, spoof checking on, link-state enable, trust off, query_rss off

compute_node2# ls -l /sys/class/net/
lrwxrwxrwx 1 root root 0 Sep 11 10:54 eth0 -> ../../devices/virtual/net/eth0
lrwxrwxrwx 1 root root 0 Sep 11 10:54 eth1 -> ../../devices/virtual/net/eth1
lrwxrwxrwx 1 root root 0 Sep 11 10:54 eth2 -> ../../devices/virtual/net/eth2
lrwxrwxrwx 1 root root 0 Sep 11 10:54 e5a1c8fcef0f327 -> ../../devices/virtual/net/e5a1c8fcef0f327
```

Access the POD

```
kubectl exec -it ovs-offload-pod1 -- /bin/bash
```

Ping other POD on second worker node
```
ping ovs-offload-pod2
```

Check traffic on the VF representor port. Verify that only the first ICMP packet appears
```
tcpdump -nnn -i e5a1c8fcef0f327

tcpdump: verbose output suppressed, use -v or -vv for full protocol decode
17:12:41.260487 IP 172.0.0.13 > 172.0.0.10: ICMP echo request, id 1263, seq 1, length 64
17:12:41.260778 IP 172.0.0.10 > 172.0.0.13: ICMP echo reply, id 1263, seq 1, length 64
17:12:46.268951 ARP, Request who-has 172.0.0.13 tell 172.0.0.10, length 42
17:12:46.271771 ARP, Reply 172.0.0.13 is-at fa:16:3e:1a:10:05, length 46
17:12:55.354737 IP6 fe80::f816:3eff:fe29:8118 > ff02::1: ICMP6, router advertisement, length 64
17:12:56.106705 IP 0.0.0.0.68 > 255.255.255.255.67: BOOTP/DHCP, Request from 62:21:f0:89:40:73, length 30
```

