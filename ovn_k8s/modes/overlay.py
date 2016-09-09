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
import time

import ovs.vlog
from ovn_k8s.common import exceptions
import ovn_k8s.common.kubernetes as kubernetes
import ovn_k8s.common.variables as variables
from ovn_k8s.common.util import ovn_nbctl

vlog = ovs.vlog.Vlog("overlay")


class OvnNB(object):
    def __init__(self):
        self.service_cache = {}
        self.logical_switch_cache = {}
        self.gateway_ip_cache = {}
        self.physical_gateway_ips = []

    def _get_physical_gateway_ips(self):
        if self.physical_gateway_ips:
            return self.physical_gateway_ips

        try:
            physical_gateway_ip_networks = ovn_nbctl(
                                "--data=bare", "--no-heading",
                                "--columns=network", "find",
                                "logical_router_port",
                                "external_ids:gateway-physical-ip=yes").split()
        except Exception as e:
            vlog.err("_populate_gateway_ip: find failed %s" % (str(e)))

        for physical_gateway_ip_network in physical_gateway_ip_networks:
            (ip, mask) = physical_gateway_ip_network.split('/')
            self.physical_gateway_ips.append(ip)

        return self.physical_gateway_ips

    def _update_service_cache(self, event_type, cache_key, service_data):
        # Remove item from cache if it was deleted.
        if event_type == 'DELETED':
            del self.service_cache[cache_key]
        else:
            # Update cache
            self.service_cache[cache_key] = service_data

    def _create_load_balancer_vip(self, service_type, service_ip, ips, port,
                                  target_port, protocol):
        vlog.dbg("received event to create/modify load_balancer vip with "
                 "service_type=%s, service_ip=%s, ips=%s, port=%s,"
                 "target_port=%s, protocol=%s"
                 % (service_type, service_ip, ips, port, target_port,
                    protocol))
        if not port or not target_port or not protocol or not service_type:
            return

        load_balancer = ""
        if protocol == "TCP" and service_type == "ClusterIP":
            load_balancer = variables.K8S_CLUSTER_LB_TCP

        if protocol == "UDP" and service_type == "ClusterIP":
            load_balancer = variables.K8S_CLUSTER_LB_UDP

        if protocol == "TCP" and service_type == "NodePort":
            load_balancer = variables.K8S_NS_LB_TCP

        if protocol == "UDP" and service_type == "NodePort":
            load_balancer = variables.K8S_NS_LB_UDP

        if not load_balancer:
            return

        # key is of the form "IP:port" (with quotes around)
        key = "\"" + service_ip + ":" + str(port) + "\""
        if not ips:
            try:
                ovn_nbctl("remove", "load_balancer", load_balancer,
                          "vips", key)
            except Exception as e:
                vlog.err("_create_load_balancer_vip remove: (%s)" % (str(e)))
            return

        # target is of the form "IP1:port, IP2:port, IP3:port"
        target = ""
        for ip in ips:
            target = target + ip + ":" + str(target_port) + ","
        target = target[:-1]
        target = "\"" + target + "\""

        try:
            ovn_nbctl("set", "load_balancer", load_balancer,
                      "vips:" + key + "=" + target)
        except Exception as e:
            vlog.err("_create_load_balancer_vip add: (%s)" % (str(e)))

    def _get_switch_gateway_ip(self, logical_switch):
        cached_logical_switch = self.logical_switch_cache.get(logical_switch,
                                                              {})
        if cached_logical_switch:
            gateway_ip_mask = cached_logical_switch.get('gateway_ip_mask')
        else:
            try:
                gateway_ip_mask = ovn_nbctl("--if-exists", "get",
                                            "logical_switch", logical_switch,
                                            "external_ids:gateway_ip"
                                            ).strip('"')
            except Exception as e:
                vlog.err("_get_switch_gateway_ip: failed to get gateway_ip %s"
                         % (str(e)))
                return (None, None)

        try:
            (gateway_ip, mask) = gateway_ip_mask.split('/')
        except Exception as e:
            vlog.err("_get_switch_gateway_ip: failed to split ip/mask %s"
                     % (gateway_ip_mask))
            return (None, None)

        if not cached_logical_switch:
            self.logical_switch_cache[logical_switch] = {'gateway_ip_mask':
                                                         gateway_ip_mask}
        return (gateway_ip, mask)

    def create_logical_port(self, event):
        data = event.metadata
        logical_switch = data['spec']['nodeName']
        pod_name = data['metadata']['name']
        namespace = data['metadata']['namespace']
        logical_port = "%s_%s" % (namespace, pod_name)
        if not logical_switch or not logical_port:
            vlog.err("absent node name or pod name in pod %s. "
                     "Not creating logical port" % (data))
            return

        (gateway_ip, mask) = self._get_switch_gateway_ip(logical_switch)
        if not gateway_ip or not mask:
            vlog.err("_create_logical_port: failed to get gateway_ip")
            return

        try:
            ovn_nbctl("--", "--may-exist", "lsp-add", logical_switch,
                      logical_port, "--", "lsp-set-addresses",
                      logical_port, "dynamic")
        except Exception as e:
            vlog.err("_create_logical_port: lsp-add (%s)" % (str(e)))
            return

        # We wait for a maximum of 3 seconds to get the dynamic addresses in
        # intervals of 0.1 seconds.
        addresses = ""
        counter = 30
        while counter != 0:
            try:
                ret = ovn_nbctl("get", "logical_switch_port", logical_port,
                                "dynamic_addresses")
                addresses = ast.literal_eval(ret)
                if len(addresses):
                    break
            except Exception as e:
                vlog.err("_create_logical_port: get dynamic_addresses (%s)"
                         % (str(e)))

            time.sleep(0.1)
            counter = counter - 1

        if not len(addresses):
            vlog.err("_create_logical_port: failed to get addresses after "
                     "multiple retries.")
            return

        (mac_address, ip_address) = addresses.split()

        namespace = data['metadata']['namespace']
        pod_name = data['metadata']['name']

        ip_address_mask = "%s/%s" % (ip_address, mask)

        annotation = {'ip_address': ip_address_mask,
                      'mac_address': mac_address,
                      'gateway_ip': gateway_ip}

        try:
            kubernetes.set_pod_annotation(variables.K8S_API_SERVER,
                                          namespace, pod_name,
                                          "ovn", str(annotation))
        except Exception as e:
            vlog.err("_create_logical_port: failed to annotate addresses (%s)"
                     % (str(e)))
            return

        vlog.info("created logical port %s" % (logical_port))

    def delete_logical_port(self, event):
        data = event.metadata
        pod_name = data['metadata']['name']
        namespace = data['metadata']['namespace']
        logical_port = "%s_%s" % (namespace, pod_name)
        if not logical_port:
            vlog.err("absent pod name in pod %s. "
                     "Not creating logical port" % (data))
            return

        try:
            ovn_nbctl("--if-exists", "lsp-del", logical_port)
        except Exception as e:
            vlog.err("_delete_logical_port: lsp-add (%s)" % (str(e)))
            return

        vlog.dbg("deleted logical port %s" % (logical_port))

    def _update_vip(self, service_data, ips):
        service_type = service_data['spec'].get('type')

        service_ip = service_data['spec'].get('clusterIP')
        if not service_ip:
            return

        service_ports = service_data['spec'].get('ports')
        if not service_ports:
            return

        physical_gateway_ips = self._get_physical_gateway_ips()

        for service_port in service_ports:
            if service_type == "NodePort":
                port = service_port.get('nodePort')
            else:
                port = service_port.get('port')

            if not port:
                continue

            protocol = service_port.get('protocol')
            if not protocol:
                protocol = "TCP"

            target_port = service_port.get('targetPort')
            if not target_port:
                target_port = port

            if service_type == "NodePort":
                for gateway_ip in physical_gateway_ips:
                    self._create_load_balancer_vip(service_type, gateway_ip,
                                                   ips, port, target_port,
                                                   protocol)
            else:
                self._create_load_balancer_vip(service_type, service_ip, ips,
                                               port, target_port, protocol)

    def update_vip(self, event):
        service_data = event.metadata
        service_type = service_data['spec'].get('type')
        vlog.dbg("update_vip: received service data %s" % (service_data))

        # We only care about services that are of type 'clusterIP' and
        # 'nodePort'.
        if service_type != "ClusterIP" and service_type != "NodePort":
            return

        service_name = service_data['metadata']['name']
        event_type = event.event_type
        namespace = service_data['metadata']['namespace']

        cache_key = "%s_%s" % (namespace, service_name)

        self._update_service_cache(event_type, cache_key, service_data)

        if event.event_type == "DELETED":
            vlog.dbg("received service delete event.")
            self._update_vip(service_data, None)

    def add_endpoint(self, event):
        endpoint_data = event.metadata
        service_name = endpoint_data['metadata']['name']
        namespace = endpoint_data['metadata']['namespace']
        ips = endpoint_data.get('custom', {}).get('ips', [])

        vlog.dbg("received endpoint data %s" % (endpoint_data))

        cache_key = "%s_%s" % (namespace, service_name)
        cached_service = self.service_cache.get(cache_key, {})
        if cached_service:
            service_data = cached_service
        else:
            try:
                response_json = kubernetes.get_service(
                                                   variables.K8S_API_SERVER,
                                                   namespace, service_name)
            except exceptions.NotFound:
                vlog.dbg("No service found for endpoint %s " % service_name)
                return
            except Exception as e:
                vlog.err("add_endpoint: k8s get service (%s)" % (str(e)))
                return

            service_data = response_json

        service_type = service_data['spec'].get('type')
        if service_type != "ClusterIP" and service_type != "NodePort":
            return

        self._update_vip(service_data, ips)
