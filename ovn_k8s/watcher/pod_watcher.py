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
from ovn_k8s.processor import policy_processor as pp
from ovn_k8s.common import util

vlog = ovs.vlog.Vlog("pod_watcher")


class PodWatcher(object):

    def __init__(self, pod_stream):
        self._pod_stream = pod_stream
        self.pod_cache = {}
        self.pod_ips = {}

    def _send_connectivity_event(self, event_type, pod_name, pod_data):
        # If available, add the pod IP to event metadata
        pod_ip = self.pod_ips.get(pod_data['metadata']['uid'])
        ev = ovn_k8s.processor.PodEvent(event_type,
                                        source=pod_name,
                                        metadata=(pod_data, pod_ip))
        conn_processor.get_event_queue().put(ev)

    def _send_policy_event(self, event_type, pod_name, pod_data):
        ev = ovn_k8s.processor.PodEvent(event_type,
                                        source=pod_name,
                                        metadata=pod_data)
        pp.get_event_queue().put(ev)

    def _update_pod_cache(self, event_type, cache_key, pod_data):
        # Remove item from cache if it was deleted
        if event_type == 'DELETED':
            # Do not take for granted that the pod is in the key, as there are
            # some corner cases in which a pod could be deleted without ever
            # making it to local cache
            self.pod_cache.pop(cache_key, None)
        else:
            # Update cache
            self.pod_cache[cache_key] = pod_data

    def _process_pod_event(self, event):
        vlog.dbg("obtained pod event %s" % json.dumps(event))
        pod_data = event['object']
        event_type = event['type']

        pod_name = pod_data['metadata'].get('name')
        namespace = pod_data['metadata'].get('namespace')
        if not pod_name or not namespace:
            return

        # To create a logical port for a pod, we need to know the node
        # where it has been scheduled.  The first event from the API server
        # may not have this information, but we will eventually get it.
        if event_type != 'DELETED' and not pod_data['spec'].get('nodeName'):
            return

        # If a pod has an IP, save it
        if pod_data['metadata']['uid'] not in self.pod_ips:
            pod_ip = pod_data['status'].get('podIP')
            if pod_ip:
                self.pod_ips[pod_data['metadata']['uid']] = pod_ip

        cache_key = "%s_%s" % (namespace, pod_name)
        cached_pod = self.pod_cache.get(cache_key, {})

        has_conn_event = False
        label_changes = False
        if not cached_pod:
            has_conn_event = True
        elif event_type == 'DELETED':
            has_conn_event = True
        else:
            label_changes = util.has_changes(
                pod_data['metadata'].get('labels', {}),
                cached_pod['metadata'].get('labels', {}))

        self._update_pod_cache(event_type, cache_key, pod_data)
        if has_conn_event:
            vlog.dbg("Sending connectivity event for event %s on pod %s"
                     % (event_type, pod_name))
            self._send_connectivity_event(event_type, pod_name, pod_data)
        if label_changes:
            vlog.dbg("Sending policy event for event %s on pod %s" %
                     (event_type, pod_name))
            self._send_policy_event(event_type, pod_name, pod_data)

    def process(self):
        util.process_stream(self._pod_stream,
                            self._process_pod_event)
