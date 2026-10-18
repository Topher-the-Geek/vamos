package fileset

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
)

// TestCacheTreeRoundTrip checks that a tree stored in the cache checks out as the same files, modes and links,
// and that files with the same content are stored once.
func TestCacheTreeRoundTrip(t *testing.T) {
	fsys, _ := testenv.DiskProject(t, map[string]string{
		"app/main.js":   "same",
		"app/copy.js":   "same",
		"app/run.sh":    "#!/bin/sh",
		"app/lib/x.txt": "x",
		"top.txt":       "top",
	}, map[string]string{
		"app/link": "lib/x.txt",
	})
	dir := testenv.ProjectDir
	testenv.Must(t, fsys.Chmod(filepath.Join(dir, "app/run.sh"), 0o755|os.ModeSetuid))
	testenv.Must(t, fsys.Chmod(filepath.Join(dir, "app/lib"), 0o750))

	ctx := t.Context()
	c, cacheDir := newTestCache(t)
	sel := testSelection(t, []string{"app", "top.txt"}, nil, func(sel *Selection) { sel.ModeMask = 0o7777 })
	stored, stats, err := Store(ctx, c, fsys, dir, sel)
	testenv.Must(t, err)
	// One blob for each distinct file, and one for the tree.
	if got, want := countBlobs(t, cacheDir), 5; got != want {
		t.Errorf("cache holds %d blobs, want %d", got, want)
	}
	// app/copy.js has the content of app/main.js, stored just before it.
	want := Stats{Count: cache.Count{Files: 5, Dirs: 1, Links: 1, Bytes: 21}, NewFiles: 4, NewBytes: 17}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	_, stats, err = Store(ctx, c, fsys, dir, sel)
	testenv.Must(t, err)
	if want.NewFiles, want.NewBytes = 0, 0; stats != want {
		t.Errorf("stats of storing the files again = %+v, want %+v", stats, want)
	}
	if _, hashed, err := Hash(ctx, fsys, dir, sel); err != nil || hashed != want {
		t.Errorf("Hash's stats = %+v, %v; want %+v", hashed, err, want)
	}

	out := t.TempDir()
	_, err = cache.Checkout(ctx, c, stored, out)
	testenv.Must(t, err)
	again, _, err := storeFiles(ctx, c, afero.NewOsFs(), out, sel)
	testenv.Must(t, err)
	if again.Digest() != stored {
		want, err := cache.ReadTree(ctx, c, stored)
		testenv.Must(t, err)
		t.Errorf("checked-out files make tree %s, want %s:\n%s\nwant:\n%s",
			again.Digest().Digest, stored.Digest, again.Encode(), want.Encode())
	}
	if got, want := countBlobs(t, cacheDir), 5; got != want {
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
			fsys, _ := testenv.DiskProject(t, map[string]string{"a.txt": "a", "sub/b.txt": "b"},
				map[string]string{"sub/link": "b.txt"})
			c, _ := newTestCache(t)
			ctx := t.Context()
			before, _, err := storeFiles(ctx, c, fsys, testenv.ProjectDir, testSelection(t, []string{"."}, nil, nil))
			testenv.Must(t, err)
			testenv.Must(t, tc.change(fsys, testenv.ProjectDir))
			after, _, err := storeFiles(ctx, c, fsys, testenv.ProjectDir, testSelection(t, []string{"."}, nil, nil))
			testenv.Must(t, err)
			if same := before.Digest() == after.Digest(); same != tc.same {
				t.Errorf("digest unchanged = %v, want %v", same, tc.same)
			}
		})
	}
}

// newTestCache returns an empty content store in a temporary directory, and the directory.
func newTestCache(t *testing.T) (*cache.Directory, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.OpenDirectory(dir)
	testenv.Must(t, err)
	return c, dir
}

// countBlobs returns how many blobs the cache in dir holds.
func countBlobs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "cas", "sha256"))
	testenv.Must(t, err)
	return len(entries)
}

// countingStore counts the calls made to a content store, so that a test sees how many round trips a capture makes.
type countingStore struct {
	cache.ContentStore
	has, missing, put int
}

// Has counts the call.
func (c *countingStore) Has(ctx context.Context, b cache.Blob) error {
	c.has++
	return c.ContentStore.Has(ctx, b)
}

// Missing counts the call.
func (c *countingStore) Missing(ctx context.Context, blobs []cache.Blob) ([]cache.Blob, error) {
	c.missing++
	return c.ContentStore.Missing(ctx, blobs)
}

// Import counts the call.
func (c *countingStore) Import(ctx context.Context, b cache.Blob, r io.Reader) error {
	c.put++
	return c.ContentStore.Import(ctx, b, r)
}

// TestStoreBatchesMissing checks that a capture asks the store once which files it lacks, then sends those alone,
// and asks nothing of a file at a time, so that a remote store sees one round trip for a warm capture.
func TestStoreBatchesMissing(t *testing.T) {
	fsys := testenv.Project(t, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "a"})
	ctx := t.Context()
	c := &countingStore{ContentStore: cache.NewMemory()}
	sel := testSelection(t, []string{"*.txt"}, nil, nil)
	_, stats, err := Store(ctx, c, fsys, testenv.ProjectDir, sel)
	testenv.Must(t, err)
	// Two distinct contents are sent, each as the blob it was hashed to; the tree is put as bytes besides.
	if c.missing != 1 || c.has != 0 || c.put != 2 || stats.NewFiles != 2 {
		t.Errorf("a cold capture made %d Missing, %d Has and %d Import calls, storing %d new files; "+
			"want 1, 0, 2 and 2", c.missing, c.has, c.put, stats.NewFiles)
	}
	c.missing, c.put = 0, 0
	_, stats, err = Store(ctx, c, fsys, testenv.ProjectDir, sel)
	testenv.Must(t, err)
	if c.missing != 1 || c.has != 0 || c.put != 0 || stats.NewFiles != 0 {
		t.Errorf("a warm capture made %d Missing, %d Has and %d Import calls, storing %d new files; "+
			"want 1, 0, 0 and 0", c.missing, c.has, c.put, stats.NewFiles)
	}
}

// TestStoreFileEditedAsStored checks that a file edited between being hashed and being sent is hashed again and
// sent as it is, and described so, while another file with the content it had keeps that content;
// one that keeps changing fails the capture.
func TestStoreFileEditedAsStored(t *testing.T) {
	fsys := &testenv.EditedFs{
		Fs:   testenv.Project(t, map[string]string{"a.txt": "same", "b.txt": "same"}),
		File: "/project/a.txt",
		Edit: "edited",
		At:   2, // as it is sent, after it was hashed
	}
	ctx := t.Context()
	m := cache.NewMemory()
	stored, stats, err := Store(ctx, m, fsys, testenv.ProjectDir, testSelection(t, []string{"*.txt"}, nil, nil))
	testenv.Must(t, err)
	tree, err := cache.ReadTree(ctx, m, stored)
	testenv.Must(t, err)
	testenv.Must(t, cache.CheckTree(ctx, m, stored))
	contents := map[string]string{}
	for _, e := range tree.Entries {
		data, err := m.Read(ctx, *e.Blob, e.Blob.Size)
		testenv.Must(t, err)
		contents[e.Path] = string(data)
	}
	if want := map[string]string{"a.txt": "edited", "b.txt": "same"}; !maps.Equal(contents, want) {
		t.Errorf("the tree's files hold %v, want %v", contents, want)
	}
	if stats.NewFiles != 2 {
		t.Errorf("stats = %+v, want both contents stored", stats)
	}

	restless := &restlessFs{Fs: testenv.Project(t, map[string]string{"a.txt": "0"}), name: "/project/a.txt"}
	_, _, err = Store(ctx, cache.NewMemory(), restless, testenv.ProjectDir,
		testSelection(t, []string{"*.txt"}, nil, nil))
	if want := "/project/a.txt kept changing as it was captured"; err == nil || err.Error() != want {
		t.Errorf("capturing a file that changes at every read = %v, want %q", err, want)
	}
}

// TestStoreFileEditedPastTotal checks that a file that grows as it is sent, within the allowance the files before it
// left it, still fails the capture if the files then total more than max_total_size.
func TestStoreFileEditedPastTotal(t *testing.T) {
	fsys := &testenv.EditedFs{
		Fs:   testenv.Project(t, map[string]string{"a.txt": "a", "b.txt": "b"}),
		File: "/project/a.txt",
		Edit: "aaa", // within the three bytes left for it as it was hashed first, but four in all
		At:   2,
	}
	sel := testSelection(t, []string{"*.txt"}, nil, func(sel *Selection) { sel.MaxTotalSize = 3 })
	_, _, err := Store(t.Context(), cache.NewMemory(), fsys, testenv.ProjectDir, sel)
	if want := "the files in /project that match total more than max_total_size=3 bytes"; err == nil ||
		err.Error() != want {
		t.Errorf("capturing files edited past their total = %v, want %q", err, want)
	}
}

// restlessFs is a filesystem in which the file name holds something else each time it is opened.
type restlessFs struct {
	afero.Fs
	name   string
	opened int
}

// Open changes the file, then opens it.
func (f *restlessFs) Open(name string) (afero.File, error) {
	if name == f.name {
		f.opened++
		if err := afero.WriteFile(f.Fs, name, []byte(fmt.Sprint(f.opened)), 0o644); err != nil {
			return nil, err
		}
	}
	return f.Fs.Open(name)
}
