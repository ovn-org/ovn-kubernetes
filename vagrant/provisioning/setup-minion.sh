#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# ARGS:
# $1: IP of second interface of master
# $2: IP of second interface of minion
# $3: IP of third interface of master
# $4: Hostname of specific minion
# $5: Subnet to use

MASTER_OVERLAY_IP=$1
MINION_OVERLAY_IP=$2
PUBLIC_IP=$3
PUBLIC_SUBNET_MASK=$4
MINION_NAME=$5
MINION_SUBNET=$6
GW_IP=$7

# Install OVS and dependencies
# FIXME(mestery): Remove once Vagrant boxes allow apt-get to work again
sudo rm -rf /var/lib/apt/lists/*
sudo apt-get update
sudo apt-get build-dep dkms
sudo apt-get install -y graphviz autoconf automake bzip2 debhelper dh-autoreconf \
                        libssl-dev libtool openssl procps python-all \
                        python-twisted-conch python-zopeinterface python-six dkms

git clone https://github.com/openvswitch/ovs.git
pushd ovs/
sudo DEB_BUILD_OPTIONS='nocheck parallel=2' fakeroot debian/rules binary

# Install OVS/OVN debs
popd
sudo dpkg -i openvswitch-datapath-dkms_2.6.90-1_all.deb
sudo dpkg -i openvswitch-switch_2.6.90-1_amd64.deb openvswitch-common_2.6.90-1_amd64.deb \
             ovn-common_2.6.90-1_amd64.deb python-openvswitch_2.6.90-1_all.deb \
             ovn-docker_2.6.90-1_amd64.deb ovn-host_2.6.90-1_amd64.deb

# Start the daemons
sudo /etc/init.d/openvswitch-switch force-reload-kmod

sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="tcp:$MASTER_OVERLAY_IP:6642" \
                                  external_ids:ovn-nb="tcp:$MASTER_OVERLAY_IP:6641" \
                                  external_ids:ovn-encap-ip=$MINION_OVERLAY_IP \
                                  external_ids:ovn-encap-type=geneve

# Re-start OVN controller
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_controller
sudo /usr/share/openvswitch/scripts/ovn-ctl start_controller

# Set k8s API server IP
sudo ovs-vsctl set Open_vSwitch . external_ids:k8s-api-server="$MASTER_OVERLAY_IP:8080"

# Install OVN+K8S Integration
sudo apt-get install -y python-pip
sudo -H pip install --upgrade pip
git clone https://github.com/openvswitch/ovn-kubernetes
pushd ovn-kubernetes
sudo -H pip install .
popd

# Initialize the minion
sudo ovn-k8s-overlay minion-init --cluster-ip-subnet="192.168.0.0/16" \
                                 --minion-switch-subnet="$MINION_SUBNET" \
                                 --node-name="$MINION_NAME"

# Create a OVS physical bridge and move IP address of enp0s9 to br-enp0s9
echo "Creating physical bridge ..."
sudo ovs-vsctl add-br br-enp0s9
sudo ovs-vsctl add-port br-enp0s9 enp0s9
sudo ip addr flush dev enp0s9
sudo ifconfig br-enp0s9 $PUBLIC_IP netmask $PUBLIC_SUBNET_MASK up

# Start a gateway
sudo ovn-k8s-overlay gateway-init --cluster-ip-subnet="192.168.0.0/16" \
                                 --bridge-interface br-enp0s9 \
                                 --physical-ip $PUBLIC_IP/$PUBLIC_SUBNET_MASK \
                                 --node-name="$MINION_NAME" --default-gw $GW_IP

# Start the gateway helper.
sudo ovn-k8s-gateway-helper --physical-bridge=br-enp0s9 \
            --physical-interface=enp0s9 --pidfile --detach


# Restore xtrace
$XTRACE
