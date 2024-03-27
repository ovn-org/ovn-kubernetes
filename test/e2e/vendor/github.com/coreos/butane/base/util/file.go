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
	"os"
	"path/filepath"
	"strings"

	"github.com/coreos/butane/config/common"
)

func EnsurePathWithinFilesDir(path, filesDir string) error {
	absBase, err := filepath.Abs(filesDir)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if absPath != absBase && !strings.HasPrefix(absPath, absBase+string(filepath.Separator)) {
		return common.ErrFilesDirEscape
	}
	return nil
}

func ReadLocalFile(configPath, filesDir string) ([]byte, error) {
	if filesDir == "" {
		// a files dir isn't configured; refuse to read anything
		return nil, common.ErrNoFilesDir
	}
	// calculate file path within FilesDir and check for path traversal
	filePath := filepath.Join(filesDir, filepath.FromSlash(configPath))
	if err := EnsurePathWithinFilesDir(filePath, filesDir); err != nil {
		return nil, err
	}
	return os.ReadFile(filePath)
}

// CheckForDecimalMode fails if the specified mode appears to have been
// incorrectly specified in decimal instead of octal.
func CheckForDecimalMode(mode int, directory bool) error {
	correctedMode, ok := decimalModeToOctal(mode)
	if !ok {
		return nil
	}
	if !isTypicalMode(mode, directory) && isTypicalMode(correctedMode, directory) {
		return common.ErrDecimalMode
	}
	return nil
}

// isTypicalMode returns true if the specified mode is unsurprising.
// It returns false for some modes that are unusual but valid in limited
// cases.
func isTypicalMode(mode int, directory bool) bool {
	// no permissions is always reasonable (root ignores mode bits)
	if mode == 0 {
		return true
	}

	// test user/group/other in reverse order
	perms := []int{mode & 0007, (mode & 0070) >> 3, (mode & 0700) >> 6}
	hadR := false
	hadW := false
	hadX := false
	for _, perm := range perms {
		r := perm&4 != 0
		w := perm&2 != 0
		x := perm&1 != 0
		// more-specific perm must have all the bits of less-specific
		// perm (r--rw----)
		if !r && hadR || !w && hadW || !x && hadX {
			return false
		}
		// if we have executable permission, it's weird for a
		// less-specific perm to have read but not execute (rwxr-----)
		if x && hadR && !hadX {
			return false
		}
		// -w- and --x are reasonable in special cases but they're
		// uncommon
		if (w || x) && !r {
			return false
		}
		hadR = hadR || r
		hadW = hadW || w
		hadX = hadX || x
	}

	// must be readable by someone
	if !hadR {
		return false
	}

	if directory {
		// must be executable by someone
		if !hadX {
			return false
		}
		// setuid forbidden
		if mode&04000 != 0 {
			return false
		}
		// setgid or sticky must be writable to someone
		if mode&03000 != 0 && !hadW {
			return false
		}
	} else {
		// setuid or setgid
		if mode&06000 != 0 {
			// must be executable to someone
			if !hadX {
				return false
			}
			// world-writable permission is a bad idea
			if mode&2 != 0 {
				return false
			}
		}
		// sticky forbidden
		if mode&01000 != 0 {
			return false
		}
	}

	return true
}

// decimalModeToOctal takes a mode written in decimal and converts it to
// octal, returning (0, false) on failure.
func decimalModeToOctal(mode int) (int, bool) {
	if mode < 0 || mode > 7777 {
		// out of range
		return 0, false
	}
	ret := 0
	for divisor := 1000; divisor > 0; divisor /= 10 {
		digit := (mode / divisor) % 10
		if digit > 7 {
			// digit not available in octal
			return 0, false
		}
		ret = (ret << 3) | digit
	}
	return ret, true
}
