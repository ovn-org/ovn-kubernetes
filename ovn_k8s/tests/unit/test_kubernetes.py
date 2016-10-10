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

    @classmethod
    def ok_from_dict(cls, data):
        return cls(json.dumps(data))


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

    def test_watch_namespaces(self):
        self._test_watch('namespaces', kubernetes.watch_namespaces)

    def test_watch_network_policies(self):
        with mock.patch('requests.get') as req_mock:
            kubernetes.watch_network_policies('http://meh:8080', 'meh_ns')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/apis/extensions/v1beta1/'
                'namespaces/meh_ns/networkpolicies?watch=true',
                headers=self.exp_headers,
                stream=True)

    def _test_get_annotations(self, annotations, namespace, get_func,
                              resource=None, resource_id=None):
        if annotations:
            ann_data = {'annotations': annotations}
        else:
            ann_data = annotations

        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse(
                '{"metadata": %s}' % json.dumps(ann_data or {}))
            if resource:
                ann = get_func('http://meh:8080', namespace, resource_id)
                self._verify_mock_call(
                    req_mock,
                    'http://meh:8080/api/v1/namespaces/%s/%s/%s' % (
                        namespace, resource, resource_id),
                    headers=self.exp_headers)
            else:
                ann = get_func('http://meh:8080', namespace)
                self._verify_mock_call(
                    req_mock,
                    'http://meh:8080/api/v1/namespaces/%s' % namespace,
                    headers=self.exp_headers)

            self.assertEqual(annotations, ann)

    def _test_get_pod_annotations(self, annotations):
        self._test_get_annotations(annotations, 'meh_ns',
                                   kubernetes.get_pod_annotations,
                                   'pods', 'meh_pod')

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

    def _test_get_ns_annotations(self, annotations):
        self._test_get_annotations(annotations, 'meh_ns',
                                   kubernetes.get_ns_annotations)

    def test_get_ns_annotations(self):
        self._test_get_ns_annotations({'foo': 'bar'})

    def test_get_ns_annotations_empty(self):
        self._test_get_ns_annotations(None)

    def test_get_ns_annotation_failure(self):
        # NOTE: this method currently returns None in case of failure. No info
        # about the failure is returned. This might change in the future.
        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse("Error", status_code=404)
            ann = kubernetes.get_ns_annotations('http://meh:8080',
                                                'meh_ns')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns',
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

    def _test_get_namespace(self, namespace, response_code=200,
                            expected_exc=None):
        with mock.patch('requests.get') as req_mock:
            req_mock.return_value = MockResponse("{}", response_code)
            if not expected_exc:
                kubernetes.get_namespace('http://meh:8080', namespace)
                self._verify_mock_call(
                    req_mock,
                    'http://meh:8080/api/v1/namespaces/%s' % namespace,
                    headers=self.exp_headers)
            else:
                self.assertRaises(expected_exc, kubernetes.get_namespace,
                                  'http://meh:8080', namespace)

    def test_get_namespace(self):
        self._test_get_namespace('meh_ns')

    def test_get_non_existent_namespace_raises_NotFound(self):
        self._test_get_namespace('meh_ns', response_code=404,
                                 expected_exc=exceptions.NotFound)

    def test_get_namespace_failure_raises_Exception(self):
        self._test_get_namespace('meh_ns', response_code=500,
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

    def test_get_pods_by_namespacse(self):
        with mock.patch('requests.get') as req_mock:
            kubernetes.get_pods_by_namespace('http://meh:8080', 'meh_ns')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods',
                headers=self.exp_headers,
                params={})

    def test_get_pods_by_namespace_with_selector(self):
        pod_selector = {'meh': ['maybe', 'dunno']}
        with mock.patch('requests.get') as req_mock:
            kubernetes.get_pods_by_namespace('http://meh:8080', 'meh_ns',
                                             pod_selector=pod_selector)
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/api/v1/namespaces/meh_ns/pods',
                headers=self.exp_headers,
                params={'labelSelector': ['meh in (maybe,dunno)']})

    def test_get_network_policies(self):
        with mock.patch('requests.get') as req_mock:
            kubernetes.get_network_policies('http://meh:8080', 'meh_ns')
            self._verify_mock_call(
                req_mock,
                'http://meh:8080/apis/extensions/v1beta1/namespaces/'
                'meh_ns/networkpolicies',
                headers=self.exp_headers)

    def test_is_namespace_isolated_no_annotations(self):
        with mock.patch(
                'ovn_k8s.common.kubernetes.get_ns_annotations') as req_mock:
            req_mock.return_value = None
            self.assertFalse(kubernetes.is_namespace_isolated(
                'http://meh:8080', 'meh'))

    def test_is_namespace_isolated_no_isolation_annotation(self):
        with mock.patch(
                'ovn_k8s.common.kubernetes.get_ns_annotations') as req_mock:
            req_mock.return_value = {'some_ann': 'some_value'}
            self.assertFalse(kubernetes.is_namespace_isolated(
                'http://meh:8080', 'meh'))

    def test_is_namespace_isolated_ingress_default_deny(self):
        with mock.patch(
                'ovn_k8s.common.kubernetes.get_ns_annotations') as req_mock:
            req_mock.return_value = {
                kubernetes.K8S_ISOLATION_ANN:
                json.dumps({'ingress': {'isolation': 'DefaultDeny'}})}
            self.assertTrue(kubernetes.is_namespace_isolated(
                'http://meh:8080', 'meh'))

    def test_is_namespace_isolated_ingress_other(self):
        with mock.patch(
                'ovn_k8s.common.kubernetes.get_ns_annotations') as req_mock:
            req_mock.return_value = {
                kubernetes.K8S_ISOLATION_ANN:
                json.dumps({'ingress': {'isolation': 'meh'}})}
            self.assertFalse(kubernetes.is_namespace_isolated(
                'http://meh:8080', 'meh'))


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
