#!/usr/bin/env bash

# pin golangci-lint version to 1.33.2
VERSION=v1.33.2

docker run --rm -v $(pwd):/app -w /app -e GO111MODULE=on golangci/golangci-lint:${VERSION} \
	golangci-lint run --verbose --print-resources-usage \
	--modules-download-mode=vendor --timeout=15m0s && \
	echo "lint OK!"
