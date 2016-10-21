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

from ovn_k8s.common import kubernetes
from ovn_k8s.common import variables
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

    def _process_ns_event(self, event, affected_pods, pod_events):
        log.dbg("Processing event %s from namespace %s" % (
            event.event_type, event.source))
        namespace = event.source
        # TODO(salv-orlando): handle namespace isolation change events

        def scan_pods():
            ns_pods = kubernetes.get_pods_by_namespace(
                variables.K8S_API_SERVER, event.source)
            if not ns_pods:
                return
            for pod in ns_pods:
                pod_id = pod['metadata']['uid']
                affected_pods[pod_id] = pod
                pod_events.setdefault(pod_id, []).append(event)

        if event.event_type == 'ADDED':
            log.dbg("Namespace %s added - spawning policy watcher" % namespace)
            watcher_thread = pw.create_namespace_watcher(
                namespace, self._pool)
            self._np_watcher_threads[namespace] = watcher_thread
            # Upon restart this event will be receive for existing pods.
            # The namespace isolation status might have changed while the
            # watcher was not running. Pods in the namespace need to be
            # checked again
            scan_pods()
        elif event.event_type == 'DELETED':
            watcher_thread = self._np_watcher_threads.pop(namespace)
            pw.remove_namespace_watcher(watcher_thread)
        elif event.event_type == 'MODIFIED':
            # This a transition in the namespace isolation status. All the pods
            # in the namespace are affected
            scan_pods()

    def _process_pod_event(self, event, affected_pods, pod_events):
        # NOTE: This function only processes pod label changes events. It does
        # not process create/delete pod events, which are taken care of in the
        # connectivity processor.
        log.dbg("Processing event %s from pod %s" % (
            event.event_type, event.source))
        pod_data = event.metadata
        # Pods are affected only if the namespace is isolated
        # TODO(salv-orlando): Verify semantics of network policy APIs
        if not kubernetes.is_namespace_isolated(
                variables.K8S_API_SERVER, pod_data['metadata']['namespace']):
            log.dbg("Namespace %s for pod %s is not isolated, no need to "
                    "add/remove pod IP from address sets" %
                    (pod_data['metadata']['namespace'], event.source))
            # ACLs for this pod must be recalculated as label changes might
            # imply the set of policies that apply to the pod changes
            # TODO(salv-orlando): Verify whether it is actually needed to
            # recalculate the ACLs for a pod in a non-isolated namespace
            pod_id = pod_data['metadata']['uid']
            affected_pods[pod_id] = pod_data
            pod_events.setdefault(pod_id, []).append(event)
        # A pod label change might also imply changes to address sets
        self.mode.delete_pod_from_address_sets(pod_data)
        self.mode.add_pod_to_address_sets(pod_data)

    def _process_np_event(self, event, affected_pods, pod_events):
        log.dbg("Processing event %s from network policy %s" % (
            event.event_type, event.source))
        namespace = event.metadata['metadata']['namespace']
        policy = event.source
        policy_data = event.metadata
        policy_id = policy_data['metadata']['uid']
        if not kubernetes.is_namespace_isolated(variables.K8S_API_SERVER,
                                                namespace):
            log.warn("Policy %s applied to non-isolated namespace:%s."
                     "Skipping processing" % (policy, namespace))
            return

        # Retrieve pods matching policy pod selector. ACLs for this policy
        # must be created (or destroyed) for these pods
        pod_selector = policy_data.get('podSelector', {})
        pods = kubernetes.get_pods_by_namespace(
            variables.K8S_API_SERVER,
            namespace=namespace,
            pod_selector=pod_selector)
        for pod in pods:
            pod_id = pod['metadata']['uid']
            affected_pods[pod_id] = pod
            pod_events.setdefault(pod_id, []).append(event)

        if event.event_type == 'DELETED':
            # Remove pseudo ACLs for policy and address sets for all of
            # its rules
            self.mode.destroy_address_set(namespace, policy_data)
            self.mode.remove_pseudo_acls(policy_id)
            log.dbg("Policy: %s (Namespace: %s): pseudo ACLs and address sets"
                    "destroyed" % (policy_id, namespace))
            return
        else:
            # As there is no MODIFIED event for policies, if we end up here
            # we are handling an ADDED event
            self.mode.create_address_set(namespace, policy_data)
            self.mode.build_pseudo_acls(policy_data)
            self.mode.add_pods_to_policy_address_sets(policy_data)
            log.dbg("Policy: %s (Namespace: %s): pseudo ACLs and address sets"
                    "created" % (policy_id, namespace))

    def _apply_pod_ns_acls(self, pod_data, pod_events):
        ns_events = [event for event in pod_events if
                     isinstance(event, processor.NSEvent)]
        if not ns_events:
            return
        namespace = pod_data['metadata']['namespace']
        pod_name = pod_data['metadata']['name']
        pod_id = pod_data['metadata']['uid']
        log.dbg("Pod: %s (Namespace: %s): Applying ACL changes for "
                "namespace events" % (pod_name, namespace))
        # In some rare cases, there could multiple namespace events. The
        # ns_events list reports then in the same order in which they were
        # detected and is therefore reliable. We only check first and last as
        # multiple transitions might nullify each other
        if ns_events[0] == ns_events[-1]:
            # Single event
            ns_data = ns_events[-1].metadata
            isolated_final = ns_data.get('custom', {}).get('isolated', False)
        else:
            ns_data_final = ns_events[-1].metadata
            isolated_final = ns_data_final.get(
                'custom', {}).get('isolated', False)
            ns_data_initial = ns_events[0].metadata
            isolated_initial = ns_data_initial.get(
                'custom', {}).get('isolated', False)
            if isolated_final == isolated_initial:
                # Nothing actually changed
                log.dbg("Pod: %s (Namespace: %s): initial and final "
                        "namespace isolation status are identical (%s) - "
                        "no processing necessary" %
                        (pod_name. namespace, isolated_final))
                return
        # TODO(salv-orlando): Verify if this routine can be improved by not
        # having to remove/recreate all ACLs upon each transition
        self.mode.remove_acls(pod_id)
        if not isolated_final:
            log.dbg("Pod: %s (Namespace: %s): not isolated, "
                    "whitelisting traffic for pod" % (pod_name, namespace))
            self.mode.whitelist_pod_traffic(pod_id, namespace, pod_name)

    def _apply_pod_acls(self, pod_data, pod_events, policies):
        pod_name = pod_data['metadata']['name']
        pod_ns = pod_data['metadata']['namespace']

        log.dbg("Pod: %s (Namespace: %s): applying ACLs..." % (
            pod_name, pod_ns))
        # Check for a namespace event. This might mean that there has
        # been a translation from isolated to non-isolated and therefore
        # all we have to do would be whitelisting traffic
        self._apply_pod_ns_acls(pod_data, pod_events)
        if kubernetes.is_namespace_isolated(
                variables.K8S_API_SERVER, pod_ns):
            # If we are here the namespace is isolated, and policies must
            # be translated into acls
            self.mode.apply_pod_policy_acls(pod_data, policies)
        log.dbg("Pod: %s (Namespace: %s): ACLs applied" % (pod_name, pod_ns))

    def process_events(self, events):
        log.dbg("Processing %d events from queue" % len(events))
        affected_pods = {}
        pod_events = {}
        for event in events:
            if isinstance(event, processor.NSEvent):
                # namespace add -> create policy watcher
                # namespace delete -> destory policy watcher
                # namespace update -> check isolation property
                self._process_ns_event(event, affected_pods, pod_events)
            if isinstance(event, processor.NPEvent):
                # policy add -> create ACLs for affected pods
                # policy delete -> remove ACLs for affected pods
                self._process_np_event(event, affected_pods, pod_events)
            if isinstance(event, processor.PodEvent):
                # relevant policies must be applied to pod
                # check policies that select pod in from clause
                self._process_pod_event(event, affected_pods, pod_events)

        ns_policy_map = {}
        for pod_id, pod_data in affected_pods.items():
            pod_ns = pod_data['metadata']['namespace']
            policies = ns_policy_map.get(
                pod_ns, kubernetes.get_network_policies(
                    variables.K8S_API_SERVER, pod_ns))
            ns_policy_map[pod_ns] = policies
            log.dbg("Rebuilding ACL for pod:%s because of:%s" %
                    (pod_id, "; ".join(['%s from %s' % (event.event_type,
                                                        event.source)
                                        for event in pod_events[pod_id]])))
            self._apply_pod_acls(pod_data, pod_events[pod_id], policies)

        log.info("Event processing terminated.")


def get_event_queue():
    """Returns the event queue from the Policy Processor instance."""
    return PolicyProcessor.get_instance().event_queue


def run_processor(pool):
    PolicyProcessor.get_instance(pool).run()
