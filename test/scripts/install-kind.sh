#!/usr/bin/env bash

set -ex

export GO111MODULE="on"

pushd $GOPATH/src/k8s.io/kubernetes/
sudo ln ./_output/local/go/bin/kubectl /usr/local/bin/kubectl
sudo ln ./_output/local/go/bin/e2e.test /usr/local/bin/e2e.test
popd

mkdir -p $GOPATH/bin
wget -O $GOPATH/bin/kind https://github.com/kubernetes-sigs/kind/releases/download/v0.8.1/kind-linux-amd64
chmod +x $GOPATH/bin/kind
pushd ../contrib
./kind.sh
popd
