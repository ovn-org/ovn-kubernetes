// Copyright 2019 Red Hat, Inc
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

package v0_1

import (
	baseutil "github.com/coreos/butane/base/util"
	"github.com/coreos/butane/config/common"

	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

func (f FileContents) Validate(c path.ContextPath) (r report.Report) {
	if f.Inline != nil && f.Source != nil {
		r.AddOnError(c.Append("inline"), common.ErrTooManyResourceSources)
	}
	return
}

func (d Directory) Validate(c path.ContextPath) (r report.Report) {
	if d.Mode != nil {
		r.AddOnWarn(c.Append("mode"), baseutil.CheckForDecimalMode(*d.Mode, true))
	}
	return
}

func (f File) Validate(c path.ContextPath) (r report.Report) {
	if f.Mode != nil {
		r.AddOnWarn(c.Append("mode"), baseutil.CheckForDecimalMode(*f.Mode, false))
	}
	return
}
