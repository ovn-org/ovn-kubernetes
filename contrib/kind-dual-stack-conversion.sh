#!/usr/bin/env bash

convert_kubelet() {
 echo "Converting kubelet on nodes $NODES"
 for n in $NODES; do
   docker exec $n cp /var/lib/kubelet/config.yaml /var/lib/kubelet/config.yaml.bak
   # kubelet already has a featureGate configured so we need to ammend a new one
   # NOTE: if there weren't feature gates configured we have to change this
   docker exec $n sed -i '/featureGates/a\  IPv6DualStack: true' /var/lib/kubelet/config.yaml
   # Restart kubelet
   docker exec $n systemctl restart kubelet
  echo "Converted kubelet on node $n"
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
  # as workaround we delete the master directly
  kubectl -n ovn-kubernetes delete pod -l name=ovnkube-master
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
    # OVN has already the SCTP feature enable, we just need to append the new one
    # NOTE: if there was not feature-gate enabled we must use    
    # docker exec $n sed -i '/service-cluster-ip-range/i\    - --feature-gates=IPv6DualStack=true' ${API_CONFIG_FILE}
    docker exec $n sed -i -e "s#.*feature-gates.*#&\,IPv6DualStack=true#" ${API_CONFIG_FILE}
    # kube-controller-manager /etc/kubernetes/manifests/kube-controller-manager.yaml
    docker exec $n cp ${CM_CONFIG_FILE} /kind/kube-controller-manager.bak
    # append second service cidr --service-cluster-ip-range=<IPv4 CIDR>,<IPv6 CIDR>
    docker exec $n sed -i -e "s#.*service-cluster-ip-range.*#&\,${SECONDARY_SERVICE_SUBNET}#" ${CM_CONFIG_FILE}
    # append second cluster cidr --cluster-cidr=<IPv4 CIDR>,<IPv6 CIDR>
    docker exec $n sed -i -e "s#.*cluster-cidr.*#&\,${SECONDARY_CLUSTER_SUBNET}#" ${CM_CONFIG_FILE}
    # OVN has already the SCTP feature enable, we just need to append the new one
    # NOTE: if there was not feature-gate enabled we must use    
    # docker exec $n sed -i '/service-cluster-ip-range/i\    - --feature-gates=IPv6DualStack=true' ${CM_CONFIG_FILE}
    docker exec $n sed -i -e "s#.*feature-gates.*#&\,IPv6DualStack=true#" ${CM_CONFIG_FILE}
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
export KUBECONFIG=${HOME}/admin.conf

# KIND nodes
NODES=$(kind get nodes --name ${CLUSTER_NAME})
CONTROL_PLANE_NODES=$(kind get nodes --name ${CLUSTER_NAME} | grep control)
WORKER_NODES=$(kind get nodes --name ${CLUSTER_NAME} | grep worker)

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
        image: k8s.gcr.io/e2e-test-images/agnhost:2.21
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

if ! kubectl wait --for=condition=ready pods --all --timeout=100s ; then
  echo "deployment is not running"
  kubectl get pods -o wide -A || true
  exit 1
fi

# Start the conversion to dual stack
# TODO revisit order
convert_kubelet
# FIXME: this is a random value to give time to pods to restart
sleep 10
convert_k8s_control_plane
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
