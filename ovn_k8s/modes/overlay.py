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
import collections
import hashlib
import json
import shlex
import time

import ovs.vlog
from ovn_k8s.common import exceptions
import ovn_k8s.common.kubernetes as kubernetes
import ovn_k8s.common.variables as variables
from ovn_k8s.common.util import ovn_nbctl

PseudoACL = collections.namedtuple(
    'PseudoACL', 'priority ports src_ips action')

vlog = ovs.vlog.Vlog("overlay")
DEFAULT_ACL_PRIORITY = 1000
DEFAULT_ALLOW_ACL_PRIORITY = 1001
POLICY_ACL_PRIORITY = 1100
ALL_PORTS = {"ports": "*"}


class OvnNB(object):
    def __init__(self):
        self.service_cache = {}
        self.logical_switch_cache = {}
        self.pseudo_acls = {}

    def _get_physical_gateway_ips(self):
        physical_gateway_ips = []
        try:
            physical_gateway_ip_networks = ovn_nbctl(
                                "--data=bare", "--no-heading",
                                "--columns=network", "find",
                                "logical_router_port",
                                "external_ids:gateway-physical-ip=yes").split()
        except Exception as e:
            vlog.err("_populate_gateway_ip: find failed %s" % (str(e)))
            return physical_gateway_ips

        for physical_gateway_ip_network in physical_gateway_ip_networks:
            ip, _mask = physical_gateway_ip_network.split('/')
            physical_gateway_ips.append(ip)

        return physical_gateway_ips

    def _update_service_cache(self, event_type, cache_key, service_data):
        # Remove item from cache if it was deleted.
        if event_type == 'DELETED':
            self.service_cache.pop(cache_key, None)
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
        elif protocol == "UDP" and service_type == "ClusterIP":
            load_balancer = variables.K8S_CLUSTER_LB_UDP
        elif protocol == "TCP" and service_type == "NodePort":
            load_balancer = variables.K8S_NS_LB_TCP
        elif protocol == "UDP" and service_type == "NodePort":
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
        target_endpoints = ",".join(["%s:%s" % (ip, target_port)
                                     for ip in ips])
        target = "\"" + target_endpoints + "\""

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
            vlog.err("_get_switch_gateway_ip: failed to split ip/mask %s: %s"
                     % (gateway_ip_mask, str(e)))
            return (None, None)

        if not cached_logical_switch:
            self.logical_switch_cache[logical_switch] = {'gateway_ip_mask':
                                                         gateway_ip_mask}
        return (gateway_ip, mask)

    def _build_logical_port_name(self, namespace, pod_name):
        return "%s_%s" % (namespace, pod_name)

    def create_logical_port(self, event, create_network_policies=False):
        data = event.metadata
        logical_switch = data['spec']['nodeName']
        pod_name = data['metadata']['name']
        namespace = data['metadata']['namespace']
        logical_port = self._build_logical_port_name(namespace, pod_name)
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
        # When a pod gets created, we need to do two things.
        # 1) Look at all network policies and see if that network policy's
        # pod selector matches this pod. If it does, add ingress control ACLs
        # based on the rules in that policy object.
        # 2) Look at all the network policies and see whether this pod matches
        # any 'from' clause in the policy rules. If it does match, then
        # this pod IP address must be added to address sets for matching rules.
        if create_network_policies:
            # Setup  policies for pod
            self.create_pod_acls(data)
            vlog.dbg("Pod: %s (Namespace: %s) - ACLs for policies created"
                     % (pod_name, namespace))
            # Add pod IP to relevant address sets
            self.add_pod_to_address_sets(data, pod_ip=ip_address)
            vlog.dbg("Pod: %s (Namespace: %s) - Pod IP added to address sets"
                     % (pod_name, namespace))

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

    def delete_logical_port(self, event, delete_network_policies):
        data = event.metadata
        pod_id = data['metadata']['uid']
        pod_name = data['metadata']['name']
        namespace = data['metadata']['namespace']
        logical_port = "%s_%s" % (namespace, pod_name)
        if not pod_name:
            vlog.err("absent pod name in pod %s. "
                     "unable to delete logical port" % data)
            return

        if delete_network_policies:
            try:
                # Remove ACLs for pod
                self.delete_pod_acls(pod_id)
                vlog.dbg("Pod: %s (Namespace: %s) - ACLs for pod removed " %
                         (pod_name, namespace))
                # Remove references to pod from address sets
                self.delete_pod_from_address_sets(data)
                vlog.dbg("Pod: %s (Namespace: %s) - Pod IP removed from "
                         "address sets" % (pod_name, namespace))
            except Exception:
                # Do not return or fail in case of an error - give the
                # routine a chance to delete the logical port at least
                vlog.exception('Pod:%s (Namespace:%s) - failure while '
                               'removing ACLs', pod_name, namespace)

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

        physical_gateway_ips = self._get_physical_gateway_ips()

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
                for gateway_ip in physical_gateway_ips:
                    self._create_load_balancer_vip(service_type, gateway_ip,
                                                   ips, port, target_port,
                                                   protocol)
            elif service_type == "ClusterIP":
                self._create_load_balancer_vip(service_type, service_ip, ips,
                                               port, target_port, protocol)

            if external_ips:
                for external_ip in external_ips:
                    self._create_load_balancer_vip("NodePort", external_ip,
                                                   ips, port, target_port,
                                                   protocol)

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

    def sync_services(self, services):
        # For all the services, we will populate the below lists with
        # IP:port that act as VIP in the OVN load-balancers.
        tcp_nodeport_services = []
        udp_nodeport_services = []
        tcp_services = []
        udp_services = []
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
                    physical_gateway_ips = self._get_physical_gateway_ips()
                    for gateway_ip in physical_gateway_ips:
                        key = "%s:%s" % (gateway_ip, port)
                        if protocol == "TCP":
                            tcp_nodeport_services.append(key)
                        else:
                            udp_nodeport_services.append(key)
                elif service_type == "ClusterIP":
                    key = "%s:%s" % (service_ip, port)
                    if protocol == "TCP":
                        tcp_services.append(key)
                    else:
                        udp_services.append(key)

                if external_ips:
                    for external_ip in external_ips:
                        key = "%s:%s" % (external_ip, port)
                        if protocol == "TCP":
                            tcp_nodeport_services.append(key)
                        else:
                            udp_nodeport_services.append(key)

        # For each of the OVN load-balancer, if the VIP that exists in
        # the load balancer is not seen in current k8s services, we
        # delete it.
        load_balancers = {variables.K8S_CLUSTER_LB_TCP: tcp_services,
                          variables.K8S_CLUSTER_LB_UDP: udp_services,
                          variables.K8S_NS_LB_TCP: tcp_nodeport_services,
                          variables.K8S_NS_LB_UDP: udp_nodeport_services}

        for load_balancer, k8s_services in load_balancers.items():
            vips = self._get_load_balancer_vips(load_balancer)
            if not vips:
                continue

            for vip in vips:
                if vip not in k8s_services:
                    vip = "\"" + vip + "\""
                    try:
                        ovn_nbctl("remove", "load_balancer", load_balancer,
                                  "vips", vip)
                        vlog.info("sync_services: deleted vip %s from %s"
                                  % (vip, load_balancer))
                    except Exception as e:
                        vlog.err("sync_services: failed to remove vip %s"
                                 "from %s (%s)" % (vip, load_balancer, str(e)))

    def remove_acls(self, pod_id, remove_default=False):
        acls_raw = ovn_nbctl('--data=bare', '--no-heading',
                             '--columns=_uuid,priority', 'find', 'ACL',
                             'external_ids:pod_id=%s' % pod_id).split()
        if not acls_raw:
            return

        # Create a list of tuples whose first element is the ACL uuid and the
        # second one its priority
        acls = zip(acls_raw[0::2], acls_raw[1::2])
        ls_name = None
        for acl in acls:
            if not ls_name:
                # On delete operations the logical port is unfortunately
                # gone, but the logical switch can be found from existing ACLs
                ls_name = ovn_nbctl(
                    '--data=bare', '--no-heading', '--columns=name',
                    'find', 'Logical_Switch', 'acls{>=}%s' % acl[0])
                if not ls_name:
                    vlog.warn("Unable to find logical switch for ACL:%s. "
                              "The ACL will not be removed" % acl[0])
                    continue
            if (not remove_default and
                    int(acl[1]) == DEFAULT_ACL_PRIORITY):
                # Do not drop the default drop rule
                continue
            ovn_nbctl('remove', 'Logical_Switch', ls_name, 'acls', acl[0])

    def _create_acl(self, pod_id, ls_name, lport_name, priority,
                    match, action, **kwargs):
        kwargs['pod_id'] = pod_id
        external_ids = " ".join('external_ids:%s=%s' % (k, v)
                                for k, v in kwargs.items())
        # Execute a find first to avoid duplicate ACLs
        find_command = ('--data=bare --no-heading --columns=_uuid '
                        'find ACL action=%s direction=to-lport priority=%d '
                        'match="%s" external_ids:lport_name=%s' %
                        (action, priority, match, lport_name))
        find_command_items = tuple(shlex.split(find_command))
        acl_uuid = ovn_nbctl(*find_command_items)
        if acl_uuid:
            vlog.dbg("ACL for port %s with match %s and action %s already "
                     "exists. Skipping ACL create" %
                     (lport_name, match, action))
            return
        # Note: The reason rather complicated expression is to be able to set
        # an external id for the ACL as well (acl-add won't return the ACL id)
        command = ('-- --id=@acl_id create ACL action=%s direction=to-lport '
                   'priority=%d match="%s" external_ids:lport_name=%s '
                   '%s -- add Logical_Switch %s acls @acl_id' %
                   (action, priority, match, lport_name,
                    external_ids, ls_name))
        command_items = tuple(shlex.split(command))
        ovn_nbctl(*command_items)

    def whitelist_pod_traffic(self, pod_id, namespace, pod_name):
        lport_name = self._build_logical_port_name(namespace, pod_name)
        lport_uuid = ovn_nbctl("--data=bare", "--no-heading",
                               "--columns=_uuid", "find",
                               "logical_switch_port",
                               "name=%s" % lport_name)
        # Do not take for granted that a logical port already exists
        if not lport_uuid:
            vlog.dbg("Pod %s (Namespace:%s): No logical port found." %
                     (pod_name, namespace))
            return
        lswitch_name = ovn_nbctl("--data=bare", "--no-heading",
                                 "--columns=_uuid", "find",
                                 "logical_switch",
                                 "ports{>=}%s" % lport_uuid)
        self._create_acl(pod_id, lswitch_name, lport_name,
                         DEFAULT_ALLOW_ACL_PRIORITY,
                         r'outport\=\=\"%s\"\ &&\ ip' % lport_name,
                         'allow-related')
        vlog.dbg("Pod %s (Namespace:%s): Traffic whitelisted"
                 % (pod_name, namespace))

    def _create_acl_match(self, lport_name, pseudo_acl):
        # TODO(salv-orlando): move this function to the util module
        src_match = None
        ports_match = None
        # NOTE: We do not validate that the protocol names are valid
        ports_clause = pseudo_acl.ports or []
        from_clause = pseudo_acl.src_ips
        if from_clause and from_clause != '*':
            # '*' means every address matches and therefore no source IP
            # match should be added to the ACL
            src_match = r"ip4.src\=\=\{%s\}" % from_clause
        match_items = set()
        protocol_port_map = {}
        for (protocol, port) in ports_clause:
            ports = protocol_port_map.setdefault(protocol, set())
            if port:
                ports.add(port)
        for protocol, ports in protocol_port_map.items():
            if ports:
                item_match_str = (r"%s.dst\=\=\{%s\}" %
                                  (protocol.lower(), "\,".join(
                                      [str(port) for port in ports
                                       if port is not None])))
            else:
                item_match_str = protocol.lower()
            match_items.add(item_match_str)
        ports_match = "\ ||\ ".join(match_items)
        if ports_match and src_match:
            policy_match = "\(%s\)\ &&\ \(%s\)" % (ports_match, src_match)
        elif ports_match:
            policy_match = ports_match
        elif src_match:
            policy_match = src_match
        else:
            policy_match = None
        outport_match = r'outport\=\=\"%s\"\ &&\ ip' % lport_name
        if policy_match:
            match = r'%s\ &&\ %s' % (outport_match, policy_match)
        else:
            match = outport_match
        return match

    def _policy_pod_traffic(self, pod_data, pseudo_acls):
        pod_name = pod_data['metadata']['name']
        pod_ns = pod_data['metadata']['namespace']
        pod_id = pod_data['metadata']['uid']
        # TODO(salv-orlando): do not remove the default drop in the next step
        # and ensure the same rule is not created if already defined in the
        # subsequent step. The current code will leave a pod without security
        # rules or with incomplete security rules if an error occurs.
        self.remove_acls(pod_id, remove_default=True)
        # TODO(salv-orlando): lswitch & lport can be easily cached
        lport_name = self._build_logical_port_name(pod_ns, pod_name)
        lport_uuid = ovn_nbctl("--data=bare", "--no-heading",
                               "--columns=_uuid", "find",
                               "logical_switch_port",
                               "name=%s" % lport_name)
        lswitch_name = ovn_nbctl("--data=bare", "--no-heading",
                                 "--columns=name", "find",
                                 "logical_switch",
                                 "ports{>=}%s" % lport_uuid)
        for pseudo_acl in pseudo_acls:
            match = self._create_acl_match(lport_name, pseudo_acl)
            vlog.dbg("Pod: %s (Namespace:%s): ACL match: %s" % (
                pod_name, pod_ns, match))
            ovn_acl_data = (pseudo_acl.priority, match, pseudo_acl.action)
            # TODO(salv-orlando): Also store policy name in external ids.
            # It could be useful for debugging
            self._create_acl(pod_id, lswitch_name, lport_name, *ovn_acl_data)

    def _build_rule_address_set_name(self, policy_name, namespace, rule_data):
        # Build a name for the rule address set concatenating:
        # 1) the name of the network policy
        # 2) the name of the policy's namespace
        # 3) a hash of the rule's info (ports and from clauses)
        hasher = hashlib.md5()
        hash_data = json.dumps(
            rule_data.get('ports'), ALL_PORTS).encode('utf-8')
        hasher.update(hash_data)
        rule_hash = hasher.hexdigest()
        return "%s_%s_%s" % (policy_name, namespace, rule_hash)

    def create_address_set(self, namespace, policy_data):
        """ Create address sets for a network policy

        This routine creates an OVN address set for each rule in a
        kubernetes network policy object.

        Args:
            namespace(:obj:`str`): The policy's namespace
            policy_data(dict): Policy info as returned by the kubernetes
                API server, contains both the spec and status sections

        """
        for rule in policy_data['spec']['ingress']:
            address_set = self._build_rule_address_set_name(
                policy_data['metadata']['name'],
                namespace,
                rule)
            # First verify that the address set does not already exist
            if not ovn_nbctl("--data=bare", "--no-heading",
                             "--columns=_uuid", "find",
                             "address_set", "name=%s" % address_set):
                ovn_nbctl('create', 'address_set', 'name=%s' % address_set)

    def destroy_address_set(self, namespace, policy_data):
        for rule in policy_data['spec']['ingress']:
            address_set = self._build_rule_address_set_name(
                policy_data['metadata']['name'],
                namespace,
                rule)
            if ovn_nbctl("--data=bare", "--no-heading",
                         "--columns=_uuid", "find",
                         "address_set", "name=%s" % address_set):
                ovn_nbctl('destroy', 'address_set', address_set)

    def add_to_address_set(self, pod_ip, namespace, policy_data):
        for rule in policy_data['spec']['ingress']:
            rule_address_set = self._build_rule_address_set_name(
                policy_data['metadata']['name'],
                namespace,
                rule)
            ovn_nbctl('--if-exists', 'add', 'address_set',
                      rule_address_set, 'addresses', pod_ip)

    def remove_from_address_set(self, pod_ip, namespace, policy_data):
        for rule in policy_data['spec']['ingress']:
            rule_address_set = self._build_rule_address_set_name(
                policy_data['metadata']['name'],
                namespace,
                rule)
            ovn_nbctl('--if-exists', 'remove', 'address_set',
                      rule_address_set, 'addresses', pod_ip)

    def build_pseudo_acls(self, policy_data):
        # Build pseudo ACL list for policy
        policy_id = policy_data['metadata']['uid']
        policy_pseudo_acls = []
        for rule in policy_data['spec']['ingress']:
            ports_data = rule.get('ports', [])
            protocol_ports = []
            for item in ports_data:
                protocol_ports.append((item['protocol'], item.get('port')))
            from_data = rule.get('from')
            if not from_data:
                src_pod_ips = '*'
            else:
                rule_address_set = self._build_rule_address_set_name(
                    policy_data['metadata']['name'],
                    policy_data['metadata']['namespace'],
                    rule)
                src_pod_ips = 'address_set(%s)' % rule_address_set
            pseudo_acl = PseudoACL(POLICY_ACL_PRIORITY, protocol_ports,
                                   src_pod_ips, 'allow-related')
            policy_pseudo_acls.append(pseudo_acl)
        self.pseudo_acls[policy_id] = policy_pseudo_acls
        return policy_pseudo_acls

    def remove_pseudo_acls(self, policy_id):
        self.pseudo_acls.pop(policy_id, None)

    def _find_policies_for_pod(self, pod_data, policies):
        # Given a set of network policiy instances and a pod, Return every
        # policy instance whose pod selector matches the pod
        pod_labels = pod_data['metadata'].get('labels', {})
        pod_policies = []
        for policy in policies:
            policy_spec = policy['spec']
            pod_selector = policy_spec.get('podSelector', {}).get(
                'matchLabels', {})
            # TODO(salv-orlando): Implement not only equality based selectors
            if pod_selector:
                for label in set(pod_labels.keys()) & set(pod_selector.keys()):
                    if pod_labels[label] == pod_selector[label]:
                        pod_policies.append(policy)
                        # policy matched, move on
                        break
            else:
                # An empty selector matches all pods in the namespace
                pod_policies.append(policy)
        return pod_policies

    def apply_pod_policy_acls(self, pod_data, policies):
        namespace = pod_data['metadata']['namespace']
        pod_name = pod_data['metadata']['name']
        pod_policies = self._find_policies_for_pod(pod_data, policies)
        # Always start with a drop all rule
        pseudo_acls = [PseudoACL(DEFAULT_ACL_PRIORITY, [], '*', 'drop')]
        for policy in pod_policies:
            policy_pseudo_acls = self.pseudo_acls.get(
                policy['metadata']['uid'])
            if not policy_pseudo_acls:
                policy_pseudo_acls = self.build_pseudo_acls(policy)
            pseudo_acls.extend(policy_pseudo_acls)
        self._policy_pod_traffic(pod_data, pseudo_acls)
        vlog.dbg("Pod: %s (Namespace: %s): ACL changes for policies applied"
                 % (pod_name, namespace))

    def create_pod_acls(self, pod_data):
        namespace = pod_data['metadata']['namespace']
        if kubernetes.is_namespace_isolated(variables.K8S_API_SERVER,
                                            namespace):
            policies = kubernetes.get_network_policies(
                variables.K8S_API_SERVER, namespace)
            self.apply_pod_policy_acls(pod_data, policies)
        else:
            self.whitelist_pod_traffic(pod_data['metadata']['uid'],
                                       namespace,
                                       pod_data['metadata']['name'])

    def delete_pod_acls(self, pod_id):
        self.remove_acls(pod_id, remove_default=True)

    def _pod_matches_from_clause(self, pod_data, ns_data, policy_rule):
        pod_labels = pod_data['metadata'].get('labels', {})
        try:
            from_clauses = policy_rule['from']
            if not from_clauses:
                # Empty from clause mean no pod can match
                return False
        except KeyError:
            # Missing from clause means policy affects all pods
            return True
        # NOTE: In this case a missing element and an empty element have
        # different semantics
        for from_clause in from_clauses:
            if 'podSelector' in from_clause:
                from_pods = from_clause.get('podSelector')
                if not from_pods:
                    # Empty pod selector means policy affect all pods in
                    # the current namespace
                    return True
                # NOTE: the current code assumes only equality-based selectors
                for label in set(pod_labels.keys()) & set(from_pods.keys()):
                    if pod_labels[label] == from_pods[label]:
                        return True
            elif 'namespaceSelector' in from_clause:
                from_namespaces = from_clause.get('namespaceSelector')
                if not from_namespaces:
                    # Empty namespace selector means all namespaces, and
                    # threfore the pod's one as well
                    return True
                # NOTE: the current code assumes only equality-based selectors
                ns_labels = ns_data['metadata'].get('labels', {})
                for label in (set(ns_labels.keys()) &
                              set(from_namespaces.keys())):
                    if pod_labels[label] == from_namespaces[label]:
                        return True
        # We tried very hard, but no match was found
        return False

    def add_pod_to_address_sets(self, pod_data, pod_ip=None):
        # Update every addresss set for rules that match the pod with the IP
        # address of the pod
        pod_ns = pod_data['metadata']['namespace']
        pod_name = pod_data['metadata']['name']
        pod_ip = pod_ip or pod_data['status'].get('podIP')
        if not pod_ip:
            vlog.dbg("Pod %s (Namespace %s): No IP address available, not "
                     "updating address sets." % (pod_name, pod_ns))
            return
        ns_data = kubernetes.get_namespace(variables.K8S_API_SERVER, pod_ns)
        nw_policies = kubernetes.get_network_policies(
                variables.K8S_API_SERVER, pod_ns)
        if not nw_policies:
            vlog.dbg("Pod %s (Namespace %s): No network policy to configure."
                     % (pod_name, pod_ns))
            return
        for policy in nw_policies:
            for rule in policy['spec']['ingress']:
                if self._pod_matches_from_clause(pod_data, ns_data, rule):
                    self.add_to_address_set(pod_ip, pod_ns, policy)

    def add_pods_to_policy_address_sets(self, policy_data):
        # Update every address sets for a given policy with all the addresses
        # from pods matching the from clayse
        policy_ns = policy_data['metadata']['namespace']
        ns_data = kubernetes.get_namespace(variables.K8S_API_SERVER,
                                           policy_ns)
        for pod_data in kubernetes.get_pods_by_namespace(
                variables.K8S_API_SERVER, policy_ns):
            pod_ip = pod_data['status'].get('podIP')
            if not pod_ip:
                continue
            for rule in policy_data['spec']['ingress']:
                if self._pod_matches_from_clause(pod_data, ns_data, rule):
                    self.add_to_address_set(pod_ip, policy_ns, policy_data)

    def delete_pod_from_address_sets(self, pod_data):
        namespace = pod_data['metadata']['namespace']
        policies = kubernetes.get_network_policies(
            variables.K8S_API_SERVER,
            namespace)
        # NOTE: Removing the pod IP from every address set is not harmful but
        # cna be optimized by removing it only from the address sets that
        # match the policy rule
        for policy_data in policies:
            self.remove_from_address_set(
                pod_data['status']['podIP'],
                namespace,
                policy_data)
