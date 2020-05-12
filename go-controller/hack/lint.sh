#!/usr/bin/env bash
GO111MODULE=on ${GOPATH}/bin/golangci-lint run \
    --timeout=15m0s --verbose --print-resources-usage --modules-download-mode=vendor \
    && echo "lint OK!"
