#!/usr/bin/env bash

VERSION=v1.52
if [ "$#" -ne 1 ]; then
    echo "Expected command line argument - container runtime (docker/podman) got $# arguments: $@"
    exit 1
fi

$1 run --security-opt label=disable --rm -v $(pwd):/app -w /app -e GO111MODULE=on golangci/golangci-lint:${VERSION} \
	golangci-lint run --verbose --print-resources-usage \
	--modules-download-mode=vendor --timeout=15m0s && \
	echo "lint OK!"
