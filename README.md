# How to use Open Virtual Networking with Kubernetes 

On Linux, the easiest way to get started is to use OVN DaemonSet and Deployments.

## Install Open vSwitch kernel modules on all hosts.

Most Linux distributions come with Open vSwitch kernel module by default.  You
can check its existence with `modinfo openvswitch`.  The features that OVN
needs are only available in kernel 4.6 and greater. But, you can also install
Open vSwitch kernel module from the Open vSwitch repository to get all the
features OVN needs (and any possible bug fixes) for any kernel.

To install Open vSwitch kernel module from Open vSwitch repo manually, please
read [INSTALL.rst](https://docs.openvswitch.org/en/latest/intro/install/). 

## Run DaemonSet and Deployment

Create OVN StatefulSet, DaemonSet and Deployment yamls from templates by running the commands below:
(The $MASTER_IP below is the IP address of the machine where kube-apiserver is
running).

```
# Clone ovn-kubernetes repo
mkdir -p $HOME/work/src/github.com/ovn-org
cd $HOME/work/src/github.com/ovn-org
git clone https://github.com/ovn-org/ovn-kubernetes
cd $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/images
./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-u:latest \
    --net-cidr=192.168.0.0/16/24 --svc-cidr=172.16.1.0/24 \
    --gateway-mode="local" \
    --k8s-apiserver=https://$MASTER_IP:6443
```

Take note that the image `docker.io/ovnkube/ovn-daemonset-u:latest` is horribly outdated. You should [build your own image](#building-the-daemonset-container) and use that instead.

To set specific logging level for OVN components, pass the related parameter from the below mentioned
list to the above command. Set values are the default values.
```
    --master-loglevel="5" \\Log level for ovnkube (master)
    --node-loglevel="5" \\ Log level for ovnkube (node)
    --dbchecker-loglevel="5" \\Log level for ovn-dbchecker (ovnkube-db)
    --ovn-loglevel-northd="-vconsole:info -vfile:info" \\ Log config for ovn northd
    --ovn-loglevel-nb="-vconsole:info -vfile:info" \\ Log config for northbound db
    --ovn-loglevel-sb="-vconsole:info -vfile:info" \\ Log config for southboudn db
    --ovn-loglevel-controller="-vconsole:info" \\ Log config for ovn-controller
    --ovn-loglevel-nbctld="-vconsole:info" \\ Log config for nbctl daemon
```

If you are not running OVS directly in the nodes, you must apply the OVS Daemonset yaml.
```
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovs-node.yaml
```

Apply OVN DaemonSet and Deployment yamls.

```
# Create OVN namespace, service accounts, ovnkube-db headless service, configmap, and policies
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovn-setup.yaml

# Optionally, if you plan to use the Egress IPs or EgressFirewall features, create the corresponding CRDs:
# create egressips.k8s.ovn.org CRD
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/k8s.ovn.org_egressips.yaml
# create egressfirewalls.k8s.ovn.org CRD
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/k8s.ovn.org_egressfirewalls.yaml

# Run ovnkube-db deployment.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-db.yaml

# Run ovnkube-master deployment.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-master.yaml

# Run ovnkube daemonset for nodes
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-node.yaml
```

NOTE: You don't need kube-proxy for OVN to work. You can delete that from your
cluster.

## Building the Daemonset container

Install build dependencies. If needed, create a softlink for `pip` to `pip3`:
~~~
ln -s $(which pip3) /usr/local/bin/pip
~~~

Now, clone the OVN Kubernetes repository, build the binaries, and build and push your image to your registry:
~~~
mkdir -p $HOME/work/src/github.com/ovn-org
cd $HOME/work/src/github.com/ovn-org
git clone https://github.com/ovn-org/ovn-kubernetes
cd $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/images

# Build ovn docker image
pushd ../../go-controller
make
popd

# Build ovn kube image
# Find all built executables, but ignore the 'windows' directory if it exists
find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
~~~

Now, build the image with:
~~~
OVN_IMAGE=<registry>/ovn-daemonset-f:latest
docker build -t $OVN_IMAGE -f Dockerfile.fedora . 
docker push $OVN_IMAGE
# or for buildah/podman:
# buildah bud -t $OVN_IMAGE -f Dockerfile.fedora .
# podman push $OVN_IMAGE
~~~
