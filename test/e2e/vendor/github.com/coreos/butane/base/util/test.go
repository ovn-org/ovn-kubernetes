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
	"testing"

	"github.com/coreos/butane/translate"
	"github.com/stretchr/testify/assert"
)

// helper functions for writing tests

// VerifyTranslations ensures all the translations are identity, unless they
// match a listed one, and verifies that all the listed ones exist.
func VerifyTranslations(t *testing.T, set translate.TranslationSet, exceptions []translate.Translation) {
	exceptionSet := translate.NewTranslationSet(set.FromTag, set.ToTag)
	for _, ex := range exceptions {
		exceptionSet.AddTranslation(ex.From, ex.To)
		if tr, ok := set.Set[ex.To.String()]; ok {
			assert.Equal(t, ex, tr, "non-identity translation with unexpected From")
		} else {
			t.Errorf("missing non-identity translation %v", ex)
		}
	}
	for key, translation := range set.Set {
		if _, ok := exceptionSet.Set[key]; !ok {
			assert.Equal(t, translation.From.Path, translation.To.Path, "translation is not identity")
		}
	}
}
