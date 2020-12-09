#!/usr/bin/env bash

set -ex

# Install latest stable kubectl
curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install latest e2e test binary
# The e2e binaries are built from https://github.com/aojea/kubernetes-e2e-binaries
# Current e2e binary versions is kuberntes 1.20 with fix for IPv6 network plicies #96856
curl -LO https://github.com/aojea/kubernetes-e2e-binaries/releases/download/a20aeb8e/e2e.test
chmod +x ./e2e.test
sudo mv ./e2e.test /usr/local/bin/e2e.test

# Install ginkgo
go get github.com/onsi/ginkgo/ginkgo
go get github.com/onsi/gomega/...

# Install kind (dual-stack is not released upstream so we have to use our own version)
if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  sudo curl -Lo /usr/local/bin/kind https://github.com/aojea/kind/releases/download/dualstack/kind
  sudo chmod +x /usr/local/bin/kind
else
  curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.9.0/kind-linux-amd64
  chmod +x ./kind
  sudo mv ./kind /usr/local/bin/
fi

pushd ../contrib
./kind.sh
popd
