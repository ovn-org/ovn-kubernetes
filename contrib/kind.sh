#!/usr/bin/env bash

set -euxo pipefail

K8S_VERSION=${K8S_VERSION:-v1.16.4}
KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
KIND_HA=${KIND_HA:-false}
if [ "$KIND_HA" == true ]; then
  DEFAULT_KIND_CONFIG=./kind-ha.yaml
else
  DEFAULT_KIND_CONFIG=./kind.yaml
fi
KIND_CONFIG=${KIND_CONFIG:-$DEFAULT_KIND_CONFIG}
KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}

# Detect IP to use as API server
API_IP=$(ip -4 addr | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | grep -v "127.0.0.1" | head -n 1)
if [ -z "$API_IP" ]; then
  echo "Error detecting machine IP to use as API server"
  exit 1
fi

sed -i "s/apiServerAddress.*/apiServerAddress: ${API_IP}/" ${KIND_CONFIG}

# Create KIND cluster
CLUSTER_NAME=${CLUSTER_NAME:-ovn}
kind create cluster --name ${CLUSTER_NAME} --kubeconfig ${HOME}/admin.conf --image kindest/node:${K8S_VERSION} --config=${KIND_CONFIG}
export KUBECONFIG=${HOME}/admin.conf
mkdir -p /tmp/kind
sudo chmod 777 /tmp/kind
count=0
until kubectl get secrets -o jsonpath='{.items[].data.ca\.crt}'
do
  if [ $count -gt 10 ]; then
    echo "Failed to get k8s crt/token"
    exit 1
  fi
  count=$((count+1))
  echo "secrets not available on attempt $count"
  sleep 5
done
kubectl get secrets -o jsonpath='{.items[].data.ca\.crt}' > /tmp/kind/ca.crt
kubectl get secrets -o jsonpath='{.items[].data.token}' > /tmp/kind/token
pushd ../go-controller
make
popd
pushd ../dist/images
sudo cp -f ../../go-controller/_output/go/bin/* .
echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
docker build -t ovn-daemonset-f:dev -f Dockerfile.fedora .
./daemonset.sh --image=docker.io/library/ovn-daemonset-f:dev --net-cidr=10.244.0.0/16 --svc-cidr=10.96.0.0/12 --gateway-mode="local" --k8s-apiserver=https://${API_IP}:11337 --kind --master-loglevel=5
popd
kind load docker-image ovn-daemonset-f:dev --name ${CLUSTER_NAME}
pushd ../dist/yaml
kubectl create -f ovn-setup.yaml
CONTROL_NODES=$(docker ps -f name=ovn-control | grep -v NAMES | awk '{ print $NF }')
for n in $CONTROL_NODES; do
  kubectl label node $n k8s.ovn.org/ovnkube-db=true
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    kubectl taint node $n node-role.kubernetes.io/master:NoSchedule-
  fi
done
if [ "$KIND_HA" == true ]; then
  kubectl create -f ovnkube-db-raft.yaml
else
  kubectl create -f ovnkube-db.yaml
fi
kubectl create -f ovnkube-master.yaml
kubectl create -f ovnkube-node.yaml
popd
kubectl -n kube-system delete ds kube-proxy
kind get clusters
kind get nodes --name ${CLUSTER_NAME}
kind export kubeconfig --name ovn
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  kubectl create -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/master/deploy/static/mandatory.yaml
fi
