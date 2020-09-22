#!/usr/bin/env bash

set -ex

export GO111MODULE="on"

pushd $GOPATH/src/k8s.io/kubernetes/
if [[ ! -f /usr/local/bin/kubectl ]]; then
  sudo ln ./_output/local/go/bin/kubectl /usr/local/bin/kubectl
fi
if [[ ! -f /usr/local/bin/e2e.test ]]; then
  sudo ln ./_output/local/go/bin/e2e.test /usr/local/bin/e2e.test
fi
popd

go get sigs.k8s.io/kind@v0.9.0
pushd ../contrib
./kind.sh
popd
