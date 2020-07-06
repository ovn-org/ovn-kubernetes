#!/usr/bin/env bash


# ensure j2 renderer installed
pip freeze | grep j2cli || pip install j2cli[yaml] --user
export PATH=~/.local/bin:$PATH

run_kubectl() {
  local retries=0
  local attempts=10
  while true; do
    if kubectl "$@"; then
      break
    fi

    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: 'kubectl $*' did not succeed, failing"
      exit 1
    fi
    echo "info: waiting for 'kubectl $*' to succeed..."
    sleep 1
  done
}

usage()
{
    echo "usage: kind.sh [[[-cf|--config-file <file>] [-kt|keep-taint] [-ha|--ha-enabled]"
    echo "                 [-ho|--hybrid-enabled] [-ii|--install-ingress] [-n4|--no-ipv4] [-i6|--ipv6]"
    echo "                 [-wk|--num-workers <num>]] [-gm|--gateway-mode <mode>] | [-h]]"
    echo ""
    echo "-cf | --config-file          Name of the KIND J2 configuration file."
    echo "                             DEFAULT: ./kind.yaml.j2"
    echo "-kt | --keep-taint           Do not remove taint components."
    echo "                             DEFAULT: Remove taint components."
    echo "-ha | --ha-enabled           Enable high availability. DEFAULT: HA Disabled."
    echo "-ho | --hybrid-enabled       Enable hybrid overlay. DEFAULT: Disabled."
    echo "-ii | --install-ingress      Flag to install Ingress Components."
    echo "                             DEFAULT: Don't install ingress components."
    echo "-n4 | --no-ipv4              Disable IPv4. DEFAULT: IPv4 Enabled."
    echo "-i6 | --ipv6                 Enable IPv6. DEFAULT: IPv6 Disabled."
    echo "-wk | --num-workers          Number of worker nodes. DEFAULT: HA - 2 worker"
    echo "                             nodes and no HA - 0 worker nodes."
    echo "-gm | --gateway-mode         Enable 'shared' or 'local' gateway mode. DEFAULT: local."
    echo "-ov | --ovn-image            Use the specified docker image instead of building locally. DEFAULT: local build."
    echo ""
}

parse_args()
{
    while [ "$1" != "" ]; do
        case $1 in
            -cf | --config-file )      shift
                                       if test ! -f "$1"; then
                                          echo "$1 does not  exist"
                                          usage
                                          exit 1
                                       fi
                                       KIND_CONFIG=$1
                                       ;;
            -ii | --install-ingress )  KIND_INSTALL_INGRESS=true
                                       ;;
            -ha | --ha-enabled )       KIND_HA=true
                                       ;;
            -ho | --hybrid-enabled )   OVN_HYBRID_OVERLAY_ENABLE=true
                                       ;;
            -kt | --keep-taint )       KIND_REMOVE_TAINT=false
                                       ;;
            -n4 | --no-ipv4 )          KIND_IPV4_SUPPORT=false
                                       ;;
            -i6 | --ipv6 )             KIND_IPV6_SUPPORT=true
                                       ;;
            -wk | --num-workers )      shift
                                       if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                          echo "Invalid num-workers: $1"
                                          usage
                                          exit 1
                                       fi
                                       KIND_NUM_WORKER=$1
                                       ;;
            -gm | --gateway-mode )     shift
                                       if [ "$1" != "local" ] && [ "$1" != "shared" ]; then
                                          echo "Invalid gateway mode: $1"
                                          usage
                                          exit 1
                                       fi
                                       OVN_GATEWAY_MODE=$1
                                       ;;
            -ov | --ovn-image )        shift
                                       OVN_IMAGE=$1
                                       ;;
            -h | --help )              usage
                                       exit
                                       ;;
            * )                        usage
                                       exit 1
        esac
        shift
    done
}

print_params()
{
     echo "Using these parameters to install KIND"
     echo ""
     echo "KIND_INSTALL_INGRESS = $KIND_INSTALL_INGRESS"
     echo "KIND_HA = $KIND_HA"
     echo "KIND_CONFIG_FILE = $KIND_CONFIG"
     echo "KIND_REMOVE_TAINT = $KIND_REMOVE_TAINT"
     echo "KIND_IPV4_SUPPORT = $KIND_IPV4_SUPPORT"
     echo "KIND_IPV6_SUPPORT = $KIND_IPV6_SUPPORT"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo ""
}

parse_args $*

# Set default values
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
K8S_VERSION=${K8S_VERSION:-v1.18.2}
OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-local}
KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
KIND_HA=${KIND_HA:-false}
KIND_CONFIG=${KIND_CONFIG:-./kind.yaml.j2}
KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
KIND_IPV4_SUPPORT=${KIND_IPV4_SUPPORT:-true}
KIND_IPV6_SUPPORT=${KIND_IPV6_SUPPORT:-false}
OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
OVN_IMAGE=${OVN_IMAGE:-local}

# Input not currently validated. Modify outside script at your own risk.
# These are the same values defaulted to in KIND code (kind/default.go).
# NOTE: Upstream KIND IPv6 masks are different (currently rejected by ParseClusterSubnetEntries()):
#  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/12}
NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/64}

KIND_NUM_MASTER=1
if [ "$KIND_HA" == true ]; then
  KIND_NUM_MASTER=3
  KIND_NUM_WORKER=${KIND_NUM_WORKER:-0}
else
  KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
fi

print_params

set -euxo pipefail

# Detect IP to use as API server
API_IPV4=""
if [ "$KIND_IPV4_SUPPORT" == true ]; then
  # ip -4 addr -> Run ip command for IPv4
  # grep -oP '(?<=inet\s)\d+(\.\d+){3}' -> Use only the lines with the
  #   IPv4 Addresses and strip off the trailing subnet mask, /xx
  # grep -v "127.0.0.1" -> Remove local host
  # head -n 1 -> Of the remaining, use first entry
  API_IPV4=$(ip -4 addr | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | grep -v "127.0.0.1" | head -n 1)
  if [ -z "$API_IPV4" ]; then
    echo "Error detecting machine IPv4 to use as API server"
    exit 1
  fi
fi

API_IPV6=""
if [ "$KIND_IPV6_SUPPORT" == true ]; then
  # ip -6 addr -> Run ip command for IPv6
  # grep "inet6" -> Use only the lines with the IPv6 Address
  # sed 's@/.*@@g' -> Strip off the trailing subnet mask, /xx
  # grep -v "^::1$" -> Remove local host
  # sed '/^fe80:/ d' -> Remove Link-Local Addresses
  # head -n 1 -> Of the remaining, use first entry
  API_IPV6=$(ip -6 addr  | grep "inet6" | awk -F' ' '{print $2}' | \
             sed 's@/.*@@g' | grep -v "^::1$" | sed '/^fe80:/ d' | head -n 1)
  if [ -z "$API_IPV6" ]; then
    echo "Error detecting machine IPv6 to use as API server"
    exit 1
  fi
fi

if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == false ]; then
  API_IP=${API_IPV4}
  IP_FAMILY=""
  NET_CIDR=$NET_CIDR_IPV4
  SVC_CIDR=$SVC_CIDR_IPV4
  echo "IPv4 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
elif [ "$KIND_IPV4_SUPPORT" == false ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  API_IP=${API_IPV6}
  IP_FAMILY="ipv6"
  NET_CIDR=$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV6
  echo "IPv6 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
elif [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  #TODO DUALSTACK: Multiple IP Addresses for APIServer not currently supported.
  #API_IP=${API_IPV4},${API_IPV6}
  API_IP=${API_IPV4}
  IP_FAMILY="DualStack"
  NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
  echo "Dual Stack Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
else
  echo "Invalid setup. KIND_IPV4_SUPPORT and/or KIND_IPV6_SUPPORT must be true."
  exit 1
fi

# Output of the j2 command
KIND_CONFIG_LCL=./kind.yaml

ovn_apiServerAddress=${API_IP} \
  ovn_ip_family=${IP_FAMILY} \
  ovn_ha=${KIND_HA} \
  ovn_num_master=${KIND_NUM_MASTER} \
  ovn_num_worker=${KIND_NUM_WORKER} \
  j2 ${KIND_CONFIG} -o ${KIND_CONFIG_LCL}

# Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
kind create cluster --name ${KIND_CLUSTER_NAME} --kubeconfig ${HOME}/admin.conf --image kindest/node:${K8S_VERSION} --config=${KIND_CONFIG_LCL}
export KUBECONFIG=${HOME}/admin.conf
cat ${KUBECONFIG}
mkdir -p /tmp/kind
sudo chmod 777 /tmp/kind
count=0
until kubectl get secrets -o jsonpath='{.items[].data.ca\.crt}'
do
  if [ $count -gt 10 ]; then
    echo "Failed to get k8s crt/token"
    exit 1
  fi
  count=$((count+1))
  echo "secrets not available on attempt $count"
  sleep 5
done
kubectl get secrets -o jsonpath='{.items[].data.ca\.crt}' > /tmp/kind/ca.crt
kubectl get secrets -o jsonpath='{.items[].data.token}' > /tmp/kind/token

if [ "$OVN_IMAGE" == local ]; then
  # Build ovn docker image
  pushd ../go-controller
  make
  popd

  # Build ovn kube image
  pushd ../dist/images
  sudo cp -f ../../go-controller/_output/go/bin/* .
  echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
  docker build -t ovn-daemonset-f:dev -f Dockerfile.fedora .
  OVN_IMAGE=ovn-daemonset-f:dev
  popd
else
  docker pull ${OVN_IMAGE}
fi

# Create ovn kube manifests
pushd ../dist/images
./daemonset.sh \
  --image=${OVN_IMAGE} \
  --net-cidr=${NET_CIDR} \
  --svc-cidr=${SVC_CIDR} \
  --gateway-mode=${OVN_GATEWAY_MODE} \
  --hybrid-enabled=${OVN_HYBRID_OVERLAY_ENABLE} \
  --k8s-apiserver=https://[${API_IP}]:11337 \
  --ovn-master-count=${KIND_NUM_MASTER} \
  --kind \
  --master-loglevel=5
popd

kind load docker-image ${OVN_IMAGE} --name ${KIND_CLUSTER_NAME}

pushd ../dist/yaml
run_kubectl apply -f ovn-setup.yaml
CONTROL_NODES=$(docker ps -f name=ovn-control | grep -v NAMES | awk '{ print $NF }')
for n in $CONTROL_NODES; do
  run_kubectl label node $n k8s.ovn.org/ovnkube-db=true
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    run_kubectl taint node $n node-role.kubernetes.io/master:NoSchedule-
  fi
done
if [ "$KIND_HA" == true ]; then
  run_kubectl apply -f ovnkube-db-raft.yaml
else
  run_kubectl apply -f ovnkube-db.yaml
fi
run_kubectl apply -f ovnkube-master.yaml
run_kubectl apply -f ovnkube-node.yaml
popd
run_kubectl -n kube-system delete ds kube-proxy
kind get clusters
kind get nodes --name ${KIND_CLUSTER_NAME}
kind export kubeconfig --name ovn
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  run_kubectl apply -f ingress/mandatory.yaml
  run_kubectl apply -f ingress/service-nodeport.yaml
fi

count=1
until [ -z "$(kubectl get pod -A -o custom-columns=NAME:metadata.name,STATUS:.status.phase | tail -n +2 | grep -v Running)" ];do
  if [ $count -gt 20 ]; then
    echo "Some pods are not running after timeout"
    exit 1
  fi
  echo "All pods not available yet on attempt $count:"
  kubectl get pod -A || true
  count=$((count+1))
  sleep 10
done
echo "Pods are all up, allowing things settle for 30 seconds..."
sleep 30

