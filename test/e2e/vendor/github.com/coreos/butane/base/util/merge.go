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

package util

import (
	"fmt"

	"github.com/coreos/butane/translate"

	"github.com/coreos/ignition/v2/config/merge"
)

// MergeTranslatedConfigs merges a parent and child config and returns the
// result.  It also generates and returns the merged TranslationSet by
// mapping the parent/child TranslationSets through the merge transcript.
func MergeTranslatedConfigs(parent interface{}, parentTranslations translate.TranslationSet, child interface{}, childTranslations translate.TranslationSet) (interface{}, translate.TranslationSet) {
	// mappings:
	//   left:  parent or child translate.TranslationSet
	//   right: merge.Transcript

	// merge configs
	result, right := merge.MergeStructTranscribe(parent, child)

	// merge left and right mappings into new TranslationSet
	if parentTranslations.FromTag != childTranslations.FromTag || parentTranslations.ToTag != childTranslations.ToTag {
		panic(fmt.Sprintf("mismatched translation tags, %s != %s || %s != %s", parentTranslations.FromTag, childTranslations.FromTag, parentTranslations.ToTag, childTranslations.ToTag))
	}
	ts := translate.NewTranslationSet(parentTranslations.FromTag, parentTranslations.ToTag)
	for _, rightEntry := range right.Mappings {
		var left *translate.TranslationSet
		switch rightEntry.From.Tag {
		case merge.TAG_PARENT:
			left = &parentTranslations
		case merge.TAG_CHILD:
			left = &childTranslations
		default:
			panic("unexpected mapping tag " + rightEntry.From.Tag)
		}
		leftEntry, ok := left.Set[rightEntry.From.String()]
		if !ok {
			// the right mapping is more comprehensive than the
			// left mapping
			continue
		}
		if _, ok := ts.Set[rightEntry.To.String()]; ok && rightEntry.From.Tag != merge.TAG_CHILD {
			// For result fields which are produced by combining
			// the parent and child, there will be two
			// transcript entries, one for each side.  We want
			// to prefer the child because the parent is
			// probably a desugared config whose source is
			// textually unrelated to the result config.
			//
			// Currently, Ignition always reports parent before
			// child, but that isn't necessarily contractual, so
			// we don't assume it.  Here, we've found the second
			// entry and it's not from the child; skip it.
			continue
		}
		rightEntry.To.Tag = leftEntry.To.Tag
		ts.AddTranslation(leftEntry.From, rightEntry.To)
	}
	return result, ts
}
