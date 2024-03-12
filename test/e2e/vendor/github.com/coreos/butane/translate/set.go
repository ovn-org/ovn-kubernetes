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
	"sort"
	"strings"

	"github.com/coreos/vcontext/path"
)

// Translation represents how a path changes when translating. If something at $yaml.storage.filesystems.4
// generates content at $json.systemd.units.3 a translation can represent that. This allows validation errors
// in Ignition structs to be tracked back to their source in the yaml.
type Translation struct {
	From path.ContextPath
	To   path.ContextPath
}

func (t Translation) String() string {
	return fmt.Sprintf("%s → %s", t.From, t.To)
}

// TranslationSet represents all of the translations that occurred. They're stored in a map from a string representation
// of the destination path to the translation struct. The map is purely an optimization to allow fast lookups. Ideally the
// map would just be from the destination path.ContextPath to the source path.ContextPath, but ContextPath contains a slice
// which are not comparable and thus cannot be used as keys in maps.
type TranslationSet struct {
	FromTag string
	ToTag   string
	Set     map[string]Translation
}

func NewTranslationSet(fromTag, toTag string) TranslationSet {
	return TranslationSet{
		FromTag: fromTag,
		ToTag:   toTag,
		Set:     map[string]Translation{},
	}
}

func (ts TranslationSet) String() string {
	type entry struct {
		sortKey   string
		formatted string
	}
	var entries []entry
	for k, v := range ts.Set {
		formatted := v.String()
		// lookup key should always match To path; report if it doesn't
		if k != v.To.String() {
			formatted += fmt.Sprintf(" (key: %s)", k)
		}
		entries = append(entries, entry{
			sortKey:   v.To.String(),
			formatted: formatted,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].sortKey < entries[j].sortKey
	})
	str := fmt.Sprintf("TranslationSet: %v → %v\n", ts.FromTag, ts.ToTag)
	for _, entry := range entries {
		str += entry.formatted + "\n"
	}
	return str
}

// AddTranslation adds a translation to the set
func (ts TranslationSet) AddTranslation(from, to path.ContextPath) {
	// create copies of the paths so if someone else changes from.Path the added translation does not change.
	from = from.Copy()
	to = to.Copy()
	translation := Translation{
		From: from,
		To:   to,
	}
	toString := translation.To.String()
	ts.Set[toString] = translation
}

// AddFromCommonSource adds translations for all of the paths in to from a single common path. This is useful
// if one part of a config generates a large struct and all of the large struct should map to one path in the
// config being translated.
func (ts TranslationSet) AddFromCommonSource(common path.ContextPath, toPrefix path.ContextPath, to interface{}) {
	v := reflect.ValueOf(to)
	vPaths := prefixPaths(getAllPaths(v, ts.ToTag, true), toPrefix.Path...)
	for _, toPath := range vPaths {
		ts.AddTranslation(common, toPath)
	}
	ts.AddTranslation(common, toPrefix)
}

// AddFromCommonObject adds translations for all of the paths in to. The paths being translated
// are prefixed by fromPrefix and the translated paths are prefixed by toPrefix.
// This is useful when we want to copy all the fields of an object to another with the same field names.
func (ts TranslationSet) AddFromCommonObject(fromPrefix path.ContextPath, toPrefix path.ContextPath, to interface{}) {
	vTo := reflect.ValueOf(to)
	vPaths := getAllPaths(vTo, ts.ToTag, true)

	for _, path := range vPaths {
		ts.AddTranslation(fromPrefix.Append(path.Path...), toPrefix.Append(path.Path...))
	}
	ts.AddTranslation(fromPrefix, toPrefix)
}

// Merge adds all the entries to the set. It mutates the Set in place.
func (ts TranslationSet) Merge(from TranslationSet) {
	for _, t := range from.Set {
		ts.AddTranslation(t.From, t.To)
	}
}

// MergeP is like Merge, but it adds a prefix to the set being merged in.
func (ts TranslationSet) MergeP(prefix interface{}, from TranslationSet) {
	ts.MergeP2(prefix, prefix, from)
}

// MergeP2 is like Merge, but it adds distinct prefixes to each side of the
// set being merged in.
func (ts TranslationSet) MergeP2(fromPrefix interface{}, toPrefix interface{}, from TranslationSet) {
	from = from.PrefixPaths(path.New(from.FromTag, fromPrefix), path.New(from.ToTag, toPrefix))
	ts.Merge(from)
}

// Prefix returns a TranslationSet with all translation paths prefixed by prefix.
func (ts TranslationSet) Prefix(prefix interface{}) TranslationSet {
	return ts.PrefixPaths(path.New(ts.FromTag, prefix), path.New(ts.ToTag, prefix))
}

// PrefixPaths returns a TranslationSet with from translation paths prefixed by
// fromPrefix and to translation paths prefixed by toPrefix.
func (ts TranslationSet) PrefixPaths(fromPrefix, toPrefix path.ContextPath) TranslationSet {
	ret := NewTranslationSet(ts.FromTag, ts.ToTag)
	for _, tr := range ts.Set {
		ret.AddTranslation(fromPrefix.Append(tr.From.Path...), toPrefix.Append(tr.To.Path...))
	}
	return ret
}

// Descend returns the subtree of translations rooted at the specified To path.
func (ts TranslationSet) Descend(to path.ContextPath) TranslationSet {
	ret := NewTranslationSet(ts.FromTag, ts.ToTag)
OUTER:
	for _, tr := range ts.Set {
		if len(tr.To.Path) < len(to.Path) {
			// can't be in the requested subtree; skip
			continue
		}
		for i, e := range to.Path {
			if tr.To.Path[i] != e {
				// not in the requested subtree; skip
				continue OUTER
			}
		}
		subtreePath := path.New(tr.To.Tag, tr.To.Path[len(to.Path):]...)
		ret.AddTranslation(tr.From, subtreePath)
	}
	return ret
}

// Map returns a new TranslationSet with To translation paths further
// translated through mappings.  Translations not listed in mappings are
// copied unmodified.
func (ts TranslationSet) Map(mappings TranslationSet) TranslationSet {
	if mappings.FromTag != ts.ToTag || mappings.ToTag != ts.ToTag {
		panic(fmt.Sprintf("mappings have incorrect tag; %q != %q || %q != %q", mappings.FromTag, ts.ToTag, mappings.ToTag, ts.ToTag))
	}
	ret := NewTranslationSet(ts.FromTag, ts.ToTag)
	ret.Merge(ts)
	for _, mapping := range mappings.Set {
		if t, ok := ret.Set[mapping.From.String()]; ok {
			delete(ret.Set, mapping.From.String())
			ret.AddTranslation(t.From, mapping.To)
		}
	}
	return ret
}

// DebugVerifyCoverage recursively checks whether every non-zero field in v
// has a translation.  If translations are missing, it returns a multi-line
// error listing them.
func (ts TranslationSet) DebugVerifyCoverage(v interface{}) error {
	var missingPaths []string
	for _, pathToCheck := range getAllPaths(reflect.ValueOf(v), ts.ToTag, false) {
		if _, ok := ts.Set[pathToCheck.String()]; !ok {
			missingPaths = append(missingPaths, pathToCheck.String())
		}
	}
	if len(missingPaths) > 0 {
		return fmt.Errorf("Missing paths in TranslationSet:\n%v\n", strings.Join(missingPaths, "\n"))
	}
	return nil
}
