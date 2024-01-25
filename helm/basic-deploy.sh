#!/usr/bin/env bash
 
# check docker
docker info >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    echo "docker not found"
    exit
fi

# check kubectl
kubectl cluster-info >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    echo "kubectl not found"
    exit
fi

# check kind
kind version >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    echo "kind not found"
    exit
fi

# check go
go version >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    echo "go not found"
    exit
fi

# Ensure that kind network does not have ip6 enabled
enabled_ipv6=$(docker network inspect kind -f '{{.EnableIPv6}}' 2>/dev/null)
[ "${enabled_ipv6}" = "false" ] || {
   docker network rm kind 2>/dev/null || :
   docker network create kind -o "com.docker.network.bridge.enable_ip_masquerade"="true" -o "com.docker.network.driver.mtu"="1500"
}
[ "${enabled_ipv6}" = "false" ] || { 2&>1 echo the kind network is not what we expected ; exit 1; }

set -euxo pipefail
# increate fs.inotify.max_user_watches
sudo sysctl fs.inotify.max_user_watches=524288
# increase fs.inotify.max_user_instances
sudo sysctl fs.inotify.max_user_instances=512
# build image
cd ../dist/images
make ubuntu
docker tag ovn-kube-u:latest ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:master
# create a cluster, with 1 controller and 1 worker node
kind_cluster_name=ovn-helm
cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOT
kind delete clusters $kind_cluster_name
kind create cluster --name $kind_cluster_name --config /tmp/kind.yaml
kind load docker-image --name $kind_cluster_name ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:master
cd ../../helm/ovn-kubernetes
helm install ovn-kubernetes . -f values.yaml \
    --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
    --set ovnkube-identity.replicas=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l) \
    --set global.image.repository=ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u --set global.image.tag=master
