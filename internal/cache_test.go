package internal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// TestCacheTreeRoundTrip checks that a tree stored in the cache checks out as the same files,
// modes and links, and that files with the same content are stored once.
func TestCacheTreeRoundTrip(t *testing.T) {
	fsys, _ := diskProject(t, map[string]string{
		"app/main.js":   "same",
		"app/copy.js":   "same",
		"app/run.sh":    "#!/bin/sh",
		"app/lib/x.txt": "x",
		"top.txt":       "top",
	}, map[string]string{
		"app/link": "lib/x.txt",
	})
	dir := projectDir
	must(t, fsys.Chmod(filepath.Join(dir, "app/run.sh"), 0o755|os.ModeSetuid))
	must(t, fsys.Chmod(filepath.Join(dir, "app/lib"), 0o750))

	c := newTestCache(t)
	paths := []string{"app", "top.txt"}
	stored, err := c.putTree(fsys, dir, paths)
	must(t, err)
	// One blob for each distinct file, and one for the tree.
	if got, want := countBlobs(t, c), 5; got != want {
		t.Errorf("cache holds %d blobs, want %d", got, want)
	}

	out := t.TempDir()
	must(t, c.checkout(stored, out))
	again, err := c.putFiles(afero.NewOsFs(), out, paths)
	must(t, err)
	if again.digest() != stored {
		want, err := c.readTree(stored)
		must(t, err)
		t.Errorf("checked-out files make tree %s, want %s:\n%s\nwant:\n%s",
			again.digest().Digest, stored.Digest, again.encode(), want.encode())
	}
	if got, want := countBlobs(t, c), 5; got != want {
		t.Errorf("cache holds %d blobs after storing the same files again, want %d", got, want)
	}
	if target, err := os.Readlink(filepath.Join(out, "app/link")); err != nil || target != "lib/x.txt" {
		t.Errorf("app/link = %q, %v; want a link to lib/x.txt", target, err)
	}
}

// TestTreeDigest checks which changes to files change their tree's digest.
func TestTreeDigest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(fsys afero.Fs, dir string) error
		same   bool
	}{
		{
			name: "content",
			change: func(fsys afero.Fs, dir string) error {
				return afero.WriteFile(fsys, dir+"/a.txt", []byte("b"), 0o644)
			},
		},
		{
			name:   "mode",
			change: func(fsys afero.Fs, dir string) error { return fsys.Chmod(dir+"/a.txt", 0o600) },
		},
		{
			name:   "directory mode",
			change: func(fsys afero.Fs, dir string) error { return fsys.Chmod(dir+"/sub", 0o700) },
		},
		{
			name: "link target",
			change: func(fsys afero.Fs, dir string) error {
				root := fsys.(*afero.BasePathFs)
				real, err := root.RealPath(dir + "/sub/link")
				if err != nil {
					return err
				}
				os.Remove(real)
				return os.Symlink("../b.txt", real)
			},
		},
		{
			name:   "new file",
			change: func(fsys afero.Fs, dir string) error { return afero.WriteFile(fsys, dir+"/sub/new", nil, 0o644) },
		},
		{
			name: "modification time",
			change: func(fsys afero.Fs, dir string) error {
				old := time.Unix(1, 0)
				return fsys.Chtimes(dir+"/a.txt", old, old)
			},
			same: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _ := diskProject(t, map[string]string{"a.txt": "a", "sub/b.txt": "b"},
				map[string]string{"sub/link": "b.txt"})
			c := newTestCache(t)
			before, err := c.putFiles(fsys, projectDir, []string{"."})
			must(t, err)
			must(t, tc.change(fsys, projectDir))
			after, err := c.putFiles(fsys, projectDir, []string{"."})
			must(t, err)
			if same := before.digest() == after.digest(); same != tc.same {
				t.Errorf("digest unchanged = %v, want %v", same, tc.same)
			}
		})
	}
}

// TestPutFileChanged checks that a file that changes while it is stored is described by the
// content stored, not the content it had when it was first read.
func TestPutFileChanged(t *testing.T) {
	fsys := &editedFs{Fs: afero.NewMemMapFs(), name: "/a.txt", edit: "new"}
	must(t, afero.WriteFile(fsys.Fs, "/a.txt", []byte("old"), 0o644))
	c := newTestCache(t)
	b, err := c.putFile(fsys, "/a.txt")
	must(t, err)
	data, err := c.readBytes(b)
	must(t, err)
	if b.Digest != digest.FromBytes(data) || string(data) != "new" {
		t.Errorf("stored %q as %s, want %q under its own digest", data, b.Digest, "new")
	}
}

// editedFs is a filesystem in which the file name is changed to hold edit when it is opened for the
// at'th time, or the second if at is zero, as if it were edited while a build reads it.
type editedFs struct {
	afero.Fs
	name, edit string
	at         int
	opened     int
}

// Open opens the file name, first editing it if it is name, opened at-1 times before.
func (f *editedFs) Open(name string) (afero.File, error) {
	if name == f.name {
		f.opened++
		if f.opened == max(f.at, 2) {
			if err := afero.WriteFile(f.Fs, name, []byte(f.edit), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return f.Fs.Open(name)
}

// TestActionResults checks that the latest result for a key is kept, and that a key with
// none, or with one that can't be read, has no result.
func TestActionResults(t *testing.T) {
	c := newTestCache(t)
	a := &action{
		Version: actionVersion,
		Kind:    "task",
		Base:    "scratch",
		Steps: []actionStep{
			{Op: "env", Vars: []string{"LANG=C"}},
			{Op: "run", Argv: []string{"true"}},
		},
	}
	key := a.key()
	if r, err := c.result(key); err != nil || r != nil {
		t.Fatalf("result before any = %v, %v; want none", r, err)
	}
	must(t, c.setResult(key, &actionResult{Epoch: 1}))
	must(t, c.setResult(key, &actionResult{Epoch: 2}))
	if r, err := c.result(key); err != nil || r == nil || r.Epoch != 2 {
		t.Errorf("result = %+v, %v; want the latest, with epoch 2", r, err)
	}

	b := *a
	b.Steps = []actionStep{a.Steps[0], {Op: "run", Argv: []string{"false"}}}
	if b.key() == key {
		t.Errorf("actions with different commands have the same key")
	}
	b.Steps = []actionStep{a.Steps[1], a.Steps[0]}
	if b.key() == key {
		t.Errorf("actions with steps in a different order have the same key")
	}

	must(t, os.WriteFile(c.actionPath(key), []byte("{"), 0o600))
	if r, err := c.result(key); err != nil || r != nil {
		t.Errorf("unreadable result = %v, %v; want none", r, err)
	}
}

// TestLogReplay checks that a recorded log replays each stream's output in its original order.
func TestLogReplay(t *testing.T) {
	c := newTestCache(t)
	log, err := c.newLog()
	must(t, err)
	defer log.discard()
	var stdout, stderr bytes.Buffer
	out := log.writer(streamStdout, &stdout)
	errOut := log.writer(streamStderr, &stderr)
	out.Write([]byte("one\n"))
	errOut.Write([]byte("two\n"))
	out.Write([]byte("three\n"))
	if stdout.String() != "one\nthree\n" || stderr.String() != "two\n" {
		t.Fatalf("passed on stdout %q and stderr %q", stdout.String(), stderr.String())
	}

	saved, err := log.save(c)
	must(t, err)
	f, err := c.open(*saved)
	must(t, err)
	defer f.Close()
	var both strings.Builder
	must(t, replayLog(f, prefixWriter{"out: ", &both}, prefixWriter{"err: ", &both}))
	if want := "out: one\nerr: two\nout: three\n"; both.String() != want {
		t.Errorf("replayed %q, want %q", both.String(), want)
	}

	empty, err := c.newLog()
	must(t, err)
	if b, err := empty.save(c); b != nil || err != nil {
		t.Errorf("saved log with nothing written = %v, %v; want none", b, err)
	}
	empty.discard()
	if entries, _ := os.ReadDir(filepath.Join(c.dir, "tmp")); len(entries) > 0 {
		t.Errorf("temporary files %v left in the cache", entries)
	}
}

// TestCheckoutRefusesEscapes checks that a tree in the cache can't write outside the folder it
// is checked out to, even if it has been tampered with.
func TestCheckoutRefusesEscapes(t *testing.T) {
	tests := []struct {
		name    string
		entries []treeEntry
		want    string
	}{
		{
			name:    "parent path",
			entries: []treeEntry{{Path: "../escaped", Type: entryLink, Target: "x"}},
			want:    `"../escaped" is outside it`,
		},
		{
			name:    "absolute path",
			entries: []treeEntry{{Path: "/escaped", Type: entryLink, Target: "x"}},
			want:    `"/escaped" is outside it`,
		},
		{
			name: "beneath a link",
			entries: []treeEntry{
				{Path: "out", Type: entryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: entryLink, Target: "x"},
			},
			want: `"out/escaped" is beneath the symbolic link "out"`,
		},
		{
			name: "beneath a link named by an unclean path",
			entries: []treeEntry{
				{Path: "a/../out", Type: entryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: entryLink, Target: "x"},
			},
			want: `"a/../out" isn't a clean path`,
		},
		{
			name:    "file without content",
			entries: []treeEntry{{Path: "empty", Type: entryFile, Mode: 0o644}},
			want:    `"empty" has unknown type "file" or no content`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(t)
			b, err := c.putBytes((&tree{Entries: tt.entries}).encode())
			must(t, err)
			err = c.checkout(b, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("checkout error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// prefixWriter writes each write to w after prefix.
type prefixWriter struct {
	prefix string
	w      *strings.Builder
}

// Write writes b after the prefix.
func (p prefixWriter) Write(b []byte) (int, error) {
	p.w.WriteString(p.prefix)
	return p.w.Write(b)
}

// newTestCache returns an empty cache in a temporary directory.
func newTestCache(t *testing.T) *cache {
	t.Helper()
	c, err := openCache(t.TempDir())
	must(t, err)
	return c
}

// countBlobs returns how many blobs c holds.
func countBlobs(t *testing.T, c *cache) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.dir, "cas", "sha256"))
	must(t, err)
	return len(entries)
}
