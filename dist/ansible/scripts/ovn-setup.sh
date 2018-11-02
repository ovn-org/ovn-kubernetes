#!/bin/bash
# set -x

# Setup for Openshift to support the ovn-cni plugin
#
# This script runs on the ovn-master node on the cluster
# since it sets up a configmap with information on how
# to access the ovn-master.
#
# This script is run as part of installation after the cluster is
# up and before the ovn daemonsets are created.

trap 'kill $(jobs -p); exit 0' TERM

cfg=${1:-"/etc/origin/master/master-config.yaml"}

is_cluster_up () {
  health=$(kubectl get --raw /healthz/ready)
  if [[ $health != 'ok' ]]; then
    return 1
  fi
  return 0
}

is_api_server_ready () {
  # kubectl cluster-info leaves ^[[0 in output string
  #apiserver=$(kubectl cluster-info | gawk '/Kubernetes master/{ print $6 }')
  apiserver=$(oc whoami --show-server)
  if [[ ${apiserver} == '' ]]; then
    return 1
  fi
  return 0
}

wait_for_condition () {
  msg="$1"
  retries=0
  while true; do
    $1
    if [[ $? != 0 ]]; then
      (( retries += 1 ))
      if [[ "${retries}" -gt 4 ]]; then
        echo "error: ${msg} - NO, exiting"
        exit 1
      fi
      echo "info: Waiting 2 sec for ${msg} - YES ..."
      sleep 2
    else
      break
    fi
  done
}

#  ---------------
# wait for cluster up
wait_for_condition is_cluster_up

# wait for  api server
wait_for_condition is_api_server_ready

# from master-config.yaml
#clusterNetworks:
#  - cidr: 10.128.0.0/14
#    hostSubnetLength: 9
#    externalIPNetworkCIDRs:
#  - 0.0.0.0/0
#    networkPluginName: cni
#  serviceNetworkCIDR: 172.30.0.0/16

# ocp 4.0 permits multiple CIDRs that include the subnet length
net_cidr=10.128.0.0/14/23
svc_cidr=172.30.0.0/16
if [[ -s ${cfg} ]]; then
  # get the network cidr (master-config.yaml clusterNetworkCIDR)
  cidr=$(gawk '/cidr:/{ print $3}'  ${cfg} )
  sub=$(gawk '/hostSubnetLength/{ print $2}'  ${cfg} )
  (( net = 32 - ${sub} ))
  net_cidr=${cidr}"/"${net}

  # get the service subnet cidr (master-config.yaml servicesSubnet)
  svc_cidr=$(gawk '/serviceNetworkCIDR/{ print $2}'  ${cfg} )
fi

# get the ovn master's ip address
host_ip=$(host -t A $(hostname) | gawk '{ print $4 }' )

# Currently using tcp, may move to ssl
OvnNorth="tcp://${host_ip}:6641"
OvnSouth="tcp://${host_ip}:6642"

# build the ovn.master file
echo apiserver ${apiserver}
echo net_cidr ${net_cidr}
echo svc_cidr ${svc_cidr}
echo OvnNorth ${OvnNorth}
echo OvnSouth ${OvnSouth}

# if the config map exists, delete it.
kubectl get configmap ovn-config > /dev/null 2>&1
if [[ $? = 0 ]]
then
  kubectl delete configmap ovn-config
fi

# create the ovn-config configmap
cat << EOF | kubectl create -f - > /dev/null 2>&1
kind: ConfigMap
apiVersion: v1
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
data:
  k8s_apiserver: ${apiserver}
  net_cidr:      ${net_cidr}
  svc_cidr:      ${svc_cidr}
  OvnNorth:      ${OvnNorth}
  OvnSouth:      ${OvnSouth}
EOF
echo "kind: ConfigMap ovn-config -- $?"

# debug="-o yaml"
echo ""
echo "project ovn-kubernetes"
kubectl get project ovn-kubernetes ${debug}
echo ""
echo "ServiceAccount ovn -n ovn-kubernetes"
kubectl get sa ovn -n ovn-kubernetes ${debug}
echo ""
echo "kind: ClusterRole system:ovn-reader -- $?"
kubectl get ClusterRole system:ovn-reader ${debug}
echo ""
echo "ClusterRoleBinding ovn-cluster-reader"
kubectl get ClusterRoleBinding ovn-cluster-reader ${debug}
echo ""
echo "ClusterRoleBinding ovn-reader"
kubectl get ClusterRoleBinding ovn-reader ${debug}
echo ""
echo "ClusterRoleBinding cluster-admin-0"
kubectl get ClusterRoleBinding cluster-admin-0 ${debug}
echo ""
echo "SecurityContextConstraints anyuid"
kubectl get SecurityContextConstraints anyuid ${debug}
echo ""
echo "ConfigMap ovn-config -n ovn-kubernetes"
kubectl get ConfigMap ovn-config -n ovn-kubernetes ${debug}

exit 0
