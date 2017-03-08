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

from eventlet import greenpool
import sys

import ovs
import ovs.unixctl
import ovs.unixctl.server
import ovs.vlog
import ovn_k8s
from ovn_k8s.common import config
from ovn_k8s.common import exceptions
from ovn_k8s.common import variables
from ovn_k8s.common import kubernetes
from ovn_k8s.watcher import pod_watcher
from ovn_k8s.watcher import service_watcher
from ovn_k8s.watcher import endpoint_watcher
from ovn_k8s.processor import conn_processor

vlog = ovs.vlog.Vlog("watcher")
exiting = False


def _unixctl_exit(conn, unused_argv, unused_aux):
    global exiting
    exiting = True
    conn.reply(None)


def _unixctl_run():
    ovs.unixctl.command_register("exit", "", 0, 0, _unixctl_exit, None)
    error, unixctl_server = ovs.unixctl.server.UnixctlServer.create(None)
    if error:
        ovs.util.ovs_fatal(error, "could not create unixctl server", vlog)

    while True:
        unixctl_server.run()
        if exiting:
            unixctl_server.close()
            sys.exit()
        poller = ovs.poller.Poller()
        unixctl_server.wait(poller)
        poller.block()


def _process_func(watcher, watcher_recycle_func):
    while True:
        try:
            watcher.process()
        except Exception as e:
            # Recycle watcher
            if not isinstance(e, exceptions.APIServerTimeout):
                vlog.exception("Failure in watcher %s"
                               % type(watcher).__name__)
            vlog.warn("Regenerating watcher because of \"%s\" and "
                      "reconnecting to stream using function %s"
                      % (str(e), watcher_recycle_func.__name__))
            watcher = watcher_recycle_func()


def _sync_k8s_pods():
    if config.get_option('ovn_mode') == "overlay":
        mode = ovn_k8s.modes.overlay.OvnNB()
    else:
        return

    try:
        pods = kubernetes.get_all_pods(variables.K8S_API_SERVER)
        if pods:
            mode.sync_pods(pods)
    except Exception as e:
        vlog.exception("failed in _sync_k8s_pods (%s)" % (str(e)))


def _sync_k8s_services():
    if config.get_option('ovn_mode') == "overlay":
        mode = ovn_k8s.modes.overlay.OvnNB()
    else:
        return

    try:
        services = kubernetes.get_all_services(variables.K8S_API_SERVER)
        if services:
            mode.sync_services(services)
    except Exception as e:
        vlog.exception("failed in _sync_k8s_services (%s)" % (str(e)))


def _create_k8s_pod_watcher():
    pod_stream = kubernetes.watch_pods(variables.K8S_API_SERVER)
    _sync_k8s_pods()
    watcher = pod_watcher.PodWatcher(pod_stream)
    return watcher


def _create_k8s_service_watcher():
    service_stream = kubernetes.watch_services(variables.K8S_API_SERVER)
    _sync_k8s_services()
    watcher = service_watcher.ServiceWatcher(service_stream)
    return watcher


def _create_k8s_endpoint_watcher():
    endpoint_stream = kubernetes.watch_endpoints(variables.K8S_API_SERVER)
    watcher = endpoint_watcher.EndpointWatcher(endpoint_stream)
    return watcher


def start_threads():
    pool = greenpool.GreenPool()
    pool.spawn(_unixctl_run)
    pool.spawn(conn_processor.run_processor)

    pod_watcher_inst = _create_k8s_pod_watcher()
    service_watcher_inst = _create_k8s_service_watcher()
    endpoint_watcher_inst = _create_k8s_endpoint_watcher()

    pool.spawn(_process_func, pod_watcher_inst, _create_k8s_pod_watcher)
    pool.spawn(_process_func, service_watcher_inst,
               _create_k8s_service_watcher)
    pool.spawn(_process_func, endpoint_watcher_inst,
               _create_k8s_endpoint_watcher)

    pool.waitall()
