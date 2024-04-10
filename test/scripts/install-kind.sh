#!/usr/bin/env bash

set -ex
ARCH=""
case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64"   ;;
esac

# from https://github.com/kubernetes-sigs/kind/releases
KIND_URL=https://kind.sigs.k8s.io/dl/v0.20.0/kind-linux-${ARCH}
KIND_SHA_URL=$KIND_URL.sha256sum
KIND_SHA="$( curl -L -s ${KIND_SHA_URL}| awk '{ print $1 }')"
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
K8S_VERSION="v1.28.0"

# Install kubectl for K8S_VERSION in use
# (to get latest stable version: $(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt )
curl -LO https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/bin/linux/${ARCH}/kubectl
chmod +x ./kubectl
sudo mv ./kubectl /usr/local/bin/kubectl

# Install e2e test binary and ginkgo
curl -L https://storage.googleapis.com/kubernetes-release/release/${K8S_VERSION}/kubernetes-test-linux-${ARCH}.tar.gz -o kubernetes-test-linux-${ARCH}.tar.gz
tar xvzf kubernetes-test-linux-${ARCH}.tar.gz
sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-${ARCH}.tar.gz

if [ "$USE_HELM" == true ]; then
    HELM_VERSION="v3.14.2"
	# to get latest stable version: https://github.com/helm/helm/releases
    curl -L  https://get.helm.sh/helm-${HELM_VERSION}-linux-${ARCH}.tar.gz -o helm-linux-${ARCH}.tar.gz
	tar xvzf helm-linux-${ARCH}.tar.gz
    chmod +x ./linux-${ARCH}/helm
	sudo mv linux-${ARCH}/helm /usr/local/bin/
	rm helm-linux-${ARCH}.tar.gz
fi

install_kind
popd # go out of $TMP_DIR

pushd $SCRIPT_DIR/../../contrib
if [ "$USE_HELM" == true ]; then
    ./kind-helm.sh
else
    ./kind.sh
fi

if [ "$KIND_INSTALL_KUBEVIRT" == true ]; then
    sudo mv ./bin/virtctl /usr/local/bin/virtctl
fi
popd # go our of $SCRIPT_DIR/../../contrib

