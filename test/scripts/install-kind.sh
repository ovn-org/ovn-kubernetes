#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -ex
ARCH=""
case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64"   ;;
esac

# from https://github.com/kubernetes-sigs/kind/releases
KIND_URL=https://kind.sigs.k8s.io/dl/v0.32.0/kind-linux-${ARCH}
DOWNLOAD_RETRIES=5

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
TMP_DIR="$(mktemp -d)"

# download_verified URL SHA_URL DEST
# Downloads URL to DEST and verifies it against the sha256 published at
# SHA_URL, retrying on transport errors or checksum mismatch. Downloads are
# never streamed straight into tar: a truncated response from the CDN must
# fail here, with a clear message, rather than as a gzip error mid-extract.
download_verified() {
	local url=$1 sha_url=$2 dest=$3 sha
	# Fail a transfer that stalls below 10 KiB/s for a minute instead of
	# hanging the step, so the retry loop actually gets a chance to run.
	local curl_opts=(-fsSL --connect-timeout 30 --speed-limit 10240 --speed-time 60)

	for retry in $(seq 1 ${DOWNLOAD_RETRIES}); do
		sha="$(curl "${curl_opts[@]}" "${sha_url}" | awk '{ print $1 }')"
		if ! [[ "${sha}" =~ ^[0-9a-f]{64}$ ]]; then
			echo "Could not fetch a sha256 from ${sha_url} (attempt ${retry}/${DOWNLOAD_RETRIES})"
		elif ! curl "${curl_opts[@]}" -o "${dest}" "${url}"; then
			echo "Download of ${url} failed (attempt ${retry}/${DOWNLOAD_RETRIES})"
		elif echo "${sha} ${dest}" | sha256sum --check; then
			return 0
		else
			echo "Checksum mismatch for ${url} (attempt ${retry}/${DOWNLOAD_RETRIES})"
		fi
		rm -f "${dest}"
		sleep 5
	done

	echo "Could not download ${url}"
	exit 1
}

install_kind() {
	download_verified "${KIND_URL}" "${KIND_URL}.sha256sum" ./kind
	chmod +x ./kind
	sudo mv ./kind /usr/local/bin/
}

pushd $TMP_DIR
K8S_VERSION="v1.36.2"

# Install kubectl for K8S_VERSION in use
K8S_CLIENT_URL=https://dl.k8s.io/${K8S_VERSION}/kubernetes-client-linux-${ARCH}.tar.gz
download_verified "${K8S_CLIENT_URL}" "${K8S_CLIENT_URL}.sha256" kubernetes-client-linux-${ARCH}.tar.gz
sudo tar xvzf kubernetes-client-linux-${ARCH}.tar.gz -C /usr/local/bin kubernetes/client/bin/kubectl --strip-components 3
sudo chmod +x /usr/local/bin/kubectl
rm kubernetes-client-linux-${ARCH}.tar.gz

# Install e2e test binary and ginkgo
K8S_TEST_URL=https://dl.k8s.io/${K8S_VERSION}/kubernetes-test-linux-${ARCH}.tar.gz
download_verified "${K8S_TEST_URL}" "${K8S_TEST_URL}.sha256" kubernetes-test-linux-${ARCH}.tar.gz
tar xvzf kubernetes-test-linux-${ARCH}.tar.gz
sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
rm kubernetes-test-linux-${ARCH}.tar.gz

HELM_VERSION="v3.17.2"
# to get latest stable version: https://github.com/helm/helm/releases
HELM_URL=https://get.helm.sh/helm-${HELM_VERSION}-linux-${ARCH}.tar.gz
download_verified "${HELM_URL}" "${HELM_URL}.sha256sum" helm-${HELM_VERSION}-linux-${ARCH}.tar.gz
tar xvzf helm-${HELM_VERSION}-linux-${ARCH}.tar.gz
chmod +x ./linux-${ARCH}/helm
sudo mv linux-${ARCH}/helm /usr/local/bin/
rm helm-${HELM_VERSION}-linux-${ARCH}.tar.gz

install_kind
popd # go out of $TMP_DIR

# Build the Kubernetes node image until official kindest/node images are
# available.
# See: https://github.com/kubernetes-sigs/kind/issues/4157
echo "Building kind node image for Kubernetes ${K8S_VERSION}..."
kind build node-image --image kindest/node:${K8S_VERSION} ${K8S_VERSION}

pushd $SCRIPT_DIR/../../contrib
./kind-helm.sh
popd # go our of $SCRIPT_DIR/../../contrib
