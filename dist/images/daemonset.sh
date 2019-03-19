#!/bin/bash
#set -x

# This is people that are nut using the ansible install.
# The script expands the templates into yaml files in ../yaml


# Create the daemonsets with the desired image
# The image name is from ../ansible/hosts
# The daemonset.yaml files are templates in ../ansible/templates
# They are expanded into daemonsets in ../yaml

image=$(awk -F = '/^ovn_image=/{ print $2 }' ../ansible/hosts | sed 's/\"//g')
if [[ ${image} == "" ]]
then
  image="docker.io/ovnkube/ovn-daemonset:latest"
fi
echo "image: ${image}"

policy=$(awk -F = '/^ovn_image_pull_policy/{ print $2 }' ../ansible/hosts)
if [[ ${policy} == "" ]]
then
  policy="IfNotPresent"
fi
echo "imagePullPolicy: ${policy}"

# Simplified expansion of template 
image_str="{{ ovn_image | default('docker.io/ovnkube/ovn-daemonset:latest') }}"
policy_str="{{ ovn_image_pull_policy | default('IfNotPresent') }}"

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-node.yaml.j2 > ../yaml/ovnkube-node.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-master.yaml.j2 > ../yaml/ovnkube-master.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-db.yaml.j2 > ../yaml/ovnkube-db.yaml

# ovn-setup.yaml
# net_cidr=10.128.0.0/14/23
# svc_cidr=172.30.0.0/16

net_cidr=$(awk -F = '/^net_cidr=/{ print $2 }' ../ansible/hosts)
svc_cidr=$(awk -F = '/^svc_cidr=/{ print $2 }' ../ansible/hosts)

if [[ ${net_cidr} == "" ]]
then
  net_cidr="10.128.0.0/14/23"
fi
if [[ ${svc_cidr} == "" ]]
then
  svc_cidr="172.30.0.0/16"
fi

net_cidr_repl="{{ net_cidr | default('10.128.0.0/14/23') }}"
svc_cidr_repl="{{ svc_cidr | default('172.30.0.0/16') }}"

echo "net_cidr: ${net_cidr}"
echo "svc_cidr: ${svc_cidr}"

sed "s,${net_cidr_repl},${net_cidr},
s,${svc_cidr_repl},${svc_cidr}," ../templates/ovn-setup.yaml.j2 > ../yaml/ovn-setup.yaml

exit 0
