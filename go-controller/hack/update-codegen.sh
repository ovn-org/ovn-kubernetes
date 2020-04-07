#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

if  ! ( command -v controller-gen > /dev/null ); then
  # Need to take an unreleased version of controller-tools
  # because of controller-tools bug 302
  echo "controller-gen not found, installing sigs.k8s.io/controller-tools@83f6193..."
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  GO111MODULE=on go get -u sigs.k8s.io/controller-tools/cmd/controller-gen@83f6193
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

echo "Generating deepcopy funcs"
deepcopy-gen \
  --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1 \
  -O zz_generated.deepcopy \
  --bounding-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd


echo "Generating clientset"
client-gen \
  --clientset-name "${CLIENTSET_NAME_VERSIONED:-versioned}" \
  --input-base "" \
  --input github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1 \
  --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset \
  "$@"

echo "Generating listers"
lister-gen \
  --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1 \
  --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers \
  "$@"

echo "Generating informers"
informer-gen \
  --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1 \
  --versioned-clientset-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned \
  --listers-package  github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers \
  --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers \
  "$@"

echo "Generating CRDs"
mkdir -p _output/crds
controller-gen crd paths=./pkg/crd/... output:crd:dir=_output/crds
