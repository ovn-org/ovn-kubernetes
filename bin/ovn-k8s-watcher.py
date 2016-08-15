#! /usr/bin/python
# Copyright (C) 2016 Nicira, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at:
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import eventlet
import sys

import ovs.dirs
import ovs.util
import ovs.daemon
import ovs.vlog
from ovn_k8s.common import config
from ovn_k8s.watcher import watcher

eventlet.monkey_patch()

vlog = ovs.vlog.Vlog("ovn-k8s-watcher")


def ovn_init_k8s(args):
    if args.overlay:
        config.ovn_init_overlay()
    elif args.underlay:
        sys.exit("Underlay mode not implemented")
    elif args.openstack:
        sys.exit("OpenStack mode not implemented")

    if not args.overlay and not args.underlay and not args.openstack:
        sys.exit("Atleast one of --overlay, --underlay or --openstack needed")


def main():
    parser = argparse.ArgumentParser()

    # For the same k8s setup, networking in OVN can happen in
    # different ways.
    parser.add_argument('--overlay', action='store_true')
    parser.add_argument('--underlay', action='store_true')
    parser.add_argument('--openstack', action='store_true')

    # Handle OVS logging and daemonization arguments.
    ovs.vlog.add_args(parser)
    ovs.daemon.add_args(parser)
    args = parser.parse_args()
    ovs.vlog.handle_args(args)
    ovs.daemon.handle_args(args)

    ovn_init_k8s(args)

    ovs.daemon.daemonize()

    watcher.start_threads()


if __name__ == '__main__':
    main()
