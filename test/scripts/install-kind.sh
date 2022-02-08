#!/usr/bin/env bash

set -ex

KIND_URL=https://kind.sigs.k8s.io/dl/v0.11.1/kind-linux-amd64
KIND_SHA=949f81b3c30ca03a3d4effdecda04f100fa3edc07a28b19400f72ede7c5f0491
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
# Install latest stable kubectl
curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
curl -L https://storage.googleapis.com/kubernetes-release/release/v1.23.0/kubernetes-test-linux-amd64.tar.gz -o kubernetes-test-linux-amd64.tar.gz
tar xvzf kubernetes-test-linux-amd64.tar.gz
sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-amd64.tar.gz

install_kind
popd # go out of $TMP_DIR

pushd $SCRIPT_DIR/../../contrib
./kind.sh
popd # go our of $SCRIPT_DIR/../../contrib
