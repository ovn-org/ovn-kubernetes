#!/usr/bin/env bash

source .golangci.env

if [ "$#" -ne 1 ]; then
    echo "Expected command line argument - container runtime (docker/podman) got $# arguments: $@"
    exit 1
fi

mkdir -p ~/.cache/golangci-lint/$GOLANGCI_LINT_VERSION

$1 run --security-opt label=disable --rm -v $(pwd):/app -v ~/.cache/golangci-lint/$GOLANGCI_LINT_VERSION:/root/.cache -w /app -e GO111MODULE=on golangci/golangci-lint:${GOLANGCI_LINT_VERSION} \
	golangci-lint run --verbose --print-resources-usage \
	--modules-download-mode=vendor --timeout=15m0s && \
	echo "lint OK!"
