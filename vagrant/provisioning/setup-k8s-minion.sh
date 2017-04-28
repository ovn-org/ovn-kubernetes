#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# args
# $1: IP of master host

MASTER_IP=$1

echo "MASTER_IP=$MASTER_IP" >> setup_minion_args.sh

# Install CNI
pushd ~/
wget https://github.com/containernetworking/cni/releases/download/v0.5.2/cni-amd64-v0.5.2.tgz
popd
sudo mkdir -p /opt/cni/bin
pushd /opt/cni/bin
sudo tar xvzf ~/cni-amd64-v0.5.2.tgz
popd

# Start k8s daemons
pushd k8s/server/kubernetes/server/bin
echo "Starting kubelet ..."
nohup sudo ./kubelet --api-servers=http://$MASTER_IP:8080 --v=2 --address=0.0.0.0 \
                     --enable-server=true --network-plugin=cni \
                     --cni-conf-dir=/etc/cni/net.d \
                     --cni-bin-dir="/opt/cni/bin/" 2>&1 0<&- &>/dev/null &
sleep 5
popd

# Restore xtrace
$XTRACE
