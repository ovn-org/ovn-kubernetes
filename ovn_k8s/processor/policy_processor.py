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

from ovs import vlog

import ovn_k8s.processor as processor
import ovn_k8s.watcher.policy_watcher

log = vlog.Vlog('policy_processor')
pw = ovn_k8s.watcher.policy_watcher


class PolicyProcessor(processor.BaseProcessor):
    """PolicyProcessor - process events related to k8s network policies

    This class processes events generated from namespace, policy, and pod
    watchers that pertain kubernetes network policies.

    Its main responsabilities are:
        - Creating/destroying OVN ACLs for Pods;
        - Ensuring programmed ACLs are always consistent with a namespace's
          isolation status;
        - Starting policy watcher threads when new namespaces are detected.

    """

    def __init__(self, pool):
        super(PolicyProcessor, self).__init__()
        self._pool = pool
        self._np_watcher_threads = {}

    def _process_ns_event(self, event):
        namespace = event.source
        # TODO(salv-orlando): handle namespace isolation change events
        if event.event_type == 'ADDED':
            log.dbg("Namespace %s added - spawning policy watcher" % namespace)
            watcher_thread = pw.create_namespace_watcher(
                namespace, self._pool)
            self._np_watcher_threads[namespace] = watcher_thread
        elif event.event_type == 'DELETED':
            watcher_thread = self._np_watcher_threads.pop(namespace)
            pw.remove_namespace_watcher(watcher_thread)

    def process_events(self, events):
        log.dbg("Processing %d events from queue" % len(events))
        for event in events[:]:
            if isinstance(event, processor.NSEvent):
                # namespace add -> create policy watcher
                # namespace delete -> destory policy watcher
                # namespace update -> check isolation property
                self._process_ns_event(event)
            events.remove(event)

        for event in events:
            log.warn("Event %s from %s was not processed. ACLs might not be "
                     "in sync with network policies",
                     event.event_type, event.source)
        else:
            log.info("Event processing terminated.")


def get_event_queue():
    """Returns the event queue from the Policy Processor instance."""
    return PolicyProcessor.get_instance().event_queue


def run_processor(pool):
    PolicyProcessor.get_instance(pool).run()
