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
	"net/url"

	"github.com/coreos/butane/config/common"
	"github.com/coreos/butane/translate"

	"github.com/coreos/ignition/v2/config/v3_0/types"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
	"github.com/vincent-petithory/dataurl"
)

// ToIgn3_0Unvalidated translates the config to an Ignition config. It also returns the set of translations
// it did so paths in the resultant config can be tracked back to their source in the source config.
// No config validation is performed on input or output.
func (c Config) ToIgn3_0Unvalidated(options common.TranslateOptions) (types.Config, translate.TranslationSet, report.Report) {
	ret := types.Config{}

	tr := translate.NewTranslator("yaml", "json", options)
	tr.AddCustomTranslator(translateIgnition)
	tr.AddCustomTranslator(translateFile)
	tr.AddCustomTranslator(translateDirectory)
	tr.AddCustomTranslator(translateLink)

	tm, r := translate.Prefixed(tr, "ignition", &c.Ignition, &ret.Ignition)
	tm.AddTranslation(path.New("yaml", "version"), path.New("json", "ignition", "version"))
	tm.AddTranslation(path.New("yaml", "ignition"), path.New("json", "ignition"))
	translate.MergeP(tr, tm, &r, "passwd", &c.Passwd, &ret.Passwd)
	translate.MergeP(tr, tm, &r, "storage", &c.Storage, &ret.Storage)
	translate.MergeP(tr, tm, &r, "systemd", &c.Systemd, &ret.Systemd)

	if r.IsFatal() {
		return types.Config{}, translate.TranslationSet{}, r
	}
	return ret, tm, r
}

func translateIgnition(from Ignition, options common.TranslateOptions) (to types.Ignition, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	to.Version = types.MaxVersion.String()
	tm, r = translate.Prefixed(tr, "config", &from.Config, &to.Config)
	translate.MergeP(tr, tm, &r, "security", &from.Security, &to.Security)
	translate.MergeP(tr, tm, &r, "timeouts", &from.Timeouts, &to.Timeouts)
	return
}

func translateFile(from File, options common.TranslateOptions) (to types.File, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	tr.AddCustomTranslator(translateFileContents)
	tm, r = translate.Prefixed(tr, "group", &from.Group, &to.Group)
	translate.MergeP(tr, tm, &r, "user", &from.User, &to.User)
	translate.MergeP(tr, tm, &r, "append", &from.Append, &to.Append)
	translate.MergeP(tr, tm, &r, "contents", &from.Contents, &to.Contents)
	translate.MergeP(tr, tm, &r, "overwrite", &from.Overwrite, &to.Overwrite)
	translate.MergeP(tr, tm, &r, "path", &from.Path, &to.Path)
	translate.MergeP(tr, tm, &r, "mode", &from.Mode, &to.Mode)
	return
}

func translateFileContents(from FileContents, options common.TranslateOptions) (to types.FileContents, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	tm, r = translate.Prefixed(tr, "verification", &from.Verification, &to.Verification)
	translate.MergeP(tr, tm, &r, "source", &from.Source, &to.Source)
	translate.MergeP(tr, tm, &r, "compression", &from.Compression, &to.Compression)
	if from.Inline != nil {
		src := (&url.URL{
			Scheme: "data",
			Opaque: "," + dataurl.EscapeString(*from.Inline),
		}).String()
		to.Source = &src
		tm.AddTranslation(path.New("yaml", "inline"), path.New("json", "source"))
	}
	return
}

func translateDirectory(from Directory, options common.TranslateOptions) (to types.Directory, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	tm, r = translate.Prefixed(tr, "group", &from.Group, &to.Group)
	translate.MergeP(tr, tm, &r, "user", &from.User, &to.User)
	translate.MergeP(tr, tm, &r, "overwrite", &from.Overwrite, &to.Overwrite)
	translate.MergeP(tr, tm, &r, "path", &from.Path, &to.Path)
	translate.MergeP(tr, tm, &r, "mode", &from.Mode, &to.Mode)
	return
}

func translateLink(from Link, options common.TranslateOptions) (to types.Link, tm translate.TranslationSet, r report.Report) {
	tr := translate.NewTranslator("yaml", "json", options)
	tm, r = translate.Prefixed(tr, "group", &from.Group, &to.Group)
	translate.MergeP(tr, tm, &r, "user", &from.User, &to.User)
	translate.MergeP(tr, tm, &r, "target", &from.Target, &to.Target)
	translate.MergeP(tr, tm, &r, "hard", &from.Hard, &to.Hard)
	translate.MergeP(tr, tm, &r, "overwrite", &from.Overwrite, &to.Overwrite)
	translate.MergeP(tr, tm, &r, "path", &from.Path, &to.Path)
	return
}
