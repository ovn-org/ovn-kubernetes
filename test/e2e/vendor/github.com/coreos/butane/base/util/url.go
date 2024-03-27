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
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"net/url"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/vincent-petithory/dataurl"
)

func MakeDataURL(contents []byte, currentCompression *string, allowCompression bool) (uri string, compression *string, err error) {
	// try three different encodings, and select the smallest one

	if util.NilOrEmpty(currentCompression) {
		// The config does not specify compression.  We need to
		// explicitly set the compression field to avoid a child
		// config inheriting a compression setting from the parent,
		// which may not have used the same compression algorithm.
		compression = util.StrToPtr("")
	} else {
		// The config specifies compression, meaning that the
		// contents were compressed by the user, so we can pick a
		// data URL encoding but we can't compress again.  Return a
		// nil compression value so the caller knows not to record a
		// translation from input contents to output compression.
		compression = nil
	}

	// URL-escaped, useful for ASCII text
	opaque := "," + dataurl.Escape(contents)

	// Base64-encoded, useful for small or incompressible binary data
	b64 := ";base64," + base64.StdEncoding.EncodeToString(contents)
	if len(b64) < len(opaque) {
		opaque = b64
	}

	// Base64-encoded gzipped, useful for compressible data.  If the
	// user already enabled compression, don't compress again.
	// We don't try base64-encoded URL-escaped because gzipped data is
	// binary and URL escaping is unlikely to be efficient.
	if util.NilOrEmpty(currentCompression) && allowCompression {
		var buf bytes.Buffer
		var compressor *gzip.Writer
		if compressor, err = gzip.NewWriterLevel(&buf, gzip.BestCompression); err != nil {
			return
		}
		if _, err = compressor.Write(contents); err != nil {
			return
		}
		if err = compressor.Close(); err != nil {
			return
		}
		gz := ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
		// Account for space needed by the compression value
		if len(gz)+len("gzip") < len(opaque) {
			opaque = gz
			compression = util.StrToPtr("gzip")
		}
	}

	uri = (&url.URL{
		Scheme: "data",
		Opaque: opaque,
	}).String()
	return
}
