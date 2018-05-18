#!/bin/bash
  
# Create a configmap of the ovn configuration information
# This script runs on the ovn-master node on the cluster
# This is run as part of installation after the cluster is
# up and before the ovn daemonsets are created.

health=$(kubectl get --raw /healthz/ready)
if [ $health != 'ok' ]
then
  echo " cluster not ready"
  exit 1
fi

svcacct=$(kubectl get serviceaccount | grep ovn)
if [ $? -ne 0 ]
then
  kubectl create serviceaccount ovn
  kubectl adm policy add-cluster-role-to-user cluster-admin -z ovn
  kubectl adm policy add-scc-to-user anyuid -z ovn
fi

# get api server
api=$(kubectl get po -n kube-system -o wide | gawk '/master-api/{ print $7 }')
apiserver=https://${api}:8443

# get the API token.
token=$(kubectl sa get-token ovn)

# get the network cidr (master-config.yaml clusterNetworkCIDR)
net_cidr=10.128.0.0/14

# get the service subnet cidr (master-config.yaml servicesSubnet)
svc_cidr=172.30.0.0/16

# get the ovn master's ip address
host_ip=$(host $(hostname) | gawk '{ print $4 }' )

OvnNorth="tcp://${host_ip}:6641"
OvnSouth="tcp://${host_ip}:6642"

# build the ovn.master file
echo apiserver $apiserver
echo token $token
echo net_cidr $net_cidr
echo svc_cidr $svc_cidr
echo OvnNorth $OvnNorth
echo OvnSouth $OvnSouth

# if the config map exists, delete it.
kubectl get configmap ovn-config > /dev/null
if [[ $? = 0 ]]
then
  kubectl delete configmap ovn-config
fi

# create the ovn-config configmap
cat << EOF | kubectl create -f -
kind: ConfigMap
apiVersion: v1
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
data:
  k8s_apiserver: $apiserver
  net_cidr:      $net_cidr
  svc_cidr:      $svc_cidr
  OvnNorth:      $OvnNorth
  OvnSouth:      $OvnSouth
EOF

exit 0
