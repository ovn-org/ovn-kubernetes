#!/bin/bash

# run on master to configure ovn-kubernetes
# The /etc/openvswitch/ovn_k8s.conf and /etc/sysconfig/ovn-kubernetes
# are used by ovnkube to direct processing.
# This script creates files that can be copied to the nodes

oc create serviceaccount ovn  2>/dev/null
oc adm policy add-cluster-role-to-user cluster-admin -z ovn  2>/dev/null
oc adm policy add-scc-to-user anyuid -z ovn  2>/dev/null

token=$(oc sa get-token ovn)

cidr=$(gawk '/clusterNetworkCIDR:/{ print $2 }' /etc/origin/master/master-config.yaml)

apifqn=$(hostname)
apiip=$(host ${apifqn} | gawk '{ print $4 }')

# edit the /etc/sysconfig/ovn-kubernetes file
sed '/cluster_cidr=/d' < /etc/sysconfig/ovn-kubernetes > /tmp/ovn-kubernetes
echo "cluster_cidr=${cidr}" >> /tmp/ovn-kubernetes

# edit /etc/openvswitch/ovn_k8s.conf 
sed "s/ovn_master_fqn/${apifqn}/
s/token=/token=${token}/
s/ovn_master_ip/${apiip}/" /etc/openvswitch/ovn_k8s.conf > /tmp/ovn_k8s.conf

exit 0

