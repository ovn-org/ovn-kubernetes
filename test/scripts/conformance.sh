#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd

pushd conformance
go mod download
mkdir -p ${E2E_REPORT_DIR}
go test -timeout=0 -v -kubeconfig ${KUBECONFIG} 2>&1 | go-junit-report | tee ${E2E_REPORT_DIR}/junit-conformance-${JOB_NAME}-${GITHUB_RUN_ID}.xml
popd