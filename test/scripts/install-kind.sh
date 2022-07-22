#!/usr/bin/env bash

set -ex

# from https://github.com/kubernetes-sigs/kind/releases
KIND_URL=https://kind.sigs.k8s.io/dl/v0.14.0/kind-linux-amd64
KIND_SHA=af5e8331f2165feab52ec2ae07c427c7b66f4ad044d09f253004a20252524c8b
KIND_DOWNLOAD_RETRIES=5

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
TMP_DIR="$(mktemp -d)"

install_kind() {
	set +e

	local download_successful=false

	for retry in $(seq 1 ${KIND_DOWNLOAD_RETRIES}); do
		curl -Lo ./kind ${KIND_URL}
		echo "${KIND_SHA} kind" | sha256sum --check
		if [ $? -eq 0 ]; then
			download_successful=true
			break
		else
			echo "Check sum '${KIND_SHA}' does not match file ./kind"
		fi

		sleep 5
	done

	if ! ${download_successful}; then
		echo "Could not download kind binary"
		exit 1
	fi

	chmod +x ./kind
	sudo mv ./kind /usr/local/bin/
	set -e
}

pushd $TMP_DIR
K8S_VERSION="v1.24.0"

# Install kubectl for K8S_VERSION in use
# (to get latest stable version: $(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt )
curl -LO https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
curl -L https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/kubernetes-test-linux-amd64.tar.gz -o kubernetes-test-linux-amd64.tar.gz
tar xvzf kubernetes-test-linux-amd64.tar.gz
sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-amd64.tar.gz

install_kind
popd # go out of $TMP_DIR

pushd $SCRIPT_DIR/../../contrib
./kind.sh
popd # go our of $SCRIPT_DIR/../../contrib
