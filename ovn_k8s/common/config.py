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

import ast
import ConfigParser
import os
import sys

import ovn_k8s
from ovn_k8s.common.util import ovs_vsctl
from ovn_k8s.common.util import ovn_nbctl
from ovn_k8s.common import variables


raw_config_parser = ConfigParser.RawConfigParser()
config_path = "/etc/openvswitch"
config_file = '%s/ovn_k8s.conf' % config_path
config_read = raw_config_parser.read(config_file)
if len(config_read) != 1:
    # raw_config_parser.read returns the list of filenames which were
    # successfully parsed.
    # If we reach here it means that it could not read the config file from
    # "/etc/openvswitch".  Search in default prefix '/usr/local', this is
    # where pip is installing the config file
    config_file = '/usr/local%s' % config_file
    config_read = raw_config_parser.read(config_file)
    if len(config_read) != 1:
        # If we reach here it means the config could not be found or read
        # from the previous locations.  This could happen if we try to run
        # it from a virtual env.  In this case, we try to read the config
        # from the current repository
        absolute_path = os.path.dirname(os.path.abspath(ovn_k8s.__file__))
        absolute_path = absolute_path[:absolute_path.rfind("/")]
        config_file = "%s/etc/ovn_k8s.conf" % absolute_path
        config_read = raw_config_parser.read(config_file)
        if len(config_read) != 1:
            # Config could not be found or read
            sys.exit("Error when reading config file: %s" % config_file)


def get_option(option_name, section_name='default'):
    try:
        config_string = raw_config_parser.get(section_name, option_name)
    except Exception:
        # Return None in case the option_name could not be found
        return None
    try:
        # Try to evaluate the string which may contain a Python expression
        expr = ast.literal_eval(config_string)
        return expr
    except Exception:
        return config_string


UNIX_SOCKET = get_option('unix_socket')


def ovn_init_overlay():
    if os.path.exists(UNIX_SOCKET):
        OVN_NB = "unix:%s" % UNIX_SOCKET
    else:
        sys.exit("OVN NB database does not have unix socket")
    variables.OVN_NB = OVN_NB

    K8S_API_SERVER = ovs_vsctl("--if-exists", "get", "Open_vSwitch", ".",
                               "external_ids:k8s-api-server").strip('"')
    if not K8S_API_SERVER:
        sys.exit("K8S_API_SERVER not set")
    if not K8S_API_SERVER.startswith("http"):
        variables.K8S_API_SERVER = "http://%s" % K8S_API_SERVER
    else:
        variables.K8S_API_SERVER = K8S_API_SERVER

    K8S_CLUSTER_ROUTER = ovn_nbctl("--data=bare", "--no-heading",
                                   "--columns=_uuid", "find", "logical_router",
                                   "external_ids:k8s-cluster-router=yes")
    if not K8S_CLUSTER_ROUTER:
        sys.exit("K8S_CLUSTER_ROUTER not set")
    variables.K8S_CLUSTER_ROUTER = K8S_CLUSTER_ROUTER

    K8S_CLUSTER_LB_TCP = ovn_nbctl("--data=bare", "--no-heading",
                                   "--columns=_uuid", "find", "load_balancer",
                                   "external_ids:k8s-cluster-lb-tcp=yes")
    if not K8S_CLUSTER_LB_TCP:
        sys.exit("K8S_CLUSTER_LB_TCP not set")
    variables.K8S_CLUSTER_LB_TCP = K8S_CLUSTER_LB_TCP

    K8S_CLUSTER_LB_UDP = ovn_nbctl("--data=bare", "--no-heading",
                                   "--columns=_uuid", "find", "load_balancer",
                                   "external_ids:k8s-cluster-lb-udp=yes")
    if not K8S_CLUSTER_LB_UDP:
        sys.exit("K8S_CLUSTER_LB_UDP not set")
    variables.K8S_CLUSTER_LB_UDP = K8S_CLUSTER_LB_UDP
