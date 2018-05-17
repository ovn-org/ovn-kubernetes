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
wget -nv https://github.com/containernetworking/cni/releases/download/v0.5.2/cni-amd64-v0.5.2.tgz
popd
sudo mkdir -p /opt/cni/bin
pushd /opt/cni/bin
sudo tar xvzf ~/cni-amd64-v0.5.2.tgz
popd

source ~/setup_minion_args.sh
# Create a kubeconfig file.
cat << KUBECONFIG >> ~/kubeconfig.yaml
apiVersion: v1
clusters:
- cluster:
    server: http://$MASTER_OVERLAY_IP:8080
  name: default-cluster
- cluster:
    server: http://$MASTER_OVERLAY_IP:8080
  name: local-server
- cluster:
    server: http://$MASTER_OVERLAY_IP:8080
  name: ubuntu
contexts:
- context:
    cluster: ubuntu
    user: ubuntu
  name: ubuntu
current-context: ubuntu
kind: Config
preferences: {}
users:
- name: ubuntu
  user:
    password: p1NVMZqhOOOqkWQq
    username: admin
KUBECONFIG

# Start k8s daemons
pushd k8s/server/kubernetes/server/bin
echo "Starting kubelet ..."
nohup sudo ./kubelet --kubeconfig $HOME/kubeconfig.yaml \
                     --v=2 --address=0.0.0.0 \
                     --fail-swap-on=false \
                     --runtime-cgroups=/systemd/system.slice \
                     --kubelet-cgroups=/systemd/system.slice \
                     --enable-server=true --network-plugin=cni \
                     --cni-conf-dir=/etc/cni/net.d \
                     --cni-bin-dir="/opt/cni/bin/" >/tmp/kubelet.log 2>&1 0<&- &
popd

# Initialize the minion and gateway.
if [ $PROTOCOL = "ssl" ]; then
  SSL_ARGS="-nb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -nb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -nb-client-cacert /etc/openvswitch/ovnnb-ca.cert \
    -sb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -sb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -sb-client-cacert /etc/openvswitch/ovnsb-ca.cert"
fi

nohup sudo ovnkube -k8s-kubeconfig $HOME/kubeconfig.yaml -loglevel=4 \
    -logfile="/var/log/openvswitch/ovnkube.log" \
    -k8s-apiserver="http://$MASTER_OVERLAY_IP:8080" \
    -init-node="$MINION_NAME"  \
    -nodeport \
    -nb-address="$PROTOCOL://$MASTER_OVERLAY_IP:6631" \
    -sb-address="$PROTOCOL://$MASTER_OVERLAY_IP:6632" \
    ${SSL_ARGS} \
    -k8s-token="test" \
    -init-gateways -gateway-interface=enp0s9 -gateway-nexthop="$GW_IP" \
    -service-cluster-ip-range=172.16.1.0/24 \
    -cluster-subnet="192.168.0.0/16" 2>&1 &

# Restore xtrace
$XTRACE
