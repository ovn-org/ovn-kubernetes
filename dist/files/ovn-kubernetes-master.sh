#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

source /etc/sysconfig/ovn-kubernetes

function ovn-kubernetes-master() {
  echo "Enable and start ovn-kubernetes master services"
  /usr/bin/ovnkube \
	--cluster-subnet "${cluster_cidr}" \
	--init-master `hostname` \
	--net-controller
}

ovn-kubernetes-master
