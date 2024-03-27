// Copyright 2021 Red Hat, Inc
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

package result

import (
	"github.com/coreos/ignition/v2/config/v3_5_experimental/types"
)

const (
	MC_API_VERSION = "machineconfiguration.openshift.io/v1"
	MC_KIND        = "MachineConfig"
)

// We round-trip through JSON because Ignition uses `json` struct tags,
// so all struct tags need to be `json` even though we're ultimately
// writing YAML.

type MachineConfig struct {
	ApiVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type Spec struct {
	Config          types.Config `json:"config"`
	KernelArguments []string     `json:"kernelArguments,omitempty"`
	Extensions      []string     `json:"extensions,omitempty"`
	FIPS            *bool        `json:"fips,omitempty"`
	KernelType      *string      `json:"kernelType,omitempty"`
}
