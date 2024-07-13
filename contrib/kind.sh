#!/usr/bin/env bash

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# Source the kind-common file from the same directory where this script is located
source "${DIR}/kind-common"

function setup_kubectl_bin() {
    ###########################################################################
    # Description:                                                            #
    # setup kubectl for querying the cluster                                  #
    #                                                                         #
    # Arguments:                                                              #
    #   $1 - error message if not provided, it will just exit                 #
    ###########################################################################
    if [ ! -d "./bin" ]
    then
        mkdir -p ./bin
        if_error_exit "Failed to create bin dir!"
    fi

    if [[ "$OSTYPE" == "linux-gnu" ]]; then
        OS_TYPE="linux"
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        OS_TYPE="darwin"
    fi

    pushd ./bin
       if [ ! -f ./kubectl ]; then
           curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/${OS_TYPE}/${ARCH}/kubectl"
           if_error_exit "Failed to download kubectl failed!"
       fi
    popd

    chmod +x ./bin/kubectl
    export PATH=${PATH}:$(pwd)/bin
}

# Some environments (Fedora32,31 on desktop), have problems when the cluster
# is deleted directly with kind `kind delete cluster --name ovn`, it restarts the host.
# The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
# with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.
delete() {
  if [ "$KIND_INSTALL_METALLB" == true ]; then
    destroy_metallb
  fi
  timeout 5 kubectl --kubeconfig "${KUBECONFIG}" delete namespace ovn-kubernetes || true
  sleep 5
  kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"
}

usage() {
    echo "usage: kind.sh [[[-cf |--config-file <file>] [-kt|--keep-taint] [-ha|--ha-enabled]"
    echo "                 [-ho |--hybrid-enabled] [-ii|--install-ingress] [-n4|--no-ipv4]"
    echo "                 [-i6 |--ipv6] [-wk|--num-workers <num>] [-ds|--disable-snat-multiple-gws]"
    echo "                 [-dp |--disable-pkt-mtu-check]"
    echo "                 [-df |--disable-forwarding]"
    echo "                 [-ecp | --encap-port |"
    echo "                 [-pl]|--install-cni-plugins ]"
    echo "                 [-nf |--netflow-targets <targets>] [sf|--sflow-targets <targets>]"
    echo "                 [-if |--ipfix-targets <targets>]  [-ifs|--ipfix-sampling <num>]"
    echo "                 [-ifm|--ipfix-cache-max-flows <num>] [-ifa|--ipfix-cache-active-timeout <num>]"
    echo "                 [-sw |--allow-system-writes] [-gm|--gateway-mode <mode>]"
    echo "                 [-nl |--node-loglevel <num>] [-ml|--master-loglevel <num>]"
    echo "                 [-dbl|--dbchecker-loglevel <num>] [-ndl|--ovn-loglevel-northd <loglevel>]"
    echo "                 [-nbl|--ovn-loglevel-nb <loglevel>] [-sbl|--ovn-loglevel-sb <loglevel>]"
    echo "                 [-cl |--ovn-loglevel-controller <loglevel>] [-me|--multicast-enabled]"
    echo "                 [-lcl|--libovsdb-client-logfile <logfile>]"
    echo "                 [-ep |--experimental-provider <name>] |"
    echo "                 [-eb |--egress-gw-separate-bridge] |"
    echo "                 [-lr |--local-kind-registry |"
    echo "                 [-dd |--dns-domain |"
    echo "                 [-ric | --run-in-container |"
    echo "                 [-cn | --cluster-name |"
    echo "                 [-ehp|--egress-ip-healthcheck-port <num>]"
    echo "                 [-is | --ipsec]"
    echo "                 [-cm | --compact-mode]"
    echo "                 [-ic | --enable-interconnect]"
    echo "                 [-nqe| --network-qos-enable]"
    echo "                 [--isolated]"
    echo "                 [-dns | --enable-dnsnameresolver]"
    echo "                 [-obs | --observability]"
    echo "                 [-h]]"
    echo ""
    echo "-cf  | --config-file                Name of the KIND J2 configuration file."
    echo "                                    DEFAULT: ./kind.yaml.j2"
    echo "-kt  | --keep-taint                 Do not remove taint components."
    echo "                                    DEFAULT: Remove taint components."
    echo "-ha  | --ha-enabled                 Enable high availability. DEFAULT: HA Disabled."
    echo "-scm | --separate-cluster-manager   Separate cluster manager from ovnkube-master and run as a separate container within ovnkube-master deployment."
    echo "-me  | --multicast-enabled          Enable multicast. DEFAULT: Disabled."
    echo "-ho  | --hybrid-enabled             Enable hybrid overlay. DEFAULT: Disabled."
    echo "-ds  | --disable-snat-multiple-gws  Disable SNAT for multiple gws. DEFAULT: Disabled."
    echo "-dp  | --disable-pkt-mtu-check      Disable checking packet size greater than MTU. Default: Disabled"
    echo "-df  | --disable-forwarding         Disable forwarding on OVNK managed interfaces. Default: Disabled"
    echo "-ecp | --encap-port                 UDP port used for geneve overlay. DEFAULT: 6081"
    echo "-pl  | --install-cni-plugins ]      Installs additional CNI network plugins. DEFAULT: Disabled"
    echo "-nf  | --netflow-targets            Comma delimited list of ip:port or :port (using node IP) netflow collectors. DEFAULT: Disabled."
    echo "-sf  | --sflow-targets              Comma delimited list of ip:port or :port (using node IP) sflow collectors. DEFAULT: Disabled."
    echo "-if  | --ipfix-targets              Comma delimited list of ip:port or :port (using node IP) ipfix collectors. DEFAULT: Disabled."
    echo "-ifs | --ipfix-sampling             Fraction of packets that are sampled and sent to each target collector: 1 packet out of every <num>. DEFAULT: 400 (1 out of 400 packets)."
    echo "-ifm | --ipfix-cache-max-flows      Maximum number of IPFIX flow records that can be cached at a time. If 0, caching is disabled. DEFAULT: Disabled."
    echo "-ifa | --ipfix-cache-active-timeout Maximum period in seconds for which an IPFIX flow record is cached and aggregated before being sent. If 0, caching is disabled. DEFAULT: 60."
    echo "-el  | --ovn-empty-lb-events        Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled"
    echo "-ii  | --install-ingress            Flag to install Ingress Components."
    echo "                                    DEFAULT: Don't install ingress components."
    echo "-mlb | --install-metallb            Install metallb to test service type LoadBalancer deployments"
    echo "-n4  | --no-ipv4                    Disable IPv4. DEFAULT: IPv4 Enabled."
    echo "-i6  | --ipv6                       Enable IPv6. DEFAULT: IPv6 Disabled."
    echo "-wk  | --num-workers                Number of worker nodes. DEFAULT: HA - 2 worker"
    echo "                                    nodes and no HA - 0 worker nodes."
    echo "-sw  | --allow-system-writes        Allow script to update system. Intended to allow"
    echo "                                    github CI to be updated with IPv6 settings."
    echo "                                    DEFAULT: Don't allow."
    echo "-gm  | --gateway-mode               Enable 'shared' or 'local' gateway mode."
    echo "                                    DEFAULT: shared."
    echo "-ov  | --ovn-image            	    Use the specified docker image instead of building locally. DEFAULT: local build."
    echo "-ml  | --master-loglevel            Log level for ovnkube (master), DEFAULT: 5."
    echo "-nl  | --node-loglevel              Log level for ovnkube (node), DEFAULT: 5"
    echo "-dbl | --dbchecker-loglevel         Log level for ovn-dbchecker (ovnkube-db), DEFAULT: 5."
    echo "-ndl | --ovn-loglevel-northd        Log config for ovn northd, DEFAULT: '-vconsole:info -vfile:info'."
    echo "-nbl | --ovn-loglevel-nb            Log config for northbound DB DEFAULT: '-vconsole:info -vfile:info'."
    echo "-sbl | --ovn-loglevel-sb            Log config for southboudn DB DEFAULT: '-vconsole:info -vfile:info'."
    echo "-cl  | --ovn-loglevel-controller    Log config for ovn-controller DEFAULT: '-vconsole:info'."
    echo "-lcl | --libovsdb-client-logfile    Separate logs for libovsdb client into provided file. DEFAULT: do not separate."
    echo "-ep  | --experimental-provider      Use an experimental OCI provider such as podman, instead of docker. DEFAULT: Disabled."
    echo "-eb  | --egress-gw-separate-bridge  The external gateway traffic uses a separate bridge."
    echo "-lr  | --local-kind-registry        Configure kind to use a local docker registry rather than manually loading images"
    echo "-dd  | --dns-domain                 Configure a custom dnsDomain for k8s services, Defaults to 'cluster.local'"
    echo "-cn  | --cluster-name               Configure the kind cluster's name"
    echo "-ric | --run-in-container           Configure the script to be run from a docker container, allowing it to still communicate with the kind controlplane"
    echo "-ehp | --egress-ip-healthcheck-port TCP port used for gRPC session by egress IP node check. DEFAULT: 9107 (Use "0" for legacy dial to port 9)."
    echo "-is  | --ipsec                      Enable IPsec encryption (spawns ovn-ipsec pods)"
    echo "-sm  | --scale-metrics              Enable scale metrics"
    echo "-cm  | --compact-mode               Enable compact mode, ovnkube master and node run in the same process."
    echo "-ic  | --enable-interconnect        Enable interconnect with each node as a zone (only valid if OVN_HA is false)"
    echo "-nqe | --network-qos-enable         Enable network QoS. DEFAULT: Disabled."
    echo "--disable-ovnkube-identity          Disable per-node cert and ovnkube-identity webhook"
    echo "-npz | --nodes-per-zone             If interconnect is enabled, number of nodes per zone (Default 1). If this value > 1, then (total k8s nodes (workers + 1) / num of nodes per zone) should be zero."
    echo "-mtu                                Define the overlay mtu"
    echo "--isolated                          Deploy with an isolated environment (no default gateway)"
    echo "--delete                            Delete current cluster"
    echo "--deploy                            Deploy ovn kubernetes without restarting kind"
    echo "--add-nodes                         Adds nodes to an existing cluster. The number of nodes to be added is specified by --num-workers. Also use -ic if the cluster is using interconnect."
    echo "-dns | --enable-dnsnameresolver     Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall."
    echo "-obs | --observability              Enable OVN Observability feature."
    echo ""
}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            -cf | --config-file )               shift
                                                if test ! -f "$1"; then
                                                    echo "$1 does not  exist"
                                                    usage
                                                    exit 1
                                                fi
                                                KIND_CONFIG=$1
                                                ;;
            -ii | --install-ingress )           KIND_INSTALL_INGRESS=true
                                                ;;
            -mlb | --install-metallb )          KIND_INSTALL_METALLB=true
                                                ;;
            -pl | --install-cni-plugins )       KIND_INSTALL_PLUGINS=true
                                                ;;
            -ikv | --install-kubevirt)          KIND_INSTALL_KUBEVIRT=true
                                                ;;
            -nokvipam | --opt-out-kv-ipam)      KIND_OPT_OUT_KUBEVIRT_IPAM=true
                                                ;;
            -ha | --ha-enabled )                OVN_HA=true
                                                ;;
            -me | --multicast-enabled)          OVN_MULTICAST_ENABLE=true
                                                ;;
            -ho | --hybrid-enabled )            OVN_HYBRID_OVERLAY_ENABLE=true
                                                ;;
            -ds | --disable-snat-multiple-gws ) OVN_DISABLE_SNAT_MULTIPLE_GWS=true
                                                ;;
            -df | --disable-forwarding )        OVN_DISABLE_FORWARDING=true
                                                ;;
            -ecp | --encap-port )               shift
                                                OVN_ENCAP_PORT=$1
                                                ;;
            -dp | --disable-pkt-mtu-check )     OVN_DISABLE_PKT_MTU_CHECK=true
                                                ;;
            -ep | --experimental-provider )     shift
                                                export KIND_EXPERIMENTAL_PROVIDER=$1
                                                ;;
            -nf | --netflow-targets )           shift
                                                OVN_NETFLOW_TARGETS=$1
                                                ;;
            -sf | --sflow-targets )             shift
                                                OVN_SFLOW_TARGETS=$1
                                                ;;
            -if | --ipfix-targets )             shift
                                                OVN_IPFIX_TARGETS=$1
                                                ;;
            -ifs | --ipfix-sampling )           shift
                                                OVN_IPFIX_SAMPLING=$1
                                                ;;
            -ifm | --ipfix-cache-max-flows )    shift
                                                OVN_IPFIX_CACHE_MAX_FLOWS=$1
                                                ;;
            -ifa | --ipfix-cache-active-timeout ) shift
                                                OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=$1
                                                ;;
            -obs | --observability )            OVN_OBSERV_ENABLE=true
                                                ;;
            -el | --ovn-empty-lb-events )       OVN_EMPTY_LB_EVENTS=true
                                                ;;
            -kt | --keep-taint )                KIND_REMOVE_TAINT=false
                                                ;;
            -n4 | --no-ipv4 )                   KIND_IPV4_SUPPORT=false
                                                ;;
            -i6 | --ipv6 )                      KIND_IPV6_SUPPORT=true
                                                ;;
            -is | --ipsec )                     ENABLE_IPSEC=true
                                                ;;
            -wk | --num-workers )               shift
                                                if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                    echo "Invalid num-workers: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                KIND_NUM_WORKER=$1
                                                ;;
            -npz | --nodes-per-zone )           shift
                                                if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                    echo "Invalid num-nodes-per-zone: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                KIND_NUM_NODES_PER_ZONE=$1
                                                ;;
            -sw | --allow-system-writes )       KIND_ALLOW_SYSTEM_WRITES=true
                                                ;;
            -gm | --gateway-mode )              shift
                                                if [ "$1" != "local" ] && [ "$1" != "shared" ]; then
                                                    echo "Invalid gateway mode: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                OVN_GATEWAY_MODE=$1
                                                ;;
            -dgb | --dummy-gateway-bridge)      OVN_DUMMY_GATEWAY_BRIDGE=true
                                                ;;
            -ov | --ovn-image )           	    shift
                                          	    OVN_IMAGE=$1
                                          	    ;;
            -ml  | --master-loglevel )          shift
                                                if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                    echo "Invalid master-loglevel: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                MASTER_LOG_LEVEL=$1
                                                ;;
            -nl  | --node-loglevel )            shift
                                                if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                    echo "Invalid node-loglevel: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                NODE_LOG_LEVEL=$1
                                                ;;
            -dbl | --dbchecker-loglevel )       shift
                                                if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                    echo "Invalid dbchecker-loglevel: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                DBCHECKER_LOG_LEVEL=$1
                                                ;;
            -eb | --egress-gw-separate-bridge ) OVN_ENABLE_EX_GW_NETWORK_BRIDGE=true
                                                ;;
            -ndl | --ovn-loglevel-northd )      shift
                                                OVN_LOG_LEVEL_NORTHD=$1
                                                ;;
            -nbl | --ovn-loglevel-nb )          shift
                                                OVN_LOG_LEVEL_NB=$1
                                                ;;
            -sbl | --ovn-loglevel-sb )          shift
                                                OVN_LOG_LEVEL_SB=$1
                                                ;;
            -cl  | --ovn-loglevel-controller )  shift
                                                OVN_LOG_LEVEL_CONTROLLER=$1
                                                ;;
            -lcl | --libovsdb-client-logfile )  shift
                                                LIBOVSDB_CLIENT_LOGFILE=$1
                                                ;;
            -hns | --host-network-namespace )   OVN_HOST_NETWORK_NAMESPACE=$1
                                                ;;
            -lr | --local-kind-registry )       KIND_LOCAL_REGISTRY=true
                                                ;;
            -dd | --dns-domain )                shift
                                                KIND_DNS_DOMAIN=$1
                                                ;;
            -cn | --cluster-name )              shift
                                                KIND_CLUSTER_NAME=$1
                                                ;;
            -kc | --kubeconfig )                shift
                                                KUBECONFIG=$1
                                                ;;
            -ric | --run-in-container )         RUN_IN_CONTAINER=true
                                                ;;
            -ehp | --egress-ip-healthcheck-port ) shift
                                                if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                    echo "Invalid egress-ip-healthcheck-port: $1"
                                                    usage
                                                    exit 1
                                                fi
                                                OVN_EGRESSIP_HEALTHCHECK_PORT=$1
                                                ;;
           -sm  | --scale-metrics )             OVN_METRICS_SCALE_ENABLE=true
                                                ;;
           -cm  | --compact-mode )              OVN_COMPACT_MODE=true
                                                ;;
            --isolated )                        OVN_ISOLATED=true
                                                ;;
            -mne | --multi-network-enable )     ENABLE_MULTI_NET=true
                                                ;;
            -nse | --network-segmentation-enable) ENABLE_NETWORK_SEGMENTATION=true
                                                  ;;
            -ic | --enable-interconnect )       OVN_ENABLE_INTERCONNECT=true
                                                ;;
            --disable-ovnkube-identity)         OVN_ENABLE_OVNKUBE_IDENTITY=false
                                                ;;
            -mtu  )                             shift
                                                OVN_MTU=$1
                                                ;;
            -nqe | --network-qos-enable )       OVN_NETWORK_QOS_ENABLE=true
                                                ;;
            --delete )                          delete
                                                exit
                                                ;;
            --deploy)                           KIND_CREATE=false
                                                ;;
            --add-nodes)                        KIND_ADD_NODES=true
                                                KIND_CREATE=false
                                                ;;
            -dns | --enable-dnsnameresolver )   OVN_ENABLE_DNSNAMERESOLVER=true
                                                ;;
            -h | --help )                       usage
                                                exit
                                                ;;
            * )                                 echo "Invalid option: $1"
                                                usage
                                                exit 1
        esac
        shift
    done
}

print_params() {
     echo "Using these parameters to install KIND"
     echo ""
     echo "KUBECONFIG = $KUBECONFIG"
     echo "MANIFEST_OUTPUT_DIR = $MANIFEST_OUTPUT_DIR"
     echo "KIND_INSTALL_INGRESS = $KIND_INSTALL_INGRESS"
     echo "KIND_INSTALL_METALLB = $KIND_INSTALL_METALLB"
     echo "KIND_INSTALL_PLUGINS = $KIND_INSTALL_PLUGINS"
     echo "KIND_INSTALL_KUBEVIRT = $KIND_INSTALL_KUBEVIRT"
     echo "KIND_OPT_OUT_KUBEVIRT_IPAM = $KIND_OPT_OUT_KUBEVIRT_IPAM"
     echo "OVN_HA = $OVN_HA"
     echo "RUN_IN_CONTAINER = $RUN_IN_CONTAINER"
     echo "KIND_CLUSTER_NAME = $KIND_CLUSTER_NAME"
     echo "KIND_LOCAL_REGISTRY = $KIND_LOCAL_REGISTRY"
     echo "KIND_LOCAL_REGISTRY_NAME = $KIND_LOCAL_REGISTRY_NAME"
     echo "KIND_LOCAL_REGISTRY_PORT = $KIND_LOCAL_REGISTRY_PORT"
     echo "KIND_DNS_DOMAIN = $KIND_DNS_DOMAIN"
     echo "KIND_CONFIG_FILE = $KIND_CONFIG"
     echo "KIND_REMOVE_TAINT = $KIND_REMOVE_TAINT"
     echo "KIND_IPV4_SUPPORT = $KIND_IPV4_SUPPORT"
     echo "KIND_IPV6_SUPPORT = $KIND_IPV6_SUPPORT"
     echo "ENABLE_IPSEC = $ENABLE_IPSEC"
     echo "KIND_ALLOW_SYSTEM_WRITES = $KIND_ALLOW_SYSTEM_WRITES"
     echo "KIND_EXPERIMENTAL_PROVIDER = $KIND_EXPERIMENTAL_PROVIDER"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_DUMMY_GATEWAY_BRIDGE = $OVN_DUMMY_GATEWAY_BRIDGE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_DISABLE_SNAT_MULTIPLE_GWS = $OVN_DISABLE_SNAT_MULTIPLE_GWS"
     echo "OVN_DISABLE_FORWARDING = $OVN_DISABLE_FORWARDING"
     echo "OVN_ENCAP_PORT = $OVN_ENCAP_PORT"
     echo "OVN_DISABLE_PKT_MTU_CHECK = $OVN_DISABLE_PKT_MTU_CHECK"
     echo "OVN_NETFLOW_TARGETS = $OVN_NETFLOW_TARGETS"
     echo "OVN_SFLOW_TARGETS = $OVN_SFLOW_TARGETS"
     echo "OVN_IPFIX_TARGETS = $OVN_IPFIX_TARGETS"
     echo "OVN_IPFIX_SAMPLING = $OVN_IPFIX_SAMPLING"
     echo "OVN_IPFIX_CACHE_MAX_FLOWS = $OVN_IPFIX_CACHE_MAX_FLOWS"
     echo "OVN_IPFIX_CACHE_ACTIVE_TIMEOUT = $OVN_IPFIX_CACHE_ACTIVE_TIMEOUT"
     echo "OVN_OBSERV_ENABLE = $OVN_OBSERV_ENABLE"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "MASTER_LOG_LEVEL = $MASTER_LOG_LEVEL"
     echo "NODE_LOG_LEVEL = $NODE_LOG_LEVEL"
     echo "DBCHECKER_LOG_LEVEL = $DBCHECKER_LOG_LEVEL"
     echo "OVN_LOG_LEVEL_NORTHD = $OVN_LOG_LEVEL_NORTHD"
     echo "OVN_LOG_LEVEL_NB = $OVN_LOG_LEVEL_NB"
     echo "OVN_LOG_LEVEL_SB = $OVN_LOG_LEVEL_SB"
     echo "OVN_LOG_LEVEL_CONTROLLER = $OVN_LOG_LEVEL_CONTROLLER"
     echo "OVN_LOG_LEVEL_NBCTLD = $OVN_LOG_LEVEL_NBCTLD"
     echo "LIBOVSDB_CLIENT_LOGFILE = $LIBOVSDB_CLIENT_LOGFILE"
     echo "OVN_HOST_NETWORK_NAMESPACE = $OVN_HOST_NETWORK_NAMESPACE"
     echo "OVN_ENABLE_EX_GW_NETWORK_BRIDGE = $OVN_ENABLE_EX_GW_NETWORK_BRIDGE"
     echo "OVN_EX_GW_NETWORK_INTERFACE = $OVN_EX_GW_NETWORK_INTERFACE"
     echo "OVN_EGRESSIP_HEALTHCHECK_PORT = $OVN_EGRESSIP_HEALTHCHECK_PORT"
     echo "OVN_DEPLOY_PODS = $OVN_DEPLOY_PODS"
     echo "OVN_METRICS_SCALE_ENABLE = $OVN_METRICS_SCALE_ENABLE"
     echo "OVN_ISOLATED = $OVN_ISOLATED"
     echo "ENABLE_MULTI_NET = $ENABLE_MULTI_NET"
     echo "ENABLE_NETWORK_SEGMENTATION= $ENABLE_NETWORK_SEGMENTATION"
     echo "OVN_ENABLE_INTERCONNECT = $OVN_ENABLE_INTERCONNECT"
     if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
       echo "KIND_NUM_NODES_PER_ZONE = $KIND_NUM_NODES_PER_ZONE"
       if [ "${KIND_NUM_NODES_PER_ZONE}" -gt 1 ] && [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" = "true" ]; then
         echo "multi_node_zone is not compatible with ovnkube_identity, disabling ovnkube_identity"
         OVN_ENABLE_OVNKUBE_IDENTITY="false"
       fi
     fi
     echo "OVN_ENABLE_OVNKUBE_IDENTITY = $OVN_ENABLE_OVNKUBE_IDENTITY"
     echo "OVN_NETWORK_QOS_ENABLE = $OVN_NETWORK_QOS_ENABLE"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo "OVN_MTU= $OVN_MTU"
     echo "OVN_ENABLE_DNSNAMERESOLVER= $OVN_ENABLE_DNSNAMERESOLVER"
     echo ""
}

install_jinjanator_renderer() {
  # ensure jinjanator renderer installed
  pip install wheel --user
  pip freeze | grep jinjanator || pip install jinjanator[yaml] --user
  export PATH=~/.local/bin:$PATH
}

check_dependencies() {
  if ! command_exists curl ; then
    echo "Dependency not met: Command not found 'curl'"
    exit 1
  fi

  if ! command_exists kubectl ; then
    echo "'kubectl' not found, installing"
    setup_kubectl_bin
  fi

  if ! command_exists kind ; then
    echo "Dependency not met: Command not found 'kind'"
    exit 1
  fi

  if ! command_exists jq ; then
    echo "Dependency not met: Command not found 'jq'"
    exit 1
  fi

  if ! command_exists awk ; then
    echo "Dependency not met: Command not found 'awk'"
    exit 1
  fi

  if ! command_exists jinjanate ; then
    if ! command_exists pip ; then
      echo "Dependency not met: 'jinjanator' not installed and cannot install with 'pip'"
      exit 1
    fi
    echo "'jinjanate' not found, installing with 'pip'"
    install_jinjanator_renderer
  fi

  if ! command_exists docker && ! command_exists podman; then
  	  echo "Dependency not met: Neither docker nor podman found"
  	  exit 1
  fi
}

OPENSSL=""
set_openssl_binary() {
  for s in openssl openssl3; do
      if ! command_exists "${s}" ; then
          continue
      fi
      if [ "$(${s} version | awk -F '[ |.]' '{print $2}')" == "3" ]; then
          OPENSSL="${s}"
          echo "Found OpenSSL version 3 in binary ${OPENSSL}"
          break
      fi
  done
  if [ "${OPENSSL}" == "" ] ; then
    echo "Dependency not met: Cannot find openssl version 3 (searched for openssl and openssl3)"
    exit 1
  fi
}

set_default_params() {
  # Set default values
  # Used for multi cluster setups
  KIND_CREATE=${KIND_CREATE:-true}
  KIND_ADD_NODES=${KIND_ADD_NODES:-false}
  KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}
  # Scrub any existing kubeconfigs at the path
  if [ "${KIND_CREATE}" == true ]; then
    rm -f ${KUBECONFIG}
  fi
  MANIFEST_OUTPUT_DIR=${MANIFEST_OUTPUT_DIR:-${DIR}/../dist/yaml}
  if [ ${KIND_CLUSTER_NAME} != "ovn" ]; then
    MANIFEST_OUTPUT_DIR="${DIR}/../dist/yaml/${KIND_CLUSTER_NAME}"
  fi
  RUN_IN_CONTAINER=${RUN_IN_CONTAINER:-false}
  KIND_IMAGE=${KIND_IMAGE:-kindest/node}
  K8S_VERSION=${K8S_VERSION:-v1.31.1}
  OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-shared}
  KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
  KIND_INSTALL_METALLB=${KIND_INSTALL_METALLB:-false}
  KIND_INSTALL_PLUGINS=${KIND_INSTALL_PLUGINS:-false}
  KIND_INSTALL_KUBEVIRT=${KIND_INSTALL_KUBEVIRT:-false}
  KIND_OPT_OUT_KUBEVIRT_IPAM=${KIND_OPT_OUT_KUBEVIRT_IPAM:-false}
  OVN_HA=${OVN_HA:-false}
  KIND_LOCAL_REGISTRY=${KIND_LOCAL_REGISTRY:-false}
  KIND_LOCAL_REGISTRY_NAME=${KIND_LOCAL_REGISTRY_NAME:-kind-registry}
  KIND_LOCAL_REGISTRY_PORT=${KIND_LOCAL_REGISTRY_PORT:-5000}
  KIND_DNS_DOMAIN=${KIND_DNS_DOMAIN:-"cluster.local"}
  KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind.yaml.j2}
  KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
  KIND_IPV4_SUPPORT=${KIND_IPV4_SUPPORT:-true}
  KIND_IPV6_SUPPORT=${KIND_IPV6_SUPPORT:-false}
  ENABLE_IPSEC=${ENABLE_IPSEC:-false}
  OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  OVN_DISABLE_SNAT_MULTIPLE_GWS=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-false}
  OVN_DISABLE_FORWARDING=${OVN_DISABLE_FORWARDING:=false}
  OVN_ENCAP_PORT=${OVN_ENCAP_PORT:-""}
  OVN_DISABLE_PKT_MTU_CHECK=${OVN_DISABLE_PKT_MTU_CHECK:-false}
  OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  KIND_ALLOW_SYSTEM_WRITES=${KIND_ALLOW_SYSTEM_WRITES:-false}
  OVN_IMAGE=${OVN_IMAGE:-local}
  MASTER_LOG_LEVEL=${MASTER_LOG_LEVEL:-5}
  NODE_LOG_LEVEL=${NODE_LOG_LEVEL:-5}
  DBCHECKER_LOG_LEVEL=${DBCHECKER_LOG_LEVEL:-5}
  OVN_LOG_LEVEL_NORTHD=${OVN_LOG_LEVEL_NORTHD:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_NB=${OVN_LOG_LEVEL_NB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_SB=${OVN_LOG_LEVEL_SB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_CONTROLLER=${OVN_LOG_LEVEL_CONTROLLER:-"-vconsole:info"}
  OVN_LOG_LEVEL_NBCTLD=${OVN_LOG_LEVEL_NBCTLD:-"-vconsole:info"}
  LIBOVSDB_CLIENT_LOGFILE=${LIBOVSDB_CLIENT_LOGFILE:-}
  OVN_ENABLE_EX_GW_NETWORK_BRIDGE=${OVN_ENABLE_EX_GW_NETWORK_BRIDGE:-false}
  OVN_EX_GW_NETWORK_INTERFACE=""
  if [ "$OVN_ENABLE_EX_GW_NETWORK_BRIDGE" == true ]; then
    OVN_EX_GW_NETWORK_INTERFACE="eth1"
  fi
  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  MASQUERADE_SUBNET_IPV4=${MASQUERADE_SUBNET_IPV4:-169.254.0.0/17}
  MASQUERADE_SUBNET_IPV6=${MASQUERADE_SUBNET_IPV6:-fd69::/112}
  TRANSIT_SWITCH_SUBNET_IPV4=${TRANSIT_SWITCH_SUBNET_IPV4:-100.88.0.0/16}
  TRANSIT_SWITCH_SUBNET_IPV6=${TRANSIT_SWITCH_SUBNET_IPV6:-fd97::/64}
  METALLB_CLIENT_NET_SUBNET_IPV4=${METALLB_CLIENT_NET_SUBNET_IPV4:-172.22.0.0/16}
  METALLB_CLIENT_NET_SUBNET_IPV6=${METALLB_CLIENT_NET_SUBNET_IPV6:-fc00:f853:ccd:e792::/64}

  KIND_NUM_MASTER=1
  OVN_ENABLE_INTERCONNECT=${OVN_ENABLE_INTERCONNECT:-false}
  OVN_ENABLE_OVNKUBE_IDENTITY=${OVN_ENABLE_OVNKUBE_IDENTITY:-true}
  OVN_NETWORK_QOS_ENABLE=${OVN_NETWORK_QOS_ENABLE:-false}


  if [ "$OVN_COMPACT_MODE" == true ] && [ "$OVN_ENABLE_INTERCONNECT" != false ]; then
     echo "Compact mode cannot be used together with Interconnect"
     exit 1
  fi

  if [ "$OVN_HA" == true ]; then
    KIND_NUM_MASTER=3
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-0}
  else
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
  fi

  if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
    KIND_NUM_NODES_PER_ZONE=${KIND_NUM_NODES_PER_ZONE:-1}

    TOTAL_NODES=$((KIND_NUM_WORKER + KIND_NUM_MASTER))
    if [[ ${KIND_NUM_NODES_PER_ZONE} -gt 1 ]] && [[ $((TOTAL_NODES % KIND_NUM_NODES_PER_ZONE)) -ne 0 ]]; then
      echo "(Total k8s nodes / number of nodes per zone) should be zero"
      exit 1
    fi
  fi

  OVN_HOST_NETWORK_NAMESPACE=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}
  OVN_EGRESSIP_HEALTHCHECK_PORT=${OVN_EGRESSIP_HEALTHCHECK_PORT:-9107}
  OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}
  OVN_DEPLOY_PODS=${OVN_DEPLOY_PODS:-"ovnkube-zone-controller ovnkube-control-plane ovnkube-master ovnkube-node"}
  OVN_METRICS_SCALE_ENABLE=${OVN_METRICS_SCALE_ENABLE:-false}
  OVN_ISOLATED=${OVN_ISOLATED:-false}
  OVN_GATEWAY_OPTS=${OVN_GATEWAY_OPTS:-""}
  if [ "$OVN_ISOLATED" == true ]; then
    OVN_GATEWAY_OPTS="--gateway-interface=eth0"
  fi
  OVN_DUMMY_GATEWAY_BRIDGE=${OVN_DUMMY_GATEWAY_BRIDGE:-false}
  if [ "$OVN_DUMMY_GATEWAY_BRIDGE" == true ]; then
    OVN_GATEWAY_OPTS="--allow-no-uplink --gateway-interface=br-ex"
  fi
  ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-false}
  ENABLE_NETWORK_SEGMENTATION=${ENABLE_NETWORK_SEGMENTATION:-false}
  OVN_COMPACT_MODE=${OVN_COMPACT_MODE:-false}
  if [ "$OVN_COMPACT_MODE" == true ]; then
    KIND_NUM_WORKER=0
  fi
  OVN_MTU=${OVN_MTU:-1400}
  OVN_ENABLE_DNSNAMERESOLVER=${OVN_ENABLE_DNSNAMERESOLVER:-false}
  OVN_OBSERV_ENABLE=${OVN_OBSERV_ENABLE:-false}
}

check_ipv6() {
  if [ "$KIND_IPV6_SUPPORT" == true ]; then
    # Collect additional IPv6 data on test environment
    ERROR_FOUND=false
    TMPVAR=$(sysctl net.ipv6.conf.all.forwarding | awk '{print $3}')
    echo "net.ipv6.conf.all.forwarding is equal to $TMPVAR"
    if [ "$TMPVAR" != 1 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.forwarding=1
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.forwarding=1' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    TMPVAR=$(sysctl net.ipv6.conf.all.disable_ipv6 | awk '{print $3}')
    echo "net.ipv6.conf.all.disable_ipv6 is equal to $TMPVAR"
    if [ "$TMPVAR" != 0 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    if [ -f /proc/net/if_inet6 ]; then
      echo "/proc/net/if_inet6 exists so IPv6 supported in kernel."
    else
      echo "/proc/net/if_inet6 does not exists so no IPv6 support found! Compile the kernel!!"
      ERROR_FOUND=true
    fi
    if "$ERROR_FOUND"; then
      exit 2
    fi
  fi
}

set_cluster_cidr_ip_families() {
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
    IP_FAMILY="dual"
    NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
    echo "Dual Stack Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  else
    echo "Invalid setup. KIND_IPV4_SUPPORT and/or KIND_IPV6_SUPPORT must be true."
    exit 1
  fi
}

create_local_registry() {
    # create registry container unless it already exists
    if [ "$($OCI_BIN inspect -f '{{.State.Running}}' "${KIND_LOCAL_REGISTRY_NAME}" 2>/dev/null || true)" != 'true' ]; then
      $OCI_BIN run \
        -d --restart=always -p "127.0.0.1:${KIND_LOCAL_REGISTRY_PORT}:5000" --name "${KIND_LOCAL_REGISTRY_NAME}" \
        registry:2
    fi
}

connect_local_registry() {
    # connect the registry to the cluster network if not already connected
    if [ "$($OCI_BIN inspect -f='{{json .NetworkSettings.Networks.kind}}' "${KIND_LOCAL_REGISTRY_NAME}")" = 'null' ]; then
      $OCI_BIN network connect "kind" "${KIND_LOCAL_REGISTRY_NAME}"
    fi

    # Reference docs for local registry:
    # - https://kind.sigs.k8s.io/docs/user/local-registry/
    # - https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${KIND_LOCAL_REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

}

scale_kind_cluster() {
  echo "Scaling cluster to ${KIND_NUM_WORKER} workers..."
  rm -rf /tmp/kindscaler
  # change this to https://github.com/lobuhi/kindscaler once PR https://github.com/lobuhi/kindscaler/pull/1 is accepted
  git clone https://github.com/trozet/kindscaler /tmp/kindscaler
  /tmp/kindscaler/kindscaler.sh ${KIND_CLUSTER_NAME} -r worker -c ${KIND_NUM_WORKER}
  if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
      if [ "${KIND_NUM_NODES_PER_ZONE}" == "1" ]; then
       label_ovn_single_node_zones
      else
        label_ovn_multiple_nodes_zones
      fi
  fi
  if [ "$OVN_IMAGE" == local ]; then
    set_ovn_image
  fi
  install_ovn_image
  if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    set_dnsnameresolver_images
    install_dnsnameresolver_images
  fi
}

create_kind_cluster() {
  # Output of the jinjanate command
  KIND_CONFIG_LCL=${DIR}/kind-${KIND_CLUSTER_NAME}.yaml

  ovn_ip_family=${IP_FAMILY} \
  ovn_ha=${OVN_HA} \
  net_cidr=${NET_CIDR} \
  svc_cidr=${SVC_CIDR} \
  use_local_registy=${KIND_LOCAL_REGISTRY} \
  dns_domain=${KIND_DNS_DOMAIN} \
  ovn_num_master=${KIND_NUM_MASTER} \
  ovn_num_worker=${KIND_NUM_WORKER} \
  cluster_log_level=${KIND_CLUSTER_LOGLEVEL:-4} \
  kind_local_registry_port=${KIND_LOCAL_REGISTRY_PORT} \
  kind_local_registry_name=${KIND_LOCAL_REGISTRY_NAME} \
  jinjanate "${KIND_CONFIG}" -o "${KIND_CONFIG_LCL}"

  # Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
  if kind get clusters | grep ovn; then
    delete
  fi
  
  if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
    create_local_registry
  fi
  
  kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config=${KIND_CONFIG_LCL} --retain

  cat "${KUBECONFIG}"
}

set_ovn_image() {
  # if we're using the local registry and still need to build, push to local registry
  if [ "$KIND_LOCAL_REGISTRY" == true ];then
    OVN_IMAGE="localhost:5000/ovn-daemonset-fedora:latest"
  else
    OVN_IMAGE="localhost/ovn-daemonset-fedora:dev"
  fi
}

build_ovn_image() {
  if [ "$OVN_IMAGE" == local ]; then
    set_ovn_image

    # Build ovn image
    pushd ${DIR}/../go-controller
    make
    popd

    # Build ovn kube image
    pushd ${DIR}/../dist/images
    # Find all built executables, but ignore the 'windows' directory if it exists
    find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
    echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
    $OCI_BIN build -t "${OVN_IMAGE}" -f Dockerfile.fedora .

    # store in local registry
    if [ "$KIND_LOCAL_REGISTRY" == true ];then
      echo "Pushing built image to local $OCI_BIN registry"
      $OCI_BIN push "${OVN_IMAGE}"
    fi
    popd
  # We should push to local registry if image is not remote
  elif [ "${OVN_IMAGE}" != "" -a "${KIND_LOCAL_REGISTRY}" == true ] && (echo "$OVN_IMAGE" | grep / -vq); then 
    local local_registry_ovn_image="localhost:5000/${OVN_IMAGE}"
    $OCI_BIN tag "$OVN_IMAGE" $local_registry_ovn_image
    OVN_IMAGE=$local_registry_ovn_image
    $OCI_BIN push $OVN_IMAGE
  fi
}

create_ovn_kube_manifests() {
    local ovnkube_image=${OVN_IMAGE}
    if [ "$KIND_LOCAL_REGISTRY" == true ];then
      # When updating with local registry we have to reference the sha
      ovnkube_image=$($OCI_BIN inspect --format='{{index .RepoDigests 0}}' $OVN_IMAGE)
    fi
    pushd ${DIR}/../dist/images
    if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
      KIND_NUM_NODES_PER_ZONE=${KIND_NUM_NODES_PER_ZONE:-1}
    fi
  ./daemonset.sh \
    --output-directory="${MANIFEST_OUTPUT_DIR}"\
    --image="${OVN_IMAGE}" \
    --ovnkube-image="${ovnkube_image}" \
    --net-cidr="${NET_CIDR}" \
    --svc-cidr="${SVC_CIDR}" \
    --gateway-mode="${OVN_GATEWAY_MODE}" \
    --dummy-gateway-bridge="${OVN_DUMMY_GATEWAY_BRIDGE}" \
    --gateway-options="${OVN_GATEWAY_OPTS}" \
    --enable-ipsec="${ENABLE_IPSEC}" \
    --hybrid-enabled="${OVN_HYBRID_OVERLAY_ENABLE}" \
    --disable-snat-multiple-gws="${OVN_DISABLE_SNAT_MULTIPLE_GWS}" \
    --disable-forwarding="${OVN_DISABLE_FORWARDING}" \
    --ovn-encap-port="${OVN_ENCAP_PORT}" \
    --disable-pkt-mtu-check="${OVN_DISABLE_PKT_MTU_CHECK}" \
    --ovn-empty-lb-events="${OVN_EMPTY_LB_EVENTS}" \
    --multicast-enabled="${OVN_MULTICAST_ENABLE}" \
    --k8s-apiserver="${API_URL}" \
    --ovn-master-count="${KIND_NUM_MASTER}" \
    --ovn-unprivileged-mode=no \
    --master-loglevel="${MASTER_LOG_LEVEL}" \
    --node-loglevel="${NODE_LOG_LEVEL}" \
    --dbchecker-loglevel="${DBCHECKER_LOG_LEVEL}" \
    --ovn-loglevel-northd="${OVN_LOG_LEVEL_NORTHD}" \
    --ovn-loglevel-nb="${OVN_LOG_LEVEL_NB}" \
    --ovn-loglevel-sb="${OVN_LOG_LEVEL_SB}" \
    --ovn-loglevel-controller="${OVN_LOG_LEVEL_CONTROLLER}" \
    --ovnkube-libovsdb-client-logfile="${LIBOVSDB_CLIENT_LOGFILE}" \
    --ovnkube-config-duration-enable=true \
    --admin-network-policy-enable=true \
    --egress-ip-enable=true \
    --egress-ip-healthcheck-port="${OVN_EGRESSIP_HEALTHCHECK_PORT}" \
    --egress-firewall-enable=true \
    --egress-qos-enable=true \
    --egress-service-enable=true \
    --v4-join-subnet="${JOIN_SUBNET_IPV4}" \
    --v6-join-subnet="${JOIN_SUBNET_IPV6}" \
    --v4-masquerade-subnet="${MASQUERADE_SUBNET_IPV4}" \
    --v6-masquerade-subnet="${MASQUERADE_SUBNET_IPV6}" \
    --v4-transit-switch-subnet="${TRANSIT_SWITCH_SUBNET_IPV4}" \
    --v6-transit-switch-subnet="${TRANSIT_SWITCH_SUBNET_IPV6}" \
    --ex-gw-network-interface="${OVN_EX_GW_NETWORK_INTERFACE}" \
    --multi-network-enable="${ENABLE_MULTI_NET}" \
    --network-segmentation-enable="${ENABLE_NETWORK_SEGMENTATION}" \
    --ovnkube-metrics-scale-enable="${OVN_METRICS_SCALE_ENABLE}" \
    --compact-mode="${OVN_COMPACT_MODE}" \
    --enable-interconnect="${OVN_ENABLE_INTERCONNECT}" \
    --enable-multi-external-gateway=true \
    --enable-ovnkube-identity="${OVN_ENABLE_OVNKUBE_IDENTITY}" \
    --enable-persistent-ips=true \
    --network-qos-enable="${OVN_NETWORK_QOS_ENABLE}" \
    --mtu="${OVN_MTU}" \
    --enable-dnsnameresolver="${OVN_ENABLE_DNSNAMERESOLVER}" \
    --mtu="${OVN_MTU}" \
    --enable-observ="${OVN_OBSERV_ENABLE}"
  popd
}

install_ovn_image() {
  install_image ${OVN_IMAGE}
}

install_ovn_global_zone() {
  if [ "$OVN_HA" == true ]; then
    run_kubectl apply -f ovnkube-db-raft.yaml
  else
    run_kubectl apply -f ovnkube-db.yaml
  fi

  if [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" == true ]; then
    run_kubectl apply -f ovnkube-identity.yaml
  fi
  run_kubectl apply -f ovnkube-master.yaml
  run_kubectl apply -f ovnkube-node.yaml
}

label_ovn_single_node_zones() {
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    kubectl label node "${n}" k8s.ovn.org/zone-name=${n} --overwrite
  done
}

install_ovn_single_node_zones() {
  label_ovn_single_node_zones

  if [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" == true ]; then
    run_kubectl apply -f ovnkube-identity.yaml
  fi
  run_kubectl apply -f ovnkube-control-plane.yaml
  run_kubectl apply -f ovnkube-single-node-zone.yaml
}

label_ovn_multiple_nodes_zones() {
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sort)
  zone_idx=1
  n=1
  for node in $KIND_NODES; do
    zone="zone-${zone_idx}"
    kubectl label node "${node}" k8s.ovn.org/zone-name=${zone} --overwrite
    if [ "${n}" == "1" ]; then
      # Mark 1st node of each zone as zone control plane
      kubectl label node "${node}" node-role.kubernetes.io/zone-controller="" --overwrite
    fi

    if [ "${n}" == "${KIND_NUM_NODES_PER_ZONE}" ]; then
      n=1
      zone_idx=$((zone_idx+1))
    else
      n=$((n+1))
    fi
  done
}


install_ovn_multiple_nodes_zones() {
  label_ovn_multiple_nodes_zones

  if [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" == true ]; then
    run_kubectl apply -f ovnkube-identity.yaml
  fi
  run_kubectl apply -f ovnkube-control-plane.yaml
  run_kubectl apply -f ovnkube-zone-controller.yaml
  run_kubectl apply -f ovnkube-node.yaml
}

install_ovn() {
  pushd ${MANIFEST_OUTPUT_DIR}

  run_kubectl apply -f k8s.ovn.org_egressfirewalls.yaml
  run_kubectl apply -f k8s.ovn.org_egressips.yaml
  run_kubectl apply -f k8s.ovn.org_egressqoses.yaml
  run_kubectl apply -f k8s.ovn.org_egressservices.yaml
  run_kubectl apply -f k8s.ovn.org_adminpolicybasedexternalroutes.yaml
  run_kubectl apply -f k8s.ovn.org_networkqoses.yaml
  run_kubectl apply -f k8s.ovn.org_userdefinednetworks.yaml
  # NOTE: When you update vendoring versions for the ANP & BANP APIs, we must update the version of the CRD we pull from in the below URL
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
  run_kubectl apply -f ovn-setup.yaml
  run_kubectl apply -f rbac-ovnkube-identity.yaml
  run_kubectl apply -f rbac-ovnkube-cluster-manager.yaml
  run_kubectl apply -f rbac-ovnkube-master.yaml
  run_kubectl apply -f rbac-ovnkube-node.yaml
  run_kubectl apply -f rbac-ovnkube-db.yaml
  MASTER_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sort | head -n "${KIND_NUM_MASTER}")
  # We want OVN HA not Kubernetes HA
  # leverage the kubeadm well-known label node-role.kubernetes.io/control-plane=
  # to choose the nodes where ovn master components will be placed
  for n in $MASTER_NODES; do
    kubectl label node "$n" k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
    if [ "$KIND_REMOVE_TAINT" == true ]; then
      # do not error if it fails to remove the taint
      # remove both master and control-plane taints until master is removed from 1.25
      # // https://github.com/kubernetes/kubernetes/pull/107533
      kubectl taint node "$n" node-role.kubernetes.io/master:NoSchedule- || true
      kubectl taint node "$n" node-role.kubernetes.io/control-plane:NoSchedule- || true
    fi
  done

  run_kubectl apply -f ovs-node.yaml

  if [ "$OVN_ENABLE_INTERCONNECT" == false ]; then
    install_ovn_global_zone
  else
    if [ "${KIND_NUM_NODES_PER_ZONE}" == "1" ]; then
      install_ovn_single_node_zones
    else
      install_ovn_multiple_nodes_zones
    fi
  fi

  popd

  # When using internal registry force pod reload just the ones with 
  # non OVS containers, restarting OVS pods breaks the cluster.
  if [ "${KIND_CREATE}" == false ] && [ "${KIND_LOCAL_REGISTRY}" == false ] ; then
    for pod in ${OVN_DEPLOY_PODS}; do
        run_kubectl delete pod -n ovn-kubernetes -l name=$pod
    done
  fi
}

# install_ipsec will apply the IPsec DaemonSet, create a CA that can be used by the IPsec pods. It will then add it to
# configmap -n ovn-kubernetes signer-ca. After that, it will monitor all CSRs that are pending and it will sign those
# with the CA cert. After each iteration, it will check if the ovn-ipsec DaemonSet pods rolled out successfully.
# Make sure to run this at the very end of the setup process.
install_ipsec() {
  pushd "${MANIFEST_OUTPUT_DIR}"
  run_kubectl apply -f ovn-ipsec.yaml
  popd

  # Create the CA (stored inside the signer-ca ConfigMap) that the IPsec pods use to sign their certificates
  ca_dir=$(mktemp -d)
  pushd "${ca_dir}"
  ${OPENSSL} genrsa -out ca-bundle.key 4096
  ${OPENSSL} req -x509 -new -nodes -key ca-bundle.key -sha256 -days 10240 -out ca-bundle.crt \
      -subj "/C=CA/ST=Arctica/L=Northpole/O=Acme Inc/OU=DevOps/CN=www.example.com/emailAddress=dev@www.example.com"
  kubectl create configmap -n ovn-kubernetes signer-ca --from-file ca-bundle.crt

  # For ca. 5 minutes max (60 * 5 seconds + overhead) ...
  success=false
  for i in {1..60}; do
    # ... try to get all CSRs and sign them
    csrs=$(oc get csr -o go-template='{{range .items}}{{if not .status}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}')
    for csr in ${csrs}; do
      kubectl get csr "${csr}" -o jsonpath='{.spec.request}' | base64 --decode | \
          sed -n '/BEGIN CERTIFICATE REQUEST/,$p' > "${csr}"
      ${OPENSSL} x509 -req -in ${csr} -CA ca-bundle.crt -CAkey ca-bundle.key -CAcreateserial -out "${csr}.crt" -days 3650  \
          -sha256 -extensions v3_req -copy_extensions copy
      kubectl get csr "${csr}" -o json | \
          jq '.status.certificate = "'$(base64 "${csr}.crt" | tr -d '\n')'"' | \
          kubectl replace --raw /apis/certificates.k8s.io/v1/certificatesigningrequests/${csr}/status -f -
    done

    # ... and then check if the ovn-ipsec DaemonSet rolled out completely (wait for 5 seconds)
    if kubectl rollout status daemonset -n ovn-kubernetes ovn-ipsec --timeout 5s; then
        echo "All IPsec pods rolled out successfully"
        success=true
        break
    fi
    echo "IPsec pods did not roll out successfully yet"
  done
  popd
  rm -Rf "${ca_dir}"

  if ! ${success}; then
      echo "IPsec pods did not roll out successfully"
      exit 1
  fi
}

docker_create_second_interface() {
  echo "adding second interfaces to nodes"

  # Create the network as dual stack, regardless of the type of the deployment. Ignore if already exists.
  "$OCI_BIN" network create --ipv6 --driver=bridge kindexgw --subnet=172.19.0.0/16 --subnet=fc00:f853:ccd:e798::/64 || true

  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    "$OCI_BIN" network connect kindexgw "$n"
  done
}

# run_script_in_container should be used when kind.sh is run nested in a container
# and makes sure the control-plane node is rechable by substituting 127.0.0.1
# with the control-plane container's IP
run_script_in_container() {
  local master_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' ${KIND_CLUSTER_NAME}-control-plane | head -n 1)
  sed -i -- "s/server: .*/server: https:\/\/$master_ip:6443/g" $KUBECONFIG
  chmod a+r $KUBECONFIG
}

# fixup_config_names should be used to ensure kind clusters are named based off
# provided values, essentially it removes the 'kind' prefix from the cluster names
fixup_kubeconfig_names() {
  sed -i -- "s/user: kind-.*/user: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/name: kind-.*/name: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/cluster: kind-.*/cluster: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
  sed -i -- "s/current-context: .*/current-context: ${KIND_CLUSTER_NAME}/g" $KUBECONFIG
}

remove_default_route() {
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    docker exec "$n" ip route delete default
  done
}

add_dns_hostnames() {
  dns="$'"
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  # find all IPs and build dns entries
  for n in $KIND_NODES; do
	ip=$(docker container inspect -f '{{ .NetworkSettings.Networks.kind.IPAddress }}' $n)
        dns+="$ip $n \n"
        ip=$(docker container inspect -f '{{ .NetworkSettings.Networks.kind.GlobalIPv6Address }}' $n)
	dns+="$ip $n \n"
  done

  dns+="'"

  # update dns on each node
  for n in $KIND_NODES; do
	docker exec $n bash -c "echo  $dns >> /etc/hosts"
  done
}

check_dependencies
# In order to allow providing arguments with spaces, e.g. "-vconsole:info -vfile:info"
# the original command <parse_args $*> was replaced by <parse_args "$@">
parse_args "$@"
set_default_params
print_params
if [ "${ENABLE_IPSEC}" == true ]; then
  set_openssl_binary
fi

set -euxo pipefail
if [ "$KIND_ADD_NODES" == true ]; then
  scale_kind_cluster
  kubectl_wait_pods
  exit 0
fi

check_ipv6
set_cluster_cidr_ip_families
if [ "$KIND_CREATE" == true ]; then
    create_kind_cluster
    if [ "$RUN_IN_CONTAINER" == true ]; then
      run_script_in_container
    fi
    # if cluster name is specified fixup kubeconfig
    if [ "$KIND_CLUSTER_NAME" != "ovn" ]; then
      fixup_kubeconfig_names
    fi
    if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
      connect_local_registry
    fi
    docker_disable_ipv6
    if [ "$OVN_ENABLE_EX_GW_NETWORK_BRIDGE" == true ]; then
      docker_create_second_interface
    fi
    coredns_patch
    if [ "$OVN_ISOLATED" == true ]; then
      remove_default_route
      add_dns_hostnames
    fi
fi
if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    build_dnsnameresolver_images
    install_dnsnameresolver_images
    install_dnsnameresolver_operator
    update_clusterrole_coredns
    add_ocp_dnsnameresolver_to_coredns_config
    update_coredns_deployment_image
fi
build_ovn_image
detect_apiserver_url
create_ovn_kube_manifests
install_ovn_image
install_ovn
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  install_ingress
fi
if [ "$ENABLE_MULTI_NET" == true ]; then
  enable_multi_net
fi
kubectl_wait_pods
if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    kubectl_wait_dnsnameresolver_pods
fi
sleep_until_pods_settle
# Launch IPsec pods last to make sure that CSR signing logic works
# Launch csr_signer in background
# Wait for DaemonSet to rollout
if [ "${ENABLE_IPSEC}" == true ]; then
  install_ipsec
fi
if [ "$KIND_INSTALL_METALLB" == true ]; then
  install_metallb
fi
if [ "$KIND_INSTALL_PLUGINS" == true ]; then
  install_plugins
fi
if [ "$KIND_INSTALL_KUBEVIRT" == true ]; then
  install_kubevirt
  deploy_kubevirt_binding
  deploy_passt_binary

  install_cert_manager
  if [ "$KIND_OPT_OUT_KUBEVIRT_IPAM" != true ]; then
    install_kubevirt_ipam_controller
  fi
fi
