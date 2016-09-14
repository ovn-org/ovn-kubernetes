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

vlog = ovs.vlog.Vlog("service_watcher")


class ServiceWatcher(object):

    def __init__(self, service_stream):
        self._service_stream = service_stream
        self.service_cache = {}

    def _send_connectivity_event(self, event_type, service_name, service_data):
        ev = ovn_k8s.processor.Event(event_type,
                                     source=service_name,
                                     metadata=service_data)

        conn_processor.get_event_queue().put(ev)

    def _update_service_cache(self, event_type, cache_key, service_data):
        # Remove item from cache if it was deleted
        if event_type == 'DELETED':
            self.service_cache.pop(cache_key, None)
        else:
            # Update cache
            self.service_cache[cache_key] = service_data

    def _process_service_event(self, event):
        service_data = event['object']
        vlog.dbg("obtained service data is %s" % json.dumps(service_data))

        cluster_ip = service_data['spec'].get('clusterIP')

        # When service is created, we may get an event where there is no
        # cluster_ip (VIP) allocated to it.
        if not cluster_ip:
            return

        service_name = service_data['metadata']['name']
        namespace = service_data['metadata']['namespace']
        event_type = event['type']

        cache_key = "%s_%s" % (namespace, service_name)
        cached_service = self.service_cache.get(cache_key, {})
        self._update_service_cache(event_type, cache_key, service_data)

        has_conn_event = False
        if not cached_service:
            has_conn_event = True
        elif event_type == 'DELETED':
            has_conn_event = True
        else:
            return

        if has_conn_event:
            vlog.dbg("Sending connectivity event for event %s on service %s"
                     % (event_type, service_name))
            self._send_connectivity_event(event_type, service_name,
                                          service_data)

    def process(self):
        util.process_stream(self._service_stream,
                            self._process_service_event)
