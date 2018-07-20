#!/bin/bash
# set -x

# Setup for Openshift to support the ovn-cni plugin
#
# Create the project, service account and policies.
# ovnkube interacts with kubernetes and the environment
# must be properly set up.
# 
# This script runs on the ovn-master node on the cluster
# since it sets up a configmap with information on how
# to access the ovn-master.
#
# This script is run as part of installation after the cluster is
# up and before the ovn daemonsets are created.

health=$(kubectl get --raw /healthz/ready)
if [ $health != 'ok' ]
then
  echo " cluster not ready"
  exit 1
fi

# make sure ovn-kubernetes project exists
cat << EOF | kubectl create -f - > /dev/null 2>&1
apiVersion: project.openshift.io/v1
kind: Project
metadata:
  annotations:
    openshift.io/description: ""
    openshift.io/display-name: ""
    openshift.io/node-selector: ""
    openshift.io/sa.scc.mcs: s0:c8,c2
    openshift.io/sa.scc.supplemental-groups: 1000060000/10000
  name: ovn-kubernetes
spec:
  finalizers:
  - openshift.io/origin
  - kubernetes
EOF
echo "kind: Project ovn-kubernetes -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
kind: ServiceAccount
apiVersion: v1
metadata:
  name: ovn
  namespace: ovn-kubernetes
EOF
echo "kind: ServiceAccount ovn-kubernetes -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
apiVersion: authorization.openshift.io/v1
kind: ClusterRole
metadata:
  annotations:
    authorization.openshift.io/system-only: "true"
    openshift.io/reconcile-protect: "false"
  name: system:ovn-reader
rules:
- apiGroups:
  - ""
  - network.openshift.io
  attributeRestrictions: null
  resources:
  - egressnetworkpolicies
  - hostsubnets
  - netnamespaces
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - ""
  attributeRestrictions: null
  resources:
  - namespaces
  - nodes
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - extensions
  attributeRestrictions: null
  resources:
  - networkpolicies
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - networking.k8s.io
  attributeRestrictions: null
  resources:
  - networkpolicies
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - ""
  - network.openshift.io
  attributeRestrictions: null
  resources:
  - clusternetworks
  verbs:
  - get
- apiGroups:
  - ""
  attributeRestrictions: null
  resources:
  - events
  verbs:
  - create
  - patch
  - update
EOF
echo "kind: ClusterRole system:ovn-reader -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
kind: ClusterRoleBinding
apiVersion: authorization.openshift.io/v1
metadata:
  name: ovn-cluster-reader
roleRef:
  name: cluster-reader
subjects:
- kind: ServiceAccount
  name: ovn
  namespace: ovn-kubernetes
userNames:
- system:serviceaccount:ovn-kubernetes:ovn
groupNames: []
EOF
echo "kind: ClusterRoleBinding ovn-cluster-reader -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
kind: ClusterRoleBinding
apiVersion: authorization.openshift.io/v1
metadata:
  name: ovn-reader
roleRef:
  name: system:ovn-reader
subjects:
- kind: ServiceAccount
  name: ovn
  namespace: ovn-kubernetes
userNames:
- system:serviceaccount:ovn-kubernetes:ovn
groupNames: []
EOF
echo "kind: ClusterRoleBinding ovn-reader -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
# oc adm policy add-cluster-role-to-user cluster-admin -z ovn -o yaml
apiVersion: authorization.openshift.io/v1
kind: ClusterRoleBinding
metadata:
  name: cluster-admin-0
roleRef:
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: ovn
  namespace: ovn-kubernetes
userNames:
- system:serviceaccount:ovn-kubernetes:ovn
groupNames: []
EOF
echo "kind: ClusterRoleBinding cluster-admin-0 -- $?"

cat << EOF | kubectl create -f - > /dev/null 2>&1
# kubectl adm policy add-scc-to-user anyuid -z ovn
apiVersion: v1
allowHostDirVolumePlugin: false
allowHostIPC: false
allowHostNetwork: false
allowHostPID: false
allowHostPorts: false
allowPrivilegedContainer: false
allowedCapabilities: null
allowedFlexVolumes: null
defaultAddCapabilities: null
fsGroup:
  type: RunAsAny
groups:
- system:cluster-admins
kind: SecurityContextConstraints
metadata:
  annotations:
    kubernetes.io/description: anyuid provides all features of the restricted SCC but allows users to run with any UID and any GID.
  name: anyuid
priority: 10
readOnlyRootFilesystem: false
requiredDropCapabilities:
- MKNOD
runAsUser:
  type: RunAsAny
seLinuxContext:
  type: MustRunAs
supplementalGroups:
  type: RunAsAny
users:
- system:serviceaccount:default:ovn
- system:serviceaccount:openshift-sdn:ovn
- system:serviceaccount:openshift-infra:ovn
- system:serviceaccount:ovn-kubernetes:ovn
volumes:
- configMap
- downwardAPI
- emptyDir
- persistentVolumeClaim
- projected
- secret
EOF
echo "kind: SecurityContextConstraints anyuid -- $?"
# kubectl create serviceaccount ovn
# kubectl adm policy add-cluster-role-to-user cluster-admin -z ovn


# get api server
api=$(kubectl get po -n kube-system -o wide | gawk '/master-api/{ print $7 }')
apiserver=https://${api}:8443

# get the network cidr (master-config.yaml clusterNetworkCIDR)
net_cidr=10.128.0.0/14

# get the service subnet cidr (master-config.yaml servicesSubnet)
svc_cidr=172.30.0.0/16

# get the ovn master's ip address
host_ip=$(host $(hostname) | gawk '{ print $4 }' )

# Currently using tcp, may move to ssl
OvnNorth="tcp://${host_ip}:6641"
OvnSouth="tcp://${host_ip}:6642"

# build the ovn.master file
echo apiserver $apiserver
echo net_cidr $net_cidr
echo svc_cidr $svc_cidr
echo OvnNorth $OvnNorth
echo OvnSouth $OvnSouth

# if the config map exists, delete it.
kubectl get configmap ovn-config > /dev/null 2>&1
if [[ $? = 0 ]]
then
  kubectl delete configmap ovn-config
fi

# create the ovn-config configmap
cat << EOF | kubectl create -f - > /dev/null 2>&1
kind: ConfigMap
apiVersion: v1
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
data:
  k8s_apiserver: $apiserver
  net_cidr:      $net_cidr
  svc_cidr:      $svc_cidr
  OvnNorth:      $OvnNorth
  OvnSouth:      $OvnSouth
EOF
echo "kind: ConfigMap ovn-config -- $?"

# debug="-o yaml"
echo ""
echo "project ovn-kubernetes"
kubectl get project ovn-kubernetes ${debug}
echo ""
echo "ServiceAccount ovn -n ovn-kubernetes"
kubectl get sa ovn -n ovn-kubernetes ${debug}
echo ""
echo "kind: ClusterRole system:ovn-reader -- $?"
kubectl get ClusterRole system:ovn-reader ${debug}
echo ""
echo "ClusterRoleBinding ovn-cluster-reader"
kubectl get ClusterRoleBinding ovn-cluster-reader ${debug}
echo ""
echo "ClusterRoleBinding ovn-reader"
kubectl get ClusterRoleBinding ovn-reader ${debug}
echo ""
echo "ClusterRoleBinding cluster-admin-0"
kubectl get ClusterRoleBinding cluster-admin-0 ${debug}
echo ""
echo "SecurityContextConstraints anyuid"
kubectl get SecurityContextConstraints anyuid ${debug}
echo ""
echo "ConfigMap ovn-config -n ovn-kubernetes"
kubectl get ConfigMap ovn-config -n ovn-kubernetes ${debug}

exit 0
