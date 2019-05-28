# How to use Open Virtual Networking with Kubernetes

The easiest way to get started is to use OVN daemonsets.

## Install Open vSwitch kernel modules on all hosts.

Most Linux distributions come with openvswitch kernel module by default.  You
can check its existence with `modinfo openvswitch`.  The features that OVN
needs are only available in kernel 4.6 and greater. But, you can also install
openvswitch kernel module from the openvswitch repository to get all the
features OVN needs (and any possible bug fixes) for any kernel.

To install openvswitch kernel module from openvswitch repo manually, please
read [INSTALL.rst].  For a quick start on Ubuntu,  you can install
the kernel module package by:

```
sudo apt-get install apt-transport-https
echo "deb http://18.191.116.101/openvswitch/stable /" |  sudo tee /etc/apt/sources.list.d/openvswitch.list
wget -O - http://18.191.116.101/openvswitch/keyFile |  sudo apt-key add -
sudo apt-get update
sudo apt-get install openvswitch-datapath-dkms -y
```

## Run daemonsets

On Kubernetes master, label it to run daemonsets. $NODENAME below is master's
node name.

```
kubectl taint nodes --all node-role.kubernetes.io/master-
kubectl label node $NODENAME node-role.kubernetes.io/master=true --overwrite
```

Create OVN daemonset yamls from templates by:
(The $MASTER_IP below is the IP address of the machine where kube-apiserver is
running)

```
# Clone ovn-kubernetes repo
mkdir -p $HOME/work/src/github.com/ovn-org
cd $HOME/work/src/github.com/ovn-org
git clone https://github.com/ovn-org/ovn-kubernetes
cd $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/images
./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-u:latest \
    --net-cidr=192.168.0.0/16 --svc-cidr=172.16.1.0/24 \
    --k8s-apiserver=https://$MASTER_IP:6443
```

Apply OVN daemonset yamls.

```
# Create OVN namespace, service accounts, ovnkube-db headless service, configmap, and policies
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovn-setup.yaml

# Run ovnkube-db daemonset.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-db.yaml

# Run ovnkube-master daemonset.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-master.yaml

# Run ovnkube daemonsets for nodes
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-node.yaml
```

## Manual installation and Vagrant

If you want to understand what daemonsets run internally, please read
[MANUAL.md] or read the scripts in the vagrant directory of this repo.

[INSTALL.rst]: http://docs.openvswitch.org/en/latest/intro/install
[INSTALL.UBUNTU.md]: docs/INSTALL.UBUNTU.md
[MANUAL.md]: README_MANUAL.md
