#!/bin/bash
#set -x

#Always exit on errors
set -e

# This is for people that are not using the ansible install.
# The script renders j2 templates into yaml files in ../yaml/

# ensure j2 renderer installed
pip freeze | grep j2cli || pip install j2cli[yaml] --user
export PATH=~/.local/bin:$PATH

OVN_IMAGE=""
OVN_IMAGE_PULL_POLICY=""
OVN_NET_CIDR=""
OVN_SVC_DIDR=""
OVN_K8S_APISERVER=""
OVN_GATEWAY_MODE=""
OVN_GATEWAY_OPTS=""
# In the future we will have RAFT based HA support.
OVN_DB_VIP_IMAGE=""
OVN_DB_VIP=""
OVN_DB_REPLICAS=""
OVN_MTU="1400"
KIND=""
MASTER_LOGLEVEL=""
NODE_LOGLEVEL=""
OVN_LOGLEVEL_NORTHD=""
OVN_LOGLEVEL_NB=""
OVN_LOGLEVEL_SB=""
OVN_LOGLEVEL_CONTROLLER=""
OVN_LOGLEVEL_NBCTLD=""


# Parse parameters given as arguments to this script.
while [ "$1" != "" ]; do
    PARAM=`echo $1 | awk -F= '{print $1}'`
    VALUE=`echo $1 | cut -d= -f2-`
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
        --db-vip-image)
            OVN_DB_VIP_IMAGE=$VALUE
            ;;
        --db-replicas)
            OVN_DB_REPLICAS=$VALUE
            ;;
        --db-vip)
            OVN_DB_VIP=$VALUE
            ;;
        --mtu)
            OVN_MTU=$VALUE
            ;;
        --kind)
            KIND=true
            ;;
        --master-loglevel)
            MASTER_LOGLEVEL=$VALUE
            ;;
        --node-loglevel)
            NODE_LOGLEVEL=$VALUE
            ;;
        --ovn-loglevel-northd)
            OVN_LOGLEVEL_NORTHD=$VALUE
            ;;
        --ovn-loglevel-nb)
            OVN_LOGLEVEL_NB=$VALUE
            ;;
        --ovn-loglevel-sb)
            OVN_LOGLEVEL_SB=$VALUE
            ;;
        --ovn-loglevel-controller)
            OVN_LOGLEVEL_CONTROLLER=$VALUE
            ;;
        --ovn-loglevel-nbctld)
            OVN_LOGLEVEL_NBCTLD=$VALUE
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

ovn_db_vip_image=${OVN_DB_VIP_IMAGE:-"docker.io/ovnkube/ovndb-vip-u:latest"}
echo "ovn_db_vip_image: ${ovn_db_vip_image}"
ovn_db_replicas=${OVN_DB_REPLICAS:-3}
echo "ovn_db_replicas: ${ovn_db_replicas}"
ovn_db_vip=${OVN_DB_VIP}
echo "ovn_db_vip: ${ovn_db_vip}"
ovn_db_minAvailable=$(((${ovn_db_replicas} + 1) / 2))
echo "ovn_db_minAvailable: ${ovn_db_minAvailable}"
master_loglevel=${MASTER_LOGLEVEL:-"4"}
echo "master_loglevel: ${master_loglevel}"
node_loglevel=${NODE_LOGLEVEL:-"4"}
echo "node_loglevel: ${node_loglevel}"
ovn_loglevel_northd=${OVN_LOGLEVEL_NORTHD:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_northd: ${ovn_loglevel_northd}"
ovn_loglevel_nb=${OVN_LOGLEVEL_NB:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_nb: ${ovn_loglevel_nb}"
ovn_loglevel_sb=${OVN_LOGLEVEL_SB:-"-vconsole:info -vfile:info"}
echo "ovn_loglevel_sb: ${ovn_loglevel_sb}"
ovn_loglevel_controller=${OVN_LOGLEVEL_CONTROLLER:-"-vconsole:info"}
echo "ovn_loglevel_controller: ${ovn_loglevel_controller}"
ovn_loglevel_nbctld=${OVN_LOGLEVEL_NBCTLD:-"-vconsole:info"}
echo "ovn_loglevel_nbctld: ${ovn_loglevel_nbctld}"

ovn_image=${image} \
ovn_image_pull_policy=${policy} \
kind=${KIND} \
ovn_gateway_mode=${ovn_gateway_mode} \
ovn_gateway_opts=${ovn_gateway_opts} \
ovnkube_node_loglevel=${node_loglevel} \
ovn_loglevel_controller=${ovn_loglevel_controller} \
j2 ../templates/ovnkube-node.yaml.j2 -o ../yaml/ovnkube-node.yaml

ovn_image=${image} \
ovn_image_pull_policy=${policy} \
ovnkube_master_loglevel=${master_loglevel} \
ovn_loglevel_northd=${ovn_loglevel_northd} \
ovn_loglevel_nbctld=${ovn_loglevel_nbctld} \
j2 ../templates/ovnkube-master.yaml.j2 -o ../yaml/ovnkube-master.yaml

ovn_image=${image} \
ovn_image_pull_policy=${policy} \
ovn_loglevel_nb=${ovn_loglevel_nb} \
ovn_loglevel_sb=${ovn_loglevel_sb} \
j2 ../templates/ovnkube-db.yaml.j2 -o ../yaml/ovnkube-db.yaml

ovn_db_vip_image=${ovn_db_vip_image} \
ovn_image_pull_policy=${policy} \
ovn_db_replicas=${ovn_db_replicas} \
ovn_db_vip=${ovn_db_vip} ovn_loglevel_nb=${ovn_loglevel_nb} \
j2 ../templates/ovnkube-db-vip.yaml.j2 -o ../yaml/ovnkube-db-vip.yaml

ovn_image=${image} \
ovn_image_pull_policy=${policy} \
ovn_db_replicas=${ovn_db_replicas} \
ovn_db_minAvailable=${ovn_db_minAvailable} \
ovn_loglevel_nb=${ovn_loglevel_nb} ovn_loglevel_sb=${ovn_loglevel_sb} \
j2 ../templates/ovnkube-db-raft.yaml.j2 > ../yaml/ovnkube-db-raft.yaml

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
mtu_repl="{{ mtu_value }}"

echo "net_cidr: ${net_cidr}"
echo "svc_cidr: ${svc_cidr}"
echo "k8s_apiserver: ${k8s_apiserver}"
echo "mtu: ${OVN_MTU}"

sed "s,${net_cidr_repl},${net_cidr},
s,${svc_cidr_repl},${svc_cidr},
s,${mtu_repl},${OVN_MTU},
s,${k8s_apiserver_repl},${k8s_apiserver}," ../templates/ovn-setup.yaml.j2 > ../yaml/ovn-setup.yaml

cp ../templates/ovnkube-monitor.yaml.j2 ../yaml/ovnkube-monitor.yaml

exit 0
