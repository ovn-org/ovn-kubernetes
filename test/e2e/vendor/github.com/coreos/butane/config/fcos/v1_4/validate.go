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
	"github.com/coreos/butane/config/common"

	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

func (d BootDevice) Validate(c path.ContextPath) (r report.Report) {
	if d.Layout != nil {
		switch *d.Layout {
		case "aarch64", "ppc64le", "x86_64":
		default:
			r.AddOnError(c.Append("layout"), common.ErrUnknownBootDeviceLayout)
		}
	}
	r.Merge(d.Mirror.Validate(c.Append("mirror")))
	return
}

func (m BootDeviceMirror) Validate(c path.ContextPath) (r report.Report) {
	if len(m.Devices) == 1 {
		r.AddOnError(c.Append("devices"), common.ErrTooFewMirrorDevices)
	}
	return
}
