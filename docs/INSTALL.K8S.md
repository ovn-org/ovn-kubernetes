Installing Kubernetes
=====================

On the master node, start etcd

```
docker run \
  --net=host \
  --detach \
  gcr.io/google_containers/etcd:2.0.12 \
  /usr/local/bin/etcd \
    --addr=127.0.0.1:4001 \
    --bind-addr=0.0.0.0:4001 \
    --data-dir=/var/etcd/data
```

Download the latest stable kubernetes.tar.gz from:
https://github.com/kubernetes/kubernetes/releases

Untar the file and look for kubernetes-server-linux-amd64.tar.gz.  Untar that
file too.  Copy kube-apiserver, kube-controller-manager, kube-scheduler,
kubelet and kubectl to a directory.

On the master node, start the following daemons: kube-apiserver,
kube-controller-manager, kube-scheduler.  You can start first two of them in
two modes: plain text HTTP or HTTPS.  For plain text use the following
commands:

* kube-apiserver
```
nohup ./kube-apiserver \
  --service-cluster-ip-range=192.168.200.0/24 \
  --address=0.0.0.0 \
  --etcd-servers=http://127.0.0.1:4001 \
  --v=2 \
  2>&1 > /dev/null &
```

* kube-controller-manager
```
nohup ./kube-controller-manager \
  --master=127.0.0.1:8080 \
  --v=2 \
  2>&1 > /dev/null &
```

For HTTPS use, the certificates and token should be generated beforehand.
Please refer to [this
document](https://coreos.com/kubernetes/docs/latest/openssl.html) on how to
generate necessary certificates.  And to [this
document](http://kubernetes.io/docs/admin/authentication/) on how to create
static token files.

After the preparations the following commands should be run:

* kube-apiserver
```
nohup ./kube-apiserver \
  --service-cluster-ip-range=192.168.200.0/24 \
  --address=0.0.0.0 \
  --etcd-servers=http://127.0.0.1:4001 \
  --v=2 \
  --secure-port=443 \
  --tls-cert-file=/etc/kubernetes/ssl/apiserver.pem \
  --tls-private-key-file=/etc/kubernetes/ssl/apiserver-key.pem \
  --client-ca-file=/etc/kubernetes/ssl/ca.pem \
  --service-account-key-file=/etc/kubernetes/ssl/apiserver-key.pem \
  --token-auth-file=/etc/kubernetes/auth/token.csv \
  2>&1 > /dev/null &
```

* kube-controller-manager
```
nohup ./kube-controller-manager \
  --master=http://127.0.0.1:8080 \
  --v=2 \
  --service-account-private-key-file=/etc/kubernetes/ssl/apiserver-key.pem
  --root-ca-file=/etc/kubernetes/ssl/ca.pem
  2>&1 > /dev/null &
```

* kube-scheduler
```
nohup ./kube-scheduler \
  --master=127.0.0.1:8080 \
  --v=2 \
  2>&1 > /dev/null &
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
nohup ./kubelet \
  --api-servers=http://10.33.74.22:8080 \
  --v=2 \
  --address=0.0.0.0 \
  --enable-server=true \
  --network-plugin=cni \
  --network-plugin-dir=/etc/cni/net.d \
  2>&1 > /dev/null &
```

If kube-apiserver and kube-controller-manager were started in HTTPS mode run
the following commands:
```
echo "apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    certificate-authority: /etc/kubernetes/ssl/ca.pem
users:
- name: kubelet
  user:
    client-certificate: /etc/kubernetes/ssl/worker.pem
    client-key: /etc/kubernetes/ssl/worker-key.pem
contexts:
- context:
    cluster: local
    user: kubelet
  name: kubelet-context
current-context: kubelet-context" > /etc/kubernetes/worker-kubeconfig.yaml

nohup ./kubelet \
  --api-servers=https://10.33.74.22 \
  --v=2 \
  --address=0.0.0.0 \
  --enable-server=true \
  --network-plugin=cni \
  --network-plugin-dir=/etc/cni/net.d \
  --kubeconfig=/etc/kubernetes/worker-kubeconfig.yaml \
  --tls-cert-file=/etc/kubernetes/ssl/worker.pem \
  --tls-private-key-file=/etc/kubernetes/ssl/worker-key.pem \
  2>&1 > /dev/null &
```

You can then verify that all your nodes are registered by running the
following on the master node.

```
./kubectl get nodes
```
