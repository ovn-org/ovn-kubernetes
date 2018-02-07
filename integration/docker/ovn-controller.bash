#!/bin/bash

set -xe

source "$(dirname "${BASH_SOURCE[0]}")/ovs-common.inc"

id_file=/etc/openvswitch/system-id.conf

test -s $id_file || get_self_system_uuid > $id_file

ovs-vsctl set Open_vSwitch . external_ids:system-id=$(cat $id_file)

exec ovn-controller "unix:$DBSOCK" -vconsole:info
