#!/bin/bash

shopt -s nullglob
set -xe

cd "$(dirname "$0")"
G="$(git rev-parse --show-toplevel)"
K="$G/integration/docker"

pushd "$G/go-controller"
make
popd

tar Cczf "$G/go-controller/_output/go/bin" "$K/ovn-go.tar.gz" .

cd "$K"
docker build -t ganboing/ovnkube:latest .
