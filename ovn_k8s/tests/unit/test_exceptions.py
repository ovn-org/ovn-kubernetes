# Copyright (C) 2017 Nicira, Inc.
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

from ovn_k8s.common import exceptions


class TestExceptions(unittest.TestCase):

    def test_notfound(self):
        try:
            raise exceptions.NotFound(resource_type='meh_type',
                                      resource_id='meh_id')
        except exceptions.NotFound as e:
            test_str = 'meh_type meh_id not found'
            self.assertEqual(test_str, e.message)
            self.assertEqual(test_str, str(e))
