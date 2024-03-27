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

package util

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/coreos/butane/config/common"
	"github.com/coreos/butane/translate"

	"github.com/clarketm/json"
	ignvalidate "github.com/coreos/ignition/v2/config/validate"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
	"github.com/coreos/vcontext/tree"
	"github.com/coreos/vcontext/validate"
	vyaml "github.com/coreos/vcontext/yaml"
	"gopkg.in/yaml.v3"
)

var (
	snakeRe = regexp.MustCompile("([A-Z])")
)

// Misc helpers

// Translate translates cfg to the corresponding Ignition config version
// using the named translation method on cfg, and returns the marshaled
// Ignition config.  It returns a report of any errors or warnings in the
// source and resultant config.  If the report has fatal errors or it
// encounters other problems translating, an error is returned.
func Translate(cfg interface{}, translateMethod string, options common.TranslateOptions) (interface{}, report.Report, error) {
	// Get method, and zero return value for error returns.
	method := reflect.ValueOf(cfg).MethodByName(translateMethod)
	zeroValue := reflect.Zero(method.Type().Out(0)).Interface()

	// Validate the input.
	r := validate.Validate(cfg, "yaml")
	if r.IsFatal() {
		return zeroValue, r, common.ErrInvalidSourceConfig
	}

	// Perform the translation.
	translateRet := method.Call([]reflect.Value{reflect.ValueOf(options)})
	final := translateRet[0].Interface()
	translations := translateRet[1].Interface().(translate.TranslationSet)
	translateReport := translateRet[2].Interface().(report.Report)
	r.Merge(translateReport)
	if r.IsFatal() {
		return zeroValue, r, common.ErrInvalidSourceConfig
	}
	if options.DebugPrintTranslations {
		fmt.Fprint(os.Stderr, translations)
		if err := translations.DebugVerifyCoverage(final); err != nil {
			fmt.Fprintf(os.Stderr, "\n%s", err)
		}
	}

	// Check for invalid duplicated keys.
	dupsReport := validate.ValidateCustom(final, "json", ignvalidate.ValidateDups)
	r.Merge(TranslateReportPaths(dupsReport, translations))

	// Validate JSON semantics.
	jsonReport := validate.Validate(final, "json")
	r.Merge(TranslateReportPaths(jsonReport, translations))

	if r.IsFatal() {
		return zeroValue, r, common.ErrInvalidGeneratedConfig
	}
	return final, r, nil
}

// TranslateBytes unmarshals the Butane config specified in input into the
// struct pointed to by container, translates it to the corresponding Ignition
// config version using the named translation method, and returns the
// marshaled Ignition config.  It returns a report of any errors or warnings
// in the source and resultant config.  If the report has fatal errors or it
// encounters other problems translating, an error is returned.
func TranslateBytes(input []byte, container interface{}, translateMethod string, options common.TranslateBytesOptions) ([]byte, report.Report, error) {
	cfg := container

	// Unmarshal the YAML.
	contextTree, err := unmarshal(input, cfg)
	if err != nil {
		return nil, report.Report{}, err
	}

	// Check for unused keys.
	unusedKeyCheck := func(v reflect.Value, c path.ContextPath) report.Report {
		return ignvalidate.ValidateUnusedKeys(v, c, contextTree)
	}
	r := validate.ValidateCustom(cfg, "yaml", unusedKeyCheck)
	r.Correlate(contextTree)
	if r.IsFatal() {
		return nil, r, common.ErrInvalidSourceConfig
	}

	// Perform the translation.
	translateRet := reflect.ValueOf(cfg).MethodByName(translateMethod).Call([]reflect.Value{reflect.ValueOf(options.TranslateOptions)})
	final := translateRet[0].Interface()
	translateReport := translateRet[1].Interface().(report.Report)
	errVal := translateRet[2]
	translateReport.Correlate(contextTree)
	r.Merge(translateReport)
	if !errVal.IsNil() {
		return nil, r, errVal.Interface().(error)
	}
	if r.IsFatal() {
		return nil, r, common.ErrInvalidSourceConfig
	}

	// Marshal the JSON.
	outbytes, err := marshal(final, options.Pretty)
	return outbytes, r, err
}

func TranslateBytesYAML(input []byte, container interface{}, translateMethod string, options common.TranslateBytesOptions) ([]byte, report.Report, error) {
	// marshal to JSON, unmarshal, remarshal to YAML.  there's no other
	// good way to respect the `json` struct tags.
	// https://github.com/go-yaml/yaml/issues/424
	jsonCfg, r, err := TranslateBytes(input, container, translateMethod, options)
	if err != nil {
		return jsonCfg, r, err
	}

	var ifaceCfg interface{}
	if err := json.Unmarshal(jsonCfg, &ifaceCfg); err != nil {
		return []byte{}, r, err
	}

	var yamlCfgBuf bytes.Buffer
	yamlCfgBuf.WriteString("# Generated by Butane; do not edit\n")
	encoder := yaml.NewEncoder(&yamlCfgBuf)
	encoder.SetIndent(2)
	if err := encoder.Encode(ifaceCfg); err != nil {
		return []byte{}, r, err
	}
	if err := encoder.Close(); err != nil {
		return []byte{}, r, err
	}
	yamlCfg := bytes.Trim(yamlCfgBuf.Bytes(), "\n")
	return yamlCfg, r, err
}

// Report an ErrFieldElided warning for any non-zero top-level fields in the
// specified output struct.  The caller will probably want to use
// translate.PrefixReport() to reparent the report into the right place in
// the `json` hierarchy, and then TranslateReportPaths() to map back into
// `yaml` space.
func CheckForElidedFields(struct_ interface{}) report.Report {
	v := reflect.ValueOf(struct_)
	t := v.Type()
	if t.Kind() != reflect.Struct {
		panic("struct type required")
	}
	var r report.Report
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.IsValid() && !f.IsZero() {
			tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
			r.AddOnWarn(path.New("json", tag), common.ErrFieldElided)
		}
	}
	return r
}

// unmarshal unmarshals the data to "to" and also returns a context tree for the source.
func unmarshal(data []byte, to interface{}) (tree.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(to); err != nil {
		return nil, err
	}
	return vyaml.UnmarshalToContext(data)
}

// marshal is a wrapper for marshaling to json with or without pretty-printing the output
func marshal(from interface{}, pretty bool) ([]byte, error) {
	if pretty {
		return json.MarshalIndent(from, "", "  ")
	}
	return json.Marshal(from)
}

// snakePath converts a path.ContextPath with camelCase elements and returns the
// same path but with snake_case elements instead
func snakePath(p path.ContextPath) path.ContextPath {
	ret := path.New(p.Tag)
	for _, part := range p.Path {
		if str, ok := part.(string); ok {
			ret = ret.Append(snake(str))
		} else {
			ret = ret.Append(part)
		}
	}
	return ret
}

// snake converts from camelCase (not CamelCase) to snake_case
func snake(in string) string {
	return strings.ToLower(snakeRe.ReplaceAllString(in, "_$1"))
}

// TranslateReportPaths takes a report from a camelCase json document and a set of translations rules,
// applies those rules and converts all camelCase to snake_case.
func TranslateReportPaths(r report.Report, ts translate.TranslationSet) report.Report {
	var ret report.Report
	ret.Merge(r)
	for i, ent := range ret.Entries {
		context := ent.Context
		if t, ok := ts.Set[context.String()]; ok {
			context = t.From
		} else {
			// Missing translation.  As a fallback, convert
			// camelCase path elements to snake_case and hope
			// there's a 1:1 mapping between the YAML and JSON
			// hierarchies.  Notably, that's not true for
			// MachineConfig output, since the Ignition config
			// is reparented to a grandchild of the root.
			// See also https://github.com/coreos/butane/issues/213.
			// This is hacky (notably, it leaves context.Tag as
			// `json`) but sometimes it's enough to help us find
			// a Marker, and when it isn't, the path still
			// provides some feedback to the user.
			context = snakePath(context)
		}
		ret.Entries[i].Context = context
	}
	return ret
}
