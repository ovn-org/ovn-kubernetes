#!/bin/bash
#set -x

#Always exit on errors
set -e

# This is people that are nut using the ansible install.
# The script expands the templates into yaml files in ../yaml

OVN_IMAGE=""
OVN_IMAGE_PULL_POLICY=""
OVN_NET_CIDR=""
OVN_SVC_DIDR=""
OVN_K8S_APISERVER=""
OVN_GATEWAY_MODE=""
OVN_GATEWAY_OPTS=""

# Parse parameters given as arguments to this script.
while [ "$1" != "" ]; do
    PARAM=`echo $1 | awk -F= '{print $1}'`
    VALUE=`echo $1 | awk -F= '{print $2}'`
    case $PARAM in
        --image)
            OVN_IMAGE=$VALUE
            ;;
        --image-pull-policy)
            OVN_IMAGE_PULL_POLICY=$VALUE
            ;;
        --gateway-mode)
            OVN_GATEWAY_MODE=$VALUE
            ;;
        --gateway-options)
            OVN_GATEWAY_OPTS=$VALUE
            ;;
        --net-cidr)
            OVN_NET_CIDR=$VALUE
            ;;
        --svc-cidr)
            OVN_SVC_CIDR=$VALUE
            ;;
        --k8s-apiserver)
            OVN_K8S_APISERVER=$VALUE
            ;;
        *)
            echo "WARNING: unknown parameter \"$PARAM\""
            exit 1
            ;;
    esac
    shift
done

# The options provided on the CLI overrides the values in ../ansible/hosts

# Create the daemonsets with the desired image
# The image name is from ../ansible/hosts
# The daemonset.yaml files are templates in ../ansible/templates
# They are expanded into daemonsets in ../yaml

if [[ ${OVN_IMAGE} == "" ]]
then
    image=$(awk -F = '/^ovn_image=/{ print $2 }' ../ansible/hosts | sed 's/\"//g')
    if [[ ${image} == "" ]]
    then
      image="docker.io/ovnkube/ovn-daemonset:latest"
    fi
else
    image=$OVN_IMAGE
fi
echo "image: ${image}"

if [[ ${OVN_IMAGE_PULL_POLICY} == "" ]]
then
    policy=$(awk -F = '/^ovn_image_pull_policy/{ print $2 }' ../ansible/hosts)
    if [[ ${policy} == "" ]]
    then
      policy="IfNotPresent"
    fi
else
    policy=$OVN_IMAGE_PULL_POLICY
fi
echo "imagePullPolicy: ${policy}"

ovn_gateway_mode=${OVN_GATEWAY_MODE}
echo "ovn_gateway_mode: ${ovn_gateway_mode}"

ovn_gateway_opts=${OVN_GATEWAY_OPTS}
echo "ovn_gateway_opts: ${ovn_gateway_opts}"

# Simplified expansion of template 
image_str="{{ ovn_image | default('docker.io/ovnkube/ovn-daemonset:latest') }}"
policy_str="{{ ovn_image_pull_policy | default('IfNotPresent') }}"
ovn_gateway_opts_repl="{{ ovn_gateway_opts }}"
ovn_gateway_mode_repl="{{ ovn_gateway_mode }}"

sed "s,${image_str},${image},
s,${ovn_gateway_mode_repl},${ovn_gateway_mode},
s,${ovn_gateway_opts_repl},${ovn_gateway_opts},
s,${policy_str},${policy}," ../templates/ovnkube-node.yaml.j2 > ../yaml/ovnkube-node.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-master.yaml.j2 > ../yaml/ovnkube-master.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-db.yaml.j2 > ../yaml/ovnkube-db.yaml

# ovn-setup.yaml
# net_cidr=10.128.0.0/14/23
# svc_cidr=172.30.0.0/16

if [[ ${OVN_NET_CIDR} == "" ]]
then
    net_cidr=$(awk -F = '/^net_cidr=/{ print $2 }' ../ansible/hosts)
    if [[ ${net_cidr} == "" ]]
    then
      net_cidr="10.128.0.0/14/23"
    fi
else
    net_cidr=$OVN_NET_CIDR
fi

if [[ ${OVN_SVC_CIDR} == "" ]]
then
    svc_cidr=$(awk -F = '/^svc_cidr=/{ print $2 }' ../ansible/hosts)
    if [[ ${svc_cidr} == "" ]]
    then
      svc_cidr="172.30.0.0/16"
    fi
else
    svc_cidr=$OVN_SVC_CIDR
fi

k8s_apiserver=${OVN_K8S_APISERVER:-10.0.2.16:6443}

net_cidr_repl="{{ net_cidr | default('10.128.0.0/14/23') }}"
svc_cidr_repl="{{ svc_cidr | default('172.30.0.0/16') }}"
k8s_apiserver_repl="{{ k8s_apiserver.stdout }}"

echo "net_cidr: ${net_cidr}"
echo "svc_cidr: ${svc_cidr}"
echo "k8s_apiserver: ${k8s_apiserver}"

sed "s,${net_cidr_repl},${net_cidr},
s,${svc_cidr_repl},${svc_cidr},
s,${k8s_apiserver_repl},${k8s_apiserver}," ../templates/ovn-setup.yaml.j2 > ../yaml/ovn-setup.yaml

exit 0
