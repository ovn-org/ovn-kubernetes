#!/bin/bash
#set -euo pipefail

# Stops IPsec daemons.
cleanup-ovn-ipsec() {
  # Stop ovn-monitor-ipsec
  /usr/share/openvswitch/scripts/ovs-ctl stop-ovs-ipsec
  # Stop libreswan
  /usr/libexec/ipsec/whack --shutdown
  /sbin/ip xfrm policy flush
  /sbin/ip xfrm state flush
  /usr/sbin/ipsec --stopnflog
  echo "=============== ovs-ipsec terminated ========== "
  exit 0
}

# v3 - Runs IPsec daemons.
ovn-ipsec() {
  check_ovn_daemonset_version "3"

  trap cleanup-ovn-ipsec SIGTERM

  echo "=============== ovn-ipsec ====================="

  # Workaround for https://github.com/libreswan/libreswan/issues/373
  ulimit -n 1024

  /usr/libexec/ipsec/addconn --config /etc/ipsec.conf --checkconfig
  # Check for kernel modules
  /usr/libexec/ipsec/_stackmanager start
  # Check for nss database status and migration
  /usr/sbin/ipsec --checknss
  # Check for nflog setup
  /usr/sbin/ipsec --checknflog
  # Start the actual IKE daemon
  /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf --logfile /var/log/libreswan.log

  # Workarounf for https://mail.openvswitch.org/pipermail/ovs-dev/2020-October/375734.html
  OVS_LOGDIR=/var/log/openvswitch OVS_RUNDIR=/var/run/openvswitch OVS_PKGDATADIR=/usr/share/openvswitch /usr/share/openvswitch/scripts/ovs-ctl --ike-daemon=libreswan start-ovs-ipsec

  # Wait for things to settle
  sleep 10

  # Only the control-plane node will be able to set this but we only need one
  # node to set it and if it fails on the others, we do not care.
  ovn-nbctl set nb_global . ipsec=true

  while true; do
    sleep 10
  done

  echo "=============== ovs-ipsec terminated ========== "
}
