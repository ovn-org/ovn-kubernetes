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
import requests

import ovs.vlog

from ovn_k8s.common import exceptions

vlog = ovs.vlog.Vlog("kubernetes")


def _stream_api(url):
    # TODO: HTTPS and authentication
    response = requests.get(url, stream=True)
    if response.status_code != 200:
        # TODO: raise here
        return
    return response.iter_lines(chunk_size=10, delimiter='\n')


def _watch_resource(server, resource):
    url = "http://%s/api/v1/%s?watch=true" % (server, resource)
    return _stream_api(url)


def watch_pods(server):
    return _watch_resource(server, 'pods')


def watch_services(server):
    return _watch_resource(server, 'services')


def watch_endpoints(server):
    return _watch_resource(server, 'endpoints')


def get_pod_annotations(server, namespace, pod):
    url = ("http://%s/api/v1/namespaces/%s/pods/%s" %
           (server, namespace, pod))
    response = requests.get(url)
    if not response or response.status_code != 200:
        # TODO: raise here
        return
    json_response = response.json()
    annotations = json_response['metadata'].get('annotations')
    vlog.dbg("Annotations for pod %s: %s" % (pod, annotations))
    return annotations


def set_pod_annotation(server, namespace, pod, key, value):
    url = ("http://%s/api/v1/namespaces/%s/pods/%s" %
           (server, namespace, pod))
    # NOTE: This is not probably compliant with RFC 7386 but appears to work
    # with the kubernetes API server.
    patch = {
        'metadata': {
            'annotations': {
                key: value
            }
        }
    }
    response = requests.patch(
        url,
        data=json.dumps(patch),
        headers={'Content-Type': 'application/merge-patch+json'})
    if not response or response.status_code != 200:
        # TODO: Raise appropriate exception
        raise Exception("Something went wrong while annotating pod: %s" %
                        response.text)
    json_response = response.json()
    annotations = json_response['metadata'].get('annotations')
    vlog.dbg("Annotations for pod after update %s: %s" % (pod, annotations))
    return annotations


def get_service(server, namespace, service):
    url = ("http://%s/api/v1/namespaces/%s/services/%s"
           % (server, namespace, service))
    response = requests.get(url)
    if not response:
        if response.status_code == 404:
            raise exceptions.NotFound(resource_type='service',
                                      resource_id=service)
        else:
            raise Exception("Failed to fetch service (%d) :%s" % (
                response.status_code, response.text))

    return response.json()
