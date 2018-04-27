#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

go get -u gopkg.in/alecthomas/gometalinter.v1; \
  ${GOPATH}/bin/gometalinter.v1 --install;

