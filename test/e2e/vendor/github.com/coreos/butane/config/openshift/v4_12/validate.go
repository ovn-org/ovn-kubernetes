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

package v4_12

import (
	"github.com/coreos/butane/config/common"

	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

func (m Metadata) Validate(c path.ContextPath) (r report.Report) {
	if m.Name == "" {
		r.AddOnError(c.Append("name"), common.ErrNameRequired)
	}
	if m.Labels[ROLE_LABEL_KEY] == "" {
		r.AddOnError(c.Append("labels", ROLE_LABEL_KEY), common.ErrRoleRequired)
	}
	return
}

func (os OpenShift) Validate(c path.ContextPath) (r report.Report) {
	if os.KernelType != nil {
		switch *os.KernelType {
		case "", "default", "realtime":
		default:
			r.AddOnError(c.Append("kernel_type"), common.ErrInvalidKernelType)
		}
	}
	return
}
