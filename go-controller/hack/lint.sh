#!/usr/bin/env bash

GO111MODULE=on golangci-lint run \
    --tests=false --enable gofmt \
    --timeout=5m0s \
    && echo "lint OK!"
