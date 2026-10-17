package cache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// TestDirectoryLockInProcess checks that a prune is refused while another handle holds the directory open,
// succeeds once that handle is closed, and that the lock is shared by handles that only use the directory.
func TestDirectoryLockInProcess(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	a, err := OpenDirectory(dir)
	testenv.Must(t, err)
	b, err := OpenDirectory(dir)
	testenv.Must(t, err)
	_, _, err = a.Prune(ctx, nil)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "another vamos is using") {
		t.Errorf("Prune with another handle open = %v, want %v naming the other", err, ErrLocked)
	}
	if _, err := a.PutBytes(ctx, []byte("still usable")); err != nil {
		t.Errorf("after a refused prune, the handle can't be used: %v", err)
	}
	testenv.Must(t, b.Close())
	if _, blobs, err := a.Prune(ctx, nil); err != nil || blobs != 1 {
		t.Errorf("Prune once alone = %d blobs, %v; want 1, nil", blobs, err)
	}
	// After the prune, the handle holds the lock shared again, so another handle opens.
	c, err := OpenDirectory(dir)
	testenv.Must(t, err)
	testenv.Must(t, c.Close())
	testenv.Must(t, a.Close())
}

// lockHelperEnv marks the test binary run by TestDirectoryLockAcrossProcesses, in which TestDirectoryLockHelper
// holds the directory the variable names: open, or being pruned if VAMOS_TEST_LOCK_PRUNE is set.
const lockHelperEnv = "VAMOS_TEST_LOCK_DIR"

// TestDirectoryLockHelper holds a directory, as TestDirectoryLockAcrossProcesses asks, until its ready file
// is removed; otherwise it does nothing.
func TestDirectoryLockHelper(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		return
	}
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	hold := func() {
		ready := filepath.Join(dir, "ready")
		testenv.Must(t, os.WriteFile(ready, nil, 0o600))
		for {
			if _, err := os.Stat(ready); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if os.Getenv("VAMOS_TEST_LOCK_PRUNE") == "" {
		hold()
		return
	}
	// Prune holds the lock exclusive while clearing; make that last by holding within an exclusive conversion.
	shared, err := d.lock.exclusive(dir)
	testenv.Must(t, err)
	hold()
	testenv.Must(t, shared())
}

// TestDirectoryLockAcrossProcesses checks that a prune in a second process is refused while this one holds
// the directory open, and that opening the directory is refused while a prune in another process holds it.
func TestDirectoryLockAcrossProcesses(t *testing.T) {
	ctx := t.Context()
	t.Run("prune refused while open elsewhere", func(t *testing.T) {
		dir := t.TempDir()
		stop := holdInSubprocess(t, dir, false)
		d, err := OpenDirectory(dir)
		testenv.Must(t, err)
		defer d.Close()
		if _, _, err := d.Prune(ctx, nil); !errors.Is(err, ErrLocked) {
			t.Errorf("Prune while another process holds the directory = %v, want %v", err, ErrLocked)
		}
		stop()
		if _, _, err := d.Prune(ctx, nil); err != nil {
			t.Errorf("Prune once the other process let go = %v", err)
		}
	})
	t.Run("open refused while pruned elsewhere", func(t *testing.T) {
		dir := t.TempDir()
		stop := holdInSubprocess(t, dir, true)
		if _, err := OpenDirectory(dir); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "pruning") {
			t.Errorf("OpenDirectory while another process prunes = %v, want %v naming the prune", err, ErrLocked)
		}
		stop()
		d, err := OpenDirectory(dir)
		testenv.Must(t, err)
		testenv.Must(t, d.Close())
	})
}

// holdInSubprocess runs the test binary's helper, holding dir open, or pruning it if prune,
// until the returned function is called, and waits for it to hold before returning.
func holdInSubprocess(t *testing.T, dir string, prune bool) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestDirectoryLockHelper$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	if prune {
		cmd.Env = append(cmd.Env, "VAMOS_TEST_LOCK_PRUNE=1")
	}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	testenv.Must(t, cmd.Start())
	ready := filepath.Join(dir, "ready")
	for start := time.Now(); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			cmd.Process.Kill()
			t.Fatalf("the helper didn't take the lock in time:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		os.Remove(ready)
		if err := cmd.Wait(); err != nil {
			t.Errorf("the helper failed: %v\n%s", err, out)
		}
	}
}
