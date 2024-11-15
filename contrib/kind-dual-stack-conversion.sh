#!/usr/bin/env bash

restart_kubelet() {
 echo "Restarting kubelet on nodes $NODES"
 for n in $NODES; do
   # Set --node-ip extra-args to be passed to kubelet. Needed for the e2e tests
   ipv4=$(docker exec $n cat /kind/old-ipv4)
   echo ${ipv4}
   ipv6=$(docker exec $n cat /kind/old-ipv6)
   echo ${ipv4}
   docker exec $n sed -i -e "s/${ipv4}/${ipv4},${ipv6}/g" /var/lib/kubelet/kubeadm-flags.env
   # Restart kubelet
   docker exec $n systemctl restart kubelet
  echo "Restarted kubelet on node $n"
 done
}

convert_cni() {
  # from kind hack/ci/e2e-k8s.sh
  # Modify current OVN config
  ORIGINAL_OVNCONFIG=$(kubectl get -oyaml -n=ovn-kubernetes configmap/ovn-config)
  echo "Original OVN config:"
  echo "${ORIGINAL_OVNCONFIG}"
  # Patch it
  FIXED_OVNCONFIG=$(
    printf '%s' "${ORIGINAL_OVNCONFIG}" | sed \
      -e "s#^  net_cidr.*#&\,${SECONDARY_CLUSTER_SUBNET}#" \
      -e "s#^  svc_cidr:.*#&\,${SECONDARY_SERVICE_SUBNET}#" \
  )
  echo "Patched OVN config:"
  echo "${FIXED_OVNCONFIG}"
  printf '%s' "${FIXED_OVNCONFIG}" | kubectl apply -f -
  # restart ovnkube-master
  # FIXME: kubectl rollout restart deployment leaves the old pod hanging 
  # as workaround we delete the master directly. When deployed with
  # OVN_INTERCONNECT_ENABLE=true, the db and cm pods need that too.
  # Depending on how kind was deployed, the pods have different labels.
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-db ||:
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-zone-controller ||:
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-master ||:
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-control-plane ||:
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-identity ||:

  # restart ovnkube-node
  kubectl -n ovn-kubernetes rollout restart daemonset ovnkube-node
  kubectl -n ovn-kubernetes rollout status daemonset ovnkube-node
  echo "Updated CNI"
}

convert_k8s_control_plane(){
  # Kubeadm installs apiserver and controller-manager as static pods
  # one the configuration has changed kubelet restart them
  API_CONFIG_FILE="/etc/kubernetes/manifests/kube-apiserver.yaml"
  CM_CONFIG_FILE="/etc/kubernetes/manifests/kube-controller-manager.yaml"

  # update all the control plane nodes
  for n in $CONTROL_PLANE_NODES; do
    echo "Converting control-plane on node $n"
    # kube-apiserver backup config file
    docker exec $n cp ${API_CONFIG_FILE} /kind/kube-apiserver.yaml.bak
    # append second service cidr --service-cluster-ip-range=<IPv4 CIDR>,<IPv6 CIDR>
    docker exec $n sed -i -e "s#.*service-cluster-ip-range.*#&\,${SECONDARY_SERVICE_SUBNET}#" ${API_CONFIG_FILE}
    # kube-controller-manager /etc/kubernetes/manifests/kube-controller-manager.yaml
    docker exec $n cp ${CM_CONFIG_FILE} /kind/kube-controller-manager.bak
    # append second service cidr --service-cluster-ip-range=<IPv4 CIDR>,<IPv6 CIDR>
    docker exec $n sed -i -e "s#.*service-cluster-ip-range.*#&\,${SECONDARY_SERVICE_SUBNET}#" ${CM_CONFIG_FILE}
    # append second cluster cidr --cluster-cidr=<IPv4 CIDR>,<IPv6 CIDR>
    docker exec $n sed -i -e "s#.*cluster-cidr.*#&\,${SECONDARY_CLUSTER_SUBNET}#" ${CM_CONFIG_FILE}
    echo "Finished converting control plane on node $n"
  done
}

usage()
{
    echo "usage: kind_dual_conversion.sh [-n|--name <cluster_name>] [-ss secondary_service_subnet] [-sc secondary_cluster_subnet]"
    echo "Convert a single stack cluster in a dual stack cluster"
}

parse_args()
{
    while [ "${1:-}" != "" ]; do
        case $1 in
            -n | --name )			                              shift
                                          	                CLUSTER_NAME=$1
                                          	;;
            -ss | --secondary-service-subnet )	           	shift
                                          	                SECONDARY_SERVICE_SUBNET=$1
                                          	;;
            -sc | --secondary-cluster-subnet )	           	shift
                                          	                SECONDARY_CLUSTER_SUBNET=$1
                                          	                ;;
            -h | --help )                       usage
                                                exit
                                                ;;
            * )                                 usage
                                                exit 1
        esac
        shift
    done
}

parse_args $*

set -euxo pipefail

# Set default values
CLUSTER_NAME=${CLUSTER_NAME:-ovn}
DUALSTACK_FEATURE_GATE="IPv6DualStack=true"
SECONDARY_SERVICE_SUBNET=${SECONDARY_SERVICE_SUBNET:-"fd00:10:96::/112"}
SECONDARY_CLUSTER_SUBNET=${SECONDARY_CLUSTER_SUBNET:-"fd00:10:244::/56"}

# NOTE: ovn only
export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}

# KIND nodes
NODES=$(kind get nodes --name ${CLUSTER_NAME})
CONTROL_PLANE_NODES=$(kind get nodes --name ${CLUSTER_NAME} | grep control)
WORKER_NODES=$(kind get nodes --name ${CLUSTER_NAME} | grep worker)

# Warm up images into KIND
for IMG in registry.k8s.io/e2e-test-images/agnhost:2.21 httpd:2.4 ; do \
  docker pull $IMG
  kind load docker-image $IMG --name ovn
done

# Create a deployment with 2 pods
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: server-deployment
  labels:
    app: MyApp
spec:
  replicas: 2
  selector:
    matchLabels:
      app: MyApp
  template:
    metadata:
      labels:
        app: MyApp
    spec:
      containers:
      - name: agnhost
        image: registry.k8s.io/e2e-test-images/agnhost:2.21
        args:
          - netexec
          - --http-port=80
          - --udp-port=80
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: nodeport-service
spec:
  type: NodePort
  selector:
    app: MyApp
  ports:
    - protocol: TCP
      port: 80
      targetPort: 80
EOF

if ! kubectl rollout status deployment server-deployment -w --timeout=100s ; then
  echo "deployment is not running"
  kubectl get pods -o wide -A || true
  exit 1
fi

# Start the conversion to dual stack
# TODO revisit order
convert_k8s_control_plane
# FIXME: Fix this random sleep value by adding a wait_till_apiserver_isready() function
sleep 60
# We need to manually restart kubelet for the changes to take effect since apiserver and kube-controller
# are created as static pods. See https://github.com/kubernetes/kubernetes/pull/107900 for details
restart_kubelet
# FIXME: this is a random value to give time to pods to restart
sleep 10
convert_cni
# FIXME: this is a random value to give time to pods to restart
sleep 10

# Check everything is fine
if ! kubectl wait --for=condition=ready pods --all --timeout=100s ; then
  echo "deployment is not running"
  kubectl get pods -o wide -A || true
  exit 1
fi

# Create dual stack services and check they work
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: httpd-deployment
  labels:
    app: MyDualApp
spec:
  replicas: 2
  selector:
    matchLabels:
      app: MyDualApp
  template:
    metadata:
      labels:
        app: MyDualApp
    spec:
      containers:
      - name: httpd
        image: httpd:2.4
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: my-service-v4
spec:
  selector:
    app: MyDualApp
  ipFamilies:
  - IPv4
  ipFamilyPolicy: SingleStack
  ports:
    - protocol: TCP
      port: 8081
      targetPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: my-service-v6
spec:
  selector:
    app: MyDualApp
  ipFamilies:
  - IPv6
  ipFamilyPolicy: SingleStack
  ports:
    - protocol: TCP
      port: 8080
      targetPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: my-service-require-dual
spec:
  selector:
    app: MyDualApp
  ipFamilyPolicy: RequireDualStack
  ports:
    - protocol: TCP
      port: 8080
      targetPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: my-service-prefer-dual
spec:
  selector:
    app: MyDualApp
  ipFamilyPolicy: PreferDualStack
  ports:
    - protocol: TCP
      port: 8080
      targetPort: 80
EOF

# Restart pods so they can acquire dual stack addresses
kubectl delete pods -l app=MyApp
kubectl delete pods -l app=MyDualApp

# Check everything is fine
if ! kubectl wait --for=condition=ready pods --all --timeout=100s ; then
  echo "deployments are not running"
  kubectl get pods -o wide -A || true
  exit 1
fi

# Check that services are reachable on both IP Families
IPS=()
CLUSTER_IPV4=$(kubectl get services my-service-v4 -o jsonpath='{.spec.clusterIPs[0]}')
IPS+=("${CLUSTER_IPV4}:8081")
CLUSTER_IPV6=$(kubectl get services my-service-v6 -o jsonpath='{.spec.clusterIPs[0]}')
IPS+=("\[${CLUSTER_IPV6}\]:8080")

for n in $NODES; do
  for ip in ${IPS[@]}; do
    docker exec $n curl --connect-timeout 5 $ip
  done
done

# Give some time for everything to settle
sleep 60
