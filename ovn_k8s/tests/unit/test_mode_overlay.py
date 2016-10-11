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

import unittest

import mock

from ovn_k8s.modes import overlay

SAMPLE_POLICY_DATA = {
    'metadata': {
        'name': 'sample_policy',
        'namespace': 'meh_ns',
        'uid': 'some_uuid'},
    'spec': {
        'podSelector': {
            'matchLabels': {'app': 'meh_app'}},
        'ingress': [
            {'ports': [
                {'port': 80, 'protocol': 'TCP'}
             ],
             'from': [
                 {'podSelector': {
                     'matchLabels': {
                         'app': 'another_meh'}}}]},
            {'ports': [
                {'port': 22, 'protocol': 'TCP'}
             ],
             'from': [
                 {'podSelector': {
                     'matchLabels': {
                         'app': 'system'}}}]}]
    }
}


class TestOverlayMode(unittest.TestCase):

    def setUp(self):
        super(TestOverlayMode, self).setUp()
        # NOTE: overlay import the ovn_nbctl symbol from util, so patching has
        # to be done there. Patching the function in util won't work
        patcher = mock.patch.object(overlay, 'ovn_nbctl')
        self.mock_ovn_nbctl = patcher.start()
        self.addCleanup(self.mock_ovn_nbctl.stop)
        self.mode = overlay.OvnNB()

    def _test_remove_acls(self, default):
        self.mock_ovn_nbctl.side_effect = [
            'meh_1\n999\ndef\n%d' % overlay.DEFAULT_ACL_PRIORITY,
            'meh_ls',
            None,
            None]
        self.mode.remove_acls('meh_pod', default)
        exp_calls = [
            mock.call('--data=bare', '--no-heading',
                      '--columns=_uuid,priority',
                      'find', 'ACL', 'external_ids:pod_id=meh_pod'),
            mock.call('--data=bare', '--no-heading', '--columns=name',
                      'find', 'Logical_Switch', 'acls{>=}meh_1'),
            mock.call('remove', 'Logical_Switch', 'meh_ls', 'acls', 'meh_1')]
        if default:
            exp_calls.append(
                mock.call('remove', 'Logical_Switch',
                          'meh_ls', 'acls', 'def'))
        self.mock_ovn_nbctl.assert_has_calls(exp_calls, any_order=False)
        self.assertEqual(len(exp_calls), self.mock_ovn_nbctl.call_count)

    def test_remove_acls_no_default(self):
        self._test_remove_acls(False)

    def test_remove_acls_including_default(self):
        self._test_remove_acls(True)

    def test_remove_acls_no_acl(self):
        self.mock_ovn_nbctl.return_value = ""
        self.mode.remove_acls('meh')
        self.mock_ovn_nbctl.assert_called_once_with(
            '--data=bare', '--no-heading', '--columns=_uuid,priority',
            'find', 'ACL', 'external_ids:pod_id=meh')

    def test_create_acl(self):
        self.mock_ovn_nbctl.return_value = ""
        self.mode._create_acl('meh_pod', 'meh_ls', 'meh_lsp', 1000,
                              'meh_match', 'meh_action', meh_ext_id='meh_meh')
        exp_calls = [
            mock.call('--data=bare', '--no-heading', '--columns=_uuid',
                      'find', 'ACL', 'action=meh_action',
                      'direction=to-lport', 'priority=1000',
                      'match=meh_match', 'external_ids:lport_name=meh_lsp'),
            mock.call('--', '--id=@acl_id', 'create', 'ACL',
                      'action=meh_action', 'direction=to-lport',
                      'priority=1000', 'match=meh_match',
                      'external_ids:lport_name=meh_lsp', mock.ANY, mock.ANY,
                      '--', 'add', 'Logical_Switch', 'meh_ls',
                      'acls', '@acl_id')]
        self.mock_ovn_nbctl.assert_has_calls(exp_calls, any_order=False)
        self.assertEqual(len(exp_calls), self.mock_ovn_nbctl.call_count)

    def test_create_acl_already_exists(self):
        self.mock_ovn_nbctl.return_value = "meh_id"
        self.mode._create_acl('meh_pod', 'meh_ls', 'meh_lsp', 1000,
                              'meh_match', 'meh_action', meh_ext_id='meh_meh')
        self.mock_ovn_nbctl.assert_called_once_with(
            '--data=bare', '--no-heading', '--columns=_uuid', 'find', 'ACL',
            'action=meh_action', 'direction=to-lport', 'priority=1000',
            'match=meh_match', 'external_ids:lport_name=meh_lsp')

    def test_whitelist_pod_traffic(self):
        self.mock_ovn_nbctl.side_effect = [
            'meh_lsp_uuid', 'meh_ls', None, None]
        self.mode.whitelist_pod_traffic('meh_pod_id', 'meh_ns', 'meh_pod')
        exp_calls = [
            mock.call('--data=bare', '--no-heading',
                      '--columns=_uuid',
                      'find', 'logical_switch_port', 'name=meh_ns_meh_pod'),
            mock.call('--data=bare', '--no-heading',
                      '--columns=_uuid',
                      'find', 'logical_switch', 'ports{>=}meh_lsp_uuid'),
            mock.call('--data=bare', '--no-heading', '--columns=_uuid',
                      'find', 'ACL', 'action=allow-related',
                      'direction=to-lport',
                      'priority=%d' % overlay.DEFAULT_ALLOW_ACL_PRIORITY,
                      'match=outport\=\=\"meh_ns_meh_pod\"\ &&\ ip',
                      'external_ids:lport_name=meh_ns_meh_pod'),
            mock.call('--', '--id=@acl_id', 'create', 'ACL',
                      'action=allow-related', 'direction=to-lport',
                      'priority=%d' % overlay.DEFAULT_ALLOW_ACL_PRIORITY,
                      'match=outport\=\=\"meh_ns_meh_pod\"\ &&\ ip',
                      'external_ids:lport_name=meh_ns_meh_pod',
                      'external_ids:pod_id=meh_pod_id',
                      '--', 'add', 'Logical_Switch', 'meh_ls',
                      'acls', '@acl_id')]
        self.mock_ovn_nbctl.assert_has_calls(exp_calls, any_order=False)

    def test_create_acl_match_from_empty_ports_empty(self):
        match = self.mode._create_acl_match(
            'meh_lsp', overlay.PseudoACL(0, None, None, 'meh'))
        self.assertEqual(r'outport\=\=\"meh_lsp\"\ &&\ ip', match)
        match = self.mode._create_acl_match(
            'meh_lsp', overlay.PseudoACL(0, [], '*', 'meh'))
        self.assertEqual(r'outport\=\=\"meh_lsp\"\ &&\ ip', match)

    def test_create_acl_match_from_populated_ports_empty(self):
        match = self.mode._create_acl_match(
            'meh_lsp', overlay.PseudoACL(0, None, 'address_set(meh)', 'meh'))
        self.assertEqual(r'outport\=\=\"meh_lsp\"\ &&\ ip\ '
                         '&&\ ip4.src\=\=\{address_set(meh)\}', match)

    def test_create_acl_match_from_empty_ports_populated(self):
        match = self.mode._create_acl_match(
            'meh_lsp',
            overlay.PseudoACL(0, [('tcp', 80), ('tcp', 22)], None, 'meh'))
        self.assertEqual(r'outport\=\=\"meh_lsp\"\ &&\ ip\ '
                         '&&\ tcp.dst\=\=\{80\\,22\}', match)

    def test_create_acl_match_from_populated_ports_populated(self):
        match = self.mode._create_acl_match(
            'meh_lsp',
            overlay.PseudoACL(0, [('tcp', 80), ('tcp', 22)],
                              'address_set(meh)', 'meh'))
        self.assertEqual(r'outport\=\=\"meh_lsp\"\ &&\ ip\ '
                         '&&\ \(tcp.dst\=\=\{80\\,22\}\)\ '
                         '&&\ \(ip4.src\=\=\{address_set(meh)\}\)', match)

    def test_policy_pod_traffic(self):
        self.mock_ovn_nbctl.side_effect = [
            '', 'meh_lsp_uuid', 'meh_ls', None, None]
        pseudo_acls = [
            overlay.PseudoACL(100, [('tcp', 443), ('tcp', 8080)],
                              '*', 'allow-related'),
            overlay.PseudoACL(100, [('tcp', 22)], 'address_set(meh)',
                              'allow-related')]
        pod_data = {'metadata': {'name': 'meh_pod',
                                 'uid': 'meh_pod_id',
                                 'namespace': 'meh_ns'}}
        with mock.patch(
                'ovn_k8s.modes.overlay.OvnNB._create_acl') as mock_create_acl:
            self.mode._policy_pod_traffic(pod_data, pseudo_acls)
            self.assertEqual(2, mock_create_acl.call_count)
            mock_create_acl.assert_has_calls([
                mock.call('meh_pod_id', 'meh_ls', 'meh_ns_meh_pod', 100,
                          mock.ANY, 'allow-related'),
                mock.call('meh_pod_id', 'meh_ls', 'meh_ns_meh_pod', 100,
                          mock.ANY, 'allow-related')])

    def test_build_rule_address_set_name(self):
        rule = {
            'from': [{
                'podSelector': {
                    'matchLabels': {
                        'meh': 'something'
                    }}}
            ]}
        address_set_name = self.mode._build_rule_address_set_name(
            'meh_policy', 'meh_ns', rule)
        self.assertTrue(address_set_name.startswith('meh_policy_meh_ns'))

    def test_create_address_set(self):
        self.mock_ovn_nbctl.return_value = ''
        self.mode.create_address_set('meh_ns', SAMPLE_POLICY_DATA)
        self.assertEqual(4, self.mock_ovn_nbctl.call_count)
        self.mock_ovn_nbctl.assert_has_calls([
            mock.call('create', 'address_set', mock.ANY),
            mock.call('create', 'address_set', mock.ANY)], any_order=True)

    def test_destroy_address_set(self):
        self.mock_ovn_nbctl.side_effect = ['meh1', '', 'meh2', '']
        self.mode.destroy_address_set('meh_ns', SAMPLE_POLICY_DATA)
        self.assertEqual(4, self.mock_ovn_nbctl.call_count)
        self.mock_ovn_nbctl.assert_has_calls([
            mock.call('--data=bare', '--no-heading', '--columns=_uuid',
                      'find', 'address_set', mock.ANY),
            mock.call('destroy', 'address_set', mock.ANY),
            mock.call('--data=bare', '--no-heading', '--columns=_uuid',
                      'find', 'address_set', mock.ANY),
            mock.call('destroy', 'address_set', mock.ANY)], any_order=False)

    def test_add_to_address_set(self):
        self.mock_ovn_nbctl.return_value = ''
        self.mode.add_to_address_set('10.0.0.1', 'meh_ns', SAMPLE_POLICY_DATA)
        self.assertEqual(2, self.mock_ovn_nbctl.call_count)
        self.mock_ovn_nbctl.assert_has_calls([
            mock.call('--if-exists', 'add', 'address_set', mock.ANY,
                      'addresses', '10.0.0.1'),
            mock.call('--if-exists', 'add', 'address_set', mock.ANY,
                      'addresses', '10.0.0.1')])

    def test_remove_from_address_set(self):
        self.mock_ovn_nbctl.return_value = ''
        self.mode.remove_from_address_set('10.0.0.1', 'meh_ns',
                                          SAMPLE_POLICY_DATA)
        self.assertEqual(2, self.mock_ovn_nbctl.call_count)
        self.mock_ovn_nbctl.assert_has_calls([
            mock.call('--if-exists', 'remove', 'address_set', mock.ANY,
                      'addresses', '10.0.0.1'),
            mock.call('--if-exists', 'remove', 'address_set', mock.ANY,
                      'addresses', '10.0.0.1')])

    def test_build_pseudo_acl_no_rule(self):
        pass

    def test_build_pseudo_acl_with_rules(self):
        pass

    def test_find_policies_for_pod_without_labels(self):
        pass

    def test_find_policies_for_pod_without_matchlabel(self):
        pass

    def test_find_policies_for_pod_no_labels(self):
        pass

    def test_find_policies_for_pod_with_labels(self):
        pass

    def test_apply_pod_policy_acl(self):
        pass

    def test_create_pod_acls_isolated_namespace(self):
        pass

    def test_create_pod_acls_non_isolated_namespace(self):
        pass

    def test_pod_matches_from_clause(self):
        pass

    def test_pod_matches_from_clause_missing_selector(self):
        pass

    def test_pod_matches_from_clause_empty_selector(self):
        pass

    def test_pod_matches_from_clause_with_ns_selector(self):
        pass

    def test_pod_matches_from_clause_multiple_items(self):
        pass

    def test_add_pods_to_address_sets(self):
        pass

    def test_add_pods_to_policy_address_sets(self):
        pass

    def test_delete_pods_from_address_sets(self):
        pass
