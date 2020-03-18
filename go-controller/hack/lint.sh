#!/usr/bin/env bash

GO111MODULE=on ${GOPATH}/bin/golangci-lint run \
    --tests=false --enable gofmt \
    --timeout=8m0s \
    && echo "lint OK!"
