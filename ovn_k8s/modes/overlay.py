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

    def _update_service_cache(self, event_type, cache_key, service_data):
        # Remove item from cache if it was deleted.
        if event_type == 'DELETED':
            self.service_cache.pop(cache_key, None)
        else:
            # Update cache
            self.service_cache[cache_key] = service_data

    def _create_load_balancer_vip(self, load_balancer, service_ip, ips, port,
                                  target_port, protocol):
        # With service_ip:port as a VIP, create an entry in 'load_balancer'

        vlog.dbg("received event to create/modify load_balancer (%s) vip "
                 "service_ip=%s, ips=%s, port=%s, target_port=%s, protocol=%s"
                 % (load_balancer, service_ip, ips, port, target_port,
                    protocol))
        if not port or not target_port or not protocol or not load_balancer:
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
        target_endpoints = ",".join(["%s:%s" % (ip, target_port)
                                     for ip in ips])
        target = "\"" + target_endpoints + "\""

        try:
            ovn_nbctl("set", "load_balancer", load_balancer,
                      "vips:" + key + "=" + target)
        except Exception as e:
            vlog.err("_create_load_balancer_vip add: (%s)" % (str(e)))

    def _get_ovn_external_ip_gateway(self):
        # XXX: In case of K8S 'external_ips', we can only expose it in one
        # gateway to prevent duplicate ARP responses.  We currently allocate
        # the first gateway to handle all 'external_ips'.
        try:
            physical_gateway = ovn_nbctl(
                                "--data=bare", "--no-heading",
                                "--columns=name", "find",
                                "logical_router",
                                "external_ids:first_gateway=yes").split()
            if not physical_gateway:
                return None
        except Exception as e:
            vlog.err("_get_ovn_external_ip_gateway: find failed %s" % (str(e)))

        return physical_gateway[0]

    def _get_ovn_gateways(self):
        # Return all created gateways.
        physical_gateways = []
        try:
            physical_gateways = ovn_nbctl(
                                "--data=bare", "--no-heading",
                                "--columns=name", "find",
                                "logical_router",
                                "options:chassis!=null").split()
        except Exception as e:
            vlog.err("_get_ovn_gateways: find failed %s" % (str(e)))

        return physical_gateways

    def _create_gateways_vip(self, ips, port, target_port, protocol):
        # Each gateway has a separate load-balancer for N/S traffic

        physical_gateways = self._get_ovn_gateways()
        if not physical_gateways:
            return

        for physical_gateway in physical_gateways:
            # Go through each gateway to get its physical_ip and load-balancer.
            try:
                physical_ip = ovn_nbctl(
                                    "get", "logical_router", physical_gateway,
                                    "external_ids:physical_ip").strip('"')
            except Exception as e:
                vlog.err("_create_gateways_vip: get failed for"
                         " %s (%s)" % (physical_gateway, str(e)))
                continue

            if not physical_ip:
                vlog.warn("physical gateway %s does not have physical ip"
                          % (physical_ip))
                continue

            try:
                external_id_key = protocol + "_lb_gateway_router"
                load_balancer = ovn_nbctl(
                                    "--data=bare", "--no-heading",
                                    "--columns=_uuid", "find", "load_balancer",
                                    "external_ids:" + external_id_key + "=" +
                                    physical_gateway
                                    ).strip('"')
            except Exception as e:
                vlog.err("_create_gateways_vip: find failed for"
                         " %s (%s)" % (physical_gateway, str(e)))
                continue

            if not load_balancer:
                vlog.warn("physical gateway %s does not have load_balancer"
                          % (physical_gateway))
                continue

            # With the physical_ip:port as the VIP, add an entry in
            # 'load_balancer'.
            self._create_load_balancer_vip(load_balancer, physical_ip, ips,
                                           port, target_port, protocol)

    def _create_cluster_vip(self, service_ip, ips, port, target_port,
                            protocol):
        # Add a VIP in the cluster load-balancer.

        if protocol == "TCP":
            load_balancer = variables.K8S_CLUSTER_LB_TCP
        elif protocol == "UDP":
            load_balancer = variables.K8S_CLUSTER_LB_UDP
        else:
            return

        # With service_ip:port as the VIP, add an entry in 'load_balancer'
        self._create_load_balancer_vip(load_balancer, service_ip, ips, port,
                                       target_port, protocol)

    def _create_external_vip(self, external_ip, ips,
                             port, target_port, protocol):
        # With external_ip:port as the VIP, create an entry in a gateway
        # load-balancer.

        # Get the gateway where we can add external_ip:port as a VIP.
        physical_gateway = self._get_ovn_external_ip_gateway()
        if not physical_gateway:
            return

        try:
            # Get the load-balancer instantiated in the gateway.
            external_id_key = protocol + "_lb_gateway_router"
            load_balancer = ovn_nbctl("--data=bare", "--no-heading",
                                      "--columns=_uuid", "find",
                                      "load_balancer",
                                      "external_ids:" + external_id_key + "=" +
                                      physical_gateway).strip('"')
        except Exception as e:
            vlog.err("_create_external_vip: get failed for"
                     " %s (%s)" % (physical_gateway, str(e)))
            return

            if not load_balancer:
                vlog.warn("physical gateway %s does not have a load_balancer"
                          % (physical_gateway))

        # With external_ip:port as VIP, add an entry in 'load_balancer'.
        self._create_load_balancer_vip(load_balancer, external_ip, ips,
                                       port, target_port, protocol)

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
        if not logical_switch or not pod_name:
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
                      logical_port, "dynamic", "--", "set",
                      "logical_switch_port", logical_port,
                      "external-ids:namespace=" + namespace,
                      "external-ids:pod=true")
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
        if not pod_name:
            vlog.err("absent pod name in pod %s. "
                     "unable to delete logical port" % data)
            return

        try:
            ovn_nbctl("--if-exists", "lsp-del", logical_port)
        except Exception:
            vlog.exception("failure in delete_logical_port: lsp-del")
            return

        vlog.info("deleted logical port %s" % logical_port)

    def _update_vip(self, service_data, ips):
        service_type = service_data['spec'].get('type')

        service_ip = service_data['spec'].get('clusterIP')
        if not service_ip:
            return

        service_ports = service_data['spec'].get('ports')
        if not service_ports:
            return

        external_ips = service_data['spec'].get('externalIPs')

        for service_port in service_ports:
            if service_type == "NodePort":
                port = service_port.get('nodePort')
            else:
                port = service_port.get('port')

            if not port:
                continue

            protocol = service_port.get('protocol', 'TCP')
            target_port = service_port.get('targetPort', port)

            if service_type == "NodePort":
                # Add the 'NodePort' to a load-balancer instantiated in
                # gateways.
                self._create_gateways_vip(ips, port, target_port, protocol)
            elif service_type == "ClusterIP":
                # Add the 'service_ip:port' as a VIP in the cluster
                # load-balancer.
                self._create_cluster_vip(service_ip, ips, port,
                                         target_port, protocol)

            if external_ips:
                for external_ip in external_ips:
                    # Add 'external_ip:port' as a VIP in a gateway
                    # load-balancer.
                    self._create_external_vip(external_ip, ips, port,
                                              target_port, protocol)

    def update_vip(self, event):
        service_data = event.metadata
        service_type = service_data['spec'].get('type')
        service_name = service_data['metadata']['name']
        vlog.dbg("update_vip: received service data %s" % (service_data))

        # We only care about services that are of type 'clusterIP' and
        # 'nodePort'.
        if service_type != "ClusterIP" and service_type != "NodePort":
            vlog.warn("ignoring unsupported service %s of type %s"
                      % (service_name, service_type))
            return

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

    def sync_pods(self, pods):
        expected_logical_ports = set()
        pods = pods.get('items', [])
        for pod in pods:
            pod_name = pod['metadata']['name']
            namespace = pod['metadata']['namespace']
            logical_port = "%s_%s" % (namespace, pod_name)
            expected_logical_ports.add(logical_port)

        try:
            existing_logical_ports = ovn_nbctl(
                                "--data=bare", "--no-heading",
                                "--columns=name", "find",
                                "logical_switch_port",
                                "external_id:pod=true").split()
            existing_logical_ports = set(existing_logical_ports)
        except Exception as e:
            vlog.err("sync_pods: find failed %s" % (str(e)))
            return

        for logical_port in existing_logical_ports - expected_logical_ports:
            try:
                ovn_nbctl("--if-exists", "lsp-del", logical_port)
            except Exception as e:
                vlog.err("sync_pods: failed to delete logical_port %s"
                         % (logical_port))
                continue

            vlog.info("sync_pods: Deleted logical port %s"
                      % (logical_port))

    def _get_load_balancer_vips(self, load_balancer):
        try:
            vips = ovn_nbctl("--data=bare", "--no-heading", "get",
                             "load_balancer", load_balancer,
                             "vips").replace('=', ":")
            return ast.literal_eval(vips)
        except Exception as e:
            vlog.err("_get_load_balancer_vips: failed to get vips for %s (%s)"
                     % (load_balancer, str(e)))
            return None

    def _delete_load_balancer_vip(self, load_balancer, vip):
        # Remove the 'vip' from the 'load_balancer'.
        try:
            ovn_nbctl("remove", "load_balancer", load_balancer, "vips", vip)
            vlog.info("deleted vip %s from %s" % (vip, load_balancer))
        except Exception as e:
            vlog.err("_delete_load_balancer_vip: failed to remove vip %s "
                     " from %s (%s)" % (vip, load_balancer, str(e)))

    def sync_services(self, services):
        # For all the 'clusterIP' services, we will populate the below lists
        # (inside dict) with IP:port.
        cluster_services = {'TCP': [], 'UDP': []}

        # For all the NodePort services, we will populate the below lists with
        # just nodeport or 'external_ip:port'.
        nodeport_services = {'TCP': [], 'UDP': []}

        services = services.get('items', [])
        for service in services:
            service_type = service['spec'].get('type')
            if service_type != "ClusterIP" and service_type != "NodePort":
                continue

            service_ip = service['spec'].get('clusterIP')
            if not service_ip:
                continue

            service_ports = service['spec'].get('ports')
            if not service_ports:
                continue

            external_ips = service['spec'].get('externalIPs')

            for service_port in service_ports:
                if service_type == "NodePort":
                    port = service_port.get('nodePort')
                else:
                    port = service_port.get('port')

                if not port:
                    continue

                protocol = service_port.get('protocol', 'TCP')

                if service_type == "NodePort":
                    if protocol == "TCP":
                        nodeport_services['TCP'].append(str(port))
                    else:
                        nodeport_services['UDP'].append(str(port))
                elif service_type == "ClusterIP":
                    key = "%s:%s" % (service_ip, port)
                    if protocol == "TCP":
                        cluster_services['TCP'].append(key)
                    else:
                        cluster_services['UDP'].append(key)

                if external_ips:
                    for external_ip in external_ips:
                        key = "%s:%s" % (external_ip, port)
                        if protocol == "TCP":
                            nodeport_services['TCP'].append(key)
                        else:
                            nodeport_services['UDP'].append(key)

        # For OVN cluster load-balancer if the VIP that exists in the
        # OVN load balancer is not seen in current k8s services, we delete it.
        load_balancers = {
                         variables.K8S_CLUSTER_LB_TCP: cluster_services['TCP'],
                         variables.K8S_CLUSTER_LB_UDP: cluster_services['UDP'],
                         }

        for load_balancer, k8s_services in load_balancers.items():
            vips = self._get_load_balancer_vips(load_balancer)
            if not vips:
                continue

            for vip in vips:
                if vip not in k8s_services:
                    vip = "\"" + vip + "\""
                    self._delete_load_balancer_vip(load_balancer, vip)

        # For each gateway, remove any VIP that does not exist in
        # 'nodeport_services'
        physical_gateways = self._get_ovn_gateways()
        for physical_gateway in physical_gateways:
            for protocol, service in nodeport_services.items():
                try:
                    external_id_key = protocol + "_lb_gateway_router"
                    load_balancer = ovn_nbctl(
                                    "--data=bare", "--no-heading",
                                    "--columns=_uuid", "find", "load_balancer",
                                    "external_ids:" + external_id_key + "=" +
                                    physical_gateway
                                    ).strip('"')
                except Exception as e:
                    vlog.err("sync_services: get failed for"
                             " %s (%s)" % (physical_gateway, str(e)))
                    continue

                if not load_balancer:
                    continue

                # Get the OVN load-balancer VIPs.
                vips = self._get_load_balancer_vips(load_balancer)
                if not vips:
                    continue

                for vip in vips:
                    vip_and_port = vip.split(":")
                    if len(vip_and_port) == 1:
                        # In a OVN load-balancer, we should always have
                        # vip:port.  In the unlikely event that it is not
                        # the case, skip it.
                        continue

                    # An example 'service' is ["3892", "10.1.1.20:8880"]
                    if vip_and_port[1] in service or vip in service:
                        continue

                    vip = "\"" + vip + "\""
                    self._delete_load_balancer_vip(load_balancer, vip)
