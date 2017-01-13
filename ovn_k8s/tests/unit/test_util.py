import unittest

from ovn_k8s.common import util


class TestHasChanges(unittest.TestCase):

    def test_values_equal(self):
        old = {'a': {'a1': [1, 2, 3]}, 'b': ['b1', 'b2']}
        new = {'a': {'a1': [3, 1, 2]}, 'b': ['b2', 'b1']}
        self.assertEqual({}, util.has_changes(new, old))

    def test_new_value_empty(self):
        old = {'a': 1, 'b': 2}
        new = {}
        self.assertEqual(
            {'a': {'deleted': None},
             'b': {'deleted': None}},
            util.has_changes(new, old))

    def test_new_value_none(self):
        old = {'a': 1, 'b': 2}
        new = None
        self.assertEqual(
            {'new': new, 'old': old},
            util.has_changes(new, old))

    def test_old_value_empty(self):
        old = {}
        new = {'a': 1, 'b': 2}
        self.assertEqual(
            {'a': {'added': 1},
             'b': {'added': 2}},
            util.has_changes(new, old))

    def test_old_value_none(self):
        old = None
        new = {'a': 1, 'b': 2}
        self.assertEqual(
            {'new': new, 'old': old},
            util.has_changes(new, old))

    def test_primitive_attribute_added(self):
        old = {'a': 1}
        new = {'a': 1, 'b': 2}
        self.assertEqual({'b': {'added': 2}}, util.has_changes(new, old))

    def test_primitive_attribute_removed(self):
        old = {'a': 1, 'b': 2}
        new = {'a': 1}
        self.assertEqual({'b': {'deleted': None}},
                         util.has_changes(new, old))

    def test_primitive_attribute_changed(self):
        old = {'a': 1, 'b': 2}
        new = {'a': 1, 'b': 3}
        self.assertEqual({'b': {'new': 3, 'old': 2}},
                         util.has_changes(new, old))

    def test_primitive_attribute_multiple_changes(self):
        old = {'a': 1, 'b': 2}
        new = {'a': 0, 'b': 3}
        self.assertEqual({'a': {'new': 0, 'old': 1},
                          'b': {'new': 3, 'old': 2}},
                         util.has_changes(new, old))

    def test_list_attribute_item_added(self):
        old = {'a': [1]}
        new = {'a': [1, 2]}
        self.assertEqual({'a': {2: {'added': None}}},
                         util.has_changes(new, old))

    def test_list_attribute_item_removed(self):
        old = {'a': [1, 2]}
        new = {'a': [1]}
        self.assertEqual({'a': {2: {'deleted': None}}},
                         util.has_changes(new, old))

    def test_list_attribute_item_changed(self):
        old = {'a': [1, 2]}
        new = {'a': [1, 3]}
        self.assertEqual({'a': {2: {'deleted': None},
                                3: {'added': None}}},
                         util.has_changes(new, old))

    def test_list_attribute_multiple_changes(self):
        old = {'a': [1, 2]}
        new = {'a': [4, 3]}
        self.assertEqual({'a': {1: {'deleted': None},  2: {'deleted': None},
                                3: {'added': None}, 4: {'added': None}}},
                         util.has_changes(new, old))

    def test_dict_attribute_item_added(self):
        old = {'a': {'a1': 1}}
        new = {'a': {'a1': 1, 'b1': 1}}
        self.assertEqual({'a': {'b1': {'added': 1}}},
                         util.has_changes(new, old))

    def test_dict_attribute_item_removed(self):
        old = {'a': {'a1': 1, 'b1': 1}}
        new = {'a': {'a1': 1}}
        self.assertEqual({'a': {'b1': {'deleted': None}}},
                         util.has_changes(new, old))

    def test_dict_attribute_item_changed(self):
        old = {'a': {'a1': 1}}
        new = {'a': {'a1': 3}}
        self.assertEqual({'a': {'a1': {'new': 3, 'old': 1}}},
                         util.has_changes(new, old))

    def test_dict_attribute_multiple_changes(self):
        old = {'a': {'a1': 1, 'b1': 1}}
        new = {'a': {'a1': 3, 'c1': 1}}
        self.assertEqual({'a': {'a1': {'new': 3, 'old': 1},
                                'b1': {'deleted': None},
                                'c1': {'added': 1}}},
                         util.has_changes(new, old))

    def test_primitive_attribute_changed_nested(self):
        old = {}
        new = {}
        exp = {}
        for level in range(0, 9):
            old = {str(level): old if old else level}
            new = {str(level): new if new else level + 1}
            exp = {str(level): exp if exp else
                   {'new': level + 1, 'old': level}}
            self.assertEqual(exp, util.has_changes(new, old))
