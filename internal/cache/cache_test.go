package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// TestPutFileChanged checks that a file that changes while it is stored is described by the
// content stored, not the content it had when it was first read.
func TestPutFileChanged(t *testing.T) {
	fsys := &testenv.EditedFs{Fs: afero.NewMemMapFs(), File: "/a.txt", Edit: "new"}
	testenv.Must(t, afero.WriteFile(fsys.Fs, "/a.txt", []byte("old"), 0o644))
	c := newTestCache(t)
	b, _, err := c.PutFile(fsys, "/a.txt", 100<<20)
	testenv.Must(t, err)
	data, err := c.ReadBytes(b)
	testenv.Must(t, err)
	if b.Digest != digest.FromBytes(data) || string(data) != "new" {
		t.Errorf("stored %q as %s, want %q under its own digest", data, b.Digest, "new")
	}
}

// TestPutFileEndless checks that putFile reads no more of a file than its limit allows, so that a file that
// keeps growing, such as a log, can't fill the cache.
func TestPutFileEndless(t *testing.T) {
	fsys := &endlessFs{Fs: afero.NewMemMapFs()}
	testenv.Must(t, afero.WriteFile(fsys.Fs, "/log", nil, 0o644))
	c := newTestCache(t)
	if _, _, err := c.PutFile(fsys, "/log", 10); !errors.Is(err, ErrOverLimit) {
		t.Errorf("putFile = %v, want %v", err, ErrOverLimit)
	}
	if n := countBlobs(t, c); n != 0 {
		t.Errorf("the cache holds %d blobs, want none", n)
	}
}

// endlessFs is a filesystem whose files never end. Reading more than a mebibyte of one fails, rather than
// running until the disk is full.
type endlessFs struct {
	afero.Fs
}

// Open opens the file name, which never ends.
func (f *endlessFs) Open(name string) (afero.File, error) {
	file, err := f.Fs.Open(name)
	return &endlessFile{File: file}, err
}

// endlessFile is a file of endless zeros.
type endlessFile struct {
	afero.File
	read int
}

// Read fills p with zeros, or fails once a mebibyte has been read.
func (f *endlessFile) Read(p []byte) (int, error) {
	if f.read += len(p); f.read > 1<<20 {
		return 0, errors.New("read more than a mebibyte of an endless file")
	}
	clear(p)
	return len(p), nil
}

// TestActionResults checks that the latest result for a key is kept, and that a key with
// none, or with one that can't be read, has no result.
func TestActionResults(t *testing.T) {
	c := newTestCache(t)
	key := digest.FromString("an action")
	if r, err := c.Result(key); err != nil || r != nil {
		t.Fatalf("result before any = %v, %v; want none", r, err)
	}
	testenv.Must(t, c.SetResult(key, &ActionResult{Epoch: 1}))
	testenv.Must(t, c.SetResult(key, &ActionResult{Epoch: 2}))
	if r, err := c.Result(key); err != nil || r == nil || r.Epoch != 2 {
		t.Errorf("result = %+v, %v; want the latest, with epoch 2", r, err)
	}

	testenv.Must(t, os.WriteFile(c.actionPath(key), []byte("{"), 0o600))
	if r, err := c.Result(key); err != nil || r != nil {
		t.Errorf("unreadable result = %v, %v; want none", r, err)
	}
}

// TestLogReplay checks that a recorded log replays each stream's output in its original order.
func TestLogReplay(t *testing.T) {
	c := newTestCache(t)
	log, err := c.NewLog()
	testenv.Must(t, err)
	defer log.Discard()
	var stdout, stderr bytes.Buffer
	out := log.Writer(StreamStdout, &stdout)
	errOut := log.Writer(StreamStderr, &stderr)
	out.Write([]byte("one\n"))
	errOut.Write([]byte("two\n"))
	out.Write([]byte("three\n"))
	if stdout.String() != "one\nthree\n" || stderr.String() != "two\n" {
		t.Fatalf("passed on stdout %q and stderr %q", stdout.String(), stderr.String())
	}

	saved, err := log.Save(c)
	testenv.Must(t, err)
	f, err := c.OpenBlob(*saved)
	testenv.Must(t, err)
	defer f.Close()
	var both strings.Builder
	testenv.Must(t, ReplayLog(f, prefixWriter{"out: ", &both}, prefixWriter{"err: ", &both}))
	if want := "out: one\nerr: two\nout: three\n"; both.String() != want {
		t.Errorf("replayed %q, want %q", both.String(), want)
	}

	empty, err := c.NewLog()
	testenv.Must(t, err)
	if b, err := empty.Save(c); b != nil || err != nil {
		t.Errorf("saved log with nothing written = %v, %v; want none", b, err)
	}
	empty.Discard()
	if entries, _ := os.ReadDir(filepath.Join(c.dir, "tmp")); len(entries) > 0 {
		t.Errorf("temporary files %v left in the cache", entries)
	}
}

// TestCheckoutRefusesEscapes checks that a tree in the cache can't write outside the folder it
// is checked out to, even if it has been tampered with.
func TestCheckoutRefusesEscapes(t *testing.T) {
	tests := []struct {
		name    string
		entries []TreeEntry
		want    string
	}{
		{
			name:    "parent path",
			entries: []TreeEntry{{Path: "../escaped", Type: EntryLink, Target: "x"}},
			want:    `"../escaped" is outside it`,
		},
		{
			name:    "absolute path",
			entries: []TreeEntry{{Path: "/escaped", Type: EntryLink, Target: "x"}},
			want:    `"/escaped" is outside it`,
		},
		{
			name: "beneath a link",
			entries: []TreeEntry{
				{Path: "out", Type: EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: EntryLink, Target: "x"},
			},
			want: `"out/escaped" is beneath the symbolic link "out"`,
		},
		{
			name: "beneath a link named by an unclean path",
			entries: []TreeEntry{
				{Path: "a/../out", Type: EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: EntryLink, Target: "x"},
			},
			want: `"a/../out" isn't a clean path`,
		},
		{
			name:    "file without content",
			entries: []TreeEntry{{Path: "empty", Type: EntryFile, Mode: 0o644}},
			want:    `"empty" has unknown type "file" or no content`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(t)
			b, err := c.PutBytes((&Tree{Entries: tt.entries}).Encode())
			testenv.Must(t, err)
			_, err = c.Checkout(b, t.TempDir())
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
func newTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(t.TempDir())
	testenv.Must(t, err)
	return c
}

// countBlobs returns how many blobs c holds.
func countBlobs(t *testing.T, c *Cache) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.dir, "cas", "sha256"))
	testenv.Must(t, err)
	return len(entries)
}
