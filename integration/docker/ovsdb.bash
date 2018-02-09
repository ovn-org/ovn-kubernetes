#!/bin/bash

set -xe

source "$(dirname "${BASH_SOURCE[0]}")/ovs-common.inc"

mkdir -p /etc/openvswitch
DB=/etc/openvswitch/conf.db

[ -f $DB ] || ovsdb-tool create $DB

exec ovsdb-server $DB -vconsole:info "--remote=punix:$DBSOCK"
