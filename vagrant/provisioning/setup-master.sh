#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# ARGS:
# $1: IP of second interface of master
# $2: Hostname of the master
# $3: Master switch subnet

OVERLAY_IP=$1
MASTER_NAME=$2
MASTER_SUBNET=$3

cat > setup_master_args.sh <<EOL
OVERLAY_IP=$1
GW_IP=$1
MASTER_NAME=$2
MASTER_SUBNET=$3
EOL

# Comment out the next line, if you prefer TCP instead of SSL.
SSL="true"

# FIXME(mestery): Remove once Vagrant boxes allow apt-get to work again
sudo rm -rf /var/lib/apt/lists/*

# Add external repos to install docker and OVS from packages.
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates
echo "deb http://18.191.116.101/openvswitch/stable /" |  sudo tee /etc/apt/sources.list.d/openvswitch.list
wget -O - http://18.191.116.101/openvswitch/keyFile |  sudo apt-key add -
sudo apt-key adv --keyserver hkp://keyserver.ubuntu.com:80 --recv-keys 58118E89F3A912897C070ADBF76221572C52609D
sudo su -c "echo \"deb https://apt.dockerproject.org/repo ubuntu-xenial main\" >> /etc/apt/sources.list.d/docker.list"
sudo apt-get update

# First, install docker
sudo apt-get purge lxc-docker
sudo apt-get install -y linux-image-extra-$(uname -r) linux-image-extra-virtual
sudo apt-get install -y docker-engine
sudo service docker start

# Install OVS and dependencies
sudo apt-get build-dep dkms
sudo apt-get install python-six openssl python-pip -y
sudo -H pip install --upgrade pip

sudo apt-get install openvswitch-datapath-dkms=2.9.2-1 -y
sudo apt-get install openvswitch-switch=2.9.2-1 openvswitch-common=2.9.2-1 libopenvswitch=2.9.2-1 -y
sudo -H pip install ovs

sudo apt-get install ovn-central=2.9.2-1 ovn-common=2.9.2-1 ovn-host=2.9.2-1 -y


if [ -n "$SSL" ]; then
    echo "PROTOCOL=ssl" >> setup_master_args.sh
    # Install SSL certificates
    pushd /etc/openvswitch
    sudo ovs-pki -d /vagrant/pki init --force
    sudo ovs-pki req ovnsb && sudo ovs-pki self-sign ovnsb

    sudo ovs-pki req ovnnb && sudo ovs-pki self-sign ovnnb

    sudo ovs-pki req ovncontroller
    sudo ovs-pki -b -d /vagrant/pki sign ovncontroller switch
    popd
else
    echo "PROTOCOL=tcp" >> setup_master_args.sh
fi

# Install golang
wget -nv https://dl.google.com/go/go1.9.2.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.9.2.linux-amd64.tar.gz
export PATH="/usr/local/go/bin:echo $PATH"
export GOPATH=$HOME/work

# Setup CNI directory
sudo mkdir -p /opt/cni/bin/

# Install OVN+K8S Integration
mkdir -p $HOME/work/src/github.com/openvswitch
pushd $HOME/work/src/github.com/openvswitch
git clone https://github.com/openvswitch/ovn-kubernetes
popd
pushd $HOME/work/src/github.com/openvswitch/ovn-kubernetes/go-controller
make 1>&2 2>/dev/null
sudo make install
popd

# Restore xtrace
$XTRACE
