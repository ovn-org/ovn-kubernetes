#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

source /etc/sysconfig/ovn-kubernetes

function ovn-kubernetes-node() {

  echo "Enable and start ovn-kubernetes node services"
  /usr/bin/ovnkube \
	--cluster-subnets "${cluster_cidr}" \
	--init-node `hostname`
}

ovn-kubernetes-node
