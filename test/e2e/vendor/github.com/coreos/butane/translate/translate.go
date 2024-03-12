// Copyright 2019 Red Hat, Inc.
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
// limitations under the License.

package translate

import (
	"fmt"
	"reflect"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/coreos/vcontext/path"
	"github.com/coreos/vcontext/report"
)

/*
 * This is an automatic translator that replace boilerplate code to copy one
 * struct into a nearly identical struct in another package. To use it first
 * call NewTranslator() to get a translator instance. This can then have
 * additional translation rules (in the form of functions) to translate from
 * types in one struct to the other. Those functions are in the form:
 *     func(fromType, optionsType) -> (toType, TranslationSet, report.Report)
 * These can be closures that reference the translator as well. This allows for
 * manually translating some fields but resuming automatic translation on the
 * other fields through the Translator.Translate() function.
 */

const (
	TAG_KEY       = "butane"
	TAG_AUTO_SKIP = "auto_skip"
)

var (
	translationsType = reflect.TypeOf(TranslationSet{})
	reportType       = reflect.TypeOf(report.Report{})
)

// Returns if this type can be translated without a custom translator. Children or other
// ancestors might require custom translators however
func (t translator) translatable(t1, t2 reflect.Type) bool {
	k1 := t1.Kind()
	k2 := t2.Kind()
	if k1 != k2 {
		return false
	}
	switch {
	case util.IsPrimitive(k1):
		return true
	case util.IsInvalidInConfig(k1):
		panic(fmt.Sprintf("Encountered invalid kind %s in config. This is a bug, please file a report", k1))
	case k1 == reflect.Ptr || k1 == reflect.Slice:
		return t.translatable(t1.Elem(), t2.Elem()) || t.hasTranslator(t1.Elem(), t2.Elem())
	case k1 == reflect.Struct:
		return t.translatableStruct(t1, t2)
	default:
		panic(fmt.Sprintf("Encountered unknown kind %s in config. This is a bug, please file a report", k1))
	}
}

// precondition: t1, t2 are both of Kind 'struct'
func (t translator) translatableStruct(t1, t2 reflect.Type) bool {
	if t1.Name() != t2.Name() {
		return false
	}
	t1Fields := 0
	for i := 0; i < t1.NumField(); i++ {
		t1f := t1.Field(i)
		if t1f.Tag.Get(TAG_KEY) == TAG_AUTO_SKIP {
			// ignore this input field
			continue
		}
		t1Fields++

		t2f, ok := t2.FieldByName(t1f.Name)
		if !ok {
			return false
		}
		if !t.translatable(t1f.Type, t2f.Type) && !t.hasTranslator(t1f.Type, t2f.Type) {
			return false
		}
	}
	return t2.NumField() == t1Fields
}

// checks that t could reasonably be the type of a translator function
func (t translator) couldBeValidTranslator(tr reflect.Type) bool {
	if tr.Kind() != reflect.Func {
		return false
	}
	if tr.NumIn() != 2 || tr.NumOut() != 3 {
		return false
	}
	if util.IsInvalidInConfig(tr.In(0).Kind()) ||
		util.IsInvalidInConfig(tr.Out(0).Kind()) ||
		tr.In(1) != reflect.TypeOf(t.options) ||
		tr.Out(1) != translationsType ||
		tr.Out(2) != reportType {
		return false
	}
	return true
}

// translate from one type to another, but deep copy all data
// precondition: vFrom and vTo are the same type as defined by translatable()
// precondition: vTo is addressable and settable
func (t translator) translateSameType(vFrom, vTo reflect.Value, fromPath, toPath path.ContextPath) {
	k := vFrom.Kind()
	switch {
	case util.IsPrimitive(k):
		// Use convert, even if not needed; type alias to primitives are not
		// directly assignable and calling Convert on primitives does no harm
		vTo.Set(vFrom.Convert(vTo.Type()))
		t.translations.AddTranslation(fromPath, toPath)
	case k == reflect.Ptr:
		if vFrom.IsNil() {
			return
		}
		vTo.Set(reflect.New(vTo.Type().Elem()))
		t.translate(vFrom.Elem(), vTo.Elem(), fromPath, toPath)
	case k == reflect.Slice:
		if vFrom.IsNil() {
			return
		}
		vTo.Set(reflect.MakeSlice(vTo.Type(), vFrom.Len(), vFrom.Len()))
		for i := 0; i < vFrom.Len(); i++ {
			t.translate(vFrom.Index(i), vTo.Index(i), fromPath.Append(i), toPath.Append(i))
		}
		t.translations.AddTranslation(fromPath, toPath)
	case k == reflect.Struct:
		for i := 0; i < vFrom.NumField(); i++ {
			if vFrom.Type().Field(i).Tag.Get(TAG_KEY) == TAG_AUTO_SKIP {
				// ignore this input field
				continue
			}
			fieldGoName := vFrom.Type().Field(i).Name
			toStructField, ok := vTo.Type().FieldByName(fieldGoName)
			if !ok {
				panic("vTo did not have a matching type. This is a bug; please file a report")
			}
			toFieldIndex := toStructField.Index[0]
			vToField := vTo.FieldByName(fieldGoName)

			from := fromPath.Append(fieldName(vFrom, i, fromPath.Tag))
			to := toPath.Append(fieldName(vTo, toFieldIndex, toPath.Tag))
			if vFrom.Type().Field(i).Anonymous {
				from = fromPath
				to = toPath
			}
			t.translate(vFrom.Field(i), vToField, from, to)
		}
		if !vFrom.IsZero() {
			t.translations.AddTranslation(fromPath, toPath)
		}
	default:
		panic("Encountered types that are not the same when they should be. This is a bug, please file a report")
	}
}

// helper to return if a custom translator was defined
func (t translator) hasTranslator(tFrom, tTo reflect.Type) bool {
	return t.getTranslator(tFrom, tTo).IsValid()
}

// vTo must be addressable, should be acquired by calling reflect.ValueOf() on a variable of the correct type
func (t translator) translate(vFrom, vTo reflect.Value, fromPath, toPath path.ContextPath) {
	tFrom := vFrom.Type()
	tTo := vTo.Type()
	if fnv := t.getTranslator(tFrom, tTo); fnv.IsValid() {
		returns := fnv.Call([]reflect.Value{vFrom, reflect.ValueOf(t.options)})
		vTo.Set(returns[0])

		// handle all the translations and "rebase" them to our current place
		retSet := returns[1].Interface().(TranslationSet)
		t.translations.Merge(retSet.PrefixPaths(fromPath, toPath))
		if len(retSet.Set) > 0 {
			t.translations.AddTranslation(fromPath, toPath)
		}

		// likewise for the report entries
		retReport := returns[2].Interface().(report.Report)
		for i := range retReport.Entries {
			entry := &retReport.Entries[i]
			entry.Context = fromPath.Append(entry.Context.Path...)
		}
		t.report.Merge(retReport)
		return
	}
	if t.translatable(tFrom, tTo) {
		t.translateSameType(vFrom, vTo, fromPath, toPath)
		return
	}

	panic(fmt.Sprintf("Translator not defined for %v to %v", tFrom, tTo))
}

type Translator interface {
	// Adds a custom translator for cases where the structs are not identical. Must be of type
	// func(fromType, optionsType) -> (toType, TranslationSet, report.Report).
	// The translator should return the set of all translations it did.
	AddCustomTranslator(t interface{})
	// Also returns a list of source and dest paths, autocompleted by fromTag and toTag
	Translate(from, to interface{}) (TranslationSet, report.Report)
}

// NewTranslator creates a new Translator for translating from types with fromTag struct tags (e.g. "yaml")
// to types with toTag struct tages (e.g. "json"). These tags are used when determining paths when generating
// the TranslationSet returned by Translator.Translate()
func NewTranslator(fromTag, toTag string, options interface{}) Translator {
	return &translator{
		options: options,
		translations: TranslationSet{
			FromTag: fromTag,
			ToTag:   toTag,
			Set:     map[string]Translation{},
		},
	}
}

type translator struct {
	options interface{}
	// List of custom translation funcs, must pass couldBeValidTranslator
	// This is only for fields that cannot or should not be trivially translated,
	// All trivially translated fields use the default behavior.
	translators  []reflect.Value
	translations TranslationSet
	report       *report.Report
}

// fn should be of the form
// func(fromType, optionsType) -> (toType, TranslationSet, report.Report)
func (t *translator) AddCustomTranslator(fn interface{}) {
	fnv := reflect.ValueOf(fn)
	if !t.couldBeValidTranslator(fnv.Type()) {
		panic("Tried to register invalid translator function")
	}
	t.translators = append(t.translators, fnv)
}

func (t translator) getTranslator(from, to reflect.Type) reflect.Value {
	for _, fn := range t.translators {
		if fn.Type().In(0) == from && fn.Type().Out(0) == to {
			return fn
		}
	}
	return reflect.Value{}
}

// Translate translates from into to and returns a set of all the path changes it performed.
func (t translator) Translate(from, to interface{}) (TranslationSet, report.Report) {
	fv := reflect.ValueOf(from)
	tv := reflect.ValueOf(to)
	if fv.Kind() != reflect.Ptr || tv.Kind() != reflect.Ptr {
		panic("Translate needs to be called on pointers")
	}
	fv = fv.Elem()
	tv = tv.Elem()
	// Make sure to clear these every time
	t.translations = TranslationSet{
		FromTag: t.translations.FromTag,
		ToTag:   t.translations.ToTag,
		Set:     map[string]Translation{},
	}
	t.report = &report.Report{}
	t.translate(fv, tv, path.New(t.translations.FromTag), path.New(t.translations.ToTag))
	return t.translations, *t.report
}
