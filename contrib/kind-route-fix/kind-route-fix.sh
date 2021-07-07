#!/bin/bash

while true; do
  if [ "$(ip route | awk '/default/ {print $NF}')" == "eth0" ] && $(ip link ls dev eth0 | grep -q "master ovs-system") ; then
    echo "Conditions met for route issue."
    ovs_container_id=$(crictl ps | awk '/ovs-daemons/ {print $1}')
    if [ "$ovs_container_id" != "" ]; then
      echo "Rewiring network from eth0 to breth0."
      crictl exec ${ovs_container_id} ovn-kube-util nics-to-bridge eth0
    else
      echo "Could not find ovs-daemons container."
    fi
  fi
  sleep 30
done
