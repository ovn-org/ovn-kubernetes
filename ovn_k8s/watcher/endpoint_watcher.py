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

import json

import ovs.vlog
import ovn_k8s.processor
from ovn_k8s.processor import conn_processor
from ovn_k8s.common import util

vlog = ovs.vlog.Vlog("endpoint_watcher")


class EndpointWatcher(object):

    def __init__(self, endpoint_stream):
        self._endpoint_stream = endpoint_stream
        self.endpoint_cache = {}

    def _send_connectivity_event(self, event_type, endpoint_name,
                                 endpoint_data):
        ev = ovn_k8s.processor.EndpointEvent(event_type,
                                             source=endpoint_name,
                                             metadata=endpoint_data)
        conn_processor.get_event_queue().put(ev)

    def _process_endpoint_event(self, event):
        vlog.dbg("obtained endpoint event %s" % json.dumps(event))
        endpoint_data = event['object']
        event_type = event['type']
        endpoint_id = endpoint_data['metadata']['uid']
        endpoint_name = endpoint_data['metadata'].get('name')
        namespace = endpoint_data['metadata'].get('namespace')

        ips = set()
        subsets = endpoint_data.get('subsets')
        if subsets:
            for subset in subsets:
                addresses = subset.get('addresses')
                if not addresses:
                    continue
                for address in addresses:
                    ip = address.get('ip')
                    if ip:
                        ips.add(ip)

        if not endpoint_name or not namespace:
            return

        cached_endpoint = self.endpoint_cache.get(endpoint_id)
        if (not cached_endpoint or
                cached_endpoint.get('custom', {}).get('ips') != ips):
            vlog.dbg("Sending connectivity event for event %s on endpoint %s"
                     % (event_type, endpoint_name))
            if cached_endpoint:
                custom_data = cached_endpoint.setdefault('custom', {})
            else:
                custom_data = {}
            custom_data['ips'] = ips
            endpoint_data['custom'] = custom_data
            self._send_connectivity_event(event_type,
                                          endpoint_name,
                                          endpoint_data)
            # Update cache
            self.endpoint_cache[endpoint_id] = endpoint_data

    def process(self):
        util.process_stream(self._endpoint_stream,
                            self._process_endpoint_event)
