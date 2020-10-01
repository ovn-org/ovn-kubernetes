#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf

SKIPPED_TESTS=""
if [ "$KIND_IPV6_SUPPORT" == true ]; then
    FOCUS_TESTS="multicast"
    # No support for these features in dual-stack or IPv6 only yet
    SKIPPED_TESTS="hybrid.overlay|external.gateway"
fi

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
export NUM_NODES=2

pushd e2e

go mod download
go test -timeout=0 -v . \
        -ginkgo.v \
        -ginkgo.flakeAttempts ${FLAKE_ATTEMPTS:-2} \
        -ginkgo.focus="${FOCUS_TESTS}" \
        -ginkgo.skip="${SKIPPED_TESTS}" \
        -provider skeleton \
        -kubeconfig ${KUBECONFIG} \
        ${CONTAINER_RUNTIME:+"--container-runtime=${CONTAINER_RUNTIME}"} \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"} \
        ${E2E_REPORT_PREFIX:+"--report-prefix=${E2E_REPORT_PREFIX}"}
popd
