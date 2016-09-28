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

import sys
from ovn_k8s.common.util import ovs_vsctl
from ovn_k8s.common.util import ovn_nbctl
from ovn_k8s.common import variables


def ovn_init_overlay():
    OVN_NB = ovs_vsctl("--if-exists", "get", "Open_vSwitch", ".",
                       "external_ids:ovn-nb").strip('"')
    if not OVN_NB:
        sys.exit("OVN central database's ip address not set")
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

    K8S_NS_LB_TCP = ovn_nbctl("--data=bare", "--no-heading",
                              "--columns=_uuid", "find", "load_balancer",
                              "external_ids:k8s-ns-lb-tcp=yes")
    variables.K8S_NS_LB_TCP = K8S_NS_LB_TCP

    K8S_NS_LB_UDP = ovn_nbctl("--data=bare", "--no-heading",
                              "--columns=_uuid", "find", "load_balancer",
                              "external_ids:k8s-ns-lb-udp=yes")
    variables.K8S_NS_LB_UDP = K8S_NS_LB_UDP

    variables.OVN_MODE = "overlay"
