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

if [[ -n "$(go env GOBIN)" ]]; then
  INSTALL_PATH=$(go env GOBIN)
else
  mkdir -p $GOPATH/bin
  INSTALL_PATH=$GOPATH/bin
fi

curl -Lo $INSTALL_PATH/kind https://github.com/kubernetes-sigs/kind/releases/download/v0.8.1/kind-linux-amd64
chmod +x $INSTALL_PATH/kind
pushd ../contrib
./kind.sh
popd
