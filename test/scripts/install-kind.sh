#!/usr/bin/env bash

set -ex

# Install latest stable kubectl
curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
# The e2e binaries are published upstream so the ecosystem can consume then directly
# xref https://github.com/kubernetes/enhancements/blob/master/keps/sig-testing/20190118-breaking-apart-the-kubernetes-test-tarball.md
# Current e2e binary version for CI is kubernetes 1.20 with the fix for IPv6 network policies #96856
# We can always get the latest version published from the URL dl.k8s.io/ci/latest.txt
# curl -L dl.k8s.io/ci/latest.txt
# v1.21.0-alpha.0.341+46d481b4556e33
curl -LO https://dl.k8s.io/ci/v1.21.0-alpha.0.341+46d481b4556e33/kubernetes-test-linux-amd64.tar.gz
tar xvzf kubernetes-test-linux-amd64.tar.gz --strip-components=3 kubernetes/test/bin/ginkgo kubernetes/test/bin/e2e.test
sudo mv ./e2e.test /usr/local/bin/e2e.test
sudo mv ./ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-amd64.tar.gz

# Install kind (dual-stack is not released upstream so we have to use our own version)
if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
  sudo curl -Lo /usr/local/bin/kind https://github.com/aojea/kind/releases/download/dualstack/kind
  sudo chmod +x /usr/local/bin/kind
else
  curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.10.0/kind-linux-amd64
  chmod +x ./kind
  sudo mv ./kind /usr/local/bin/
fi

pushd ../contrib
./kind.sh
popd
