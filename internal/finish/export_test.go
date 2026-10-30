package finish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// epoch is the time the test tree's files are stamped with: after 2262, which nanoseconds since 1970 can't hold.
var epoch = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)

// testTree stores a tree in a new cache, and returns the cache and a result naming it.
// Its files are a.txt, and deep/x/y.txt, whose folders it doesn't list, and links to victim, a file outside the tree:
// by its absolute path, and by a path leading out of the tree, and a link leading nowhere.
func testTree(t *testing.T, victim string) (*cache.Directory, api.Exported) {
	t.Helper()
	c, err := cache.OpenDirectory(t.TempDir())
	testenv.Must(t, err)
	a, err := c.PutBytes(t.Context(), []byte("a"))
	testenv.Must(t, err)
	tree := &cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
		{Path: "abs", Type: cache.EntryLink, Target: victim},
		{Path: "dangling", Type: cache.EntryLink, Target: "/no/such/file"},
		{Path: "deep/x/y.txt", Type: cache.EntryFile, Mode: 0o600, Blob: &a},
		{Path: "up", Type: cache.EntryLink, Target: "../../../../../../../../.." + victim},
	}}
	stored, err := c.PutBytes(t.Context(), tree.Encode())
	testenv.Must(t, err)
	return c, api.Exported{Files: &stored, Epoch: epoch.Unix()}
}

// newVictim returns a file outside any export, with an old time, which exports must leave alone.
func newVictim(t *testing.T) (string, time.Time) {
	t.Helper()
	victim := filepath.Join(t.TempDir(), "victim")
	testenv.Must(t, os.WriteFile(victim, []byte("keep"), 0o644))
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	testenv.Must(t, os.Chtimes(victim, old, old))
	return victim, old
}

// noWarnings fails the test if it is called.
func noWarnings(t *testing.T) func(error) {
	return func(err error) { t.Errorf("warned: %v", err) }
}

// assertNoStaging fails the test if a staging folder is left beside dir.
func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	staged := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".vamos-*")
	if leftover, _ := filepath.Glob(staged); leftover != nil {
		t.Errorf("staging folders left behind: %q", leftover)
	}
}

// TestExportFolderFilledDuringBuild checks that files added to an empty folder after it was checked,
// as if during the build, are kept, and the export refused, unless it replaces the folder.
func TestExportFolderFilledDuringBuild(t *testing.T) {
	victim, _ := newVictim(t)
	c, res := testTree(t, victim)
	dir := t.TempDir()
	testenv.Must(t, CheckExportDest(dir, false))
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644))

	err := Export(context.Background(), c, res, dir, false, noWarnings(t))
	if !errors.Is(err, errNotEmpty) {
		t.Fatalf("Export = %v, want %v", err, errNotEmpty)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "new.txt")); err != nil || string(got) != "new" {
		t.Errorf("new.txt = %q, %v; want it kept", got, err)
	}
	assertNoStaging(t, dir)
}

// TestExportReplacesOnlyFolders checks that a file or link put in place of the folder is kept,
// and the export refused, even with replace.
func TestExportReplacesOnlyFolders(t *testing.T) {
	victim, _ := newVictim(t)
	for _, kind := range []string{"file", "link"} {
		t.Run(kind, func(t *testing.T) {
			c, res := testTree(t, victim)
			dir := filepath.Join(t.TempDir(), "out")
			if kind == "file" {
				testenv.Must(t, os.WriteFile(dir, []byte("file"), 0o644))
			} else {
				testenv.Must(t, os.Symlink(t.TempDir(), dir))
			}
			err := Export(context.Background(), c, res, dir, true, noWarnings(t))
			if !errors.Is(err, errNotDir) {
				t.Fatalf("Export = %v, want %v", err, errNotDir)
			}
			info, err := os.Lstat(dir)
			testenv.Must(t, err)
			if kind == "file" && !info.Mode().IsRegular() || kind == "link" && info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is %v now, want the %s kept", dir, info.Mode(), kind)
			}
			assertNoStaging(t, dir)
		})
	}
}

// TestExportCleanupFails checks that failing to remove what an export replaced is a warning,
// once the files are in place.
func TestExportCleanupFails(t *testing.T) {
	removeAll = func(string) error { return errors.New("disk on fire") }
	t.Cleanup(func() { removeAll = os.RemoveAll })
	victim, _ := newVictim(t)
	c, res := testTree(t, victim)
	dir := t.TempDir()
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644))

	var warnings []error
	warn := func(err error) { warnings = append(warnings, err) }
	testenv.Must(t, Export(context.Background(), c, res, dir, true, warn))
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "removing the files replaced at ") ||
		!strings.Contains(warnings[0].Error(), "disk on fire") {
		t.Errorf("warnings = %v, want one about removing the files replaced", warnings)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("the task's files aren't in place: %v", err)
	}
}

// TestExportPutBackFails checks that if what was at the folder's place, a file,
// can't be put back after being swapped with the staged files, it is kept where it went, and the error says where.
func TestExportPutBackFails(t *testing.T) {
	calls := 0
	exchange = func(a, b string) error {
		if calls++; calls > 1 {
			return syscall.EBUSY
		}
		return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	}
	t.Cleanup(func() { exchange = defaultExchange })
	victim, _ := newVictim(t)
	c, res := testTree(t, victim)
	dir := filepath.Join(t.TempDir(), "out")
	testenv.Must(t, os.WriteFile(dir, []byte("file"), 0o644))

	err := Export(context.Background(), c, res, dir, true, noWarnings(t))
	staged := filepath.Join(filepath.Dir(dir), ".out.vamos-*")
	leftover, _ := filepath.Glob(staged)
	if err == nil || len(leftover) != 1 || !strings.Contains(err.Error(), "which is now at "+leftover[0]) {
		t.Fatalf("Export = %v, leaving %q; want an error saying where the file is", err, leftover)
	}
	if got, err := os.ReadFile(leftover[0]); err != nil || string(got) != "file" {
		t.Errorf("%s = %q, %v; want the file that was at %s", leftover[0], got, err, dir)
	}
}

// TestExportReplaceWithoutExchange checks that replace works on a filesystem that can't exchange two names in one step,
// and still keeps a file put in place of the folder.
func TestExportReplaceWithoutExchange(t *testing.T) {
	exchange = func(string, string) error { return unix.EINVAL }
	t.Cleanup(func() { exchange = defaultExchange })
	victim, _ := newVictim(t)

	t.Run("folder", func(t *testing.T) {
		c, res := testTree(t, victim)
		dir := t.TempDir()
		testenv.Must(t, os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644))
		testenv.Must(t, Export(context.Background(), c, res, dir, true, noWarnings(t)))
		if _, err := os.Stat(filepath.Join(dir, "old.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old.txt is still there: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(got) != "a" {
			t.Errorf("a.txt = %q, %v; want the task's", got, err)
		}
		assertNoStaging(t, dir)
	})
	t.Run("file", func(t *testing.T) {
		c, res := testTree(t, victim)
		dir := filepath.Join(t.TempDir(), "out")
		testenv.Must(t, os.WriteFile(dir, []byte("file"), 0o644))
		if err := Export(context.Background(), c, res, dir, true, noWarnings(t)); !errors.Is(err, errNotDir) {
			t.Fatalf("Export = %v, want %v", err, errNotDir)
		}
		if got, err := os.ReadFile(dir); err != nil || string(got) != "file" {
			t.Errorf("%s = %q, %v; want the file kept", dir, got, err)
		}
		assertNoStaging(t, dir)
	})
}
