#!/usr/bin/env bash

set -euxo pipefail

# Set default values
CLUSTER_NAME=${CLUSTER_NAME:-ovn}
K8S_VERSION=${K8S_VERSION:-v1.17.2}
KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
KIND_HA=${KIND_HA:-false}
if [ "$KIND_HA" == true ]; then
  DEFAULT_KIND_CONFIG=./kind-ha.yaml
else
  DEFAULT_KIND_CONFIG=./kind-noha.yaml
fi
KIND_CONFIG=${KIND_CONFIG:-$DEFAULT_KIND_CONFIG}
KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
IPV4_SUPPORT=${IPV4_SUPPORT:-true}
IPV6_SUPPORT=${IPV6_SUPPORT:-false}

# Input not currently validated. Modify outside script at your own risk.
NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/12}
NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd01::/24}
SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd02::/24}

# Make a copy of the kind-xxx.yaml file so it can be edited in place.
KIND_CONFIG_LCL=kind.yaml
cp ${KIND_CONFIG} ${KIND_CONFIG_LCL}

# Detect IP to use as API server
API_IPV4=""
if [ "$IPV4_SUPPORT" == true ]; then
  # ip -4 addr -> Run ip command for IPv4
  # grep -oP '(?<=inet\s)\d+(\.\d+){3}' -> Use only the lines with the
  #   IPv4 Addresses and strip off the trailing subnet mask, /xx
  # grep -v "127.0.0.1" -> Remove local host
  # head -n 1 -> Of the remaining, use first entry
  API_IPV4=$(ip -4 addr | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | grep -v "127.0.0.1" | head -n 1)
  if [ -z "$API_IPV4" ]; then
    echo "Error detecting machine IPv4 to use as API server"
    exit 1
  fi
fi

API_IPV6=""
if [ "$IPV6_SUPPORT" == true ]; then
  # ip -6 addr -> Run ip command for IPv6
  # grep "inet6" -> Use only the lines with the IPv6 Address
  # sed 's@/.*@@g' -> Strip off the trailing subnet mask, /xx
  # grep -v "::1" -> Remove local host
  # sed '/^fe80:/ d' -> Remove Link-Local Addresses
  # head -n 1 -> Of the remaining, use first entry
  API_IPV6=$(ip -6 addr  | grep "inet6" | awk -F' ' '{print $2}' | \
             sed 's@/.*@@g' | grep -v "::1" | sed '/^fe80:/ d' | head -n 1)
  if [ -z "$API_IPV6" ]; then
    echo "Error detecting machine IPv6 to use as API server"
    exit 1
  fi
fi

if [ "$IPV4_SUPPORT" == true ] && [ "$IPV6_SUPPORT" == false ]; then
  API_IP=${API_IPV4}
  NET_CIDR=$NET_CIDR_IPV4
  SVC_CIDR=$SVC_CIDR_IPV4
  echo "IPv4 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
elif [ "$IPV4_SUPPORT" == false ] && [ "$IPV6_SUPPORT" == true ]; then
  API_IP=${API_IPV6}
  NET_CIDR=$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV6
  echo "IPv6 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"

  # Bail for now
  echo "Invalid setup. IPv6 single stack not currently supported."
  exit 1
elif [ "$IPV4_SUPPORT" == true ] && [ "$IPV6_SUPPORT" == true ]; then
  # Dual stack controls
  K8S_VERSION_MIN=v1.18.0

  if [ "$K8S_VERSION" != $K8S_VERSION_MIN ]; then
    echo "Dual Stack Support requires Kubernetes $K8S_VERSION_MIN"
    exit 1
  fi

  # Run local script to add additional settings to ${KIND_CONFIG_LCL}
  ./dualstack.sh ${KIND_CONFIG_LCL}

  # TODO DUALSTACK: Multiple IP Addresses for APIServer not currently supported.
  #API_IP=${API_IPV4},${API_IPV6}
  API_IP=${API_IPV4}
  NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
  echo "Dual Stack Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
else
  echo "Invalid setup. IPV4_SUPPORT and/or IPV6_SUPPORT must be true."
  exit 1
fi

sed -i "s/apiServerAddress.*/apiServerAddress: ${API_IP}/" ${KIND_CONFIG_LCL}

# Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
kind create cluster --name ${CLUSTER_NAME} --kubeconfig ${HOME}/admin.conf --image kindest/node:${K8S_VERSION} --config=${KIND_CONFIG_LCL}
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
./daemonset.sh --image=docker.io/library/ovn-daemonset-f:dev --net-cidr=${NET_CIDR} --svc-cidr=${SVC_CIDR} --gateway-mode="local" --k8s-apiserver=https://${API_IP}:11337 --kind --master-loglevel=5
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
