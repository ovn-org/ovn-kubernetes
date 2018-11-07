#!/bin/bash
#set -x

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
s,${policy_str},${policy}," ../templates/ovnkube.yaml.j2 > ../yaml/ovnkube.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-master.yaml.j2 > ../yaml/ovnkube-master.yaml

exit 0
