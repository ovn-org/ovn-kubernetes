#!/usr/bin/env bash

# check docker
docker info >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    >&2 echo "docker not found"
    exit 1
fi

# check kubectl
kubectl cluster-info >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    >&2 echo "kubectl not found"
    exit 1
fi

# check kind
kind version >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    >&2 echo "kind not found"
    exit 1
fi

# check go
go version >/dev/null 2>&1
if [ "$?" -eq 127 ]; then
    >&2 echo "go not found"
    exit 1
fi

# kubectl_wait_pods will set a total timeout of 300s for IPv4 and 480s for IPv6. It will first wait for all
# DaemonSets to complete with kubectl rollout. This command will block until all pods of the DS are actually up.
# Next, it iterates over all pods in ovn-kubernetes namespace and waits for them to post "Ready".
# Last, it will do the same with all pods in the kube-system namespace.
kubectl_wait_pods() {
  local OVN_TIMEOUT=300

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + OVN_TIMEOUT ))

  dss=$(kubectl -n ovn-kubernetes get daemonset --no-headers --sort-by=.metadata.name -o jsonpath='{.items[*].metadata.name}')
  for ds in ${dss}; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to launch all ${ds} pods (timeout ${timeout})..."
    kubectl rollout status daemonset -n ovn-kubernetes ${ds} --timeout ${timeout}s
  done

  pods=$(kubectl -n ovn-kubernetes get pod --no-headers --sort-by=.metadata.name -o jsonpath='{.items[*].metadata.name}')
  for pod in ${pods}; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to create pod ${pod} (timeout ${timeout})..."
    if ! kubectl wait pod ${pod} -n ovn-kubernetes --for condition=Ready --timeout=${timeout}s ; then
        >&2 echo "pod $pod in the ovn-kubernetes namespace is not ready after time out"
        kubectl get pods -A -o wide || true
        exit 1
    fi
  done

  timeout=$(calculate_timeout ${endtime})
  if ! kubectl wait -n kube-system --for=condition=ready pods --all --timeout=${timeout}s ; then
    >&2 echo "some pods in the system are not running"
    kubectl get pods -A -o wide || true
    exit 1
  fi
}

# calculate_timeout takes an absolute endtime in seconds (based on bash script runtime, see
# variable $SECONDS) and calculates a relative timeout value. Should the calculated timeout
# be <= 0, return one second.
calculate_timeout() {
  endtime=$1
  timeout=$(( endtime - SECONDS ))
  if [ ${timeout} -le 0 ]; then
      timeout=1
  fi
  echo ${timeout}
}

# Ensure that kind network does not have ip6 enabled
enabled_ipv6=$(docker network inspect kind -f '{{.EnableIPv6}}' 2>/dev/null)
[ "${enabled_ipv6}" = "false" ] || {
   docker network rm kind 2>/dev/null || :
   docker network create kind -o "com.docker.network.bridge.enable_ip_masquerade"="true" -o "com.docker.network.driver.mtu"="1500"
}
[ "${enabled_ipv6}" = "false" ] || { 2&>1 echo the kind network is not what we expected ; exit 1; }

set -euxo pipefail
# increate fs.inotify.max_user_watches
sudo sysctl fs.inotify.max_user_watches=524288
# increase fs.inotify.max_user_instances
sudo sysctl fs.inotify.max_user_instances=512
# build image
cd ../dist/images
make ubuntu
docker tag ovn-kube-u:latest ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:master
# create a cluster, with 1 controller and 1 worker node
kind_cluster_name=ovn-helm
cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOT
kind delete clusters $kind_cluster_name
kind create cluster --name $kind_cluster_name --config /tmp/kind.yaml
kind load docker-image --name $kind_cluster_name ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:master
cd ../../helm/ovn-kubernetes
helm install ovn-kubernetes . -f values.yaml \
    --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
    --set ovnkube-identity.replicas=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l) \
    --set global.image.repository=ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u --set global.image.tag=master

set +x
kubectl_wait_pods
