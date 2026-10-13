package fileset

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
)

// TestCacheTreeRoundTrip checks that a tree stored in the cache checks out as the same files,
// modes and links, and that files with the same content are stored once.
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

	c, cacheDir := newTestCache(t)
	sel := testSelection(t, []string{"app", "top.txt"}, nil, func(sel *Selection) { sel.ModeMask = 0o7777 })
	stored, stats, err := Store(c, fsys, dir, sel)
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
	_, stats, err = Store(c, fsys, dir, sel)
	testenv.Must(t, err)
	if want.NewFiles, want.NewBytes = 0, 0; stats != want {
		t.Errorf("stats of storing the files again = %+v, want %+v", stats, want)
	}
	if _, hashed, err := Hash(fsys, dir, sel); err != nil || hashed != want {
		t.Errorf("Hash's stats = %+v, %v; want %+v", hashed, err, want)
	}

	out := t.TempDir()
	_, err = c.Checkout(stored, out)
	testenv.Must(t, err)
	again, _, err := storeFiles(c, afero.NewOsFs(), out, sel)
	testenv.Must(t, err)
	if again.Digest() != stored {
		want, err := c.ReadTree(stored)
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
			before, _, err := storeFiles(c, fsys, testenv.ProjectDir, testSelection(t, []string{"."}, nil, nil))
			testenv.Must(t, err)
			testenv.Must(t, tc.change(fsys, testenv.ProjectDir))
			after, _, err := storeFiles(c, fsys, testenv.ProjectDir, testSelection(t, []string{"."}, nil, nil))
			testenv.Must(t, err)
			if same := before.Digest() == after.Digest(); same != tc.same {
				t.Errorf("digest unchanged = %v, want %v", same, tc.same)
			}
		})
	}
}

// newTestCache returns an empty cache in a temporary directory, and the directory.
func newTestCache(t *testing.T) (*cache.Cache, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(dir)
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
