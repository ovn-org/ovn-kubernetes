#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env
set -x

pushd ${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}

if [ -z "$PKGS" ]; then
  # by default, test everything that's not in vendor
  PKGS="$(go list ./... | grep -v vendor | xargs echo)"
fi

bash -c "umask 0; PATH=${GOROOT}/bin:$(pwd)/bin:${PATH} CGO_ENABLED=0 go test "${goflags[@]:+${goflags[@]}}" ${PKGS}"

popd
