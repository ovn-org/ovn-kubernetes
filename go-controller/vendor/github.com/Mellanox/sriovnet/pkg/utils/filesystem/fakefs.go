//nolint:gomnd
package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/afero"
)

// fakeFs is implemented in terms of afero
type fakeFs struct {
	a afero.Afero
}

// NewFakeFs returns a fake Filesystem that exists at fakeFsRoot as its base path, useful for unit tests.
// Returns: Filesystem interface, teardown method (cleanup of provided root path) and error.
// teardown method should be called at the end of each test to ensure environment is left clean.
func NewFakeFs(fakeFsRoot string) (Filesystem, func(), error) {
	_, err := os.Stat(fakeFsRoot)
	// if fakeFsRoot dir exists remove it.
	if err == nil {
		err = os.RemoveAll(fakeFsRoot)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to cleanup fake root dir %s. %s", fakeFsRoot, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("failed to lstat fake root dir %s. %s", fakeFsRoot, err)
	}

	// create fakeFsRoot dir
	if err = os.MkdirAll(fakeFsRoot, os.FileMode(0755)); err != nil {
		return nil, nil, fmt.Errorf("failed to create fake root dir: %s. %s", fakeFsRoot, err)
	}

	return &fakeFs{a: afero.Afero{Fs: afero.NewBasePathFs(afero.NewOsFs(), fakeFsRoot)}},
		func() {
			os.RemoveAll(fakeFsRoot)
		},
		nil
}

// Stat via afero.Fs.Stat
func (fs *fakeFs) Stat(name string) (os.FileInfo, error) {
	return fs.a.Fs.Stat(name)
}

// Create via afero.Fs.Create
func (fs *fakeFs) Create(name string) (File, error) {
	file, err := fs.a.Fs.Create(name)
	if err != nil {
		return nil, err
	}
	return &fakeFile{file}, nil
}

// Rename via afero.Fs.Rename
func (fs *fakeFs) Rename(oldpath, newpath string) error {
	return fs.a.Fs.Rename(oldpath, newpath)
}

// MkdirAll via afero.Fs.MkdirAll
func (fs *fakeFs) MkdirAll(path string, perm os.FileMode) error {
	return fs.a.Fs.MkdirAll(path, perm)
}

// Chtimes via afero.Fs.Chtimes
func (fs *fakeFs) Chtimes(name string, atime, mtime time.Time) error {
	return fs.a.Fs.Chtimes(name, atime, mtime)
}

// ReadFile via afero.ReadFile
func (fs *fakeFs) ReadFile(filename string) ([]byte, error) {
	return fs.a.ReadFile(filename)
}

// TempDir via afero.TempDir
func (fs *fakeFs) TempDir(dir, prefix string) (string, error) {
	return fs.a.TempDir(dir, prefix)
}

// TempFile via afero.TempFile
func (fs *fakeFs) TempFile(dir, prefix string) (File, error) {
	file, err := fs.a.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	return &fakeFile{file}, nil
}

// ReadDir via afero.ReadDir
func (fs *fakeFs) ReadDir(dirname string) ([]os.FileInfo, error) {
	return fs.a.ReadDir(dirname)
}

// Walk via afero.Walk
func (fs *fakeFs) Walk(root string, walkFn filepath.WalkFunc) error {
	return fs.a.Walk(root, walkFn)
}

// RemoveAll via afero.RemoveAll
func (fs *fakeFs) RemoveAll(path string) error {
	return fs.a.RemoveAll(path)
}

// Remove via afero.Remove
func (fs *fakeFs) Remove(name string) error {
	return fs.a.Remove(name)
}

// Readlink via afero.ReadlinkIfPossible
func (fs *fakeFs) Readlink(name string) (string, error) {
	return fs.a.Fs.(afero.Symlinker).ReadlinkIfPossible(name)
}

// Symlink via afero.FS.(Symlinker).SymlinkIfPossible
func (fs *fakeFs) Symlink(oldname, newname string) error {
	return fs.a.Fs.(afero.Symlinker).SymlinkIfPossible(oldname, newname)
}

// fakeFile implements File; for use with fakeFs
type fakeFile struct {
	file afero.File
}

// Name via afero.File.Name
func (file *fakeFile) Name() string {
	return file.file.Name()
}

// Write via afero.File.Write
func (file *fakeFile) Write(b []byte) (n int, err error) {
	return file.file.Write(b)
}

// Sync via afero.File.Sync
func (file *fakeFile) Sync() error {
	return file.file.Sync()
}

// Close via afero.File.Close
func (file *fakeFile) Close() error {
	return file.file.Close()
}
