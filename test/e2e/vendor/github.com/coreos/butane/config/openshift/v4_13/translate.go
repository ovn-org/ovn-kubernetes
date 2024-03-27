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

package v4_13

import (
	"net/url"
	"reflect"
	"strings"

	"github.com/coreos/butane/config/common"
	"github.com/coreos/butane/config/openshift/v4_13/result"
	cutil "github.com/coreos/butane/config/util"
	"github.com/coreos/butane/translate"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

const (
	// FIPS 140-2 doesn't allow the default XTS mode
	fipsCipherOption      = types.LuksOption("--cipher")
	fipsCipherShortOption = types.LuksOption("-c")
	fipsCipherArgument    = types.LuksOption("aes-cbc-essiv:sha256")
)

// ToMachineConfig4_13Unvalidated translates the config to a MachineConfig.  It also
// returns the set of translations it did so paths in the resultant config
// can be tracked back to their source in the source config.  No config
// validation is performed on input or output.
func (c Config) ToMachineConfig4_13Unvalidated(options common.TranslateOptions) (result.MachineConfig, translate.TranslationSet, report.Report) {
	cfg, ts, r := c.Config.ToIgn3_2Unvalidated(options)
	if r.IsFatal() {
		return result.MachineConfig{}, ts, r
	}

	// wrap
	ts = ts.PrefixPaths(path.New("yaml"), path.New("json", "spec", "config"))
	mc := result.MachineConfig{
		ApiVersion: result.MC_API_VERSION,
		Kind:       result.MC_KIND,
		Metadata: result.Metadata{
			Name:   c.Metadata.Name,
			Labels: make(map[string]string),
		},
		Spec: result.Spec{
			Config: cfg,
		},
	}
	ts.AddTranslation(path.New("yaml", "version"), path.New("json", "apiVersion"))
	ts.AddTranslation(path.New("yaml", "version"), path.New("json", "kind"))
	ts.AddTranslation(path.New("yaml", "metadata"), path.New("json", "metadata"))
	ts.AddTranslation(path.New("yaml", "metadata", "name"), path.New("json", "metadata", "name"))
	ts.AddTranslation(path.New("yaml", "metadata", "labels"), path.New("json", "metadata", "labels"))
	ts.AddTranslation(path.New("yaml", "version"), path.New("json", "spec"))
	ts.AddTranslation(path.New("yaml"), path.New("json", "spec", "config"))
	for k, v := range c.Metadata.Labels {
		mc.Metadata.Labels[k] = v
		ts.AddTranslation(path.New("yaml", "metadata", "labels", k), path.New("json", "metadata", "labels", k))
	}

	// translate OpenShift fields
	tr := translate.NewTranslator("yaml", "json", options)
	from := &c.OpenShift
	to := &mc.Spec
	ts2, r2 := translate.Prefixed(tr, "extensions", &from.Extensions, &to.Extensions)
	translate.MergeP(tr, ts2, &r2, "fips", &from.FIPS, &to.FIPS)
	translate.MergeP2(tr, ts2, &r2, "kernel_arguments", &from.KernelArguments, "kernelArguments", &to.KernelArguments)
	translate.MergeP2(tr, ts2, &r2, "kernel_type", &from.KernelType, "kernelType", &to.KernelType)
	ts.MergeP2("openshift", "spec", ts2)
	r.Merge(r2)

	// apply FIPS options to LUKS volumes
	ts.Merge(addLuksFipsOptions(&mc))

	// finally, check the fully desugared config for RHCOS and MCO support
	r.Merge(validateRHCOSSupport(mc, ts))
	r.Merge(validateMCOSupport(mc, ts))

	return mc, ts, r
}

// ToMachineConfig4_13 translates the config to a MachineConfig.  It returns a
// report of any errors or warnings in the source and resultant config.  If
// the report has fatal errors or it encounters other problems translating,
// an error is returned.
func (c Config) ToMachineConfig4_13(options common.TranslateOptions) (result.MachineConfig, report.Report, error) {
	cfg, r, err := cutil.Translate(c, "ToMachineConfig4_13Unvalidated", options)
	return cfg.(result.MachineConfig), r, err
}

// ToIgn3_2Unvalidated translates the config to an Ignition config.  It also
// returns the set of translations it did so paths in the resultant config
// can be tracked back to their source in the source config.  No config
// validation is performed on input or output.
func (c Config) ToIgn3_2Unvalidated(options common.TranslateOptions) (types.Config, translate.TranslationSet, report.Report) {
	mc, ts, r := c.ToMachineConfig4_13Unvalidated(options)
	cfg := mc.Spec.Config

	// report warnings if there are any non-empty fields in Spec (other
	// than the Ignition config itself) that we're ignoring
	mc.Spec.Config = types.Config{}
	warnings := translate.PrefixReport(cutil.CheckForElidedFields(mc.Spec), "spec")
	// translate from json space into yaml space
	r.Merge(cutil.TranslateReportPaths(warnings, ts))

	ts = ts.Descend(path.New("json", "spec", "config"))
	return cfg, ts, r
}

// ToIgn3_2 translates the config to an Ignition config.  It returns a
// report of any errors or warnings in the source and resultant config.  If
// the report has fatal errors or it encounters other problems translating,
// an error is returned.
func (c Config) ToIgn3_2(options common.TranslateOptions) (types.Config, report.Report, error) {
	cfg, r, err := cutil.Translate(c, "ToIgn3_2Unvalidated", options)
	return cfg.(types.Config), r, err
}

// ToConfigBytes translates from a v4.13 Butane config to a v4.13 MachineConfig or a v3.2.0 Ignition config. It returns a report of any errors or
// warnings in the source and resultant config. If the report has fatal errors or it encounters other problems
// translating, an error is returned.
func ToConfigBytes(input []byte, options common.TranslateBytesOptions) ([]byte, report.Report, error) {
	if options.Raw {
		return cutil.TranslateBytes(input, &Config{}, "ToIgn3_2", options)
	} else {
		return cutil.TranslateBytesYAML(input, &Config{}, "ToMachineConfig4_13", options)
	}
}

func addLuksFipsOptions(mc *result.MachineConfig) translate.TranslationSet {
	ts := translate.NewTranslationSet("yaml", "json")
	if !util.IsTrue(mc.Spec.FIPS) {
		return ts
	}

OUTER:
	for i := range mc.Spec.Config.Storage.Luks {
		luks := &mc.Spec.Config.Storage.Luks[i]
		// Only add options if the user hasn't already specified
		// a cipher option.  Do this in-place, since config merging
		// doesn't support conditional logic.
		for _, option := range luks.Options {
			if option == fipsCipherOption ||
				strings.HasPrefix(string(option), string(fipsCipherOption)+"=") ||
				option == fipsCipherShortOption {
				continue OUTER
			}
		}
		for j := 0; j < 2; j++ {
			ts.AddTranslation(path.New("yaml", "openshift", "fips"), path.New("json", "spec", "config", "storage", "luks", i, "options", len(luks.Options)+j))
		}
		if len(luks.Options) == 0 {
			ts.AddTranslation(path.New("yaml", "openshift", "fips"), path.New("json", "spec", "config", "storage", "luks", i, "options"))
		}
		luks.Options = append(luks.Options, fipsCipherOption, fipsCipherArgument)
	}
	return ts
}

// Error on fields that are rejected by RHCOS.
//
// Some of these fields may have been generated by sugar (e.g.
// boot_device.luks), so we work in JSON (output) space and then translate
// paths back to YAML (input) space.  That's also the reason we do these
// checks after translation, rather than during validation.
func validateRHCOSSupport(mc result.MachineConfig, ts translate.TranslationSet) report.Report {
	var r report.Report
	for i, fs := range mc.Spec.Config.Storage.Filesystems {
		if fs.Format != nil && *fs.Format == "btrfs" {
			// we don't ship mkfs.btrfs
			r.AddOnError(path.New("json", "spec", "config", "storage", "filesystems", i, "format"), common.ErrBtrfsSupport)
		}
	}
	return cutil.TranslateReportPaths(r, ts)
}

// Error on fields that are rejected outright by the MCO, or that are
// unsupported by the MCO and we want to discourage.
//
// https://github.com/openshift/machine-config-operator/blob/d6dabadeca05/MachineConfigDaemon.md#supported-vs-unsupported-ignition-config-changes
//
// Some of these fields may have been generated by sugar (e.g. storage.trees),
// so we work in JSON (output) space and then translate paths back to YAML
// (input) space.  That's also the reason we do these checks after
// translation, rather than during validation.
func validateMCOSupport(mc result.MachineConfig, ts translate.TranslationSet) report.Report {
	// Error classes for the purposes of this function:
	//
	// UNPARSABLE - Cannot be rendered into a config by the MCC.  If
	// present in MC, MCC will mark the pool degraded.  We reject these.
	//
	// FORBIDDEN - Not supported by the MCD.  If present in MC, MCD will
	// mark the node degraded.  We reject these.
	//
	// IMMUTABLE - Permitted in MC, passed through to Ignition, but not
	// supported by the MCD.  MCD will mark the node degraded if the
	// field changes after the node is provisioned.  We reject these
	// outright to discourage their use.
	//
	// TRIPWIRE - A subset of fields in the containing struct are
	// supported by the MCD.  If the struct contents change after the node
	// is provisioned, and the struct contains unsupported fields, MCD
	// will mark the node degraded, even if the change only affects
	// supported fields.  We reject these.

	var r report.Report
	for i := range mc.Spec.Config.Storage.Directories {
		// IMMUTABLE
		r.AddOnError(path.New("json", "spec", "config", "storage", "directories", i), common.ErrDirectorySupport)
	}
	for i, file := range mc.Spec.Config.Storage.Files {
		if len(file.Append) > 0 {
			// FORBIDDEN
			r.AddOnError(path.New("json", "spec", "config", "storage", "files", i, "append"), common.ErrFileAppendSupport)
		}
		if file.Contents.Source != nil {
			fileSource, err := url.Parse(*file.Contents.Source)
			// parse errors will be caught by normal config validation
			if err == nil && fileSource.Scheme != "data" {
				// FORBIDDEN
				r.AddOnError(path.New("json", "spec", "config", "storage", "files", i, "contents", "source"), common.ErrFileSchemeSupport)
			}
		}
		if file.Mode != nil && *file.Mode & ^0777 != 0 {
			// UNPARSABLE
			r.AddOnError(path.New("json", "spec", "config", "storage", "files", i, "mode"), common.ErrFileSpecialModeSupport)
		}
	}
	for i := range mc.Spec.Config.Storage.Links {
		// IMMUTABLE
		// If you change this to be less restrictive without adding
		// link support in the MCO, consider what should happen if
		// the user specifies a storage.tree that includes symlinks.
		r.AddOnError(path.New("json", "spec", "config", "storage", "links", i), common.ErrLinkSupport)
	}
	for i := range mc.Spec.Config.Passwd.Groups {
		// IMMUTABLE
		r.AddOnError(path.New("json", "spec", "config", "passwd", "groups", i), common.ErrGroupSupport)
	}
	for i, user := range mc.Spec.Config.Passwd.Users {
		if user.Name == "core" {
			// PasswordHash and SSHAuthorizedKeys are managed; other fields are not
			v := reflect.ValueOf(user)
			t := v.Type()
			for j := 0; j < v.NumField(); j++ {
				fv := v.Field(j)
				ft := t.Field(j)
				switch ft.Name {
				case "Name", "PasswordHash", "SSHAuthorizedKeys":
					continue
				default:
					if fv.IsValid() && !fv.IsZero() {
						tag := strings.Split(ft.Tag.Get("json"), ",")[0]
						// TRIPWIRE
						r.AddOnError(path.New("json", "spec", "config", "passwd", "users", i, tag), common.ErrUserFieldSupport)
					}
				}
			}
		} else {
			// TRIPWIRE
			r.AddOnError(path.New("json", "spec", "config", "passwd", "users", i), common.ErrUserNameSupport)
		}
	}
	return cutil.TranslateReportPaths(r, ts)
}
