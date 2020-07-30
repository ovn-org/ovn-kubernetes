#!/usr/bin/env bash

set -ex

pushd e2e
go mod download
popd

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf
export MASTER_NAME=${KIND_CLUSTER_NAME}-control-plane
export NODE_NAMES=${MASTER_NAME}
export KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS}
export E2E_REPORT_DIR=${E2E_REPORT_DIR}
export E2E_REPORT_PREFIX=${E2E_REPORT_PREFIX}

sed -E -i 's/"\$\{ginkgo\}" "\$\{ginkgo_args\[\@\]\:\+\$\{ginkgo_args\[\@\]\}\}" "\$\{e2e_test\}"/pushd \$GITHUB_WORKSPACE\/test\/e2e\nGO111MODULE=on "\$\{ginkgo\}" "\$\{ginkgo_args\[\@\]\:\+\$\{ginkgo_args\[\@\]\}\}"/' ${GOPATH}/src/k8s.io/kubernetes/hack/ginkgo-e2e.sh

pushd ${GOPATH}/src/k8s.io/kubernetes
# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
# FIXME we should not tolerate flakes
# but until then, we retry the test in the same job
# to stop PR retriggers for totally broken code
export GINKGO_TOLERATE_FLAKES='y'
export FLAKE_ATTEMPTS=2
NUM_NODES=2
./hack/ginkgo-e2e.sh \
'--provider=skeleton' "--num-nodes=${NUM_NODES}" \
"--report-dir=${E2E_REPORT_DIR}" '--disable-log-dump=true'
popd
