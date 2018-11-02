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

# Work around sudo's PATH handling since Travis puts in Go in the travis user's homedir
GO=`which go`
sudo -E bash -c "umask 0; CGO_ENABLED=0 ${GO} test "${goflags[@]:+${goflags[@]}}" ${PKGS}"

popd
