#!/usr/bin/env bash

set -ex

K8S_VERSION=${K8S_VERSION:-v1.21.1}
KIND_VERSION=${KIND_VERSION:-v0.11.0}

# Install latest stable kubectl
curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
curl -LO "https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/kubernetes-test-linux-amd64.tar.gz"
tar xvzf kubernetes-test-linux-amd64.tar.gz --strip-components=3 kubernetes/test/bin/ginkgo kubernetes/test/bin/e2e.test
sudo mv ./e2e.test /usr/local/bin/e2e.test
sudo mv ./ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-amd64.tar.gz

# Install kind (dual-stack is not released upstream so we have to use our own version)
curl -Lo ./kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
chmod +x ./kind
sudo mv ./kind /usr/local/bin/

pushd ../contrib
./kind.sh
popd
