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
import os
import sys

from ovn_k8s.common.util import ovs_vsctl
from ovn_k8s.common.util import ovn_nbctl
from ovn_k8s.common import variables

if sys.version_info < (3, 0):
    # python 2
    import ConfigParser
else:
    # python 3
    import configparser as ConfigParser


raw_config_parser = ConfigParser.RawConfigParser()


def init_config():
    config_filename = 'ovn_k8s.conf'
    if sys.platform != 'win32':
        config_path = "/etc/openvswitch"
    else:
        config_path = "C:\\etc"
    config_file = '%s/%s' % (config_path, config_filename)
    # Convert the path to use platform specific path delimitators
    config_file = os.path.abspath(config_file)
    # Return True if the file could be read, otherwise False
    return len(raw_config_parser.read(config_file)) == 1


config_successfully_read = init_config()


def get_default_value(option_name, section_name):
    section_default = {
        "mtu": 1400,
        "conntrack_zone": 64000,
        "ovn_mode": "overlay",
        "log_path": "/var/log/openvswitch/ovn-k8s-cni-overlay.log",
        "unix_socket": "/var/run/openvswitch/ovnnb_db.sock",
        "cni_conf_path": "/etc/cni/net.d",
        "cni_link_path": "/opt/cni/bin/",
        "cni_plugin": "ovn-k8s-cni-overlay",
        "private_key": "/etc/openvswitch/ovncontroller-privkey.pem",
        "certificate": "/etc/openvswitch/ovncontroller-cert.pem",
        "ca_cert": "/etc/openvswitch/ovnnb-ca.cert",
        "k8s_ca_certificate": "/etc/openvswitch/k8s-ca.crt",
        "rundir": "",
        "logdir": ""
    }
    default_config = {'default': section_default}

    section_dict = default_config.get(section_name, {})
    return section_dict.get(option_name, None)


def get_option(option_name, section_name='default'):
    if not config_successfully_read:
        return get_default_value(option_name, section_name)

    try:
        config_string = raw_config_parser.get(section_name, option_name)
    except Exception:
        # Config value could not be found in the file, retrieve the
        # default value
        config_string = get_default_value(option_name, section_name)

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
