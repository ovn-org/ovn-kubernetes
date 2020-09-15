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

go get sigs.k8s.io/kind@c58694155106d0ac6e9612b7af5d0ac4f0559ba3
pushd ../contrib
./kind.sh
popd
