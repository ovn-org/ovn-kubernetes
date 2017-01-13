#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# ARGS:
# $1: IP of second interface of master
# $2: IP of third interface of master
# $3: Hostname of the master
# $4: Master switch subnet

OVERLAY_IP=$1
GW_IP=$2
MASTER_NAME=$3
MASTER_SUBNET=$4

# FIXME(mestery): Remove once Vagrant boxes allow apt-get to work again
sudo rm -rf /var/lib/apt/lists/*

# First, install docker
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates
sudo apt-key adv --keyserver hkp://p80.pool.sks-keyservers.net:80 --recv-keys 58118E89F3A912897C070ADBF76221572C52609D
sudo su -c "echo \"deb https://apt.dockerproject.org/repo ubuntu-xenial main\" >> /etc/apt/sources.list.d/docker.list"
sudo apt-get update
sudo apt-get purge lxc-docker
sudo apt-get install -y linux-image-extra-$(uname -r) linux-image-extra-virtual
sudo apt-get install -y docker-engine
sudo service docker start

# Install OVS and dependencies
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
             ovn-central_2.6.90-1_amd64.deb ovn-common_2.6.90-1_amd64.deb \
             python-openvswitch_2.6.90-1_all.deb ovn-docker_2.6.90-1_amd64.deb \
             ovn-host_2.6.90-1_amd64.deb

# Start the daemons
sudo /etc/init.d/openvswitch-switch force-reload-kmod
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_northd
sudo /usr/share/openvswitch/scripts/ovn-ctl start_northd

sudo ovn-nbctl set-connection ptcp:6641
sudo ovn-sbctl set-connection ptcp:6642

sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="tcp:$OVERLAY_IP:6642" \
                                  external_ids:ovn-nb="tcp:$OVERLAY_IP:6641" \
                                  external_ids:ovn-encap-ip=$OVERLAY_IP \
                                  external_ids:ovn-encap-type=geneve

# Re-start OVN controller
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_controller
sudo /usr/share/openvswitch/scripts/ovn-ctl start_controller

# Set k8s API server IP
sudo ovs-vsctl set Open_vSwitch . external_ids:k8s-api-server="0.0.0.0:8080"

# Install OVN+K8S Integration
sudo apt-get install -y python-pip
sudo -H pip install --upgrade pip
git clone https://github.com/openvswitch/ovn-kubernetes
pushd ovn-kubernetes
sudo -H pip install .
popd

# Initialize the master
sudo ovn-k8s-overlay master-init --cluster-ip-subnet="192.168.0.0/16" \
                                 --master-switch-subnet="$MASTER_SUBNET" \
                                 --node-name="$MASTER_NAME"

# Restore xtrace
$XTRACE
