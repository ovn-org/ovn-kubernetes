#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -eo pipefail

# Returns the full directory name of the script
export DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

# Source the kind-common.sh file from the same directory where this script is located
source "${DIR}/kind-common.sh"
source "${DIR}/kind-dpu-sim-lib.sh"

OVN_HELM_EXTRA_VALUES=()

set_default_params() {
  set_common_default_params
  check_ipv6
  set_cluster_cidr_ip_families
  DPU_MODE=${DPU_MODE:-none}
  if [[ "${DPU_MODE}" != "none" && "${DPU_MODE}" != "host" && "${DPU_MODE}" != "dpu" ]]; then
    echo "Invalid DPU_MODE: ${DPU_MODE}. Expected one of: none, host, dpu"
    exit 1
  fi
  local ovnkube_identity_default=true
  if [ "${DPU_MODE}" != "none" ]; then
    ovnkube_identity_default=false
  fi
  OVN_ENABLE_OVNKUBE_IDENTITY=${OVN_ENABLE_OVNKUBE_IDENTITY:-${ovnkube_identity_default}}
}

usage() {
    echo "usage: kind-helm.sh [--delete]"
    echo "       [ -cf  | --config-file <file> ]"
    echo "       [ -kt  | --keep-taint ]"
    echo "       [ -me  | --multicast-enabled ]"
    echo "       [ -ho  | --hybrid-enabled ]"
    echo "       [ -el  | --ovn-empty-lb-events ]"
    echo "       [ -ii  | --install-ingress ]"
    echo "       [ -mlb | --install-metallb ]"
    echo "       [ -pl  | --install-cni-plugins ]"
    echo "       [ -ikv | --install-kubevirt ]"
    echo "       [ -mne | --multi-network-enable ]"
    echo "       [ -nse | --network-segmentation-enable ]"
    echo "       [ -nce | --network-connect-enable ]"
    echo "       [ -ue | --uplink-enable ]"
    echo "       [ -uae | --preconfigured-udn-addresses-enable ]"
    echo "       [ -rae | --route-advertisements-enable ]"
    echo "       [ -evpn | --evpn-enable ]"
    echo "       [-dudn | --dynamic-udn-allocation]"
    echo "       [-dug | --dynamic-udn-removal-grace-period]"
    echo "       [-adv | --advertise-default-network]"
    echo "       [-rud | --routed-udn-isolation-disable]"
    echo "       [ -nqe | --network-qos-enable ]"
    echo "       [ -noe | --no-overlay-enable [snat-enabled|managed] ]"
    echo "       [ -n4  | --no-ipv4 ]"
    echo "       [ -i6  | --ipv6 ]"
    echo "       [ -wk  | --num-workers <num> ]"
    echo "       [ -ov  | --ovn-image <image> ]"
    echo "       [ -ovr | --ovn-repo <repo> ]"
    echo "       [ -ovg | --ovn-gitref <ref> ]"
    echo "       [ -cn  | --cluster-name ]"
    echo "       [ -mip | --metrics-ip <ip> ]"
    echo "       [ -mtu <mtu> ]"
    echo "       [ --dpu-mode <none|host|dpu> ]"
    echo "       [ -f | --extra-values <file> ]"
    echo "       [ --frr-k8s-remote-kubeconfig <file> ]"
    echo "       [ --frr-k8s-host-kubeconfig <file> ]"
    echo "       [ --frr-k8s-remote-node-map <host=dpu[,host=dpu...]> ]"
    echo "       [ --bgp-net-name <name> ]"
    echo "       [ --bgp-server-name <name> ]"
    echo "       [ --frr-container-name <name> ]"
    echo "       [ --bgp-server-net-subnet-ipv4 <subnet> ]"
    echo "       [ --bgp-server-net-subnet-ipv6 <subnet> ]"
    echo "       [ --bgp-server-host-port <port> ]"
    echo "       [ --enable-coredumps ]"
    echo "       [ -ub | --uplink-bridge ]"
    echo "       [ -h ]"
    echo ""
    echo "--delete                                      Delete current cluster"
    echo "-cf  | --config-file                          Name of the KIND configuration file"
    echo "-kt  | --keep-taint                           Do not remove taint components"
    echo "                                              DEFAULT: Remove taint components"
    echo "-me  | --multicast-enabled                    Enable multicast. DEFAULT: Disabled"
    echo "-ho  | --hybrid-enabled                       Enable hybrid overlay. DEFAULT: Disabled"
    echo "-obs | --observability                        Enable observability. DEFAULT: Disabled"
    echo "-el  | --ovn-empty-lb-events                  Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled"
    echo "-ii  | --install-ingress                      Flag to install Ingress Components."
    echo "                                              DEFAULT: Don't install ingress components."
    echo "-mlb | --install-metallb                      Install metallb to test service type LoadBalancer deployments"
    echo "-pl  | --install-cni-plugins                  Install CNI plugins"
    echo "-ikv | --install-kubevirt                     Install kubevirt"
    echo "-mne | --multi-network-enable                 Enable multi networks. DEFAULT: Disabled"
    echo "-nse | --network-segmentation-enable          Enable network segmentation. DEFAULT: Disabled"
    echo "-nce | --network-connect-enable               Enable network connect (requires network segmentation). DEFAULT: Disabled"
    echo "-ue  | --uplink-enable                        Enable uplink (requires network segmentation). DEFAULT: Disabled"
    echo "-uae | --preconfigured-udn-addresses-enable   Enable connecting workloads with preconfigured network to user-defined networks. DEFAULT: Disabled"
    echo "-rae | --route-advertisements-enable          Enable route advertisements"
    echo "-evpn | --evpn-enable                         Enable EVPN"
    echo "-dudn | --dynamic-udn-allocation              Enable dynamic UDN allocation. DEFAULT: Disabled"
    echo "-dug | --dynamic-udn-removal-grace-period     Configure the grace period in seconds for dynamic UDN removal. DEFAULT: 120 seconds"
    echo "-adv | --advertise-default-network            Applies a RouteAdvertisements configuration to advertise the default network on all nodes"
    echo "-rud | --routed-udn-isolation-disable         Disable isolation across BGP-advertised UDNs (sets advertised-udn-isolation-mode=loose). DEFAULT: strict."
    echo "-nqe | --network-qos-enable                   Enable network QoS. DEFAULT: Disabled"
    echo "-noe | --no-overlay-enable [snat-enabled|managed] Enable no overlay for the default network. Optional value: 'snat-enabled' to enable SNAT, 'managed' to enable SNAT and managed routing. DEFAULT: disabled."
    echo "-ds  | --disable-snat-multiple-gws            Disable SNAT for multiple external gateways. DEFAULT: Enabled"
    echo "-df  | --disable-forwarding                   Disable forwarding on all interfaces. DEFAULT: Enabled"
    echo "--disable-ovnkube-identity                    Disable per-node cert and ovnkube-identity webhook. DEFAULT: Enabled"
    echo "-dgb | --dummy-gateway-bridge                 Use a dummy instead of a real gateway bridge. DEFAULT: Disabled"
    echo "-gm  | --gateway-mode                         Configure the cluster gateway mode (local|shared). DEFAULT: shared"
    echo "-n4  | --no-ipv4                              Disable IPv4. DEFAULT: IPv4 Enabled."
    echo "-i6  | --ipv6                                 Enable IPv6. DEFAULT: IPv6 Disabled."
    echo "-wk  | --num-workers                          Number of worker nodes. DEFAULT: 2 workers"
    echo "-ov  | --ovn-image                            Use the specified docker image instead of building locally. DEFAULT: local build."
    echo "-ovr | --ovn-repo                             Specify the repository to build OVN from"
    echo "-ovg | --ovn-gitref                           Specify the branch, tag or commit id to build OVN from, it can be a pattern like 'branch-*' it will order results and use the first one"
    echo "-cn  | --cluster-name                         Configure the kind cluster's name"
    echo "-mip | --metrics-ip                           IP address to bind metrics endpoints. DEFAULT: K8S_NODE_IP or 0.0.0.0"
    echo "-mtu                                          Define the overlay mtu. DEFAULT: 1400 (1500 for no-overlay mode)"
    echo "--enable-coredumps                            Enable coredump collection on kind nodes. DEFAULT: Disabled"
    echo "-dns | --enable-dnsnameresolver               Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall."
    echo "-mps | --multi-pod-subnet                     Use multiple subnets for the default cluster network"
    echo "--allow-icmp-netpol                           Allows ICMP and ICMPv6 traffic globally, regardless of network policy rules"
    echo "-ecp | --encap-port                           GENEVE UDP tunnel port."
    echo "-dp  | --disable-pkt-mtu-check                Disable checking for packets mtu size. DEFAULT: false"
    echo "-is  | --ipsec                                Enable IPsec. DEFAULT: false"
    echo "-sm  | --scale-metrics                        Enable scale metrics. DEFAULT: false"
    echo "-ehp | --egress-ip-healthcheck-port           TCP port used for gRPC session by egress IP node check. DEFAULT: 9107 (Use \"0\" for legacy dial to port 9)."
    echo "-nf  | --netflow-targets                      A comma-separated set of NetFlow collectors to export flow data. DEFAULT: Disabled"
    echo "-sf  | --sflow-targets                        A comma-separated set of SFlow collectors to export flow data. DEFAULT: Disabled"
    echo "-if  | --ipfix-targets                        A comma-separated set of IPFIX collectors to export flow data. DEFAULT: Disabled"
    echo "-ifs | --ipfix-sampling                       Rate at which packets should be sampled and sent to each target collector. DEFAULT: 400"
    echo "-ifm | --ipfix-cache-max-flows                Maximum number of IPFIX flow records that can be cached at a time. DEFAULT: 0 (disabled)"
    echo "-ifa | --ipfix-cache-active-timeout           Maximum period in seconds for which an IPFIX flow record is cached. DEFAULT: 60"
    echo "-lcl | --libovsdb-client-logfile              Separate logs for libovsdb client into provided file. DEFAULT: do not separate."
    echo "-eb  | --egress-gw-separate-bridge            The external gateway traffic uses a separate bridge (sets up xgw bridge and eth1)."
    echo "-ub  | --uplink-bridge                        Create an unmanaged OVS bridge for Uplink testing on each KIND node."
    echo "-lr  | --local-kind-registry                  Configure kind to use a local container registry for images."
    echo "-ep  | --experimental-provider                Use an experimental OCI provider such as podman instead of docker."
    echo "--deploy                                      Deploy ovn-kubernetes without restarting kind"
    echo "--add-nodes                                   Adds nodes to an existing cluster. Number of nodes set by --num-workers."
    echo "--isolated                                    After cluster creation, remove default route from nodes and publish kind node IPs as /etc/hosts entries for DNS-less isolation."
    echo "-ml  | --master-loglevel                      Log level for ovnkube-control-plane pods (0..5). DEFAULT: 4"
    echo "-nl  | --node-loglevel                        Log level for ovnkube-node pods (0..5). DEFAULT: 4"
    echo "-nbl | --ovn-loglevel-nb                      Log level for ovn-nbdb. DEFAULT: '-vconsole:info -vfile:info'"
    echo "-sbl | --ovn-loglevel-sb                      Log level for ovn-sbdb. DEFAULT: '-vconsole:info -vfile:info'"
    echo "-ndl | --ovn-loglevel-northd                  Log level for ovn-northd. DEFAULT: '-vconsole:info -vfile:info'"
    echo "-cl  | --ovn-loglevel-controller              Log level for ovn-controller. DEFAULT: '-vconsole:info'"
    echo "-dd  | --dns-domain                           Configure a custom dnsDomain for k8s services. DEFAULT: 'cluster.local'"
    echo "-inf | --num-infra                            Number of infra (tainted, not-ready) kind nodes. DEFAULT: 0"
    echo "-hns | --host-network-namespace               Namespace used to classify host-network traffic. DEFAULT: 'ovn-host-network'"
    echo "-prom | --install-prometheus                  Install Prometheus monitoring stack."
    echo "-sw  | --allow-system-writes                  Allow the script to write to /etc/hosts and other system files when needed."
    echo "-ric | --run-in-container                     Run the script from inside a docker container (adapts kubeconfig API URL)."
    echo "-kc  | --kubeconfig                           Output kubeconfig path. DEFAULT: \$HOME/\$KIND_CLUSTER_NAME.conf"
    echo "--dpu-mode                                    Deploy OVN-Kubernetes for DPU simulator mode: none, host, or dpu. DEFAULT: none"
    echo "-f | --extra-values                           Extra Helm values file appended after the base values file. May be repeated."
    echo "--frr-k8s-remote-kubeconfig                   Kubeconfig used by DPU-cluster FRR-K8S pods to watch the host cluster API."
    echo "--frr-k8s-host-kubeconfig                     Kubeconfig used by this script to write FRR-K8S resources to the host cluster API."
    echo "--frr-k8s-remote-node-map                     Comma-separated host-node=dpu-node pairs for remote FRR-K8S node-name mapping."
    echo "--bgp-net-name                                Name of the docker network hosting the external BGP server. DEFAULT: bgpnet"
    echo "--bgp-server-name                             Name of the external BGP server container. DEFAULT: bgpserver"
    echo "--frr-container-name                          Name of the external FRR router container. DEFAULT: frr"
    echo "--bgp-server-net-subnet-ipv4                  IPv4 subnet of the external BGP server network. DEFAULT: 172.26.0.0/16"
    echo "--bgp-server-net-subnet-ipv6                  IPv6 subnet of the external BGP server network. DEFAULT: fc00:f853:ccd:e796::/64"
    echo "--bgp-server-host-port                        Host port published by the external BGP server container. DEFAULT: 18080"
    echo "-nokvipam | --opt-out-kv-ipam                 Skip installing the KubeVirt IPAM controller (requires --install-kubevirt)."
    echo ""

}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            --delete )                            KIND_DELETE=true
                                                  ;;
            -cf | --config-file )                 shift
                                                  if test ! -f "$1"; then
                                                      echo "$1 does not  exist"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_CONFIG=$1
                                                  ;;
            -kt | --keep-taint )                  KIND_REMOVE_TAINT=false
                                                  ;;
            -me | --multicast-enabled)            OVN_MULTICAST_ENABLE=true
                                                  ;;
            -ho | --hybrid-enabled )              OVN_HYBRID_OVERLAY_ENABLE=true
                                                  ;;
            -obs | --observability )              OVN_OBSERV_ENABLE=true
                                                  ;;
            -el | --ovn-empty-lb-events )         OVN_EMPTY_LB_EVENTS=true
                                                  ;;
            -ii | --install-ingress )             KIND_INSTALL_INGRESS=true
                                                  ;;
            -mlb | --install-metallb )            KIND_INSTALL_METALLB=true
                                                  ;;
            -pl | --install-cni-plugins )         KIND_INSTALL_PLUGINS=true
                                                  ;;
            -ikv | --install-kubevirt)            KIND_INSTALL_KUBEVIRT=true
                                                  ;;
            -mne | --multi-network-enable )       ENABLE_MULTI_NET=true
                                                  ;;
            -nse | --network-segmentation-enable) ENABLE_NETWORK_SEGMENTATION=true
                                                  ;;
            -nce | --network-connect-enable )     ENABLE_NETWORK_CONNECT=true
                                                  ;;
            -ue | --uplink-enable )               ENABLE_UPLINK=true
                                                  ;;
            -uae | --preconfigured-udn-addresses-enable)    ENABLE_PRE_CONF_UDN_ADDR=true
                                                  ;;
            -rae | --route-advertisements-enable) ENABLE_ROUTE_ADVERTISEMENTS=true
                                                  ;;
            -evpn | --evpn-enable)                ENABLE_EVPN=true
                                                  ;;
            -adv | --advertise-default-network)   ADVERTISE_DEFAULT_NETWORK=true
                                                  ;;
            -rud | --routed-udn-isolation-disable) ADVERTISED_UDN_ISOLATION_MODE=loose
                                                  ;;
            -dudn | --dynamic-udn-allocation)     DYNAMIC_UDN_ALLOCATION=true
                                                  ;;
            -dug  | --dynamic-udn-removal-grace-period) shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --dynamic-udn-removal-grace-period" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  DYNAMIC_UDN_GRACE_PERIOD=$1
                                                  if [[ "$DYNAMIC_UDN_GRACE_PERIOD" =~ ^[0-9]+$ ]]; then
                                                    DYNAMIC_UDN_GRACE_PERIOD="${DYNAMIC_UDN_GRACE_PERIOD}s"
                                                  fi
                                                  ;;
            -nqe | --network-qos-enable )         OVN_NETWORK_QOS_ENABLE=true
                                                  ;;
            -noe | --no-overlay-enable )          ENABLE_NO_OVERLAY=true
                                                  # Check if next argument is a valid value
                                                  if [[ -n "$2" && ! "$2" =~ ^- ]]; then
                                                    if [[ "$2" == "snat-enabled" ]]; then
                                                      ENABLE_NO_OVERLAY_OUTBOUND_SNAT=true
                                                      shift  # consume the value argument
                                                    elif [[ "$2" == "managed" ]]; then
                                                      ENABLE_NO_OVERLAY_OUTBOUND_SNAT=true
                                                      ENABLE_NO_OVERLAY_MANAGED_ROUTING=true
                                                      shift  # consume the value argument
                                                    else
                                                      echo "Error: Invalid value for --no-overlay-enable: $2"
                                                      echo "Valid values are: snat-enabled, managed"
                                                      exit 1
                                                    fi
                                                  else
                                                    ENABLE_NO_OVERLAY_OUTBOUND_SNAT=false
                                                    ENABLE_NO_OVERLAY_MANAGED_ROUTING=false
                                                  fi
                                                  ;;
            -ds | --disable-snat-multiple-gws )   OVN_DISABLE_SNAT_MULTIPLE_GWS=true
                                                  ;;
            -df | --disable-forwarding )          OVN_DISABLE_FORWARDING=true
                                                  ;;
            -dgb | --dummy-gateway-bridge )       OVN_DUMMY_GATEWAY_BRIDGE=true
                                                  ;;
            -gm | --gateway-mode )                shift
                                                  OVN_GATEWAY_MODE=$1
                                                  ;;
            -n4 | --no-ipv4 )                     PLATFORM_IPV4_SUPPORT=false
                                                  ;;
            -i6 | --ipv6 )                        PLATFORM_IPV6_SUPPORT=true
                                                  ;;
            -wk | --num-workers )                 shift
                                                  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                      echo "Invalid num-workers: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_NUM_WORKER=$1
                                                  ;;
            -ov | --ovn-image )                   shift
                                                  OVN_IMAGE=$1
                                                  ;;
            -ovr | --ovn-repo )                   shift
                                                  OVN_REPO=$1
                                                  ;;
            -ovg | --ovn-gitref )                 shift
                                                  OVN_GITREF=$1
                                                  ;;
            -cn | --cluster-name )                shift
                                                  KIND_CLUSTER_NAME=$1
                                                  ;;
            -dns | --enable-dnsnameresolver )     OVN_ENABLE_DNSNAMERESOLVER=true
                                                  ;;
            --allow-icmp-netpol )                 OVN_ALLOW_ICMP_NETPOL=true
                                                  ;;
            --disable-ovnkube-identity )          OVN_ENABLE_OVNKUBE_IDENTITY=false
                                                  ;;
            -mps| --multi-pod-subnet )            MULTI_POD_SUBNET=true
                                                  ;;
            -mip | --metrics-ip ) shift
                                                  METRICS_IP="$1"
                                                  ;;
            -mtu )                                shift
                                                  OVN_MTU=$1
                                                  ;;
            --enable-coredumps )                  ENABLE_COREDUMPS=true
                                                  ;;
            -ecp | --encap-port )                 shift
                                                  OVN_ENCAP_PORT=$1
                                                  ;;
            -dp | --disable-pkt-mtu-check )       OVN_DISABLE_PKT_MTU_CHECK=true
                                                  ;;
            -is | --ipsec )                       ENABLE_IPSEC=true
                                                  ;;
            -sm | --scale-metrics )               OVN_METRICS_SCALE_ENABLE=true
                                                  ;;
            -ehp | --egress-ip-healthcheck-port ) shift
                                                  OVN_EGRESSIP_HEALTHCHECK_PORT=$1
                                                  ;;
            -nf | --netflow-targets )             shift
                                                  OVN_NETFLOW_TARGETS=$1
                                                  ;;
            -sf | --sflow-targets )               shift
                                                  OVN_SFLOW_TARGETS=$1
                                                  ;;
            -if | --ipfix-targets )               shift
                                                  OVN_IPFIX_TARGETS=$1
                                                  ;;
            -ifs | --ipfix-sampling )             shift
                                                  OVN_IPFIX_SAMPLING=$1
                                                  ;;
            -ifm | --ipfix-cache-max-flows )      shift
                                                  OVN_IPFIX_CACHE_MAX_FLOWS=$1
                                                  ;;
            -ifa | --ipfix-cache-active-timeout ) shift
                                                  OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=$1
                                                  ;;
            -lcl | --libovsdb-client-logfile )    shift
                                                  LIBOVSDB_CLIENT_LOGFILE=$1
                                                  ;;
            -eb | --egress-gw-separate-bridge )   OVN_SECOND_BRIDGE=true
                                                  ;;
            -ub | --uplink-bridge )               OVN_UPLINK_BRIDGE=true
                                                  ;;
            -lr | --local-kind-registry )         KIND_LOCAL_REGISTRY=true
                                                  ;;
            -ep | --experimental-provider )       shift
                                                  export KIND_EXPERIMENTAL_PROVIDER="$1"
                                                  ;;
            -h | --help )                         usage
                                                  exit
                                                  ;;
            --deploy )                            KIND_CREATE=false
                                                  ;;
            --add-nodes )                         KIND_ADD_NODES=true
                                                  KIND_CREATE=false
                                                  ;;
            --isolated )                          OVN_ISOLATED=true
                                                  ;;
            -ml | --master-loglevel )             shift
                                                  MASTER_LOG_LEVEL=$1
                                                  ;;
            -nl | --node-loglevel )               shift
                                                  NODE_LOG_LEVEL=$1
                                                  ;;
            -nbl | --ovn-loglevel-nb )            shift
                                                  OVN_LOG_LEVEL_NB=$1
                                                  ;;
            -sbl | --ovn-loglevel-sb )            shift
                                                  OVN_LOG_LEVEL_SB=$1
                                                  ;;
            -ndl | --ovn-loglevel-northd )        shift
                                                  OVN_LOG_LEVEL_NORTHD=$1
                                                  ;;
            -cl | --ovn-loglevel-controller )     shift
                                                  OVN_LOG_LEVEL_CONTROLLER=$1
                                                  ;;
            -dd | --dns-domain )                  shift
                                                  KIND_DNS_DOMAIN=$1
                                                  ;;
            -inf | --num-infra )                  shift
                                                  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                      echo "Invalid num-infra: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_NUM_INFRA=$1
                                                  ;;
            -hns | --host-network-namespace )     shift
                                                  OVN_HOST_NETWORK_NAMESPACE=$1
                                                  ;;
            -prom | --install-prometheus )        KIND_INSTALL_PROMETHEUS=true
                                                  ;;
            -sw | --allow-system-writes )         KIND_ALLOW_SYSTEM_WRITES=true
                                                  ;;
            -ric | --run-in-container )           RUN_IN_CONTAINER=true
                                                  ;;
            -kc | --kubeconfig )                  shift
                                                  KUBECONFIG=$1
                                                  ;;
            --dpu-mode )                          shift
                                                  DPU_MODE=$1
                                                  if [[ "${DPU_MODE}" != "none" && "${DPU_MODE}" != "host" && "${DPU_MODE}" != "dpu" ]]; then
                                                    echo "Invalid --dpu-mode: ${DPU_MODE}. Expected one of: none, host, dpu"
                                                    exit 1
                                                  fi
                                                  if [[ "${DPU_MODE}" != "none" && -z "${OVN_ENABLE_OVNKUBE_IDENTITY:-}" ]]; then
                                                    OVN_ENABLE_OVNKUBE_IDENTITY=false
                                                  fi
                                                  ;;
            -f | --extra-values )                 shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --extra-values" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  OVN_HELM_EXTRA_VALUES+=("$1")
                                                  ;;
            --frr-k8s-remote-kubeconfig )         shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --frr-k8s-remote-kubeconfig" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  FRR_K8S_REMOTE_KUBECONFIG=$1
                                                  ;;
            --frr-k8s-host-kubeconfig )           shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --frr-k8s-host-kubeconfig" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  FRR_K8S_HOST_KUBECONFIG=$1
                                                  ;;
            --frr-k8s-remote-node-map )           shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --frr-k8s-remote-node-map" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  FRR_K8S_REMOTE_NODE_MAP=$1
                                                  ;;
            --bgp-net-name )                      shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --bgp-net-name" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  BGP_NET_NAME=$1
                                                  ;;
            --bgp-server-name )                   shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --bgp-server-name" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  BGP_SERVER_NAME=$1
                                                  ;;
            --frr-container-name )                shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --frr-container-name" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  FRR_CONTAINER_NAME=$1
                                                  ;;
            --bgp-server-net-subnet-ipv4 )        shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --bgp-server-net-subnet-ipv4" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  BGP_SERVER_NET_SUBNET_IPV4=$1
                                                  ;;
            --bgp-server-net-subnet-ipv6 )        shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --bgp-server-net-subnet-ipv6" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  BGP_SERVER_NET_SUBNET_IPV6=$1
                                                  ;;
            --bgp-server-host-port )              shift
                                                  if ! [[ "${1:-}" =~ ^[0-9]+$ ]]; then
                                                    echo "Invalid bgp-server-host-port: ${1:-}" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  BGP_SERVER_HOST_PORT=$1
                                                  ;;
            -nokvipam | --opt-out-kv-ipam )       KIND_OPT_OUT_KUBEVIRT_IPAM=true
                                                  ;;
            * )                                   usage
                                                  exit 1
        esac
        shift
    done

}

print_params() {
     echo "Using these parameters to deploy KIND + helm"
     echo ""
     echo "KIND_CONFIG_FILE = $KIND_CONFIG"
     echo "KUBECONFIG = $KUBECONFIG"
     echo "OCI_BIN = $OCI_BIN"
     echo "KIND_INSTALL_INGRESS = $KIND_INSTALL_INGRESS"
     echo "KIND_INSTALL_METALLB = $KIND_INSTALL_METALLB"
     echo "KIND_INSTALL_PLUGINS = $KIND_INSTALL_PLUGINS"
     echo "KIND_INSTALL_KUBEVIRT = $KIND_INSTALL_KUBEVIRT"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_OBSERV_ENABLE = $OVN_OBSERV_ENABLE"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "KIND_CLUSTER_NAME = $KIND_CLUSTER_NAME"
     echo "KIND_REMOVE_TAINT = $KIND_REMOVE_TAINT"
     echo "ENABLE_MULTI_NET = $ENABLE_MULTI_NET"
     echo "ENABLE_NETWORK_SEGMENTATION = $ENABLE_NETWORK_SEGMENTATION"
     echo "ENABLE_NETWORK_CONNECT = $ENABLE_NETWORK_CONNECT"
     echo "ENABLE_UPLINK = $ENABLE_UPLINK"
     echo "ENABLE_PRE_CONF_UDN_ADDR = $ENABLE_PRE_CONF_UDN_ADDR"
     echo "ENABLE_ROUTE_ADVERTISEMENTS = $ENABLE_ROUTE_ADVERTISEMENTS"
     echo "BGP_NET_NAME = $BGP_NET_NAME"
     echo "BGP_SERVER_NAME = $BGP_SERVER_NAME"
     echo "FRR_CONTAINER_NAME = $FRR_CONTAINER_NAME"
     echo "BGP_SERVER_NET_SUBNET_IPV4 = $BGP_SERVER_NET_SUBNET_IPV4"
     echo "BGP_SERVER_NET_SUBNET_IPV6 = $BGP_SERVER_NET_SUBNET_IPV6"
     echo "BGP_SERVER_HOST_PORT = $BGP_SERVER_HOST_PORT"
     echo "ENABLE_EVPN = $ENABLE_EVPN"
     echo "ADVERTISE_DEFAULT_NETWORK = $ADVERTISE_DEFAULT_NETWORK"
     echo "ADVERTISED_UDN_ISOLATION_MODE = $ADVERTISED_UDN_ISOLATION_MODE"
     echo "OVN_NETWORK_QOS_ENABLE = $OVN_NETWORK_QOS_ENABLE"
     echo "ENABLE_NO_OVERLAY = $ENABLE_NO_OVERLAY"
     echo "ENABLE_NO_OVERLAY_OUTBOUND_SNAT = $ENABLE_NO_OVERLAY_OUTBOUND_SNAT"
     echo "ENABLE_NO_OVERLAY_MANAGED_ROUTING = $ENABLE_NO_OVERLAY_MANAGED_ROUTING"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_SECOND_BRIDGE = $OVN_SECOND_BRIDGE"
     echo "OVN_UPLINK_BRIDGE = $OVN_UPLINK_BRIDGE"
     echo "OVN_UPLINK_BRIDGE_NAME = $OVN_UPLINK_BRIDGE_NAME"
     echo "OVN_UPLINK_NETWORK_NAME = $OVN_UPLINK_NETWORK_NAME"
     echo "OVN_UPLINK_NETWORK_IPV4 = $OVN_UPLINK_NETWORK_IPV4"
     echo "OVN_UPLINK_NETWORK_IPV6 = $OVN_UPLINK_NETWORK_IPV6"
     echo "OVN_DISABLE_SNAT_MULTIPLE_GWS = $OVN_DISABLE_SNAT_MULTIPLE_GWS"
     echo "OVN_DISABLE_FORWARDING = $OVN_DISABLE_FORWARDING"
     echo "OVN_UNPRIVILEGED_MODE = $OVN_UNPRIVILEGED_MODE"
     echo "OVN_MTU = $OVN_MTU"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "OVN_REPO = $OVN_REPO"
     echo "OVN_GITREF = $OVN_GITREF"
     echo "KIND_NUM_MASTER = $KIND_NUM_MASTER"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo "OVN_ENABLE_DNSNAMERESOLVER= $OVN_ENABLE_DNSNAMERESOLVER"
     echo "MULTI_POD_SUBNET= $MULTI_POD_SUBNET"
     echo "OVN_ALLOW_ICMP_NETPOL= $OVN_ALLOW_ICMP_NETPOL"
     echo "DYNAMIC_UDN_ALLOCATION = $DYNAMIC_UDN_ALLOCATION"
     echo "DYNAMIC_UDN_GRACE_PERIOD =  $DYNAMIC_UDN_GRACE_PERIOD"
     echo "ENABLE_IPSEC = $ENABLE_IPSEC"
     echo "OVN_ENCAP_PORT = $OVN_ENCAP_PORT"
     echo "OVN_DISABLE_PKT_MTU_CHECK = $OVN_DISABLE_PKT_MTU_CHECK"
     echo "OVN_METRICS_SCALE_ENABLE = $OVN_METRICS_SCALE_ENABLE"
     echo "OVN_EGRESSIP_HEALTHCHECK_PORT = $OVN_EGRESSIP_HEALTHCHECK_PORT"
     echo "OVN_NETFLOW_TARGETS = $OVN_NETFLOW_TARGETS"
     echo "OVN_SFLOW_TARGETS = $OVN_SFLOW_TARGETS"
     echo "OVN_IPFIX_TARGETS = $OVN_IPFIX_TARGETS"
     echo "OVN_IPFIX_SAMPLING = $OVN_IPFIX_SAMPLING"
     echo "OVN_IPFIX_CACHE_MAX_FLOWS = $OVN_IPFIX_CACHE_MAX_FLOWS"
     echo "OVN_IPFIX_CACHE_ACTIVE_TIMEOUT = $OVN_IPFIX_CACHE_ACTIVE_TIMEOUT"
     echo "LIBOVSDB_CLIENT_LOGFILE = $LIBOVSDB_CLIENT_LOGFILE"
     echo "OVN_ISOLATED = $OVN_ISOLATED"
     echo "KIND_ADD_NODES = $KIND_ADD_NODES"
     echo "KIND_CREATE = $KIND_CREATE"
     echo "KIND_LOCAL_REGISTRY = $KIND_LOCAL_REGISTRY"
     echo "KIND_DNS_DOMAIN = $KIND_DNS_DOMAIN"
     echo "KIND_NUM_INFRA = $KIND_NUM_INFRA"
     echo "OVN_HOST_NETWORK_NAMESPACE = $OVN_HOST_NETWORK_NAMESPACE"
     echo "KIND_INSTALL_PROMETHEUS = $KIND_INSTALL_PROMETHEUS"
     echo "KIND_ALLOW_SYSTEM_WRITES = $KIND_ALLOW_SYSTEM_WRITES"
     echo "RUN_IN_CONTAINER = $RUN_IN_CONTAINER"
     echo "DPU_MODE = $DPU_MODE"
     echo "OVN_HELM_EXTRA_VALUES = ${OVN_HELM_EXTRA_VALUES[*]}"
     echo "FRR_K8S_REMOTE_KUBECONFIG = ${FRR_K8S_REMOTE_KUBECONFIG:-}"
     echo "FRR_K8S_HOST_KUBECONFIG = ${FRR_K8S_HOST_KUBECONFIG:-}"
     echo "FRR_K8S_REMOTE_NODE_MAP = ${FRR_K8S_REMOTE_NODE_MAP:-}"
     echo "MASTER_LOG_LEVEL = $MASTER_LOG_LEVEL"
     echo "NODE_LOG_LEVEL = $NODE_LOG_LEVEL"
     echo "OVN_LOG_LEVEL_NB = $OVN_LOG_LEVEL_NB"
     echo "OVN_LOG_LEVEL_SB = $OVN_LOG_LEVEL_SB"
     echo "OVN_LOG_LEVEL_NORTHD = $OVN_LOG_LEVEL_NORTHD"
     echo "OVN_LOG_LEVEL_CONTROLLER = $OVN_LOG_LEVEL_CONTROLLER"
     echo ""
}

check_dependencies() {
  check_common_dependencies
  if ! command_exists helm ; then
    echo "'helm' not found, exiting"
    exit 1
  fi
}

# ensure_sysctl_min <key> <required>: raise a sysctl only when its current value is
# below <required>.
ensure_sysctl_min() {
    local key="$1" required="$2" current
    current=$(sysctl -n "$key" 2>/dev/null)
    if [ -z "$current" ] || [ "$current" -lt "$required" ]; then
        sudo sysctl -w "$key=$required"
    fi
}

helm_prereqs() {
    ensure_sysctl_min fs.inotify.max_user_watches 524288
    ensure_sysctl_min fs.inotify.max_user_instances 512
    if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ] ||
       [ "$OVN_UPLINK_BRIDGE" == true ]; then
      disable_bridge_netfilter
    fi
}

helm_extra_values_args() {
    local args=""
    local values_file
    for values_file in "${OVN_HELM_EXTRA_VALUES[@]}"; do
        args+=" -f $(printf '%q' "${values_file}")"
    done
    printf '%s' "${args}"
}

skip_ovn_image_build_load() {
    [ "${DPU_MODE}" != "none" ] && [ "${#OVN_HELM_EXTRA_VALUES[@]}" -gt 0 ] && [ "${OVN_IMAGE}" == "local" ]
}

create_ovn_kubernetes() {
    cd ${DIR}/../helm/ovn-kubernetes
    value_file="values-single-node-zone.yaml"
    if [ "${DPU_MODE}" == "dpu" ]; then
      value_file="values-single-node-zone-dpu.yaml"
    fi
    echo "value_file=${value_file}"
    local extra_values_args helm_network_args helm_image_args helm_log_args
    extra_values_args="$(helm_extra_values_args)"
    # For multi-pod-subnet case, NET_CIDR_IPV4 is a list of CIDRs separated by comma.
    # When Helm encounters a comma within a string value in a --set argument, it attempts to parse the comma as a separator
    # for multiple values (like a list or a map), not as part of a single string value.
    set -x
    ESCAPED_NET_CIDR="${NET_CIDR//,/\\,}"
    ESCAPED_SVC_CIDR="${SVC_CIDR//,/\\,}"
    if [ "${DPU_MODE}" == "dpu" ]; then
      helm_network_args="--set global.mtu=${OVN_MTU}"
      if skip_ovn_image_build_load; then
        helm_image_args=""
      else
        helm_image_args="--set global.dpuImage.repository=${OVN_IMAGE%:*} --set global.dpuImage.tag=${OVN_IMAGE##*:}"
      fi
      helm_log_args="--set ovnkube-single-node-zone-dpu.ovnkubeNodeLogLevel=${NODE_LOG_LEVEL} --set-string ovnkube-single-node-zone-dpu.nbLogLevel=\"${OVN_LOG_LEVEL_NB}\" --set-string ovnkube-single-node-zone-dpu.sbLogLevel=\"${OVN_LOG_LEVEL_SB}\" --set-string ovnkube-single-node-zone-dpu.northdLogLevel=\"${OVN_LOG_LEVEL_NORTHD}\" --set-string ovnkube-single-node-zone-dpu.ovnControllerLogLevel=\"${OVN_LOG_LEVEL_CONTROLLER}\""
    else
      helm_network_args="--set k8sAPIServer=${API_URL} --set podNetwork=\"${ESCAPED_NET_CIDR}\" --set serviceNetwork=\"${ESCAPED_SVC_CIDR}\" --set mtu=${OVN_MTU}"
      if skip_ovn_image_build_load; then
        helm_image_args=""
      else
        helm_image_args="--set global.image.repository=${OVN_IMAGE%:*} --set global.image.tag=${OVN_IMAGE##*:}"
      fi
      helm_log_args="--set ovnkube-control-plane.logLevel=${MASTER_LOG_LEVEL} --set ovnkube-single-node-zone.ovnkubeNodeLogLevel=${NODE_LOG_LEVEL} --set-string ovnkube-single-node-zone.nbLogLevel=\"${OVN_LOG_LEVEL_NB}\" --set-string ovnkube-single-node-zone.sbLogLevel=\"${OVN_LOG_LEVEL_SB}\" --set-string ovnkube-single-node-zone.northdLogLevel=\"${OVN_LOG_LEVEL_NORTHD}\" --set-string ovnkube-single-node-zone.ovnControllerLogLevel=\"${OVN_LOG_LEVEL_CONTROLLER}\""
    fi
    cmd=$(cat <<EOF
helm upgrade --install ovn-kubernetes . -f "${value_file}" ${extra_values_args} \
          ${helm_network_args} \
          ${helm_image_args} \
          --set-string global.v4JoinSubnet="${JOIN_SUBNET_IPV4}" \
          --set-string global.v6JoinSubnet="${JOIN_SUBNET_IPV6}" \
          --set-string global.v4MasqueradeSubnet="${MASQUERADE_SUBNET_IPV4}" \
          --set-string global.v6MasqueradeSubnet="${MASQUERADE_SUBNET_IPV6}" \
          --set-string global.v4TransitSubnet="${TRANSIT_SUBNET_IPV4}" \
          --set-string global.v6TransitSubnet="${TRANSIT_SUBNET_IPV6}" \
          --set global.enableAdminNetworkPolicy=true \
          --set global.enableMultiExternalGateway=true \
          --set global.enableMulticast=$(if [ "${OVN_MULTICAST_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableMultiNetwork=$(if [ "${ENABLE_MULTI_NET}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkSegmentation=$(if [ "${ENABLE_NETWORK_SEGMENTATION}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkConnect=$(if [ "${ENABLE_NETWORK_CONNECT}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableUplink=$(if [ "${ENABLE_UPLINK}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableDynamicUDNAllocation=$(if [ "${DYNAMIC_UDN_ALLOCATION}" == "true" ]; then echo "true"; else echo "false"; fi) \
          $( [ -n "$DYNAMIC_UDN_GRACE_PERIOD" ] && echo "--set global.dynamicUDNGracePeriod=$DYNAMIC_UDN_GRACE_PERIOD" ) \
          --set global.enablePreconfiguredUDNAddresses=$(if [ "${ENABLE_PRE_CONF_UDN_ADDR}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableRouteAdvertisements=$(if [ "${ENABLE_ROUTE_ADVERTISEMENTS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableEVPN=$(if [ "${ENABLE_EVPN}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.advertiseDefaultNetwork=$(if [ "${ADVERTISE_DEFAULT_NETWORK}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.advertisedUDNIsolationMode="${ADVERTISED_UDN_ISOLATION_MODE}" \
          --set global.enableHybridOverlay=$(if [ "${OVN_HYBRID_OVERLAY_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableObservability=$(if [ "${OVN_OBSERV_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.emptyLbEvents=$(if [ "${OVN_EMPTY_LB_EVENTS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableDNSNameResolver=$(if [ "${OVN_ENABLE_DNSNAMERESOLVER}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkQos=$(if [ "${OVN_NETWORK_QOS_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNoOverlay=$(if [ "${ENABLE_NO_OVERLAY}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNoOverlaySnat=$(if [ "${ENABLE_NO_OVERLAY_OUTBOUND_SNAT}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNoOverlayManagedRouting=$(if [ "${ENABLE_NO_OVERLAY_MANAGED_ROUTING}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enablePersistentIPs=true \
          --set global.enableConfigDuration=true \
          --set global.enableCoredumps=$(if [ "${ENABLE_COREDUMPS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.allowICMPNetworkPolicy=$(if [ "${OVN_ALLOW_ICMP_NETPOL}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.gatewayMode="${OVN_GATEWAY_MODE}" \
          $( [ -n "$OVN_GATEWAY_OPTS" ] && echo "--set global.gatewayOpts=\"${OVN_GATEWAY_OPTS}\"" ) \
          --set global.extGatewayNetworkInterface=$(if [ "${OVN_SECOND_BRIDGE}" == "true" ]; then echo "eth1"; else echo ""; fi) \
          --set global.disableSnatMultipleGws=$(if [ "${OVN_DISABLE_SNAT_MULTIPLE_GWS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.disableForwarding=$(if [ "${OVN_DISABLE_FORWARDING}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.unprivilegedMode=false \
          --set global.metricsIp="${METRICS_IP:-}" \
          --set ovs-node.updateStrategy="${OVS_NODE_UPDATE_STRATEGY:-RollingUpdate}" \
          --set global.dummyGatewayBridge=$(if [ "${OVN_DUMMY_GATEWAY_BRIDGE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          $( [ -n "${OVN_ENCAP_PORT}" ] && echo "--set global.encapPort=${OVN_ENCAP_PORT}" ) \
          --set global.disablePacketMtuCheck=$(if [ "${OVN_DISABLE_PKT_MTU_CHECK}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableIpsec=$(if [ "${ENABLE_IPSEC}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set tags.ovn-ipsec=$(if [ "${ENABLE_IPSEC}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableOvnKubeIdentity=$(if [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set tags.ovnkube-identity=$(if [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableMetricsScale=$(if [ "${OVN_METRICS_SCALE_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          $( [ -n "${OVN_EGRESSIP_HEALTHCHECK_PORT}" ] && echo "--set global.egressIpHealthCheckPort=${OVN_EGRESSIP_HEALTHCHECK_PORT}" ) \
          $( [ -n "${OVN_NETFLOW_TARGETS}" ] && echo "--set global.netFlowTargets=${OVN_NETFLOW_TARGETS//,/\\,}" ) \
          $( [ -n "${OVN_SFLOW_TARGETS}" ] && echo "--set global.sflowTargets=${OVN_SFLOW_TARGETS//,/\\,}" ) \
          $( [ -n "${OVN_IPFIX_TARGETS}" ] && echo "--set global.ipfixTargets=${OVN_IPFIX_TARGETS//,/\\,}" ) \
          $( [ -n "${OVN_IPFIX_SAMPLING}" ] && echo "--set global.ipfixSampling=${OVN_IPFIX_SAMPLING}" ) \
          $( [ -n "${OVN_IPFIX_CACHE_MAX_FLOWS}" ] && echo "--set global.ipfixCacheMaxFlows=${OVN_IPFIX_CACHE_MAX_FLOWS}" ) \
          $( [ -n "${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT}" ] && echo "--set global.ipfixCacheActiveTimeout=${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT}" ) \
          $( [ -n "${LIBOVSDB_CLIENT_LOGFILE}" ] && echo "--set global.libovsdbClientLogFile=${LIBOVSDB_CLIENT_LOGFILE}" ) \
          --set hostNetworkNamespace=${OVN_HOST_NETWORK_NAMESPACE} \
          ${helm_log_args}
EOF
       )
    echo "${cmd}"
    eval "${cmd}"
}

install_online_ovn_kubernetes_crds() {
  # NOTE: When you update vendoring versions for the ANP & BANP APIs, we must update the version of the CRD we pull from in the below URL
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
}

check_dependencies
parse_args "$@"
set_default_params
# --delete runs only after all arguments are parsed and defaults are set, so
# that name flags (e.g. --frr-container-name) select which BGP scaffolding to
# tear down.
if [ "${KIND_DELETE:-false}" == true ]; then
  delete
  exit
fi
if [ "${DPU_MODE}" == "dpu" ] && [ "${ENABLE_ROUTE_ADVERTISEMENTS}" == true ]; then
  if [[ -z "${FRR_K8S_REMOTE_KUBECONFIG:-}" || -z "${FRR_K8S_REMOTE_NODE_MAP:-}" ]]; then
    echo "DPU mode with route advertisements requires --frr-k8s-remote-kubeconfig and --frr-k8s-remote-node-map" >&2
    exit 1
  fi
  validate_frr_k8s_remote
fi
print_params
helm_prereqs

# --add-nodes: scale an existing cluster and exit without touching the helm release
if [ "$KIND_ADD_NODES" == true ]; then
  scale_kind_cluster
  if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
    connect_local_registry
  fi
  if [ "$OVN_UPLINK_BRIDGE" == true ]; then
    docker_create_uplink_interface
  fi
  kubectl_wait_pods
  if [ "$OVN_UPLINK_BRIDGE" == true ]; then
    configure_kind_uplink_bridge
  fi
  configure_no_overlay_unmanaged_no_ra_static_routes
  # self-gated on needs_host_pod_subnet_routes
  configure_pod_subnet_routes_on_runner_host
  exit 0
fi

if [ "$KIND_CREATE" == true ]; then
  create_kind_cluster
  if [ "$ENABLE_COREDUMPS" == true ]; then
    setup_coredumps
  fi
  if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
    connect_local_registry
  fi
  docker_disable_ipv6
  if [ "$OVN_SECOND_BRIDGE" == true ]; then
    docker_create_second_interface
  fi
  coredns_patch
  if [ "$OVN_ISOLATED" == true ]; then
    remove_default_route
    add_dns_hostnames
  fi
fi
if [ "$OVN_UPLINK_BRIDGE" == true ]; then
  docker_create_uplink_interface
fi
# when kind-helm.sh is run from inside a container, rewrite the kubeconfig API URL
# to the control-plane container's IP (127.0.0.1 is not reachable across containers).
if [ "$RUN_IN_CONTAINER" == true ]; then
  run_script_in_container
fi
# when using a non-default cluster name created by this script, fix up the
# context/cluster/user names in kubeconfig. In deploy mode the kubeconfig is
# supplied by the caller and should be used as-is.
if [ "$KIND_CREATE" == true ] && [ "$KIND_CLUSTER_NAME" != "ovn" ]; then
  fixup_kubeconfig_names
fi
if skip_ovn_image_build_load; then
  echo "Skipping OVN image build/load; DPU extra values are expected to provide the image"
else
  build_ovn_image
fi
detect_apiserver_url
if ! skip_ovn_image_build_load; then
  install_ovn_image
fi
if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    build_dnsnameresolver_images
    install_dnsnameresolver_images
    install_dnsnameresolver_operator
    update_clusterrole_coredns
    add_ocp_dnsnameresolver_to_coredns_config
    update_coredns_deployment_image
fi
if needs_external_frr_router; then
  deploy_frr_external_container
  deploy_bgp_external_server
fi
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  frr_port=0
  if [ "$ENABLE_NO_OVERLAY_MANAGED_ROUTING" == true ]; then
    # Enable bgp port listening on node, required for managed mode. FRR will listen on port 179 to receive BGP updates from other nodes.
    frr_port=179
  fi
  if [ "${DPU_MODE}" == "host" ]; then
    install_frr_k8s_crds
  else
    install_frr_k8s $frr_port
  fi
fi
if [ "$KIND_REMOVE_TAINT" == true ]; then
  remove_no_schedule_taint
fi
create_ovn_kubernetes

# --deploy: helm sees no spec diff (same OVN_IMAGE tag), so refresh pods manually.
if [ "$KIND_CREATE" == false ] && [ "$KIND_LOCAL_REGISTRY" == false ]; then
  refresh_ovn_pods
fi

install_online_ovn_kubernetes_crds
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  install_ingress
fi

if [ "$ENABLE_MULTI_NET" == true ]; then
  enable_multi_net
fi

# if ! kubectl wait -n ovn-kubernetes --for=condition=ready pods --all --timeout=300s ; then
#  echo "some pods in the system are not running"
#  kubectl get pods -A -o wide || true
#  kubectl describe po -A
#  exit 1
# fi

kubectl_wait_pods
if [ "$OVN_UPLINK_BRIDGE" == true ]; then
  configure_kind_uplink_bridge
fi

if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    kubectl_wait_dnsnameresolver_pods
fi
sleep_until_pods_settle

if [ "$KIND_INSTALL_METALLB" == true ]; then
  install_metallb
fi
if [ "$KIND_INSTALL_PLUGINS" == true ]; then
  install_plugins
fi
if [ "$KIND_INSTALL_KUBEVIRT" == true ]; then
  install_kubevirt
  install_cert_manager
  if [ "$KIND_OPT_OUT_KUBEVIRT_IPAM" != true ]; then
    install_kubevirt_ipam_controller
  fi
fi

if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ] && [ "${DPU_MODE}" != "host" ]; then
  # wait for frr-k8s to be ready
  wait_for_frr_k8s
  if [ "$ENABLE_NO_OVERLAY_MANAGED_ROUTING" != true ]; then
    configure_frr_k8s
    configure_frr_uplink_peers
  fi
fi
configure_no_overlay_unmanaged_no_ra_static_routes

restart_dpu_sim_system_deployments_after_ovnk

# IPsec pods need the signer-ca ConfigMap and signed CSRs before they can roll out.
# The ovn-ipsec DaemonSet is created by the helm chart (tags.ovn-ipsec=true), install_ipsec
# handles the CA creation and CSR signing (manifest apply is skipped when helm owns the DS).
if [ "$ENABLE_IPSEC" == true ]; then
  set_openssl_binary
  install_ipsec
fi
