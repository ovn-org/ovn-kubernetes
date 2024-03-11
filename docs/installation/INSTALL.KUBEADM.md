The following is a walkthrough for an installation in an environment with 4 virtual machines, and a cluster deployed with `kubeadm`. This shall serve as a guide for people who are curious enough to deploy OVN Kubernetes on a manually created cluster and to play around with the components. 

Note that the resulting environment might be highly unstable.

If your goal is to set up an environment quickly or to set up a development environment, see the [kind installation documentation](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/kind.md) instead.

## Environment setup

### Overview

The environment consists of 4 libvirt/qemu virtual machines, all deployed with Rocky Linux 8 or CentOS 8. `node1` will serve as the sole master node and nodes `node2` and `node3` as the worker nodes. `gw1` will be the default gateway for the cluster via the `Isolated Network`. It will also host an HTTP registry to store the OVN Kubernetes images.

~~~
       to hypervisor         to hypervisor         to hypervisor
             │                     │                     │
             │                     │                     │
           ┌─┴─┐                 ┌─┴─┐                 ┌─┴─┐
           │if1│                 │if1│                 │if1│
     ┌─────┴───┴─────┐     ┌─────┴───┴─────┐     ┌─────┴───┴─────┐
     │               │     │               │     │               │
     │               │     │               │     │               │
     │     node1     │     │     node2     │     │     node3     │
     │               │     │               │     │               │
     │               │     │               │     │               │
     └─────┬───┬─────┘     └─────┬───┬─────┘     └─────┬───┬─────┘
           │if2│                 │if2│                 │if2│
           └─┬─┘                 └─┬─┘                 └─┬─┘
             │                     │                     │
             │                     │                     │
             │                    xxxxxxxx               │
             │                 xxx       xxx             │
             │                xx           xx            │
             │               x   Isolated   x            │
             └──────────────x     Network   x────────────┘
                            xxx            x
                              xxxxxx  xxxxx
                                   xxxx
                                   │
                                 ┌─┴─┐
                                 │if2│
                           ┌─────┴───┴─────┐
                           │               │
                           │               │
                           │      gw1      │
                           │               │
                           │               │
                           └─────┬───┬─────┘
                                 │if1│
                                 └─┬─┘
                                   │
                                   │
                              to hypervisor
~~~

Legend:
* if1 - enp1s0 | 192.168.122.0/24
* if2 - enp7s0 | 192.168.123.0/24

`to hypervisor` is libvirt's default network with full DHCP. It will be used as management access to all nodes as well as on `gw1` as the interface for outside connectivity:
~~~
$ sudo virsh net-dumpxml default
<network connections='2'>
  <name>default</name>
  <uuid>76b7e8c1-7c2c-456b-ac10-09c98c6275a5</uuid>
  <forward mode='nat'>
    <nat>
      <port start='1024' end='65535'/>
    </nat>
  </forward>
  <bridge name='virbr0' stp='on' delay='0'/>
  <mac address='52:54:00:4b:4d:f8'/>
  <ip address='192.168.122.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.122.2' end='192.168.122.254'/>
    </dhcp>
  </ip>
</network>
~~~

And `Isolated Network` is an isolated network. `gw1` will be the default gateway for this network, and `node1` through `node3` will have their default route go through this network:
~~~
$ sudo virsh net-dumpxml ovn
<network connections='2'>
  <name>ovn</name>
  <uuid>fecea98b-8b92-438e-a759-f6cfb366614c</uuid>
  <bridge name='virbr2' stp='on' delay='0'/>
  <mac address='52:54:00:d4:f2:cc'/>
  <domain name='ovn'/>
</network>
~~~

### Gateway setup (gw1)

Deploy the gateway virtual machine first. Set it up as a simple gateway which will NAT everything that comes in on interface enp7s0:
~~~
IF1=enp1s0
IF2=enp7s0
hostnamectl set-hostname gw1
nmcli conn mod ${IF1} connection.autoconnect yes
nmcli conn mod ${IF2} ipv4.address 192.168.123.254/24
nmcli conn mod ${IF2} ipv4.method static
nmcli conn mod ${IF2} connection.autoconnect yes
nmcli conn reload
systemctl stop firewalld
cat /proc/sys/net/ipv4/ip_forward
sysctl -a | grep ip_forward
echo "net.ipv4.ip_forward=1" >> /etc/sysctl.d/99-sysctl.conf
sysctl --system
yum install iptables-services -y
yum remove firewalld -y
systemctl enable --now iptables
iptables-save
iptables -t nat -I POSTROUTING --src 192.168.123.0/24  -j MASQUERADE
iptables -I FORWARD --j ACCEPT
iptables -I INPUT -p tcp --dport 5000 -j ACCEPT
iptables-save > /etc/sysconfig/iptables
~~~

Also set up an HTTP registry:
~~~
yum install podman -y
mkdir -p /opt/registry/data
podman run --name mirror-registry \
  -p 5000:5000 -v /opt/registry/data:/var/lib/registry:z      \
  -d docker.io/library/registry:2
podman generate systemd --name mirror-registry > /etc/systemd/system/mirror-registry-container.service
systemctl daemon-reload
systemctl enable --now mirror-registry-container
~~~

Now, reboot the gateway:
~~~
reboot
~~~

### node1 through node3 base setup

You must install Open vSwitch on `node1` through `node3`. You will then connect `enp7s0` to an OVS bridge called `br-ex`. This bridge will be used later by OVN Kubernetes.
Furthermore, you  must assign IP addresses to `br-ex` and point the nodes' default route via `br-ex` to `gw1`. 

#### Set hostnames

Set the hostnames manually, even if they are set correctly by DHCP. Set them manually to:
~~~
hostnamectl set-hostname node<x>
~~~

#### Disable swap

Make sure to disable swap. Kubelet will not run otherwise:
~~~
sed -i '/ swap /d' /etc/fstab
reboot
~~~

#### Remove firewalld

Make sure to uninstall firewalld. Otherwise, it will block the kubernetes management ports (that can easily be fixed by configuration) and it will also preempt and block the OVN Kubernetes installed NAT and FORWARD rules (this is more difficult to remediate). The easiest fix is hence not to use firewalld at all:
~~~
systemctl disable --now firewalld
yum remove -y firewalld
~~~
> For more details, see [https://gitmemory.com/issue/firewalld/firewalld/767/790687269](https://gitmemory.com/issue/firewalld/firewalld/767/790687269); this is about Calico, but it highlights the same issue.

#### Install Open vSwitch

Install Open vSwitch from [https://wiki.centos.org/SpecialInterestGroup/NFV](https://wiki.centos.org/SpecialInterestGroup/NFV)

##### On CentOS:

~~~
yum install centos-release-nfv-openvswitch -y
yum install openvswitch2.13 --nobest -y
yum install NetworkManager-ovs.x86_64 -y
systemctl enable --now openvswitch
~~~

##### On Rocky Linux

Rocky doesn't have access to CentOS's repositories. However, you can still use the CentOS NFV repositories:
~~~
rpm -ivh http://mirror.centos.org/centos/8-stream/extras/x86_64/os/Packages/centos-release-nfv-common-1-3.el8.noarch.rpm --nodeps
rpm -ivh http://mirror.centos.org/centos/8-stream/extras/x86_64/os/Packages/centos-release-nfv-openvswitch-1-3.el8.noarch.rpm
yum install openvswitch2.13 --nobest -y
yum install NetworkManager-ovs.x86_64 -y
systemctl enable --now openvswitch
~~~

Alternatively, on Rocky Linux, you can also build your own RPMs directly from the SRPMs, e.g.:
~~~
yum install '@Development Tools'
yum install desktop-file-utils libcap-ng-devel libmnl-devel numactl-devel openssl-devel python3-devel python3-pyOpenSSL python3-setuptools python3-sphinx rdma-core-devel unbound-devel -y
rpmbuild --rebuild  http://ftp.redhat.com/pub/redhat/linux/enterprise/8Base/en/Fast-Datapath/SRPMS/openvswitch2.13-2.13.0-79.el8fdp.src.rpm
yum install selinux-policy-devel -y
rpmbuild --rebuild http://ftp.redhat.com/pub/redhat/linux/enterprise/8Base/en/Fast-Datapath/SRPMS/openvswitch-selinux-extra-policy-1.0-28.el8fdp.src.rpm
yum localinstall /root/rpmbuild/RPMS/noarch/openvswitch-selinux-extra-policy-1.0-28.el8.noarch.rpm /root/rpmbuild/RPMS/x86_64/openvswitch2.13-2.13.0-79.el8.x86_64.rpm -y
yum install NetworkManager-ovs.x86_64 -y
systemctl enable --now openvswitch
~~~

#### Configure networking

Set up networking:
~~~
BRIDGE_NAME=br-ex
IF1=enp1s0
IF2=enp7s0
IP_ADDRESS="192.168.123.$(hostname | sed 's/node//')/24"
~~~

Verify the `IP_ADDRESS` - it should be unique for every node and the last octet should be the same as the node's numeric identifier:
~~~
echo $IP_ADDRESS
~~~

Then, continue:
~~~
nmcli c add type ovs-bridge conn.interface ${BRIDGE_NAME} con-name ${BRIDGE_NAME}
nmcli c add type ovs-port conn.interface ${BRIDGE_NAME} master ${BRIDGE_NAME} con-name ovs-port-${BRIDGE_NAME}
nmcli c add type ovs-interface slave-type ovs-port conn.interface ${BRIDGE_NAME} master ovs-port-${BRIDGE_NAME}  con-name ovs-if-${BRIDGE_NAME}
nmcli c add type ovs-port conn.interface ${IF2} master ${BRIDGE_NAME} con-name ovs-port-${IF2}
nmcli c add type ethernet conn.interface ${IF2} master ovs-port-${IF2} con-name ovs-if-${IF2}
nmcli conn delete ${IF2}
nmcli conn mod ${BRIDGE_NAME} connection.autoconnect yes
nmcli conn mod ovs-if-${BRIDGE_NAME} connection.autoconnect yes
nmcli conn mod ovs-if-${IF2} connection.autoconnect yes
nmcli conn mod ovs-port-${IF2} connection.autoconnect yes
nmcli conn mod ovs-port-${BRIDGE_NAME} connection.autoconnect yes
nmcli conn mod ovs-if-${BRIDGE_NAME} ipv4.address ${IP_ADDRESS}
nmcli conn mod ovs-if-${BRIDGE_NAME} ipv4.method static
nmcli conn mod ovs-if-${BRIDGE_NAME} ipv4.route-metric 50

# move the default route to br-ex
BRIDGE_NAME=br-ex
nmcli conn mod ovs-if-${BRIDGE_NAME} ipv4.gateway "192.168.123.254"
nmcli conn mod ${IF1} ipv4.never-default yes
# Change DNS to 8.8.8.8
nmcli conn mod ${IF1} ipv4.ignore-auto-dns yes
nmcli conn mod ovs-if-${BRIDGE_NAME} ipv4.dns "8.8.8.8"
~~~

Now, reboot the node:
~~~
reboot
~~~

After the reboot, you should see something like this, for example on node1:
~~~
[root@node1 ~]# cat /etc/resolv.conf 
# Generated by NetworkManager
nameserver 8.8.8.8
[root@node1 ~]# ovs-vsctl show
c1aee179-b425-4b48-8648-dd8746f59add
    Bridge br-ex
        Port enp7s0
            Interface enp7s0
                type: system
        Port br-ex
            Interface br-ex
                type: internal
    ovs_version: "2.13.4"
[root@node1 ~]# ip r
default via 192.168.123.254 dev br-ex proto static metric 800 
192.168.122.0/24 dev enp1s0 proto kernel scope link src 192.168.122.205 metric 100 
192.168.123.0/24 dev br-ex proto kernel scope link src 192.168.123.1 metric 800 
[root@node1 ~]# ip a ls dev br-ex
6: br-ex: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 26:98:69:4a:d7:43 brd ff:ff:ff:ff:ff:ff
    inet 192.168.123.1/24 brd 192.168.123.255 scope global noprefixroute br-ex
       valid_lft forever preferred_lft forever
    inet6 fe80::4a1d:4d35:7c28:1ff2/64 scope link noprefixroute 
       valid_lft forever preferred_lft forever
[root@node1 ~]# nmcli conn
NAME             UUID                                  TYPE           DEVICE 
ovs-if-br-ex     d434980e-ea23-4ab4-8414-289b7af44c50  ovs-interface  br-ex  
enp1s0           52060cdd-913e-4df8-9e9e-776f31647323  ethernet       enp1s0 
br-ex            950f405f-cd5c-4d51-b2ab-3d8e1e938c8b  ovs-bridge     br-ex  
ovs-if-enp7s0    0279d1c9-212c-4be8-8dfe-88a7b0b6d623  ethernet       enp7s0 
ovs-port-br-ex   3b47e5ae-a27a-4522-bea5-1fbf9c8c08eb  ovs-port       br-ex  
ovs-port-enp7s0  1baea5a3-09ee-4972-8f6b-bb8195ae46c4  ovs-port       enp7s0 
~~~

And you should be able to ping outside of the cluster:
~~~
[root@node1 ~]# ping -c1 -W1 8.8.8.8
PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.
64 bytes from 8.8.8.8: icmp_seq=1 ttl=112 time=18.5 ms

--- 8.8.8.8 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 18.506/18.506/18.506/0.000 ms
~~~

### Install container runtime engine and kubeadm (node1, node2, node3)

The following will be a brief walkthrough of what's requried to install the container runtime and kubernetes. For further details, follow the `kubeadm` documentation:
* [https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/)
* [https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/)

#### Install the container runtime

See [https://kubernetes.io/docs/setup/production-environment/container-runtimes/](https://kubernetes.io/docs/setup/production-environment/container-runtimes/) for further details.

Set up iptables:
~~~
# Create the .conf file to load the modules at bootup
cat <<EOF | sudo tee /etc/modules-load.d/crio.conf
overlay
br_netfilter
EOF

sudo modprobe overlay
sudo modprobe br_netfilter

# Set up required sysctl params, these persist across reboots.
cat <<EOF | sudo tee /etc/sysctl.d/99-kubernetes-cri.conf
net.bridge.bridge-nf-call-iptables  = 1
net.ipv4.ip_forward                 = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF

sudo sysctl --system
~~~

Then, install cri-o. At time of this writing, the latest version was 1.21:
~~~
OS=CentOS_8
VERSION=1.21
curl -L -o /etc/yum.repos.d/devel:kubic:libcontainers:stable.repo https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/$OS/devel:kubic:libcontainers:stable.repo
curl -L -o /etc/yum.repos.d/devel:kubic:libcontainers:stable:cri-o:$VERSION.repo https://download.opensuse.org/repositories/devel:kubic:libcontainers:stable:cri-o:$VERSION/$OS/devel:kubic:libcontainers:stable:cri-o:$VERSION.repo
yum install cri-o -y
~~~

Make sure to set 192.168.123.254 (gw1) as an insecure registry:
~~~
cat <<'EOF' | tee /etc/containers/registries.conf.d/999-insecure.conf
[[registry]]
location = "192.168.123.254:5000"
insecure = true
EOF
~~~

Also, make sure to remove `/etc/cni/net.d/100-crio-bridge.conf` as we do not want to fall back to crio's default networking:
~~~
mv /etc/cni/net.d/100-crio-bridge.conf /root/.
~~~
> **Note:** If you forget to move or delete this file, your CoreDNS pods will come up with an IP address in the 10.0.0.0/8 range.

Finally, start crio:
~~~
systemctl daemon-reload
systemctl enable crio --now
~~~

#### Install kubelet, kubectl, kubeadm

See [https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/#installing-kubeadm-kubelet-and-kubectl](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/#installing-kubeadm-kubelet-and-kubectl) for further details.

~~~
cat <<EOF | sudo tee /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=https://packages.cloud.google.com/yum/repos/kubernetes-el7-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://packages.cloud.google.com/yum/doc/yum-key.gpg https://packages.cloud.google.com/yum/doc/rpm-package-key.gpg
exclude=kubelet kubeadm kubectl
EOF

# Set SELinux in permissive mode (effectively disabling it)
sudo setenforce 0
sudo sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config

sudo yum install -y kubelet kubeadm kubectl --disableexcludes=kubernetes

sudo systemctl enable --now kubelet
~~~

## Deploying a cluster with OVN Kubernetes

Execute the following instructions **only** on the master node, `node1`.

### Install instructions for kubeadm

Deploy on the master node `node1`:
~~~
kubeadm init --pod-network-cidr 172.16.0.0/16 --service-cidr 172.17.0.0/16 --apiserver-advertise-address 192.168.123.1
mkdir -p $HOME/.kube
sudo cp -i /etc/kubernetes/ovn.conf $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config
~~~

Write down the join command for worker nodes - you will need it later.

You will now have a one node cluster without a CNI plugin and as such the CoreDNS pods will not start:
~~~
[root@node1 ~]# kubectl get pods -o wide -A
NAMESPACE     NAME                            READY   STATUS              RESTARTS   AGE   IP                NODE    NOMINATED NODE   READINESS GATES
kube-system   coredns-78fcd69978-dvpjg        0/1     ContainerCreating   0          21s   <none>            node1   <none>           <none>
kube-system   coredns-78fcd69978-mzpzr        0/1     ContainerCreating   0          21s   <none>            node1   <none>           <none>
kube-system   etcd-node1                      1/1     Running             2          33s   192.168.122.205   node1   <none>           <none>
kube-system   kube-apiserver-node1            1/1     Running             2          33s   192.168.122.205   node1   <none>           <none>
kube-system   kube-controller-manager-node1   1/1     Running             3          33s   192.168.122.205   node1   <none>           <none>
kube-system   kube-proxy-vm44k                1/1     Running             0          22s   192.168.122.205   node1   <none>           <none>
kube-system   kube-scheduler-node1            1/1     Running             3          28s   192.168.122.205   node1   <none>           <none>
~~~

Now, deploy OVN Kubernetes - see below.

### Deploying OVN Kubernetes on node1

Install build dependencies and create a softlink for `pip` to `pip3`:
~~~
yum install git python3-pip make podman buildah -y
ln -s $(which pip3) /usr/local/bin/pip
~~~

Install golang, for further details see [https://golang.org/doc/install](https://golang.org/doc/install):
~~~
curl -L -O https://golang.org/dl/go1.17.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go1.17.linux-amd64.tar.gz
echo "export PATH=$PATH:/usr/local/go/bin" >> ~/.bashrc
source ~/.bashrc
go version
~~~

Now, clone the OVN Kubernetes repository:
~~~
mkdir -p $HOME/work/src/github.com/ovn-org
cd $HOME/work/src/github.com/ovn-org
git clone https://github.com/ovn-org/ovn-kubernetes
cd $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/images
~~~

Build the latest ovn-daemonset image and push it to the registry. Prepare the binaries:
~~~
# Build ovn docker image
pushd ../../go-controller
make
popd

# Build ovn kube image
# Find all built executables, but ignore the 'windows' directory if it exists
find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
~~~

Now, build and push the image with:
~~~
OVN_IMAGE=192.168.123.254:5000/ovn-daemonset-f:latest
buildah bud -t $OVN_IMAGE -f Dockerfile.fedora .
podman push $OVN_IMAGE
~~~

Next, run:
~~~
OVN_IMAGE=192.168.123.254:5000/ovn-daemonset-f:latest
MASTER_IP=192.168.123.1
NET_CIDR="172.16.0.0/16/24"
SVC_CIDR="172.17.0.0/16"
./daemonset.sh --image=${OVN_IMAGE} \
    --net-cidr="${NET_CIDR}" --svc-cidr="${SVC_CIDR}" \
    --gateway-mode="local" \
    --k8s-apiserver=https://${MASTER_IP}:6443
~~~

You might also have to work around an issue where br-int is added by OVN, but the necessary files in /var/run/openvswitch are not created until Open vSwitch is restarted - [see here for more details](#issues--workarounds). This only happens on the master, so let's pre-create `br-int` there:
~~~
ovs-vsctl add-br br-int
~~~

Now, set up ovnkube:
~~~
# set up the namespace
kubectl apply -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovn-setup.yaml
# set up the database pods - wait until the pods are up and running before progressing to the next command:
kubectl apply -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-db.yaml
# set up the master pods - wait until the pods are up and running before progressing to the next command:
kubectl apply -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-master.yaml
# set up the ovnkube-node pods - wait until the pods are up and running before progressing to the next command:
kubectl apply -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-node.yaml
~~~

Once all OVN related pods are up, you should see that the CoreDNS pods have started as well and they should be in the correct network.
~~~
[root@node1 images]# kubectl get pods -A -o wide | grep coredns
kube-system      coredns-78fcd69978-ms969         1/1     Running   0          29s     172.16.0.6        node1   <none>           <none>
kube-system      coredns-78fcd69978-w6k2z         1/1     Running   0          36s     172.16.0.5        node1   <none>           <none>
~~~

Finally, delete the kube-proxy DaemonSet:
~~~
kubectl delete ds -n kube-system kube-proxy
~~~

You should now see the following when listing all pods:
~~~
[root@node1 ~]# kubectl get pods -A -o wide
NAMESPACE        NAME                             READY   STATUS    RESTARTS   AGE     IP                NODE    NOMINATED NODE   READINESS GATES
kube-system      coredns-78fcd69978-rhjgh         1/1     Running   0          10s     172.16.0.4        node1   <none>           <none>
kube-system      coredns-78fcd69978-xcxnx         1/1     Running   0          17s     172.16.0.3        node1   <none>           <none>
kube-system      etcd-node1                       1/1     Running   1          74m     192.168.122.205   node1   <none>           <none>
kube-system      kube-apiserver-node1             1/1     Running   1          74m     192.168.122.205   node1   <none>           <none>
kube-system      kube-controller-manager-node1    1/1     Running   1          74m     192.168.122.205   node1   <none>           <none>
kube-system      kube-scheduler-node1             1/1     Running   1          74m     192.168.122.205   node1   <none>           <none>
ovn-kubernetes   ovnkube-db-7767c6b7c5-25drn      2/2     Running   2          11m     192.168.122.205   node1   <none>           <none>
ovn-kubernetes   ovnkube-master-775d45fd5-mzkcb   3/3     Running   3          10m     192.168.122.205   node1   <none>           <none>
ovn-kubernetes   ovnkube-node-xmgrj               3/3     Running   3          8m49s   192.168.122.205   node1   <none>           <none>
~~~

### Verifying the deployment 

Create a test deployment to make sure that everything works as expected:
~~~
cd ~
cat <<'EOF' > fedora.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fedora-deployment
  labels:
    app: fedora-deployment
spec:
  replicas: 2
  selector:
    matchLabels:
      app: fedora-pod
  template:
    metadata:
      labels:
        app: fedora-pod
    spec:
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
      containers:
      - name: fedora
        image: fedora
        command:
          - sleep
          - infinity
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsUser: 0
          capabilities:
            add:
              - "SETFCAP"
              - "CAP_NET_RAW"
              - "CAP_NET_ADMIN"
EOF
kubectl apply -f fedora.yaml
~~~

Make sure that the pods have a correct IP address and that they can reach the outside world, e.g. by installing some software:
~~~
[root@node1 ~]# kubectl get pods -o wide
NAME                                 READY   STATUS    RESTARTS   AGE   IP           NODE    NOMINATED NODE   READINESS GATES
fedora-deployment-86f7647bd6-dllbs   1/1     Running   0          58s   172.16.0.5   node1   <none>           <none>
fedora-deployment-86f7647bd6-k42wm   1/1     Running   0          36s   172.16.0.6   node1   <none>           <none>
[root@node1 ~]# kubectl exec -it fedora-deployment-86f7647bd6-dllbs -- /bin/bash
[root@fedora-deployment-86f7647bd6-dllbs /]# yum install iputils -y
Fedora 34 - x86_64                                                                   4.2 MB/s |  74 MB     00:17    
Fedora 34 openh264 (From Cisco) - x86_64                                             1.7 kB/s | 2.5 kB     00:01    
Fedora Modular 34 - x86_64                                                           2.8 MB/s | 4.9 MB     00:01    
Fedora 34 - x86_64 - Updates                                                         3.7 MB/s |  25 MB     00:06    
Fedora Modular 34 - x86_64 - Updates                                                 2.0 MB/s | 4.6 MB     00:02    
Last metadata expiration check: 0:00:01 ago on Tue Aug 24 17:04:04 2021.
Dependencies resolved.
=====================================================================================================================
 Package                   Architecture             Version                           Repository                Size
=====================================================================================================================
Installing:
 iputils                   x86_64                   20210202-2.fc34                   fedora                   170 k

Transaction Summary
=====================================================================================================================
Install  1 Package

Total download size: 170 k
Installed size: 527 k
Downloading Packages:
iputils-20210202-2.fc34.x86_64.rpm                                                   1.2 MB/s | 170 kB     00:00    
---------------------------------------------------------------------------------------------------------------------
Total                                                                                265 kB/s | 170 kB     00:00     
Running transaction check
Transaction check succeeded.
Running transaction test
Transaction test succeeded.
Running transaction
  Preparing        :                                                                                             1/1 
  Installing       : iputils-20210202-2.fc34.x86_64                                                              1/1 
  Running scriptlet: iputils-20210202-2.fc34.x86_64                                                              1/1 
  Verifying        : iputils-20210202-2.fc34.x86_64                                                              1/1 

Installed:
  iputils-20210202-2.fc34.x86_64                                                                                     

Complete!
~~~

### Uninstalling OVN Kubernetes

In order to uninstall OVN kubernetes:
~~~
kubectl delete -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-node.yaml
kubectl delete -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-master.yaml
kubectl delete -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-db.yaml
kubectl delete -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovn-setup.yaml
~~~

### Issues / workarounds:

br-int might be added by OVN, but the files for it are not created in /var/run/openvswitch. `ovs-ofctl dump-flows br-int` fails, and one will see the following log messages among others:
~~~
2021-08-24T12:42:43.810Z|00025|rconn|WARN|unix:/var/run/openvswitch/br-int.mgmt: connection failed (No such file or directory)
~~~

The best workaroud is to pre-create br-int before the OVN Kubernetes installation:
~~~
ovs-vsctl add-br br-int
~~~

## Joining worker nodes to the environment

Finally, join your worker nodes. Set them up using [the base setup steps for the nodes](#node1-through-node3-base-setup) and the [CRI and kubeadm installation steps](#install-container-runtime-engine-and-kubeadm-node1-node2-node3). Then, use the output from the `kubeadm init` command that you ran earlier to join the node to the cluster:
~~~
kubeadm join 192.168.123.10:6443 --token <...> \
	--discovery-token-ca-cert-hash <...>
~~~

## kubeadm reset instructions

If you must reset your master and worker nodes, the following commands can be used to reset the lab environment. Run this on each node and then ideally reboot the node right after:
~~~
IF2=enp7s0
echo "y" | kubeadm reset
rm -f /etc/cni/net.d/10-*
rm -Rf ~/.kube
rm -f /etc/openvswitch/conf.db
nmcli conn del cni0
systemctl restart openvswitch
systemctl restart NetworkManager
nmcli conn up ovs-if-${IF2}
~~~

