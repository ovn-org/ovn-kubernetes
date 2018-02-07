#!/bin/bash

umask 022
shopt -s nullglob
set -xe

source "$(dirname "${BASH_SOURCE[0]}")/common-api.bash"

BIN_DIR=/opt/ovn-go-kube
BIN_NAME=ovn-k8s-cni-overlay
CONF_NAME=10-ovn-kubernetes.conf
CONF_TEMPLATE="{\"name\":\"ovn-kubernetes\", \"type\":\"$BIN_NAME\"}"

HOST_BIN_DIR=/host/opt/cni/bin
HOST_CONF_DIR=/host/etc/cni/net.d

#remove old files
rm -rf $OVN_V/$BIN_NAME $HOST_BIN_DIR/$BIN_NAME $HOST_CONF_DIR/$CONF_NAME

# Assuming the CA cert is already in host's CA bundle

#install cni binary
TB="$(mktemp -p "$HOST_BIN_DIR" "${BIN_NAME}-XXXXXX")"
cp -a "$BIN_DIR/$BIN_NAME" "$TB"
#Using mv for atomic operation
mv "$TB" "$HOST_BIN_DIR/$BIN_NAME"

#install cni config
TF="$(mktemp -p "$HOST_CONF_DIR" "${CONF_NAME}-XXXXXX")"
echo "$CONF_TEMPLATE" | jq . > "$TF"
#Using mv for atomic operation
mv "$TF" "$HOST_CONF_DIR/$CONF_NAME"

trap "{ rm -rf $HOST_BIN_DIR/$BIN_NAME* $HOST_CONF_DIR/$CONF_NAME* ; }" EXIT
tail -f /dev/null

exit 1
