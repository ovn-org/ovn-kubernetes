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

from ovs import vlog

import ovn_k8s
from ovn_k8s.common import kubernetes
from ovn_k8s.common import util
from ovn_k8s.common import variables
import ovn_k8s.processor
import ovn_k8s.processor.policy_processor as pp


log = vlog.Vlog('namespace_watcher')


class NamespaceWatcher(object):

    def __init__(self, ns_stream):
        self._ns_stream = ns_stream
        self.ns_cache = {}

    def _send_event(self, ns_name, event_type):
        # Send an event to the policy processor to inform it about a
        # transition in namespace isolation or about a new namespace
        event = ovn_k8s.processor.NSEvent(event_type,
                                          source=ns_name,
                                          metadata=self.ns_cache[ns_name])
        pp.get_event_queue().put(event)

    def _process_ns_event(self, event):
        log.dbg("obtained namespace event %s" % json.dumps(event))
        ns_metadata = event['object']['metadata']
        ns_name = ns_metadata['name']
        event_type = event['type']
        cached_ns = self.ns_cache.get(ns_name, {})
        custom_ns_data = cached_ns.setdefault('custom', {})
        isolated_old = custom_ns_data.get('isolated', False)
        isolated_new = kubernetes.is_namespace_isolated(
            variables.K8S_API_SERVER, ns_name)
        custom_ns_data['isolated'] = isolated_new
        ns_metadata['custom'] = custom_ns_data
        self.ns_cache[ns_name] = ns_metadata
        # Always send events for namespaces that are not in cache
        if (event_type in ('ADDED', 'DELETED') or
                isolated_new != isolated_old):
            # Must send event
            self._send_event(ns_name, event_type)
        else:
            log.dbg("No change detected in namespace isolation for:%s" %
                    ns_name)

    def process(self):
        util.process_stream(self._ns_stream,
                            self._process_ns_event)
