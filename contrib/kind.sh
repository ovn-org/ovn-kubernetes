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
     echo "KIND_UPDATE_SYSTEM = $KIND_UPDATE_SYSTEM"
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
KIND_UPDATE_SYSTEM=${KIND_UPDATE_SYSTEM:-false}

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

check_ipv6() {
  # Collect additional IPv6 data on test environment
  # TEST-BEGIN
  TMPVAR=`sysctl net.ipv6.conf.all.forwarding | awk '{print $3}'`
  echo "net.ipv6.conf.all.forwarding is equal to $TMPVAR"
  if [ "$TMPVAR" != 1 ]; then
    if [ "$KIND_UPDATE_SYSTEM" == true ]; then
      sudo sysctl -w net.ipv6.conf.all.forwarding=1
    else
      echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.forwarding=1' to use IPv6."
      ERROR_FOUND=true
    fi
  fi
  TMPVAR=`sysctl net.ipv6.conf.all.disable_ipv6 | awk '{print $3}'`
  echo "net.ipv6.conf.all.disable_ipv6 is equal to $TMPVAR"
  if [ "$TMPVAR" != 0 ]; then
    if [ "$KIND_UPDATE_SYSTEM" == true ]; then
      sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0
    else
      echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0' to use IPv6."
      ERROR_FOUND=true
    fi
  fi

  # REMOVE: Verify docker has ipv6 enabled
  docker run --rm busybox ip a

  echo ""
  echo "Running: ip a"
  ip a

  echo ""
  echo "Running: Check for /proc/net/if_inet6"
  [ -f /proc/net/if_inet6 ] && echo '  IPv6 ready system!' || echo '  No IPv6 support found! Compile the kernel!!'

  echo ""
  echo "Running: lsmod"
  lsmod | grep -qw ipv6 && echo "IPv6 kernel driver loaded and configured." || echo "IPv6 not configured and/or driver loaded on the system."
}

if [ "$KIND_IPV6_SUPPORT" == true ]; then 
  check_ipv6
fi

if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == false ]; then
  IP_FAMILY=""
  NET_CIDR=$NET_CIDR_IPV4
  SVC_CIDR=$SVC_CIDR_IPV4
  echo "IPv4 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
elif [ "$KIND_IPV4_SUPPORT" == false ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  IP_FAMILY="ipv6"
  NET_CIDR=$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV6
  echo "IPv6 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
elif [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  #TODO DUALSTACK: Multiple IP Addresses for APIServer not currently supported.
  IP_FAMILY="DualStack"
  NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
  SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
  echo "Dual Stack Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
else
  echo "Invalid setup. KIND_IPV4_SUPPORT and/or KIND_IPV6_SUPPORT must be true."
  exit 1
fi

# Output of the j2 command
KIND_CONFIG_LCL=./kind.yaml

ovn_ip_family=${IP_FAMILY} \
  ovn_ha=${KIND_HA} \
  ovn_num_master=${KIND_NUM_MASTER} \
  ovn_num_worker=${KIND_NUM_WORKER} \
  j2 ${KIND_CONFIG} -o ${KIND_CONFIG_LCL}

# Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
kind create cluster --name ${KIND_CLUSTER_NAME} --verbosity 5 --kubeconfig ${HOME}/admin.conf --image kindest/node:${K8S_VERSION} --config=${KIND_CONFIG_LCL}
export KUBECONFIG=${HOME}/admin.conf
cat ${KUBECONFIG}

if [[ "$IP_FAMILY" == "ipv6" ]]; then
  echo "BILLY: IPv6 Only - UPDATE CoreDNS"
  # IPv6 clusters need some CoreDNS changes in order to work in k8s CI:
  # 1. k8s CI doesn´t offer IPv6 connectivity, so CoreDNS should be configured
  # to work in an offline environment:
  # https://github.com/coredns/coredns/issues/2494#issuecomment-457215452
  # 2. k8s CI adds following domains to resolv.conf search field:
  # c.k8s-prow-builds.internal google.internal.
  # CoreDNS should handle those domains and answer with NXDOMAIN instead of SERVFAIL
  # otherwise pods stops trying to resolve the domain.
  # Get the current config
  original_coredns=$(kubectl get -oyaml -n=kube-system configmap/coredns)
  echo "Original CoreDNS config:"
  echo "${original_coredns}"
  # Patch it
  fixed_coredns=$(
    printf '%s' "${original_coredns}" | sed \
      -e 's/^.*kubernetes cluster\.local/& internal/' \
      -e '/^.*upstream$/d' \
      -e '/^.*fallthrough.*$/d' \
      -e '/^.*forward . \/etc\/resolv.conf$/d' \
      -e '/^.*loop$/d' \
  )
  echo "Patched CoreDNS config:"
  echo "${fixed_coredns}"
  printf '%s' "${fixed_coredns}" | kubectl apply -f -
fi

# Build the ovn-kube controller
pushd ../go-controller
make
popd

# Create the ovn-kube image
pushd ../dist/images
sudo cp -f ../../go-controller/_output/go/bin/* .
echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
docker build -t ovn-daemonset-f:dev -f Dockerfile.fedora .

# Detect API IP address for OVN

# Despite OVN run in pod they will only obtain the VIRTUAL apiserver address
# and since OVN has to provide the connectivity to service
# it can not be bootstrapped

# This is the address of the node with the control-plane
# If HA, multiple control-plane nodes, KIND deploys
API_URL=$(kind get kubeconfig --internal --name ${KIND_CLUSTER_NAME} | grep server | awk '{ print $2 }')
# Create ovn-kube manifests
./daemonset.sh --image=docker.io/library/ovn-daemonset-f:dev \
  --net-cidr=${NET_CIDR} \
  --svc-cidr=${SVC_CIDR} \
  --gateway-mode=${OVN_GATEWAY_MODE} \
  --k8s-apiserver=${API_URL} \
  --ovn-master-count=${KIND_NUM_MASTER} \
  --kind \
  --master-loglevel=5
popd

# Preload ovn-kube images in the kind cluster
kind load docker-image ovn-daemonset-f:dev --name ${KIND_CLUSTER_NAME}

# Deploy ovn-kube
pushd ../dist/yaml
run_kubectl create -f ovn-setup.yaml
CONTROL_NODES=$(docker ps -f name=ovn-control | grep -v NAMES | awk '{ print $NF }')
for n in $CONTROL_NODES; do
  run_kubectl label node $n k8s.ovn.org/ovnkube-db=true
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    run_kubectl taint node $n node-role.kubernetes.io/master:NoSchedule-
  fi
done
if [ "$KIND_HA" == true ]; then
  run_kubectl create -f ovnkube-db-raft.yaml
else
  run_kubectl create -f ovnkube-db.yaml
fi
run_kubectl create -f ovnkube-master.yaml
run_kubectl create -f ovnkube-node.yaml
popd

# We can delete kube-proxy
run_kubectl -n kube-system delete ds kube-proxy
kind get clusters
kind get nodes --name ${KIND_CLUSTER_NAME}
kind export kubeconfig --name ${KIND_CLUSTER_NAME}
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  run_kubectl apply -f ingress/mandatory.yaml
  run_kubectl apply -f ingress/service-nodeport.yaml
fi

# Check that everything is fine and running

if ! kubectl wait -n ovn-kubernetes --for=condition=ready pods --all --timeout=300s ; then
  echo "some pods in OVN Kubernetes are not running"
  exit 1
fi

if ! kubectl wait -n kube-system --for=condition=ready pods --all --timeout=300s ; then
  echo "some pods in the system are not running"
  exit 1
fi

echo "Pods are all up, allowing things settle for 30 seconds..."
sleep 30

