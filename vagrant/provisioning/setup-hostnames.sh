#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# ARGS:
# $1: Master IP
# $2: Master hostname
# $3: Minion1 IP
# $4: Minion1 hostname
# $5: Minion2 IP
# $6: Minion2 hostname

MASTER_IP=$1
MASTER_HOSTNAME=$2
MINION1_IP=$3
MINION1_HOSTNAME=$4
MINION2_IP=$5
MINION2_HOSTNAME=$6

cat << HOSTEOF >> /etc/hosts
$MASTER_IP $MASTER_HOSTNAME
$MINION1_IP $MINION1_HOSTNAME
$MINION2_IP $MINION2_HOSTNAME
HOSTEOF

# Restore xtrace
$XTRACE
