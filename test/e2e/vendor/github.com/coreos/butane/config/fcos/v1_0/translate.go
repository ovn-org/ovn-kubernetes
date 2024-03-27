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

package v1_0

import (
	"github.com/coreos/butane/config/common"
	cutil "github.com/coreos/butane/config/util"
	"github.com/coreos/butane/translate"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/coreos/ignition/v2/config/v3_0/types"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

// ToIgn3_0Unvalidated translates the config to an Ignition config. It also
// returns the set of translations it did so paths in the resultant config
// can be tracked back to their source in the source config.  No config
// validation is performed on input or output.
func (c Config) ToIgn3_0Unvalidated(options common.TranslateOptions) (types.Config, translate.TranslationSet, report.Report) {
	ret, ts, r := c.Config.ToIgn3_0Unvalidated(options)
	if r.IsFatal() {
		return types.Config{}, translate.TranslationSet{}, r
	}

	for i, disk := range ret.Storage.Disks {
		// Don't warn if wipeTable is set, matching later spec versions
		if !util.IsTrue(disk.WipeTable) {
			for j, partition := range disk.Partitions {
				// check for reserved partlabels
				if partition.Label != nil {
					if (*partition.Label == "BIOS-BOOT" && partition.Number != 1) || (*partition.Label == "PowerPC-PReP-boot" && partition.Number != 1) || (*partition.Label == "EFI-SYSTEM" && partition.Number != 2) || (*partition.Label == "boot" && partition.Number != 3) || (*partition.Label == "root" && partition.Number != 4) {
						r.AddOnWarn(path.New("json", "storage", "disks", i, "partitions", j, "label"), common.ErrWrongPartitionNumber)
					}
				}
			}
		}
	}
	return ret, ts, r
}

// ToIgn3_0 translates the config to an Ignition config.  It returns a
// report of any errors or warnings in the source and resultant config.  If
// the report has fatal errors or it encounters other problems translating,
// an error is returned.
func (c Config) ToIgn3_0(options common.TranslateOptions) (types.Config, report.Report, error) {
	cfg, r, err := cutil.Translate(c, "ToIgn3_0Unvalidated", options)
	return cfg.(types.Config), r, err
}

// ToIgn3_0Bytes translates from a v1.0 Butane config to a v3.0.0 Ignition config. It returns a report of any errors or
// warnings in the source and resultant config. If the report has fatal errors or it encounters other problems
// translating, an error is returned.
func ToIgn3_0Bytes(input []byte, options common.TranslateBytesOptions) ([]byte, report.Report, error) {
	return cutil.TranslateBytes(input, &Config{}, "ToIgn3_0", options)
}
