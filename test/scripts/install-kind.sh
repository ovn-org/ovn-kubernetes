#!/usr/bin/env bash

set -ex

# Install latest stable kubectl
curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
curl -L https://github.com/trozet/ovnFiles/blob/master/kubernetes-test-linux-v1.21.0-alpha.0.341%2B46d481b4556e33.tar.gz?raw=true -o kubernetes-test-linux-amd64.tar.gz
tar xvzf kubernetes-test-linux-amd64.tar.gz
sudo mv ./e2e.test /usr/local/bin/e2e.test
sudo mv ./ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-amd64.tar.gz

curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
chmod +x ./kind
sudo mv ./kind /usr/local/bin/

pushd ../contrib
./kind.sh
popd
