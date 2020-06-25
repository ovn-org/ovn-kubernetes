#!/usr/bin/env bash

set -ex

pushd e2e
go mod download
popd

export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf
export MASTER_NAME=${KIND_CLUSTER_NAME}-control-plane
export NODE_NAMES=${MASTER_NAME}
export KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS}
export E2E_REPORT_DIR=${E2E_REPORT_DIR}
export E2E_REPORT_PREFIX=${E2E_REPORT_PREFIX}

sed -E -i 's/"\$\{ginkgo\}" "\$\{ginkgo_args\[\@\]\:\+\$\{ginkgo_args\[\@\]\}\}" "\$\{e2e_test\}"/pushd \$GITHUB_WORKSPACE\/test\/e2e\nGO111MODULE=on "\$\{ginkgo\}" "\$\{ginkgo_args\[\@\]\:\+\$\{ginkgo_args\[\@\]\}\}"/' ${GOPATH}/src/k8s.io/kubernetes/hack/ginkgo-e2e.sh

pushd ${GOPATH}/src/k8s.io/kubernetes
hack/ginkgo-e2e.sh --disable-log-dump=false
popd
