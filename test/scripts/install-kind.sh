#!/usr/bin/env bash

set -ex

export GO111MODULE="on"
mkdir -p $GOPATH/bin
curl -fs https://chunk.io/trozet/ba750701d0af4e2b94b249ab9de27b50 -o $GOPATH/bin/kubetest
chmod +x $GOPATH/bin/kubetest

pushd $GOPATH/src/k8s.io/kubernetes/
sudo ln ./_output/local/go/bin/kubectl /usr/local/bin/kubectl
popd

GO111MODULE="on" go get sigs.k8s.io/kind@v0.7.0
pushd ../contrib
./kind.sh
popd
