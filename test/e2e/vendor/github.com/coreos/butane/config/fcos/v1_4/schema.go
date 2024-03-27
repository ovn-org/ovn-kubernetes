// Copyright 2020 Red Hat, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.)

package v1_4

import (
	base "github.com/coreos/butane/base/v0_4"
)

type Config struct {
	base.Config `yaml:",inline"`
	BootDevice  BootDevice `yaml:"boot_device"`
}

type BootDevice struct {
	Layout *string          `yaml:"layout"`
	Luks   BootDeviceLuks   `yaml:"luks"`
	Mirror BootDeviceMirror `yaml:"mirror"`
}

type BootDeviceLuks struct {
	Tang      []base.Tang `yaml:"tang"`
	Threshold *int        `yaml:"threshold"`
	Tpm2      *bool       `yaml:"tpm2"`
}

type BootDeviceMirror struct {
	Devices []string `yaml:"devices"`
}
