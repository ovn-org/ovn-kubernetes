#!/usr/bin/env bash

GO111MODULE=on ${GOPATH}/bin/golangci-lint run \
    --tests=false --enable gofmt \
    --timeout=5m0s \
    && echo "lint OK!"
