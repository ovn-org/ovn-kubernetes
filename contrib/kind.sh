#!/usr/bin/env bash

set -euxo pipefail

# Detect IP to use as API server
API_IP=$(ip -4 addr | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | grep -v "127.0.0.1" | head -n 1)
if [ -z "$API_IP" ]; then
  echo "Error detecting machine IP to use as API server"
  exit 1
fi

sed -i "s/apiServerAddress.*/apiServerAddress: ${API_IP}/" kind.yaml

# Create KIND cluster
kind create cluster --kubeconfig ${HOME}/admin.conf --image kindest/node:v1.16.4 --config=./kind.yaml
export KUBECONFIG=${HOME}/admin.conf
mkdir -p /tmp/kind
sudo chmod 777 /tmp/kind
echo $(kubectl get secrets -o jsonpath='{.items[].data.ca\.crt}') > /tmp/kind/ca.crt
echo $(kubectl get secrets -o jsonpath='{.items[].data.token}') > /tmp/kind/token
pushd ../go-controller
make
popd
pushd ../dist/images
sudo cp -f ../../go-controller/_output/go/bin/* .
echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
docker build -t ovn-daemonset-f:dev -f Dockerfile.fedora .
docker save ovn-daemonset-f:dev -o /tmp/kind/image.tar.gz
./daemonset.sh --image=docker.io/library/ovn-daemonset-f:dev --net-cidr=10.244.0.0/16 --svc-cidr=10.96.0.0/12 --gateway-mode="local" --k8s-apiserver=https://${API_IP}:11337
popd
for container in $(docker ps  | grep kindest/node | awk '{print $NF}'); do 
  docker exec $container ctr --namespace=k8s.io images import /var/run/secrets/kubernetes.io/serviceaccount/image.tar.gz
done
pushd ../dist/yaml
kubectl create -f ovn-setup.yaml
kubectl create -f ovnkube-db.yaml
kubectl create -f ovnkube-master.yaml
kubectl create -f ovnkube-node.yaml
popd



