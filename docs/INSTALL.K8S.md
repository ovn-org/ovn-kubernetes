Installing Kubernetes
=====================

On the master node, start etcd

```
docker run --net=host -d gcr.io/google_containers/etcd:2.0.12 /usr/local/bin/etcd --addr=127.0.0.1:4001 --bind-addr=0.0.0.0:4001 --data-dir=/var/etcd/data
```

Download the latest stable kubernetes.tar.gz from: https://github.com/kubernetes/kubernetes/releases

Untar the file and look for kubernetes-server-linux-amd64.tar.gz.
Untar that file too.  Copy kube-apiserver, kube-controller-manager,
kube-scheduler, kubelet and kubectl to a directory.

On the master node, start the following daemons.

```
nohup ./kube-apiserver --service-cluster-ip-range=192.168.200.0/24 \
--address=0.0.0.0 --etcd-servers=http://127.0.0.1:4001 \
--v=2  2>&1 > /dev/null &

nohup ./kube-controller-manager --master=127.0.0.1:8080 \
--v=2 2>&1 > /dev/null &

nohup ./kube-scheduler --master=127.0.0.1:8080 --v=2 2>&1 > /dev/null &
```

On the minions, you need to download a few upstream CNI plugins (as root or
using sudo)

```
mkdir -p /opt/cni/bin && cd /opt/cni/bin
wget https://github.com/containernetworking/cni/releases/download/v0.2.0/cni-v0.2.0.tgz
tar xfz cni-v0.2.0.tgz
```

On minions, start the kubelet specifying that the network plugin is of type
CNI and the network plugin directory to be /etc/cni/net.d. e.g:

```
nohup ./kubelet --api-servers=http://10.33.74.22:8080 --v=2 --address=0.0.0.0 \
--enable-server=true --network-plugin=cni --network-plugin-dir=/etc/cni/net.d  2>&1 > /dev/null &
```

You can then verify that all your nodes are registered by running the following
on the master node.

```
./kubectl get nodes
```
