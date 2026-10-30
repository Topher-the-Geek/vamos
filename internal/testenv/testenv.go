package testenv

import (
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/afero"
)

// ProjectDir is where in-memory test projects live.
const ProjectDir = "/project"

// PullBusybox declares the base image for tests, as the target "busybox", in one line.
// Tests that build pull it once into their suite's store, and later builds find it there.
const PullBusybox = `pull(name="busybox", image="docker.io/library/busybox", tag="1.36")` + "\n"

// NewNonce returns a value new to each call, for actions that must run rather than come from the cache.
func NewNonce() string {
	return fmt.Sprintf("%016x", rand.Uint64())
}

// Project returns an in-memory filesystem holding files, keyed by their path within ProjectDir.
func Project(t *testing.T, files map[string]string) afero.Fs {
	t.Helper()
	fsys := afero.NewMemMapFs()
	if err := fsys.MkdirAll(ProjectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(ProjectDir, name)
		// MemMapFs would make missing folders with no permissions.
		if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(fsys, path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fsys
}

// DiskProject is like Project, but keeps the files on disk, under root/project,
// for tests that need symbolic links or that write into the project.
// Links maps a link's path within ProjectDir to its target.
func DiskProject(t *testing.T, files, links map[string]string) (fsys afero.Fs, root string) {
	t.Helper()
	root = t.TempDir()
	fsys = afero.NewBasePathFs(afero.NewOsFs(), root)
	if err := fsys.MkdirAll(ProjectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(ProjectDir, name)
		if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(fsys, path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		path := filepath.Join(root, ProjectDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	// Give files and folders the modes that Project does, whatever the umask took from them.
	err := filepath.WalkDir(filepath.Join(root, ProjectDir), func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return os.Chmod(path, 0o755)
		case d.Type().IsRegular():
			return os.Chmod(path, 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys, root
}

// Must fails the test at once if err isn't nil.
func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// EditedFs is a filesystem in which the file File is changed to hold Edit when it is opened for the At'th time,
// or the second if At is zero, as if it were edited while a build reads it.
type EditedFs struct {
	afero.Fs
	File, Edit string
	At         int
	opened     int
}

// LstatIfPossible describes the file path without following a link, as the wrapped filesystem does.
func (f *EditedFs) LstatIfPossible(path string) (fs.FileInfo, bool, error) {
	return lstat(f.Fs, path)
}

// ReadlinkIfPossible reads the symbolic link path, as the wrapped filesystem does.
func (f *EditedFs) ReadlinkIfPossible(path string) (string, error) {
	return readlink(f.Fs, path)
}

// readlink reads the symbolic link path in fsys, if fsys can.
func readlink(fsys afero.Fs, path string) (string, error) {
	if r, ok := fsys.(afero.LinkReader); ok {
		return r.ReadlinkIfPossible(path)
	}
	return "", &fs.PathError{Op: "readlink", Path: path, Err: afero.ErrNoReadlink}
}

// lstat describes the file path in fsys without following a link, if fsys can.
func lstat(fsys afero.Fs, path string) (fs.FileInfo, bool, error) {
	if l, ok := fsys.(afero.Lstater); ok {
		return l.LstatIfPossible(path)
	}
	info, err := fsys.Stat(path)
	return info, false, err
}

// Open opens the file name, first editing it if it is File, opened At-1 times before.
func (f *EditedFs) Open(name string) (afero.File, error) {
	if name == f.File {
		f.opened++
		if f.opened == max(f.At, 2) {
			if err := afero.WriteFile(f.Fs, name, []byte(f.Edit), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return f.Fs.Open(name)
}

// DeniedFs is a filesystem in which the files Files, clean absolute paths, can't be checked, read or opened,
// as when their directory isn't searchable: Stat, LstatIfPossible, ReadlinkIfPossible, Open and OpenFile fail for
// those paths, though not for files beneath them. Tests run as root, which permissions on disk don't stop.
type DeniedFs struct {
	afero.Fs
	Files []string
}

// Stat describes the file path, or fails for one of Files.
func (f *DeniedFs) Stat(path string) (fs.FileInfo, error) {
	if err := f.denied("stat", path); err != nil {
		return nil, err
	}
	return f.Fs.Stat(path)
}

// LstatIfPossible describes the file path without following a link, as the wrapped filesystem does,
// or fails for one of Files.
func (f *DeniedFs) LstatIfPossible(path string) (fs.FileInfo, bool, error) {
	if err := f.denied("lstat", path); err != nil {
		return nil, false, err
	}
	return lstat(f.Fs, path)
}

// ReadlinkIfPossible reads the symbolic link path, as the wrapped filesystem does, or fails for one of Files.
func (f *DeniedFs) ReadlinkIfPossible(path string) (string, error) {
	if err := f.denied("readlink", path); err != nil {
		return "", err
	}
	return readlink(f.Fs, path)
}

// Open opens the file name, or fails for one of Files.
func (f *DeniedFs) Open(name string) (afero.File, error) {
	if err := f.denied("open", name); err != nil {
		return nil, err
	}
	return f.Fs.Open(name)
}

// OpenFile opens the file name, or fails for one of Files.
func (f *DeniedFs) OpenFile(name string, flag int, perm fs.FileMode) (afero.File, error) {
	if err := f.denied("open", name); err != nil {
		return nil, err
	}
	return f.Fs.OpenFile(name, flag, perm)
}

// denied returns the error that op on path gets, if path is one of Files.
func (f *DeniedFs) denied(op, path string) error {
	if slices.Contains(f.Files, filepath.Clean(path)) {
		return &fs.PathError{Op: op, Path: path, Err: fs.ErrPermission}
	}
	return nil
}
