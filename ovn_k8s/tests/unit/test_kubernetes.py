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
import unittest

import mock

from ovn_k8s.common import exceptions
from ovn_k8s.common import kubernetes


class MockResponse(object):

    def __init__(self, data, status_code=200):
        self.text = data
        self.status_code = status_code

    def json(self):
        return json.loads(self.text)

    def __nonzero__(self):
        return self.status_code == 200

    def __bool__(self):
        return self.status_code == 200


class TestKubernetes(unittest.TestCase):

    def setUp(self):
        super(TestKubernetes, self).setUp()
        patcher = mock.patch('ovn_k8s.common.kubernetes._get_api_params')
        self.mock_get_api_params = patcher.start()
        self.mock_get_api_params.return_value = (None, None)
        self.addCleanup(self.mock_get_api_params.stop)
        self.exp_headers = {}

    def _verify_mock_call(self, req_mock, url, **params):
        req_mock.assert_called_once_with(url, **params)

    def _test_watch(self, resource, watch_func):
        with mock.patch('requests.get') as req_mock:
            watch_func('http://meh:8080')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/%s?watch=true' % resource,
                headers=self.exp_headers,
                stream=True)

    def test_watch_pods(self):
        self._test_watch('pods', kubernetes.watch_pods)

    def test_watch_services(self):
        self._test_watch('services', kubernetes.watch_services)

    def test_watch_endpoints(self):
        self._test_watch('services', kubernetes.watch_services)

    def _test_get_pod_annotations(self, annotations):
        if annotations:
            ann_data = {'annotations': annotations}
        else:
            ann_data = annotations

        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse(
                '{"metadata": %s}' % json.dumps(ann_data or {}))
            ann = kubernetes.get_pod_annotations('http://meh:8080',
                                                 'meh_ns',
                                                 'meh_pod')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods/meh_pod',
                headers=self.exp_headers)
            self.assertEqual(annotations, ann)

    def test_get_pod_annotations(self):
        self._test_get_pod_annotations({'foo': 'bar'})

    def test_get_pod_annotations_empty(self):
        self._test_get_pod_annotations(None)

    def test_get_pod_annotation_failure(self):
        # NOTE: this method currently returns None in case of failure. No info
        # about the failure is returned. This might change in the future.
        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse("Error", status_code=404)
            ann = kubernetes.get_pod_annotations('http://meh:8080',
                                                 'meh_ns',
                                                 'meh_pod')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods/meh_pod',
                headers=self.exp_headers)
            self.assertIsNone(ann)

    def test_set_pod_annotation(self):
        with mock.patch('requests.patch') as req_mock:
            self.exp_headers['Content-Type'] = 'application/merge-patch+json'
            patch = '{"metadata": {"annotations": {"foo": "bar"}}}'
            req_mock.return_value = MockResponse(patch)
            ret_value = kubernetes.set_pod_annotation(
                'http://meh:8080', 'meh_ns', 'meh_pod', 'foo', 'bar')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods/meh_pod',
                data=patch, headers=self.exp_headers)
            self.assertEqual({'foo': 'bar'}, ret_value)

    def test_set_pod_annotation_failure(self):
        with mock.patch('requests.patch') as req_mock:
            self.exp_headers['Content-Type'] = 'application/merge-patch+json'
            patch = '{"metadata": {"annotations": {"foo": "bar"}}}'
            req_mock.return_value = MockResponse("Error", status_code=500)
            self.assertRaises(Exception,
                              kubernetes.set_pod_annotation,
                              'http://meh:8080', 'meh_ns',
                              'meh_pod', 'foo', 'bar')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods/meh_pod',
                data=patch, headers=self.exp_headers)

    def _test_get_object(self, namespace, resource, resource_id, get_func,
                         response_code=200, expected_exc=None):
        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse("{}", response_code)
            if not expected_exc:
                get_func('http://meh:8080', namespace, resource_id)
                self._verify_mock_call(
                    req_mock,
                    'http://meh:8080/api/v1/namespaces/%s/%s/%s' % (
                        namespace, resource, resource_id),
                    headers=self.exp_headers)
            else:
                self.assertRaises(expected_exc, get_func,
                                  'http://meh:8080', namespace, resource_id)

    def test_get_service(self):
        self._test_get_object('meh_ns', 'services', 'meh_service',
                              kubernetes.get_service)

    def test_get_non_existent_service_raises_NotFound(self):
        self._test_get_object('meh_ns', 'services', 'meh_service',
                              kubernetes.get_service, response_code=404,
                              expected_exc=exceptions.NotFound)

    def test_get_service_failure_raises_Exception(self):
        self._test_get_object('meh_ns', 'services', 'meh_service',
                              kubernetes.get_service, response_code=500,
                              expected_exc=Exception)

    def _test_get_all_objects(self, resource, get_func):
        with mock.patch('requests.get') as req_mock:
            get_func('http://meh:8080')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/%s' % resource,
                headers=self.exp_headers)

    def test_get_all_services(self):
        self._test_get_all_objects('services', kubernetes.get_all_services)

    def test_get_all_pods(self):
        self._test_get_all_objects('pods', kubernetes.get_all_pods)


class TestKubernetesAuth(TestKubernetes):

    def setUp(self):
        # It is a bit weird that unit tests still use http protocol, but as
        # calls to requests are mocked this is fine
        super(TestKubernetesAuth, self).setUp()
        api_token = 'of light'
        self.mock_get_api_params.return_value = (None, api_token)
        self.exp_headers['Authorization'] = 'Bearer %s' % api_token


class TestKubernetesAuthCertificate(TestKubernetesAuth):

    ca_cert = 'certified'

    def _verify_mock_call(self, req_mock, url, **params):
        params['verify'] = self.ca_cert
        req_mock.assert_called_once_with(url, **params)

    def setUp(self):
        # It is a bit weird that unit tests still use http protocol, but as
        # calls to requests are mocked this is fine
        super(TestKubernetesAuth, self).setUp()
        self.mock_get_api_params.return_value = (
            self.ca_cert, self.mock_get_api_params.return_value[1])
