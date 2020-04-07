#!/usr/bin/env bash
GO111MODULE=on ${GOPATH}/bin/golangci-lint run \
    --skip-dirs=pkg/crd/egressfirewall/v1/apis/ \
    --timeout=15m0s --verbose --print-resources-usage --modules-download-mode=vendor \
    && echo "lint OK!"
