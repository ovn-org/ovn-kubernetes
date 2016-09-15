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

from os import path
from ovn_k8s.common import exceptions
from ovn_k8s.common.util import ovs_vsctl

CA_CERTIFICATE = "/usr/share/openvswitch/k8s-ca.crt"
vlog = ovs.vlog.Vlog("kubernetes")


def _get_api_params():
    k8s_api_server = ovs_vsctl("--if-exists", "get", "Open_vSwitch", ".",
                               "external_ids:k8s-api-server").strip('"')
    if "https" in k8s_api_server:
        k8s_ca_certificate = ovs_vsctl("--if-exists", "get", "Open_vSwitch",
                                       ".", "external_ids:k8s-ca-certificate"
                                       ).strip('"')
        if k8s_ca_certificate:
            k8s_ca_certificate = k8s_ca_certificate.replace("\\n", "\n")
            if not path.isfile(CA_CERTIFICATE):
                ca_file = open(CA_CERTIFICATE, 'w+')
                ca_file.write(k8s_ca_certificate)
            ca_cert = CA_CERTIFICATE
        else:
            ca_cert = None

    k8s_api_token = ovs_vsctl("--if-exists", "get", "Open_vSwitch", ".",
                              "external_ids:k8s-api-token").strip('"')
    if k8s_api_token:
        token = k8s_api_token
    else:
        token = None

    return ca_cert, token


def _stream_api(url):
    ca_certificate, api_token = _get_api_params()
    headers = {}
    if api_token:
        headers.update({'Authorization': 'Bearer %s' % api_token})

    if ca_certificate:
        response = requests.get(url, headers=headers,
                                verify=ca_certificate, stream=True)
    else:
        response = requests.get(url, headers=headers, stream=True)

    if response.status_code != 200:
        # TODO: raise here
        return
    return response.iter_lines(chunk_size=10, delimiter='\n')


def _watch_resource(server, resource):
    url = "%s/api/v1/%s?watch=true" % (server, resource)
    return _stream_api(url)


def watch_pods(server):
    return _watch_resource(server, 'pods')


def watch_services(server):
    return _watch_resource(server, 'services')


def watch_endpoints(server):
    return _watch_resource(server, 'endpoints')


def get_pod_annotations(server, namespace, pod):
    ca_certificate, api_token = _get_api_params()
    url = ("%s/api/v1/namespaces/%s/pods/%s" %
           (server, namespace, pod))

    headers = {}
    if api_token:
        headers.update({'Authorization': 'Bearer %s' % api_token})

    if ca_certificate:
        response = requests.get(url, headers=headers, verify=ca_certificate)
    else:
        response = requests.get(url, headers=headers)
    if not response or response.status_code != 200:
        # TODO: raise here
        return
    json_response = response.json()
    annotations = json_response['metadata'].get('annotations')
    vlog.dbg("Annotations for pod %s: %s" % (pod, annotations))
    return annotations


def set_pod_annotation(server, namespace, pod, key, value):
    ca_certificate, api_token = _get_api_params()
    url = ("%s/api/v1/namespaces/%s/pods/%s" %
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

    headers = {'Content-Type': 'application/merge-patch+json'}
    if api_token:
        headers.update({'Authorization': 'Bearer %s' % api_token})
    if ca_certificate:
        response = requests.patch(
            url,
            data=json.dumps(patch),
            headers=headers,
            verify=ca_certificate)
    else:
        response = requests.patch(
            url,
            data=json.dumps(patch),
            headers=headers)

    if not response or response.status_code != 200:
        # TODO: Raise appropriate exception
        raise Exception("Something went wrong while annotating pod: %s" %
                        response.text)
    json_response = response.json()
    annotations = json_response['metadata'].get('annotations')
    vlog.dbg("Annotations for pod after update %s: %s" % (pod, annotations))
    return annotations


def _get_objects(url, namespace, resource_type, resource_id):
    ca_certificate, api_token = _get_api_params()

    headers = {}
    if api_token:
        headers.update({'Authorization': 'Bearer %s' % api_token})
    if ca_certificate:
        response = requests.get(url, headers=headers, verify=ca_certificate)
    else:
        response = requests.get(url, headers=headers)

    if not response:
        if response.status_code == 404:
            raise exceptions.NotFound(resource_type=resource_type,
                                      resource_id=resource_id)
        else:
            raise Exception("Failed to fetch %s:%s in namespace %s (%d) :%s"
                            % (resource_type, resource_id, namespace,
                               response.status_code, response.text))

    return response.json()


def get_service(server, namespace, service):
    url = "%s/api/v1/namespaces/%s/services/%s" \
            % (server, namespace, service)
    return _get_objects(url, namespace, 'service', service)


def get_all_pods(server):
    url = "http://%s/api/v1/pods" % (server)
    return _get_objects(url, 'all', 'pod', "all_pods")


def get_all_services(server):
    url = "http://%s/api/v1/services" % (server)
    return _get_objects(url, 'all', 'service', "all_services")
