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
nohup sudo ./kubelet --api-servers=http://$MASTER_IP:8080 --v=2 --address=0.0.0.0 \
                     --enable-server=true --network-plugin=cni \
                     --cni-conf-dir=/etc/cni/net.d \
                     --cni-bin-dir="/opt/cni/bin/" 2>&1 0<&- &>/dev/null &
sleep 10
popd

# Initialize the minion and gateway.
if [ $PROTOCOL = "ssl" ]; then
sudo ovnkube -kubeconfig $HOME/kubeconfig.yaml -v=4 -alsologtostderr \
    -apiserver="http://$MASTER_OVERLAY_IP:8080" \
    -init-node="$MINION_NAME"  \
    -ovn-north-db="$PROTOCOL://$MASTER_OVERLAY_IP:6631" \
    -ovn-south-db="$PROTOCOL://$MASTER_OVERLAY_IP:6632" --token="test" \
    -ovn-north-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -ovn-north-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -ovn-north-client-cacert /etc/openvswitch/ovnnb-ca.cert \
    -ovn-south-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -ovn-south-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -ovn-south-client-cacert /etc/openvswitch/ovnsb-ca.cert \
    -init-gateways -gateway-interface=enp0s9 -gateway-nexthop="$GW_IP" \
    -cluster-subnet="192.168.0.0/16" 2>&1
else
sudo ovnkube -kubeconfig $HOME/kubeconfig.yaml -v=4 -alsologtostderr \
    -apiserver="http://$MASTER_OVERLAY_IP:8080" \
    -init-node="$MINION_NAME"  \
    -ovn-north-db="$PROTOCOL://$MASTER_OVERLAY_IP:6631" \
    -ovn-south-db="$PROTOCOL://$MASTER_OVERLAY_IP:6632" --token="test" \
    -init-gateways -gateway-interface=enp0s9 -gateway-nexthop="$GW_IP" \
    -cluster-subnet="192.168.0.0/16" 2>&1
fi

# Start the gateway helper.
sudo ovn-k8s-gateway-helper --physical-bridge=brenp0s9 \
            --physical-interface=enp0s9 --pidfile --detach

# Restore xtrace
$XTRACE
