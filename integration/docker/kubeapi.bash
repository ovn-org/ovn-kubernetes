#!/bin/bash

KUBE_SECRETS_DIR=/var/run/secrets/kubernetes.io/serviceaccount

export OVN_K8S_API_SERVER="${OVN_K8S_API_SERVER:-"$(\
python -c "import yaml
with open('/var/lib/kubelet/kubelet.yaml' ,'r') as f:
  tree = yaml.load(f)
print tree['clusters'][0]['cluster']['server']")"}"

get_ca_cert_path(){
  [ -a "$KUBE_SECRETS_DIR/ca.crt" ] || return 1
  echo "$KUBE_SECRETS_DIR/ca.crt"
}

get_token(){
  set -e
  cat "$KUBE_SECRETS_DIR/token"
}

get_api_server(){
  echo "$OVN_K8S_API_SERVER"
}

kube_api_get() {
  curl -sS --cacert $KUBE_SECRETS_DIR/ca.crt -H "Authorization: Bearer $(cat $KUBE_SECRETS_DIR/token)" "${OVN_K8S_API_SERVER}${1}"
}

kube_api_put() {
  curl -sS --cacert $KUBE_SECRETS_DIR/ca.crt -X PATCH -H "Content-Type: application/merge-patch+json" -H "Authorization: Bearer $(cat $KUBE_SECRETS_DIR/token)" -d "$2" "${OVN_K8S_API_SERVER}${1}"
}

kube_api_get_node() {
  kube_api_get "/api/v1/nodes/$1"
}

kube_api_put_node() {
  kube_api_put "/api/v1/nodes/$1" "$2"
}
