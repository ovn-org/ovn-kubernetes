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
import random
import six
import subprocess

import ovs.vlog
from ovn_k8s.common import variables

vlog = ovs.vlog.Vlog("util")


def call_popen(cmd):
    child = subprocess.Popen(cmd, stdout=subprocess.PIPE)
    output = child.communicate()
    if child.returncode:
        raise RuntimeError("Fatal error executing %s" % (cmd))
    if len(output) == 0 or output[0] is None:
        output = ""
    else:
        output = output[0].strip()
    return output


def call_prog(prog, args_list):
    cmd = [prog, "--timeout=5", "-vconsole:off"] + args_list
    return call_popen(cmd)


def ovs_vsctl(*args):
    return call_prog("ovs-vsctl", list(args))


def ovs_ofctl(*args):
    return call_prog("ovs-ofctl", list(args))


def ovn_nbctl(*args):
    args_list = list(args)
    database_option = "%s=%s" % ("--db", variables.OVN_NB)
    args_list.insert(0, database_option)
    return call_prog("ovn-nbctl", args_list)


def generate_mac(prefix="00:00:00"):
    random.seed()
    mac = "%s:%02X:%02X:%02X" % (
        prefix,
        random.randint(0, 255),
        random.randint(0, 255),
        random.randint(0, 255))
    return mac


def process_stream(data_stream, event_callback):
    # StopIteration should be caught in the routine that sets up the stream
    # and reconnects it
    line = next(data_stream)
    if not line:
        return

    try:
        event_callback(json.loads(line))
    except ValueError:
        vlog.warn("Invalid JSON data from response stream:%s" % line)


def _scalar_diff(old_value, new_value):
    if old_value != new_value:
        return {'old': old_value, 'new': new_value}


def has_changes(new_value, old_value):
    """Detect changes in an object, assumed to be accessible as a dict.

    :param new_value: current state of the object
    :param old_value: previous state of the object
    :returns: a dict describing changes to the object
    """
    # Special case for strings. Treating them as lists is just unnecessary
    # complexity
    if (isinstance(new_value, six.string_types) and
            isinstance(old_value, six.string_types)):
        return _scalar_diff(old_value, new_value)

    # Check if we're dealing with a dict
    try:
        new_value_dict = new_value
        old_value_dict = old_value
        new_value = new_value.items()
        old_value = old_value.items()
        old_value_copy = old_value_dict.copy()
        is_dict = True
    except AttributeError:
        # not a dict, maybe it's iterable anyway
        is_dict = False
        try:
            old_value_copy = list(old_value[:])
        except TypeError:
            # if it's not iterabile, then it must be scalar. Or at least we
            # can consider it as a scalar.
            return _scalar_diff(old_value_dict, new_value_dict)

    compare_result = {}
    try:
        for new_item in new_value:
            if is_dict:
                # Leverage O(1) search in dicts
                try:
                    old_item = old_value_copy[new_item[0]]
                    ret_value = has_changes(
                        new_item[1], old_item)
                    if ret_value:
                        compare_result[new_item[0]] = ret_value
                    del old_value_copy[new_item[0]]
                except KeyError:
                    compare_result[new_item[0]] = {'added': new_item[1]}
            else:
                found = None
                for old_item in old_value:
                    ret_value = has_changes(new_item, old_item)
                    if not ret_value:
                        found = old_item
                        break
                if found is None:
                    compare_result[new_item] = {'added': None}
                else:
                    old_value_copy.remove(found)
        # Any item left in old_value_copy at this stage has been removed from
        # new_value
        for item in old_value_copy:
            compare_result[item] = {'deleted': None}
    except TypeError:
        # If we end up here either the old or new value, or both, are not
        # iterable. Do a scalar comparison.
        return _scalar_diff(old_value, new_value)

    return compare_result
