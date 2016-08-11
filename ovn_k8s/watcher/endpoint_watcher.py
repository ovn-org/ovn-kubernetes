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
        ev = ovn_k8s.processor.Event(event_type,
                                     source=endpoint_name,
                                     metadata=endpoint_data)
        conn_processor.get_event_queue().put(ev)

    def _process_endpoint_event(self, event):
        vlog.dbg("obtained endpoint event %s" % json.dumps(event))
        endpoint_data = event['object']
        event_type = event['type']

        endpoint_name = endpoint_data['metadata'].get('name')
        namespace = endpoint_data['metadata'].get('namespace')
        if not endpoint_name or not namespace:
            return

        vlog.dbg("Sending connectivity event for event %s on endpoint %s"
                 % (event_type, endpoint_name))
        self._send_connectivity_event(event_type, endpoint_name, endpoint_data)

    def process(self):
        util.process_stream(self._endpoint_stream,
                            self._process_endpoint_event)
