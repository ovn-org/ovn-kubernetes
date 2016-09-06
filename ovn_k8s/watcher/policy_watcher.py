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

from eventlet import greenthread
from ovs import vlog

import ovn_k8s
from ovn_k8s.common import kubernetes
from ovn_k8s.common import util
from ovn_k8s.common import variables
import ovn_k8s.processor.policy_processor

log = vlog.Vlog('policy_watcher')


class NetworkPolicyNSWatcher(object):
    """Watch for changes in network policies for a given namespace."""

    def __init__(self, namespace, np_stream):
        self._namespace = namespace
        self._np_stream = np_stream

    def _send_event(self, np_name, event_type, np_data):
        event_metadata = np_data
        log.dbg("Sending event %s for policy %s" % (event_type, np_name))
        event = ovn_k8s.processor.NPEvent(event_type,
                                          source=np_name,
                                          metadata=event_metadata)
        ovn_k8s.processor.policy_processor.get_event_queue().put(event)

    def _process_np_event(self, event):
        event_type = event['type']
        np_data = event['object']
        np_name = np_data['metadata']['name']
        ns_name = np_data['metadata']['namespace']
        log.dbg("Processing event %s for policy %s on namespace %s" %
                (event_type, np_name, ns_name))
        # NOTE: kubernetes currently does not allow updates to network policy
        # spec. Therefore there is no need to worry about changes in pod
        # selectors, or rules (either in the from or ports clause)
        if event_type == 'ADDED':
            self._send_event(np_name, event_type, np_data)
        elif event_type == 'DELETED':
            self._send_event(np_name, event_type, np_data)

    def process(self):
        util.process_stream(self._np_stream,
                            self._process_np_event)


def _generate_watcher(namespace):
    np_stream = kubernetes.watch_network_policies(variables.K8S_API_SERVER,
                                                  namespace)
    return NetworkPolicyNSWatcher(namespace, np_stream)


def create_namespace_watcher(namespace, pool):
    """Add a namespace from which watch network policies."""
    log.info("Starting network policy watcher for namespace: %s" % namespace)

    def _process_loop():
        np_watcher = _generate_watcher(namespace)
        while True:
            try:
                np_watcher.process()
            except StopIteration:
                log.dbg("The watch stream was closed. Re-opening policy "
                        "watch stream for namespace: %s" % namespace)
                np_watcher = _generate_watcher(namespace)

    return pool.spawn(_process_loop)


def remove_namespace_watcher(namespace, watcher_thread):
    log.dbg("Removing network policy watcher for namespace: %s" % namespace)
    greenthread.kill(watcher_thread)
