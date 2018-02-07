#!/bin/bash

set -xe

source "$(dirname "${BASH_SOURCE[0]}")/ovn-central-common.inc"

exec_db sb
