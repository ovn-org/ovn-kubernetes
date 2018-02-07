#!/bin/bash

set -xe

source "$(dirname "${BASH_SOURCE[0]}")/ovn-central-common.inc"

exec ovn-northd -vconsole:info "--ovnnb-db=unix:$(get_nbsb_sock nb)" "--ovnsb-db=unix:$(get_nbsb_sock sb)"
