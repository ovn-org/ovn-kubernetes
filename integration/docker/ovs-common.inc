#!/bin/bash

source "$(dirname "${BASH_SOURCE[0]}")/common-api.bash"

DBSOCK=/var/run/openvswitch/db.sock

get_nbsb_kube_nodes() {
  #convert to [{name: , port: }..] array
  kube_api_get /api/v1/nodes | jq ".items | map(select(.metadata.labels.\"ovnkube/ovn${1}\" != null)) | map( { name : .metadata.name, port : .metadata.labels.\"ovnkube/ovn${1}\" })"
}

get_nbsb_kube_remote() {
  local R
  R="$(get_nbsb_kube_nodes "$1")"
  [ "$( echo "$R" | jq '. | length')" == 1 ] || return 1
  local N
  local P
  N="$(echo "$R" | jq -er '.[0].name')"
  P="$(echo "$R" | jq -er '.[0].port')"
  local A
  A="$(get_node_internal_ip "$N")"
  echo "$A:$P"
}

add_nbsb_kube_label() {
  kube_api_put_node "$(get_self_name)" "{\"metadata\":{\"labels\":{\"ovnkube/ovn${1}\":\"$(get_nbsb_remote_port "$1")\"}}}" | jq ".metadata.labels"
}

del_nbsb_kube_label() {
  kube_api_put_node "$(get_self_name)" "{\"metadata\":{\"labels\":{\"ovnkube/ovn${1}\":null}}}" | jq ".metadata.labels"
}
