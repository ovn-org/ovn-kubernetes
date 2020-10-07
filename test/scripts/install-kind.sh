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

# Install kind (dual-stack is not released upstream so we have to use our own version)
if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  sudo curl -Lo /usr/local/bin/kind https://github.com/aojea/kind/releases/download/dualstack/kind
  sudo chmod +x /usr/local/bin/kind
else
  go get sigs.k8s.io/kind@v0.9.0
fi

pushd ../contrib
./kind.sh
popd
