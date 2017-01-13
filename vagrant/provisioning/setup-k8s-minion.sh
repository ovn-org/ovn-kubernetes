#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# args
# $1: IP of master host

MASTER_IP=$1

# Install k8s

# Download Kubernetes ... yes, it's huge
#mkdir k8s
#pushd k8s
#wget https://github.com/kubernetes/kubernetes/releases/download/v1.3.7/kubernetes.tar.gz
#tar xvzf kubernetes.tar.gz

# Now untar kubernetes-server-linux-amd64.tar.gz
#mkdir server
#cd server
#tar xvzf ../kubernetes/server/linux/kubernetes-server-linux-amd64.tar.gz
#popd

# Install CNI
pushd ~/
wget https://github.com/containernetworking/cni/releases/download/v0.2.0/cni-v0.2.0.tgz
popd
sudo mkdir -p /opt/cni/bin
pushd /opt/cni/bin
sudo tar xvzf ~/cni-v0.2.0.tgz
popd

# Start k8s daemons
pushd k8s/server/kubernetes/server/bin
echo "Starting kubelet ..."
nohup sudo ./kubelet --api-servers=http://$MASTER_IP:8080 --v=2 --address=0.0.0.0 \
                     --enable-server=true --network-plugin=cni \
                     --network-plugin-dir=/etc/cni/net.d 2>&1 0<&- &>/dev/null &
sleep 5
popd

# Restore xtrace
$XTRACE
