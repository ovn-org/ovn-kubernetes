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

package v0_2

import (
	"github.com/coreos/ignition/v2/config/v3_1/types"
)

type nodeTracker struct {
	files   *[]types.File
	fileMap map[string]int

	dirs   *[]types.Directory
	dirMap map[string]int

	links   *[]types.Link
	linkMap map[string]int
}

func newNodeTracker(c *types.Config) *nodeTracker {
	t := nodeTracker{
		files:   &c.Storage.Files,
		fileMap: make(map[string]int, len(c.Storage.Files)),

		dirs:   &c.Storage.Directories,
		dirMap: make(map[string]int, len(c.Storage.Directories)),

		links:   &c.Storage.Links,
		linkMap: make(map[string]int, len(c.Storage.Links)),
	}
	for i, n := range *t.files {
		t.fileMap[n.Path] = i
	}
	for i, n := range *t.dirs {
		t.dirMap[n.Path] = i
	}
	for i, n := range *t.links {
		t.linkMap[n.Path] = i
	}
	return &t
}

func (t *nodeTracker) Exists(path string) bool {
	for _, m := range []map[string]int{t.fileMap, t.dirMap, t.linkMap} {
		if _, ok := m[path]; ok {
			return true
		}
	}
	return false
}

func (t *nodeTracker) GetFile(path string) (int, *types.File) {
	if i, ok := t.fileMap[path]; ok {
		return i, &(*t.files)[i]
	} else {
		return 0, nil
	}
}

func (t *nodeTracker) AddFile(f types.File) (int, *types.File) {
	if f.Path == "" {
		panic("File path missing")
	}
	if _, ok := t.fileMap[f.Path]; ok {
		panic("Adding already existing file")
	}
	i := len(*t.files)
	*t.files = append(*t.files, f)
	t.fileMap[f.Path] = i
	return i, &(*t.files)[i]
}

func (t *nodeTracker) GetDir(path string) (int, *types.Directory) {
	if i, ok := t.dirMap[path]; ok {
		return i, &(*t.dirs)[i]
	} else {
		return 0, nil
	}
}

func (t *nodeTracker) AddDir(d types.Directory) (int, *types.Directory) {
	if d.Path == "" {
		panic("Directory path missing")
	}
	if _, ok := t.dirMap[d.Path]; ok {
		panic("Adding already existing directory")
	}
	i := len(*t.dirs)
	*t.dirs = append(*t.dirs, d)
	t.dirMap[d.Path] = i
	return i, &(*t.dirs)[i]
}

func (t *nodeTracker) GetLink(path string) (int, *types.Link) {
	if i, ok := t.linkMap[path]; ok {
		return i, &(*t.links)[i]
	} else {
		return 0, nil
	}
}

func (t *nodeTracker) AddLink(l types.Link) (int, *types.Link) {
	if l.Path == "" {
		panic("Link path missing")
	}
	if _, ok := t.linkMap[l.Path]; ok {
		panic("Adding already existing link")
	}
	i := len(*t.links)
	*t.links = append(*t.links, l)
	t.linkMap[l.Path] = i
	return i, &(*t.links)[i]
}
