#!/bin/bash

install_redhat_linux() {
	# Add a repo for where we can get OVS 2.6 packages
	if [ ! -f /etc/yum.repos.d/delorean-deps.repo ] ; then
	    curl -k https://trunk.rdoproject.org/centos7/delorean-deps.repo | sudo tee /etc/yum.repos.d/delorean-deps.repo
	fi
	sudo yum install -y openvswitch openvswitch-ovn-central openvswitch-ovn-host
}

install_debian_linux() {
	echo "TODO: openvswitch/ovn installation will be skipped for debian distribution"
}

install_linux() {
	echo "TODO: openvswitch/ovn installation will be skipped for generic linux distribution"
}

OS=`uname -s`
if [ "${OS}" = "Linux" ] ; then
	if [ -f /etc/redhat-release ] ; then
		install_redhat_linux
	elif [ -f /etc/debian_version ] ; then
		install_debian_linux
	else
		install_linux
	fi
fi
