OVNNB_SOCK=/var/run/openvswitch/ovnnb_db.sock

source "$(dirname "${BASH_SOURCE[0]}")/common-api.bash"
source "$(dirname "${BASH_SOURCE[0]}")/ovs-common.inc"

mkdir -p /var/lib/openvswitch >/dev/null 2>&1

get_nbsb_db() {
  echo "/var/lib/openvswitch/ovn${1}_db.db"
}

get_nbsb_sock() {
  echo "/var/run/openvswitch/ovn${1}_db.sock"
}

get_nbsb_unixctl() {
  echo "/var/run/openvswitch/ovn${1}_db.ctl"
}

get_nbsb_schema() {
  echo "/usr/share/openvswitch/ovn-${1}.ovsschema"
}

get_nbsb_remote_port() {
  case "$1" in
    "nb")
      echo "6641"
      ;;
    "sb")
      echo "6642"
      ;;
  esac
}

exec_db() {
  [ -f "$(get_nbsb_db "$1" )" ] || ovsdb-tool create "$(get_nbsb_db "$1")" "$(get_nbsb_schema "$1")"
  add_nbsb_kube_label "$1"
  get_nbsb_kube_remote "$1"
  local R
  trap "{ del_nbsb_kube_label "$1"; }" EXIT
  ovsdb-server -vconsole:info \
"--remote=punix:$(get_nbsb_sock "$1")" \
"--remote=ptcp:$(get_nbsb_remote_port "$1")" \
"--unixctl=$(get_nbsb_unixctl "$1")" \
"$(get_nbsb_db "$1")" || R=$?
  del_nbsb_kube_label "$1" || :
  return $R
}
