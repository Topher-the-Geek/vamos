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

// TestPutFileChanged checks that a file that changes while it is stored is described by the content stored,
// not the content it had when it was first read.
func TestPutFileChanged(t *testing.T) {
	fsys := &testenv.EditedFs{Fs: afero.NewMemMapFs(), File: "/a.txt", Edit: "new"}
	testenv.Must(t, afero.WriteFile(fsys.Fs, "/a.txt", []byte("old"), 0o644))
	d := newTestDirectory(t)
	b, _, err := PutFile(t.Context(), d, fsys, "/a.txt", 100<<20)
	testenv.Must(t, err)
	data, err := d.Read(t.Context(), b, 100)
	testenv.Must(t, err)
	if b.Digest != digest.FromBytes(data) || string(data) != "new" {
		t.Errorf("stored %q as %s, want %q under its own digest", data, b.Digest, "new")
	}
}

// TestPutFileEndless checks that PutFile reads no more of a file than its limit allows,
// so that a file that keeps growing, such as a log, can't fill the store.
func TestPutFileEndless(t *testing.T) {
	fsys := &endlessFs{Fs: afero.NewMemMapFs()}
	testenv.Must(t, afero.WriteFile(fsys.Fs, "/log", nil, 0o644))
	d := newTestDirectory(t)
	if _, _, err := PutFile(t.Context(), d, fsys, "/log", 10); !errors.Is(err, ErrOverLimit) {
		t.Errorf("PutFile = %v, want %v", err, ErrOverLimit)
	}
	if n := countBlobs(t, d); n != 0 {
		t.Errorf("the store holds %d blobs, want none", n)
	}
}

// TestPutFileCorrupt checks that a file whose content the store holds corrupt, as far as Has can tell,
// is stored again.
func TestPutFileCorrupt(t *testing.T) {
	fsys := afero.NewMemMapFs()
	testenv.Must(t, afero.WriteFile(fsys, "/a.txt", []byte("content"), 0o644))
	d := newTestDirectory(t)
	b, stored, err := PutFile(t.Context(), d, fsys, "/a.txt", 100)
	testenv.Must(t, err)
	if !stored {
		t.Error("the first put didn't store the file")
	}
	testenv.Must(t, os.WriteFile(d.blobPath(b), []byte("truncated"[:3]), 0o600))
	if _, stored, err := PutFile(t.Context(), d, fsys, "/a.txt", 100); err != nil || !stored {
		t.Errorf("PutFile over a corrupt blob stored = %v, %v; want it stored again", stored, err)
	}
	if data, err := d.Read(t.Context(), b, 100); err != nil || string(data) != "content" {
		t.Errorf("after storing again, Read = %q, %v", data, err)
	}
}

// endlessFs is a filesystem whose files never end.
// Reading more than a mebibyte of one fails, rather than running until the disk is full.
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

// TestLogReplay checks that a recorded log replays each stream's output in its original order.
func TestLogReplay(t *testing.T) {
	d := newTestDirectory(t)
	log, err := NewLog()
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

	saved, err := log.Save(t.Context(), d)
	testenv.Must(t, err)
	f, err := d.Get(t.Context(), *saved)
	testenv.Must(t, err)
	defer f.Close()
	var both strings.Builder
	testenv.Must(t, ReplayLog(t.Context(), f, prefixWriter{"out: ", &both}, prefixWriter{"err: ", &both}))
	if want := "out: one\nerr: two\nout: three\n"; both.String() != want {
		t.Errorf("replayed %q, want %q", both.String(), want)
	}

	empty, err := NewLog()
	testenv.Must(t, err)
	if b, err := empty.Save(t.Context(), d); b != nil || err != nil {
		t.Errorf("saved log with nothing written = %v, %v; want none", b, err)
	}
	empty.Discard()
	for _, l := range []*LogRecorder{log, empty} {
		if l.file != nil {
			t.Errorf("the log's temporary file %s is left behind", l.file.Name())
		}
	}
}

// TestLogTruncated checks that a log stops recording at its cap, ending with a marker on stderr,
// while the output still reaches its destination, and that a log that isn't whole is reported.
func TestLogTruncated(t *testing.T) {
	d := newTestDirectory(t)
	log, err := NewLog()
	testenv.Must(t, err)
	defer log.Discard()
	var stdout bytes.Buffer
	out := log.Writer(StreamStdout, &stdout)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range MaxLogSize/len(chunk) + 2 {
		out.Write(chunk)
	}
	if stdout.Len() != (MaxLogSize/len(chunk)+2)*len(chunk) {
		t.Errorf("the destination got %d bytes, want all of the output", stdout.Len())
	}
	saved, err := log.Save(t.Context(), d)
	testenv.Must(t, err)
	if saved.Size > MaxLogSize {
		t.Errorf("the log holds %d bytes, more than %d", saved.Size, MaxLogSize)
	}
	data, err := d.Read(t.Context(), *saved, MaxLogSize)
	testenv.Must(t, err)
	testenv.Must(t, CheckLog(data))
	var replayedOut, replayedErr bytes.Buffer
	testenv.Must(t, ReplayLog(t.Context(), bytes.NewReader(data), &replayedOut, &replayedErr))
	if replayedErr.String() != Truncated || replayedOut.Len() == 0 || replayedOut.Len() >= stdout.Len() {
		t.Errorf("replayed %d bytes of stdout and %q on stderr; want some of the output, then the marker",
			replayedOut.Len(), replayedErr.String())
	}
	if err := CheckLog(data[:len(data)-3]); err == nil {
		t.Error("CheckLog accepted a log cut short")
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

// newTestDirectory returns empty stores in a temporary directory.
func newTestDirectory(t *testing.T) *Directory {
	t.Helper()
	d, err := OpenDirectory(t.TempDir())
	testenv.Must(t, err)
	return d
}

// countBlobs returns how many blobs d holds.
func countBlobs(t *testing.T, d *Directory) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(d.dir, "cas", "sha256"))
	testenv.Must(t, err)
	return len(entries)
}
