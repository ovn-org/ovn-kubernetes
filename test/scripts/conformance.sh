#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/ovn.conf

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd

pushd conformance
GO111MODULE=on go get -u github.com/jstemmer/go-junit-report
go mod download
GOFLAGS="-tags=conformance_tests" \
        go test -timeout=0 -v \
        -kubeconfig ${KUBECONFIG} \
        go-junit-report > ${E2E_REPORT_DIR}/${E2E_REPORT_PREFIX}-network-policy-conformance-report.xml
popd