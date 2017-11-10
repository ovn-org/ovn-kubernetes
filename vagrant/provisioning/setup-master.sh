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

cat > setup_master_args.sh <<EOL
OVERLAY_IP=$1
GW_IP=$2
MASTER_NAME=$3
MASTER_SUBNET=$4
EOL

# Comment out the next line, if you prefer TCP instead of SSL.
SSL="true"

# FIXME(mestery): Remove once Vagrant boxes allow apt-get to work again
sudo rm -rf /var/lib/apt/lists/*

# Add external repos to install docker and OVS from packages.
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates
echo "deb https://packages.wand.net.nz $(lsb_release -sc) main" | sudo tee /etc/apt/sources.list.d/wand.list
sudo curl https://packages.wand.net.nz/keyring.gpg -o /etc/apt/trusted.gpg.d/wand.gpg
sudo apt-key adv --keyserver hkp://p80.pool.sks-keyservers.net:80 --recv-keys 58118E89F3A912897C070ADBF76221572C52609D
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

sudo apt-get install openvswitch-datapath-dkms=2.8.1-1 -y
sudo apt-get install openvswitch-switch=2.8.1-1 openvswitch-common=2.8.1-1 -y
sudo -H pip install ovs

sudo apt-get install ovn-central=2.8.1-1 ovn-common=2.8.1-1 ovn-host=2.8.1-1 -y


if [ -n "$SSL" ]; then
    # Install SSL certificates
    pushd /etc/openvswitch
    sudo ovs-pki -d /vagrant/pki init --force
    sudo ovs-pki req ovnsb && sudo ovs-pki self-sign ovnsb
    sudo ovn-sbctl set-ssl /etc/openvswitch/ovnsb-privkey.pem \
        /etc/openvswitch/ovnsb-cert.pem  /vagrant/pki/switchca/cacert.pem

    sudo ovs-pki req ovnnb && sudo ovs-pki self-sign ovnnb
    sudo ovn-nbctl set-ssl /etc/openvswitch/ovnnb-privkey.pem \
        /etc/openvswitch/ovnnb-cert.pem  /vagrant/pki/switchca/cacert.pem

    sudo ovs-pki req ovncontroller
    sudo ovs-pki -b -d /vagrant/pki sign ovncontroller switch
    popd

    sudo ovn-nbctl set-connection pssl:6641
    sudo ovn-sbctl set-connection pssl:6642

    sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="ssl:$OVERLAY_IP:6642" \
                                  external_ids:ovn-nb="ssl:$OVERLAY_IP:6641" \
                                  external_ids:ovn-encap-ip=$OVERLAY_IP \
                                  external_ids:ovn-encap-type=geneve

    # Set ovn-controller SSL options in /etc/default/ovn-host
    sudo bash -c 'cat >> /etc/default/ovn-host <<EOF
OVN_CTL_OPTS="--ovn-controller-ssl-key=/etc/openvswitch/ovncontroller-privkey.pem  --ovn-controller-ssl-cert=/etc/openvswitch/ovncontroller-cert.pem --ovn-controller-ssl-bootstrap-ca-cert=/etc/openvswitch/ovnsb-ca.cert"
EOF'

else
    # Plain TCP.
    sudo ovn-nbctl set-connection ptcp:6641
    sudo ovn-sbctl set-connection ptcp:6642

    sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="tcp:$OVERLAY_IP:6642" \
                                  external_ids:ovn-nb="tcp:$OVERLAY_IP:6641" \
                                  external_ids:ovn-encap-ip=$OVERLAY_IP \
                                  external_ids:ovn-encap-type=geneve
fi

# Re-start OVN controller
sudo /etc/init.d/ovn-host restart

# Set k8s API server IP
sudo ovs-vsctl set Open_vSwitch . external_ids:k8s-api-server="0.0.0.0:8080"

# Install OVN+K8S Integration
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
